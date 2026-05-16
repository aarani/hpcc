// Package cdimage is the containerd-backed implementation of
// image.Store. It pulls user images via containerd, injects the hpcc
// pause binary as a fresh top layer, and registers the prepared variant
// under a synthetic "prepared.hpcc.local/img:<digest>" name.
//
// This is the path used on Windows under hcsshim's Hyper-V isolation;
// the Linux raw-Firecracker driver uses image/rootfs instead.
package cdimage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// userDigestLabel marks containerd image records that hpcc has prepared
// (pause-binary injected) so the worker can tell them apart from raw
// user-pulled images. The value is the digest of the user-supplied image
// that this prepared variant is derived from.
const userDigestLabel = "hpcc.dev/user-digest"

// PauseBinaries lists the host filesystem paths of the per-platform
// pause binaries the worker has built (or shipped). The Store reads
// from these when constructing the injected layer for a prepared image.
type PauseBinaries struct {
	LinuxAmd64   string
	LinuxArm64   string
	WindowsAmd64 string
}

// Store implements image.Store on top of containerd. The zero value is
// not valid — callers must set Client; Pause must cover every platform
// for which images are pulled.
type Store struct {
	Pause  PauseBinaries
	Client *containerd.Client
}

func (s *Store) GetExistingImages(ctx context.Context) ([]string, error) {
	imgs, err := s.Client.ListImages(ctx)
	if err != nil {
		return nil, fmt.Errorf("list images: %w", err)
	}

	digests := make([]string, 0, len(imgs))
	for _, img := range imgs {
		if d := img.Labels()[userDigestLabel]; d != "" {
			digests = append(digests, d)
		}
	}
	return digests, nil
}

func (s *Store) UntagImage(ctx context.Context, userDigest string) error {
	name := preparedImageName(normalizeDigest(userDigest))
	err := s.Client.ImageService().Delete(ctx, name, images.SynchronousDelete())
	if err != nil && errdefs.IsNotFound(err) {
		return nil
	}
	return err
}

func (s *Store) PullImage(ctx context.Context, imagePath string, expectedDigest string) error {
	pull, err := s.Client.Pull(ctx, imagePath, containerd.WithPullUnpack)
	if err != nil {
		return fmt.Errorf("pull %q: %w", imagePath, err)
	}

	want := normalizeDigest(expectedDigest)
	if pull.Target().Digest != want {
		return fmt.Errorf("image digest mismatch: expected %s, got %s",
			want, pull.Target().Digest)
	}

	return s.injectPause(ctx, pull, want)
}

