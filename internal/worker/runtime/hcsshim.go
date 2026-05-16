package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/aarani/hpcc/internal/protocol/gen"
	"github.com/aarani/hpcc/internal/worker/image/cdimage"
)

// HandlerHcsshim is the config.toml runtime.handler value that selects
// the containerd + hcsshim driver. Wires the worker to a containerd
// daemon that owns a Windows host running the runhcs-wcow-hypervisor
// runtime — each per-tenant container is a Hyper-V utility VM
// (§4.1.1). The handler string matches the OCI runtime name the daemon
// dispatches to.
const HandlerHcsshim = "runhcs-wcow-hypervisor"

// Defaults used when HcsshimOptions leaves a knob empty. The runtime
// and snapshotter values are the standard names containerd ships for
// Hyper-V isolated Windows containers; the namespace mirrors what
// containerd CLIs default to so an operator can poke around with `ctr
// -n hpcc images ls` without overriding anything.
const (
	defaultHcsshimNamespace   = "hpcc"
	defaultHcsshimRuntime     = "io.containerd.runhcs.v1"
	defaultHcsshimSnapshotter = "windows"
)

// IsolationHyperV runs each container as a separate Hyper-V utility
// VM (the production posture; matches the §4.1 boundary claim).
// IsolationProcess runs the container in a Windows Server silo on the
// host kernel — the boundary is namespace-level, not VM-level — and
// exists for environments that cannot nest virtualization (GitHub
// Actions hosted runners, dev laptops without Hyper-V, etc.). The
// runhcs shim accepts both; hpcc picks one at container-create time
// from HcsshimOptions.Isolation.
const (
	IsolationHyperV  = "hyperv"
	IsolationProcess = "process"
)

// In-container path roots the runtime exposes to every Exec. The
// host-side backing dirs are per-container scratch under
// HcsshimOptions.RunDir/<container-id>/{src,out}; per-Exec
// subdirectories named after ExecID isolate concurrent compiles inside
// one warm container. guestPauseDir is the directory the runtime
// bind-mounts read-only into every container; the pause binary lives
// at <guestPauseDir>\pause.exe and is the container's PID 1.
const (
	guestSrcRoot   = `C:\src`
	guestOutRoot   = `C:\out`
	guestPauseDir  = `C:\.hpcc`
	guestPauseExe  = `C:\.hpcc\pause.exe`
	pauseFileName  = "pause.exe"
	pauseMountSub  = ".hpcc-pause-mount"
)

// HcsshimOptions is the host-side configuration for the containerd +
// hcsshim runtime. Address is the containerd dial target (Windows
// default `\\.\pipe\containerd-containerd`); Namespace scopes hpcc's
// images and containers inside containerd; RunDir is where per-
// container host-side staging lives. Runtime/Snapshotter override the
// OCI runtime + snapshotter containerd selects — empty values fall
// back to the runhcs.v1 / windows defaults appropriate for Hyper-V
// isolation.
type HcsshimOptions struct {
	Address     string
	Namespace   string
	RunDir      string
	Runtime     string
	Snapshotter string
	// Isolation is IsolationHyperV (default) or IsolationProcess. Process
	// isolation is for CI / dev only — it shares the host kernel with
	// every other container and breaks the §4.1 security claim. Anything
	// else fails at NewHcsshim so a typo doesn't silently flip a worker
	// out of Hyper-V mode.
	Isolation string
	// PauseHostPath is the absolute path of hpcc-pause.exe on the host.
	// NewHcsshim copies the file into <RunDir>/.hpcc-pause-mount and
	// every container gets that dir bind-mounted read-only at C:\.hpcc
	// so the OCI spec entrypoint can point at C:\.hpcc\pause.exe.
	// Required: cdimage on Windows registers a plain alias of the
	// user's image (no layer injection), so the pause binary has no
	// other way into the container.
	PauseHostPath string
}

