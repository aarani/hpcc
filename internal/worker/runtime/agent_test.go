package runtime

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	agentpb "github.com/aarani/hpcc/proto/agent"
)

// startTestAgent spins up an in-process gRPC server backed by the
// supplied handler over a bufconn listener. Returns a real
// *grpc.ClientConn the helper can drive end-to-end without an
// actual HvSocket / vsock transport on the host. Test fixtures use
// this to exercise the wire protocol of execViaAgent: no Hyper-V
// nesting required.
func startTestAgent(t *testing.T, handler agentpb.AgentServiceServer) *grpc.ClientConn {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := grpc.NewServer()
	agentpb.RegisterAgentServiceServer(server, handler)
	serveDone := make(chan struct{})
	go func() {
		_ = server.Serve(lis)
		close(serveDone)
	}()
	t.Cleanup(func() {
		server.Stop()
		<-serveDone
	})

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// recordingAgent captures the client→server half of an Exec stream
// for assertions, then ships a canned set of stdio + result + output
// frames back. Mirror image of what a real hpcc-agent would do
// without the actual compile.
type recordingAgent struct {
	agentpb.UnimplementedAgentServiceServer

	mu      sync.Mutex
	header  *agentpb.ExecHeader
	inputs  map[string][]byte // path -> body assembled from chunks

	// Canned responses
	stdoutChunks [][]byte
	stderrChunks [][]byte
	exitCode     int32
	outputs      map[string][]byte // path -> body to ship back

	// failOnRecvAfter abandons the stream after this many client
	// frames have been received. -1 (default) means never.
	failOnRecvAfter int
}

func (a *recordingAgent) Exec(stream agentpb.AgentService_ExecServer) error {
	a.mu.Lock()
	a.inputs = map[string][]byte{}
	failAfter := a.failOnRecvAfter
	a.mu.Unlock()

	recvd := 0
	for {
		frame, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		recvd++
		if failAfter >= 0 && recvd > failAfter {
			return errors.New("recordingAgent: synthetic failure")
		}
		switch f := frame.Frame.(type) {
		case *agentpb.ExecClientFrame_Header:
			a.mu.Lock()
			a.header = f.Header
			a.mu.Unlock()
		case *agentpb.ExecClientFrame_Input:
			if err := a.handleInput(f.Input); err != nil {
				return err
			}
		default:
			return errors.New("recordingAgent: unknown client frame")
		}
	}

	for _, chunk := range a.stdoutChunks {
		if err := stream.Send(&agentpb.ExecServerFrame{
			Frame: &agentpb.ExecServerFrame_Stdio{
				Stdio: &agentpb.StdioChunk{Stream: agentpb.StdioChunk_STDOUT, Bytes: chunk},
			},
		}); err != nil {
			return err
		}
	}
	for _, chunk := range a.stderrChunks {
		if err := stream.Send(&agentpb.ExecServerFrame{
			Frame: &agentpb.ExecServerFrame_Stdio{
				Stdio: &agentpb.StdioChunk{Stream: agentpb.StdioChunk_STDERR, Bytes: chunk},
			},
		}); err != nil {
			return err
		}
	}
	if err := stream.Send(&agentpb.ExecServerFrame{
		Frame: &agentpb.ExecServerFrame_Result{
			Result: &agentpb.ExecResult{ExitCode: a.exitCode},
		},
	}); err != nil {
		return err
	}
	// Deterministic order for output frames so test assertions can
	// rely on a stable sequence.
	paths := make([]string, 0, len(a.outputs))
	for p := range a.outputs {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		body := a.outputs[p]
		if err := stream.Send(&agentpb.ExecServerFrame{
			Frame: &agentpb.ExecServerFrame_Output{
				Output: &agentpb.OutputFile{Path: p, Chunk: body},
			},
		}); err != nil {
			return err
		}
		if err := stream.Send(&agentpb.ExecServerFrame{
			Frame: &agentpb.ExecServerFrame_Output{
				Output: &agentpb.OutputFile{Path: p, Eof: true},
			},
		}); err != nil {
			return err
		}
	}
	return nil
}

func (a *recordingAgent) handleInput(in *agentpb.InputFile) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.inputs == nil {
		a.inputs = map[string][]byte{}
	}
	a.inputs[in.Path] = append(a.inputs[in.Path], in.Chunk...)
	return nil
}

