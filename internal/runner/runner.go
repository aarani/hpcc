// Package runner is the single entrypoint for executing a compile through
// hpcc. Both invocation paths — the cobra subcommand `hpcc wrap` and the
// symlink-as-compiler shim in main — call into Run, so the cache loop,
// dispatch logic, and error handling live in exactly one place.
package runner

import (
	"os"

	"github.com/aarani/hpcc/internal/compiler"
	"github.com/aarani/hpcc/internal/daemon/client"
	"github.com/aarani/hpcc/internal/enum"
)

// Run executes a single compiler invocation. The args here are the
// compiler-side argv (without the compiler name itself) — what the user
// would have passed to gcc/clang/cl directly.
//
// If a daemon is running, the request is forwarded to it: the daemon owns
// the cache loop and the actual compiler invocation, and we just relay
// stdout/stderr/exit. Otherwise we fall back to running the cache lookup
// and the compiler in-process.
func Run(ctx *compiler.Context, args []string) error {
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

	inv, err := ctx.Compiler.Parse(args)
	if err != nil {
		return err
	}

	// Only compile mode is cacheable: link/preprocess/dep-only/assemble
	// invocations either depend on too much external state (libraries,
	// link order) or are cheap enough that caching adds no value. Their
	// inputs (.o/.a files, etc.) also can't be fed through the
	// preprocessor that CacheKey runs.
	var result *compiler.InvocationResult
	if inv.Mode == enum.CompileMode {
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
