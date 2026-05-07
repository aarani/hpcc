/*
Copyright © 2026 Afshin Arani <afshin@arani.dev>
*/
package cmd

import (
	"os"

	"github.com/spf13/cobra"
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "hpcc",
	Short: "Distributed compilation but it makes sense",
	Long: `hpcc speeds up C and C++ builds by caching compilation outputs and
distributing the work across a cluster of isolated workers.

Each invocation parses the compiler command line, hashes the relevant
inputs (preprocessed source or dependency closure, plus toolchain identity
and the flags that affect codegen), and looks the result up in a local —
and optionally remote — cache. On a hit, the cached object is replayed
with the recorded stdout, stderr, and exit code. On a miss, the work is
either run locally or dispatched to a worker that compiles inside a
hardware-virtualized sandbox, then the result is cached for next time.

hpcc supports clang and cl.exe today, and is designed to be invoked either
explicitly (` + "`hpcc wrap clang -c foo.c -o foo.o`" + `) or as a drop-in
replacement via symlink (` + "`ln -s hpcc clang`" + `). Build systems like
make, ninja, and cmake see no difference from the underlying compiler.`,
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
	}
}

