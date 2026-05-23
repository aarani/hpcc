package scheduler

import (
	"context"
	"testing"
)

func TestConfig_ResolveSecrets_Env(t *testing.T) {
	t.Setenv("HPCC_TEST_WORKER_TOKEN", "0123456789abcdef0123456789abcdef")
	c := &Config{Auth: Auth{WorkerToken: "env:HPCC_TEST_WORKER_TOKEN"}}
	if err := c.ResolveSecrets(context.Background()); err != nil {
		t.Fatalf("ResolveSecrets: %v", err)
	}
	if c.Auth.WorkerToken != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("got %q want resolved env value", c.Auth.WorkerToken)
	}
}

func TestConfig_ResolveSecrets_Literal(t *testing.T) {
	c := &Config{Auth: Auth{WorkerToken: "literal-token-1234567890"}}
	if err := c.ResolveSecrets(context.Background()); err != nil {
		t.Fatalf("ResolveSecrets: %v", err)
	}
	if c.Auth.WorkerToken != "literal-token-1234567890" {
		t.Fatalf("literal token should pass through unchanged")
	}
}

func TestConfig_Validate_TLSRefAccepted(t *testing.T) {
	c := Config{
		Listen: ":9091",
		TLS:    TLSConfig{CertRef: "aws-sm://hpcc/cert", KeyRef: "aws-sm://hpcc/key"},
		Auth:   Auth{WorkerToken: "0123456789abcdef"},
		Tenants: []Tenant{{
			ID: "acme", Issuer: "https://idp/", JWKSURL: "https://idp/jwks",
			TokenURL: "https://idp/token", Audience: "hpcc",
		}},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate with cert_ref/key_ref: %v", err)
	}
}

func TestConfig_Validate_TLSBothFormsRejected(t *testing.T) {
	c := Config{
		Listen: ":9091",
		TLS:    TLSConfig{CertFile: "/a.crt", CertRef: "aws-sm://x", KeyFile: "/a.key"},
		Auth:   Auth{WorkerToken: "0123456789abcdef"},
		Tenants: []Tenant{{
			ID: "acme", Issuer: "https://idp/", JWKSURL: "https://idp/jwks",
			TokenURL: "https://idp/token", Audience: "hpcc",
		}},
	}
	if err := c.Validate(); err == nil {
		t.Fatal("expected Validate to reject cert_file + cert_ref combo")
	}
}