// Hcsshim is the containerd + hcsshim Runtime. It owns the
// containerd client and the per-container host scratch dir layout; the
// per-tenant Hyper-V utility VM lifecycle is delegated to the runhcs
// shim via containerd. Compiles dispatch as one `Task.Exec` per
// invocation against the long-running pause binary the prepared image
// runs as PID 1 (§4.2).
type Hcsshim struct {
	opts          HcsshimOptions
	client        *containerd.Client
	pauseMountDir string
}

// NewHcsshim validates the configuration and dials containerd. Empty
// optional fields take the documented defaults so a minimal worker.toml
// (handler + address + run_dir) boots without ceremony, but every
// required path is checked up front so a misconfigured worker fails at
// startup instead of on the first Compile.
func NewHcsshim(opts HcsshimOptions) (*Hcsshim, error) {
	if opts.Address == "" {
		return nil, fmt.Errorf("hcsshim runtime: address is required")
	}
	if opts.RunDir == "" {
		return nil, fmt.Errorf("hcsshim runtime: run_dir is required")
	}
	if opts.Namespace == "" {
		opts.Namespace = defaultHcsshimNamespace
	}
	if opts.Runtime == "" {
		opts.Runtime = defaultHcsshimRuntime
	}
	if opts.Snapshotter == "" {
		opts.Snapshotter = defaultHcsshimSnapshotter
	}
	switch opts.Isolation {
	case "":
		opts.Isolation = IsolationHyperV
	case IsolationHyperV, IsolationProcess:
		// ok
	default:
		return nil, fmt.Errorf("hcsshim runtime: unknown isolation %q (want %q or %q)",
			opts.Isolation, IsolationHyperV, IsolationProcess)
	}
	if opts.PauseHostPath == "" {
		return nil, fmt.Errorf("hcsshim runtime: pause_host_path is required (host path to hpcc-pause.exe)")
	}
	pauseMountDir, err := stagePauseMount(opts.RunDir, opts.PauseHostPath)
	if err != nil {
		return nil, fmt.Errorf("hcsshim runtime: stage pause mount: %w", err)
	}
	cli, err := containerd.New(opts.Address, containerd.WithDefaultNamespace(opts.Namespace))
	if err != nil {
		return nil, fmt.Errorf("hcsshim runtime: dial containerd at %q: %w", opts.Address, err)
	}
	return &Hcsshim{opts: opts, client: cli, pauseMountDir: pauseMountDir}, nil
}

// stagePauseMount copies hpcc-pause.exe from the operator-supplied
// PauseHostPath into a runtime-owned subdirectory under RunDir. Every
// container the runtime starts bind-mounts that subdirectory read-only
// at C:\.hpcc, so the OCI spec can name C:\.hpcc\pause.exe as its
// entrypoint without the pause binary needing to be inside the user's
// image. Done once at NewHcsshim because the source file shouldn't
// change at runtime and per-container copies would burn disk.
func stagePauseMount(runDir, src string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", fmt.Errorf("open %q: %w", src, err)
	}
	defer in.Close()

	mountDir := filepath.Join(runDir, pauseMountSub)
	if err := os.MkdirAll(mountDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %q: %w", mountDir, err)
	}
	dst := filepath.Join(mountDir, pauseFileName)
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return "", fmt.Errorf("create %q: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return "", fmt.Errorf("copy %q -> %q: %w", src, dst, err)
	}
	if err := out.Close(); err != nil {
		return "", fmt.Errorf("close %q: %w", dst, err)
	}
	if err := grantContainerReadExecute(mountDir); err != nil {
		return "", fmt.Errorf("grant container access on %q: %w", mountDir, err)
	}
	return mountDir, nil
}

func (h *Hcsshim) Close() error {
	if h.client == nil {
		return nil
	}
	return h.client.Close()
}

