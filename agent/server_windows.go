//go:build windows

package main

import (
	"fmt"

	"github.com/Microsoft/go-winio"
	"google.golang.org/grpc"

	agentpb "github.com/aarani/hpcc/proto/agent"
)

// stagingRoot is the per-container scratch dir the cross-platform
// server code joins with src/<ExecID> and out/<ExecID> to lay out
// per-Exec trees. Sits on the Windows container's writable layer.
// Picked under C:\ rather than %TEMP% so the path is the same
// whether the agent runs as ContainerUser (default) or a custom
// USER from the image — %TEMP% expands differently per user, and
// the host-side runner reasons about a single in-container layout.
const stagingRoot = `C:\hpcc`

// serveAgent brings up the gRPC service on AF_HYPERV (the Hyper-V
// "hvsock" socket family) and blocks until the listener fails or
// the process exits. Same wire schema and port number as the Linux
// AF_VSOCK build — the only thing that changes is the transport
// layer.
//
// Why HvSocket: the host-guest channel for a Hyper-V-isolated
// Windows container is a single hvsock device. Routing file +
// stdio + result over that one channel is the §4.1 "no SMB across
// the partition boundary" story (docs/plan/phase-4-distributed.md
// §4.1.1, "Why not VSMB"). VMID wildcard accepts a connection from
// the partition parent (the host worker); ServiceID encodes the
// shared vsock-style port number so a single agentVsockPort
// constant covers both transports.
func serveAgent() error {
	addr := &winio.HvsockAddr{
		VMID:      winio.HvsockGUIDWildcard(),
		ServiceID: winio.VsockServiceID(agentVsockPort),
	}
	lis, err := winio.ListenHvsock(addr)
	if err != nil {
		return fmt.Errorf("hvsock listen on service %s: %w", addr.ServiceID, err)
	}
	s := grpc.NewServer()
	agentpb.RegisterAgentServiceServer(s, &execServer{})
	return s.Serve(lis)
}
