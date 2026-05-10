// Package rootfs is the Linux raw-Firecracker implementation of
// image.Store. It pulls user images, flattens them onto a sparse
// ext4 filesystem image, and injects the in-VM hpcc-agent so the
// host-side vsock client has something to talk to once the microVM
// boots.
//
// On disk each prepared artifact is one regular file under CacheDir
// named "<algo>-<hex>.ext4" (e.g. "sha256-abc123…ef.ext4"). Colons
// in the user digest are not portable in filenames on every host
// filesystem worth caring about, so we encode them as a dash and
// reverse the mapping when listing. The ".ext4" suffix is part of
// the contract — it lets ad-hoc tooling tell prepared rootfs files
// apart from anything else an operator might drop into CacheDir,
// and lets GetExistingImages skip strays without parsing them.
//
// PullImage builds the prepared rootfs in four stages: (1) crane.Pull
// fetches the user image and we sanity-check its digest matches
// expectedDigest. (2) mutate.Extract gives us a flattened tarball
// with whiteouts already applied; we spool that to disk. (3) we
// shell out to `tar -xpf` to materialize the tarball under a
// staging directory — using the system `tar` rather than rolling
// our own gives us full OCI-tar coverage (hardlinks, symlinks,
// xattrs, modes, ownership) for free. The per-arch hpcc-agent
// binary is dropped into <staging>/.hpcc/agent right after.
// (4) we shell out to `mkfs.ext4 -d` to format a fresh ext4 image
// and populate it from the staging dir in one pass.
//
// Why shell out instead of using a pure-Go ext4 writer (go-diskfs)?
// go-diskfs's ext4 implementation is incomplete on the write path —
// it fails extent-tree promotion on multi-MB files (e.g. our agent)
// and journal initialization on rootfs sizes typical of real
// toolchain images (~1 GB and up). e2fsprogs' mkfs.ext4 has been
// the canonical implementation for two decades and is on every
// Linux that could host a worker; the dependency cost is one
// stable, package-managed binary.
package rootfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
)

// rootfsSuffix is appended to every prepared file. See package doc.
const rootfsSuffix = ".ext4"

// agentInstallPath is where the agent is dropped inside the rootfs.
// The kernel boot args point init at this path.
const agentInstallPath = "/.hpcc/agent"

// ext4 sizing constants. The staging-dir byte count gives us a lower
// bound on file content; ext4 itself needs blocks for inodes,
// directory entries, the journal, and group metadata. The 2× content
// multiplier plus minimum floor cover that overhead with margin —
// rootfs sizing isn't a hot path, the file is sparse, and going
// short means mkfs.ext4 either rejects the size or produces an
// image that fills up immediately. Round up to a 4 MiB boundary so
// the device size aligns with ext4's block group geometry.
const (
	ext4Mib4         int64 = 4 * 1024 * 1024
	ext4MinBytes     int64 = 256 * 1024 * 1024
	ext4Overhead     int64 = 64 * 1024 * 1024
	ext4ContentScale       = 2 // multiplier on the staging dir's content size
)

// AgentBinaries lists the host filesystem paths of the per-arch
// hpcc-agent binaries the worker has built (or shipped). The Store
// reads from these when laying down the agent inside the rootfs at
// /.hpcc/agent before the ext4 image is sealed. Windows is absent
// on purpose — that path goes through cdimage instead.
type AgentBinaries struct {
	LinuxAmd64 string
	LinuxArm64 string
}

// Store implements image.Store on top of an on-disk rootfs cache.
// The zero value is not valid: CacheDir must be a writable directory
// on a filesystem that supports sparse files (the prepared rootfs is
// sized for the largest expected install but only the populated
// extents are stored), and Agent must cover every architecture for
// which images will be pulled.
type Store struct {
	// CacheDir is where prepared rootfs files live. One file per
	// user-image digest; the file path is the artifact handed to
	// the Firecracker driver.
	CacheDir string

	// Agent is the per-arch hpcc-agent binaries to inject as PID 1
	// inside the prepared rootfs.
	Agent AgentBinaries
}