func TestExecViaAgent_RoundTripHeaderInputsOutputsStdio(t *testing.T) {
	src := t.TempDir()
	// Two regular files plus one subdir to confirm walk + relative-path
	// handling. Sizes deliberately under one chunk so a single frame
	// per file is enough; the larger-than-chunk case is covered in
	// the streaming test below.
	mustWrite(t, filepath.Join(src, "a.txt"), []byte("alpha"))
	mustWrite(t, filepath.Join(src, "sub", "b.h"), []byte("bravo"))

	agent := &recordingAgent{
		failOnRecvAfter: -1,
		stdoutChunks:    [][]byte{[]byte("hello\n")},
		stderrChunks:    [][]byte{[]byte("warn\n")},
		exitCode:        0,
		outputs:         map[string][]byte{"main.o": []byte("obj-bytes")},
	}
	conn := startTestAgent(t, agent)

	out := t.TempDir()
	var stdout, stderr bytes.Buffer
	res, err := execViaAgent(context.Background(), conn, AgentExecRequest{
		ExecID:      "e1",
		Argv:        []string{"gcc", "-c", "a.txt"},
		Env:         []string{"FOO=BAR"},
		Cwd:         "/work",
		SrcHostPath: src,
		OutHostPath: out,
		Stdout:      &stdout,
		Stderr:      &stderr,
	})
	if err != nil {
		t.Fatalf("execViaAgent: %v", err)
	}
	if res == nil || res.ExitCode != 0 {
		t.Fatalf("exit = %v, want 0", res)
	}

	// Header round-tripped intact.
	agent.mu.Lock()
	hdr := agent.header
	gotInputs := agent.inputs
	agent.mu.Unlock()
	if hdr == nil {
		t.Fatal("agent received no header")
	}
	if hdr.ExecId != "e1" || hdr.Cwd != "/work" {
		t.Errorf("header mismatched: %+v", hdr)
	}
	if got := strings.Join(hdr.Argv, " "); got != "gcc -c a.txt" {
		t.Errorf("argv = %q, want %q", got, "gcc -c a.txt")
	}
	if got := strings.Join(hdr.Env, ","); got != "FOO=BAR" {
		t.Errorf("env = %q, want FOO=BAR", got)
	}

	// Inputs arrived under forward-slash paths regardless of host OS.
	wantInputs := map[string]string{
		"a.txt":     "alpha",
		"sub/b.h":   "bravo",
	}
	for path, body := range wantInputs {
		got, ok := gotInputs[path]
		if !ok {
			t.Errorf("agent missing input %q", path)
			continue
		}
		if string(got) != body {
			t.Errorf("input %q body = %q, want %q", path, got, body)
		}
	}
	if len(gotInputs) != len(wantInputs) {
		t.Errorf("unexpected inputs received: %v", gotInputs)
	}

	// Stdio forwarded to caller writers.
	if stdout.String() != "hello\n" {
		t.Errorf("stdout = %q, want %q", stdout.String(), "hello\n")
	}
	if stderr.String() != "warn\n" {
		t.Errorf("stderr = %q, want %q", stderr.String(), "warn\n")
	}

	// Output materialized under OutHostPath with native separators.
	got, err := os.ReadFile(filepath.Join(out, "main.o"))
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if string(got) != "obj-bytes" {
		t.Errorf("output bytes = %q, want %q", got, "obj-bytes")
	}
}

func TestExecViaAgent_LargeInputSplitsAcrossChunks(t *testing.T) {
	src := t.TempDir()
	big := bytes.Repeat([]byte{'x'}, agentInputChunkSize*2+512)
	mustWrite(t, filepath.Join(src, "huge.bin"), big)

	agent := &recordingAgent{failOnRecvAfter: -1, exitCode: 0}
	conn := startTestAgent(t, agent)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := execViaAgent(ctx, conn, AgentExecRequest{
		ExecID:      "e1",
		Argv:        []string{"true"},
		SrcHostPath: src,
	})
	if err != nil {
		t.Fatalf("execViaAgent: %v", err)
	}
	agent.mu.Lock()
	got := agent.inputs["huge.bin"]
	agent.mu.Unlock()
	if len(got) != len(big) {
		t.Fatalf("reassembled input length = %d, want %d", len(got), len(big))
	}
	if !bytes.Equal(got, big) {
		t.Errorf("reassembled input bytes differ from source")
	}
}

func TestExecViaAgent_NonZeroExitPropagated(t *testing.T) {
	agent := &recordingAgent{
		failOnRecvAfter: -1,
		exitCode:        42,
		stderrChunks:    [][]byte{[]byte("the compile failed")},
	}
	conn := startTestAgent(t, agent)

	var stderr bytes.Buffer
	res, err := execViaAgent(context.Background(), conn, AgentExecRequest{
		ExecID: "e1",
		Argv:   []string{"gcc"},
		Stderr: &stderr,
	})
	if err != nil {
		t.Fatalf("execViaAgent: %v", err)
	}
	if res.ExitCode != 42 {
		t.Errorf("exit = %d, want 42", res.ExitCode)
	}
	if !strings.Contains(stderr.String(), "the compile failed") {
		t.Errorf("stderr = %q, expected to contain compile failure message", stderr.String())
	}
}

func TestExecViaAgent_RequiresExecIDAndArgv(t *testing.T) {
	agent := &recordingAgent{failOnRecvAfter: -1}
	conn := startTestAgent(t, agent)

	if _, err := execViaAgent(context.Background(), conn, AgentExecRequest{Argv: []string{"x"}}); err == nil {
		t.Error("expected error for missing ExecID")
	}
	if _, err := execViaAgent(context.Background(), conn, AgentExecRequest{ExecID: "e1"}); err == nil {
		t.Error("expected error for empty Argv")
	}
}

func mustWrite(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}
