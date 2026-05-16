//go:build integration && windows

package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

	"github.com/aarani/hpcc/internal/worker/image/cdimage"
)

// Windows-only end-to-end test for the hcsshim runtime. Builds and
// boots a real Windows process-isolated container under a real
// containerd daemon, dispatches one Exec, and verifies the output
// reached the host. Behind both `integration` and `windows` build
// tags so the cross-platform `go test ./...` job ignores it and the
// Linux suite never tries to compile it.
//
// The CI workflow's windows-runtime job is what runs this:
//   1. Install containerd + runhcs shim and start the service.
//   2. Build pause.exe from /pause and point HPCC_HCSSHIM_PAUSE at it.
//   3. go test -tags=integration -run TestHcsshim_EndToEnd_Integration ./internal/worker/runtime/...
//
// Local repro on a Windows dev box: same shape, plus
// HPCC_HCSSHIM_ADDRESS if the daemon is on a non-default pipe.

const (
	// Image used for the end-to-end test. The ltsc2022 tag tracks the
	// Windows Server 2022 baseline; process isolation needs a host
	// kernel that matches the image's kernel within the policy
	// runhcs enforces, and windows-2022 GitHub runners are Windows
	// Server 2022. nanoserver is the smallest image that still ships
	// cmd.exe so the compile-shape test doesn't need PowerShell.
	testWindowsImageRef = "mcr.microsoft.com/windows/nanoserver:ltsc2022"
	// Marker the in-container cmd.exe writes; the host-side check
	// reads the captured output file and asserts this string appears.
	testWindowsExecMarker = "hpcc-windows-runtime-marker"
)

// Shared containerd client across all integration tests in this
// binary. Same pattern as cdimage_integration_test.go — probing per
// test would burn the daemon's dial timeout on every skip.
var (
	sharedHcsshimClient     *containerd.Client
	sharedHcsshimClientErr  error
	sharedHcsshimClientOnce sync.Once
)

func sharedHcsshim(t *testing.T) *containerd.Client {
	t.Helper()
	sharedHcsshimClientOnce.Do(func() {
		addr := os.Getenv("HPCC_HCSSHIM_ADDRESS")
		if addr == "" {
			addr = `\\.\pipe\containerd-containerd`
		}
		cli, err := containerd.New(addr)
		if err != nil {
			sharedHcsshimClientErr = fmt.Errorf("dial %s: %w", addr, err)
			return
		}
		probeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		ok, err := cli.IsServing(probeCtx)
		if err != nil || !ok {
			_ = cli.Close()
			sharedHcsshimClientErr = fmt.Errorf("daemon at %s not usable (serving=%v err=%v)", addr, ok, err)
			return
		}
		sharedHcsshimClient = cli
	})
	if sharedHcsshimClientErr != nil {
		t.Skipf("containerd not usable: %v", sharedHcsshimClientErr)
	}
	return sharedHcsshimClient
}

// resolveWindowsAmd64Digest pulls ref far enough to know its index,
// then returns the windows/amd64 manifest digest. cdimage.PullImage
// errors on indexes by design, so the test resolves the platform
// digest up front and pulls by `<ref>@<manifest-digest>`.
func resolveWindowsAmd64Digest(t *testing.T, ctx context.Context, cli *containerd.Client, ref string) digest.Digest {
	t.Helper()

	img, err := cli.Pull(ctx, ref)
	if err != nil {
		t.Fatalf("pull %q for digest resolution: %v", ref, err)
	}
	target := img.Target()
	if target.MediaType == ocispec.MediaTypeImageManifest ||
		target.MediaType == "application/vnd.docker.distribution.manifest.v2+json" {
		return target.Digest
	}

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
		if m.Platform.OS == "windows" && m.Platform.Architecture == "amd64" {
			return m.Digest
		}
	}
	t.Fatalf("no windows/amd64 manifest in index for %q", ref)
	return ""
}