// Start creates a per-tenant Hyper-V utility VM under containerd. The
// flow mirrors the Linux/Firecracker path but the heavy lifting (image
// snapshot, VM boot, pause binary as PID 1) is delegated to
// runhcs.v1: hpcc resolves the prepared image cdimage.Store registered
// at `prepared.hpcc.local/img:<digest>`, creates a containerd
// container with WithImageConfig + WithWindowsHyperV plus the per-
// container src/out VSMB mounts, then NewTask + Start the pause
// binary so the VM stays warm across compiles.
func (h *Hcsshim) Start(ctx context.Context, spec ContainerSpec) (Container, error) {
	if spec.ID == "" {
		return nil, fmt.Errorf("hcsshim: ContainerSpec.ID is required")
	}
	if spec.ImageDigest == "" {
		return nil, fmt.Errorf("hcsshim: ContainerSpec.ImageDigest is required")
	}

	ctx = namespaces.WithNamespace(ctx, h.opts.Namespace)

	hostSrcDir, hostOutDir, hostScratchDir, err := h.prepareScratchDirs(spec.ID)
	if err != nil {
		return nil, fmt.Errorf("hcsshim: prepare scratch dirs: %w", err)
	}

	imgName := cdimage.PreparedImageName(spec.ImageDigest)
	img, err := h.client.GetImage(ctx, imgName)
	if err != nil {
		_ = os.RemoveAll(hostScratchDir)
		return nil, fmt.Errorf("hcsshim: resolve prepared image %q: %w", imgName, err)
	}

	specOpts := []oci.SpecOpts{
		oci.WithImageConfig(img),
		oci.WithMounts([]specs.Mount{
			// Bind mount of the runtime-owned pause dir; its
			// pause.exe is the container's PID 1. cdimage on Windows
			// doesn't inject a layer, so this mount is the only way
			// pause.exe gets into the container. No "ro" option —
			// some Windows mount paths interpret it as noexec, which
			// blocks the entrypoint with ERROR_ACCESS_DENIED.
			{Source: h.pauseMountDir, Destination: guestPauseDir},
			{Source: hostSrcDir, Destination: guestSrcRoot},
			{Source: hostOutDir, Destination: guestOutRoot},
		}),
		// Override the image's entrypoint to the pause binary so the
		// container stays alive across Execs regardless of what the
		// user image declares (nanoserver's default is cmd.exe, which
		// would exit immediately under cio.NullIO).
		oci.WithProcessArgs(guestPauseExe),
	}
	if h.opts.Isolation == IsolationHyperV {
		// Process isolation runs in a Windows Server silo on the host
		// kernel — no Windows.HyperV section, which is exactly the
		// signal the runhcs shim uses to skip uVM allocation. Setting
		// the field unconditionally would force Hyper-V on every CI
		// runner that can't nest virtualization.
		specOpts = append(specOpts, oci.WithWindowsHyperV)
	}
	if spec.VCPUs > 0 {
		specOpts = append(specOpts, oci.WithWindowsCPUCount(uint64(spec.VCPUs)))
	}
	if spec.MemoryBytes > 0 {
		specOpts = append(specOpts, oci.WithMemoryLimit(uint64(spec.MemoryBytes)))
	}

	containerOpts := []containerd.NewContainerOpts{
		containerd.WithImage(img),
		containerd.WithImageName(imgName),
		containerd.WithSnapshotter(h.opts.Snapshotter),
		containerd.WithNewSnapshot(spec.ID, img),
		containerd.WithRuntime(h.opts.Runtime, nil),
		containerd.WithNewSpec(specOpts...),
		containerd.WithContainerLabels(map[string]string{
			"hpcc.dev/tenant":       spec.TenantID,
			"hpcc.dev/user-digest":  spec.ImageDigest,
			"hpcc.dev/container-id": spec.ID,
		}),
	}
	cont, err := h.client.NewContainer(ctx, spec.ID, containerOpts...)
	if err != nil {
		_ = os.RemoveAll(hostScratchDir)
		return nil, fmt.Errorf("hcsshim: create container %s: %w", spec.ID, err)
	}

	task, err := cont.NewTask(ctx, cio.NullIO)
	if err != nil {
		_ = cont.Delete(ctx, containerd.WithSnapshotCleanup)
		_ = os.RemoveAll(hostScratchDir)
		return nil, fmt.Errorf("hcsshim: create task: %w", err)
	}
	if err := task.Start(ctx); err != nil {
		_, _ = task.Delete(ctx)
		_ = cont.Delete(ctx, containerd.WithSnapshotCleanup)
		_ = os.RemoveAll(hostScratchDir)
		return nil, fmt.Errorf("hcsshim: start task: %w", err)
	}

	return &hcsshimContainer{
		owner:          h,
		spec:           spec,
		container:      cont,
		task:           task,
		hostSrcDir:     hostSrcDir,
		hostOutDir:     hostOutDir,
		hostScratchDir: hostScratchDir,
	}, nil
}

