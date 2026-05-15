// Package rootfs is the Linux raw-Firecracker implementation of
// image.Store. It pulls user images and seals them into squashfs
// rootfs files, injecting the in-VM hpcc-agent so the host-side
// vsock client has something to talk to once the microVM boots.
//
// On disk each prepared artifact is one regular file under CacheDir
// named "<algo>-<hex>.sqsh" (e.g. "sha256-abc123…ef.sqsh"). Colons
// in the user digest are not portable in filenames on every host
// filesystem worth caring about, so we encode them as a dash and
// reverse the mapping when listing. The ".sqsh" suffix is part of
// the contract — it lets ad-hoc tooling tell prepared rootfs files
// apart from anything else an operator might drop into CacheDir,
// and lets GetExistingImages skip strays without parsing them.
//
// PullImage builds the prepared rootfs in three streaming stages:
// (1) crane.Pull fetches the user image and we sanity-check its
// digest matches expectedDigest. (2) mutate.Extract gives us a
// flattened tarball with whiteouts already applied. (3) we stream
// the tarball through the in-tree squashfs writer
// (github.com/aarani/hpcc/squashfs), mapping each tar entry to a
// squashfs Create* call. No staging directory on the host; no
// shell-outs to `tar` or `mkfs.*`; no temp spool for the layer tar.
//
// The agent injection and standard-mountpoint setup happen inline
// against the same squashfs writer — /.hpcc/agent is written from
// the bytes the worker has on hand, and /proc /sys /dev /tmp /run
// are created (if the user's tar didn't already include them) so
// the agent's setupInit can mount tmpfses on top of them without
// first needing to mkdir on a read-only rootfs.
//
// Why squashfs rather than ext4? Read-only by design, naturally
// streaming-writable from a tar without a staging dir, and the
// host has no GPL e2fsprogs / squashfs-tools shell-out in the hot
// path. The kernel mounts the result read-only as /dev/vda inside
// the guest. See docs/plan/phase-4-distributed.md §4.3 and §4.14.
package rootfs

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/aarani/hpcc/squashfs"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
)

// rootfsSuffix is appended to every prepared file. See package doc.
const rootfsSuffix = ".sqsh"

// agentInstallPath is where the agent is dropped inside the rootfs.
// The kernel boot args point init at this path.
const agentInstallPath = "/.hpcc/agent"

// agentInstallDir is the parent directory of agentInstallPath. The
// pipeline owns the whole subtree — any /.hpcc the user's image
// might ship gets stripped during the tar stream.
const agentInstallDir = "/.hpcc"

// standardMountpoints are pre-created (with default perms, if the
// user's tar didn't already include them) so the in-VM agent's
// setupInit can mount kernel filesystems and tmpfses without first
// needing to mkdir on a read-only rootfs.
var standardMountpoints = []string{"/proc", "/sys", "/dev", "/tmp", "/run"}

// AgentBinaries lists the host filesystem paths of the per-arch
// hpcc-agent binaries the worker has built (or shipped). The Store
// reads from these when laying down the agent inside the rootfs at
// /.hpcc/agent before the squashfs image is sealed. Windows is
// absent on purpose — that path goes through cdimage instead.
type AgentBinaries struct {
	LinuxAmd64 string
	LinuxArm64 string
}

// Store implements image.Store on top of an on-disk rootfs cache.
// The zero value is not valid: CacheDir must be a writable
// directory, and Agent must cover every architecture for which
// images will be pulled.
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
// the "<algo>-<hex>.sqsh" shape are skipped — operators sometimes
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
// streams its flattened layer tar through the squashfs writer
// (injecting /.hpcc/agent and standard mountpoints inline), and
// publishes the result as CacheDir/<algo>-<hex>.sqsh.
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

	flat := mutate.Extract(img)
	defer flat.Close()

	if err := buildSquashfs(flat, agentBin, tmpPath); err != nil {
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
// raw Firecracker runtime calls this to find the squashfs image to
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

// buildSquashfs streams tarStream into a fresh squashfs image at
// outPath, injects the agent binary at /.hpcc/agent, and ensures
// the standard guest mountpoints exist. The caller is responsible
// for renaming outPath into place on success — buildSquashfs
// leaves the file behind on failure for diagnostics.
//
// The compressor is gzip (stdlib compress/zlib under the hood; the
// squashfs "gzip" ID expects a zlib stream, not a gzip-with-header
// stream). Gzip support is the most universally compiled-in
// squashfs decompressor across Linux kernel builds, including the
// minimal Firecracker reference kernels — so we pick it for v1 over
// zstd, which is faster but less universal. Switching to zstd later
// is a one-file adapter against the same Compressor interface.
func buildSquashfs(tarStream io.Reader, agentBin []byte, outPath string) error {
	out, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create rootfs image %q: %w", outPath, err)
	}
	closeOut := func(retErr error) error {
		if cerr := out.Close(); cerr != nil && retErr == nil {
			return fmt.Errorf("close rootfs image %q: %w", outPath, cerr)
		}
		return retErr
	}

	w, err := squashfs.NewWriter(out, squashfs.WithCompressor(squashfs.GzipCompressor{}))
	if err != nil {
		return closeOut(fmt.Errorf("init squashfs writer: %w", err))
	}

	created := map[string]bool{}

	if err := streamTarToSquashfs(w, tar.NewReader(tarStream), created); err != nil {
		return closeOut(fmt.Errorf("stream layer tar: %w", err))
	}
	if err := injectAgentBinary(w, agentBin, created); err != nil {
		return closeOut(fmt.Errorf("inject agent: %w", err))
	}
	if err := ensureStandardMountpoints(w, created); err != nil {
		return closeOut(fmt.Errorf("create mountpoints: %w", err))
	}
	if err := w.Close(); err != nil {
		return closeOut(fmt.Errorf("seal squashfs: %w", err))
	}
	return closeOut(nil)
}

