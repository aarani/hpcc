package cmd

import (
	"bytes"
	"crypto/tls"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aarani/hpcc/internal/config"
	"github.com/aarani/hpcc/internal/enum"
	"github.com/aarani/hpcc/internal/scheduler"
	"github.com/aarani/hpcc/internal/worker"
)

// fakeTLSFiles creates two zero-content files for the scheduler cert
// and key. Validate() only checks that the *paths* are non-empty;
// loading the actual material is the scheduler-start codepath, which
// isn't under test here.
func fakeTLSFiles(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	dir := t.TempDir()
	certPath = filepath.Join(dir, "tls.crt")
	keyPath = filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return
}

func runSchedulerInit(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newInitSchedulerCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func runWorkerInit(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newInitWorkerCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func runClientInit(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newInitClientCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestInitScheduler_WritesValidatableConfig(t *testing.T) {
	certPath, keyPath := fakeTLSFiles(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "scheduler.toml")

	out, err := runSchedulerInit(t,
		"--config", cfgPath,
		"--cert-file", certPath,
		"--key-file", keyPath,
		"--tenant-id", "acme",
		"--issuer", "https://idp.acme.example/",
		"--jwks-url", "https://idp.acme.example/.well-known/jwks.json",
		"--token-url", "https://idp.acme.example/oauth/token",
		"--audience", "hpcc",
	)
	if err != nil {
		t.Fatalf("init scheduler failed: %v\noutput:\n%s", err, out)
	}

	info, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("scheduler.toml perm = %o, want 0600", perm)
	}

	cfg, err := scheduler.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("generated config did not Validate: %v", err)
	}
	if len(cfg.Auth.WorkerToken) < 16 {
		t.Errorf("worker_token too short: %q", cfg.Auth.WorkerToken)
	}
	if !strings.Contains(out, cfg.Auth.WorkerToken) {
		t.Errorf("init scheduler did not print the token in its output: %q", out)
	}
}

func TestInitScheduler_RefusesOverwrite(t *testing.T) {
	certPath, keyPath := fakeTLSFiles(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "scheduler.toml")
	if err := os.WriteFile(cfgPath, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runSchedulerInit(t,
		"--config", cfgPath,
		"--cert-file", certPath,
		"--key-file", keyPath,
		"--tenant-id", "acme",
		"--issuer", "https://idp.acme.example/",
		"--jwks-url", "https://idp.acme.example/.well-known/jwks.json",
		"--token-url", "https://idp.acme.example/oauth/token",
		"--audience", "hpcc",
	)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected refusal to overwrite, got err=%v output=%q", err, out)
	}
}

func TestInitScheduler_RejectsMissingTenantFlag(t *testing.T) {
	certPath, keyPath := fakeTLSFiles(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "scheduler.toml")

	_, err := runSchedulerInit(t,
		"--config", cfgPath,
		"--cert-file", certPath,
		"--key-file", keyPath,
		// --tenant-id omitted
		"--issuer", "https://idp.acme.example/",
		"--jwks-url", "https://idp.acme.example/.well-known/jwks.json",
		"--token-url", "https://idp.acme.example/oauth/token",
		"--audience", "hpcc",
	)
	if err == nil || !strings.Contains(err.Error(), "tenant-id") {
		t.Fatalf("expected --tenant-id required error, got %v", err)
	}
}

func TestInitScheduler_RejectsCertFileAndRefBothSet(t *testing.T) {
	certPath, keyPath := fakeTLSFiles(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "scheduler.toml")

	_, err := runSchedulerInit(t,
		"--config", cfgPath,
		"--cert-file", certPath,
		"--cert-ref", "aws-sm://something",
		"--key-file", keyPath,
		"--tenant-id", "acme",
		"--issuer", "https://idp.acme.example/",
		"--jwks-url", "https://idp.acme.example/.well-known/jwks.json",
		"--token-url", "https://idp.acme.example/oauth/token",
		"--audience", "hpcc",
	)
	if err == nil || !strings.Contains(err.Error(), "cert-file") {
		t.Fatalf("expected cert-file/cert-ref exclusivity error, got %v", err)
	}
}

func TestInitWorker_GeneratesUsableTLSAndConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "worker.toml")

	out, err := runWorkerInit(t,
		"--config", cfgPath,
		"--scheduler", "scheduler.test:9091",
		"--token", "this-is-a-sufficiently-long-token-abc123",
		"--public-addr", "worker-1.internal:9092",
		"--runtime", "really_really_dangerous",
	)
	if err != nil {
		t.Fatalf("init worker failed: %v\noutput:\n%s", err, out)
	}

	certPath := filepath.Join(dir, "worker.crt")
	keyPath := filepath.Join(dir, "worker.key")
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
		t.Fatalf("generated TLS pair did not load: %v", err)
	}
	if info, err := os.Stat(keyPath); err == nil {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("worker.key perm = %o, want 0600", perm)
		}
	}

	cfg, err := worker.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("generated worker.toml did not Validate: %v", err)
	}
	if cfg.Scheduler.URL != "scheduler.test:9091" {
		t.Errorf("scheduler.url = %q", cfg.Scheduler.URL)
	}
	if cfg.TLS.CertFile != certPath {
		t.Errorf("tls.cert_file = %q, want %q", cfg.TLS.CertFile, certPath)
	}
}

