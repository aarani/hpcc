/*
Copyright © 2026 Afshin Arani <afshin@arani.dev>
*/
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aarani/hpcc/cmd"
	"github.com/aarani/hpcc/internal/compiler"
	"github.com/aarani/hpcc/internal/runner"
)

func main() {
	self := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	if self != "hpcc" {
		if ctx, err := compiler.NewContext(self); err == nil {
			// Symlink mode: bypass cobra entirely. Cobra's root-level
			// flag parser would reject compiler flags like -c before any
			// subcommand could see them.
			if err := runner.Run(ctx, os.Args[1:]); err != nil {
				fmt.Fprintln(os.Stderr, "hpcc:", err)
				os.Exit(1)
			}
			return
		}
	}
	cmd.Execute()
}
