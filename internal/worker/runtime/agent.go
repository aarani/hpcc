package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"google.golang.org/grpc"

	agentpb "github.com/aarani/hpcc/proto/agent"
)

// injectAgentTraceContext extracts the current trace context from ctx
// into the W3C `traceparent` / `tracestate` strings carried on
// ExecHeader. Both empty when the worker isn't exporting traces — the
// agent then opens a fresh root span (or noop, if its own SDK is
// unconfigured).
func injectAgentTraceContext(ctx context.Context) (traceparent, tracestate string) {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	return carrier["traceparent"], carrier["tracestate"]
}

// agentInputChunkSize bounds one ExecClientFrame.input.chunk. Matches
// the agent's output side (server.go: outputChunkSize) so a streamed
// file's frame size is the same in both directions. Picked to keep
// per-frame allocations well below gRPC's default 4 MiB cap with
// headroom for protobuf overhead.
const agentInputChunkSize = 256 * 1024

// AgentExecRequest is the host-side view of one compile dispatch
// against an in-container hpcc-agent. Same surface as ExecRequest
// but agent-shaped: paths are in-container (the agent demuxes them
// into its own staging dir) and the transport is the AgentService
// bidi stream rather than Task.Exec.
//
// Stdin is intentionally absent: AgentService.Exec doesn't carry a
// stdin stream today. None of the §4.5 dispatch paths need it
// (CAS-mode source ships as InputFile chunks; preprocessed-mode
// source rides in one InputFile too). If a future feature needs
// stdin we add a new oneof variant to ExecClientFrame rather than
// bolting it onto the host-side API.
type AgentExecRequest struct {
	ExecID string
	Argv   []string
	Env    []string
	Cwd    string

	// SrcHostPath, if non-empty, is the host directory whose tree
	// the helper streams across as InputFile frames. Files are
	// walked in filesystem order and shipped relative to this root.
	// Empty means "no inputs"; the header is still sent and the
	// stream is half-closed immediately after.
	SrcHostPath string

	// OutHostPath, if non-empty, is where OutputFile chunks the
	// agent streams back get written. Paths inside the OutputFile
	// frames are agent-relative (forward-slash); the helper joins
	// them under this root with native separators.
	OutHostPath string

	Stdout io.Writer
	Stderr io.Writer
}

// execViaAgent runs one compile against an already-dialled agent.
// Opens the AgentService.Exec bidi stream, sends header + every
// regular file under req.SrcHostPath as InputFile chunks,
// half-closes, then consumes stdio / result / output frames until
// the stream ends. Returns the agent's ExecResult or the first
// transport-level error.
//
// Streaming both directions: input files are read in chunks so peak
// memory is bounded by chunk size (not sum-of-sizes); stdio is
// forwarded to req.Stdout/Stderr as it arrives rather than buffered.
// Same shape as the in-VM agent (server.go) — host and guest mirror
// each other.
func execViaAgent(ctx context.Context, conn *grpc.ClientConn, req AgentExecRequest) (*agentpb.ExecResult, error) {
	if req.ExecID == "" {
		return nil, errors.New("agentExec: ExecID is required")
	}
	if len(req.Argv) == 0 {
		return nil, errors.New("agentExec: Argv must be non-empty")
	}

	client := agentpb.NewAgentServiceClient(conn)
	stream, err := client.Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("open Exec stream: %w", err)
	}

	tp, ts := injectAgentTraceContext(ctx)
	if err := stream.Send(&agentpb.ExecClientFrame{
		Frame: &agentpb.ExecClientFrame_Header{
			Header: &agentpb.ExecHeader{
				ExecId:      req.ExecID,
				Argv:        req.Argv,
				Env:         req.Env,
				Cwd:         req.Cwd,
				Traceparent: tp,
				Tracestate:  ts,
			},
		},
	}); err != nil {
		return nil, fmt.Errorf("send header: %w", err)
	}

	if req.SrcHostPath != "" {
		if err := streamInputsToAgent(stream, req.SrcHostPath); err != nil {
			return nil, fmt.Errorf("stream inputs: %w", err)
		}
	}
	if err := stream.CloseSend(); err != nil {
		return nil, fmt.Errorf("half-close: %w", err)
	}

	return drainAgentResponses(stream, req)
}