// prepareScratchDirs lays out the per-container host scratch root
// before container creation. Subdirs map to the in-container mounts at
// C:\src and C:\out; per-Exec subdirectories appear underneath at
// Exec time.
func (h *Hcsshim) prepareScratchDirs(id string) (src, out, scratch string, err error) {
	scratch = filepath.Join(h.opts.RunDir, id)
	src = filepath.Join(scratch, "src")
	out = filepath.Join(scratch, "out")
	for _, d := range []string{src, out} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", "", "", fmt.Errorf("mkdir %q: %w", d, err)
		}
	}
	return src, out, scratch, nil
}

// hcsshimContainer is one running per-tenant Hyper-V utility VM under
// containerd. The task field is the pause binary (PID 1 inside the
// container) — every compiler invocation dispatches as a Task.Exec
// child of that pause.
type hcsshimContainer struct {
	owner *Hcsshim
	spec  ContainerSpec

	container      containerd.Container
	task           containerd.Task
	hostSrcDir     string
	hostOutDir     string
	hostScratchDir string

	mu      sync.Mutex
	stopped bool
}

func (c *hcsshimContainer) ID() string          { return c.spec.ID }
func (c *hcsshimContainer) TenantID() string    { return c.spec.TenantID }
func (c *hcsshimContainer) ImageDigest() string { return c.spec.ImageDigest }

// State reports RUNNING once Start succeeded. The pause-binary task
// only exits on Stop, so any state other than RUNNING from the
// container's perspective is a crash — handled higher up by the worker
// pool's health-check on next dispatch, not modelled here.
func (c *hcsshimContainer) State() gen.VMState { return gen.VMState_RUNNING }

