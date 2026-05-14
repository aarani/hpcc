package runtime

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	httptransport "github.com/go-openapi/runtime/client"
	"github.com/go-openapi/strfmt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	fcclient "github.com/aarani/hpcc/firecracker/client"
	"github.com/aarani/hpcc/firecracker/client/operations"
	"github.com/aarani/hpcc/firecracker/models"
	"github.com/aarani/hpcc/internal/protocol/gen"
	"github.com/aarani/hpcc/internal/worker/image/rootfs"
	agentpb "github.com/aarani/hpcc/proto/agent"
)

// HandlerFirecracker is the config.toml runtime.handler value that
// selects the raw Firecracker driver. Linux-only. See docs/plan.md §4.1
// for why hpcc drives Firecracker directly instead of going through
// firecracker-containerd.
const HandlerFirecracker = "firecracker"

// defaultBootArgs boots the prepared rootfs (sealed by the rootfs
// package) read-only with the in-VM hpcc-agent — staged at
// /.hpcc/agent by the rootfs builder — running as PID 1. `pci=off`
// matches Firecracker's recommended cmdline; serial console is left
// on ttyS0 so an operator can hook stdio for debugging. No explicit
// `rootfstype=` — the kernel auto-detects from the on-disk magic
// (see feedback_kernel_boot_args.md for why explicit pinning is
// fragile across deployments).
const defaultBootArgs = "console=ttyS0 reboot=k panic=1 pci=off init=/.hpcc/agent root=/dev/vda ro"

// socketWaitTimeout is the upper bound on how long Start blocks
// waiting for jailer → firecracker to come up far enough that the API
// socket accepts a connection. A correctly-installed firecracker boots
// in well under a second; the timeout exists so a misconfigured kernel
// or busted jailer doesn't wedge a Compile RPC indefinitely. The
// happy-path doesn't sit on this — waitForFirecrackerSocket also
// watches the process-exit channel and fails fast if jailer dies.
const socketWaitTimeout = 30 * time.Second

// Vsock wiring between host and agent. The agent listens on
// agentVsockPort inside the guest; the host reaches it via
// Firecracker's UDS bridge ("CONNECT <port>\n" handshake on a fresh
// UDS connection). agentVsockCID is per-VM as far as Firecracker is
// concerned — every microVM has its own CID namespace, so 3 (the
// conventional "first guest" value) is fine for all of them.
const (
	agentVsockPort   = 17727
	agentVsockCID    = int64(3)
	agentVsockUDS    = "/run/vsock.sock"
	agentDialTimeout = 10 * time.Second
)

// inputChunkSize bounds one InputFile.chunk frame the runner sends
// over the gRPC stream. Same neighbourhood as the agent's output
// chunk size, well under gRPC's default 4 MiB max-message ceiling.
const inputChunkSize = 256 * 1024

// FirecrackerOptions is the runtime's host-side configuration. All
// path fields are required; UID/GID are the non-root credentials
// jailer drops to before exec'ing firecracker. BootArgs is optional
// and falls back to defaultBootArgs.
type FirecrackerOptions struct {
	FirecrackerBin string
	JailerBin      string
	KernelImage    string
	RootfsDir      string
	RunDir         string
	UID, GID       int
	BootArgs       string
}

// Firecracker is the raw Firecracker Runtime. It owns the per-tenant
// VMM lifecycle (jailer + firecracker), wiring the prepared squashfs
// rootfs and hpcc-supplied vmlinux into a freshly-jailed chroot and
// driving the Firecracker API to the InstanceStart action.
//
// The host-side vsock agent and Exec dispatch are not wired here yet
// — Exec returns errFirecrackerExecNotImplemented. Boot is
// independently testable now so the rest of the worker plumbing
// (image pull, pool, runtime.Select) can be exercised end-to-end
// against a real microVM.
type Firecracker struct {
	opts FirecrackerOptions
}