// GetExistingImages enumerates the user-image digests of every
// prepared rootfs file in CacheDir. Files whose name doesn't match
// the "<algo>-<hex>.ext4" shape are skipped — operators sometimes
// stage scratch files or the in-progress output of a crashed build
// alongside finished artifacts, and treating those as catalogue
// entries would surface bogus digests in the worker's heartbeat.
//
// A missing CacheDir is not an error: a fresh worker host hasn't
// prepared anything yet, and the empty result is the right answer.
func (s *Store) GetExistingImages(_ context.Context) ([]string, error) {
	entries, err := os.ReadDir(s.CacheDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read cache dir %q: %w", s.CacheDir, err)
	}

	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		digest, ok := decodeRootfsName(e.Name())
		if !ok {
			continue
		}
		out = append(out, digest)
	}
	return out, nil
}

// PullImage pulls imagePath, verifies it matches expectedDigest,
// flattens its layers onto disk, injects /.hpcc/agent, and runs
// mkfs.ext4 -d to seal everything into CacheDir/<algo>-<hex>.ext4.
//
// The build runs against a "<final>.tmp" sibling and atomically
// renames into place on success — partial files left behind by a
// crash never look like a finished artifact to GetExistingImages.
func (s *Store) PullImage(_ context.Context, imagePath, expectedDigest string) error {
	finalName, err := encodeRootfsName(expectedDigest)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.CacheDir, 0o755); err != nil {
		return fmt.Errorf("create cache dir %q: %w", s.CacheDir, err)
	}
	finalPath := filepath.Join(s.CacheDir, finalName)
	tmpPath := finalPath + ".tmp"
	_ = os.Remove(tmpPath)

	img, err := crane.Pull(imagePath)
	if err != nil {
		return fmt.Errorf("pull %q: %w", imagePath, err)
	}
	dgst, err := img.Digest()
	if err != nil {
		return fmt.Errorf("read pulled digest: %w", err)
	}
	if dgst.String() != expectedDigest {
		return fmt.Errorf("image digest mismatch: expected %s, got %s",
			expectedDigest, dgst.String())
	}

	cfg, err := img.ConfigFile()
	if err != nil {
		return fmt.Errorf("read image config: %w", err)
	}
	if cfg.OS != "linux" {
		return fmt.Errorf("rootfs store: image OS %q is not linux", cfg.OS)
	}

	agentBin, err := s.loadAgentBinary(cfg.Architecture)
	if err != nil {
		return err
	}

	// Spool the flattened tar to disk so `tar -xpf` can stream it.
	// We could pipe directly, but writing through a file lets us
	// retain the spool for diagnostics if extraction fails.
	spool, err := os.CreateTemp(s.CacheDir, "rootfs-tar-*.tar")
	if err != nil {
		return fmt.Errorf("create rootfs spool: %w", err)
	}
	spoolPath := spool.Name()
	defer os.Remove(spoolPath)

	flat := mutate.Extract(img)
	_, err = io.Copy(spool, flat)
	_ = flat.Close()
	if cerr := spool.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("spool flattened tar: %w", err)
	}

	// Stage on the same filesystem as CacheDir so cleanup is
	// quick and the optional rename trick (if we ever needed it)
	// stays atomic. Removed on every exit path.
	stagingDir, err := os.MkdirTemp(s.CacheDir, "rootfs-stage-*")
	if err != nil {
		return fmt.Errorf("create staging dir: %w", err)
	}
	defer os.RemoveAll(stagingDir)

	if err := extractTar(spoolPath, stagingDir); err != nil {
		return err
	}
	if err := injectAgent(stagingDir, agentBin); err != nil {
		return err
	}
	if err := ensureMountpoints(stagingDir); err != nil {
		return err
	}
	if err := buildExt4FromDir(stagingDir, tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}

	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("publish rootfs %q: %w", finalPath, err)
	}
	return nil
}

