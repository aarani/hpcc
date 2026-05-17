package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	agentpb "github.com/aarani/hpcc/proto/agent"
	"go.uber.org/zap"
)

// agentVsockPort is the numeric AF_VSOCK port the agent listens on.
// On Linux the vsock listener uses it as the AF_VSOCK port; on
// Windows the Hyper-V transport encodes it into a vsock-style service
// GUID via winio.VsockServiceID — same wire-port number both sides,
// so the host-side runner can target a single constant regardless of
// which guest OS is running. Pinned at build time; there's exactly
// one agent service per VM/container and no value in making it
// configurable.
const agentVsockPort = 17727

// outputChunkSize bounds one OutputFile.chunk frame. Picked to keep
// per-frame allocations in the same neighbourhood as gRPC's default
// 4 MiB frame cap with headroom for protobuf overhead — large enough
// that streaming a multi-MB .o is dominated by syscall reads, small
// enough that backpressure stays responsive.
const outputChunkSize = 256 * 1024

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
		securityEvent("agent-malformed-request",
			"agent Exec rejected: first frame missing ExecHeader",
			zap.String("rpc", "Exec"),
		)
		return errors.New("first frame must carry ExecHeader")
	}
	if hdr.ExecId == "" {
		securityEvent("agent-malformed-request",
			"agent Exec rejected: ExecHeader.exec_id is required",
			zap.String("rpc", "Exec"),
		)
		return errors.New("ExecHeader.exec_id is required")
	}
	if len(hdr.Argv) == 0 {
		securityEvent("agent-malformed-request",
			"agent Exec rejected: ExecHeader.argv must not be empty",
			zap.String("rpc", "Exec"),
			zap.String("exec_id", hdr.ExecId),
		)
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
	// dirs live on tmpfs (Linux) or the container's writable layer
	// (Windows) so the rm is cheap, but leaving them around would
	// leak guest resources across repeated Execs in a long-lived VM
	// / container.
	defer func() {
		_ = os.RemoveAll(srcDir)
		_ = os.RemoveAll(outDir)
	}()

	// Compilers refuse to create the parent directory of an output
	// file — they open(...O_CREAT) the leaf only. CAS-mode argv
	// frequently has -Wp,-MMD,<outDir>/build/main.d which needs
	// build/ to exist or clang errors with ENOENT. Scan argv for
	// substrings under outDir and mkdir each parent before invoking
	// the compiler. Compiler-agnostic — just substring search.
	if err := mkdirOutputParents(hdr.Argv, outDir); err != nil {
		return fmt.Errorf("mkdir output parents: %w", err)
	}

	if err := stageInputs(stream, srcDir); err != nil {
		return fmt.Errorf("stage inputs: %w", err)
	}

	// CAS only ships files in the dep closure, so a search-path
	// directory whose contents this TU doesn't pull in never gets
	// created during stageInputs. -Werror=missing-include-dirs
	// (kernel and many other builds enable it) and
	// -Werror=missing-library-dirs then fail before the
	// compile/link runs. Materialise each -I/-iquote/-isystem/
	// -idirafter/-L dir that resolves into srcDir; system paths
	// under /usr etc. are left alone since the rootfs provides them.
	if err := mkdirSearchPaths(hdr.Argv, srcDir); err != nil {
		return fmt.Errorf("mkdir search-path dirs: %w", err)
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
			securityEvent("agent-malformed-request",
				"agent stageInputs rejected: expected InputFile frame after header",
				zap.String("rpc", "Exec"),
			)
			return errors.New("expected InputFile frame after header")
		}
		if err := safeRel(in.Path); err != nil {
			securityEvent("agent-path-traversal",
				"agent stageInputs rejected: input path is absolute or escapes staging dir",
				zap.String("rpc", "Exec"),
				zap.String("path", in.Path),
				zap.Error(err),
			)
			return fmt.Errorf("input path %q: %w", in.Path, err)
		}
		fh, ok := open[in.Path]
		if !ok {
			full := filepath.Join(srcDir, filepath.FromSlash(in.Path))
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
		// Send paths with forward slashes on the wire regardless of
		// host OS; the receiver's filepath.FromSlash converts back
		// when materialising. Mirrors the BlobRef.Path convention on
		// the manifest side.
		return streamOneOutput(stream, path, filepath.ToSlash(rel))
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

// mkdirOutputParents walks argv for substrings under outDir and
// creates the parent directory of each. Compilers (gcc, clang) open
// output files with O_CREAT but don't mkdir missing parents — without
// this, a CAS-mode -Wp,-MMD,<outDir>/build/foo.d errors with ENOENT
// because build/ doesn't exist. Mirrors the host-side worker.go
// helper of the same name; duplicated rather than imported because
// the agent is a separate Go module.
//
// The substring scan handles every shape the compiler driver uses:
// positional (-o <path>), joined (-o<path>), and embedded inside
// another flag's value (-Wp,-MMD,<path>). Stops at the first
// path-impossible char (comma, whitespace, end-of-string) so a flag
// carrying multiple comma-separated values is segmented correctly.
func mkdirOutputParents(args []string, outDir string) error {
	prefix := outDir
	sep := string(filepath.Separator)
	if !strings.HasSuffix(prefix, sep) && !strings.HasSuffix(prefix, "/") {
		prefix += sep
	}
	for _, a := range args {
		rest := a
		for {
			idx := strings.Index(rest, prefix)
			if idx < 0 {
				break
			}
			tail := rest[idx+len(prefix):]
			end := strings.IndexAny(tail, ", \t")
			var rel string
			if end < 0 {
				rel = tail
				rest = ""
			} else {
				rel = tail[:end]
				rest = tail[end:]
			}
			if rel == "" {
				continue
			}
			parent := filepath.Dir(rel)
			if parent == "" || parent == "." {
				continue
			}
			full := filepath.Join(outDir, parent)
			if err := os.MkdirAll(full, 0o755); err != nil {
				return fmt.Errorf("mkdir %q: %w", full, err)
			}
		}
	}
	return nil
}

// searchPathFlags is the in-agent twin of worker.searchPathFlags —
// argv flags whose value is a directory the compiler/linker will
// search (gcc's -I/-iquote/-isystem/-idirafter header search and the
// linker's -L). Longer prefixes first so HasPrefix("-isystem", "-i")
// doesn't shadow them.
var searchPathFlags = []string{
	"-idirafter",
	"-isystem",
	"-iquote",
	"-I",
	"-L",
}

// mkdirSearchPaths walks argv for compiler/linker search-path flags
// (see searchPathFlags) and ensures each named directory exists
// under srcDir. Handles joined (-Idir) and separate (-I dir) forms.
//
// Two path shapes get materialised:
//
//   - Absolute paths under srcDir: stripped to the relative
//     component and created beneath srcDir. The runtime translates
//     /src/<rel> to <srcDir>/<rel> before forwarding the argv here.
//   - Relative paths (e.g. ./include/generated/uapi or include/foo):
//     created under srcDir as-is, since the compiler runs with
//     cwd = srcDir.
//
// Everything else (system paths like /usr/include/foo) is skipped —
// the rootfs provides those; creating them under srcDir would only
// confuse the header search.
//
// Mirrors worker.mkdirSearchPaths; duplicated rather than imported
// because the agent is a separate Go module.
//
// Idempotent: MkdirAll is a no-op on existing dirs.
func mkdirSearchPaths(args []string, srcDir string) error {
	if srcDir == "" {
		return nil
	}
	srcAbs := filepath.Clean(srcDir)
	srcPrefix := srcAbs + string(filepath.Separator)
	mkRel := func(rel string) error {
		if rel == "" || rel == "." {
			return nil
		}
		full := filepath.Join(srcAbs, filepath.FromSlash(rel))
		if err := os.MkdirAll(full, 0o755); err != nil {
			return fmt.Errorf("mkdir %q: %w", full, err)
		}
		return nil
	}
	mkOne := func(p string) error {
		if p == "" {
			return nil
		}
		if p == srcAbs {
			return nil
		}
		if strings.HasPrefix(p, srcPrefix) {
			return mkRel(p[len(srcPrefix):])
		}
		if filepath.IsAbs(p) {
			return nil
		}
		return mkRel(p)
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		for _, f := range searchPathFlags {
			if a == f {
				if i+1 < len(args) {
					if err := mkOne(args[i+1]); err != nil {
						return err
					}
					i++
				}
				break
			}
			if strings.HasPrefix(a, f) {
				if err := mkOne(a[len(f):]); err != nil {
					return err
				}
				break
			}
		}
	}
	return nil
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