// NewFirecracker validates required paths/credentials up front so a
// misconfigured worker fails at startup rather than on the first
// Compile RPC. All path fields and UID/GID must be set; BootArgs may
// be empty (defaultBootArgs is used).
func NewFirecracker(opts FirecrackerOptions) (*Firecracker, error) {
	switch {
	case opts.FirecrackerBin == "":
		return nil, fmt.Errorf("firecracker runtime: firecracker_bin is required")
	case opts.JailerBin == "":
		return nil, fmt.Errorf("firecracker runtime: jailer_bin is required")
	case opts.KernelImage == "":
		return nil, fmt.Errorf("firecracker runtime: kernel_image is required")
	case opts.RootfsDir == "":
		return nil, fmt.Errorf("firecracker runtime: rootfs_dir is required")
	case opts.RunDir == "":
		return nil, fmt.Errorf("firecracker runtime: run_dir is required")
	case opts.UID <= 0 || opts.GID <= 0:
		return nil, fmt.Errorf("firecracker runtime: uid/gid must be set to a non-root user (jailer requirement)")
	}
	if opts.BootArgs == "" {
		opts.BootArgs = defaultBootArgs
	}
	return &Firecracker{opts: opts}, nil
}

func (f *Firecracker) Start(ctx context.Context, spec ContainerSpec) (Container, error) {
	rootfsHost, err := (&rootfs.Store{CacheDir: f.opts.RootfsDir}).PathFor(spec.ImageDigest)
	if err != nil {
		return nil, fmt.Errorf("firecracker: resolve rootfs path: %w", err)
	}
	if _, err := os.Stat(rootfsHost); err != nil {
		return nil, fmt.Errorf("firecracker: prepared rootfs missing for %s: %w", spec.ImageDigest, err)
	}

	// Lay out the chroot ourselves rather than letting jailer create
	// an empty one. jailer accepts an existing dir and we need the
	// kernel + rootfs in place before firecracker tries to open them
	// during InstanceStart.
	chrootDir := filepath.Join(f.opts.RunDir, "firecracker", spec.ID)
	chrootRoot := filepath.Join(chrootDir, "root")
	if err := os.MkdirAll(chrootRoot, 0o755); err != nil {
		return nil, fmt.Errorf("firecracker: create chroot %q: %w", chrootRoot, err)
	}

	if err := stageIntoChroot(f.opts.KernelImage, filepath.Join(chrootRoot, "vmlinux"), f.opts.UID, f.opts.GID); err != nil {
		_ = os.RemoveAll(chrootDir)
		return nil, fmt.Errorf("firecracker: stage kernel: %w", err)
	}
	if err := stageIntoChroot(rootfsHost, filepath.Join(chrootRoot, "rootfs.sqsh"), f.opts.UID, f.opts.GID); err != nil {
		_ = os.RemoveAll(chrootDir)
		return nil, fmt.Errorf("firecracker: stage rootfs: %w", err)
	}

	// Jailer chroots firecracker into <run_dir>/firecracker/<id>/root
	// and exposes /run as a tmpfs underneath it. As of jailer 1.10+
	// jailer also unshares its mount namespace (CLONE_NEWNS), so
	// that tmpfs is invisible from the host's view of the same path
	// — the API socket and vsock UDS bridge live under /run from
	// firecracker's view, but `<chrootRoot>/run/...` on the host
	// shows an empty directory. The kernel's /proc/<pid>/root link
	// dereferences through the target process's mount namespace, so
	// `/proc/<firecracker-pid>/root/run/<thing>` is the host-side
	// path that actually reaches firecracker's tmpfs. We compute
	// those paths after cmd.Start().
	jailerArgs := []string{
		"--id", spec.ID,
		"--exec-file", f.opts.FirecrackerBin,
		"--uid", strconv.Itoa(f.opts.UID),
		"--gid", strconv.Itoa(f.opts.GID),
		"--chroot-base-dir", f.opts.RunDir,
		"--",
		"--api-sock", "/run/firecracker.socket",
	}
	cmd := exec.Command(f.opts.JailerBin, jailerArgs...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(chrootDir)
		return nil, fmt.Errorf("firecracker: start jailer %q: %w", f.opts.JailerBin, err)
	}
	// jailer exec's firecracker (no --daemonize, no fork dance), so
	// cmd.Process.Pid is firecracker's pid for the lifetime of the
	// VM. /proc/<pid>/root/* paths resolve through firecracker's
	// (un-shared) mount namespace.
	procRoot := fmt.Sprintf("/proc/%d/root", cmd.Process.Pid)
	socketPath := filepath.Join(procRoot, "run", "firecracker.socket")
	vsockHostUDS := filepath.Join(procRoot, agentVsockUDS)
	// Reap the jailer/firecracker process when it exits. Without this
	// goroutine the process sits as a zombie until container.Stop
	// runs Wait, which is fine for the happy path but leaks if a VM
	// dies on its own (kernel panic, OOM, etc.).
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()

	// teardown unwinds everything Start has accumulated when one of
	// the post-spawn API calls fails. Inlining this ten times made
	// the function unreadable; the cost is one closure per failure.
	teardown := func() {
		_ = cmd.Process.Kill()
		<-exited
		_ = os.RemoveAll(chrootDir)
	}

	if err := waitForFirecrackerSocket(ctx, socketPath, exited, socketWaitTimeout); err != nil {
		teardown()
		return nil, fmt.Errorf("firecracker: wait for api socket: %w", err)
	}

	fc := newFirecrackerAPIClient(socketPath)

	// Pre-boot API contract: machine config → boot source → drives →
	// vsock → InstanceStart. No network interface; compiles never
	// reach the network from inside the VM.
	memMib := spec.MemoryBytes / (1024 * 1024)
	if memMib < 1 {
		memMib = 1
	}
	vcpuCount := int64(spec.VCPUs)
	if _, err := fc.Operations.PutMachineConfiguration(operations.NewPutMachineConfigurationParamsWithContext(ctx).WithBody(&models.MachineConfiguration{
		VcpuCount:  &vcpuCount,
		MemSizeMib: &memMib,
	})); err != nil {
		teardown()
		return nil, fmt.Errorf("firecracker: put machine config: %w", err)
	}

	kernelGuestPath := "/vmlinux"
	if _, err := fc.Operations.PutGuestBootSource(operations.NewPutGuestBootSourceParamsWithContext(ctx).WithBody(&models.BootSource{
		KernelImagePath: &kernelGuestPath,
		BootArgs:        f.opts.BootArgs,
	})); err != nil {
		teardown()
		return nil, fmt.Errorf("firecracker: put boot source: %w", err)
	}

	driveID := "rootfs"
	isRoot := true
	if _, err := fc.Operations.PutGuestDriveByID(operations.NewPutGuestDriveByIDParamsWithContext(ctx).WithDriveID(driveID).WithBody(&models.Drive{
		DriveID:      &driveID,
		IsRootDevice: &isRoot,
		IsReadOnly:   true,
		PathOnHost:   "/rootfs.sqsh",
	})); err != nil {
		teardown()
		return nil, fmt.Errorf("firecracker: put rootfs drive: %w", err)
	}

	// Vsock device: one per VM. uds_path is relative to firecracker's
	// chroot view; the host sees the same socket at vsockHostUDS.
	guestCID := agentVsockCID
	udsPath := agentVsockUDS
	if _, err := fc.Operations.PutGuestVsock(operations.NewPutGuestVsockParamsWithContext(ctx).WithBody(&models.Vsock{
		GuestCid: &guestCID,
		UdsPath:  &udsPath,
	})); err != nil {
		teardown()
		return nil, fmt.Errorf("firecracker: put vsock: %w", err)
	}

	startAction := models.InstanceActionInfoActionTypeInstanceStart
	if _, err := fc.Operations.CreateSyncAction(operations.NewCreateSyncActionParamsWithContext(ctx).WithInfo(&models.InstanceActionInfo{
		ActionType: &startAction,
	})); err != nil {
		teardown()
		return nil, fmt.Errorf("firecracker: instance start: %w", err)
	}

	// Wait for the agent to be reachable before declaring the
	// container ready. gRPC's NewClient is lazy — it doesn't connect
	// until first RPC, so probing here is what turns "agent never
	// started" into a Start error rather than a confusing first-Exec
	// timeout.
	agentConn, err := dialAgent(ctx, vsockHostUDS, agentVsockPort, exited)
	if err != nil {
		teardown()
		return nil, fmt.Errorf("firecracker: dial agent: %w", err)
	}

	return &firecrackerContainer{
		spec:       spec,
		cmd:        cmd,
		exited:     exited,
		chrootDir:  chrootDir,
		fc:         fc,
		agentConn:  agentConn,
		agent:      agentpb.NewAgentServiceClient(agentConn),
	}, nil
}

