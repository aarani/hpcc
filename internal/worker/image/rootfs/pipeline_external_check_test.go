package rootfs

import (
	"archive/tar"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// External-validator tests for the production buildSquashfs
// pipeline. Skip when unsquashfs is absent so macOS-only developer
// machines don't hard-fail; CI installs squashfs-tools and runs
// these for real, catching format regressions long before the
// firecracker e2e job tries to boot a kernel against a broken
// rootfs.

// TestBuildSquashfs_externalRoundTrip drives the production
// buildSquashfs function with the same fixture tar used by the
// in-process test, extracts the result with unsquashfs, and
// asserts tree contents, agent injection,
// hardlink-shares-inode, and the standard mountpoint dirs.
func TestBuildSquashfs_externalRoundTrip(t *testing.T) {
	bin, err := exec.LookPath("unsquashfs")
	if err != nil {
		t.Skip("unsquashfs not installed; install squashfs-tools to run")
	}

	tarBytes := makeFixtureTar(t)
	out := filepath.Join(t.TempDir(), "rootfs.sqsh")
	agentMarker := []byte("REAL-HPCC-AGENT-MARKER\n")

	if err := buildSquashfs(bytes.NewReader(tarBytes), agentMarker, out); err != nil {
		t.Fatalf("buildSquashfs: %v", err)
	}

	extracted := filepath.Join(t.TempDir(), "extract")
	if combined, err := exec.Command(bin, "-d", extracted, "-no-progress", out).CombinedOutput(); err != nil {
		t.Fatalf("unsquashfs failed: %v\n%s", err, combined)
	}

	// User-supplied entries round-trip with correct contents.
	checkFile(t, filepath.Join(extracted, "etc/hostname"), []byte("hpcc\n"))
	checkFile(t, filepath.Join(extracted, "bin/sh"), []byte("#!/bin/sh\nexit 0\n"))

	// Agent injection lands the right bytes at /.hpcc/agent.
	checkFile(t, filepath.Join(extracted, ".hpcc/agent"), agentMarker)

	// Standard mountpoints exist as directories. The agent will
	// mount over them; the on-disk perms don't matter for runtime
	// behaviour but the entries must be present so mount(2)
	// succeeds against a read-only rootfs.
	for _, mp := range []string{"proc", "sys", "dev", "tmp", "run"} {
		info, err := os.Stat(filepath.Join(extracted, mp))
		if err != nil {
			t.Errorf("mountpoint /%s missing: %v", mp, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("mountpoint /%s is not a directory", mp)
		}
	}

	// /tmp gets the sticky-world-writable mode in the writer.
	// Check the IMAGE's listing rather than the extracted dir
	// because unsquashfs running as a non-root user can't apply
	// sticky bits to user-owned directories on every platform
	// (notably macOS dev hosts).
	listing, err := exec.Command(bin, "-ll", "-no-progress", out).CombinedOutput()
	if err != nil {
		t.Fatalf("unsquashfs -ll: %v\n%s", err, listing)
	}
	if !strings.Contains(string(listing), "drwxrwxrwt") {
		t.Errorf("listing missing sticky-world-writable /tmp entry:\n%s", listing)
	}

	// Hardlink shares the inode of its target — the squashfs
	// hardlink encoding's "single-inode" promise.
	if got := pipelineStatIno(t, filepath.Join(extracted, "bin/sh")); got != pipelineStatIno(t, filepath.Join(extracted, "bin/ash")) {
		t.Errorf("/bin/sh and /bin/ash should share an inode; got distinct values")
	}

	// Symlink target round-trip.
	target, err := os.Readlink(filepath.Join(extracted, "etc/host"))
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if target != "hostname" {
		t.Errorf("/etc/host -> %q, want %q", target, "hostname")
	}
}

// TestBuildSquashfs_externalStripsHpccNamespace confirms that
// user-supplied /.hpcc bytes from a hostile image are NOT visible
// after extraction — the hpcc-injected agent is. Same property as
// the in-process test, but validated through the canonical reader
// rather than via byte-substring scanning on the raw archive.
func TestBuildSquashfs_externalStripsHpccNamespace(t *testing.T) {
	bin, err := exec.LookPath("unsquashfs")
	if err != nil {
		t.Skip("unsquashfs not installed; install squashfs-tools to run")
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	mustTarFile(t, tw, ".hpcc/agent", []byte("USER-EVIL-AGENT"))
	mustTarFile(t, tw, "etc/hostname", []byte("hpcc\n"))
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "rootfs.sqsh")
	realAgent := []byte("REAL-HPCC-AGENT-MARKER\n")
	if err := buildSquashfs(&buf, realAgent, out); err != nil {
		t.Fatalf("buildSquashfs: %v", err)
	}

	extracted := filepath.Join(t.TempDir(), "extract")
	if combined, err := exec.Command(bin, "-d", extracted, "-no-progress", out).CombinedOutput(); err != nil {
		t.Fatalf("unsquashfs: %v\n%s", err, combined)
	}

	got, err := os.ReadFile(filepath.Join(extracted, ".hpcc/agent"))
	if err != nil {
		t.Fatalf("read .hpcc/agent: %v", err)
	}
	if bytes.Equal(got, []byte("USER-EVIL-AGENT")) {
		t.Errorf("hostile user-supplied /.hpcc/agent reached the extracted rootfs")
	}
	if !bytes.Equal(got, realAgent) {
		t.Errorf("/.hpcc/agent = %q, want %q", got, realAgent)
	}
}

func checkFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Errorf("read %s: %v", path, err)
		return
	}
	if !bytes.Equal(got, want) {
		gs, ws := string(got), string(want)
		if len(gs) > 200 {
			gs = gs[:200] + "..."
		}
		if len(ws) > 200 {
			ws = ws[:200] + "..."
		}
		t.Errorf("%s content mismatch: got %q want %q", path, gs, ws)
	}
}

// pipelineStatIno is named distinctively to avoid collision with
// the same-shaped helper in the squashfs package's tests, since Go
// test names share package scope. Returns the underlying inode
// number for hardlink-shared-inode assertions.
func pipelineStatIno(t *testing.T, p string) uint64 {
	t.Helper()
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat %s: %v", p, err)
	}
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("stat %s: platform lacks inode reporting", p)
	}
	return uint64(sys.Ino)
}
