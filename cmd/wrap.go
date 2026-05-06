/*
Copyright © 2026 Afshin Arani <afshin@arani.dev>
*/
package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// wrapCmd represents the wrap command
var wrapCmd = &cobra.Command{
	Use:  "wrap",
	Args: cobra.ArbitraryArgs,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("wrap called")
	},
	DisableFlagParsing: true,
}

func init() {
	rootCmd.AddCommand(wrapCmd)
}