// streamInputsToAgent walks srcRoot and emits one or more InputFile
// frames per regular file. Paths in the frame are forward-slash
// relative to srcRoot; the agent's server-side FromSlash converts
// them back when writing. Symlinks/dirs/special files are skipped
// for the same reason streamOutputs on the agent side does — only
// regular files are shippable as concatenated chunks.
func streamInputsToAgent(stream agentpb.AgentService_ExecClient, srcRoot string) error {
	return filepath.WalkDir(srcRoot, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(srcRoot, path)
		if err != nil {
			return err
		}
		wireRel := filepath.ToSlash(rel)
		return streamOneInputToAgent(stream, path, wireRel)
	})
}

func streamOneInputToAgent(stream agentpb.AgentService_ExecClient, full, rel string) error {
	f, err := os.Open(full)
	if err != nil {
		return fmt.Errorf("open input %q: %w", rel, err)
	}
	defer f.Close()

	buf := make([]byte, agentInputChunkSize)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			if sendErr := stream.Send(&agentpb.ExecClientFrame{
				Frame: &agentpb.ExecClientFrame_Input{
					Input: &agentpb.InputFile{
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
			return fmt.Errorf("read input %q: %w", rel, err)
		}
	}
	return stream.Send(&agentpb.ExecClientFrame{
		Frame: &agentpb.ExecClientFrame_Input{
			Input: &agentpb.InputFile{Path: rel, Eof: true},
		},
	})
}

// drainAgentResponses consumes the agent's server-side frames until
// EOF, forwarding stdio to req.Stdout/Stderr, writing OutputFile
// chunks under req.OutHostPath, and returning the ExecResult.
//
// Frame ordering contract (server.go on the agent): stdio frames may
// interleave with anything before the result; ExecResult appears
// exactly once; OutputFile frames only after the result. The helper
// tolerates any order within that constraint and writes outputs
// streamingly so peak memory stays bounded.
func drainAgentResponses(stream agentpb.AgentService_ExecClient, req AgentExecRequest) (*agentpb.ExecResult, error) {
	var result *agentpb.ExecResult
	openOutputs := map[string]*os.File{}
	defer func() {
		for _, fh := range openOutputs {
			_ = fh.Close()
		}
	}()

	for {
		frame, err := stream.Recv()
		if err == io.EOF {
			return result, nil
		}
		if err != nil {
			return result, err
		}
		switch f := frame.Frame.(type) {
		case *agentpb.ExecServerFrame_Stdio:
			if err := writeStdio(req, f.Stdio); err != nil {
				return result, err
			}
		case *agentpb.ExecServerFrame_Result:
			result = f.Result
		case *agentpb.ExecServerFrame_Output:
			if err := writeOutput(openOutputs, req.OutHostPath, f.Output); err != nil {
				return result, err
			}
		default:
			return result, fmt.Errorf("agent sent unknown frame %T", frame.Frame)
		}
	}
}

func writeStdio(req AgentExecRequest, chunk *agentpb.StdioChunk) error {
	if len(chunk.Bytes) == 0 {
		return nil
	}
	w := req.Stdout
	if chunk.Stream == agentpb.StdioChunk_STDERR {
		w = req.Stderr
	}
	if w == nil {
		return nil
	}
	_, err := w.Write(chunk.Bytes)
	return err
}

func writeOutput(open map[string]*os.File, outRoot string, file *agentpb.OutputFile) error {
	if outRoot == "" {
		// No output dir configured: ignore output frames. Mirrors
		// the runtime's existing "missing OutHostPath = no capture"
		// contract.
		return nil
	}
	if file.Path == "" {
		return errors.New("agent output frame missing path")
	}
	fh, ok := open[file.Path]
	if !ok {
		full := filepath.Join(outRoot, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("mkdir parent of %q: %w", file.Path, err)
		}
		var err error
		fh, err = os.OpenFile(full, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return fmt.Errorf("create output %q: %w", file.Path, err)
		}
		open[file.Path] = fh
	}
	if len(file.Chunk) > 0 {
		if _, err := fh.Write(file.Chunk); err != nil {
			return fmt.Errorf("write output %q: %w", file.Path, err)
		}
	}
	if file.Eof {
		if err := fh.Close(); err != nil {
			return fmt.Errorf("close output %q: %w", file.Path, err)
		}
		delete(open, file.Path)
	}
	return nil
}
