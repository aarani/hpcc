/*
Copyright © 2026 Afshin Arani <afshin@arani.dev>
*/
package cmd

import (
	"context"
	"crypto/tls"
	"net"
	"os/signal"
	"syscall"
	"time"

	"github.com/aarani/hpcc/internal/metrics"
	"github.com/aarani/hpcc/internal/protocol/gen"
	"github.com/aarani/hpcc/internal/tracing"
	"github.com/aarani/hpcc/internal/worker"
	"github.com/spf13/cobra"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

var workerCmd = &cobra.Command{
	Use:   "worker",
	Short: "Start a worker",
	Long: `Start a worker daemon that accepts Compile RPCs from clients and
maintains the scheduler liaison loop.

The worker does two things in parallel:

  1. Listens for inbound Compile RPCs from clients on the configured TLS
     address. Clients are routed here by the scheduler, which hands them
     this worker's address and the SHA-256 fingerprint of the cert below
     for pinning.

  2. Outbound, dials the scheduler, authenticates with the static worker
     token, registers, and heartbeats every ~10s with current capacity
     and active VM list.

Both run for the lifetime of the process; either failing terminates the
daemon. SIGINT/SIGTERM trigger a graceful gRPC shutdown.

Requires TLS (cert_file, key_file) and a valid scheduler.url +
scheduler.worker_token in the config file. The runtime.handler value
selects which sandbox backend is used; for development the deliberately
awful "really_really_dangerous" handler runs every compile as a child
of the worker process with no isolation at all.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		configPath, _ := cmd.Flags().GetString("config")
		if configPath == "" {
			p, err := worker.DefaultConfigPath()
			if err != nil {
				return err
			}
			configPath = p
		}

		cfg, err := worker.LoadConfig(configPath)
		if err != nil {
			return err
		}
		if err := cfg.Validate(); err != nil {
			return err
		}

		cert, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		if err != nil {
			return err
		}
		tlsCfg := &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS13,
		}

		w, err := worker.NewWorker(cfg)
		if err != nil {
			return err
		}

		// Catch SIGINT/SIGTERM so a kill returns control to the deferred
		// graceful-stop instead of dropping in-flight RPCs.
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()

		metrics.SetComponent("worker")
		metricsResult, err := metrics.Init(ctx, metrics.Options{
			ServiceName:      "hpcc-worker",
			PrometheusReader: cfg.MetricsListen != "",
		})
		if err != nil {
			return err
		}
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = metricsResult.Shutdown(shutdownCtx)
		}()
		if err := metrics.RegisterWorkerInflight(w.Inflight); err != nil {
			return err
		}
		if pool := w.PooledRuntime(); pool != nil {
			if err := metrics.RegisterWorkerContainers(pool.EntriesByTenant); err != nil {
				return err
			}
		}

		// Tracing is a no-op unless OTEL_EXPORTER_OTLP_ENDPOINT (or the
		// trace-specific variant) is set. otelgrpc's stats handler
		// creates a root span per inbound RPC and extracts an upstream
		// traceparent if the client sent one, so worker spans nest
		// under client spans for end-to-end visibility.
		tracingShutdown, err := tracing.Init(ctx, "hpcc-worker")
		if err != nil {
			return err
		}
		defer func() {
			// Allow up to a few seconds for in-flight batched spans to
			// drain; the parent ctx may already be cancelled, so
			// shutdown gets its own context.
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = tracingShutdown(shutdownCtx)
		}()

		srv := grpc.NewServer(
			grpc.Creds(credentials.NewTLS(tlsCfg)),
			grpc.MaxRecvMsgSize(gen.MaxCompileMessageBytes),
			grpc.MaxSendMsgSize(gen.MaxCompileMessageBytes),
			grpc.StatsHandler(otelgrpc.NewServerHandler()),
		)
		gen.RegisterWorkerServiceServer(srv, w)

		lis, err := net.Listen("tcp", cfg.Listen)
		if err != nil {
			return err
		}

		// Start serving before the scheduler liaison so clients routed
		// to this worker (immediately after RegisterWorker returns)
		// can connect without a transient connection-refused window.
		errCh := make(chan error, 3)
		go func() {
			zap.S().Infof("worker listening on %s", lis.Addr())
			errCh <- srv.Serve(lis)
		}()
		go func() {
			errCh <- w.Run(ctx)
		}()
		if cfg.MetricsListen != "" {
			go func() {
				zap.S().Infof("worker /metrics on %s", cfg.MetricsListen)
				errCh <- metrics.ServePrometheus(ctx, cfg.MetricsListen, metricsResult.PromHandler)
			}()
		}

		select {
		case err := <-errCh:
			srv.GracefulStop()
			return err
		case <-ctx.Done():
			zap.S().Infof("worker: shutting down")
			srv.GracefulStop()
			return nil
		}
	},
}

func init() {
	workerCmd.Flags().String("config", "", "path to worker config file")
	rootCmd.AddCommand(workerCmd)
}