// Exec runs one compiler invocation inside the warm utility VM. The
// host-side flow is:
//
//  1. Allocate per-Exec staging dirs under the container's src/out
//     scratch roots and copy req.SrcHostPath into the src side.
//  2. Translate /src and /out roots in argv/cwd to C:\src\<ExecID> and
//     C:\out\<ExecID> so the in-container paths match where the
//     mounts surface.
//  3. Task.Exec the translated argv, streaming stdout/stderr to the
//     caller's writers.
//  4. Copy the out-side staging dir back to req.OutHostPath and tear
//     both staging dirs down.
//
// Cleanup runs in defer so a mid-stream cancellation or an Exec error
// doesn't leak per-Exec scratch.
func (c *hcsshimContainer) Exec(ctx context.Context, req ExecRequest) (ExecResult, error) {
	if c.task == nil {
		return ExecResult{ExitCode: -1}, fmt.Errorf("hcsshim: container has no running task")
	}
	if req.ExecID == "" {
		return ExecResult{ExitCode: -1}, fmt.Errorf("hcsshim: ExecRequest.ExecID is required")
	}
	if len(req.Argv) == 0 {
		return ExecResult{ExitCode: -1}, fmt.Errorf("hcsshim: ExecRequest.Argv must be non-empty")
	}

	ctx = namespaces.WithNamespace(ctx, c.owner.opts.Namespace)

	stage, err := c.stageExecDirs(req)
	if err != nil {
		return ExecResult{ExitCode: -1}, fmt.Errorf("stage exec: %w", err)
	}
	defer stage.cleanup()

	argv := translateArgs(req.Argv, stage.guestSrc, stage.guestOut)
	cwd := req.Cwd
	switch {
	case cwd != "":
		cwd = translateExecPath(cwd, stage.guestSrc, stage.guestOut)
	case req.SrcHostPath != "":
		cwd = stage.guestSrc
	}

	pspec, err := c.container.Spec(ctx)
	if err != nil {
		return ExecResult{ExitCode: -1}, fmt.Errorf("hcsshim: read container spec: %w", err)
	}
	proc := *pspec.Process
	proc.Args = argv
	if cwd != "" {
		proc.Cwd = cwd
	}
	if len(req.Env) > 0 {
		proc.Env = append([]string(nil), req.Env...)
	}

	stdout, stderr := req.Stdout, req.Stderr
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	creator := cio.NewCreator(cio.WithStreams(req.Stdin, stdout, stderr))

	process, err := c.task.Exec(ctx, req.ExecID, &proc, creator)
	if err != nil {
		return ExecResult{ExitCode: -1}, fmt.Errorf("hcsshim: task.Exec: %w", err)
	}
	defer func() {
		_, _ = process.Delete(ctx)
	}()

	wait, err := process.Wait(ctx)
	if err != nil {
		_ = process.Kill(ctx, syscall.SIGKILL)
		return ExecResult{ExitCode: -1}, fmt.Errorf("hcsshim: wait setup: %w", err)
	}
	if err := process.Start(ctx); err != nil {
		return ExecResult{ExitCode: -1}, fmt.Errorf("hcsshim: process.Start: %w", err)
	}

	select {
	case status := <-wait:
		exit := int(status.ExitCode())
		if err := status.Error(); err != nil {
			return ExecResult{ExitCode: exit}, fmt.Errorf("hcsshim: exec status: %w", err)
		}
		if err := stage.captureOutputs(req.OutHostPath); err != nil {
			return ExecResult{ExitCode: exit}, fmt.Errorf("hcsshim: capture outputs: %w", err)
		}
		return ExecResult{ExitCode: exit}, nil
	case <-ctx.Done():
		_ = process.Kill(ctx, syscall.SIGKILL)
		// Best-effort drain so the shim doesn't leak a zombie process.
		select {
		case <-wait:
		case <-time.After(5 * time.Second):
		}
		return ExecResult{ExitCode: -1}, ctx.Err()
	}
}

// Stop reaps the warm VM. We always try to delete the task and
// container, even if either step fails, so a wedged shim can't pin a
// container record alongside a half-dead snapshot. Idempotent — repeat
// calls return nil once the first one finished.
func (c *hcsshimContainer) Stop(ctx context.Context) error {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return nil
	}
	c.stopped = true
	c.mu.Unlock()

	ctx = namespaces.WithNamespace(ctx, c.owner.opts.Namespace)

	var errs []error
	if c.task != nil {
		killCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		_ = c.task.Kill(killCtx, syscall.SIGKILL)
		wait, werr := c.task.Wait(killCtx)
		if werr == nil {
			select {
			case <-wait:
			case <-killCtx.Done():
			}
		}
		cancel()
		if _, err := c.task.Delete(ctx); err != nil {
			errs = append(errs, fmt.Errorf("task.Delete: %w", err))
		}
	}
	if c.container != nil {
		if err := c.container.Delete(ctx, containerd.WithSnapshotCleanup); err != nil {
			errs = append(errs, fmt.Errorf("container.Delete: %w", err))
		}
	}
	if c.hostScratchDir != "" {
		if err := os.RemoveAll(c.hostScratchDir); err != nil {
			errs = append(errs, fmt.Errorf("remove scratch %q: %w", c.hostScratchDir, err))
		}
	}
	return errors.Join(errs...)
}

