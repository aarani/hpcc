//go:build integration && linux

package runtime

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/crane"
	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/aarani/hpcc/firecracker/client/operations"
	"github.com/aarani/hpcc/internal/worker/image/rootfs"
)

// End-to-end tests for the raw Firecracker runtime. Each one boots a
// real microVM under jailer + firecracker on a CI host with KVM,
// running the in-VM hpcc-agent as PID 1, then either inspects the
// boot path or dispatches a real Exec over vsock and checks the
// reply. Skips unless:
//   - process is root (jailer needs CAP_SYS_ADMIN-equivalent for
//     cgroup setup + chroot)
//   - /dev/kvm is accessible
//   - HPCC_FIRECRACKER_BIN, HPCC_JAILER_BIN, HPCC_TEST_KERNEL point
//     at real artifacts (the workflow downloads these; locally, set
//     them by hand)
//
// The rootfs is built through the production rootfs.Store pipeline
// against a real public image (busybox) with the agent injected at
// /.hpcc/agent — exercises the same image→squashfs streaming path
// operators rely on, including the hardlink handling that
// busybox-style images depend on.

const (
	envFirecrackerBin = "HPCC_FIRECRACKER_BIN"
	envJailerBin      = "HPCC_JAILER_BIN"
	envTestKernel     = "HPCC_TEST_KERNEL"

	// e2eBusyboxRef is the small fast rootfs every cheap test boots
	// against. Hardlink-heavy (every applet links to /bin/busybox) so
	// it also keeps the rootfs.Store hardlink-fallback regressed.
	e2eBusyboxRef = "docker.io/library/busybox:1.37"

	// e2eCompilerRef is the rootfs the real-compile test boots
	// against. Chainguard's gcc-glibc:latest-dev ships gcc +
	// binutils + glibc headers, publicly pullable, ~200 MB on disk;
	// exercises the "image with a real toolchain" path the worker
	// is built for.
	//
	// Pinned to a digest, not the floating :latest-dev tag, so two
	// CI runs days apart test bit-identical rootfs contents and a
	// regression in the registry doesn't silently green-wash a
	// runtime-side bug. crane.Digest re-resolves with WithPlatform
	// so this still works whether the digest is an OCI index or a
	// platform-specific manifest — the test always pins to the
	// linux/amd64 manifest crane.Pull will actually fetch. Bump
	// when intentionally upgrading the toolchain.
	e2eCompilerRef = "cgr.dev/chainguard/gcc-glibc@sha256:56a34fd7a965fe220676cf124993a2949c65bf0798a5a0fbe21cdbcb11a2e939"
)

// Shared bootstrap state. The agent is built once across all tests
// (same binary regardless of image); each user image is pulled and
// turned into a squashfs once, cached by ref. Done lazily — TestMain
// can't t.Skip, and we want the host-prereq checks (root, /dev/kvm,
// fc/jailer paths) to govern whether bootstrap runs at all.
var (
	e2eAgentOnce sync.Once
	e2eAgentPath string
	e2eAgentErr  error

	e2eRootfsMu    sync.Mutex
	e2eRootfsCache = map[string]e2eRootfsEntry{}
)

type e2eRootfsEntry struct {
	dir    string
	digest string
	err    error
}

// e2eAgent builds the in-VM hpcc-agent statically once and caches
// the resulting binary path. Cached under os.TempDir (not t.TempDir)
// so subsequent tests that trigger a different image's bootstrap
// can reuse the same agent bytes.
func e2eAgent(t *testing.T) string {
	t.Helper()
	e2eAgentOnce.Do(func() {
		dir, err := os.MkdirTemp("", "hpcc-e2e-agent-*")
		if err != nil {
			e2eAgentErr = fmt.Errorf("mktemp: %w", err)
			return
		}
		out := filepath.Join(dir, "hpcc-agent")
		cmd := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", out, ".")
		cmd.Dir = filepath.Join(moduleRoot(t), "agent")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
		if b, err := cmd.CombinedOutput(); err != nil {
			e2eAgentErr = fmt.Errorf("go build agent: %v\n%s", err, b)
			return
		}
		e2eAgentPath = out
	})
	if e2eAgentErr != nil {
		t.Fatalf("e2e agent build: %v", e2eAgentErr)
	}
	return e2eAgentPath
}

