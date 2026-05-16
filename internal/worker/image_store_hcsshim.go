package worker

import (
	"fmt"

	containerd "github.com/containerd/containerd/v2/client"

	"github.com/aarani/hpcc/internal/worker/image"
	"github.com/aarani/hpcc/internal/worker/image/cdimage"
)

// newHcsshimImageStore wires a cdimage.Store onto the same containerd
// daemon the hcsshim runtime drives. The runtime dials a containerd
// client of its own; the image store needs its own client so its
// lifecycle (and namespace) are independent of the runtime's. The
// pause-binary paths come from worker.toml's [image] block —
// hcsshim deployments must point pause_windows_amd64 at the prebuilt
// hpcc-pause.exe so cdimage can inject it as PID 1 when preparing an
// image (§4.3.1).
func newHcsshimImageStore(cfg Config) (image.Store, error) {
	addr := cfg.Runtime.Hcsshim.Address
	if addr == "" {
		return nil, fmt.Errorf("runtime.hcsshim.address is required when runtime.handler = %q", cfg.Runtime.Handler)
	}
	ns := cfg.Runtime.Hcsshim.Namespace
	if ns == "" {
		// Mirror the runtime's default so images prepared here are
		// visible to the runtime's GetImage call without an extra knob.
		ns = "hpcc"
	}
	cli, err := containerd.New(addr, containerd.WithDefaultNamespace(ns))
	if err != nil {
		return nil, fmt.Errorf("dial containerd at %q: %w", addr, err)
	}
	return &cdimage.Store{
		Client: cli,
		Pause: cdimage.PauseBinaries{
			LinuxAmd64:   cfg.Image.PauseLinuxAmd64,
			LinuxArm64:   cfg.Image.PauseLinuxArm64,
			WindowsAmd64: cfg.Image.PauseWindowsAmd64,
		},
		// Same snapshotter the runtime will ask its containers to
		// fork off of. Empty = containerd's default, which is what
		// the runtime also defaults to ("windows" on Windows).
		Snapshotter: cfg.Runtime.Hcsshim.Snapshotter,
	}, nil
}
