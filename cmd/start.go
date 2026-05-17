/*
Copyright © 2026 Afshin Arani <afshin@arani.dev>
*/
package cmd

import (
	"context"
	"time"

	"github.com/aarani/hpcc/internal/daemon"
	"github.com/aarani/hpcc/internal/metrics"
	"github.com/spf13/cobra"
)

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the daemon",
	Long: `Start a long-running daemon that handles compilations on behalf of
short-lived hpcc invocations.

Without the daemon, every hpcc call (whether via "hpcc wrap" or a symlink
shim) is a standalone process: it loads the config, initialises the cache
stores, preprocesses, and invokes the compiler — then exits. The daemon
amortises that startup cost by keeping compiler contexts, cache handles,
and the preprocessor warm across invocations.

When the daemon is running, hpcc clients connect over a local TCP socket,
authenticate with a per-session token, and send their compile request as
a length-prefixed protobuf message. The daemon resolves relative paths,
runs the cache lookup, invokes the compiler on a miss, writes the output
file, and streams stdout/stderr/exit-code back over the same connection.
Identical in-flight compilations (same preprocessed content hash) are
coalesced via singleflight so the compiler runs once even when a parallel
build submits the same translation unit concurrently.

If no daemon is running, hpcc falls back to in-process compilation
transparently — the daemon is an optimisation, not a requirement.

The daemon binds to an ephemeral localhost port and writes its PID, port,
and auth token to ` + "`$XDG_CONFIG_HOME/hpcc/daemon.json`" + ` (or the
platform equivalent). Use --force to replace a stale or misbehaving
daemon without manually cleaning up that file.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		isForceStart, err := cmd.Flags().GetBool("force")
		if err != nil {
			isForceStart = false
		}

		// Daemons run on client machines that typically can't be
		// scraped, so metrics export here is gated entirely on the
		// standard OTEL env vars — no /metrics listener. Without a
		// configured OTLP endpoint Init returns a working noop
		// MeterProvider so call-site Add()/Record() pays nothing.
		metrics.SetComponent("daemon")
		metricsResult, err := metrics.Init(context.Background(), metrics.Options{
			ServiceName: "hpcc-daemon",
		})
		if err != nil {
			return err
		}
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = metricsResult.Shutdown(shutdownCtx)
		}()

		d := daemon.NewDefaultDaemon()
		if err := metrics.RegisterDaemonInflight(d.Inflight); err != nil {
			return err
		}
		return d.Run(isForceStart)
	},
}

func init() {
	startCmd.Flags().Bool("force", false, "Force start even if the daemon is already running")
	rootCmd.AddCommand(startCmd)
}