func (f *Firecracker) Close() error { return nil }

// firecrackerContainer is the per-microVM handle. cmd is the jailer
// process (which exec'd into firecracker after setup); exited fires
// once Wait returns so Stop can join cleanly without leaking a zombie.
type firecrackerContainer struct {
	spec      ContainerSpec
	cmd       *exec.Cmd
	exited    chan struct{}
	chrootDir string
	fc        *fcclient.Firecracker

	// agentConn is the gRPC client connection over the vsock UDS
	// bridge; agent is the streaming client built off it. The same
	// connection is reused for every Exec on this container — gRPC
	// multiplexes streams over HTTP/2 so per-Exec dial overhead is
	// zero after the initial handshake in Start.
	agentConn *grpc.ClientConn
	agent     agentpb.AgentServiceClient

	mu      sync.Mutex
	stopped bool
}

func (c *firecrackerContainer) ID() string          { return c.spec.ID }
func (c *firecrackerContainer) TenantID() string    { return c.spec.TenantID }
func (c *firecrackerContainer) ImageDigest() string { return c.spec.ImageDigest }

// State always reports RUNNING once Start returned. There's no
// STOPPED enum and tracking a separate "booting" phase doesn't add
// value here — Start blocks until InstanceStart succeeds.
func (c *firecrackerContainer) State() gen.VMState { return gen.VMState_RUNNING }