// TestHcsshim_EndToEnd_Integration is the only integration test for
// the runtime today: image pull → cdimage pause injection → container
// create with process isolation → Task.Exec → output capture. It's
// gated on `integration && windows`, requires a reachable containerd
// daemon with the runhcs shim available, and skips cleanly when the
// pause binary path isn't supplied (HPCC_HCSSHIM_PAUSE).
func TestHcsshim_EndToEnd_Integration(t *testing.T) {
	pausePath := os.Getenv("HPCC_HCSSHIM_PAUSE")
	if pausePath == "" {
		t.Skip("HPCC_HCSSHIM_PAUSE not set; build pause.exe and point this at it")
	}
	if _, err := os.Stat(pausePath); err != nil {
		t.Fatalf("HPCC_HCSSHIM_PAUSE = %q: %v", pausePath, err)
	}

	cli := sharedHcsshim(t)
	ns := fmt.Sprintf("hpcc-rt-test-%d", time.Now().UnixNano())
	ctx := namespaces.WithNamespace(context.Background(), ns)

	t.Cleanup(func() {
		cleanCtx, cancel := context.WithTimeout(
			namespaces.WithNamespace(context.Background(), ns),
			60*time.Second,
		)
		defer cancel()
		// Best-effort: drop every image record in the namespace so
		// the next run starts clean. Snapshot cleanup follows from
		// the container Delete in container.Stop.
		is := cli.ImageService()
		list, _ := is.List(cleanCtx)
		for _, img := range list {
			_ = is.Delete(cleanCtx, img.Name, images.SynchronousDelete())
		}
	})

	manifestDigest := resolveWindowsAmd64Digest(t, ctx, cli, testWindowsImageRef)
	pinnedRef := fmt.Sprintf("mcr.microsoft.com/windows/nanoserver@%s", manifestDigest)

	store := &cdimage.Store{
		Client: cli,
		Pause:  cdimage.PauseBinaries{WindowsAmd64: pausePath},
		// Must match the snapshotter the runtime below will create
		// its container snapshot under; otherwise the runhcs shim
		// can't find the parent chain. defaultHcsshimSnapshotter is
		// "windows".
		Snapshotter: defaultHcsshimSnapshotter,
	}
	if err := store.PullImage(ctx, pinnedRef, manifestDigest.String()); err != nil {
		t.Fatalf("cdimage PullImage: %v", err)
	}

	addr := os.Getenv("HPCC_HCSSHIM_ADDRESS")
	if addr == "" {
		addr = `\\.\pipe\containerd-containerd`
	}
	rt, err := NewHcsshim(HcsshimOptions{
		Address:   addr,
		Namespace: ns,
		RunDir:    t.TempDir(),
		// Process isolation: GitHub Actions hosted runners don't
		// expose nested virtualization, so Hyper-V isolation can't
		// boot. The §4.1 security boundary is *not* what this test
		// asserts — it asserts the wire (image → runhcs → Exec →
		// copy-out) works end to end on Windows.
		Isolation:     IsolationProcess,
		PauseHostPath: pausePath,
	})
	if err != nil {
		t.Fatalf("NewHcsshim: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	// No inner timeout on Start: image preparation costs scale with
	// image size (nanoserver is ~100MB but a real MSVC toolchain
	// image can hit 30 GB), and the runtime doesn't impose its own
	// deadline either — it respects the caller's ctx. The workflow's
	// `go test -timeout=...` is the only ceiling so test authors who
	// swap in a heavier image don't have to chase a hardcoded minute
	// count here.
	container, err := rt.Start(ctx, ContainerSpec{
		ID:          fmt.Sprintf("hpcc-rt-e2e-%d", time.Now().UnixNano()),
		TenantID:    "test",
		ImageDigest: manifestDigest.String(),
		VCPUs:       1,
		MemoryBytes: 512 * 1024 * 1024,
	})
	if err != nil {
		t.Fatalf("rt.Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, scancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer scancel()
		_ = container.Stop(namespaces.WithNamespace(stopCtx, ns))
	})

	outDir := t.TempDir()
	var stdout, stderr bytes.Buffer

	// cmd.exe runs with `Cwd = /out` (the runtime rewrites that to
	// the per-Exec staging dir under C:\out), so the redirect target
	// `hello.txt` lands inside the staging dir and the runtime's
	// copy-out lifts it to outDir. No inner timeout on Exec either —
	// this command is trivially fast, but the same reasoning applies:
	// real compiles can take many minutes and the runtime defers to
	// ctx.
	res, err := container.Exec(ctx, ExecRequest{
		ExecID:      "e1",
		Argv:        []string{"cmd.exe", "/c", fmt.Sprintf("echo %s> hello.txt", testWindowsExecMarker)},
		Cwd:         "/out",
		OutHostPath: outDir,
		Stdout:      &stdout,
		Stderr:      &stderr,
	})
	if err != nil {
		t.Fatalf("Exec: %v\nstdout=%q\nstderr=%q", err, stdout.String(), stderr.String())
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0\nstdout=%q\nstderr=%q",
			res.ExitCode, stdout.String(), stderr.String())
	}

	out, err := os.ReadFile(filepath.Join(outDir, "hello.txt"))
	if err != nil {
		t.Fatalf("read captured output: %v", err)
	}
	if !strings.Contains(string(out), testWindowsExecMarker) {
		t.Errorf("captured output %q does not contain marker %q", out, testWindowsExecMarker)
	}
}
