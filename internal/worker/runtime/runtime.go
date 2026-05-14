package runtime

import (
	"context"
	"io"

	"github.com/aarani/hpcc/internal/protocol/gen"
)

// Runtime owns per-tenant compile sandboxes. Implementations are
// backend-specific — a raw Firecracker driver on Linux (hpcc owns image
// pull, squashfs rootfs build, VMM lifecycle, and the host-side vsock channel
// to the in-VM agent), containerd + hcsshim (Hyper-V isolation) on Windows
// — but the surface is the same: start a container from a prepared image,
// dispatch Execs into it, stop it. Image preparation (pulling,
// pause/agent injection) is the image package's job and is out of scope
// here.
type Runtime interface {
	// Start launches a new per-tenant container from a prepared image
	// (pause binary already injected as PID 1) and waits until it's
	// ready to accept Execs. The returned Container is owned by the
	// caller and must be Stopped to release the underlying VM.
	Start(ctx context.Context, spec ContainerSpec) (Container, error)

	// Close releases client-level resources (containerd connection).
	// Outstanding Containers must be Stopped first.
	Close() error
}

// ContainerSpec describes one per-tenant container. The runtime
// translates this into a backend-native shape: a Firecracker VMM
// configuration on Linux, an OCI runtime spec for the containerd +
// hcsshim path on Windows.
//
// Per-RPC source/output mount paths are deliberately NOT here — they
// live on ExecRequest instead. Containers are reusable across compiles
// (see PooledRuntime), so the spec captures only stable, tenant-level
// shape (identity, sizing); the mounts that change per compile bind at
// Exec time.
type ContainerSpec struct {
	ID          string // worker-unique container id, also the VM id reported in heartbeats
	TenantID    string
	ImageDigest string // prepared-image digest (output of image.Store)

	VCPUs       int32
	MemoryBytes int64
}

// Container is one running per-tenant sandbox. One container == one VM
// (Firecracker microVM on Linux, Hyper-V utility VM on Windows), so this
// is also what the worker reports in WorkerHeartbeat.active_vms.
//
// Methods are safe for concurrent use; Exec calls fan out as separate
// Task.Execs against the same underlying task.
type Container interface {
	ID() string
	TenantID() string
	ImageDigest() string
	State() gen.VMState

	// Exec runs one compiler invocation inside the container and blocks
	// until the process exits, ctx is cancelled, or the container dies.
	// On ctx cancellation the in-container process is killed via the
	// shim's process API and ctx.Err() is returned.
	Exec(ctx context.Context, req ExecRequest) (ExecResult, error)

	// Stop tears the container down (SIGTERM → grace → SIGKILL) and
	// reaps the VM. Idempotent; safe to call after the container has
	// already exited on its own.
	Stop(ctx context.Context) error
}

// ExecRequest is one Task.Exec — a fully-resolved toolchain invocation.
// No shell; argv goes straight to execve in the guest.
//
// SrcHostPath / OutHostPath are the per-Exec bind-mount targets. The
// runtime is responsible for making them visible at /src and /out
// inside the sandbox for the lifetime of this Exec; an empty string
// means "no such mount." Real backends (raw Firecracker on Linux,
// hcsshim on Windows) attach these at Exec time so a pooled container
// can serve compiles for different RPC tmpdirs without restart.
type ExecRequest struct {
	ExecID string // unique within the container; surfaces in backend events (vsock RPC id on Linux, containerd task id on Windows)
	Argv   []string
	Env    []string
	Cwd    string

	SrcHostPath string
	OutHostPath string

	Stdin          io.Reader // optional
	Stdout, Stderr io.Writer // optional; nil discards
}

// ExecResult is what Exec returns when the process exited cleanly under
// the runtime's control. A non-zero ExitCode is *not* an error — the
// caller decides whether a compiler failure is fatal.
type ExecResult struct {
	ExitCode int
}
