package worker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"

	"github.com/golang-jwt/jwt/v5"
	"github.com/zeebo/blake3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/aarani/hpcc/internal/cache/store"
	"github.com/aarani/hpcc/internal/compiler"
	"github.com/aarani/hpcc/internal/protocol/gen"
)

// fakeFindMissingStream implements the FindMissingBlobs bidi stream
// in-memory for handler tests. Recv pops from sendQueue (digests the
// "client" wants probed); Send appends to recvQueue (digests the
// worker is reporting as missing). EOF when sendQueue drains.
type fakeFindMissingStream struct {
	grpc.ServerStream
	ctx        context.Context
	sendQueue  []*gen.BlobDigest
	recvQueue  []*gen.BlobDigest
	sendErr    error // optional: error to return from Send
	recvErrAt  int   // optional: index in sendQueue to return error from Recv (instead of EOF)
	recvErrMsg string
}

func (s *fakeFindMissingStream) Context() context.Context     { return s.ctx }
func (s *fakeFindMissingStream) SetHeader(metadata.MD) error  { return nil }
func (s *fakeFindMissingStream) SendHeader(metadata.MD) error { return nil }
func (s *fakeFindMissingStream) SetTrailer(metadata.MD)       {}

// SendMsg/RecvMsg exist only to satisfy the grpc.ServerStream
// interface. The CAS handlers call the typed Recv/Send helpers
// instead, so these are unreached in tests.
func (s *fakeFindMissingStream) SendMsg(any) error { return nil }
func (s *fakeFindMissingStream) RecvMsg(any) error { return nil }

func (s *fakeFindMissingStream) Recv() (*gen.BlobDigest, error) {
	if s.recvErrMsg != "" && s.recvErrAt == 0 {
		return nil, errors.New(s.recvErrMsg)
	}
	if len(s.sendQueue) == 0 {
		return nil, io.EOF
	}
	d := s.sendQueue[0]
	s.sendQueue = s.sendQueue[1:]
	if s.recvErrAt > 0 {
		s.recvErrAt--
	}
	return d, nil
}

func (s *fakeFindMissingStream) Send(d *gen.BlobDigest) error {
	if s.sendErr != nil {
		return s.sendErr
	}
	s.recvQueue = append(s.recvQueue, d)
	return nil
}

// newSourceTestWorker builds a Worker with a sourceStore backed by a
// fresh temp dir. No runtime / image / scheduler wiring — only the
// pieces FindMissingBlobs touches.
func newSourceTestWorker(t *testing.T) *Worker {
	t.Helper()
	ds, err := store.NewDiskCacheStore(t.TempDir(), "")
	if err != nil {
		t.Fatalf("NewDiskCacheStore: %v", err)
	}
	return &Worker{sourceStore: ds.Namespace("source")}
}

func TestFindMissingBlobs_emptyStoreReportsAllMissing(t *testing.T) {
	w := newSourceTestWorker(t)
	probes := []*gen.BlobDigest{
		{Digest: bytes.Repeat([]byte{0x11}, 32), Size: 100},
		{Digest: bytes.Repeat([]byte{0x22}, 32), Size: 200},
		{Digest: bytes.Repeat([]byte{0x33}, 32), Size: 300},
	}
	stream := &fakeFindMissingStream{ctx: context.Background(), sendQueue: probes}

	if err := w.FindMissingBlobs(stream); err != nil {
		t.Fatalf("FindMissingBlobs: %v", err)
	}
	if len(stream.recvQueue) != len(probes) {
		t.Errorf("got %d missing, want %d", len(stream.recvQueue), len(probes))
	}
}

