/*
Copyright © 2026 Afshin Arani <afshin@arani.dev>
*/
package cmd

import (
	"github.com/aarani/hpcc/internal/compiler"
	"github.com/spf13/cobra"
)

var wrapCmd = &cobra.Command{
	Use:   "wrap <compiler> [args...]",
	Short: "Wrap a compiler invocation",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := compiler.Detect(args[0])
		if err != nil {
			return err
		}
		inv, err := c.Parse(args[1:])
		if err != nil {
			return err
		}
		_ = inv // TODO: cache lookup / invoke
		return nil
	},
	DisableFlagParsing: true,
}

func init() {
	rootCmd.AddCommand(wrapCmd)
}