// Exec dispatches one compiler invocation into the in-VM agent over
// vsock. The exchange is one bidi-streaming gRPC call:
//
//  1. Send ExecHeader with argv/env/cwd translated to the agent's
//     in-VM staging paths (/run/hpcc/src/<exec>/, /run/hpcc/out/<exec>/).
//  2. Stream every regular file under req.SrcHostPath as InputFile
//     chunks; close the send side.
//  3. Drain server frames: stdio chunks → req.Stdout/Stderr; the
//     ExecResult; OutputFile chunks → files under req.OutHostPath.
//
// A non-zero exit code is returned as data, not as err — only RPC
// failures (agent dead, ctx cancelled mid-stream) come back as err.
func (c *firecrackerContainer) Exec(ctx context.Context, req ExecRequest) (ExecResult, error) {
	if c.agent == nil {
		return ExecResult{ExitCode: -1}, fmt.Errorf("firecracker: agent client not initialized")
	}
	stream, err := c.agent.Exec(ctx)
	if err != nil {
		return ExecResult{ExitCode: -1}, fmt.Errorf("agent.Exec: %w", err)
	}

	inSrc := agentSrcDir(req.ExecID)
	inOut := agentOutDir(req.ExecID)
	argv := make([]string, len(req.Argv))
	for i, a := range req.Argv {
		argv[i] = translateExecPath(a, inSrc, inOut)
	}
	cwd := req.Cwd
	if cwd != "" {
		cwd = translateExecPath(cwd, inSrc, inOut)
	} else if req.SrcHostPath != "" {
		// No cwd specified: default to the staging dir so the
		// compiler resolves relative paths against the source tree
		// the runner just shipped.
		cwd = inSrc
	}

	if err := stream.Send(&agentpb.ExecClientFrame{
		Frame: &agentpb.ExecClientFrame_Header{
			Header: &agentpb.ExecHeader{
				ExecId: req.ExecID,
				Argv:   argv,
				Env:    req.Env,
				Cwd:    cwd,
			},
		},
	}); err != nil {
		return ExecResult{ExitCode: -1}, fmt.Errorf("send header: %w", err)
	}

	if req.SrcHostPath != "" {
		if err := streamInputDir(stream, req.SrcHostPath); err != nil {
			return ExecResult{ExitCode: -1}, fmt.Errorf("stream inputs: %w", err)
		}
	}
	if err := stream.CloseSend(); err != nil {
		return ExecResult{ExitCode: -1}, fmt.Errorf("close send: %w", err)
	}

	return drainExecStream(stream, req)
}

