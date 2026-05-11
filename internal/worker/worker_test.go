package worker

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aarani/hpcc/internal/protocol/gen"
	"github.com/aarani/hpcc/internal/worker/runtime"
	"github.com/golang-jwt/jwt/v5"
)

// These are end-to-end tests against Worker.Compile, going through token
// validation, argv validation, source staging, the dangerous host
// runtime, the runtimeExecutor adapter, and the compiler package's
// Detect/Parse/Invoke pipeline. They do NOT cross the gRPC boundary —
// that's its own (later) integration test.

const testWorkerID = "test-worker-1"

func newTestWorker(t *testing.T) (*Worker, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519 key: %v", err)
	}
	cfg := Config{
		WorkerID: testWorkerID,
		Runtime:  RuntimeConfig{Handler: runtime.HandlerReallyReallyDangerous},
		VM:       VMConfig{VCPUs: 1, Memory: "1GB"},
	}
	w, err := NewWorker(cfg)
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	// bootstrap() also loads the cert fingerprint from disk; skip it and
	// set the fields the Compile handler actually reads directly.
	w.workerID = testWorkerID
	w.schedulerPubKey = pub
	return w, priv
}

func signToken(t *testing.T, priv ed25519.PrivateKey, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	s, err := tok.SignedString(priv)
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}
	return s
}

func validClaims() jwt.MapClaims {
	return jwt.MapClaims{
		"tenant_id":    "t1",
		"image_digest": "d1",
		"worker_id":    testWorkerID,
	}
}

func preprocessedDescriptor(token string, ppBytes []byte) *gen.RemoteDescriptor {
	return &gen.RemoteDescriptor{
		TenantId:       "t1",
		ImageDigest:    "d1",
		SchedulerToken: token,
		SourceMode:     gen.SourceMode_PREPROCESSED,
		SourceSettings: &gen.RemoteDescriptor_Preprocessed{
			Preprocessed: &gen.PreprocessedDescriptor{PreprocessedSource: ppBytes},
		},
	}
}

// --- gate-logic tests --------------------------------------------------

func TestCompile_RejectsMissingDescriptor(t *testing.T) {
	w, _ := newTestWorker(t)
	_, err := w.Compile(context.Background(), &gen.CompileRequest{Args: []string{"clang"}})
	if err == nil {
		t.Fatal("expected error for missing descriptor, got nil")
	}
}

func TestCompile_RejectsUnsignedToken(t *testing.T) {
	w, _ := newTestWorker(t)
	req := &gen.CompileRequest{
		Args:        []string{"clang", "-c", "/src/main.i", "-o", "/out/main.o"},
		Descriptor_: preprocessedDescriptor("not-a-jwt", nil),
	}
	_, err := w.Compile(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for invalid token, got nil")
	}
}

func TestCompile_RejectsTokenWithMismatchedTenant(t *testing.T) {
	w, priv := newTestWorker(t)
	claims := validClaims()
	claims["tenant_id"] = "different-tenant"
	token := signToken(t, priv, claims)
	req := &gen.CompileRequest{
		Args:        []string{"clang", "-c", "/src/main.i", "-o", "/out/main.o"},
		Descriptor_: preprocessedDescriptor(token, nil),
	}
	_, err := w.Compile(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for tenant claim mismatch, got nil")
	}
}

func TestCompile_RejectsHostPathInArgv(t *testing.T) {
	w, priv := newTestWorker(t)
	token := signToken(t, priv, validClaims())
	req := &gen.CompileRequest{
		// Host-shaped path leaks past client-side rewriting.
		Args:        []string{"clang", "-c", "/home/alice/main.cpp", "-o", "/out/main.o"},
		Descriptor_: preprocessedDescriptor(token, nil),
	}
	_, err := w.Compile(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for host-shaped path, got nil")
	}
	if !strings.Contains(err.Error(), "argv validation") {
		t.Errorf("expected argv-validation error, got: %v", err)
	}
}

// --- happy-path test ---------------------------------------------------

