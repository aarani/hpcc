package daemon

import (
	"encoding/binary"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aarani/hpcc/internal/cache"
	"github.com/aarani/hpcc/internal/config"
	"github.com/aarani/hpcc/internal/cache/store"
	"github.com/aarani/hpcc/internal/compiler"
	"github.com/aarani/hpcc/internal/enum"
	"github.com/aarani/hpcc/internal/protocol/gen"
	"google.golang.org/protobuf/proto"
)

func clangAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("clang"); err != nil {
		t.Skip("clang not found in PATH")
	}
}

func setupTestContext(t *testing.T) *compiler.Context {
	t.Helper()
	cacheDir := filepath.Join(t.TempDir(), "cache")
	ds, err := store.NewDiskCacheStore(cacheDir, "100M")
	if err != nil {
		t.Fatal(err)
	}
	c, err := compiler.Detect("clang")
	if err != nil {
		t.Fatal(err)
	}
	ctx := &compiler.Context{
		Compiler: c,
		Config:   &config.Config{PreprocessingMode: enum.PreprocessLocal},
	}
	ctx.Cache = cache.NewCompileCache(ctx, []store.Store{ds})
	return ctx
}

func startTestDaemon(t *testing.T, d *DefaultDaemon) *net.TCPListener {
	t.Helper()
	a, err := net.ResolveTCPAddr("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.ListenTCP("tcp", a)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })

	go func() {
		for {
			conn, err := l.AcceptTCP()
			if err != nil {
				return
			}
			go func() {
				_ = d.handleConnection(conn)
			}()
		}
	}()

	return l
}

func dialDaemon(t *testing.T, l *net.TCPListener) *net.TCPConn {
	t.Helper()
	addr := l.Addr().(*net.TCPAddr)
	tcpAddr, err := net.ResolveTCPAddr("tcp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTCP("tcp", nil, tcpAddr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func sendCompileRequest(t *testing.T, conn *net.TCPConn, req *gen.CompileRequest) {
	t.Helper()
	data, err := proto.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	lengthBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(lengthBytes, uint32(len(data)))
	if _, err := conn.Write(lengthBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(data); err != nil {
		t.Fatal(err)
	}
}

func readCompileResponse(t *testing.T, conn *net.TCPConn) *gen.CompileResponse {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	lengthBytes := make([]byte, 4)
	if _, err := io.ReadFull(conn, lengthBytes); err != nil {
		t.Fatalf("read response length: %v", err)
	}
	length := binary.BigEndian.Uint32(lengthBytes)
	data := make([]byte, length)
	if _, err := io.ReadFull(conn, data); err != nil {
		t.Fatalf("read response body: %v", err)
	}
	resp := &gen.CompileResponse{}
	if err := proto.Unmarshal(data, resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	return resp
}

func writeSource(t *testing.T, dir, name, src string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDaemonCompileSuccess(t *testing.T) {
	clangAvailable(t)
	ctx := setupTestContext(t)
	d := NewDefaultDaemon()
	d.Contexts.Store("clang", ctx)

	l := startTestDaemon(t, d)
	conn := dialDaemon(t, l)

	dir := t.TempDir()
	src := writeSource(t, dir, "hello.c", "int main(void) { return 0; }\n")
	out := filepath.Join(dir, "hello.o")

	sendCompileRequest(t, conn, &gen.CompileRequest{
		Args: []string{"clang", "-c", src, "-o", out},
	})

	resp := readCompileResponse(t, conn)
	if resp.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d: %s", resp.ExitCode, resp.Stderr)
	}

	if _, err := os.Stat(out); err != nil {
		t.Fatalf("output file not written: %v", err)
	}
}

func TestDaemonCacheHit(t *testing.T) {
	clangAvailable(t)
	ctx := setupTestContext(t)
	d := NewDefaultDaemon()
	d.Contexts.Store("clang", ctx)

	l := startTestDaemon(t, d)
	conn := dialDaemon(t, l)

	dir := t.TempDir()
	src := writeSource(t, dir, "cached.c", "int cached(void) { return 42; }\n")
	out := filepath.Join(dir, "cached.o")

	req := &gen.CompileRequest{
		Args: []string{"clang", "-c", src, "-o", out},
	}

	sendCompileRequest(t, conn, req)
	resp1 := readCompileResponse(t, conn)
	if resp1.ExitCode != 0 {
		t.Fatalf("first compile failed: %s", resp1.Stderr)
	}

	os.Remove(out)

	sendCompileRequest(t, conn, req)
	resp2 := readCompileResponse(t, conn)
	if resp2.ExitCode != 0 {
		t.Fatalf("cached compile failed: %s", resp2.Stderr)
	}

	if _, err := os.Stat(out); err != nil {
		t.Fatalf("output file not restored from cache: %v", err)
	}
}

func TestDaemonCompileError(t *testing.T) {
	clangAvailable(t)
	ctx := setupTestContext(t)
	d := NewDefaultDaemon()
	d.Contexts.Store("clang", ctx)

	l := startTestDaemon(t, d)
	conn := dialDaemon(t, l)

	dir := t.TempDir()
	src := writeSource(t, dir, "bad.c", "this is not valid C\n")
	out := filepath.Join(dir, "bad.o")

	sendCompileRequest(t, conn, &gen.CompileRequest{
		Args: []string{"clang", "-c", src, "-o", out},
	})

	resp := readCompileResponse(t, conn)
	if resp.ExitCode == 0 {
		t.Fatal("expected non-zero exit code for invalid source")
	}
	if len(resp.Stderr) == 0 {
		t.Fatal("expected stderr output for compile error")
	}
}

func TestDaemonMultipleRequestsSameConnection(t *testing.T) {
	clangAvailable(t)
	ctx := setupTestContext(t)
	d := NewDefaultDaemon()
	d.Contexts.Store("clang", ctx)

	l := startTestDaemon(t, d)
	conn := dialDaemon(t, l)

	dir := t.TempDir()

	for i := range 5 {
		name := filepath.Join(dir, "multi_"+string(rune('a'+i))+".c")
		writeSource(t, dir, filepath.Base(name), "int f(void) { return 0; }\n")
		out := name + ".o"

		sendCompileRequest(t, conn, &gen.CompileRequest{
			Args: []string{"clang", "-c", name, "-o", out},
		})

		resp := readCompileResponse(t, conn)
		if resp.ExitCode != 0 {
			t.Fatalf("request %d failed: %s", i, resp.Stderr)
		}
	}
}

func TestDaemonConcurrentConnections(t *testing.T) {
	clangAvailable(t)
	ctx := setupTestContext(t)
	d := NewDefaultDaemon()
	d.Contexts.Store("clang", ctx)

	l := startTestDaemon(t, d)

	dir := t.TempDir()
	const n = 5
	var wg sync.WaitGroup

	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn := dialDaemon(t, l)
			name := filepath.Join(dir, "conc_"+string(rune('a'+i))+".c")
			writeSource(t, dir, filepath.Base(name), "int f(void) { return 0; }\n")
			out := name + ".o"

			sendCompileRequest(t, conn, &gen.CompileRequest{
				Args: []string{"clang", "-c", name, "-o", out},
			})
			resp := readCompileResponse(t, conn)
			if resp.ExitCode != 0 {
				t.Errorf("concurrent request %d failed: %s", i, resp.Stderr)
			}
		}()
	}

	wg.Wait()
}

func TestDaemonHpccPrefixStripped(t *testing.T) {
	clangAvailable(t)
	ctx := setupTestContext(t)
	d := NewDefaultDaemon()
	d.Contexts.Store("clang", ctx)

	l := startTestDaemon(t, d)
	conn := dialDaemon(t, l)

	dir := t.TempDir()
	src := writeSource(t, dir, "strip.c", "int strip(void) { return 0; }\n")
	out := filepath.Join(dir, "strip.o")

	sendCompileRequest(t, conn, &gen.CompileRequest{
		Args: []string{"hpcc", "clang", "-c", src, "-o", out},
	})

	resp := readCompileResponse(t, conn)
	if resp.ExitCode != 0 {
		t.Fatalf("expected exit 0 with hpcc prefix, got %d: %s", resp.ExitCode, resp.Stderr)
	}
}

func TestDaemonPreservesStderr(t *testing.T) {
	clangAvailable(t)
	ctx := setupTestContext(t)
	d := NewDefaultDaemon()
	d.Contexts.Store("clang", ctx)

	l := startTestDaemon(t, d)
	conn := dialDaemon(t, l)

	dir := t.TempDir()
	src := writeSource(t, dir, "warn.c",
		"#include <stdio.h>\nint main(void) { int x; return x; }\n")
	out := filepath.Join(dir, "warn.o")

	sendCompileRequest(t, conn, &gen.CompileRequest{
		Args: []string{"clang", "-c", "-Wall", src, "-o", out},
	})

	resp := readCompileResponse(t, conn)
	if len(resp.Stderr) == 0 {
		t.Fatal("expected warnings in stderr")
	}
}

func TestDaemonInvalidProtobuf(t *testing.T) {
	d := NewDefaultDaemon()
	l := startTestDaemon(t, d)
	conn := dialDaemon(t, l)

	garbage := []byte{0xff, 0xfe, 0xfd, 0xfc, 0xfb}
	lengthBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(lengthBytes, uint32(len(garbage)))
	conn.Write(lengthBytes)
	conn.Write(garbage)

	resp := readCompileResponse(t, conn)
	if resp.ExitCode == 0 {
		t.Fatal("expected error response for invalid protobuf")
	}
}

func TestDaemonCwdResolvesRelativePaths(t *testing.T) {
	clangAvailable(t)
	ctx := setupTestContext(t)
	d := NewDefaultDaemon()
	d.Contexts.Store("clang", ctx)

	l := startTestDaemon(t, d)
	conn := dialDaemon(t, l)

	dir := t.TempDir()
	writeSource(t, dir, "rel.c", "int rel(void) { return 0; }\n")

	sendCompileRequest(t, conn, &gen.CompileRequest{
		Cwd:  dir,
		Args: []string{"clang", "-c", "rel.c", "-o", "rel.o"},
	})

	resp := readCompileResponse(t, conn)
	if resp.ExitCode != 0 {
		t.Fatalf("expected exit 0 with cwd-relative paths, got %d: %s", resp.ExitCode, resp.Stderr)
	}

	out := filepath.Join(dir, "rel.o")
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("output file not written at cwd-relative path: %v", err)
	}
}

// TestDaemonAbsolutePathsRespectedFromAnyCwd checks that when args
// are fully absolute, the daemon-spawned compile still succeeds —
// the client's cwd is now load-bearing (the daemon chdirs there so
// joined-form -Iinclude / auto-derived depfiles resolve correctly),
// so use a real cwd here. Path-flag resolution is exercised by
// TestDaemonCwdResolvesRelativePaths and TestResolveRelativePaths_*.
func TestDaemonAbsolutePathsRespectedFromAnyCwd(t *testing.T) {
	clangAvailable(t)
	ctx := setupTestContext(t)
	d := NewDefaultDaemon()
	d.Contexts.Store("clang", ctx)

	l := startTestDaemon(t, d)
	conn := dialDaemon(t, l)

	dir := t.TempDir()
	src := writeSource(t, dir, "abs.c", "int absolute(void) { return 0; }\n")
	out := filepath.Join(dir, "abs.o")

	// A different real directory: the daemon chdirs here, but every
	// path in argv is absolute so it doesn't matter for resolution.
	cwd := t.TempDir()

	sendCompileRequest(t, conn, &gen.CompileRequest{
		Cwd:  cwd,
		Args: []string{"clang", "-c", src, "-o", out},
	})

	resp := readCompileResponse(t, conn)
	if resp.ExitCode != 0 {
		t.Fatalf("absolute paths under non-source cwd should still compile, got %d: %s", resp.ExitCode, resp.Stderr)
	}
}

// TestResolveRelativePaths_dontMangleNonPathValues pins the rule
// that separate-form value flags (like `-x lang`) don't get their
// value run through filepath.Join. The Linux kernel's
// scripts/as-version.sh probes the assembler with
// `gcc -Wa,--version -c -x assembler-with-cpp /dev/null -o /dev/null`;
// before the nonPathValueFlags carve-out the daemon would rewrite
// that to `-x /cwd/assembler-with-cpp` and gcc would silently fall
// back to extension-based language detection, losing the probe.
func TestResolveRelativePaths_dontMangleNonPathValues(t *testing.T) {
	cwd := "/build"
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "-x language value stays verbatim",
			args: []string{"-c", "-x", "assembler-with-cpp", "/dev/null", "-o", "/dev/null"},
			want: []string{"-c", "-x", "assembler-with-cpp", "/dev/null", "-o", "/dev/null"},
		},
		{
			name: "-x with relative source still resolves source",
			args: []string{"-c", "-x", "c", "foo.c", "-o", "foo.o"},
			want: []string{"-c", "-x", "c", "/build/foo.c", "-o", "/build/foo.o"},
		},
		{
			name: "-target value stays verbatim",
			args: []string{"-target", "x86_64-linux-gnu", "-c", "foo.c"},
			want: []string{"-target", "x86_64-linux-gnu", "-c", "/build/foo.c"},
		},
		{
			name: "ordinary -o path still resolves",
			args: []string{"-c", "foo.c", "-o", "build/foo.o"},
			want: []string{"-c", "/build/foo.c", "-o", "/build/build/foo.o"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveRelativePaths(tc.args, cwd)
			if len(got) != len(tc.want) {
				t.Fatalf("length mismatch: got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("arg %d: got %q, want %q (full: %v)", i, got[i], tc.want[i], got)
				}
			}
		})
	}
}