// e2eEnsureRootfs pulls imageRef and writes a prepared rootfs.sqsh
// for it once, caching the result. Concurrent calls (different
// images) serialize on the mutex; the build is CPU-bound and tests
// run serially anyway, so the contention is irrelevant.
func e2eEnsureRootfs(t *testing.T, imageRef string) (rootfsDir, digest string) {
	t.Helper()
	e2eRootfsMu.Lock()
	defer e2eRootfsMu.Unlock()

	if r, ok := e2eRootfsCache[imageRef]; ok {
		if r.err != nil {
			t.Fatalf("e2e rootfs %s: %v", imageRef, r.err)
		}
		return r.dir, r.digest
	}

	agentPath := e2eAgent(t)
	dir, dgst, err := buildE2ERootfs(t, imageRef, agentPath)
	e2eRootfsCache[imageRef] = e2eRootfsEntry{dir: dir, digest: dgst, err: err}
	if err != nil {
		t.Fatalf("e2e rootfs %s: %v", imageRef, err)
	}
	return dir, dgst
}

// requireRunnable bails out early when the host can't run firecracker
// e2e — non-root, no /dev/kvm, missing binaries. Returns the resolved
// FirecrackerOptions paths so the test can stop worrying about env
// vars.
func requireRunnable(t *testing.T) (fcBin, jailerBin, kernelPath string) {
	t.Helper()
	if syscall.Geteuid() != 0 {
		t.Skip("firecracker e2e requires root for jailer (cgroups + chroot)")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("/dev/kvm not available: %v", err)
	}
	fcBin = os.Getenv(envFirecrackerBin)
	jailerBin = os.Getenv(envJailerBin)
	kernelPath = os.Getenv(envTestKernel)
	if fcBin == "" || jailerBin == "" || kernelPath == "" {
		t.Skipf("set %s, %s, %s to run", envFirecrackerBin, envJailerBin, envTestKernel)
	}
	for _, p := range []string{fcBin, jailerBin, kernelPath} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("required artifact %q missing: %v", p, err)
		}
	}
	return
}

// bootSpec parameterizes bootContainer. Zero values pick sane
// defaults: busybox rootfs, 256 MiB RAM, 1 vCPU. Pass non-zero
// values for the gcc-compile test, which needs a bigger image
// rootfs and a few hundred extra MB of guest RAM.
type bootSpec struct {
	vmID     string
	imageRef string // "" → e2eBusyboxRef
	memBytes int64  // 0 → 256 MiB
	vcpus    int32  // 0 → 1
}

// bootContainer prepares a fresh microVM against the requested
// rootfs and returns a started Container plus a per-test out dir
// for the runner to materialize Exec outputs into. t.Cleanup tears
// the VM down.
func bootContainer(t *testing.T, ctx context.Context, spec bootSpec) (Container, string) {
	t.Helper()
	if spec.imageRef == "" {
		spec.imageRef = e2eBusyboxRef
	}
	if spec.memBytes == 0 {
		spec.memBytes = 256 * 1024 * 1024
	}
	if spec.vcpus == 0 {
		spec.vcpus = 1
	}

	fcBin, jailerBin, kernelPath := requireRunnable(t)
	rootfsDir, digest := e2eEnsureRootfs(t, spec.imageRef)

	runDir := t.TempDir()
	outDir := t.TempDir()
	uid, gid := pickJailerCreds()
	// jailer needs the chroot base + out dir traversable by the
	// dropped uid/gid; t.TempDir's 0700 owned by root won't do.
	if err := os.Chmod(runDir, 0o755); err != nil {
		t.Fatalf("relax run_dir perms: %v", err)
	}
	if err := os.Chmod(outDir, 0o755); err != nil {
		t.Fatalf("relax out_dir perms: %v", err)
	}

	fc, err := NewFirecracker(FirecrackerOptions{
		FirecrackerBin: fcBin,
		JailerBin:      jailerBin,
		KernelImage:    kernelPath,
		RootfsDir:      rootfsDir,
		RunDir:         runDir,
		UID:            uid,
		GID:            gid,
	})
	if err != nil {
		t.Fatalf("NewFirecracker: %v", err)
	}

	container, err := fc.Start(ctx, ContainerSpec{
		ID:          spec.vmID,
		TenantID:    "tenant-e2e",
		ImageDigest: digest,
		VCPUs:       spec.vcpus,
		MemoryBytes: spec.memBytes,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		_ = container.Stop(context.Background())
	})
	return container, outDir
}