// streamTarToSquashfs iterates the OCI layer tar one entry at a
// time and translates each into a squashfs Create* call. The
// translation is intentionally strict: any path that fails
// normalization (".." segments, absolute paths inside the
// archive, empty names, NUL bytes) terminates the build —
// attacker-controlled OCI bytes flow through this loop, so
// permissiveness here is the wrong default. Duplicate entries for
// the same path keep the first (squashfs has no replace
// semantics), matching what `tar -xpf` does to silent overwrites
// when the second copy can't change anything observable.
//
// Entries under /.hpcc are dropped — that namespace belongs to the
// pipeline, and injectAgentBinary writes it fresh after the stream
// completes.
func streamTarToSquashfs(w *squashfs.Writer, tr *tar.Reader, created map[string]bool) error {
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar header: %w", err)
		}

		p, ok := normalizeTarPath(h.Name)
		if !ok {
			return fmt.Errorf("rejected tar entry name %q", h.Name)
		}
		if p == "" {
			// Tar archive root ("./" or "/") — squashfs has an
			// implicit root, nothing to do.
			continue
		}
		if p == agentInstallDir || strings.HasPrefix(p, agentInstallDir+"/") {
			// Pipeline-owned namespace. Drop whatever the user
			// shipped; injectAgentBinary writes ours.
			continue
		}
		if created[p] {
			continue
		}
		if err := ensureAncestors(w, p, h.ModTime.Unix(), created); err != nil {
			return err
		}

		attrs := squashfs.Attrs{
			Path:  p,
			Mode:  fs.FileMode(h.Mode) & fs.ModePerm,
			UID:   uint32(h.Uid),
			GID:   uint32(h.Gid),
			Mtime: h.ModTime,
		}

		switch h.Typeflag {
		case tar.TypeReg, tar.TypeRegA:
			fw, err := w.CreateFile(attrs)
			if err != nil {
				return fmt.Errorf("create file %q: %w", p, err)
			}
			if _, err := io.Copy(fw, tr); err != nil {
				return fmt.Errorf("write file %q: %w", p, err)
			}
		case tar.TypeDir:
			if err := w.CreateDir(attrs); err != nil {
				return fmt.Errorf("create dir %q: %w", p, err)
			}
		case tar.TypeSymlink:
			if h.Linkname == "" {
				return fmt.Errorf("symlink %q has empty target", p)
			}
			if err := w.CreateSymlink(attrs, h.Linkname); err != nil {
				return fmt.Errorf("create symlink %q: %w", p, err)
			}
		case tar.TypeLink:
			target, ok := normalizeTarPath(h.Linkname)
			if !ok || target == "" {
				return fmt.Errorf("hardlink %q has invalid target %q", p, h.Linkname)
			}
			if err := w.CreateHardlink(p, target); err != nil {
				return fmt.Errorf("create hardlink %q -> %q: %w", p, target, err)
			}
		case tar.TypeChar:
			if err := w.CreateCharDevice(attrs, uint32(h.Devmajor), uint32(h.Devminor)); err != nil {
				return fmt.Errorf("create char device %q: %w", p, err)
			}
		case tar.TypeBlock:
			if err := w.CreateBlockDevice(attrs, uint32(h.Devmajor), uint32(h.Devminor)); err != nil {
				return fmt.Errorf("create block device %q: %w", p, err)
			}
		case tar.TypeFifo:
			if err := w.CreateFIFO(attrs); err != nil {
				return fmt.Errorf("create fifo %q: %w", p, err)
			}
		case tar.TypeXGlobalHeader, tar.TypeXHeader:
			// PAX metadata — archive/tar applies it to subsequent
			// entries before we see them. Nothing for us to do.
			continue
		default:
			// Unknown tar type. Skipping rather than erroring
			// because OCI tars occasionally carry vendor-specific
			// markers we don't need to interpret.
			continue
		}
		created[p] = true
	}
}

