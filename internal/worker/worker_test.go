package worker

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"os/exec"
	goruntime "runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aarani/hpcc/internal/compiler"
	"github.com/aarani/hpcc/internal/config"
	"github.com/aarani/hpcc/internal/enum"
	"github.com/aarani/hpcc/internal/protocol/gen"
	"github.com/aarani/hpcc/internal/worker/runtime"
	"github.com/golang-jwt/jwt/v5"
	"github.com/zeebo/blake3"
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

// --- CAS-mode end-to-end ------------------------------------------------

// newCASTestWorker returns a Worker wired with everything needed to
// run a real CAS Compile RPC end-to-end through the dangerous runtime:
// dispatcher-shaped token validation, a disk-backed CompileCache, the
// namespaced sourceStore for blob materialization, and the dangerous
// runtime so the test process forks a real compiler. The temp dir
// backing the cache is reused for both compile and source namespaces.
func newCASTestWorker(t *testing.T) (*Worker, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519 key: %v", err)
	}
	cfg := Config{
		WorkerID: testWorkerID,
		Runtime:  RuntimeConfig{Handler: runtime.HandlerReallyReallyDangerous},
		VM:       VMConfig{VCPUs: 1, Memory: "1GB"},
		Caches: []config.CacheConfig{
			{Type: enum.CacheDisk, Location: t.TempDir()},
		},
	}
	w, err := NewWorker(cfg)
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	w.workerID = testWorkerID
	w.schedulerPubKey = pub
	return w, priv
}

// casDescriptor assembles a CasDescriptor from path → content pairs,
// hashing each content with BLAKE3 and computing the manifest digest
// the same way BuildManifest does (sort-by-path aggregation). It also
// returns the BlobRefs so the caller can pre-seed the source store.
func casDescriptor(t *testing.T, entryPath string, files map[string][]byte) *gen.CasDescriptor {
	t.Helper()
	refs := make([]compiler.BlobRef, 0, len(files))
	protoRefs := make([]*gen.BlobRef, 0, len(files))
	for path, content := range files {
		h := blake3.New()
		h.Write(content)
		var d [32]byte
		copy(d[:], h.Sum(nil))
		refs = append(refs, compiler.BlobRef{Path: path, Digest: d, Size: int64(len(content))})
		protoRefs = append(protoRefs, &gen.BlobRef{
			Path:   path,
			Digest: append([]byte(nil), d[:]...),
			Size:   uint64(len(content)),
		})
	}
	// Sort both lists by path; AggregateManifestDigest requires sorted input.
	sort.Slice(refs, func(i, j int) bool { return refs[i].Path < refs[j].Path })
	sort.Slice(protoRefs, func(i, j int) bool { return protoRefs[i].Path < protoRefs[j].Path })
	md := compiler.AggregateManifestDigest(refs)
	return &gen.CasDescriptor{
		ManifestDigest: md[:],
		Blobs:          protoRefs,
		EntryPath:      entryPath,
	}
}

// seedSourceStore puts every (path → content) pair into the worker's
// sourceStore keyed by BLAKE3(content). The path is informational;
// the store is content-addressed, so only the digest and bytes matter.
// Hardcoded to tenant "t1" — every worker test uses that tenant on
// its CompileRequest, so the store namespace must match.
func seedSourceStore(t *testing.T, w *Worker, files map[string][]byte) {
	t.Helper()
	ts := w.sourceStore.Namespace("t1")
	for _, content := range files {
		h := blake3.New()
		h.Write(content)
		if err := ts.Put(h.Sum(nil), blobData, content); err != nil {
			t.Fatalf("sourceStore.Put: %v", err)
		}
	}
}