// TestFirecracker_BootsVM_Integration verifies the boot path: VM
// comes up under jailer with the agent listening on vsock, the
// runner successfully dials it during Start, and the firecracker
// API still reports a sane state.
func TestFirecracker_BootsVM_Integration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	container, _ := bootContainer(t, ctx, bootSpec{vmID: "fc-e2e-boot"})

	if got := container.State(); got.String() != "RUNNING" {
		t.Errorf("State() = %v, want RUNNING", got)
	}

	fcc := container.(*firecrackerContainer)
	select {
	case <-fcc.exited:
		t.Fatal("firecracker process exited before assertions ran")
	default:
	}

	descCtx, descCancel := context.WithTimeout(ctx, 5*time.Second)
	defer descCancel()
	desc, err := fcc.fc.Operations.DescribeInstance(operations.NewDescribeInstanceParamsWithContext(descCtx))
	if err != nil {
		t.Fatalf("DescribeInstance: %v", err)
	}
	if desc.Payload == nil || desc.Payload.State == nil {
		t.Fatalf("DescribeInstance returned nil payload/state: %+v", desc.Payload)
	}
	if got := *desc.Payload.State; got == "Not started" {
		t.Errorf("instance state = %q after InstanceStart", got)
	} else {
		t.Logf("firecracker state = %q (vmm %s)", got, derefStr(desc.Payload.VmmVersion))
	}

	if err := container.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-fcc.exited:
	case <-time.After(5 * time.Second):
		t.Fatal("firecracker did not exit within 5s of Stop")
	}
	if _, err := os.Stat(fcc.chrootDir); !os.IsNotExist(err) {
		t.Errorf("chroot %q still present after Stop (err=%v)", fcc.chrootDir, err)
	}
}

// TestFirecracker_ExecCommand_Integration is the real proof of the
// vsock dispatch path: boot a VM, send an Exec request over the
// agent's gRPC stream, and verify the agent ran the requested
// command and shipped stdout back. busybox's /bin/echo is enough —
// we're testing the runner↔agent loop, not the toolchain.
func TestFirecracker_ExecCommand_Integration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	container, outDir := bootContainer(t, ctx, bootSpec{vmID: "fc-e2e-exec"})

	var stdout, stderr bytes.Buffer
	res, err := container.Exec(ctx, ExecRequest{
		ExecID:      "exec-echo",
		Argv:        []string{"/bin/echo", "hello from inside the vm"},
		OutHostPath: outDir,
		Stdout:      &stdout,
		Stderr:      &stderr,
	})
	if err != nil {
		t.Fatalf("Exec: %v\nstderr=%q", err, stderr.String())
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0; stderr=%q", res.ExitCode, stderr.String())
	}
	got := strings.TrimRight(stdout.String(), "\n")
	if want := "hello from inside the vm"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Logf("stderr (non-empty but echo exited 0): %q", stderr.String())
	}
}