// Stop tears down the VM. SIGKILL on the jailer process is the
// quickest reliable shutdown — we don't have a guest-side init that
// honours SendCtrlAltDel yet, and the alternative (waiting on a guest
// agent that may not respond) would let a wedged VM hang the worker.
// Idempotent.
func (c *firecrackerContainer) Stop(_ context.Context) error {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return nil
	}
	c.stopped = true
	c.mu.Unlock()

	if c.agentConn != nil {
		_ = c.agentConn.Close()
	}
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	<-c.exited
	cleanupJailerMounts(c.chrootDir)
	_ = os.RemoveAll(c.chrootDir)
	return nil
}


// agentSrcDir / agentOutDir build the in-VM staging paths the agent
// uses for the given exec. Mirrored verbatim in agent/server_linux.go;
// keep both ends aligned.
func agentSrcDir(execID string) string { return "/run/hpcc/src/" + execID }
func agentOutDir(execID string) string { return "/run/hpcc/out/" + execID }

// streamInputDir walks srcDir and ships every regular file as a
// sequence of InputFile chunks. Path on the wire is relative to
// srcDir — agent reassembles under /run/hpcc/src/<exec>/<rel>.
func streamInputDir(stream agentpb.AgentService_ExecClient, srcDir string) error {
	return filepath.WalkDir(srcDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		return streamOneInput(stream, path, filepath.ToSlash(rel))
	})
}

func streamOneInput(stream agentpb.AgentService_ExecClient, full, rel string) error {
	f, err := os.Open(full)
	if err != nil {
		return fmt.Errorf("open input %q: %w", rel, err)
	}
	defer f.Close()

	buf := make([]byte, inputChunkSize)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			if err := stream.Send(&agentpb.ExecClientFrame{
				Frame: &agentpb.ExecClientFrame_Input{
					Input: &agentpb.InputFile{
						Path:  rel,
						Chunk: append([]byte(nil), buf[:n]...),
					},
				},
			}); err != nil {
				return err
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fmt.Errorf("read input %q: %w", rel, rerr)
		}
	}
	return stream.Send(&agentpb.ExecClientFrame{
		Frame: &agentpb.ExecClientFrame_Input{
			Input: &agentpb.InputFile{Path: rel, Eof: true},
		},
	})
}

// drainExecStream consumes the agent's response stream — stdio,
// then the result frame, then output file chunks — and writes
// stdio/outputs into the targets named on req. Returns the exit
// code from the result frame.
func drainExecStream(stream agentpb.AgentService_ExecClient, req ExecRequest) (ExecResult, error) {
	stdout, stderr := req.Stdout, req.Stderr
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}

	open := map[string]*os.File{}
	defer func() {
		for _, fh := range open {
			_ = fh.Close()
		}
	}()

	var exitCode int32 = -1
	gotResult := false

	for {
		frame, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return ExecResult{ExitCode: int(exitCode)}, fmt.Errorf("recv: %w", err)
		}
		switch f := frame.Frame.(type) {
		case *agentpb.ExecServerFrame_Stdio:
			switch f.Stdio.Stream {
			case agentpb.StdioChunk_STDOUT:
				if _, err := stdout.Write(f.Stdio.Bytes); err != nil {
					return ExecResult{ExitCode: int(exitCode)}, fmt.Errorf("stdout: %w", err)
				}
			case agentpb.StdioChunk_STDERR:
				if _, err := stderr.Write(f.Stdio.Bytes); err != nil {
					return ExecResult{ExitCode: int(exitCode)}, fmt.Errorf("stderr: %w", err)
				}
			}
		case *agentpb.ExecServerFrame_Result:
			exitCode = f.Result.ExitCode
			gotResult = true
		case *agentpb.ExecServerFrame_Output:
			if req.OutHostPath == "" {
				continue // nowhere to put it; agent shouldn't have shipped, but be defensive
			}
			if err := writeOutputChunk(req.OutHostPath, f.Output, open); err != nil {
				return ExecResult{ExitCode: int(exitCode)}, err
			}
		}
	}

	if !gotResult {
		return ExecResult{ExitCode: -1}, fmt.Errorf("agent stream closed without ExecResult")
	}
	return ExecResult{ExitCode: int(exitCode)}, nil
}

