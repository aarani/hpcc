/*
Copyright © 2026 Afshin Arani <afshin@arani.dev>
*/
package cmd

import (
	"context"
	"crypto/tls"
	"log"
	"net"
	"os/signal"
	"syscall"

	"github.com/aarani/hpcc/internal/protocol/gen"
	"github.com/aarani/hpcc/internal/worker"
	"github.com/spf13/cobra"
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

		srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsCfg)))
		gen.RegisterWorkerServiceServer(srv, w)

		lis, err := net.Listen("tcp", cfg.Listen)
		if err != nil {
			return err
		}

		// Catch SIGINT/SIGTERM so a kill returns control to the deferred
		// graceful-stop instead of dropping in-flight RPCs.
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()

		// Start serving before the scheduler liaison so clients routed
		// to this worker (immediately after RegisterWorker returns)
		// can connect without a transient connection-refused window.
		errCh := make(chan error, 2)
		go func() {
			log.Printf("worker listening on %s", lis.Addr())
			errCh <- srv.Serve(lis)
		}()
		go func() {
			errCh <- w.Run(ctx)
		}()

		select {
		case err := <-errCh:
			srv.GracefulStop()
			return err
		case <-ctx.Done():
			log.Printf("worker: shutting down")
			srv.GracefulStop()
			return nil
		}
	},
}

func init() {
	workerCmd.Flags().String("config", "", "path to worker config file")
	rootCmd.AddCommand(workerCmd)
}
