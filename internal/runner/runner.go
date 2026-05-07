// Package runner is the single entrypoint for executing a compile through
// hpcc. Both invocation paths — the cobra subcommand `hpcc wrap` and the
// symlink-as-compiler shim in main — call into Run, so the cache loop,
// dispatch logic, and error handling live in exactly one place.
package runner

import "github.com/aarani/hpcc/internal/compiler"

// Run executes a single compiler invocation. The args here are the
// compiler-side argv (without the compiler name itself) — what the user
// would have passed to gcc/clang/cl directly.
//
// Today this just parses. The cache lookup, distributed dispatch, and
// real compiler invocation will all hang off this function as they land.
func Run(c compiler.Compiler, args []string) error {
	inv, err := c.Parse(args)
	if err != nil {
		return err
	}
	_ = inv // TODO: cache lookup → hit replay or invoke
	return nil
}