// execStage owns one per-Exec staging area: per-ExecID subdirs under
// the container's src/out scratch roots, plus the matching in-VM
// paths the runtime rewrites argv against.
type execStage struct {
	hostSrc, hostOut   string
	guestSrc, guestOut string
}

func (s *execStage) cleanup() {
	if s.hostSrc != "" {
		_ = os.RemoveAll(s.hostSrc)
	}
	if s.hostOut != "" {
		_ = os.RemoveAll(s.hostOut)
	}
}

// captureOutputs lifts whatever the compile dropped under
// stage.hostOut back to req.OutHostPath. Missing destination = no
// capture (matches the Linux runner contract).
func (s *execStage) captureOutputs(outHost string) error {
	if outHost == "" {
		return nil
	}
	if err := os.MkdirAll(outHost, 0o755); err != nil {
		return fmt.Errorf("mkdir %q: %w", outHost, err)
	}
	return copyTree(s.hostOut, outHost)
}

func (c *hcsshimContainer) stageExecDirs(req ExecRequest) (*execStage, error) {
	hostSrc := filepath.Join(c.hostSrcDir, req.ExecID)
	hostOut := filepath.Join(c.hostOutDir, req.ExecID)
	if err := os.MkdirAll(hostOut, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir out staging %q: %w", hostOut, err)
	}
	if req.SrcHostPath != "" {
		if err := copyTree(req.SrcHostPath, hostSrc); err != nil {
			_ = os.RemoveAll(hostOut)
			return nil, fmt.Errorf("copy src %q -> %q: %w", req.SrcHostPath, hostSrc, err)
		}
	} else {
		// Even when no source is shipped, create the dir so argv path
		// rewriting that mentions /src resolves to a real (empty) tree
		// rather than a nonexistent path that confuses the compiler.
		if err := os.MkdirAll(hostSrc, 0o755); err != nil {
			_ = os.RemoveAll(hostOut)
			return nil, fmt.Errorf("mkdir src staging %q: %w", hostSrc, err)
		}
	}
	return &execStage{
		hostSrc:  hostSrc,
		hostOut:  hostOut,
		guestSrc: guestSrcRoot + `\` + req.ExecID,
		guestOut: guestOutRoot + `\` + req.ExecID,
	}, nil
}

// translateArgs rewrites every argv element. Done as a separate helper
// so unit tests can exercise the path-translation rules independently
// of the rest of the runtime — the rules are identical on Windows and
// Linux above the boundary: leading /src or /out maps to the per-Exec
// staging directory.
func translateArgs(argv []string, srcRoot, outRoot string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = translateExecPath(a, srcRoot, outRoot)
	}
	return out
}

// copyTree mirrors src into dst preserving the directory shape and
// regular-file contents. Symlinks, devices, etc. are intentionally
// dropped — the compile staging dirs only contain regular files and
// directories in practice; supporting the long tail would add a Windows
// reparse-point trust surface for no benefit.
func copyTree(src, dst string) error {
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return copyRegularFile(src, dst, st.Mode().Perm())
	}
	return filepath.WalkDir(src, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			info, ierr := d.Info()
			if ierr != nil {
				return ierr
			}
			if err := os.MkdirAll(target, info.Mode().Perm()); err != nil {
				return fmt.Errorf("mkdir %q: %w", target, err)
			}
			return nil
		case d.Type().IsRegular():
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("mkdir parent of %q: %w", target, err)
			}
			info, ierr := d.Info()
			if ierr != nil {
				return ierr
			}
			return copyRegularFile(path, target, info.Mode().Perm())
		default:
			// Skip symlinks, devices, sockets, etc. Bridging those
			// across host/Hyper-V boundaries is undefined territory.
			return nil
		}
	})
}

func copyRegularFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %q: %w", src, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("create %q: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy %q -> %q: %w", src, dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close %q: %w", dst, err)
	}
	return nil
}