func TestFindMissingBlobs_presentBlobsOmitted(t *testing.T) {
	w := newSourceTestWorker(t)
	present := bytes.Repeat([]byte{0xab}, 32)
	missing := bytes.Repeat([]byte{0xcd}, 32)

	// Plant `present` in the store so it should be filtered out.
	if err := w.sourceStore.Put(present, blobData, []byte("hello")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	stream := &fakeFindMissingStream{
		ctx: context.Background(),
		sendQueue: []*gen.BlobDigest{
			{Digest: present, Size: 5},
			{Digest: missing, Size: 7},
		},
	}
	if err := w.FindMissingBlobs(stream); err != nil {
		t.Fatalf("FindMissingBlobs: %v", err)
	}
	if len(stream.recvQueue) != 1 {
		t.Fatalf("got %d missing, want 1", len(stream.recvQueue))
	}
	if !bytes.Equal(stream.recvQueue[0].Digest, missing) {
		t.Errorf("wrong digest reported missing: got %x, want %x", stream.recvQueue[0].Digest, missing)
	}
}

func TestFindMissingBlobs_noSourceStoreIsFailedPrecondition(t *testing.T) {
	w := &Worker{} // sourceStore is nil
	stream := &fakeFindMissingStream{ctx: context.Background()}

	err := w.FindMissingBlobs(stream)
	if err == nil {
		t.Fatal("FindMissingBlobs returned nil; want FailedPrecondition")
	}
	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Errorf("got code %v, want FailedPrecondition", got)
	}
}

func TestFindMissingBlobs_rejectsEmptyDigest(t *testing.T) {
	w := newSourceTestWorker(t)
	stream := &fakeFindMissingStream{
		ctx:       context.Background(),
		sendQueue: []*gen.BlobDigest{{Digest: nil, Size: 0}},
	}
	err := w.FindMissingBlobs(stream)
	if err == nil {
		t.Fatal("FindMissingBlobs returned nil; want InvalidArgument")
	}
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Errorf("got code %v, want InvalidArgument", got)
	}
}

// fakeUploadStream implements the UploadBlobs client-stream in
// memory. Recv pops from sendQueue; SendAndClose captures the
// terminal result.
type fakeUploadStream struct {
	grpc.ServerStream
	ctx       context.Context
	sendQueue []*gen.BlobChunk
	result    *gen.UploadResult
}

func (s *fakeUploadStream) Context() context.Context     { return s.ctx }
func (s *fakeUploadStream) SetHeader(metadata.MD) error  { return nil }
func (s *fakeUploadStream) SendHeader(metadata.MD) error { return nil }
func (s *fakeUploadStream) SetTrailer(metadata.MD)       {}

// SendMsg/RecvMsg are unused by the CAS handlers (which use the
// typed helpers); see fakeFindMissingStream.
func (s *fakeUploadStream) SendMsg(any) error { return nil }
func (s *fakeUploadStream) RecvMsg(any) error { return nil }

func (s *fakeUploadStream) Recv() (*gen.BlobChunk, error) {
	if len(s.sendQueue) == 0 {
		return nil, io.EOF
	}
	c := s.sendQueue[0]
	s.sendQueue = s.sendQueue[1:]
	return c, nil
}

func (s *fakeUploadStream) SendAndClose(r *gen.UploadResult) error {
	s.result = r
	return nil
}

func mkHeader(digest []byte, size uint64) *gen.BlobChunk {
	return &gen.BlobChunk{Body: &gen.BlobChunk_Header{Header: &gen.BlobDigest{Digest: digest, Size: size}}}
}

func mkData(data []byte) *gen.BlobChunk {
	return &gen.BlobChunk{Body: &gen.BlobChunk_Data{Data: data}}
}

func b3(data []byte) []byte {
	h := blake3.New()
	h.Write(data)
	return h.Sum(nil)
}

func TestUploadBlobs_acceptsMatchingHash(t *testing.T) {
	w := newSourceTestWorker(t)
	payload := []byte("the quick brown fox")
	digest := b3(payload)

	stream := &fakeUploadStream{
		ctx:       context.Background(),
		sendQueue: []*gen.BlobChunk{mkHeader(digest, uint64(len(payload))), mkData(payload)},
	}
	if err := w.UploadBlobs(stream); err != nil {
		t.Fatalf("UploadBlobs: %v", err)
	}
	if stream.result == nil {
		t.Fatal("no UploadResult returned")
	}
	if stream.result.BlobsReceived != 1 {
		t.Errorf("BlobsReceived=%d, want 1", stream.result.BlobsReceived)
	}
	if stream.result.BytesReceived != uint64(len(payload)) {
		t.Errorf("BytesReceived=%d, want %d", stream.result.BytesReceived, len(payload))
	}
	if len(stream.result.RejectedDigests) != 0 {
		t.Errorf("expected no rejections; got %v", stream.result.RejectedDigests)
	}

	got, err := w.sourceStore.Get(digest, blobData)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("stored bytes mismatch: got %q want %q", got, payload)
	}
}

