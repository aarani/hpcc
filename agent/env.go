package main

import (
	"os"
	"strings"
)

// defaultPath is the PATH the agent uses for itself when the kernel
// boot env didn't supply one, and that it falls back to for spawned
// child processes when neither the request nor the agent's own env
// carries a PATH. The agent runs as PID 1 directly off the kernel,
// so its environment is whatever the kernel passed via init= —
// typically just HOME=/, sometimes a kernel-set PATH, often neither.
// Without an explicit PATH the spawned compiler can't find
// `gcc`/`cc`/etc. by name; in fact even exec.Command's own LookPath
// reads the *agent's* environment, not the child's, so we have to
// guarantee both halves.
//
// Conventional POSIX search order. If a user image installs its
// toolchain somewhere exotic, the dispatcher / worker can pass an
// explicit PATH in ExecHeader.Env and resolveEnv will let it win.
const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// resolveEnv builds the child process environment from the request,
// inheriting any agent-process env the request didn't override and
// guaranteeing a sensible PATH. Request-supplied vars win over both
// the agent's env and the default PATH.
func resolveEnv(reqEnv []string) []string {
	keys := make(map[string]struct{}, len(reqEnv)+len(os.Environ())+1)
	out := make([]string, 0, len(reqEnv)+len(os.Environ())+1)
	for _, kv := range reqEnv {
		if k := envKey(kv); k != "" {
			keys[k] = struct{}{}
		}
		out = append(out, kv)
	}
	for _, kv := range os.Environ() {
		k := envKey(kv)
		if k == "" {
			continue
		}
		if _, dup := keys[k]; dup {
			continue
		}
		keys[k] = struct{}{}
		out = append(out, kv)
	}
	if _, hasPath := keys["PATH"]; !hasPath {
		out = append(out, "PATH="+defaultPath)
	}
	return out
}

// envKey returns the variable name from a "KEY=VALUE" entry, or ""
// if the entry is malformed.
func envKey(kv string) string {
	if i := strings.IndexByte(kv, '='); i > 0 {
		return kv[:i]
	}
	return ""
}