// injectPause reads the base manifest+config from the content store,
// builds a tar.gz layer containing /.hpcc/pause, writes the layer + a
// modified config + a new manifest as fresh blobs, and registers a new
// image record pointing at the new manifest.
func (s *Store) injectPause(ctx context.Context, base containerd.Image, userDigest digest.Digest) error {
	// A lease keeps the blobs we're about to write reachable across the
	// window before the image record (and the manifest's gc.ref labels)
	// take over as their GC roots. Without it, containerd's background
	// GC can collect the layer/config as orphans before we finish.
	ctx, done, err := s.Client.WithLease(ctx)
	if err != nil {
		return fmt.Errorf("acquire lease: %w", err)
	}
	defer func() { _ = done(ctx) }()

	cs := s.Client.ContentStore()

	// 1. Read the base manifest. The pulled descriptor must point at a
	// single-platform manifest — multi-arch indexes need a platform
	// selection step the caller hasn't made.
	manifestData, err := content.ReadBlob(ctx, cs, base.Target())
	if err != nil {
		return fmt.Errorf("read manifest %s: %w", base.Target().Digest, err)
	}
	if !isManifestMediaType(base.Target().MediaType) {
		return fmt.Errorf("expected image manifest, got %q (resolve to a platform-specific digest)",
			base.Target().MediaType)
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return fmt.Errorf("decode manifest: %w", err)
	}

	// 2. Read the base image config so we can copy/extend it.
	configData, err := content.ReadBlob(ctx, cs, manifest.Config)
	if err != nil {
		return fmt.Errorf("read image config: %w", err)
	}
	var imgCfg ocispec.Image
	if err := json.Unmarshal(configData, &imgCfg); err != nil {
		return fmt.Errorf("decode image config: %w", err)
	}

	// 3. Build the pause layer for the image's platform. Two digests:
	// the diff_id (uncompressed tar) goes into rootfs.diff_ids; the
	// layer descriptor digest (gzipped tar) is what the manifest points
	// at.
	pauseBin, pausePath, err := s.loadPauseBinary(imgCfg.OS, imgCfg.Architecture)
	if err != nil {
		return err
	}
	uncompressed, compressed, err := buildPauseLayer(pausePath, pauseBin)
	if err != nil {
		return fmt.Errorf("build pause layer: %w", err)
	}
	diffID := digest.FromBytes(uncompressed)
	layerDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageLayerGzip,
		Digest:    digest.FromBytes(compressed),
		Size:      int64(len(compressed)),
	}

	// 4. Write the layer blob.
	if err := content.WriteBlob(
		ctx, cs,
		"hpcc-pause-layer-"+layerDesc.Digest.Encoded(),
		bytes.NewReader(compressed),
		layerDesc,
	); err != nil {
		return fmt.Errorf("write pause layer blob: %w", err)
	}

	// 5. Modify the image config: append diff_id, override entrypoint,
	// clear cmd. The injected layer is recorded in history so `docker
	// history` and audit tooling can see what hpcc added.
	imgCfg.RootFS.DiffIDs = append(imgCfg.RootFS.DiffIDs, diffID)
	imgCfg.Config.Entrypoint = []string{pausePath}
	imgCfg.Config.Cmd = nil
	imgCfg.History = append(imgCfg.History, ocispec.History{
		CreatedBy: "hpcc: inject pause binary",
	})

	newConfigData, err := json.Marshal(imgCfg)
	if err != nil {
		return fmt.Errorf("encode new image config: %w", err)
	}
	newConfigDesc := ocispec.Descriptor{
		MediaType: manifest.Config.MediaType,
		Digest:    digest.FromBytes(newConfigData),
		Size:      int64(len(newConfigData)),
	}
	if err := content.WriteBlob(
		ctx, cs,
		"hpcc-prepared-config-"+newConfigDesc.Digest.Encoded(),
		bytes.NewReader(newConfigData),
		newConfigDesc,
	); err != nil {
		return fmt.Errorf("write image config blob: %w", err)
	}

	// 6. Build and write the new manifest. Append the pause layer to
	// the existing layer list; reuse the original media type so an OCI
	// index of mixed-flavor manifests stays valid.
	newManifest := ocispec.Manifest{
		Versioned:   manifest.Versioned,
		MediaType:   manifest.MediaType,
		Config:      newConfigDesc,
		Layers:      append(append([]ocispec.Descriptor{}, manifest.Layers...), layerDesc),
		Annotations: manifest.Annotations,
	}
	newManifestData, err := json.Marshal(newManifest)
	if err != nil {
		return fmt.Errorf("encode new manifest: %w", err)
	}
	newManifestDesc := ocispec.Descriptor{
		MediaType: manifest.MediaType,
		Digest:    digest.FromBytes(newManifestData),
		Size:      int64(len(newManifestData)),
	}
	if newManifestDesc.MediaType == "" {
		newManifestDesc.MediaType = ocispec.MediaTypeImageManifest
	}
	// gc.ref labels make the manifest the GC root for its config and
	// layers. The image record references the manifest; these labels
	// extend that reference one hop further, so the config and every
	// layer (including the pause layer we just wrote) stay alive after
	// our lease is released.
	manifestLabels := map[string]string{
		"containerd.io/gc.ref.content.config": newConfigDesc.Digest.String(),
	}
	for i, l := range newManifest.Layers {
		manifestLabels[fmt.Sprintf("containerd.io/gc.ref.content.l.%d", i)] = l.Digest.String()
	}
	if err := content.WriteBlob(
		ctx, cs,
		"hpcc-prepared-manifest-"+newManifestDesc.Digest.Encoded(),
		bytes.NewReader(newManifestData),
		newManifestDesc,
		content.WithLabels(manifestLabels),
	); err != nil {
		return fmt.Errorf("write manifest blob: %w", err)
	}

	// 7. Register the image record. If a prepared image for this user
	// digest already exists, replace it (the layer/manifest digests may
	// have changed if the pause binary was rebuilt).
	is := s.Client.ImageService()
	rec := images.Image{
		Name:   preparedImageName(userDigest),
		Target: newManifestDesc,
		Labels: map[string]string{userDigestLabel: userDigest.String()},
	}
	if _, err := is.Create(ctx, rec); err != nil {
		// Best-effort overwrite if it already exists.
		if _, uerr := is.Update(ctx, rec); uerr != nil {
			return fmt.Errorf("register prepared image (create=%v, update=%v)", err, uerr)
		}
	}
	return nil
}

