/*
Copyright © 2026 Afshin Arani <afshin@arani.dev>
*/
package cmd

import (
	"fmt"
	"time"

	"github.com/aarani/hpcc/internal"
	"github.com/aarani/hpcc/internal/runner"
	"github.com/spf13/cobra"
)

var cleanCmd = &cobra.Command{
	Use:   "clean",
	Short: "Evict cache entries by size or age",
	Long: `Remove cached compilation results to free disk space.

Without flags, all cache entries are removed. Use --max-size to shrink
the cache to a target size (LRU eviction), or --max-age to remove entries
older than a duration. Both flags can be combined.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		maxSizeStr, _ := cmd.Flags().GetString("max-size")
		maxAgeStr, _ := cmd.Flags().GetString("max-age")

		var maxSize int64
		if maxSizeStr != "" {
			sz, err := internal.ParseSize(maxSizeStr)
			if err != nil {
				return fmt.Errorf("--max-size: %w", err)
			}
			maxSize = sz
		}

		var maxAge time.Duration
		if maxAgeStr != "" {
			d, err := time.ParseDuration(maxAgeStr)
			if err != nil {
				return fmt.Errorf("--max-age: %w", err)
			}
			maxAge = d
		}

		stores, err := runner.LoadStores()
		if err != nil {
			return err
		}
		if len(stores) == 0 {
			fmt.Println("No cache stores configured.")
			return nil
		}

		evictAll := maxSize == 0 && maxAge == 0
		if evictAll {
			maxSize = 1 // evict until ≤ 1 byte, effectively everything
		}

		for _, s := range stores {
			before, beforeSize, err := s.Stats()
			if err != nil {
				return err
			}

			if err := s.Clean(maxSize, maxAge); err != nil {
				return fmt.Errorf("clean %s: %w", s.Dir(), err)
			}

			after, afterSize, err := s.Stats()
			if err != nil {
				return err
			}
			fmt.Printf("%s: %d → %d entries, %s → %s\n",
				s.Dir(),
				before, after,
				formatSize(beforeSize), formatSize(afterSize))
		}
		return nil
	},
}

func init() {
	cleanCmd.Flags().String("max-size", "", "target cache size (e.g. 5G, 500M)")
	cleanCmd.Flags().String("max-age", "", "remove entries older than duration (e.g. 720h for 30 days)")
	rootCmd.AddCommand(cleanCmd)
}