func TestUploadBlobs_rejectsHashMismatch(t *testing.T) {
	w := newSourceTestWorker(t)
	claimed := bytes.Repeat([]byte{0xff}, 32)
	payload := []byte("doesn't match")

	stream := &fakeUploadStream{
		ctx:       context.Background(),
		sendQueue: []*gen.BlobChunk{mkHeader(claimed, uint64(len(payload))), mkData(payload)},
	}
	if err := w.UploadBlobs(stream); err != nil {
		t.Fatalf("UploadBlobs: %v", err)
	}
	if stream.result.BlobsReceived != 0 {
		t.Errorf("BlobsReceived=%d, want 0", stream.result.BlobsReceived)
	}
	if len(stream.result.RejectedDigests) != 1 || !bytes.Equal(stream.result.RejectedDigests[0], claimed) {
		t.Errorf("RejectedDigests=%v, want [%x]", stream.result.RejectedDigests, claimed)
	}
	got, err := w.sourceStore.Get(claimed, blobData)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != nil {
		t.Errorf("rejected blob should not be stored; got %d bytes", len(got))
	}
}

func TestUploadBlobs_multiBlobMixHitAndMiss(t *testing.T) {
	w := newSourceTestWorker(t)
	goodPayload := []byte("good")
	goodDigest := b3(goodPayload)
	badClaimed := bytes.Repeat([]byte{0x55}, 32)
	badPayload := []byte("won't match")

	stream := &fakeUploadStream{
		ctx: context.Background(),
		sendQueue: []*gen.BlobChunk{
			mkHeader(goodDigest, uint64(len(goodPayload))), mkData(goodPayload),
			mkHeader(badClaimed, uint64(len(badPayload))), mkData(badPayload),
		},
	}
	if err := w.UploadBlobs(stream); err != nil {
		t.Fatalf("UploadBlobs: %v", err)
	}
	if stream.result.BlobsReceived != 1 {
		t.Errorf("BlobsReceived=%d, want 1", stream.result.BlobsReceived)
	}
	if len(stream.result.RejectedDigests) != 1 {
		t.Errorf("expected 1 rejection, got %d", len(stream.result.RejectedDigests))
	}
	got, _ := w.sourceStore.Get(goodDigest, blobData)
	if !bytes.Equal(got, goodPayload) {
		t.Errorf("good blob not stored correctly: got %q want %q", got, goodPayload)
	}
}

func TestUploadBlobs_multiChunkBlob(t *testing.T) {
	w := newSourceTestWorker(t)
	payload := bytes.Repeat([]byte("abcdefgh"), 1000)
	digest := b3(payload)

	chunks := []*gen.BlobChunk{mkHeader(digest, uint64(len(payload)))}
	for i := 0; i < len(payload); i += 1024 {
		end := i + 1024
		if end > len(payload) {
			end = len(payload)
		}
		chunks = append(chunks, mkData(payload[i:end]))
	}

	stream := &fakeUploadStream{ctx: context.Background(), sendQueue: chunks}
	if err := w.UploadBlobs(stream); err != nil {
		t.Fatalf("UploadBlobs: %v", err)
	}
	if stream.result.BlobsReceived != 1 {
		t.Errorf("BlobsReceived=%d, want 1", stream.result.BlobsReceived)
	}
	got, _ := w.sourceStore.Get(digest, blobData)
	if !bytes.Equal(got, payload) {
		t.Errorf("stored bytes don't match payload")
	}
}