// injectAgentBinary writes the per-arch hpcc-agent binary to
// /.hpcc/agent inside the squashfs image, creating /.hpcc itself
// if streamTarToSquashfs hasn't already done so (it always
// strips /.hpcc, so /.hpcc never exists at this point — but the
// idempotency keeps the function safe to reorder).
//
// Mode 0755 so the kernel's exec sees an executable; uid/gid 0 so
// the agent runs as root inside the guest, which it has to —
// PID 1 mounts /proc, /sys, /dev/shm.
func injectAgentBinary(w *squashfs.Writer, agentBin []byte, created map[string]bool) error {
	if !created[agentInstallDir] {
		if err := w.CreateDir(squashfs.Attrs{
			Path: agentInstallDir,
			Mode: 0o755,
		}); err != nil {
			return fmt.Errorf("create %s: %w", agentInstallDir, err)
		}
		created[agentInstallDir] = true
	}
	fw, err := w.CreateFile(squashfs.Attrs{
		Path: agentInstallPath,
		Mode: 0o755,
	})
	if err != nil {
		return fmt.Errorf("create %s: %w", agentInstallPath, err)
	}
	if _, err := io.Copy(fw, bytes.NewReader(agentBin)); err != nil {
		return fmt.Errorf("write %s: %w", agentInstallPath, err)
	}
	created[agentInstallPath] = true
	return nil
}

// ensureStandardMountpoints creates the kernel-filesystem and
// tmpfs mountpoint directories the in-VM agent expects. If the
// user's image already shipped one, we keep their entry (mode and
// ownership) — the agent shadows it with a mount immediately
// anyway, so the on-disk attributes don't matter for runtime
// behaviour, only for image cleanliness.
func ensureStandardMountpoints(w *squashfs.Writer, created map[string]bool) error {
	for _, p := range standardMountpoints {
		if created[p] {
			continue
		}
		mode := fs.FileMode(0o755)
		if p == "/tmp" {
			mode = 0o1777
		}
		if err := w.CreateDir(squashfs.Attrs{Path: p, Mode: mode}); err != nil {
			return fmt.Errorf("create %s: %w", p, err)
		}
		created[p] = true
	}
	return nil
}

// ensureAncestors walks p's ancestor directories from shallowest
// to deepest, calling CreateDir for any that haven't been created
// yet. OCI tars sometimes elide explicit dir entries for ancestors
// (especially "./" segments and shallow paths) on the assumption
// that the extractor will mkdir -p; the squashfs writer needs the
// directory tree explicit by Close. Default ancestor perms are
// 0755 owned by root — matching what `tar -xpf` would do for an
// implicit ancestor.
func ensureAncestors(w *squashfs.Writer, p string, mtimeUnix int64, created map[string]bool) error {
	parent := path.Dir(p)
	if parent == "/" || parent == "." {
		return nil
	}
	// Build ancestors from shallowest to deepest so children of
	// ancestors don't get attempted before the ancestors exist.
	var ancestors []string
	for cur := parent; cur != "/" && cur != "."; cur = path.Dir(cur) {
		ancestors = append([]string{cur}, ancestors...)
	}
	for _, a := range ancestors {
		if created[a] {
			continue
		}
		if err := w.CreateDir(squashfs.Attrs{
			Path: a,
			Mode: 0o755,
		}); err != nil {
			return fmt.Errorf("create implicit ancestor %q for %q: %w", a, p, err)
		}
		created[a] = true
	}
	return nil
}

// normalizeTarPath maps a tar Header.Name into the canonical
// absolute form the squashfs writer expects ("/foo/bar"), or
// reports ok=false for anything we refuse to admit into the
// archive: empty names, NUL bytes, paths containing ".." or "."
// components, absolute paths (interpreted as tar-escape attempts).
//
// The "./" prefix and trailing "/" common to dir entries are
// stripped. The bare archive root ("." or "/") returns ("", true)
// so callers can skip it cleanly without a name error.
func normalizeTarPath(name string) (string, bool) {
	if name == "" || strings.ContainsRune(name, 0) {
		return "", false
	}
	if name == "." || name == "/" || name == "./" {
		return "", true
	}
	name = strings.TrimPrefix(name, "./")
	if strings.HasPrefix(name, "/") {
		// Reject absolute paths inside the archive — a well-formed
		// OCI layer tar never carries them, and `tar -xpf`
		// historically treats them as a path-traversal attempt.
		return "", false
	}
	name = strings.TrimSuffix(name, "/")
	if name == "" {
		return "", false
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", false
		}
	}
	return "/" + name, true
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
