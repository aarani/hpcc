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
	"github.com/aarani/hpcc/internal/scheduler"
	"github.com/aarani/hpcc/internal/tracing"
	"github.com/spf13/cobra"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

var schedulerCmd = &cobra.Command{
	Use:   "scheduler",
	Short: "Start the scheduler",
	Long: `Start the central scheduler that routes compile jobs to workers.

The scheduler is a lightweight lookup service. Clients call Route() to
get a worker address and a signed task JWT; the compile RPC goes directly
from the client to the worker. The scheduler never touches compile
payloads or artifact bytes.

Workers register on startup and send periodic heartbeats. The scheduler
tracks their load, available images, and active VMs to make routing
decisions (image-digest match, lowest load).

Requires TLS (cert_file, key_file) and at least one auth method
(worker_token for workers, jwks for users) in the config file.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		configPath, _ := cmd.Flags().GetString("config")
		if configPath == "" {
			p, err := scheduler.DefaultConfigPath()
			if err != nil {
				return err
			}
			configPath = p
		}

		cfg, err := scheduler.LoadConfig(configPath)
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

		s, err := scheduler.NewScheduler(cfg)
		if err != nil {
			return err
		}

		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()

		metrics.SetComponent("scheduler")
		metricsResult, err := metrics.Init(ctx, metrics.Options{
			ServiceName:      "hpcc-scheduler",
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

		// Tracing is a no-op unless OTEL_EXPORTER_OTLP_ENDPOINT (or the
		// trace-specific variant) is set. otelgrpc's stats handler
		// creates a root span per inbound RPC so scheduler routing
		// decisions appear alongside the worker spans the client
		// dialled separately.
		tracingShutdown, err := tracing.Init(ctx, "hpcc-scheduler")
		if err != nil {
			return err
		}
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = tracingShutdown(shutdownCtx)
		}()

		srv := grpc.NewServer(
			grpc.Creds(credentials.NewTLS(tlsCfg)),
			grpc.StatsHandler(otelgrpc.NewServerHandler()),
		)
		gen.RegisterSchedulerServiceServer(srv, s)

		lis, err := net.Listen("tcp", cfg.Listen)
		if err != nil {
			return err
		}

		errCh := make(chan error, 2)
		go func() {
			zap.S().Infof("scheduler listening on %s", lis.Addr())
			errCh <- srv.Serve(lis)
		}()
		if cfg.MetricsListen != "" {
			go func() {
				zap.S().Infof("scheduler /metrics on %s", cfg.MetricsListen)
				errCh <- metrics.ServePrometheus(ctx, cfg.MetricsListen, metricsResult.PromHandler)
			}()
		}

		select {
		case err := <-errCh:
			srv.GracefulStop()
			return err
		case <-ctx.Done():
			zap.S().Infof("scheduler: shutting down")
			srv.GracefulStop()
			return nil
		}
	},
}

func init() {
	schedulerCmd.Flags().String("config", "", "path to scheduler config file")
	rootCmd.AddCommand(schedulerCmd)
}