func TestUploadBlobs_emptyBlobIsValid(t *testing.T) {
	w := newSourceTestWorker(t)
	emptyDigest := b3(nil)

	stream := &fakeUploadStream{
		ctx:       context.Background(),
		sendQueue: []*gen.BlobChunk{mkHeader(emptyDigest, 0)},
	}
	if err := w.UploadBlobs(stream); err != nil {
		t.Fatalf("UploadBlobs: %v", err)
	}
	if stream.result.BlobsReceived != 1 {
		t.Errorf("BlobsReceived=%d, want 1", stream.result.BlobsReceived)
	}
}

func TestUploadBlobs_dataBeforeHeaderIsInvalidArg(t *testing.T) {
	w := newSourceTestWorker(t)
	stream := &fakeUploadStream{
		ctx:       context.Background(),
		sendQueue: []*gen.BlobChunk{mkData([]byte("orphan"))},
	}
	err := w.UploadBlobs(stream)
	if err == nil {
		t.Fatal("UploadBlobs returned nil; want InvalidArgument")
	}
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Errorf("got code %v, want InvalidArgument", got)
	}
}

func TestUploadBlobs_malformedHeaderIsInvalidArg(t *testing.T) {
	w := newSourceTestWorker(t)
	stream := &fakeUploadStream{
		ctx:       context.Background(),
		sendQueue: []*gen.BlobChunk{mkHeader(nil, 0)},
	}
	err := w.UploadBlobs(stream)
	if err == nil {
		t.Fatal("UploadBlobs returned nil; want InvalidArgument")
	}
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Errorf("got code %v, want InvalidArgument", got)
	}
}

func TestUploadBlobs_noSourceStoreIsFailedPrecondition(t *testing.T) {
	w := &Worker{}
	stream := &fakeUploadStream{ctx: context.Background()}
	err := w.UploadBlobs(stream)
	if err == nil {
		t.Fatal("UploadBlobs returned nil; want FailedPrecondition")
	}
	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Errorf("got code %v, want FailedPrecondition", got)
	}
}

func TestMkdirOutputParents_createsForEveryOutPathInArgv(t *testing.T) {
	root := t.TempDir()
	args := []string{
		"clang",
		"-c", "/src/main.c",
		"-o", "/out/build/main.o",                     // separate -o
		"-Wp,-MMD,/out/scripts/mod/.empty.o.d",        // -Wp,-M*, embedded
		"-MF", "/out/deep/nested/path/foo.d",          // separate -MF
		"-I/usr/include",                              // no /out/, ignored
	}
	if err := mkdirOutputParents(args, root); err != nil {
		t.Fatalf("mkdirOutputParents: %v", err)
	}

	wantDirs := []string{
		filepath.Join(root, "build"),
		filepath.Join(root, "scripts", "mod"),
		filepath.Join(root, "deep", "nested", "path"),
	}
	for _, d := range wantDirs {
		info, err := os.Stat(d)
		if err != nil {
			t.Errorf("expected dir %q to exist: %v", d, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("%q exists but isn't a dir", d)
		}
	}
}

func TestMkdirOutputParents_idempotent(t *testing.T) {
	root := t.TempDir()
	args := []string{"-Wp,-MMD,/out/build/foo.d"}
	for i := 0; i < 3; i++ {
		if err := mkdirOutputParents(args, root); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "build")); err != nil {
		t.Errorf("build/ should exist: %v", err)
	}
}

func TestMkdirOutputParents_skipsTopLevelOnlyPath(t *testing.T) {
	// /out/foo.o has no nested parent — outHostPath itself already
	// exists, so we shouldn't try to MkdirAll the empty-string or "."
	// parent (would error or no-op depending on platform).
	root := t.TempDir()
	args := []string{"-o", "/out/foo.o"}
	if err := mkdirOutputParents(args, root); err != nil {
		t.Fatalf("mkdirOutputParents: %v", err)
	}
}

// --- ProbeCompileCache tests ---------------------------------------

