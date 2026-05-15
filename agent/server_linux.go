//go:build linux

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/mdlayher/vsock"
	"google.golang.org/grpc"

	agentpb "github.com/aarani/hpcc/proto/agent"
)

// agentVsockPort is the AF_VSOCK port the agent listens on inside
// the guest. The runner dials this same port via Firecracker's UDS
// bridge. Pinned at build time — there's only ever one agent service
// per VM and there's no value in making it configurable.
const agentVsockPort = 17727

// stagingRoot is where per-Exec input/output trees live. Created as
// part of init bootstrap on a fresh /run tmpfs.
const stagingRoot = "/run/hpcc"

// outputChunkSize bounds one OutputFile.chunk frame. Picked to keep
// per-frame allocations in the same neighbourhood as gRPC's default
// 4 MiB frame cap with headroom for protobuf overhead — large enough
// that streaming a multi-MB .o is dominated by syscall reads, small
// enough that backpressure stays responsive.
const outputChunkSize = 256 * 1024

// serveAgent brings up the gRPC service on AF_VSOCK and blocks until
// the listener fails or the process exits. Errors are returned to
// main, which logs+exits — there's no graceful-restart story for an
// in-VM PID-1.
func serveAgent() error {
	lis, err := vsock.Listen(agentVsockPort, nil)
	if err != nil {
		return fmt.Errorf("vsock listen on %d: %w", agentVsockPort, err)
	}
	s := grpc.NewServer()
	agentpb.RegisterAgentServiceServer(s, &execServer{})
	return s.Serve(lis)
}

// execServer implements the bidi-streaming Exec RPC defined in
// proto/agent/agent.proto. One stream == one compiler invocation;
// concurrent Execs on the same VM run in independent goroutines and
// independent staging dirs.
type execServer struct {
	agentpb.UnimplementedAgentServiceServer
}

func (s *execServer) Exec(stream agentpb.AgentService_ExecServer) error {
	first, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("recv header: %w", err)
	}
	hdr := first.GetHeader()
	if hdr == nil {
		return errors.New("first frame must carry ExecHeader")
	}
	if hdr.ExecId == "" {
		return errors.New("ExecHeader.exec_id is required")
	}
	if len(hdr.Argv) == 0 {
		return errors.New("ExecHeader.argv must not be empty")
	}

	srcDir := filepath.Join(stagingRoot, "src", hdr.ExecId)
	outDir := filepath.Join(stagingRoot, "out", hdr.ExecId)
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		return fmt.Errorf("mkdir src staging: %w", err)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("mkdir out staging: %w", err)
	}
	// Cleanup on stream exit (both happy and error paths). Per-Exec
	// dirs live on tmpfs so the rm is cheap, but leaving them around
	// would leak guest RAM across repeated Execs in a long-lived VM.
	defer func() {
		_ = os.RemoveAll(srcDir)
		_ = os.RemoveAll(outDir)
	}()

	if err := stageInputs(stream, srcDir); err != nil {
		return fmt.Errorf("stage inputs: %w", err)
	}

	exitCode, err := runCompiler(stream, hdr, srcDir)
	if err != nil {
		return err
	}

	// Result frame goes BEFORE output files so the client knows the
	// exit code immediately; if it doesn't care about outputs (compile
	// failed, ctx cancelled), it can close the stream and skip
	// receiving the rest.
	if err := stream.Send(&agentpb.ExecServerFrame{
		Frame: &agentpb.ExecServerFrame_Result{
			Result: &agentpb.ExecResult{ExitCode: exitCode},
		},
	}); err != nil {
		return fmt.Errorf("send result: %w", err)
	}

	if err := streamOutputs(stream, outDir); err != nil {
		return fmt.Errorf("stream outputs: %w", err)
	}
	return nil
}

// stageInputs reads InputFile chunks until the client half-closes the
// stream and writes them under srcDir. Multiple files may interleave;
// we demultiplex by path. EOF on a chunk closes the file fd. Any file
// without an explicit eof gets closed implicitly when the stream ends
// (the client may legitimately rely on stream-close as the EOF
// signal).
func stageInputs(stream agentpb.AgentService_ExecServer, srcDir string) error {
	open := map[string]*os.File{}
	defer func() {
		for _, fh := range open {
			_ = fh.Close()
		}
	}()
	for {
		frame, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		in := frame.GetInput()
		if in == nil {
			return errors.New("expected InputFile frame after header")
		}
		if err := safeRel(in.Path); err != nil {
			return fmt.Errorf("input path %q: %w", in.Path, err)
		}
		fh, ok := open[in.Path]
		if !ok {
			full := filepath.Join(srcDir, in.Path)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return fmt.Errorf("mkdir parent of %q: %w", in.Path, err)
			}
			fh, err = os.OpenFile(full, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return fmt.Errorf("open %q: %w", in.Path, err)
			}
			open[in.Path] = fh
		}
		if len(in.Chunk) > 0 {
			if _, err := fh.Write(in.Chunk); err != nil {
				return fmt.Errorf("write %q: %w", in.Path, err)
			}
		}
		if in.Eof {
			if err := fh.Close(); err != nil {
				return fmt.Errorf("close %q: %w", in.Path, err)
			}
			delete(open, in.Path)
		}
	}
}

