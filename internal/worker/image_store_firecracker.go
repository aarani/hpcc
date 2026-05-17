package worker

import (
	"fmt"

	"github.com/aarani/hpcc/internal/worker/image"
	"github.com/aarani/hpcc/internal/worker/image/rootfs"
)

// newFirecrackerImageStore wires a rootfs.Store onto the same
// CacheDir the Firecracker runtime resolves prepared squashfs images
// from. The runtime opens prepared rootfs files by path (PathFor) at
// Start time; the Store hands PullImage / GetExistingImages /
// UntagImage to the worker's image-catalogue path so cold-pull, the
// bootstrap "what's already on disk" scan, and idle-eviction all work
// the same way the hcsshim path does. RootfsDir mirrors
// rootfs.Store.CacheDir on purpose (see FirecrackerConfig comment) —
// both point at the same directory so a file PullImage publishes is
// the same file the runtime later opens.
func newFirecrackerImageStore(cfg Config) (image.Store, error) {
	if cfg.Runtime.Firecracker.RootfsDir == "" {
		return nil, fmt.Errorf("runtime.firecracker.rootfs_dir is required when runtime.handler = %q", cfg.Runtime.Handler)
	}
	return &rootfs.Store{
		CacheDir: cfg.Runtime.Firecracker.RootfsDir,
		Agent: rootfs.AgentBinaries{
			LinuxAmd64: cfg.Image.AgentLinuxAmd64,
			LinuxArm64: cfg.Image.AgentLinuxArm64,
		},
	}, nil
}