// newProbeTestWorker builds a Worker with a disk-backed compile cache
// and the token-validation fields the probe handler reads. Returns the
// worker plus the scheduler private key (for signing test tokens) and
// the path of the cache root (in case a test wants to inspect it).
func newProbeTestWorker(t *testing.T) (*Worker, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519 key: %v", err)
	}
	ds, err := store.NewDiskCacheStore(t.TempDir(), "")
	if err != nil {
		t.Fatalf("NewDiskCacheStore: %v", err)
	}
	w := &Worker{
		caches:          []store.Store{ds},
		sourceStore:     ds.Namespace("source"),
		workerID:        testWorkerID,
		schedulerPubKey: pub,
	}
	return w, priv
}

// probeWith primes a probe request with a valid scheduler-signed token
// for (tenant, image). The handler verifies tenant_id/image_digest/
// worker_id claims against the request, so the helper threads matching
// values through.
func probeWith(t *testing.T, priv ed25519.PrivateKey, manifestDigest []byte, args []string, tenant, image string) *gen.CompileProbe {
	t.Helper()
	tok := signToken(t, priv, jwt.MapClaims{
		"tenant_id":    tenant,
		"image_digest": image,
		"worker_id":    testWorkerID,
	})
	return &gen.CompileProbe{
		ManifestDigest:  manifestDigest,
		Args:            args,
		TenantId:        tenant,
		ImageDigest:     image,
		SchedulerToken:  tok,
	}
}

// primeCompileCache writes a CompileResponse-shaped entry that
// ProbeCompileCache should hit. The cache key is derived the same way
// the handler will derive it: parse args, set ManifestDigest, build
// the worker's compileContext with the image digest as
// IdentityOverride, call CompileCache.Store.
func primeCompileCache(t *testing.T, w *Worker, manifestDigest []byte, args []string, image string, result *compiler.InvocationResult) {
	t.Helper()
	c, err := compiler.Detect(args[0])
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	inv, err := c.Parse(args[1:])
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var md [32]byte
	copy(md[:], manifestDigest)
	inv.ManifestDigest = &md

	cctx := w.compileContext(c, image)
	if err := cctx.Cache.Store(inv, result); err != nil {
		t.Fatalf("Cache.Store: %v", err)
	}
}

func TestProbeCompileCache_hitReturnsCachedArtifact(t *testing.T) {
	w, priv := newProbeTestWorker(t)
	manifestDigest := bytes.Repeat([]byte{0xab}, 32)
	args := []string{"clang", "-c", "/src/main.i", "-o", "/out/main.o"}
	want := &compiler.InvocationResult{
		Output:   []byte("ELF magic bytes go here"),
		Stdout:   []byte("compiling..."),
		Stderr:   []byte("warning: unused var"),
		ExitCode: 0,
	}
	primeCompileCache(t, w, manifestDigest, args, "d1", want)

	req := probeWith(t, priv, manifestDigest, args, "t1", "d1")
	resp, err := w.ProbeCompileCache(context.Background(), req)
	if err != nil {
		t.Fatalf("ProbeCompileCache: %v", err)
	}
	hit := resp.GetHit()
	if hit == nil {
		t.Fatalf("expected Hit, got %T", resp.Result)
	}
	if !bytes.Equal(hit.OutputArtifact, want.Output) {
		t.Errorf("OutputArtifact mismatch: got %q want %q", hit.OutputArtifact, want.Output)
	}
	if !bytes.Equal(hit.Stdout, want.Stdout) {
		t.Errorf("Stdout mismatch: got %q want %q", hit.Stdout, want.Stdout)
	}
	if !bytes.Equal(hit.Stderr, want.Stderr) {
		t.Errorf("Stderr mismatch: got %q want %q", hit.Stderr, want.Stderr)
	}
	if hit.ExitCode != int32(want.ExitCode) {
		t.Errorf("ExitCode=%d, want %d", hit.ExitCode, want.ExitCode)
	}
	if hit.CacheKey == nil || *hit.CacheKey == "" {
		t.Errorf("expected non-empty CacheKey on hit response")
	}
}