// PathFor returns the absolute on-disk path of the prepared rootfs
// for userDigest, whether or not the file has been built yet. The
// raw Firecracker runtime calls this to find the ext4 image to
// attach as /dev/vda inside the microVM.
func (s *Store) PathFor(userDigest string) (string, error) {
	name, err := encodeRootfsName(userDigest)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.CacheDir, name), nil
}

// UntagImage removes the prepared rootfs for userDigest. A missing
// file is not an error — eviction can race against a crash that
// left no artifact behind, and against catalogue/disk drift in
// either direction. The caller treats this as best-effort cleanup
// so the blob becomes reclaimable; the in-memory catalogue entry
// is the authoritative "do we have this digest" signal.
func (s *Store) UntagImage(_ context.Context, userDigest string) error {
	name, err := encodeRootfsName(userDigest)
	if err != nil {
		return err
	}
	path := filepath.Join(s.CacheDir, name)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove rootfs %q: %w", path, err)
	}
	return nil
}

// loadAgentBinary returns the bytes of the agent for arch.
func (s *Store) loadAgentBinary(arch string) ([]byte, error) {
	var path string
	switch arch {
	case "amd64":
		path = s.Agent.LinuxAmd64
	case "arm64":
		path = s.Agent.LinuxArm64
	default:
		return nil, fmt.Errorf("no hpcc-agent configured for linux/%s", arch)
	}
	if path == "" {
		return nil, fmt.Errorf("hpcc-agent path for linux/%s is not set", arch)
	}
	bin, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read hpcc-agent %q: %w", path, err)
	}
	return bin, nil
}

// extractTar shells out to the system tar to materialize the
// spooled tarball under destDir. -p preserves modes; default tar
// behaviour preserves hardlinks, symlinks, ownership (when running
// as root). We trust the upstream tar over a hand-rolled extractor
// — the OCI tar variants we encounter in the wild (PAX headers,
// long names, sparse files, xattr extensions) are all things tar
// already handles correctly.
func extractTar(tarPath, destDir string) error {
	cmd := exec.Command("tar", "-xpf", tarPath, "-C", destDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tar -xpf %q: %v\n%s", tarPath, err, out)
	}
	return nil
}

// injectAgent writes bin to <stagingDir>/.hpcc/agent with mode 0755.
// The kernel boot args point init at /.hpcc/agent so the file has
// to exist after extraction; if the user image happened to ship
// its own /.hpcc/agent, we overwrite — hpcc owns that path.
func injectAgent(stagingDir string, bin []byte) error {
	agentDir := filepath.Join(stagingDir, ".hpcc")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", agentDir, err)
	}
	target := filepath.Join(agentDir, "agent")
	if err := os.WriteFile(target, bin, 0o755); err != nil {
		return fmt.Errorf("write agent %q: %w", target, err)
	}
	// WriteFile honours umask, so the explicit Chmod ensures the
	// kernel's exec sees 0755 even on hosts with umask 0077.
	if err := os.Chmod(target, 0o755); err != nil {
		return fmt.Errorf("chmod agent %q: %w", target, err)
	}
	return nil
}

// ensureMountpoints pre-creates the standard Linux mountpoints under
// stagingDir so the in-VM agent's setupInit can mount tmpfses on top
// of them without first needing to mkdir on a read-only rootfs. Many
// minimal images (busybox, distroless, scratch derivatives) ship a
// tar that omits these as a runtime concern; without this step the
// agent fails at PID-1 boot with "mount /proc: no such file or
// directory" and the kernel panics.
//
// /tmp gets the conventional sticky-world-writable mode the agent
// would otherwise have to chmod after mount.
func ensureMountpoints(stagingDir string) error {
	for _, p := range []string{"proc", "sys", "dev", "tmp", "run"} {
		full := filepath.Join(stagingDir, p)
		if err := os.MkdirAll(full, 0o755); err != nil {
			return fmt.Errorf("mkdir mountpoint %q: %w", full, err)
		}
	}
	if err := os.Chmod(filepath.Join(stagingDir, "tmp"), 0o1777); err != nil {
		return fmt.Errorf("chmod /tmp sticky: %w", err)
	}
	return nil
}

