// Package runner is the single entrypoint for executing a compile through
// hpcc. Both invocation paths — the cobra subcommand `hpcc wrap` and the
// symlink-as-compiler shim in main — call into Run, so the cache loop,
// dispatch logic, and error handling live in exactly one place.
package runner

import (
	"os"
	"path/filepath"

	"github.com/aarani/hpcc/internal/compiler"
	"github.com/aarani/hpcc/internal/daemon/client"
)

// Run executes a single compiler invocation. The args here are the
// compiler-side argv (without the compiler name itself) — what the user
// would have passed to gcc/clang/cl directly.
//
// If a daemon is running, the request is forwarded to it: the daemon owns
// the cache loop and the actual compiler invocation, and we just relay
// stdout/stderr/exit. Otherwise we fall back to running the cache lookup
// and the compiler in-process.
//
// One exception: invocations that read source from stdin (`-` or
// `/dev/stdin` as an input) must run in-process even when the daemon is
// up. The compile protocol carries argv but not stdin bytes, so a
// daemon-dispatched stdin compile would silently see EOF. The most
// common caller is the Linux kernel's scripts/cc-version.sh probe.
func Run(ctx *compiler.Context, args []string) error {
	inv, err := ctx.Compiler.Parse(args)
	if err != nil {
		return err
	}

	if !inv.ReadsStdin() {
		if cfg := client.Load(); cfg != nil {
			cwd, _ := os.Getwd()
			full := append([]string{ctx.Compiler.Name()}, args...)
			if resp, err := client.Dispatch(cfg, cwd, full); err == nil {
				if len(resp.Stdout) > 0 {
					os.Stdout.Write(resp.Stdout)
				}
				if len(resp.Stderr) > 0 {
					os.Stderr.Write(resp.Stderr)
				}
				os.Exit(int(resp.ExitCode))
				return nil
			}
		}
	}

	// Cacheable() encodes the rules for which invocations can safely
	// round-trip through CompileCache: single-input compile mode with a
	// real source file. Link/preprocess/assemble, multi-input
	// compiles, and stdin-source compiles all bypass the cache and
	// hand off straight to the compiler.
	var result *compiler.InvocationResult
	if inv.Cacheable() {
		result, err = ctx.Cache.Lookup(inv)
		if err != nil || result == nil {
			result, err = ctx.Compiler.Invoke(inv)
			if err != nil {
				return err
			}
			// Capture the user's -Wp,-MMD,<path> / -MF <path> .d
			// outputs the compiler just wrote so the cache stores
			// them and warm hits can replay them. Without this,
			// warm rebuilds after `make clean` get the .o back but
			// no .d, and tools that re-read the .d (kernel fixdep,
			// ninja's depfile parser) fail. Same shape as the
			// daemon path in internal/daemon/daemon.go.
			if extras := compiler.CollectDepEmissionExtras(inv); extras != nil {
				if result.Extras == nil {
					result.Extras = extras
				} else {
					for k, v := range extras {
						result.Extras[k] = v
					}
				}
			}
			ctx.Cache.Store(inv, result)
		}
	} else {
		result, err = ctx.Compiler.Invoke(inv)
		if err != nil {
			return err
		}
	}

	if (inv.Output != "") && (result.Output != nil) {
		os.WriteFile(inv.Output, result.Output, 0644)
	}

	// Materialise side-effect outputs (.d files etc.) under inv.Cwd.
	// Runs on cache-hit and cache-miss results alike: on a hit the
	// extras come back from the cache blob, on a miss they came from
	// the compiler invocation above. Either way, the user's build
	// rules expect to find the .d file on disk after this returns.
	for path, bytes := range result.Extras {
		full := path
		if !filepath.IsAbs(path) && inv.Cwd != "" {
			full = filepath.Join(inv.Cwd, path)
		}
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		_ = os.WriteFile(full, bytes, 0o644)
	}

	if result.Stdout != nil {
		os.Stdout.Write(result.Stdout)
	}

	if result.Stderr != nil {
		os.Stderr.Write(result.Stderr)
	}

	os.Exit(result.ExitCode)
	return nil
}
