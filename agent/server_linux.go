//go:build linux

package main

import (
	"fmt"

	"github.com/mdlayher/vsock"
	"google.golang.org/grpc"

	agentpb "github.com/aarani/hpcc/proto/agent"
)

// stagingRoot is where per-Exec input/output trees live. Lives on
// the /run tmpfs the agent's init code mounts (see init_linux.go) —
// guest RAM, not the read-only rootfs. The cross-platform server
// code in server.go joins this with src/<ExecID> and out/<ExecID>
// to lay out per-Exec dirs.
const stagingRoot = "/run/hpcc"

// serveAgent brings up the gRPC service on AF_VSOCK and blocks until
// the listener fails or the process exits. Errors are returned to
// main, which logs+exits — there's no graceful-restart story for an
// in-VM PID-1.
func serveAgent() error {
	lis, err := vsock.Listen(agentVsockPort, nil)
	if err != nil {
		return fmt.Errorf("vsock listen on %d: %w", agentVsockPort, err)
	}
	s := grpc.NewServer()
	agentpb.RegisterAgentServiceServer(s, &execServer{})
	return s.Serve(lis)
}
