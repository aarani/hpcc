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
	"google.golang.org/grpc"

	"github.com/aarani/hpcc/internal/protocol/gen"
	"github.com/aarani/hpcc/internal/worker/image/cdimage"

	"github.com/containerd/containerd/v2/core/containers"
)

// withNoWindowsNIC zeroes spec.Windows.Network. The runhcs shim
// reads EndpointList / NetworkNamespace to decide whether to attach
// an HNS endpoint (and, for hyperv, whether the utility VM gets a
// synthetic NIC). Leaving the field nil yields "no networking,"
// which is what hpcc wants — compiles dispatch over HvSocket
// (hyperv) or Task.Exec (process), so the container needs no IP
// stack and exposing one would only widen the attack surface.
func withNoWindowsNIC(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
	if s.Windows == nil {
		s.Windows = &specs.Windows{}
	}
	s.Windows.Network = nil
	return nil
}

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
	guestSrcRoot  = `C:\src`
	guestOutRoot  = `C:\out`
	guestPauseDir = `C:\.hpcc`
	guestPauseExe = `C:\.hpcc\pause.exe`
	guestAgentExe = `C:\.hpcc\agent.exe`
	pauseFileName = "pause.exe"
	pauseMountSub = ".hpcc-pause-mount"
	agentFileName = "agent.exe"
	agentMountSub = ".hpcc-agent-mount"

	// Per-Exec staging roots the Windows agent (agent/server.go +
	// agent/server_windows.go) creates inside the utility VM —
	// stagingRoot = C:\hpcc, per-Exec subdirs at .\src\<ExecID> and
	// .\out\<ExecID>. Used by execViaAgentTransport to rewrite the
	// /src and /out tokens in argv/cwd to the absolute paths the
	// agent's runCompiler will chdir into and the agent's
	// streamOutputs will walk.
	guestAgentStagingSrc = `C:\hpcc\src`
	guestAgentStagingOut = `C:\hpcc\out`
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
	// PauseHostPath is the absolute path of hpcc-pause.exe on the
	// host. The runtime copies the file into the per-runtime mount
	// dir at NewHcsshim time. Every process-isolated container gets
	// that dir bind-mounted read-only at C:\.hpcc with pause.exe as
	// its entrypoint — the §4.1.1 process-isolation path keeps
	// Task.Exec + copyTree for staging because there's no partition
	// boundary to stream across.
	PauseHostPath string
	// AgentHostPath is the absolute path of hpcc-agent.exe on the
	// host. Same staging treatment as PauseHostPath, but bind-mounted
	// into Hyper-V isolated containers as their entrypoint. The agent
	// listens on HvSocket and the runtime dials it post-Start to
	// drive compiles via the bidi-streaming AgentService.Exec instead
	// of Task.Exec + per-Exec file copy — see plan §4.1.1 "Why not
	// VSMB" for the rationale. Required when Isolation == hyperv.
	AgentHostPath string
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
	agentMountDir string // empty when Isolation != hyperv
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
	pauseMountDir, err := stageEntrypointMount(opts.RunDir, pauseMountSub, pauseFileName, opts.PauseHostPath)
	if err != nil {
		return nil, fmt.Errorf("hcsshim runtime: stage pause mount: %w", err)
	}
	var agentMountDir string
	if opts.Isolation == IsolationHyperV {
		// Agent is only used under Hyper-V isolation; process
		// isolation keeps the pause + Task.Exec path. Require it
		// explicitly so a hyperv-configured worker fails at startup
		// rather than at the first Compile.
		if opts.AgentHostPath == "" {
			return nil, fmt.Errorf("hcsshim runtime: agent_host_path is required for Hyper-V isolation (host path to hpcc-agent.exe)")
		}
		agentMountDir, err = stageEntrypointMount(opts.RunDir, agentMountSub, agentFileName, opts.AgentHostPath)
		if err != nil {
			return nil, fmt.Errorf("hcsshim runtime: stage agent mount: %w", err)
		}
	}
	cli, err := containerd.New(opts.Address, containerd.WithDefaultNamespace(opts.Namespace))
	if err != nil {
		return nil, fmt.Errorf("hcsshim runtime: dial containerd at %q: %w", opts.Address, err)
	}
	return &Hcsshim{
		opts:          opts,
		client:        cli,
		pauseMountDir: pauseMountDir,
		agentMountDir: agentMountDir,
	}, nil
}