func TestCompile_PreprocessedHappyPath(t *testing.T) {
	clangPath, err := exec.LookPath("clang")
	if err != nil {
		t.Skipf("clang not in PATH: %v", err)
	}

	// Produce real preprocessed bytes from a tiny C program. clang -E on
	// stdin emits the same line-marked output the client would ship in
	// PreprocessedDescriptor.preprocessed_source.
	preCmd := exec.Command(clangPath, "-E", "-x", "c", "-")
	preCmd.Stdin = strings.NewReader("int main(void){return 42;}\n")
	var ppOut bytes.Buffer
	preCmd.Stdout = &ppOut
	var preErr bytes.Buffer
	preCmd.Stderr = &preErr
	if err := preCmd.Run(); err != nil {
		t.Fatalf("clang -E: %v (stderr=%q)", err, preErr.String())
	}

	w, priv := newTestWorker(t)
	token := signToken(t, priv, validClaims())

	req := &gen.CompileRequest{
		// argv is in the post-RewriteForPreprocessed shape: -x cpp-output,
		// -c referencing the in-container path the worker stages to,
		// and -o pointing into /out so runtimeExecutor.ReadOutput can
		// translate it back to the host outDir.
		Args: []string{
			"clang",
			"-x", "cpp-output",
			"-c", "/src/" + preprocessedSourceName,
			"-o", "/out/main.o",
		},
		Descriptor_: preprocessedDescriptor(token, ppOut.Bytes()),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := w.Compile(ctx, req)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("ExitCode=%d, stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(resp.OutputArtifact) == 0 {
		t.Fatal("expected non-empty OutputArtifact")
	}

	// Sanity-check that the bytes really are a compiled object — the
	// magic numbers are short and stable, so a regression in the read-back
	// path would manifest as garbage and trip this assertion.
	if !looksLikeObjectFile(resp.OutputArtifact) {
		head := resp.OutputArtifact
		if len(head) > 8 {
			head = head[:8]
		}
		t.Errorf("OutputArtifact doesn't look like ELF/Mach-O/COFF: first bytes = % x", head)
	}

	if resp.CacheKey == nil || *resp.CacheKey == "" {
		t.Errorf("expected non-empty CacheKey for PREPROCESSED-mode compile")
	}

	// Audit record sanity: every field the worker can fill on the
	// happy path should be populated. SourceDigest comes from the
	// PREPROCESSED descriptor; OutputDigest is BLAKE3 of the artifact;
	// the rest pass through from request / worker identity.
	if resp.Audit == nil {
		t.Fatal("expected Audit on CompileResponse")
	}
	a := resp.Audit
	if a.TenantId != "t1" {
		t.Errorf("Audit.TenantId = %q, want t1", a.TenantId)
	}
	if a.WorkerId != testWorkerID {
		t.Errorf("Audit.WorkerId = %q, want %q", a.WorkerId, testWorkerID)
	}
	if a.VmId == "" {
		t.Error("Audit.VmId is empty")
	}
	if a.ImageDigest != "d1" {
		t.Errorf("Audit.ImageDigest = %q, want d1", a.ImageDigest)
	}
	if a.CacheKey == "" || a.CacheKey != *resp.CacheKey {
		t.Errorf("Audit.CacheKey = %q, want %q", a.CacheKey, *resp.CacheKey)
	}
	if a.ExitCode != resp.ExitCode {
		t.Errorf("Audit.ExitCode = %d, want %d", a.ExitCode, resp.ExitCode)
	}
	if got := len(a.SourceDigest); got != 32 {
		t.Errorf("Audit.SourceDigest length = %d, want 32 (BLAKE3-256)", got)
	}
	if got := len(a.OutputDigest); got != 32 {
		t.Errorf("Audit.OutputDigest length = %d, want 32 (BLAKE3-256)", got)
	}
	if a.Timestamp == 0 {
		t.Error("Audit.Timestamp not set")
	}
	if !slicesEqualString(a.Flags, req.Args[1:]) {
		t.Errorf("Audit.Flags = %q, want %q", a.Flags, req.Args[1:])
	}
}

func slicesEqualString(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestCompile_PreprocessedSameInputProducesStableCacheKey(t *testing.T) {
	clangPath, err := exec.LookPath("clang")
	if err != nil {
		t.Skipf("clang not in PATH: %v", err)
	}

	preCmd := exec.Command(clangPath, "-E", "-x", "c", "-")
	preCmd.Stdin = strings.NewReader("int main(void){return 7;}\n")
	var ppOut bytes.Buffer
	preCmd.Stdout = &ppOut
	if err := preCmd.Run(); err != nil {
		t.Fatalf("clang -E: %v", err)
	}
	ppBytes := ppOut.Bytes()

	doCompile := func(t *testing.T) string {
		t.Helper()
		w, priv := newTestWorker(t)
		token := signToken(t, priv, validClaims())
		req := &gen.CompileRequest{
			Args: []string{
				"clang",
				"-x", "cpp-output",
				"-c", "/src/" + preprocessedSourceName,
				"-o", "/out/main.o",
				"-O2",
			},
			Descriptor_: preprocessedDescriptor(token, ppBytes),
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		resp, err := w.Compile(ctx, req)
		if err != nil {
			t.Fatalf("Compile: %v", err)
		}
		if resp.CacheKey == nil {
			t.Fatal("CacheKey is nil")
		}
		return *resp.CacheKey
	}

	k1 := doCompile(t)
	k2 := doCompile(t)
	if k1 != k2 {
		t.Errorf("cache keys for identical input differ:\n  k1=%s\n  k2=%s", k1, k2)
	}
	if k1 == "" {
		t.Error("cache key is empty")
	}
}

func TestCompile_PreprocessedNonZeroExitReturnsAsData(t *testing.T) {
	if _, err := exec.LookPath("clang"); err != nil {
		t.Skipf("clang not in PATH: %v", err)
	}

	w, priv := newTestWorker(t)
	token := signToken(t, priv, validClaims())

	// Deliberately broken preprocessed source — clang exits non-zero,
	// which should come back as data on the response, not as an RPC error.
	req := &gen.CompileRequest{
		Args: []string{
			"clang",
			"-x", "cpp-output",
			"-c", "/src/" + preprocessedSourceName,
			"-o", "/out/main.o",
		},
		Descriptor_: preprocessedDescriptor(token, []byte("not valid C source @@@\n")),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := w.Compile(ctx, req)
	if err != nil {
		t.Fatalf("Compile returned error for compile failure (should be data): %v", err)
	}
	if resp.ExitCode == 0 {
		t.Errorf("ExitCode=0, expected non-zero for invalid source")
	}
	if len(resp.Stderr) == 0 {
		t.Errorf("expected non-empty stderr for compile failure")
	}
}

func looksLikeObjectFile(b []byte) bool {
	if len(b) < 4 {
		return false
	}
	switch {
	case b[0] == 0x7f && b[1] == 'E' && b[2] == 'L' && b[3] == 'F': // ELF
		return true
	case b[0] == 0xfe && b[1] == 0xed && b[2] == 0xfa && (b[3] == 0xce || b[3] == 0xcf): // Mach-O big-endian
		return true
	case b[0] == 0xcf && b[1] == 0xfa && b[2] == 0xed && b[3] == 0xfe: // Mach-O 64 little-endian
		return true
	case b[0] == 0xce && b[1] == 0xfa && b[2] == 0xed && b[3] == 0xfe: // Mach-O 32 little-endian
		return true
	case len(b) >= 2 && b[0] == 0x4c && b[1] == 0x01: // COFF x86_64
		return true
	}
	return false
}

// --- image eviction -----------------------------------------------------

func TestEvictImagesOnce(t *testing.T) {
	w, _ := newTestWorker(t)

	now := time.Now()
	fresh := &imageEntry{}
	fresh.lastUsed.Store(now.UnixNano())
	stale := &imageEntry{}
	stale.lastUsed.Store(now.Add(-2 * time.Hour).UnixNano())
	pinnedStale := &imageEntry{}
	pinnedStale.lastUsed.Store(now.Add(-2 * time.Hour).UnixNano())

	w.Images.Store("fresh-digest", fresh)
	w.Images.Store("stale-digest", stale)
	w.Images.Store("pinned-digest", pinnedStale)

	pinned := map[string]struct{}{"pinned-digest": {}}
	w.evictImagesOnce(context.Background(), time.Hour, pinned)

	if _, ok := w.Images.Load("fresh-digest"); !ok {
		t.Errorf("fresh entry was evicted")
	}
	if _, ok := w.Images.Load("stale-digest"); ok {
		t.Errorf("stale entry was not evicted")
	}
	if _, ok := w.Images.Load("pinned-digest"); !ok {
		t.Errorf("pinned (advertised) entry was evicted")
	}
}
