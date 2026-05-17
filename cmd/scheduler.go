/*
Copyright © 2026 Afshin Arani <afshin@arani.dev>
*/
package cmd

import (
	"crypto/tls"
	"net"

	"github.com/aarani/hpcc/internal/protocol/gen"
	"github.com/aarani/hpcc/internal/scheduler"
	"github.com/spf13/cobra"
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

		srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsCfg)))
		gen.RegisterSchedulerServiceServer(srv, s)

		lis, err := net.Listen("tcp", cfg.Listen)
		if err != nil {
			return err
		}

		zap.S().Infof("scheduler listening on %s", lis.Addr())
		return srv.Serve(lis)
	},
}

func init() {
	schedulerCmd.Flags().String("config", "", "path to scheduler config file")
	rootCmd.AddCommand(schedulerCmd)
}
