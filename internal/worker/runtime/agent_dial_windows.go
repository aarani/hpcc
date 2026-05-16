//go:build windows

package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"

	winio "github.com/Microsoft/go-winio"
	"github.com/Microsoft/go-winio/pkg/guid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// dialAgentHvsock opens a gRPC client connection to the
// in-container hpcc-agent over the partition boundary's HvSocket
// transport. The Linux counterpart (`dialAgent` in firecracker.go)
// dials a vsock-over-UDS bridge that the Firecracker VMM exposes;
// the two coexist in the same package because the runtime selection
// happens at Start time, not at compile time.
//
// vmID is the Hyper-V utility VM's GUID — the runhcs shim's
// per-container UVM identifier. Looking that up from a
// containerd.Container handle needs an hcsshim-internals call;
// the planned home for it is a `Hcsshim.containerUVMID` helper
// (TODO, separate change). This dialer takes the GUID as a
// parameter so the dial logic can be unit-tested independent of
// that lookup.
//
// Insecure credentials are correct here: the only thing on the
// other end of HvSocket is the partition's child VM (a fully
// isolated, single-tenant utility VM whose only purpose is running
// this agent), and the host↔guest channel is itself the §4.1
// boundary the audit story rests on. There's no man-in-the-middle
// to defend against on a kernel-level pipe.
func dialAgentHvsock(ctx context.Context, vmID guid.GUID) (*grpc.ClientConn, error) {
	if vmID == (guid.GUID{}) {
		return nil, errors.New("dialAgent: vmID is required (zero GUID would mean wildcard, which is listen-only)")
	}
	addr := &winio.HvsockAddr{
		VMID:      vmID,
		ServiceID: winio.VsockServiceID(agentVsockPort),
	}
	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		// HvsockDialer doesn't accept a context directly on older
		// go-winio releases; emulate one via a goroutine that races
		// the dial against ctx.Done. The library closes the partial
		// connection on cancellation downstream of returning the
		// goroutine's result.
		type dialResult struct {
			conn net.Conn
			err  error
		}
		ch := make(chan dialResult, 1)
		go func() {
			c, err := winio.Dial(ctx, addr)
			ch <- dialResult{c, err}
		}()
		select {
		case r := <-ch:
			return r.conn, r.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	conn, err := grpc.DialContext(ctx, "hvsock://"+vmID.String(),
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		return nil, fmt.Errorf("grpc dial agent on %s: %w", vmID, err)
	}
	return conn, nil
}

