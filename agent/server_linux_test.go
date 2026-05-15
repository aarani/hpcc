//go:build linux

package main

import (
	"os"
	"strings"
	"testing"
)

// findEnv returns the value of key in env, or "" if absent.
func findEnv(env []string, key string) string {
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 && kv[:i] == key {
			return kv[i+1:]
		}
	}
	return ""
}

// TestResolveEnv_supplements_default_PATH pins the central guarantee:
// even with no request env and a sparse PID-1 environment, the
// spawned compiler always sees a usable PATH. The kernel-bench
// failure mode this protects against is `exec: "gcc": executable
// file not found in $PATH` from inside the FC VM where the agent's
// own env (set by the kernel) carries no PATH at all.
func TestResolveEnv_supplements_default_PATH(t *testing.T) {
	// Snapshot and clear the agent's PATH for the duration of the
	// test, mirroring the kernel-boot environment a real PID-1 agent
	// runs under.
	t.Setenv("PATH", "")
	if got := os.Getenv("PATH"); got != "" {
		t.Fatalf("setup: PATH = %q, want empty", got)
	}

	env := resolveEnv(nil)
	if got := findEnv(env, "PATH"); got != defaultPath {
		t.Errorf("PATH = %q, want %q (default)", got, defaultPath)
	}
}

func TestResolveEnv_request_PATH_wins(t *testing.T) {
	t.Setenv("PATH", "/agent/inherited")
	env := resolveEnv([]string{"PATH=/req/wins"})
	if got := findEnv(env, "PATH"); got != "/req/wins" {
		t.Errorf("PATH = %q, want /req/wins", got)
	}
}

func TestResolveEnv_agent_PATH_inherited_when_request_omits(t *testing.T) {
	t.Setenv("PATH", "/agent/path")
	env := resolveEnv([]string{"VSLANG=1033"})
	if got := findEnv(env, "PATH"); got != "/agent/path" {
		t.Errorf("PATH = %q, want /agent/path", got)
	}
	if got := findEnv(env, "VSLANG"); got != "1033" {
		t.Errorf("VSLANG = %q, want 1033", got)
	}
}

func TestResolveEnv_request_keys_dedupe_agent_keys(t *testing.T) {
	t.Setenv("FOO", "agent")
	env := resolveEnv([]string{"FOO=req"})
	// FOO should appear exactly once, with the request value.
	count := 0
	for _, kv := range env {
		if envKey(kv) == "FOO" {
			count++
			if v := kv[len("FOO="):]; v != "req" {
				t.Errorf("FOO = %q, want req", v)
			}
		}
	}
	if count != 1 {
		t.Errorf("FOO appeared %d times, want 1; env=%v", count, env)
	}
}

func TestEnvKey(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"PATH=/usr/bin", "PATH"},
		{"FOO=", "FOO"},
		{"=value", ""},
		{"no_equals", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := envKey(tc.in); got != tc.want {
			t.Errorf("envKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
