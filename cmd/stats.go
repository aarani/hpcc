/*
Copyright © 2026 Afshin Arani <afshin@arani.dev>
*/
package cmd

import (
	"fmt"

	"github.com/aarani/hpcc/internal/runner"
	"github.com/spf13/cobra"
)

var statsCmd = &cobra.Command{
	Use:   "stats",
	Short: "Show local cache statistics",
	RunE: func(cmd *cobra.Command, args []string) error {
		stores, err := runner.LoadStores()
		if err != nil {
			return err
		}
		if len(stores) == 0 {
			fmt.Println("No cache stores configured.")
			return nil
		}

		for i, s := range stores {
			entries, size, err := s.Stats()
			if err != nil {
				return fmt.Errorf("store %d (%s): %w", i, s.Dir(), err)
			}
			fmt.Printf("Cache %d: %s\n", i, s.Dir())
			fmt.Printf("  Entries: %d\n", entries)
			fmt.Printf("  Size:    %s\n", formatSize(size))
			if i < len(stores)-1 {
				fmt.Println()
			}
		}
		return nil
	},
}

func formatSize(bytes int64) string {
	switch {
	case bytes >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(bytes)/float64(1<<30))
	case bytes >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(1<<20))
	case bytes >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(bytes)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

func init() {
	rootCmd.AddCommand(statsCmd)
}