// stageEntrypointMount copies one host binary (pause.exe or
// agent.exe) into a runtime-owned subdirectory under RunDir and
// returns the staged directory path. Every container the runtime
// starts bind-mounts that subdirectory read-only at C:\.hpcc, so
// the OCI spec can name C:\.hpcc\<binary> as its entrypoint
// without the binary needing to be inside the user's image. Done
// once at NewHcsshim because the source file shouldn't change at
// runtime and per-container copies would burn disk.
//
// The ACL grant fires after the copy so the bind-mount is readable
// + executable by the in-container ContainerUser SID; the runner
// account's restrictive temp-dir ACL otherwise inherits onto the
// staged file and the entrypoint dies with ERROR_ACCESS_DENIED at
// hcs::System::CreateProcess.
func stageEntrypointMount(runDir, sub, binName, src string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", fmt.Errorf("open %q: %w", src, err)
	}
	defer in.Close()

	mountDir := filepath.Join(runDir, sub)
	if err := os.MkdirAll(mountDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %q: %w", mountDir, err)
	}
	dst := filepath.Join(mountDir, binName)
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

// Start creates a per-tenant container under containerd, branched by
// isolation mode:
//
//   - Hyper-V isolation: the container is a fresh utility VM with
//     hpcc-agent.exe as PID 1. The host bind-mounts only the agent
//     dir (no C:\src / C:\out) and dials the agent over HvSocket
//     after Task.Start; subsequent Exec calls stream inputs/outputs
//     through the agent's bidi-gRPC RPC, no host-disk staging.
//   - Process isolation: the container runs in a Windows Server
//     silo on the host kernel with pause.exe as PID 1; per-Exec
//     compiles dispatch as Task.Exec calls under that pause and use
//     copyTree to stage src and capture out. Hyper-V provides no
//     security boundary in this mode, so there's no point in
//     paying the gRPC streaming cost either — this is the CI / dev
//     path on hosts that can't nest virtualization.
//
// The branch is decided here; everything downstream
// (hcsshimContainer.Exec, .Stop) checks the agent connection's
// presence to pick the right path.
func (h *Hcsshim) Start(ctx context.Context, spec ContainerSpec) (Container, error) {
	if spec.ID == "" {
		return nil, fmt.Errorf("hcsshim: ContainerSpec.ID is required")
	}
	if spec.ImageDigest == "" {
		return nil, fmt.Errorf("hcsshim: ContainerSpec.ImageDigest is required")
	}

	ctx = namespaces.WithNamespace(ctx, h.opts.Namespace)

	imgName := cdimage.PreparedImageName(spec.ImageDigest)
	img, err := h.client.GetImage(ctx, imgName)
	if err != nil {
		return nil, fmt.Errorf("hcsshim: resolve prepared image %q: %w", imgName, err)
	}

	useAgent := h.opts.Isolation == IsolationHyperV

	var hostSrcDir, hostOutDir, hostScratchDir string
	if !useAgent {
		hostSrcDir, hostOutDir, hostScratchDir, err = h.prepareScratchDirs(spec.ID)
		if err != nil {
			return nil, fmt.Errorf("hcsshim: prepare scratch dirs: %w", err)
		}
	}

	mounts, entrypoint := h.containerMountsAndEntrypoint(hostSrcDir, hostOutDir)
	specOpts := []oci.SpecOpts{
		oci.WithImageConfig(img),
		oci.WithMounts(mounts),
		// Override the image's entrypoint to either pause.exe or
		// agent.exe so the container stays alive across Execs
		// regardless of what the user image declares (nanoserver's
		// default is cmd.exe, which would exit immediately under
		// cio.NullIO).
		oci.WithProcessArgs(entrypoint),
		// Explicitly zero Windows.Network so the runhcs shim attaches
		// no HNS endpoint and (under Hyper-V isolation) the utility VM
		// is started with no synthetic NIC. The host ↔ container
		// channel is HvSocket — agent.exe under hyperv, Task.Exec
		// under process — neither needs IP. Asserting nil here keeps
		// a future oci.WithImageConfig that learns to propagate
		// image-level Windows.Network from silently re-attaching one.
		withNoWindowsNIC,
	}
	if useAgent {
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
	cleanupScratch := func() {
		if hostScratchDir != "" {
			_ = os.RemoveAll(hostScratchDir)
		}
	}
	cont, err := h.client.NewContainer(ctx, spec.ID, containerOpts...)
	if err != nil {
		cleanupScratch()
		return nil, fmt.Errorf("hcsshim: create container %s: %w", spec.ID, err)
	}

	task, err := cont.NewTask(ctx, cio.NullIO)
	if err != nil {
		_ = cont.Delete(ctx, containerd.WithSnapshotCleanup)
		cleanupScratch()
		return nil, fmt.Errorf("hcsshim: create task: %w", err)
	}
	if err := task.Start(ctx); err != nil {
		_, _ = task.Delete(ctx)
		_ = cont.Delete(ctx, containerd.WithSnapshotCleanup)
		cleanupScratch()
		return nil, fmt.Errorf("hcsshim: start task: %w", err)
	}

	hc := &hcsshimContainer{
		owner:          h,
		spec:           spec,
		container:      cont,
		task:           task,
		hostSrcDir:     hostSrcDir,
		hostOutDir:     hostOutDir,
		hostScratchDir: hostScratchDir,
	}
	if useAgent {
		conn, err := h.dialContainerAgent(ctx, spec.ID)
		if err != nil {
			_ = task.Kill(ctx, syscall.SIGKILL)
			_, _ = task.Delete(ctx)
			_ = cont.Delete(ctx, containerd.WithSnapshotCleanup)
			return nil, fmt.Errorf("hcsshim: dial agent: %w", err)
		}
		hc.agentConn = conn
	}
	return hc, nil
}

// containerMountsAndEntrypoint picks the bind-mount set and the OCI
// process entrypoint for the active isolation mode. Process isolation
// needs C:\src + C:\out host-side staging (copyTree-based Exec) plus
// the pause-binary mount; Hyper-V isolation just needs the agent
// mount because the agent streams files in-band over HvSocket.
func (h *Hcsshim) containerMountsAndEntrypoint(srcDir, outDir string) ([]specs.Mount, string) {
	if h.opts.Isolation == IsolationHyperV {
		return []specs.Mount{
			// Read-only bind mount of the runtime-owned agent dir.
			// agent.exe is shared by every Hyper-V container the
			// runtime starts and lives across container lifetimes
			// (staged once in NewHcsshim, mounted into every
			// container); letting a tenant compile write to it
			// would let one tenant poison the entrypoint future
			// tenants run as PID 1. Defence in depth: the file ACL
			// already grants Everyone RX only (no write), but "ro"
			// blocks writes too if a future runhcs version honours
			// it.
			{Source: h.agentMountDir, Destination: guestPauseDir, Options: []string{"ro"}},
		}, guestAgentExe
	}
	return []specs.Mount{
		// Same read-only argument applies to the pause dir.
		{Source: h.pauseMountDir, Destination: guestPauseDir, Options: []string{"ro"}},
		{Source: srcDir, Destination: guestSrcRoot},
		{Source: outDir, Destination: guestOutRoot},
	}, guestPauseExe
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
	// Container processes run as a non-admin SID (ContainerUser in
	// stock nanoserver / servercore) and inherit the host runner's
	// restrictive temp-dir ACL otherwise — cmd.exe inside the
	// container gets "Access is denied" trying to write its output
	// to C:\out\<ExecID>\... Apply the grant once on scratch; the
	// inheritance flags carry through to per-Exec subdirs.
	if err := grantContainerModify(scratch); err != nil {
		return "", "", "", fmt.Errorf("grant container access on %q: %w", scratch, err)
	}
	return src, out, scratch, nil
}

// hcsshimContainer is one running per-tenant container under
// containerd. Under Hyper-V isolation the task is hpcc-agent.exe
// (PID 1 of the utility VM) and agentConn is the gRPC connection
// to that agent — Exec calls flow through it. Under process
// isolation the task is pause.exe and agentConn is nil; Exec calls
// dispatch as Task.Exec children of pause with host-side
// copyTree-based file staging through hostSrcDir / hostOutDir.
type hcsshimContainer struct {
	owner *Hcsshim
	spec  ContainerSpec

	container containerd.Container
	task      containerd.Task

	// agentConn is non-nil iff Isolation == hyperv. Its presence is
	// what Exec / Stop branch on.
	agentConn *grpc.ClientConn

	// hostSrcDir / hostOutDir / hostScratchDir are non-empty only
	// in process-isolation mode (Hyper-V mode streams files via the
	// agent and needs no host-disk staging).
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

// Exec runs one compiler invocation. Branched by whether the
// container was started with an agent gRPC connection:
//
//   - agentConn != nil (Hyper-V isolation): the host streams the
//     compile as one AgentService.Exec bidi RPC — header + every
//     file under req.SrcHostPath as InputFile chunks, then drains
//     stdio + result + OutputFile frames back. No per-Exec host
//     scratch dirs, no copyTree.
//   - agentConn == nil (process isolation): per-Exec staging dirs
//     under the container's src/out scratch roots, copyTree to
//     populate src, Task.Exec the translated argv, copyTree to
//     drain outputs back to req.OutHostPath.
//
// Both paths run with the same ExecRequest contract; only the
// transport differs.
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

	if c.agentConn != nil {
		return c.execViaAgentTransport(ctx, req)
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

// execViaAgentTransport drives a compile through the agent's gRPC
// channel instead of Task.Exec + host-side copyTree. argv is shipped
// as-is — the agent's in-VM translator (server.go) handles the
// /src → C:\hpcc\src\<ExecID> rewrite at runCompiler time, just like
// the host-side translator does in process-isolation mode.
func (c *hcsshimContainer) execViaAgentTransport(ctx context.Context, req ExecRequest) (ExecResult, error) {
	// Translate the platform-neutral /src and /out tokens in argv +
	// cwd to the agent's in-VM staging dirs BEFORE shipping them
	// across HvSocket. The agent runs each Exec under
	// stagingRoot/{src,out}/<ExecID> (C:\hpcc\{src,out}\<ExecID>
	// on Windows) and does no path rewriting itself — runCompiler
	// just sets cmd.Dir = hdr.Cwd verbatim. So "/out" reaches the
	// agent as a literal Windows path that doesn't exist, and
	// chdir fails with "system cannot find the file specified."
	// Mirrors firecracker.go's host-side translation against its
	// /run/hpcc/{src,out}/<id> staging.
	inSrc := guestAgentStagingSrc + `\` + req.ExecID
	inOut := guestAgentStagingOut + `\` + req.ExecID
	argv := translateArgs(req.Argv, inSrc, inOut)
	cwd := req.Cwd
	if cwd != "" {
		cwd = translateExecPath(cwd, inSrc, inOut)
	}

	result, err := execViaAgent(ctx, c.agentConn, AgentExecRequest{
		ExecID:      req.ExecID,
		Argv:        argv,
		Env:         req.Env,
		Cwd:         cwd,
		SrcHostPath: req.SrcHostPath,
		OutHostPath: req.OutHostPath,
		Stdout:      req.Stdout,
		Stderr:      req.Stderr,
	})
	if err != nil {
		return ExecResult{ExitCode: -1}, fmt.Errorf("hcsshim: agent exec: %w", err)
	}
	return ExecResult{ExitCode: int(result.ExitCode)}, nil
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
	if c.agentConn != nil {
		// Close before killing the task: the agent is the task's
		// PID 1, so a pending Exec call would error mid-stream
		// anyway when the agent exits — closing first surfaces a
		// clean "EOF" rather than a transport-level reset.
		if err := c.agentConn.Close(); err != nil {
			errs = append(errs, fmt.Errorf("agentConn.Close: %w", err))
		}
		c.agentConn = nil
	}
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

