//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// setupInit on Windows is a much smaller affair than on Linux. The
// container's kernel filesystems are already wired by the host
// (HCS does all that); there's no devtmpfs/proc/sysfs work to do
// and the agent is not PID 1 of the container OS in the
// Linux-init sense — it's just the OCI entrypoint hcsshim's spec
// pivots to.
//
// What we DO need: ensure stagingRoot and its src/out children
// exist before the gRPC server starts handing per-Exec dirs out.
// The container's writable layer covers this — MkdirAll succeeds
// on first boot of a fresh container and is a no-op on subsequent
// agent restarts inside the same warm container.
func setupInit() error {
	for _, d := range []string{
		stagingRoot,
		filepath.Join(stagingRoot, "src"),
		filepath.Join(stagingRoot, "out"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}
	return nil
}