func TestInitClient_WritesParseableConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")

	out, err := runClientInit(t,
		"--config", cfgPath,
		"--scheduler", "scheduler.internal:9091",
		"--tenant", "acme",
		"--image-digest", "sha256:deadbeefcafe",
		"--image-ref", "ghcr.io/example/toolchain",
	)
	if err != nil {
		t.Fatalf("init client failed: %v\noutput:\n%s", err, out)
	}

	info, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config.toml perm = %o, want 0600", perm)
	}

	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.Remote.Enabled {
		t.Error("expected remote.enabled = true")
	}
	if cfg.Remote.TenantID != "acme" {
		t.Errorf("tenant_id = %q, want acme", cfg.Remote.TenantID)
	}
	if cfg.Remote.Scheduler.URL != "scheduler.internal:9091" {
		t.Errorf("scheduler.url = %q", cfg.Remote.Scheduler.URL)
	}
	if cfg.Remote.ImageDigest != "sha256:deadbeefcafe" {
		t.Errorf("image_digest = %q", cfg.Remote.ImageDigest)
	}
	if len(cfg.Caches) != 1 || cfg.Caches[0].Type != enum.CacheDisk {
		t.Errorf("expected one disk cache, got %#v", cfg.Caches)
	}
}

func TestInitClient_RejectsMissingScheduler(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")

	_, err := runClientInit(t,
		"--config", cfgPath,
		"--tenant", "acme",
		"--image-digest", "sha256:deadbeefcafe",
	)
	if err == nil || !strings.Contains(err.Error(), "scheduler") {
		t.Fatalf("expected --scheduler required error, got %v", err)
	}
}

func TestInitClient_RefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := runClientInit(t,
		"--config", cfgPath,
		"--scheduler", "scheduler.internal:9091",
		"--tenant", "acme",
		"--image-digest", "sha256:deadbeefcafe",
	)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected refusal to overwrite, got %v", err)
	}
}

func TestInitWorker_RunhcsRendersHcsshimDefaults(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "worker.toml")

	out, err := runWorkerInit(t,
		"--config", cfgPath,
		"--scheduler", "scheduler.test:9091",
		"--token", "this-is-a-sufficiently-long-token-abc123",
		"--public-addr", "worker-win-1.internal:9092",
		"--runtime", "runhcs-wcow-hypervisor",
	)
	if err != nil {
		t.Fatalf("init worker failed: %v\noutput:\n%s", err, out)
	}

	cfg, err := worker.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("generated worker.toml did not Validate: %v", err)
	}
	// Backslash escaping in the template is fragile — verify the
	// pipe address and Windows paths round-trip through TOML
	// decoding to their actual values, not to the escaped form.
	if got, want := cfg.Runtime.Hcsshim.Address, `\\.\pipe\containerd-containerd`; got != want {
		t.Errorf("hcsshim.address = %q, want %q", got, want)
	}
	if got, want := cfg.Runtime.Hcsshim.RunDir, `C:\ProgramData\hpcc\run`; got != want {
		t.Errorf("hcsshim.run_dir = %q, want %q", got, want)
	}
	if cfg.Runtime.Hcsshim.Isolation != "hyperv" {
		t.Errorf("hcsshim.isolation = %q, want hyperv", cfg.Runtime.Hcsshim.Isolation)
	}
	if got, want := cfg.Image.AgentWindowsAmd64, `C:\ProgramData\hpcc\hpcc-agent.exe`; got != want {
		t.Errorf("image.agent_windows_amd64 = %q, want %q", got, want)
	}
}

func TestInitWorker_RejectsShortToken(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "worker.toml")

	_, err := runWorkerInit(t,
		"--config", cfgPath,
		"--scheduler", "scheduler.test:9091",
		"--token", "short",
		"--public-addr", "worker-1.internal:9092",
		"--runtime", "really_really_dangerous",
	)
	if err == nil || !strings.Contains(err.Error(), "16 characters") {
		t.Fatalf("expected post-write validation to reject short token, got %v", err)
	}
}
