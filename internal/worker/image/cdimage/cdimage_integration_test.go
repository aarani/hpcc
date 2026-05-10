//go:build integration

package cdimage

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Integration tests for Store. Each test connects to a real containerd
// daemon if one is reachable (via $CONTAINERD_ADDRESS or the default
// /run/containerd/containerd.sock), otherwise it skips. Tests run inside
// a unique containerd namespace so they cannot collide with each other
// or with anything else running on the host; the namespace's image
// records are deleted in t.Cleanup.

const testImageRef = "registry.k8s.io/pause:3.10"

// Shared containerd client across all integration tests in this binary.
// containerd.New is lazy and IsServing's dial waits ~5s on a missing
// daemon, so probing per-test would mean every CI run on a host without
// containerd burns 5s × N tests just to skip. We probe once and cache.
var (
	sharedClient     *containerd.Client
	sharedClientErr  error
	sharedClientOnce sync.Once
)

func sharedContainerd(t *testing.T) *containerd.Client {
	t.Helper()
	sharedClientOnce.Do(func() {
		addr := os.Getenv("CONTAINERD_ADDRESS")
		if addr == "" {
			addr = "/run/containerd/containerd.sock"
		}
		cli, err := containerd.New(addr)
		if err != nil {
			sharedClientErr = fmt.Errorf("dial %s: %w", addr, err)
			return
		}
		probeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ok, err := cli.IsServing(probeCtx)
		if err != nil || !ok {
			_ = cli.Close()
			sharedClientErr = fmt.Errorf("daemon at %s not usable (serving=%v err=%v)",
				addr, ok, err)
			return
		}
		sharedClient = cli
		// Don't t.Cleanup the close — this client outlives the test
		// that first set it up. The OS reclaims it at process exit.
	})
	if sharedClientErr != nil {
		t.Skipf("containerd not usable: %v", sharedClientErr)
	}
	return sharedClient
}

// connect returns a containerd client and a context bound to a fresh
// per-test namespace. Skips if containerd isn't reachable OR the daemon
// is reachable but unusable (typically: socket exists with root-only
// permissions and the test process is unprivileged — exactly what
// happens on the default GitHub-hosted Ubuntu runner where Docker
// installs containerd but doesn't expose it to the runner user).
func connect(t *testing.T) (*containerd.Client, context.Context) {
	t.Helper()
	cli := sharedContainerd(t)

	ns := fmt.Sprintf("hpcc-image-test-%d", time.Now().UnixNano())
	ctx := namespaces.WithNamespace(context.Background(), ns)

	t.Cleanup(func() {
		cleanCtx, cancel := context.WithTimeout(
			namespaces.WithNamespace(context.Background(), ns),
			30*time.Second,
		)
		defer cancel()
		is := cli.ImageService()
		list, err := is.List(cleanCtx)
		if err != nil {
			return
		}
		for _, img := range list {
			_ = is.Delete(cleanCtx, img.Name, images.SynchronousDelete())
		}
	})

	return cli, ctx
}

// resolveHostPlatformDigest pulls ref far enough to know its index, then
// picks the manifest matching the host platform and returns that
// platform-specific manifest digest. Lets the test use Store.PullImage
// (which expects a single-platform digest) on multi-arch refs.
func resolveHostPlatformDigest(t *testing.T, ctx context.Context, cli *containerd.Client, ref string) digest.Digest {
	t.Helper()

	img, err := cli.Pull(ctx, ref)
	if err != nil {
		t.Fatalf("pull %q for digest resolution: %v", ref, err)
	}

	target := img.Target()
	if isManifestMediaType(target.MediaType) {
		return target.Digest
	}

	// Multi-arch index — find the host platform.
	data, err := content.ReadBlob(ctx, cli.ContentStore(), target)
	if err != nil {
		t.Fatalf("read index blob: %v", err)
	}
	var idx ocispec.Index
	if err := json.Unmarshal(data, &idx); err != nil {
		t.Fatalf("decode index: %v", err)
	}
	for _, m := range idx.Manifests {
		if m.Platform == nil {
			continue
		}
		if m.Platform.OS == runtime.GOOS && m.Platform.Architecture == runtime.GOARCH {
			return m.Digest
		}
	}
	t.Fatalf("no manifest for %s/%s in index for %q", runtime.GOOS, runtime.GOARCH, ref)
	return ""
}

