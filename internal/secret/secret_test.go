package secret

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveString_Literal(t *testing.T) {
	r := &Resolver{}
	got, err := r.ResolveString(context.Background(), "plain-value")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "plain-value" {
		t.Fatalf("got %q want %q", got, "plain-value")
	}
}

func TestResolveString_Empty(t *testing.T) {
	r := &Resolver{}
	got, err := r.ResolveString(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestResolveString_Env(t *testing.T) {
	t.Setenv("HPCC_TEST_SECRET", "supersecret")
	r := &Resolver{}
	got, err := r.ResolveString(context.Background(), "env:HPCC_TEST_SECRET")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "supersecret" {
		t.Fatalf("got %q want %q", got, "supersecret")
	}
}

func TestResolveString_EnvMissing(t *testing.T) {
	os.Unsetenv("HPCC_TEST_SECRET_MISSING")
	r := &Resolver{}
	_, err := r.ResolveString(context.Background(), "env:HPCC_TEST_SECRET_MISSING")
	if err == nil {
		t.Fatal("expected error for missing env var, got nil")
	}
	if !strings.Contains(err.Error(), "HPCC_TEST_SECRET_MISSING") {
		t.Fatalf("error %q does not mention env var name", err)
	}
}

func TestResolveString_File(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(path, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &Resolver{}
	got, err := r.ResolveString(context.Background(), "file:"+path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "file-token" {
		t.Fatalf("got %q want %q (trailing newline should be stripped)", got, "file-token")
	}
}

func TestResolveString_FileMissing(t *testing.T) {
	r := &Resolver{}
	_, err := r.ResolveString(context.Background(), "file:/nope/does-not-exist")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestResolveString_AWSSM_String(t *testing.T) {
	r := &Resolver{
		awsSM: func(ctx context.Context, region, secretID string) (string, []byte, error) {
			if secretID != "hpcc/scheduler/worker-token" {
				t.Errorf("unexpected secret id %q", secretID)
			}
			if region != "us-east-1" {
				t.Errorf("unexpected region %q", region)
			}
			return "sm-string-value", nil, nil
		},
	}
	got, err := r.ResolveString(context.Background(), "aws-sm://hpcc/scheduler/worker-token?region=us-east-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "sm-string-value" {
		t.Fatalf("got %q want %q", got, "sm-string-value")
	}
}

func TestResolveString_AWSSM_Binary(t *testing.T) {
	r := &Resolver{
		awsSM: func(ctx context.Context, region, secretID string) (string, []byte, error) {
			return "", []byte("binary-bytes"), nil
		},
	}
	got, err := r.ResolveString(context.Background(), "aws-sm://name")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "binary-bytes" {
		t.Fatalf("got %q want %q", got, "binary-bytes")
	}
}

func TestResolveString_AWSSM_JSONKey(t *testing.T) {
	r := &Resolver{
		awsSM: func(ctx context.Context, region, secretID string) (string, []byte, error) {
			return `{"cert":"PEM-CERT","key":"PEM-KEY"}`, nil, nil
		},
	}
	got, err := r.ResolveString(context.Background(), "aws-sm://hpcc/tls#key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "PEM-KEY" {
		t.Fatalf("got %q want %q", got, "PEM-KEY")
	}
}

func TestResolveString_AWSSM_JSONKeyMissing(t *testing.T) {
	r := &Resolver{
		awsSM: func(ctx context.Context, region, secretID string) (string, []byte, error) {
			return `{"cert":"x"}`, nil, nil
		},
	}
	_, err := r.ResolveString(context.Background(), "aws-sm://hpcc/tls#absent")
	if err == nil || !strings.Contains(err.Error(), "absent") {
		t.Fatalf("expected error mentioning missing key, got %v", err)
	}
}

func TestResolveString_AWSSM_EmptyPayload(t *testing.T) {
	r := &Resolver{
		awsSM: func(ctx context.Context, region, secretID string) (string, []byte, error) {
			return "", nil, nil
		},
	}
	_, err := r.ResolveString(context.Background(), "aws-sm://hpcc/empty")
	if err == nil {
		t.Fatal("expected error for empty payload")
	}
}

func TestResolveString_AWSSM_ErrorPropagates(t *testing.T) {
	sentinel := errors.New("sm-down")
	r := &Resolver{
		awsSM: func(ctx context.Context, region, secretID string) (string, []byte, error) {
			return "", nil, sentinel
		},
	}
	_, err := r.ResolveString(context.Background(), "aws-sm://hpcc/x")
	if err == nil || !strings.Contains(err.Error(), "sm-down") {
		t.Fatalf("expected wrapped sentinel, got %v", err)
	}
}

func TestResolveString_AWSSM_MissingID(t *testing.T) {
	r := &Resolver{
		awsSM: func(ctx context.Context, region, secretID string) (string, []byte, error) {
			t.Fatalf("awsSM should not be called for malformed ref")
			return "", nil, nil
		},
	}
	_, err := r.ResolveString(context.Background(), "aws-sm://")
	if err == nil {
		t.Fatal("expected error for empty secret id")
	}
}

func TestResolveBytes_Literal(t *testing.T) {
	r := &Resolver{}
	got, err := r.ResolveBytes(context.Background(), "raw")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != "raw" {
		t.Fatalf("got %q want %q", got, "raw")
	}
}

func TestResolveBytes_FilePreservesNewlines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key.pem")
	pem := "-----BEGIN PRIVATE KEY-----\nMIIE...\n-----END PRIVATE KEY-----\n"
	if err := os.WriteFile(path, []byte(pem), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &Resolver{}
	got, err := r.ResolveBytes(context.Background(), "file:"+path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != pem {
		t.Fatalf("ResolveBytes mangled the file content (trailing newline must survive)")
	}
}

func TestIsRef(t *testing.T) {
	cases := map[string]bool{
		"":                     false,
		"plain":                false,
		"aws-sm://x":           true,
		"env:NAME":             true,
		"file:/tmp/x":          true,
		"unknown://x":          false,
		"aws-sm-not-a-scheme":  false,
	}
	for in, want := range cases {
		if got := IsRef(in); got != want {
			t.Errorf("IsRef(%q) = %v want %v", in, got, want)
		}
	}
}
