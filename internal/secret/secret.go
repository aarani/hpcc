// Package secret resolves config-time secret references so callers never
// have to put a credential in a TOML file. A "reference" is a string
// that may carry a scheme prefix:
//
//	aws-sm://<secret-id>[?region=<r>][#<json-key>]
//	    AWS Secrets Manager. <secret-id> may contain '/' (e.g.
//	    "hpcc/scheduler/worker-token"). When ?region= is omitted the
//	    AWS SDK default credential / region chain applies (env,
//	    ~/.aws, IMDS, IRSA). When #<json-key> is set, the resolver
//	    expects the secret payload to be a JSON object and returns
//	    the named field.
//	env:<NAME>
//	    Process environment variable.
//	file:<path>
//	    Read from the local filesystem. Trailing newlines are stripped
//	    for string lookups; bytes pass through unmodified.
//
// A bare value (no recognised scheme prefix) is returned literally, so
// existing configs containing inline secrets keep working.
package secret

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

// Resolver fetches values referenced by URI. The zero value is usable
// and lazy-initialises an AWS Secrets Manager client on first aws-sm:
// lookup.
type Resolver struct {
	smMu    sync.Mutex
	smCache map[string]*secretsmanager.Client // keyed by region (empty key = SDK default)

	// awsSM bypasses the AWS SDK when set. Tests inject a stub here so
	// scheme-dispatch and URL-parsing logic can be exercised without
	// touching the AWS network.
	awsSM func(ctx context.Context, region, secretID string) (string, []byte, error)
}

// Default is the package-level Resolver used by the config loaders.
// Reusing a single instance keeps the AWS client cache warm across all
// lookups in a process.
var Default = &Resolver{}

// IsRef reports whether s carries a scheme prefix Resolve* will look
// up. Useful in validation paths that want to skip length checks on
// references whose resolved length isn't known yet.
func IsRef(s string) bool {
	return strings.HasPrefix(s, "aws-sm:") ||
		strings.HasPrefix(s, "env:") ||
		strings.HasPrefix(s, "file:")
}

// ResolveString returns the textual value the reference points at. A
// bare (un-prefixed) input is returned verbatim.
func (r *Resolver) ResolveString(ctx context.Context, ref string) (string, error) {
	if ref == "" {
		return "", nil
	}
	switch {
	case strings.HasPrefix(ref, "aws-sm:"):
		return r.resolveAWSSM(ctx, ref)
	case strings.HasPrefix(ref, "env:"):
		name := strings.TrimPrefix(ref, "env:")
		v, ok := os.LookupEnv(name)
		if !ok {
			return "", fmt.Errorf("secret env var %q is not set", name)
		}
		if v == "" {
			return "", fmt.Errorf("secret env var %q is empty", name)
		}
		return v, nil
	case strings.HasPrefix(ref, "file:"):
		path := trimFileScheme(ref)
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read secret file %q: %w", path, err)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	default:
		return ref, nil
	}
}

// ResolveBytes returns the raw bytes the reference points at. A bare
// (un-prefixed) input is returned as []byte(ref); callers that need to
// distinguish "literal" from "reference" should test with IsRef first.
//
// For aws-sm: lookups the resolver prefers SecretBinary when present
// and falls back to SecretString — that lets TLS keypairs live in SM
// regardless of which storage shape the operator chose.
func (r *Resolver) ResolveBytes(ctx context.Context, ref string) ([]byte, error) {
	if ref == "" {
		return nil, nil
	}
	switch {
	case strings.HasPrefix(ref, "aws-sm:"):
		s, err := r.resolveAWSSM(ctx, ref)
		if err != nil {
			return nil, err
		}
		return []byte(s), nil
	case strings.HasPrefix(ref, "env:"):
		name := strings.TrimPrefix(ref, "env:")
		v, ok := os.LookupEnv(name)
		if !ok {
			return nil, fmt.Errorf("secret env var %q is not set", name)
		}
		return []byte(v), nil
	case strings.HasPrefix(ref, "file:"):
		path := trimFileScheme(ref)
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read secret file %q: %w", path, err)
		}
		return b, nil
	default:
		return []byte(ref), nil
	}
}

func trimFileScheme(ref string) string {
	s := strings.TrimPrefix(ref, "file:")
	s = strings.TrimPrefix(s, "//")
	return s
}

func (r *Resolver) resolveAWSSM(ctx context.Context, ref string) (string, error) {
	// Normalise so url.Parse always sees a "//" authority; that lets a
	// secret-id with '/' separators (e.g. "hpcc/scheduler/worker-token")
	// land in Host+Path rather than Opaque.
	body := strings.TrimPrefix(ref, "aws-sm:")
	body = strings.TrimPrefix(body, "//")
	u, err := url.Parse("aws-sm://" + body)
	if err != nil {
		return "", fmt.Errorf("parse %q: %w", ref, err)
	}
	secretID := u.Host + u.Path
	if secretID == "" {
		return "", fmt.Errorf("aws-sm reference %q is missing a secret id", ref)
	}
	region := u.Query().Get("region")
	jsonKey := u.Fragment

	var val string
	if r.awsSM != nil {
		s, bin, err := r.awsSM(ctx, region, secretID)
		if err != nil {
			return "", fmt.Errorf("aws secrets manager get %q: %w", secretID, err)
		}
		switch {
		case len(bin) > 0:
			val = string(bin)
		case s != "":
			val = s
		default:
			return "", fmt.Errorf("aws secrets manager returned empty payload for %q", secretID)
		}
	} else {
		client, err := r.smClient(ctx, region)
		if err != nil {
			return "", err
		}
		out, err := client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
			SecretId: &secretID,
		})
		if err != nil {
			return "", fmt.Errorf("aws secrets manager get %q: %w", secretID, err)
		}
		switch {
		case len(out.SecretBinary) > 0:
			val = string(out.SecretBinary)
		case out.SecretString != nil:
			val = *out.SecretString
		default:
			return "", fmt.Errorf("aws secrets manager returned empty payload for %q", secretID)
		}
	}

	if jsonKey == "" {
		return val, nil
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(val), &obj); err != nil {
		return "", fmt.Errorf("decode JSON secret %q: %w", secretID, err)
	}
	raw, ok := obj[jsonKey]
	if !ok {
		return "", fmt.Errorf("secret %q has no key %q", secretID, jsonKey)
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("secret %q key %q is not a string", secretID, jsonKey)
	}
	return s, nil
}

func (r *Resolver) smClient(ctx context.Context, region string) (*secretsmanager.Client, error) {
	r.smMu.Lock()
	defer r.smMu.Unlock()
	if r.smCache == nil {
		r.smCache = make(map[string]*secretsmanager.Client)
	}
	if c, ok := r.smCache[region]; ok {
		return c, nil
	}
	var opts []func(*awsconfig.LoadOptions) error
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	c := secretsmanager.NewFromConfig(awsCfg)
	r.smCache[region] = c
	return c, nil
}