// writeOutputChunk appends one OutputFile chunk under outHost. Open
// fds are kept in `open` so multi-chunk files don't reopen on every
// frame. A path-traversal guard rejects anything escaping outHost —
// the agent is trusted, but a kernel CVE or corrupted frame
// shouldn't translate into the runner clobbering paths on the host.
func writeOutputChunk(outHost string, out *agentpb.OutputFile, open map[string]*os.File) error {
	if out.Path == "" || filepath.IsAbs(out.Path) {
		return fmt.Errorf("output path %q invalid", out.Path)
	}
	clean := filepath.Clean(out.Path)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("output path %q escapes /out", out.Path)
	}
	full := filepath.Join(outHost, clean)
	fh, ok := open[clean]
	if !ok {
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("mkdir parent of %q: %w", out.Path, err)
		}
		var err error
		fh, err = os.OpenFile(full, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return fmt.Errorf("open output %q: %w", out.Path, err)
		}
		open[clean] = fh
	}
	if len(out.Chunk) > 0 {
		if _, err := fh.Write(out.Chunk); err != nil {
			return fmt.Errorf("write %q: %w", out.Path, err)
		}
	}
	if out.Eof {
		if err := fh.Close(); err != nil {
			return fmt.Errorf("close %q: %w", out.Path, err)
		}
		delete(open, clean)
	}
	return nil
}

// stageIntoChroot makes src visible at dst inside the jailer chroot.
// Hard-link first (free, but only works on the same filesystem); copy
// is the fallback. dst is chowned to uid/gid so firecracker — running
// as that user after jailer drops privs — can open the file.
func stageIntoChroot(src, dst string, uid, gid int) error {
	// os.Link refuses to overwrite, and a leftover from a previous
	// run with the same VM ID would otherwise wedge us.
	_ = os.Remove(dst)
	if err := os.Link(src, dst); err != nil {
		if err := copyFile(src, dst); err != nil {
			return err
		}
	}
	if err := os.Chown(dst, uid, gid); err != nil {
		return fmt.Errorf("chown %q: %w", dst, err)
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %q: %w", src, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create %q: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy %q -> %q: %w", src, dst, err)
	}
	return out.Close()
}

