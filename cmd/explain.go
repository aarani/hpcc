/*
Copyright © 2026 Afshin Arani <afshin@arani.dev>
*/
package cmd

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/aarani/hpcc/internal/explain"
	"github.com/spf13/cobra"
)

var explainCmd = &cobra.Command{
	Use:   "explain <source-file>",
	Short: "Explain the last compile outcome for a source file",
	Long: `Show what hpcc did the last time it compiled <source-file>, and — when
the outcome was a cache miss — name the specific input that changed
since the prior compile.

The daemon writes one explain record per compile attempt under
` + "`$HPCC_EXPLAIN_DIR`" + ` (or the platform's user-cache directory by default).
On every later run of this command the record is read back and
diffed against the prior record to produce structured reasons:

  - compiler            — the compiler binary's identity changed
                          (rebuilt, replaced, symlink moved)
  - flags               — the cache-key-relevant flag set changed
                          (added/removed/reordered flags)
  - source              — the source file's bytes changed
  - header <path>       — a specific transitively-included header
                          changed, was added, or was removed
  - image               — the OCI toolchain image digest the
                          dispatcher pins changed

Only source-file lookup is supported in the MVP — pass the .c/.cpp
path, not the .o output path. MSVC compiles don't yet emit per-
header attribution because hpcc doesn't currently capture the
/showIncludes stream into the explain record.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dir, err := explain.DefaultDir()
		if err != nil {
			return fmt.Errorf("locate explain dir: %w", err)
		}
		store, err := explain.NewDiskStore(dir, 0)
		if err != nil {
			return fmt.Errorf("open explain store: %w", err)
		}

		src, err := filepath.Abs(args[0])
		if err != nil {
			return fmt.Errorf("resolve %q: %w", args[0], err)
		}

		latest, err := store.Get(src)
		if err != nil {
			return fmt.Errorf("read explain record for %s: %w", src, err)
		}
		if latest == nil {
			fmt.Printf("no explain record for %s\n", src)
			fmt.Println("the daemon writes records on every compile; if the file has been")
			fmt.Println("compiled via hpcc since the daemon started, the record will appear")
			fmt.Println("under", dir)
			return nil
		}

		renderRecord(cmd.OutOrStdout(), latest)
		return nil
	},
}

func renderRecord(w interface{ Write([]byte) (int, error) }, r *explain.Record) {
	bw := &lineWriter{w: w}
	bw.line("source:  %s", r.SourcePath)
	if r.OutputPath != "" {
		bw.line("output:  %s", r.OutputPath)
	}
	bw.line("outcome: %s", r.Outcome)
	if !r.Timestamp.IsZero() {
		bw.line("when:    %s", r.Timestamp.Format(time.RFC3339))
	}
	if r.CacheKey != "" {
		bw.line("key:     %s", r.CacheKey)
	}
	if r.CompilerBinary != "" {
		bw.line("compiler:%s", " "+r.CompilerBinary)
	}
	if r.ImageDigest != "" {
		bw.line("image:   %s", r.ImageDigest)
	}
	bw.line("")

	switch r.Outcome {
	case explain.OutcomeLocalHit, explain.OutcomeRemote:
		bw.line("This compile was served from cache — no work re-run.")
	case explain.OutcomeBypass:
		bw.line("This compile bypassed the cache (argv carried a flag")
		bw.line("hpcc can't currently round-trip, e.g. -save-temps).")
	case explain.OutcomeLocalInvoke:
		if len(r.Diffs) > 0 {
			bw.line("This compile was a miss. Inputs that changed since")
			bw.line("the previous compile of this source:")
			bw.line("")
			renderDiffs(bw, r.Diffs)
		} else {
			bw.line("This compile was a miss. No prior explain record was")
			bw.line("available to diff against — either this is the first")
			bw.line("hpcc-handled compile of this source, or the prior")
			bw.line("record was evicted from the explain store.")
		}
	case explain.OutcomeError:
		bw.line("This compile failed.")
	}

	if n := len(r.HeaderHashes); n > 0 {
		bw.line("")
		bw.line("tracked headers (%d):", n)
		for path := range r.HeaderHashes {
			bw.line("  %s", path)
		}
	}
}

// renderDiffs prints one human line per Diff entry.
func renderDiffs(bw *lineWriter, diffs []explain.Diff) {
	for _, d := range diffs {
		switch d.Kind {
		case explain.DiffCompiler:
			bw.line("  - compiler binary identity changed")
		case explain.DiffFlags:
			bw.line("  - cache-key-relevant flags changed")
		case explain.DiffSource:
			bw.line("  - source file bytes changed")
		case explain.DiffHeader:
			switch {
			case d.Before == "":
				bw.line("  - header added:   %s", d.Path)
			case d.After == "":
				bw.line("  - header removed: %s", d.Path)
			default:
				bw.line("  - header changed: %s", d.Path)
			}
		case explain.DiffImage:
			bw.line("  - OCI toolchain image digest changed")
		default:
			bw.line("  - %s changed", d.Kind)
		}
	}
}

// lineWriter writes printf-formatted lines to w, appending "\n" so
// callers don't have to.
type lineWriter struct {
	w interface{ Write([]byte) (int, error) }
}

func (b *lineWriter) line(format string, args ...any) {
	s := fmt.Sprintf(format, args...)
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	_, _ = b.w.Write([]byte(s))
}

func init() {
	rootCmd.AddCommand(explainCmd)
}
