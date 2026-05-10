// Package image is the worker's prepared-image abstraction. It defines
// the per-runtime contract — pull, list, evict — and leaves the
// preparation step (what gets injected, what the on-disk artifact looks
// like) to backend-specific subpackages:
//
//   - cdimage   — Linux/Windows containerd image store. Injects the
//     hpcc pause binary as PID 1 and registers a prepared image record
//     under a synthetic "prepared.hpcc.local/img:<digest>" name. Used
//     on Windows under hcsshim's Hyper-V isolation.
//   - rootfs    — Linux raw-Firecracker rootfs builder. Pulls layers,
//     flattens them onto a sparse ext4 file, and injects the in-VM
//     hpcc-agent so the host-side vsock client has something to talk
//     to. Used on Linux under the raw Firecracker driver.
//
// Image identity is stable across backends: the user-supplied image
// digest is what the worker advertises, what the scheduler routes on,
// and what the cache catalogue keys. Each backend keeps its own
// prepared variant locally and reverses the digest→artifact mapping
// internally.
package image

import "context"

// Store is the worker-side prepared-image catalogue. It owns the
// translation from "user image digest" to "ready-to-run sandbox
// artifact" — an OCI image record on the containerd path, an ext4
// rootfs file on the Firecracker path — including the per-backend
// injection step (pause binary or hpcc-agent).
//
// All methods are keyed by the *user* image digest. The prepared
// artifact's own digest is an implementation detail of the backend and
// is never exposed to the worker.
//
// A nil Store is the dev-mode "no image store" sentinel — the worker
// short-circuits Pull to a no-op so handlers like the dangerous
// host-exec runtime can run without containerd or a rootfs builder.
type Store interface {
	// GetExistingImages enumerates the user-image digests of every
	// prepared artifact already present locally. Used at worker
	// startup to pre-populate the in-memory image catalogue without
	// re-pulling.
	GetExistingImages(ctx context.Context) ([]string, error)

	// PullImage pulls imagePath from a registry, verifies the
	// resolved digest matches expectedDigest, and produces the
	// backend-specific prepared artifact. Idempotent against an
	// already-prepared digest: re-running is allowed (and may
	// overwrite the prepared record if the injected binary changed).
	PullImage(ctx context.Context, imagePath, expectedDigest string) error

	// UntagImage drops the prepared artifact for userDigest from
	// local storage. Idempotent — a missing artifact is not an
	// error, so the eviction loop is safe to run against catalogue
	// drift. The underlying blobs (containerd content, ext4 file)
	// become reclaimable on the backend's next GC pass.
	UntagImage(ctx context.Context, userDigest string) error
}
