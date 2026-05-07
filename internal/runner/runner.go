// Package runner is the single entrypoint for executing a compile through
// hpcc. Both invocation paths — the cobra subcommand `hpcc wrap` and the
// symlink-as-compiler shim in main — call into Run, so the cache loop,
// dispatch logic, and error handling live in exactly one place.
package runner

import (
	"os"

	"github.com/aarani/hpcc/internal/compiler"
)

// Run executes a single compiler invocation. The args here are the
// compiler-side argv (without the compiler name itself) — what the user
// would have passed to gcc/clang/cl directly.
//
// Today this just parses. The cache lookup, distributed dispatch, and
// real compiler invocation will all hang off this function as they land.
func Run(ctx *compiler.Context, args []string) error {
	inv, err := ctx.Compiler.Parse(args)
	if err != nil {
		return err
	}
	result, err := ctx.Cache.Lookup(inv)

	if err != nil || result == nil {
		result, err = ctx.Compiler.Invoke(inv)
		if err != nil {
			return err
		}
		ctx.Cache.Store(inv, result)
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
