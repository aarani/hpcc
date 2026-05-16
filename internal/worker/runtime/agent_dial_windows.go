//go:build windows

package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/Microsoft/hcsshim"
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
// per-container UVM identifier. Resolved from the containerd
// container ID by lookupContainerUVMID (below); the dialer takes
// the GUID as a parameter so the dial logic can be unit-tested
// independent of that lookup.
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

// dialContainerAgent resolves the utility VM's HvSocket address for
// the given containerd container ID and dials its hpcc-agent. Called
// from Hcsshim.Start after Task.Start in Hyper-V isolation mode.
// Wraps the two-step lookup-then-dial sequence so callers (and the
// non-Windows stub) have a single entry point.
func (h *Hcsshim) dialContainerAgent(ctx context.Context, containerID string) (*grpc.ClientConn, error) {
	vmID, err := lookupContainerUVMID(containerID)
	if err != nil {
		return nil, fmt.Errorf("lookup UVM ID for container %q: %w", containerID, err)
	}
	return dialAgentHvsock(ctx, vmID)
}

// lookupContainerUVMID returns the Hyper-V VM GUID that runhcs
// allocated to host the named container. The containerd-shim-runhcs
// names the utility VM compute-system "<container-id>@vm"
// (see hcsshim's cmd/containerd-shim-runhcs-v1/task_hcs.go) and the
// RuntimeID property of that compute system is the Hyper-V VM GUID
// HvSocket addresses by. Two-call sequence:
//
//   - hcsshim.GetContainers queries the HCS service for a system
//     matching the given ID; returns ContainerProperties whose
//     RuntimeID field is the VM GUID.
//   - The returned RuntimeID is what dialAgentHvsock takes.
//
// The "@vm" naming is a runhcs-internals contract — if a future
// shim release changes it, this helper has to adapt. Documenting
// it explicitly here so the assumption isn't buried in a magic
// string.
func lookupContainerUVMID(containerID string) (guid.GUID, error) {
	const uvmSuffix = "@vm"
	uvmID := containerID + uvmSuffix
	props, err := hcsshim.GetContainers(hcsshim.ComputeSystemQuery{
		IDs: []string{uvmID},
	})
	if err != nil {
		return guid.GUID{}, fmt.Errorf("hcsshim.GetContainers(%q): %w", uvmID, err)
	}
	if len(props) == 0 {
		return guid.GUID{}, fmt.Errorf("no compute system %q (utility VM not allocated? containerd-shim-runhcs likely renamed the @vm suffix)", uvmID)
	}
	if props[0].RuntimeID == (guid.GUID{}) {
		return guid.GUID{}, fmt.Errorf("compute system %q has zero RuntimeID (not a Hyper-V isolated container)", uvmID)
	}
	return props[0].RuntimeID, nil
}