// buildExt4FromDir creates an ext4 image at outPath sized to fit
// stagingDir's contents (with overhead + minimum floor) and
// populates it via mkfs.ext4 -d in one pass. -F forces formatting
// over the truncated zero-filled file; -q quiets the progress
// chatter so an extraction failure isn't buried in mkfs noise.
func buildExt4FromDir(stagingDir, outPath string) error {
	contentSize, err := dirContentBytes(stagingDir)
	if err != nil {
		return fmt.Errorf("size staging dir: %w", err)
	}
	devSize := ext4SizeFor(contentSize)

	// Truncate creates a sparse file of devSize; mkfs.ext4 uses
	// the file's size as the device geometry.
	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create rootfs image %q: %w", outPath, err)
	}
	if cerr := f.Close(); cerr != nil {
		return fmt.Errorf("close rootfs image %q: %w", outPath, cerr)
	}
	if err := os.Truncate(outPath, devSize); err != nil {
		return fmt.Errorf("truncate rootfs image %q to %d: %w", outPath, devSize, err)
	}

	cmd := exec.Command("mkfs.ext4", "-q", "-F", "-d", stagingDir, outPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mkfs.ext4 -d %q %q: %v\n%s", stagingDir, outPath, err, out)
	}
	return nil
}

// dirContentBytes sums the sizes of every regular file under root.
// Symlinks and special files are skipped — they cost a few hundred
// bytes of inode/dirent space each, well within ext4Overhead.
func dirContentBytes(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

// ext4SizeFor returns the device size to allocate for a populated
// content-bytes total, satisfying ext4's minimum and rounding to
// 4 MiB.
func ext4SizeFor(contentBytes int64) int64 {
	want := contentBytes*ext4ContentScale + ext4Overhead
	if want < ext4MinBytes {
		want = ext4MinBytes
	}
	if rem := want % ext4Mib4; rem != 0 {
		want += ext4Mib4 - rem
	}
	return want
}

// encodeRootfsName turns a user digest ("sha256:abc…", or bare hex
// which we assume is sha256 — same convention as the cdimage path)
// into the on-disk filename. Rejects empty input and digests whose
// hex part is empty; both would silently produce ambiguous
// filenames otherwise.
func encodeRootfsName(userDigest string) (string, error) {
	algo, hex, ok := splitDigest(userDigest)
	if !ok {
		return "", fmt.Errorf("invalid user digest %q", userDigest)
	}
	return algo + "-" + hex + rootfsSuffix, nil
}

// decodeRootfsName is the reverse of encodeRootfsName. Returns
// ok=false for anything that doesn't match the shape, so the caller
// can skip strays.
func decodeRootfsName(name string) (digest string, ok bool) {
	if !strings.HasSuffix(name, rootfsSuffix) {
		return "", false
	}
	stem := strings.TrimSuffix(name, rootfsSuffix)
	dash := strings.IndexByte(stem, '-')
	if dash <= 0 || dash == len(stem)-1 {
		return "", false
	}
	return stem[:dash] + ":" + stem[dash+1:], true
}

func splitDigest(s string) (algo, hex string, ok bool) {
	if s == "" {
		return "", "", false
	}
	if i := strings.IndexByte(s, ':'); i >= 0 {
		if i == 0 || i == len(s)-1 {
			return "", "", false
		}
		return s[:i], s[i+1:], true
	}
	// No algo prefix: assume sha256, mirroring cdimage.normalizeDigest.
	return "sha256", s, true
}