func TestDaemonGracefulDisconnect(t *testing.T) {
	d := NewDefaultDaemon()
	l := startTestDaemon(t, d)
	conn := dialDaemon(t, l)

	lengthBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(lengthBytes, 0)
	if _, err := conn.Write(lengthBytes); err != nil {
		t.Fatal(err)
	}

	time.Sleep(50 * time.Millisecond)
}

// countingCompiler wraps a real Compiler and counts Invoke calls.
// An optional delay is injected into Invoke so concurrent requests
// overlap long enough for singleflight to coalesce them.
type countingCompiler struct {
	compiler.Compiler
	invokeCount atomic.Int32
	invokeDelay time.Duration
}

func (c *countingCompiler) Invoke(inv *compiler.Invocation) (*compiler.InvocationResult, error) {
	c.invokeCount.Add(1)
	if c.invokeDelay > 0 {
		time.Sleep(c.invokeDelay)
	}
	return c.Compiler.Invoke(inv)
}

func setupCountingContext(t *testing.T, delay time.Duration) (*compiler.Context, *countingCompiler) {
	t.Helper()
	cacheDir := filepath.Join(t.TempDir(), "cache")
	ds, err := store.NewDiskCacheStore(cacheDir, "100M")
	if err != nil {
		t.Fatal(err)
	}
	real, err := compiler.Detect("clang")
	if err != nil {
		t.Fatal(err)
	}
	cc := &countingCompiler{Compiler: real, invokeDelay: delay}
	ctx := &compiler.Context{
		Compiler: cc,
		Config:   &config.Config{PreprocessingMode: enum.PreprocessLocal},
	}
	ctx.Cache = cache.NewCompileCache(ctx, []store.Store{ds})
	return ctx, cc
}

