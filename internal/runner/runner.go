// Package runner is the single entrypoint for executing a compile through
// hpcc. Both invocation paths — the cobra subcommand `hpcc wrap` and the
// symlink-as-compiler shim in main — call into Run, so the cache loop,
// dispatch logic, and error handling live in exactly one place.
package runner

import (
	"os"

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
	// round-trip through V1Cache: single-input compile mode with a
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

	if result.Stdout != nil {
		os.Stdout.Write(result.Stdout)
	}

	if result.Stderr != nil {
		os.Stderr.Write(result.Stderr)
	}

	os.Exit(result.ExitCode)
	return nil
}