func TestProbeCompileCache_missOnUnknownManifest(t *testing.T) {
	w, priv := newProbeTestWorker(t)
	// Cache is empty; any probe should miss.
	req := probeWith(t, priv,
		bytes.Repeat([]byte{0x01}, 32),
		[]string{"clang", "-c", "/src/main.i", "-o", "/out/main.o"},
		"t1", "d1")
	resp, err := w.ProbeCompileCache(context.Background(), req)
	if err != nil {
		t.Fatalf("ProbeCompileCache: %v", err)
	}
	if resp.GetMiss() == nil {
		t.Errorf("expected Miss, got %T", resp.Result)
	}
}

func TestProbeCompileCache_missOnDifferentArgs(t *testing.T) {
	// Same manifest digest in cache, but probe uses different args
	// (different cache key) — should miss.
	w, priv := newProbeTestWorker(t)
	manifestDigest := bytes.Repeat([]byte{0xab}, 32)
	primeCompileCache(t, w, manifestDigest,
		[]string{"clang", "-O2", "-c", "/src/main.i", "-o", "/out/main.o"},
		"d1",
		&compiler.InvocationResult{Output: []byte("x"), ExitCode: 0})

	req := probeWith(t, priv, manifestDigest,
		[]string{"clang", "-O0", "-c", "/src/main.i", "-o", "/out/main.o"},
		"t1", "d1")
	resp, err := w.ProbeCompileCache(context.Background(), req)
	if err != nil {
		t.Fatalf("ProbeCompileCache: %v", err)
	}
	if resp.GetMiss() == nil {
		t.Errorf("expected Miss on -O level change, got %T", resp.Result)
	}
}

func TestProbeCompileCache_missOnDifferentImage(t *testing.T) {
	w, priv := newProbeTestWorker(t)
	manifestDigest := bytes.Repeat([]byte{0xab}, 32)
	args := []string{"clang", "-c", "/src/main.i", "-o", "/out/main.o"}
	primeCompileCache(t, w, manifestDigest, args, "d1",
		&compiler.InvocationResult{Output: []byte("x"), ExitCode: 0})

	// Probe with the same manifest+args but a different image digest.
	// Image digest is mixed into the cache key (as IdentityOverride),
	// so this must miss — different toolchain image → different
	// expected output.
	req := probeWith(t, priv, manifestDigest, args, "t1", "d2")
	resp, err := w.ProbeCompileCache(context.Background(), req)
	if err != nil {
		t.Fatalf("ProbeCompileCache: %v", err)
	}
	if resp.GetMiss() == nil {
		t.Errorf("expected Miss on different image digest, got %T", resp.Result)
	}
}

func TestProbeCompileCache_rejectsBadToken(t *testing.T) {
	w, _ := newProbeTestWorker(t)
	req := &gen.CompileProbe{
		ManifestDigest:  bytes.Repeat([]byte{0xab}, 32),
		Args:            []string{"clang", "-c", "/src/main.i", "-o", "/out/main.o"},
		TenantId:        "t1",
		ImageDigest:     "d1",
		SchedulerToken:  "not-a-jwt",
	}
	_, err := w.ProbeCompileCache(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for bad token, got nil")
	}
}

func TestProbeCompileCache_rejectsWrongDigestLength(t *testing.T) {
	w, priv := newProbeTestWorker(t)
	req := probeWith(t, priv,
		bytes.Repeat([]byte{0xab}, 16), // too short
		[]string{"clang", "-c", "/src/main.i", "-o", "/out/main.o"},
		"t1", "d1")
	_, err := w.ProbeCompileCache(context.Background(), req)
	if err == nil {
		t.Fatal("expected InvalidArgument for 16-byte digest, got nil")
	}
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Errorf("got code %v, want InvalidArgument", got)
	}
}

func TestProbeCompileCache_rejectsEmptyArgs(t *testing.T) {
	w, priv := newProbeTestWorker(t)
	req := probeWith(t, priv, bytes.Repeat([]byte{0xab}, 32), nil, "t1", "d1")
	_, err := w.ProbeCompileCache(context.Background(), req)
	if err == nil {
		t.Fatal("expected InvalidArgument for empty args, got nil")
	}
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Errorf("got code %v, want InvalidArgument", got)
	}
}