// TestFirecracker_ExecWithFiles_Integration exercises the file
// transport: ship one input file in, run a command that reads it
// and writes an output, verify the output bytes round-trip back to
// req.OutHostPath. Uses busybox's /bin/cp because (a) it's there
// regardless of toolchain, (b) it's the simplest "read input,
// produce output" command we can lean on.
func TestFirecracker_ExecWithFiles_Integration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	container, outDir := bootContainer(t, ctx, bootSpec{vmID: "fc-e2e-files"})

	// Stage input on the host. The runner walks SrcHostPath and
	// ships every regular file it finds; agent reassembles under
	// /run/hpcc/src/<exec>/<rel>.
	srcDir := t.TempDir()
	if err := os.Chmod(srcDir, 0o755); err != nil {
		t.Fatalf("relax src_dir perms: %v", err)
	}
	const wantContent = "the quick brown fox jumps over the lazy dog\n"
	srcFile := filepath.Join(srcDir, "input.txt")
	if err := os.WriteFile(srcFile, []byte(wantContent), 0o644); err != nil {
		t.Fatalf("write input: %v", err)
	}

	var stdout, stderr bytes.Buffer
	// /src and /out get rewritten by translateExecPath to the
	// in-VM staging dirs the runner already told the agent about
	// in the ExecHeader. So the argv looks like a normal compile.
	res, err := container.Exec(ctx, ExecRequest{
		ExecID:      "exec-cp",
		Argv:        []string{"/bin/cp", "/src/input.txt", "/out/copy.txt"},
		SrcHostPath: srcDir,
		OutHostPath: outDir,
		Stdout:      &stdout,
		Stderr:      &stderr,
	})
	if err != nil {
		t.Fatalf("Exec: %v\nstdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d (cp failed); stdout=%q stderr=%q",
			res.ExitCode, stdout.String(), stderr.String())
	}

	// The agent should have shipped /run/hpcc/out/<exec>/copy.txt
	// back; the runner writes it under outDir/copy.txt.
	got, err := os.ReadFile(filepath.Join(outDir, "copy.txt"))
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if string(got) != wantContent {
		t.Errorf("output bytes mismatch:\n got  %q\n want %q", got, wantContent)
	}
}