// pauseBinaryConfig returns a PauseBinaries struct whose configured
// path for the host platform points at a temp-file fake binary.
func pauseBinaryConfig(t *testing.T) (PauseBinaries, []byte) {
	t.Helper()
	body := []byte("fake-pause-binary-for-tests")
	dir := t.TempDir()
	path := filepath.Join(dir, "pause")
	if err := os.WriteFile(path, body, 0o755); err != nil {
		t.Fatalf("write fake pause binary: %v", err)
	}

	pb := PauseBinaries{}
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "linux/amd64":
		pb.LinuxAmd64 = path
	case "linux/arm64":
		pb.LinuxArm64 = path
	case "windows/amd64":
		pb.WindowsAmd64 = path
	default:
		t.Skipf("no pause-binary slot for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	return pb, body
}

// --- GetExistingImages ---------------------------------------------------

func TestIntegration_GetExistingImages_filtersByLabel(t *testing.T) {
	cli, ctx := connect(t)
	is := cli.ImageService()

	fakeDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    digest.FromString("fake-target"),
		Size:      11,
	}

	if _, err := is.Create(ctx, images.Image{
		Name:   "test/no-label:1",
		Target: fakeDesc,
	}); err != nil {
		t.Fatalf("create no-label record: %v", err)
	}
	if _, err := is.Create(ctx, images.Image{
		Name:   "test/with-label-a:1",
		Target: fakeDesc,
		Labels: map[string]string{userDigestLabel: "sha256:aaaa"},
	}); err != nil {
		t.Fatalf("create labeled record A: %v", err)
	}
	if _, err := is.Create(ctx, images.Image{
		Name:   "test/with-label-b:1",
		Target: fakeDesc,
		Labels: map[string]string{userDigestLabel: "sha256:bbbb"},
	}); err != nil {
		t.Fatalf("create labeled record B: %v", err)
	}

	store := &Store{Client: cli}
	got, err := store.GetExistingImages(ctx)
	if err != nil {
		t.Fatalf("GetExistingImages: %v", err)
	}

	want := map[string]bool{"sha256:aaaa": true, "sha256:bbbb": true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for _, d := range got {
		if !want[d] {
			t.Errorf("unexpected digest %q in result", d)
		}
	}
}

func TestIntegration_GetExistingImages_emptyStore(t *testing.T) {
	cli, ctx := connect(t)
	store := &Store{Client: cli}

	got, err := store.GetExistingImages(ctx)
	if err != nil {
		t.Fatalf("GetExistingImages: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty result on fresh namespace, got %v", got)
	}
}

// --- PullImage -----------------------------------------------------------

func TestIntegration_PullImage_endToEnd(t *testing.T) {
	cli, ctx := connect(t)

	// Resolve to a single-platform manifest digest; Store.PullImage
	// errors on indexes by design.
	platformDigest := resolveHostPlatformDigest(t, ctx, cli, testImageRef)
	pinnedRef := fmt.Sprintf("registry.k8s.io/pause@%s", platformDigest)

	pause, pauseBody := pauseBinaryConfig(t)
	store := &Store{
		Client: cli,
		Pause:  pause,
	}

	if err := store.PullImage(ctx, pinnedRef, platformDigest.String()); err != nil {
		t.Fatalf("PullImage: %v", err)
	}

	// Prepared image record should now exist under the synthetic name
	// with the user-digest label set to the pulled digest.
	is := cli.ImageService()
	preparedName := preparedImageName(platformDigest)
	rec, err := is.Get(ctx, preparedName)
	if err != nil {
		t.Fatalf("get prepared image %q: %v", preparedName, err)
	}
	if rec.Labels[userDigestLabel] != platformDigest.String() {
		t.Errorf("label %s = %q, want %q",
			userDigestLabel, rec.Labels[userDigestLabel], platformDigest)
	}

	// Walk the prepared manifest: it must have one more layer than the
	// base, and the last layer's uncompressed bytes must be a tar
	// containing the pause binary we configured.
	cs := cli.ContentStore()
	preparedManifestData, err := content.ReadBlob(ctx, cs, rec.Target)
	if err != nil {
		t.Fatalf("read prepared manifest: %v", err)
	}
	var prepared ocispec.Manifest
	if err := json.Unmarshal(preparedManifestData, &prepared); err != nil {
		t.Fatalf("decode prepared manifest: %v", err)
	}

	baseManifestDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    platformDigest,
	}
	baseManifestData, err := content.ReadBlob(ctx, cs, baseManifestDesc)
	if err != nil {
		t.Fatalf("read base manifest: %v", err)
	}
	var base ocispec.Manifest
	if err := json.Unmarshal(baseManifestData, &base); err != nil {
		t.Fatalf("decode base manifest: %v", err)
	}

	if len(prepared.Layers) != len(base.Layers)+1 {
		t.Fatalf("prepared layers = %d, want base+1 (%d)", len(prepared.Layers), len(base.Layers)+1)
	}
	pauseLayerDesc := prepared.Layers[len(prepared.Layers)-1]
	pauseLayerData, err := content.ReadBlob(ctx, cs, pauseLayerDesc)
	if err != nil {
		t.Fatalf("read pause layer blob: %v", err)
	}

	uncompressed := mustGunzip(t, pauseLayerData)
	entries := readTar(t, uncompressed)
	pauseEntry, ok := entries[".hpcc/pause"]
	if !ok {
		t.Fatalf("pause layer missing /.hpcc/pause entry; entries: %v", sortedKeys(entries))
	}
	if string(pauseEntry.body) != string(pauseBody) {
		t.Errorf("pause file body = %q, want %q", pauseEntry.body, pauseBody)
	}

	// Config must now have entrypoint=/.hpcc/pause and an extra diff_id.
	cfgData, err := content.ReadBlob(ctx, cs, prepared.Config)
	if err != nil {
		t.Fatalf("read prepared config: %v", err)
	}
	var imgCfg2 ocispec.Image
	if err := json.Unmarshal(cfgData, &imgCfg2); err != nil {
		t.Fatalf("decode prepared config: %v", err)
	}
	wantEntrypoint := []string{"/.hpcc/pause"}
	if runtime.GOOS == "windows" {
		wantEntrypoint = []string{`C:\.hpcc\pause.exe`}
	}
	if !equalSlices(imgCfg2.Config.Entrypoint, wantEntrypoint) {
		t.Errorf("entrypoint = %v, want %v", imgCfg2.Config.Entrypoint, wantEntrypoint)
	}
	if len(imgCfg2.Config.Cmd) != 0 {
		t.Errorf("cmd should be cleared, got %v", imgCfg2.Config.Cmd)
	}
}