// --- Step 6 tests: manifest verification + CAS materialization ---

// blobRefWithDigest returns a gen.BlobRef whose Digest field is the
// BLAKE3 of the given content. Used to build valid CasDescriptor
// fixtures.
func blobRefWithDigest(path string, content []byte) *gen.BlobRef {
	d := b3(content)
	return &gen.BlobRef{Path: path, Digest: d, Size: uint64(len(content))}
}

// blobRefToCompiler converts a gen.BlobRef into the compiler-package
// shape so we can call compiler.AggregateManifestDigest.
func blobRefToCompiler(b *gen.BlobRef) compiler.BlobRef {
	var d [32]byte
	copy(d[:], b.Digest)
	return compiler.BlobRef{Path: b.Path, Digest: d, Size: int64(b.Size)}
}

func TestVerifyManifestDigest_acceptsValid(t *testing.T) {
	a := blobRefWithDigest("src/main.c", []byte("int main(){return 0;}"))
	b := blobRefWithDigest("include/foo.h", []byte("#define FOO 1"))
	expected := compiler.AggregateManifestDigest([]compiler.BlobRef{
		blobRefToCompiler(b), // sorted by path: include/ before src/
		blobRefToCompiler(a),
	})
	cas := &gen.CasDescriptor{
		ManifestDigest: expected[:],
		// Deliberately unsorted on the wire; verifier sorts.
		Blobs: []*gen.BlobRef{a, b},
	}
	got, err := verifyManifestDigest(cas)
	if err != nil {
		t.Fatalf("verifyManifestDigest: %v", err)
	}
	if !bytes.Equal(got[:], expected[:]) {
		t.Errorf("returned digest %x != expected %x", got, expected)
	}
}

func TestVerifyManifestDigest_rejectsMismatch(t *testing.T) {
	a := blobRefWithDigest("src/main.c", []byte("x"))
	cas := &gen.CasDescriptor{
		ManifestDigest: bytes.Repeat([]byte{0xff}, 32), // wrong
		Blobs:          []*gen.BlobRef{a},
	}
	_, err := verifyManifestDigest(cas)
	if err == nil {
		t.Fatal("expected mismatch error")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("got code %v, want InvalidArgument", status.Code(err))
	}
}

func TestVerifyManifestDigest_rejectsBadDigestLength(t *testing.T) {
	cas := &gen.CasDescriptor{
		ManifestDigest: bytes.Repeat([]byte{0xaa}, 16), // too short
		Blobs:          nil,
	}
	_, err := verifyManifestDigest(cas)
	if err == nil || status.Code(err) != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument; got %v", err)
	}
}

// validateBlobPath tests cover the path-traversal defence layer.
func TestValidateBlobPath_acceptsCleanRelative(t *testing.T) {
	for _, p := range []string{"main.c", "src/main.c", "include/sub/foo.h"} {
		if err := validateBlobPath(p); err != nil {
			t.Errorf("validateBlobPath(%q) = %v, want nil", p, err)
		}
	}
}

func TestValidateBlobPath_rejectsTraversal(t *testing.T) {
	bad := []string{
		"",
		"..",
		"../escape",
		"src/../../etc/passwd",
		"src/sub/../../../etc/passwd",
		"/absolute",
		"src\\windows",
		"src/with\x00nul",
	}
	for _, p := range bad {
		if err := validateBlobPath(p); err == nil {
			t.Errorf("validateBlobPath(%q) accepted; want rejection", p)
		}
	}
}