func TestDaemonDedupIdenticalRequests(t *testing.T) {
	clangAvailable(t)
	ctx, cc := setupCountingContext(t, 200*time.Millisecond)
	d := NewDefaultDaemon()
	d.Contexts.Store("clang", ctx)

	l := startTestDaemon(t, d)

	dir := t.TempDir()
	src := writeSource(t, dir, "dedup.c", "int dedup(void) { return 0; }\n")
	out := filepath.Join(dir, "dedup.o")

	req := &gen.CompileRequest{
		Args: []string{"clang", "-c", src, "-o", out},
	}

	const n = 3
	resps := make([]*gen.CompileResponse, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn := dialDaemon(t, l)
			sendCompileRequest(t, conn, req)
			resps[i] = readCompileResponse(t, conn)
		}()
	}
	wg.Wait()

	for i, resp := range resps {
		if resp.ExitCode != 0 {
			t.Errorf("request %d: expected exit 0, got %d: %s", i, resp.ExitCode, resp.Stderr)
		}
	}

	if got := cc.invokeCount.Load(); got != 1 {
		t.Errorf("expected 1 compiler invocation (dedup), got %d", got)
	}
}

func TestDaemonDedupDifferentSourcesNotCoalesced(t *testing.T) {
	clangAvailable(t)
	ctx, cc := setupCountingContext(t, 200*time.Millisecond)
	d := NewDefaultDaemon()
	d.Contexts.Store("clang", ctx)

	l := startTestDaemon(t, d)
	dir := t.TempDir()

	srcA := writeSource(t, dir, "a.c", "int a(void) { return 1; }\n")
	outA := filepath.Join(dir, "a.o")
	srcB := writeSource(t, dir, "b.c", "int b(void) { return 2; }\n")
	outB := filepath.Join(dir, "b.o")

	var wg sync.WaitGroup
	resps := make([]*gen.CompileResponse, 2)

	wg.Add(1)
	go func() {
		defer wg.Done()
		conn := dialDaemon(t, l)
		sendCompileRequest(t, conn, &gen.CompileRequest{
			Args: []string{"clang", "-c", srcA, "-o", outA},
		})
		resps[0] = readCompileResponse(t, conn)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		conn := dialDaemon(t, l)
		sendCompileRequest(t, conn, &gen.CompileRequest{
			Args: []string{"clang", "-c", srcB, "-o", outB},
		})
		resps[1] = readCompileResponse(t, conn)
	}()

	wg.Wait()

	for i, resp := range resps {
		if resp.ExitCode != 0 {
			t.Errorf("request %d: expected exit 0, got %d: %s", i, resp.ExitCode, resp.Stderr)
		}
	}

	if got := cc.invokeCount.Load(); got != 2 {
		t.Errorf("expected 2 compiler invocations (different sources), got %d", got)
	}
}