// loadPauseBinary reads the platform-specific pause binary from the
// configured path and returns its bytes plus the in-image install path
// (Linux: /.hpcc/pause; Windows: Files\.hpcc\pause.exe — Windows OCI
// layers prepend "Files/" to all in-container paths).
func (s *Store) loadPauseBinary(goos, goarch string) (data []byte, installPath string, err error) {
	var path string
	switch {
	case goos == "linux" && goarch == "amd64":
		path = s.Pause.LinuxAmd64
		installPath = "/.hpcc/pause"
	case goos == "linux" && goarch == "arm64":
		path = s.Pause.LinuxArm64
		installPath = "/.hpcc/pause"
	case goos == "windows" && goarch == "amd64":
		path = s.Pause.WindowsAmd64
		installPath = `C:\.hpcc\pause.exe`
	default:
		return nil, "", fmt.Errorf("no pause binary configured for %s/%s", goos, goarch)
	}
	if path == "" {
		return nil, "", fmt.Errorf("pause binary path for %s/%s is not set", goos, goarch)
	}
	bin, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read pause binary %q: %w", path, err)
	}
	return bin, installPath, nil
}

// buildPauseLayer produces both the uncompressed tar (for diff_id) and
// the gzipped tar (for blob storage). The layer contains a single
// regular file at installPath plus its parent directory entries, all
// owned by uid/gid 0 with conservative modes.
func buildPauseLayer(installPath string, bin []byte) (uncompressed, compressed []byte, err error) {
	// In OCI image layers all paths use forward slashes and have no
	// leading slash. Windows layers prepend "Files/"; we normalize the
	// caller-supplied path here.
	tarPath := normalizeLayerPath(installPath)
	dirs := parentDirs(tarPath)

	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, d := range dirs {
		if err := tw.WriteHeader(&tar.Header{
			Name:     d + "/",
			Mode:     0o755,
			Typeflag: tar.TypeDir,
		}); err != nil {
			return nil, nil, fmt.Errorf("tar dir %q: %w", d, err)
		}
	}
	if err := tw.WriteHeader(&tar.Header{
		Name:     tarPath,
		Mode:     0o755,
		Size:     int64(len(bin)),
		Typeflag: tar.TypeReg,
	}); err != nil {
		return nil, nil, fmt.Errorf("tar header: %w", err)
	}
	if _, err := tw.Write(bin); err != nil {
		return nil, nil, fmt.Errorf("tar body: %w", err)
	}
	if err := tw.Close(); err != nil {
		return nil, nil, fmt.Errorf("tar close: %w", err)
	}
	uncompressed = raw.Bytes()

	var gz bytes.Buffer
	gzw := gzip.NewWriter(&gz)
	if _, err := gzw.Write(uncompressed); err != nil {
		return nil, nil, fmt.Errorf("gzip write: %w", err)
	}
	if err := gzw.Close(); err != nil {
		return nil, nil, fmt.Errorf("gzip close: %w", err)
	}
	return uncompressed, gz.Bytes(), nil
}

func normalizeLayerPath(p string) string {
	// Detect Windows shape from drive letter or backslashes BEFORE we
	// strip them — those are the signals that this path needs the
	// OCI-on-Windows "Files/" prefix.
	isWindows := strings.Contains(p, `\`) || (len(p) >= 2 && p[1] == ':')

	if len(p) >= 2 && p[1] == ':' {
		p = p[2:]
	}
	p = strings.ReplaceAll(p, `\`, "/")
	p = strings.TrimLeft(p, "/")

	if isWindows && !strings.HasPrefix(p, "Files/") {
		p = "Files/" + p
	}
	return p
}

func parentDirs(p string) []string {
	parts := strings.Split(p, "/")
	if len(parts) <= 1 {
		return nil
	}
	out := make([]string, 0, len(parts)-1)
	for i := 1; i < len(parts); i++ {
		out = append(out, strings.Join(parts[:i], "/"))
	}
	return out
}

func isManifestMediaType(mt string) bool {
	switch mt {
	case ocispec.MediaTypeImageManifest,
		"application/vnd.docker.distribution.manifest.v2+json":
		return true
	}
	return false
}

func normalizeDigest(s string) digest.Digest {
	if !strings.Contains(s, ":") {
		return digest.Digest("sha256:" + s)
	}
	return digest.Digest(s)
}

// PreparedImageName returns the containerd image-record name under
// which a prepared image (pause-binary injected) is registered for a
// given user-supplied image digest. Exported so the Windows runtime
// can resolve a prepared image by user digest without re-implementing
// the naming convention.
func PreparedImageName(userDigest string) string {
	return preparedImageName(normalizeDigest(userDigest))
}

func preparedImageName(userDigest digest.Digest) string {
	// containerd image references must look like a registry path. A
	// fake "prepared.hpcc.local" host keeps things parseable without
	// implying a real registry.
	return "prepared.hpcc.local/img:" + userDigest.Encoded()
}
