//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// initMount is one filesystem the agent brings up at boot. Order in
// bootMounts matters: /dev (devtmpfs) has to land before /dev/pts and
// /dev/shm so we can mkdir those under a writable parent — the
// rootfs itself is read-only.
type initMount struct {
	source string
	target string
	fstype string
	flags  uintptr
	data   string
}

// bootMounts is the standard kernel filesystem set plus the writable
// scratch tmpfses every compile relies on. Everything writable lives
// in tmpfs (= guest RAM) by design — RW rootfs would force per-VM
// copies and break the "image digest is the toolchain identity" rule.
//
// The MS_NOSUID/NODEV/NOEXEC flag combinations follow the
// systemd/mkinitcpio defaults for the same mountpoints: tighten
// where the kernel never produces files we need to suid, exec, or
// treat as device nodes.
var bootMounts = []initMount{
	{source: "proc", target: "/proc", fstype: "proc",
		flags: syscall.MS_NOSUID | syscall.MS_NOEXEC | syscall.MS_NODEV},
	{source: "sysfs", target: "/sys", fstype: "sysfs",
		flags: syscall.MS_NOSUID | syscall.MS_NOEXEC | syscall.MS_NODEV | syscall.MS_RDONLY},
	{source: "devtmpfs", target: "/dev", fstype: "devtmpfs",
		flags: syscall.MS_NOSUID, data: "mode=0755"},
	{source: "devpts", target: "/dev/pts", fstype: "devpts",
		flags: syscall.MS_NOSUID | syscall.MS_NOEXEC,
		data:  "newinstance,ptmxmode=0666,mode=0620,gid=5"},
	{source: "tmpfs", target: "/dev/shm", fstype: "tmpfs",
		flags: syscall.MS_NOSUID | syscall.MS_NODEV, data: "mode=1777"},
	{source: "tmpfs", target: "/tmp", fstype: "tmpfs",
		flags: syscall.MS_NOSUID | syscall.MS_NODEV, data: "mode=1777"},
	{source: "tmpfs", target: "/run", fstype: "tmpfs",
		flags: syscall.MS_NOSUID | syscall.MS_NODEV, data: "mode=0755"},
}

// stagingDirs sit on the freshly-mounted /run tmpfs. The vsock RPC
// path will create per-Exec subdirectories under src/ and out/ to
// stage source bytes shipped from the host and collect outputs the
// compiler produces.
var stagingDirs = []string{
	"/run/hpcc",
	"/run/hpcc/src",
	"/run/hpcc/out",
}

func setupInit() error {
	for _, m := range bootMounts {
		// Best-effort mkdir for mountpoints whose parent is already
		// writable (e.g. /dev/pts after /dev is up). On the read-only
		// rootfs the mkdir fails with EROFS; that's expected — the
		// underlying mount target almost always exists in practice
		// and the syscall.Mount below will surface a clear ENOENT
		// otherwise.
		_ = os.MkdirAll(m.target, 0o755)
		if err := syscall.Mount(m.source, m.target, m.fstype, m.flags, m.data); err != nil {
			// EBUSY = kernel already mounted this for us. Common for
			// /dev when CONFIG_DEVTMPFS_MOUNT=y (the Firecracker CI
			// vmlinuxes ship with it on). Treat as a successful
			// mount and move on.
			if errors.Is(err, syscall.EBUSY) {
				continue
			}
			return fmt.Errorf("mount %s on %s: %w", m.fstype, m.target, err)
		}
	}
	for _, d := range stagingDirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}
	return nil
}