// runCompiler execve's the requested binary with stdio piped back
// over the gRPC stream as it arrives. Returns the exit code; an err
// means we couldn't even run the process (the typical "command not
// found" case). A non-zero exit code is returned as data.
func runCompiler(stream agentpb.AgentService_ExecServer, hdr *agentpb.ExecHeader, srcDir string) (int32, error) {
	ctx := stream.Context()
	cmd := exec.CommandContext(ctx, hdr.Argv[0], hdr.Argv[1:]...)
	cmd.Env = resolveEnv(hdr.Env)
	cmd.Dir = hdr.Cwd
	if cmd.Dir == "" {
		cmd.Dir = srcDir
	}
	cmd.Stdout = &stdioForwarder{stream: stream, kind: agentpb.StdioChunk_STDOUT}
	cmd.Stderr = &stdioForwarder{stream: stream, kind: agentpb.StdioChunk_STDERR}

	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return int32(ee.ExitCode()), nil
		}
		return -1, fmt.Errorf("exec %q: %w", hdr.Argv[0], err)
	}
	return 0, nil
}

// stdioForwarder turns each Write from the compiler into one
// StdioChunk frame. cmd.Stdout/Stderr writes are batched by the
// runtime, so each call here is typically a few KB — fine for a
// single proto message.
type stdioForwarder struct {
	stream agentpb.AgentService_ExecServer
	kind   agentpb.StdioChunk_Stream
}

func (f *stdioForwarder) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	err := f.stream.Send(&agentpb.ExecServerFrame{
		Frame: &agentpb.ExecServerFrame_Stdio{
			Stdio: &agentpb.StdioChunk{
				Stream: f.kind,
				// Copy: gRPC may queue the message, and the caller
				// (os/exec's reader) reuses p on the next Read.
				Bytes: append([]byte(nil), p...),
			},
		},
	})
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

// streamOutputs walks outDir and ships every regular file as one or
// more OutputFile frames. The runner reassembles them under its
// per-Exec out directory; matches the dangerous-runtime contract
// where the worker reads finished artifacts off disk after Exec.
//
// Symlinks/dirs/special files are skipped — compilers we care about
// only produce regular file outputs and shipping anything else would
// confuse the runner's "concatenate chunks into a file" logic.
func streamOutputs(stream agentpb.AgentService_ExecServer, outDir string) error {
	return filepath.WalkDir(outDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(outDir, path)
		if err != nil {
			return err
		}
		return streamOneOutput(stream, path, rel)
	})
}

func streamOneOutput(stream agentpb.AgentService_ExecServer, full, rel string) error {
	f, err := os.Open(full)
	if err != nil {
		return fmt.Errorf("open output %q: %w", rel, err)
	}
	defer f.Close()

	buf := make([]byte, outputChunkSize)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			if sendErr := stream.Send(&agentpb.ExecServerFrame{
				Frame: &agentpb.ExecServerFrame_Output{
					Output: &agentpb.OutputFile{
						Path:  rel,
						Chunk: append([]byte(nil), buf[:n]...),
					},
				},
			}); sendErr != nil {
				return sendErr
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read output %q: %w", rel, err)
		}
	}
	// Trailing eof frame so the client can close its destination fd
	// without waiting for the whole stream to end.
	return stream.Send(&agentpb.ExecServerFrame{
		Frame: &agentpb.ExecServerFrame_Output{
			Output: &agentpb.OutputFile{Path: rel, Eof: true},
		},
	})
}

// safeRel rejects paths that would escape the staging dir. The
// runner is trusted, but a path-traversal bug on the host shouldn't
// translate into the agent overwriting /etc/passwd inside the guest
// — even if the guest is single-tenant, the rootfs is RO and a
// confused write would be a noisy failure mode.
func safeRel(p string) error {
	if p == "" {
		return errors.New("path is empty")
	}
	if filepath.IsAbs(p) {
		return errors.New("path must be relative")
	}
	clean := filepath.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return errors.New("path escapes staging dir")
	}
	return nil
}

