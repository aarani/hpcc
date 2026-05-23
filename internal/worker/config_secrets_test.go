package worker

import (
	"context"
	"testing"
)

func TestConfig_ResolveSecrets_Env(t *testing.T) {
	t.Setenv("HPCC_TEST_WORKER_TOKEN", "0123456789abcdef0123456789abcdef")
	c := &Config{Scheduler: SchedulerLink{WorkerToken: "env:HPCC_TEST_WORKER_TOKEN"}}
	if err := c.ResolveSecrets(context.Background()); err != nil {
		t.Fatalf("ResolveSecrets: %v", err)
	}
	if c.Scheduler.WorkerToken != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("got %q want resolved env value", c.Scheduler.WorkerToken)
	}
}

func TestConfig_Validate_TLSRefAccepted(t *testing.T) {
	c := minimalValidConfig()
	c.TLS = TLSConfig{CertRef: "aws-sm://hpcc/cert", KeyRef: "aws-sm://hpcc/key"}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate with cert_ref/key_ref: %v", err)
	}
}

func TestConfig_Validate_TLSBothFormsRejected(t *testing.T) {
	c := minimalValidConfig()
	c.TLS = TLSConfig{CertFile: "/a.crt", CertRef: "aws-sm://x", KeyFile: "/a.key"}
	if err := c.Validate(); err == nil {
		t.Fatal("expected Validate to reject cert_file + cert_ref combo")
	}
}

func minimalValidConfig() Config {
	return Config{
		Listen:     ":9092",
		PublicAddr: "worker.local:9092",
		TLS:        TLSConfig{CertFile: "/etc/c.crt", KeyFile: "/etc/c.key"},
		Scheduler:  SchedulerLink{URL: "scheduler.local:9091", WorkerToken: "0123456789abcdef"},
		Runtime:    RuntimeConfig{Handler: "really_really_dangerous"},
		VM:         VMConfig{Memory: "2GB", VCPUs: 4, IdleTimeout: "10m", SessionTimeout: "8h"},
		Pool:       PoolConfig{MaxActive: 32},
		Image:      ImageConfig{IdleTimeout: "24h"},
	}
}