func TestDaemonDedupErrorPropagation(t *testing.T) {
	clangAvailable(t)
	ctx, cc := setupCountingContext(t, 200*time.Millisecond)
	d := NewDefaultDaemon()
	d.Contexts.Store("clang", ctx)

	l := startTestDaemon(t, d)

	dir := t.TempDir()
	src := writeSource(t, dir, "bad.c", "this is not valid C\n")
	out := filepath.Join(dir, "bad.o")

	req := &gen.CompileRequest{
		Args: []string{"clang", "-c", src, "-o", out},
	}

	const n = 3
	resps := make([]*gen.CompileResponse, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn := dialDaemon(t, l)
			sendCompileRequest(t, conn, req)
			resps[i] = readCompileResponse(t, conn)
		}()
	}
	wg.Wait()

	for i, resp := range resps {
		if resp.ExitCode == 0 {
			t.Errorf("request %d: expected non-zero exit for bad source", i)
		}
		if len(resp.Stderr) == 0 {
			t.Errorf("request %d: expected stderr output", i)
		}
	}

	if got := cc.invokeCount.Load(); got != 1 {
		t.Errorf("expected 1 compiler invocation (dedup even on error), got %d", got)
	}
}

func TestDaemonDedupSequentialNotCoalesced(t *testing.T) {
	clangAvailable(t)
	ctx, cc := setupCountingContext(t, 0)
	d := NewDefaultDaemon()
	d.Contexts.Store("clang", ctx)

	l := startTestDaemon(t, d)

	dir := t.TempDir()
	src := writeSource(t, dir, "seq.c", "int seq(void) { return 0; }\n")
	out := filepath.Join(dir, "seq.o")

	req := &gen.CompileRequest{
		Args: []string{"clang", "-c", src, "-o", out},
	}

	conn := dialDaemon(t, l)
	sendCompileRequest(t, conn, req)
	resp1 := readCompileResponse(t, conn)
	if resp1.ExitCode != 0 {
		t.Fatalf("first request failed: %s", resp1.Stderr)
	}

	// Second request after first completes — singleflight should not
	// coalesce; the second hits the cache instead.
	os.Remove(out)
	conn2 := dialDaemon(t, l)
	sendCompileRequest(t, conn2, req)
	resp2 := readCompileResponse(t, conn2)
	if resp2.ExitCode != 0 {
		t.Fatalf("second request failed: %s", resp2.Stderr)
	}

	if got := cc.invokeCount.Load(); got != 1 {
		t.Errorf("expected 1 compiler invocation (second should be cache hit), got %d", got)
	}
}