func TestIntegration_PullImage_idempotent(t *testing.T) {
	cli, ctx := connect(t)

	platformDigest := resolveHostPlatformDigest(t, ctx, cli, testImageRef)
	pinnedRef := fmt.Sprintf("registry.k8s.io/pause@%s", platformDigest)

	pause, _ := pauseBinaryConfig(t)
	store := &Store{Client: cli, Pause: pause}

	for i := 0; i < 2; i++ {
		if err := store.PullImage(ctx, pinnedRef, platformDigest.String()); err != nil {
			t.Fatalf("PullImage iteration %d: %v", i, err)
		}
	}

	got, err := store.GetExistingImages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, d := range got {
		if d == platformDigest.String() {
			count++
		}
	}
	if count != 1 {
		t.Errorf("prepared image present %d times, want 1 (got=%v)", count, got)
	}
}

func TestIntegration_PullImage_digestMismatch(t *testing.T) {
	cli, ctx := connect(t)

	// Pin to one digest in the ref but pass a different expectedDigest.
	platformDigest := resolveHostPlatformDigest(t, ctx, cli, testImageRef)
	pinnedRef := fmt.Sprintf("registry.k8s.io/pause@%s", platformDigest)

	store := &Store{Client: cli}

	wrong := "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	err := store.PullImage(ctx, pinnedRef, wrong)
	if err == nil {
		t.Fatal("expected digest mismatch error")
	}
	if !strings.Contains(err.Error(), "digest mismatch") {
		t.Errorf("error = %v, want 'digest mismatch'", err)
	}
}

func TestIntegration_PullImage_rejectsMultiArchIndex(t *testing.T) {
	cli, ctx := connect(t)

	// Pull the tagged ref (no digest pin) so Target is the index.
	img, err := cli.Pull(ctx, testImageRef)
	if err != nil {
		t.Fatalf("warm up pull: %v", err)
	}
	if isManifestMediaType(img.Target().MediaType) {
		t.Skipf("test image %q resolved to a single-platform manifest; can't exercise index rejection",
			testImageRef)
	}

	store := &Store{Client: cli}
	err = store.PullImage(ctx, testImageRef, img.Target().Digest.String())
	if err == nil {
		t.Fatal("expected error rejecting multi-arch index")
	}
	if !strings.Contains(err.Error(), "expected image manifest") {
		t.Errorf("error = %v, want 'expected image manifest'", err)
	}
}

// --- helpers -------------------------------------------------------------

func mustGunzip(t *testing.T, data []byte) []byte {
	t.Helper()
	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer r.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("gzip read: %v", err)
	}
	return out
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
