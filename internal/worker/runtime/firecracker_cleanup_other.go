//go:build !linux

package runtime

// cleanupJailerMounts is a no-op on non-Linux. The Firecracker
// runtime can't actually run anywhere except Linux (jailer +
// /dev/kvm requirements); this stub just keeps the package
// compiling on Mac dev workstations and the Windows side of the
// repo.
func cleanupJailerMounts(string) {}