// waitForFirecrackerSocket polls until the API socket accepts a
// connection, ctx expires, the deadline expires, or jailer exits.
// The last case is the important one — without it, a startup failure
// (bad cgroup setup, missing kernel, perms problem) blocks for the
// full timeout with a generic "timeout" error and no clue what
// actually broke. Catching the exit early surfaces "jailer died,
// scroll up for stderr" immediately.
func waitForFirecrackerSocket(ctx context.Context, path string, exited <-chan struct{}, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		select {
		case <-exited:
			return fmt.Errorf("jailer exited before api socket appeared at %q (check stderr above)", path)
		default:
		}
		conn, err := net.DialTimeout("unix", path, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout after %s waiting for %q", timeout, path)
		}
		select {
		case <-exited:
			return fmt.Errorf("jailer exited before api socket appeared at %q (check stderr above)", path)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// newFirecrackerAPIClient wires the generated swagger client to the
// VMM's unix-domain HTTP socket. The host/port given to httptransport
// are nominal — every request goes through DialContext to socketPath.
func newFirecrackerAPIClient(socketPath string) *fcclient.Firecracker {
	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
	}
	transport := httptransport.NewWithClient("localhost", "/", []string{"http"}, httpClient)
	return fcclient.New(transport, strfmt.Default)
}

// dialAgent opens a gRPC client to the in-VM agent over Firecracker's
// vsock UDS bridge. Steps:
//
//  1. Probe the bridge with retries until the agent's vsock listener
//     accepts a CONNECT (the agent only starts listening after its
//     own setupInit, so there's a real race window with InstanceStart).
//     If jailer/firecracker exits during probing — typical when the
//     guest panics on init failure — fail fast with a clear error.
//  2. Build a gRPC ClientConn whose dialer re-runs the CONNECT
//     handshake for every new HTTP/2 transport — gRPC may reconnect
//     on idle/error and each fresh connection needs the dance.
//
// Insecure transport credentials are correct here: the channel is a
// host↔guest pipe inside one VMM, no network exposure, so TLS would
// burn cycles for no security gain.
func dialAgent(ctx context.Context, udsPath string, port uint32, exited <-chan struct{}) (*grpc.ClientConn, error) {
	probeCtx, cancel := context.WithTimeout(ctx, agentDialTimeout)
	defer cancel()
	if err := waitForAgent(probeCtx, udsPath, port, exited); err != nil {
		return nil, err
	}

	dialer := func(dialCtx context.Context, _ string) (net.Conn, error) {
		return dialVsockUDS(dialCtx, udsPath, port)
	}
	return grpc.NewClient(
		"passthrough:///agent",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
}

// waitForAgent retries dialVsockUDS until the agent accepts, ctx
// expires, or jailer/firecracker exits. The retry cadence is 100ms
// because we expect first-success within a few hundred ms of
// InstanceStart and a tighter spin would just burn CPU on the host.
// Watching `exited` is what surfaces guest-init failures cleanly:
// when the agent panics on a missing /proc and the kernel hits
// init-died, firecracker exits, and we see "guest exited before
// agent answered" instead of an opaque vsock-dial timeout.
func waitForAgent(ctx context.Context, udsPath string, port uint32, exited <-chan struct{}) error {
	var lastErr error
	for {
		select {
		case <-exited:
			if lastErr != nil {
				return fmt.Errorf("guest exited before agent answered: %w (check stderr above)", lastErr)
			}
			return fmt.Errorf("guest exited before agent answered (check stderr above)")
		default:
		}
		if ctx.Err() != nil {
			if lastErr != nil {
				return fmt.Errorf("%w (last vsock error: %v)", ctx.Err(), lastErr)
			}
			return ctx.Err()
		}
		c, err := dialVsockUDS(ctx, udsPath, port)
		if err == nil {
			_ = c.Close()
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
		case <-exited:
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// dialVsockUDS opens a transparent byte stream to the guest's vsock
// port via Firecracker's UDS bridge. The bridge speaks a tiny
// line-based handshake: the host sends "CONNECT <port>\n" on a
// fresh UDS connection; firecracker either replies "OK <hostport>\n"
// — meaning "the guest accepted, the rest of this conn is your
// vsock pipe" — or returns an error line we surface verbatim.
func dialVsockUDS(ctx context.Context, udsPath string, port uint32) (net.Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", udsPath)
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	if _, err := fmt.Fprintf(c, "CONNECT %d\n", port); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("send CONNECT: %w", err)
	}
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("read CONNECT reply: %w", err)
	}
	if !strings.HasPrefix(line, "OK ") {
		_ = c.Close()
		return nil, fmt.Errorf("vsock connect rejected: %q", strings.TrimSpace(line))
	}
	// Clear the read deadline; the gRPC client owns lifecycle from here.
	_ = c.SetDeadline(time.Time{})
	return c, nil
}
