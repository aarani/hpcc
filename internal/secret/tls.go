package secret

import (
	"context"
	"crypto/tls"
	"fmt"
	"os"
)

// LoadTLSCertificate resolves a TLS keypair from either filesystem
// paths or secret references. Exactly one of certFile/certRef must be
// set, and exactly one of keyFile/keyRef.
//
// The returned cert PEM bytes are handed back alongside the parsed
// keypair so callers that need the cert separately (e.g. fingerprint
// computation) don't have to re-resolve.
func LoadTLSCertificate(ctx context.Context, r *Resolver, certFile, certRef, keyFile, keyRef string) (tls.Certificate, []byte, error) {
	if r == nil {
		r = Default
	}
	certPEM, err := loadOneTLS(ctx, r, "cert", certFile, certRef)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	keyPEM, err := loadOneTLS(ctx, r, "key", keyFile, keyRef)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("parse TLS keypair: %w", err)
	}
	return cert, certPEM, nil
}

func loadOneTLS(ctx context.Context, r *Resolver, kind, path, ref string) ([]byte, error) {
	switch {
	case path != "" && ref != "":
		return nil, fmt.Errorf("tls.%s_file and tls.%s_ref are mutually exclusive", kind, kind)
	case path != "":
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read tls.%s_file %q: %w", kind, path, err)
		}
		return b, nil
	case ref != "":
		b, err := r.ResolveBytes(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("resolve tls.%s_ref: %w", kind, err)
		}
		return b, nil
	default:
		return nil, fmt.Errorf("one of tls.%s_file or tls.%s_ref is required", kind, kind)
	}
}