func TestCompile_CASHappyPath(t *testing.T) {
	clangPath, err := exec.LookPath("clang")
	if err != nil {
		t.Skipf("clang not in PATH: %v", err)
	}
	_ = clangPath

	w, priv := newCASTestWorker(t)
	token := signToken(t, priv, validClaims())

	// One-file source closure: a tiny .c TU under src/.
	files := map[string][]byte{
		"src/main.c": []byte("int main(void){return 42;}\n"),
	}
	cas := casDescriptor(t, "src/main.c", files)
	seedSourceStore(t, w, files)

	req := &gen.CompileRequest{
		// Post-RewriteForCAS argv: project paths under /src, output under /out.
		Args: []string{"clang", "-c", "/src/src/main.c", "-o", "/out/main.o"},
		Descriptor_: &gen.RemoteDescriptor{
			TenantId:       "t1",
			ImageDigest:    "d1",
			SchedulerToken: token,
			SourceMode:     gen.SourceMode_CAS,
			SourceSettings: &gen.RemoteDescriptor_Cas{Cas: cas},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := w.Compile(ctx, req)
	if err != nil {
		t.Fatalf("Compile (cold): %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("ExitCode=%d, stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if !looksLikeObjectFile(resp.OutputArtifact) {
		head := resp.OutputArtifact
		if len(head) > 8 {
			head = head[:8]
		}
		t.Errorf("OutputArtifact doesn't look like an object: % x", head)
	}
	if resp.CacheKey == nil || *resp.CacheKey == "" {
		t.Errorf("expected non-empty CacheKey on CAS compile")
	}

	// Second compile with the same descriptor must hit the worker's
	// compile cache (Step 6 widened useCache to fire under CAS even
	// without paranoid mode). Asserting on the cache key match
	// catches regressions in the manifest→cache-key derivation.
	firstKey := *resp.CacheKey
	resp2, err := w.Compile(ctx, req)
	if err != nil {
		t.Fatalf("Compile (warm): %v", err)
	}
	if resp2.ExitCode != 0 {
		t.Fatalf("warm ExitCode=%d, stderr=%q", resp2.ExitCode, resp2.Stderr)
	}
	if resp2.CacheKey == nil || *resp2.CacheKey != firstKey {
		t.Errorf("warm cache key %v differs from cold %q", resp2.CacheKey, firstKey)
	}
	if !bytes.Equal(resp.OutputArtifact, resp2.OutputArtifact) {
		t.Errorf("warm artifact differs from cold; cache replay broken")
	}
}

func TestCompile_CASAssemblyWithIncbin(t *testing.T) {
	// Step 8's forcing function: a .S file with .incbin compiles
	// end-to-end through CAS. PREPROCESSED dispatch can't represent
	// this — the .incbin'd data isn't in the preprocessor output —
	// but CAS ships the full closure, so the assembler finds the
	// .bin alongside the .S inside the source root.
	//
	// Linux-only: the .section directive shape below is GNU GAS
	// syntax. Mach-O assemblers on macOS use a different vocabulary
	// (.const_data, leading underscores on globals) and would need
	// a separate fixture. The Step 8 design target is Linux kernel
	// .S files, so that's the platform we validate on.
	if goruntime.GOOS != "linux" {
		t.Skipf("skipping on %s: test fixture uses Linux GAS .section syntax", goruntime.GOOS)
	}
	gccPath, err := exec.LookPath("gcc")
	if err != nil {
		t.Skipf("gcc not in PATH: %v", err)
	}
	_ = gccPath

	w, priv := newCASTestWorker(t)
	token := signToken(t, priv, validClaims())

	// A tiny GAS source that .incbin's a data file at a sibling path.
	// The path inside .incbin is relative to the assembler's cwd —
	// the dangerous runtime sets cwd to the source-staging dir, so
	// "data.bin" resolves against the materialized source root.
	asmSrc := []byte(`
.section .rodata
.globl payload
payload:
.incbin "data.bin"
`)
	payload := []byte("hello-from-incbin")
	files := map[string][]byte{
		"foo.S":    asmSrc,
		"data.bin": payload,
	}
	cas := casDescriptor(t, "foo.S", files)
	seedSourceStore(t, w, files)

	req := &gen.CompileRequest{
		Args: []string{"gcc", "-c", "/src/foo.S", "-o", "/out/foo.o"},
		Descriptor_: &gen.RemoteDescriptor{
			TenantId:       "t1",
			ImageDigest:    "d1",
			SchedulerToken: token,
			SourceMode:     gen.SourceMode_CAS,
			SourceSettings: &gen.RemoteDescriptor_Cas{Cas: cas},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := w.Compile(ctx, req)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("assembly+incbin failed with exit=%d, stderr=%q (the .incbin'd file may not have been materialized at the assembler's cwd)", resp.ExitCode, resp.Stderr)
	}
	if !looksLikeObjectFile(resp.OutputArtifact) {
		t.Errorf("OutputArtifact doesn't look like an object")
	}
	// Sanity: the resulting object should contain the payload bytes
	// since .incbin embeds them verbatim into .rodata.
	if !bytes.Contains(resp.OutputArtifact, payload) {
		t.Errorf("compiled .o does not contain .incbin payload %q; .incbin probably didn't find data.bin", payload)
	}
}

func TestCompile_CASReturnsDepFileAsExtraOutput(t *testing.T) {
	// End-to-end: a CAS compile that asks for a Makefile-style .d
	// file via -Wp,-MMD,<path> must come back with that file in
	// CompileResponse.extra_outputs. Mirrors the kernel build's
	// incremental-build dependency tracking. Without this, the
	// kernel's `make` runs without dep info and falls back to
	// "rebuild every TU" on the next iteration.
	clangPath, err := exec.LookPath("clang")
	if err != nil {
		t.Skipf("clang not in PATH: %v", err)
	}
	_ = clangPath

	w, priv := newCASTestWorker(t)
	token := signToken(t, priv, validClaims())

	files := map[string][]byte{
		"src/main.c": []byte("int main(void){return 7;}\n"),
	}
	cas := casDescriptor(t, "src/main.c", files)
	seedSourceStore(t, w, files)

	// The dispatcher would have rewritten -Wp,-MMD,build/main.d to
	// /out/build/main.d; we send the post-rewrite form directly
	// (worker-side test, no client) so gcc writes its .d into the
	// output staging dir where collectExtraOutputs will pick it up.
	req := &gen.CompileRequest{
		Args: []string{
			"clang",
			"-Wp,-MMD,/out/build/main.d",
			"-c", "/src/src/main.c",
			"-o", "/out/main.o",
		},
		Descriptor_: &gen.RemoteDescriptor{
			TenantId:       "t1",
			ImageDigest:    "d1",
			SchedulerToken: token,
			SourceMode:     gen.SourceMode_CAS,
			SourceSettings: &gen.RemoteDescriptor_Cas{Cas: cas},
		},
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

	depBytes, ok := resp.ExtraOutputs["build/main.d"]
	if !ok {
		t.Fatalf("expected build/main.d in ExtraOutputs; got keys: %v", extraOutputKeys(resp.ExtraOutputs))
	}
	// Sanity: a Make-style dep file should mention the source file
	// after the colon target.
	if !strings.Contains(string(depBytes), "main.c") {
		t.Errorf(".d file doesn't reference main.c; got %q", depBytes)
	}

	// Warm-cache hit must replay the same .d bytes. The cache
	// stores Extras alongside the primary artifact; without that,
	// deleting a .d on disk would leave `make` re-firing the rule
	// forever on subsequent cache-hit responses.
	resp2, err := w.Compile(ctx, req)
	if err != nil {
		t.Fatalf("Compile (warm): %v", err)
	}
	if resp2.ExitCode != 0 {
		t.Fatalf("warm ExitCode=%d, stderr=%q", resp2.ExitCode, resp2.Stderr)
	}
	depBytes2, ok := resp2.ExtraOutputs["build/main.d"]
	if !ok {
		t.Fatalf("warm response missing build/main.d in ExtraOutputs; got %v", extraOutputKeys(resp2.ExtraOutputs))
	}
	if !bytes.Equal(depBytes, depBytes2) {
		t.Errorf("warm .d bytes differ from cold:\n cold %q\n warm %q", depBytes, depBytes2)
	}
}

func extraOutputKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestCompile_CASRejectsBadManifestDigest(t *testing.T) {
	w, priv := newCASTestWorker(t)
	token := signToken(t, priv, validClaims())
	files := map[string][]byte{"src/main.c": []byte("int main(){}\n")}
	cas := casDescriptor(t, "src/main.c", files)
	// Tamper with the manifest digest. Worker should refuse before
	// touching the source store.
	cas.ManifestDigest = bytes.Repeat([]byte{0xff}, 32)

	req := &gen.CompileRequest{
		Args: []string{"clang", "-c", "/src/src/main.c", "-o", "/out/main.o"},
		Descriptor_: &gen.RemoteDescriptor{
			TenantId:       "t1",
			ImageDigest:    "d1",
			SchedulerToken: token,
			SourceMode:     gen.SourceMode_CAS,
			SourceSettings: &gen.RemoteDescriptor_Cas{Cas: cas},
		},
	}

	_, err := w.Compile(context.Background(), req)
	if err == nil {
		t.Fatal("expected manifest verification error; got nil")
	}
	if !strings.Contains(err.Error(), "manifest digest") {
		t.Errorf("expected manifest-digest error; got %v", err)
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