func TestMaterializeCASBlobs_writesProjectFilesSkipsSystem(t *testing.T) {
	w := newSourceTestWorker(t)
	main := []byte("int main(void){return 0;}\n")
	header := []byte("#define X 1\n")

	mainBlob := blobRefWithDigest("src/main.c", main)
	headerBlob := blobRefWithDigest("include/x.h", header)
	sysBlob := blobRefWithDigest("/usr/include/stdio.h", []byte("/* system */"))

	// Pre-populate sourceStore with the two project blobs.
	for _, b := range []*gen.BlobRef{mainBlob, headerBlob} {
		if err := w.sourceStore.Put(b.Digest, blobData, contentFor(b)); err != nil {
			t.Fatalf("seed sourceStore: %v", err)
		}
	}

	srcDir := t.TempDir()
	cas := &gen.CasDescriptor{
		ManifestDigest: nil, // not checked by materializeCASBlobs
		Blobs:          []*gen.BlobRef{mainBlob, headerBlob, sysBlob},
	}
	if err := materializeCASBlobs(srcDir, cas, w.sourceStore); err != nil {
		t.Fatalf("materializeCASBlobs: %v", err)
	}

	// Project files landed.
	gotMain, err := os.ReadFile(filepath.Join(srcDir, "src", "main.c"))
	if err != nil {
		t.Fatalf("read main: %v", err)
	}
	if !bytes.Equal(gotMain, main) {
		t.Errorf("main.c bytes mismatch")
	}
	gotHdr, err := os.ReadFile(filepath.Join(srcDir, "include", "x.h"))
	if err != nil {
		t.Fatalf("read x.h: %v", err)
	}
	if !bytes.Equal(gotHdr, header) {
		t.Errorf("x.h bytes mismatch")
	}

	// System absolute path was NOT created under srcDir — the heuristic
	// is "in the image, don't ship."
	sysCandidate := filepath.Join(srcDir, "usr", "include", "stdio.h")
	if _, err := os.Stat(sysCandidate); !os.IsNotExist(err) {
		t.Errorf("system blob unexpectedly materialised at %s", sysCandidate)
	}
}

func TestMaterializeCASBlobs_missingBlobIsError(t *testing.T) {
	w := newSourceTestWorker(t)
	// Reference a blob digest we never seeded.
	b := blobRefWithDigest("src/main.c", []byte("x"))
	cas := &gen.CasDescriptor{Blobs: []*gen.BlobRef{b}}
	srcDir := t.TempDir()
	err := materializeCASBlobs(srcDir, cas, w.sourceStore)
	if err == nil {
		t.Fatal("expected error for missing blob; got nil")
	}
	if !strings.Contains(err.Error(), "unavailable") {
		t.Errorf("error %v should mention unavailability", err)
	}
}

func TestMaterializeCASBlobs_rejectsTraversalBlobPath(t *testing.T) {
	w := newSourceTestWorker(t)
	content := []byte("oops")
	b := &gen.BlobRef{Path: "../escape.txt", Digest: b3(content), Size: uint64(len(content))}
	if err := w.sourceStore.Put(b.Digest, blobData, content); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cas := &gen.CasDescriptor{Blobs: []*gen.BlobRef{b}}
	srcDir := t.TempDir()
	if err := materializeCASBlobs(srcDir, cas, w.sourceStore); err == nil {
		t.Fatal("expected traversal rejection; got nil")
	}
}

// contentFor recovers the source bytes that produced b.Digest. Used in
// the materialize test to seed sourceStore via Put, since we don't
// keep the raw bytes around after blobRefWithDigest constructs the
// digest. Cheap helper specific to test fixtures: we maintain a small
// in-test lookup table populated alongside the fixture builders.
//
// Implementation: re-derive from the test's known fixture contents.
// Tests pass the original bytes alongside the BlobRef when they need
// to seed; this helper just returns whatever's in a per-test map.
func contentFor(b *gen.BlobRef) []byte {
	if c, ok := blobContentFixture[string(b.Digest)]; ok {
		return c
	}
	return nil
}

// blobContentFixture is keyed by the blob digest bytes (as a string)
// and lets tests register content alongside BlobRef construction.
var blobContentFixture = map[string][]byte{}

func init() {
	// Re-derive fixtures at init so tests can call contentFor on any
	// BlobRef built via blobRefWithDigest. Test ergonomics only.
	register := func(content []byte) {
		blobContentFixture[string(b3(content))] = content
	}
	register([]byte("int main(void){return 0;}\n"))
	register([]byte("#define X 1\n"))
	register([]byte("/* system */"))
}