// TestFirecracker_ExecCompile_Integration is the real-compile
// proof: boot a VM whose rootfs is a real toolchain image
// (chainguard/gcc-glibc), ship a one-line C source over vsock,
// invoke gcc inside the VM, verify a valid ELF object lands back
// on the host. This is the end-to-end any production worker has to
// pass — no synthetic stand-ins.
//
// Bumped resources vs. the busybox tests: gcc + glibc image rootfs
// is bigger, gcc itself uses real RAM during compilation.
func TestFirecracker_ExecCompile_Integration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	container, outDir := bootContainer(t, ctx, bootSpec{
		vmID:     "fc-e2e-compile",
		imageRef: e2eCompilerRef,
		memBytes: 1024 * 1024 * 1024, // 1 GiB — gcc is hungrier than echo
		vcpus:    2,
	})

	srcDir := t.TempDir()
	if err := os.Chmod(srcDir, 0o755); err != nil {
		t.Fatalf("relax src_dir perms: %v", err)
	}
	const src = "int main(void) { return 42; }\n"
	if err := os.WriteFile(filepath.Join(srcDir, "hello.c"), []byte(src), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	var stdout, stderr bytes.Buffer
	res, err := container.Exec(ctx, ExecRequest{
		ExecID:      "exec-gcc",
		Argv:        []string{"/usr/bin/gcc", "-c", "/src/hello.c", "-o", "/out/hello.o"},
		SrcHostPath: srcDir,
		OutHostPath: outDir,
		Stdout:      &stdout,
		Stderr:      &stderr,
	})
	if err != nil {
		t.Fatalf("Exec: %v\nstdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	if res.ExitCode != 0 {
		t.Fatalf("gcc exit code = %d (compile failed)\nstdout=%q\nstderr=%q",
			res.ExitCode, stdout.String(), stderr.String())
	}

	obj, err := os.ReadFile(filepath.Join(outDir, "hello.o"))
	if err != nil {
		t.Fatalf("read hello.o: %v", err)
	}
	// ELF magic: 0x7f 'E' 'L' 'F'. If gcc shipped something else
	// back, the file-streaming path is corrupting bytes — the
	// magic check catches that immediately, and the size check
	// catches truncation/zero-byte writes.
	if len(obj) < 64 {
		t.Fatalf("hello.o is suspiciously small (%d bytes)", len(obj))
	}
	if !bytes.HasPrefix(obj, []byte{0x7f, 'E', 'L', 'F'}) {
		t.Errorf("hello.o is not an ELF object; first 16 bytes = % x", obj[:16])
	}
	t.Logf("compiled hello.o: %d bytes ELF object", len(obj))
}

// buildE2ERootfs runs the slow per-image bootstrap: pull imageRef
// at a platform-pinned digest, then rootfs.Store.PullImage onto a
// fresh cache dir with /.hpcc/agent injected. Returns the rootfs
// cache dir and the user-image digest the runtime uses to find it.
//
// The cache dir lives under os.TempDir() (not t.TempDir) so it
// survives across the multiple tests sharing this bootstrap.
func buildE2ERootfs(t *testing.T, imageRef, agentPath string) (string, string, error) {
	t.Helper()

	rootfsDir, err := os.MkdirTemp("", "hpcc-e2e-rootfs-*")
	if err != nil {
		return "", "", err
	}
	if err := os.Chmod(rootfsDir, 0o755); err != nil {
		return "", "", err
	}

	store := &rootfs.Store{
		CacheDir: rootfsDir,
		Agent: rootfs.AgentBinaries{
			LinuxAmd64: agentPath,
			LinuxArm64: agentPath,
		},
	}

	platform := &v1.Platform{OS: "linux", Architecture: "amd64"}
	digest, err := crane.Digest(imageRef, crane.WithPlatform(platform))
	if err != nil {
		return "", "", fmt.Errorf("digest %s: %w", imageRef, err)
	}
	// Pin the ref to the platform-specific manifest digest so the
	// rootfs.Store digest check matches what crane.Pull resolves.
	// For tagless refs like "image@sha256:..." we still split on ":"
	// — there's no tag part, so SplitN("@" + ref, ":", 2)[0] just
	// strips the digest — but for safety, use a more careful split:
	// if there's no tag, just use the bare ref + "@digest".
	base := imageRef
	if idx := strings.IndexByte(imageRef, '@'); idx >= 0 {
		base = imageRef[:idx]
	} else if idx := strings.LastIndexByte(imageRef, ':'); idx > strings.LastIndexByte(imageRef, '/') {
		base = imageRef[:idx]
	}
	pinnedRef := base + "@" + digest
	if err := store.PullImage(context.Background(), pinnedRef, digest); err != nil {
		return "", "", fmt.Errorf("pull %s: %w", pinnedRef, err)
	}
	return rootfsDir, digest, nil
}

// pickJailerCreds returns a non-root uid/gid for jailer to drop to.
// Running under sudo, $SUDO_UID/$SUDO_GID names the invoking user
// (preferred — they own the temp dirs); otherwise fall back to
// nobody:nogroup.
func pickJailerCreds() (int, int) {
	if u := os.Getenv("SUDO_UID"); u != "" {
		if g := os.Getenv("SUDO_GID"); g != "" {
			ui, err1 := strconv.Atoi(u)
			gi, err2 := strconv.Atoi(g)
			if err1 == nil && err2 == nil && ui > 0 && gi > 0 {
				return ui, gi
			}
		}
	}
	return 65534, 65534
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for d := wd; d != "/"; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
	}
	t.Fatal("no go.mod found above test cwd")
	return ""
}

func derefStr(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
