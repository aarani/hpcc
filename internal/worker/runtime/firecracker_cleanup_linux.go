//go:build linux

package runtime

import (
	"path/filepath"
	"syscall"
)

// cleanupJailerMounts lazy-unmounts anything jailer left behind
// inside chrootDir. jailer's setup does:
//
//  1. mount --bind <chrootRoot> <chrootRoot> (in the host ns,
//     inheriting whatever propagation flags the host's root has)
//  2. unshare CLONE_NEWNS into a new mount ns
//  3. mount tmpfs over <chrootRoot>/run inside the new ns
//
// On systems where the host root mount is MS_SHARED (default on
// most distros — Ubuntu/Debian/Fedora all do this), step (3)'s
// tmpfs propagates back into the host's namespace through the
// bind mount from step (1). When firecracker exits, jailer's
// mount ns dies and its private mounts go with it, but the
// propagated tmpfs and the original bind mount remain in the host
// ns — `os.RemoveAll` doesn't unmount, so those mountpoints linger
// across runs and slowly poison the propagation graph (later VMs
// get inconsistent visibility into their own fresh tmpfses).
//
// MNT_DETACH (lazy unmount) handles the "busy" case gracefully —
// any straggler reference is released as soon as it's closed; we
// don't have to enumerate them. Errors are ignored: a missing
// mount means there's nothing to clean up here, which is the
// success case.
func cleanupJailerMounts(chrootDir string) {
	for _, p := range []string{
		filepath.Join(chrootDir, "root", "run"),
		filepath.Join(chrootDir, "root"),
		chrootDir,
	} {
		_ = syscall.Unmount(p, syscall.MNT_DETACH)
	}
}
