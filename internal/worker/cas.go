package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/zeebo/blake3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/aarani/hpcc/internal/compiler"
	"github.com/aarani/hpcc/internal/protocol/gen"
)

// ProbeCompileCache is the cache-short-circuit for CAS-mode dispatch.
// Client sends just the manifest digest + args + image; the worker
// derives the compile cache key (same formula the worker uses when it
// stores a fresh compile) and probes the compile cache. Hit returns
// the cached artifact; miss tells the client to proceed with the
// upload dance. Mirrors Bazel's ActionCache.GetActionResult.
//
// Paranoid-mode invariant. Client sends `args`, not a pre-baked
// flag-key. The worker re-parses and computes flag-key itself, so the
// client cannot influence what cache_key the worker probes. A fake
// manifest_digest produces a wasted probe (miss → client falls
// through to the upload dance, worker materializes for real and
// computes the *real* cache_key on the way out); the probe is not a
// write path and cannot poison cache content.
func (w *Worker) ProbeCompileCache(ctx context.Context, req *gen.CompileProbe) (*gen.ProbeResponse, error) {
	if err := w.validateSchedulerToken(req.SchedulerToken, req.TenantId, req.ImageDigest); err != nil {
		return nil, fmt.Errorf("validate token: %w", err)
	}
	if len(req.ManifestDigest) != 32 {
		return nil, status.Error(codes.InvalidArgument, "manifest_digest must be 32 bytes")
	}
	if len(req.Args) == 0 {
		return nil, status.Error(codes.InvalidArgument, "args must not be empty")
	}

	c, err := compiler.Detect(req.Args[0])
	if err != nil {
		return nil, fmt.Errorf("detect compiler %q: %w", req.Args[0], err)
	}
	inv, err := c.Parse(req.Args[1:])
	if err != nil {
		return nil, fmt.Errorf("parse args: %w", err)
	}

	// Bind the client-supplied manifest digest to the invocation so
	// CacheKey short-circuits to the manifest-digest path (no
	// preprocessor invocation, no dep walk). The digest is *not*
	// re-verified here — the probe is read-only and a fake digest
	// only costs the client a wasted round trip.
	var md [32]byte
	copy(md[:], req.ManifestDigest)
	inv.ManifestDigest = &md

	cctx := w.compileContext(c, req.ImageDigest)

	hit, err := cctx.Cache.Lookup(inv)
	if err != nil || hit == nil {
		// Lookup errors are non-fatal — treat as miss. The client
		// will fall through to the upload dance and the next
		// Compile RPC will produce a fresh entry.
		return probeMiss(), nil
	}

	cacheKey, _ := inv.ComputeHash(cctx)
	resp := &gen.CompileResponse{
		Stdout:   hit.Stdout,
		Stderr:   hit.Stderr,
		ExitCode: int32(hit.ExitCode),
		CacheKey: &cacheKey,
	}
	if len(hit.Output) > 0 {
		resp.OutputArtifact = hit.Output
	}
	return &gen.ProbeResponse{Result: &gen.ProbeResponse_Hit{Hit: resp}}, nil
}

func probeMiss() *gen.ProbeResponse {
	return &gen.ProbeResponse{Result: &gen.ProbeResponse_Miss{Miss: &gen.ProbeMiss{}}}
}

// verifyManifestDigest recomputes the manifest digest from a
// CasDescriptor's BlobRefs and compares it to the digest the client
// claims. Returns the verified 32-byte digest on success, or an
// error on mismatch / malformed input. Defends against a client
// shipping a digest that doesn't match its blob list — without this,
// the worker would store compile outputs under a key the client
// chose freely.
func verifyManifestDigest(cas *gen.CasDescriptor) ([32]byte, error) {
	var zero [32]byte
	if len(cas.ManifestDigest) != 32 {
		return zero, status.Error(codes.InvalidArgument, "manifest_digest must be 32 bytes")
	}
	blobs := make([]compiler.BlobRef, 0, len(cas.Blobs))
	for _, b := range cas.Blobs {
		if len(b.Digest) != 32 {
			return zero, status.Errorf(codes.InvalidArgument, "blob digest for %q is not 32 bytes", b.Path)
		}
		var d [32]byte
		copy(d[:], b.Digest)
		blobs = append(blobs, compiler.BlobRef{Path: b.Path, Digest: d, Size: int64(b.Size)})
	}
	sort.Slice(blobs, func(i, j int) bool { return blobs[i].Path < blobs[j].Path })
	recomputed := compiler.AggregateManifestDigest(blobs)
	if !bytes.Equal(recomputed[:], cas.ManifestDigest) {
		return zero, status.Error(codes.InvalidArgument, "manifest digest does not match (path, blob digest) list")
	}
	return recomputed, nil
}

// maxSourceBlobBytes caps how much one upload-side blob can buffer
// while we re-hash it. Source files in real builds are well under
// this; the cap exists so a hostile or runaway client can't OOM the
// worker with a single multi-gigabyte stream. Tunable later if
// pathological generated headers turn up.
const maxSourceBlobBytes = 256 << 20 // 256 MiB

// FindMissingBlobs is the worker's CAS-mode "what do I need?" probe.
// Client streams the digests it intends to upload; worker streams
// back the subset that aren't already present locally. 1:1 bidi so
// the client can pipeline this with UploadBlobs without waiting for
// the full probe set.
//
// The lookup is a single layer (local-disk Has). There is no
// in-memory confirmed-set and no S3 probing — source blobs are
// worker-local-ephemeral by design (docs/cas.md Step 3/4), so
// nothing further to consult. If profiling later shows os.Stat
// overhead dominating, an LRU in front of Has is the place to add
// one.
func (w *Worker) FindMissingBlobs(stream gen.WorkerService_FindMissingBlobsServer) error {
	if w.sourceStore == nil {
		return status.Error(codes.FailedPrecondition, "worker has no disk cache configured; CAS mode unavailable")
	}
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if len(req.Digest) == 0 {
			return status.Error(codes.InvalidArgument, "blob digest must not be empty")
		}
		has, err := w.sourceStore.Has(req.Digest)
		if err != nil {
			return status.Errorf(codes.Internal, "source store has: %v", err)
		}
		if has {
			continue
		}
		if err := stream.Send(req); err != nil {
			return err
		}
	}
}

// blobInProgress is the per-blob receive state inside UploadBlobs.
// Headers open a new instance; data chunks fold into it; the next
// header or stream EOF closes it (commit on hash match, reject on
// mismatch / oversize / bad header).
type blobInProgress struct {
	header   *gen.BlobDigest
	hasher   *blake3.Hasher
	buf      bytes.Buffer
	rejected bool // true → drain remaining data chunks and report in UploadResult
}

// UploadBlobs accepts a client-stream of BlobChunk messages and
// commits verified blobs to the worker-local source store. Each blob
// is framed as a header (carrying the client-claimed digest) followed
// by zero or more data chunks. The worker re-hashes the bytes with
// BLAKE3 as they arrive; on a hash match the bytes land in
// sourceStore under the *recomputed* digest (so the client cannot
// influence the key under which an entry is stored — paranoid-mode
// invariant). Mismatches, malformed headers, and oversize blobs are
// reported in UploadResult.rejected_digests without aborting the
// stream — the client may have other blobs to upload that should
// still land.
//
// Bytes are buffered in memory per-blob (capped at maxSourceBlobBytes)
// rather than streamed to disk. Source files in real builds sit well
// under that cap; if profiling later shows RSS pressure under
// concurrent large-blob uploads, swap in a streaming write via a
// future Store.OpenWriter primitive.
//
// No S3 write-through: source blobs are worker-local-ephemeral
// (docs/cas.md Step 4). The client is the source of truth and
// re-uploads on cache miss.
func (w *Worker) UploadBlobs(stream gen.WorkerService_UploadBlobsServer) error {
	if w.sourceStore == nil {
		return status.Error(codes.FailedPrecondition, "worker has no disk cache configured; CAS mode unavailable")
	}

	result := &gen.UploadResult{}
	var active *blobInProgress

	// commit finalizes the current blob (if any) into the result. On a
	// hash match the bytes land in the source store; otherwise the
	// claimed digest is recorded as rejected.
	commit := func() error {
		if active == nil {
			return nil
		}
		defer func() { active = nil }()

		if active.rejected {
			result.RejectedDigests = append(result.RejectedDigests, active.header.Digest)
			return nil
		}

		var recomputed [32]byte
		copy(recomputed[:], active.hasher.Sum(nil))
		if !bytes.Equal(recomputed[:], active.header.Digest) {
			result.RejectedDigests = append(result.RejectedDigests, active.header.Digest)
			return nil
		}

		// Store under the recomputed digest — never the client-supplied
		// one, even though they're equal here. Keeps the invariant
		// "worker stores by hash it computed itself" textually obvious.
		if err := w.sourceStore.Put(recomputed[:], blobData, active.buf.Bytes()); err != nil {
			return status.Errorf(codes.Internal, "store source blob: %v", err)
		}
		result.BlobsReceived++
		result.BytesReceived += uint64(active.buf.Len())
		return nil
	}

	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			if err := commit(); err != nil {
				return err
			}
			return stream.SendAndClose(result)
		}
		if err != nil {
			return err
		}

		switch body := msg.Body.(type) {
		case *gen.BlobChunk_Header:
			// New blob: commit the previous one (if any), then open
			// a fresh receive state.
			if err := commit(); err != nil {
				return err
			}
			h := body.Header
			if h == nil || len(h.Digest) != 32 {
				// Malformed header — return an RPC error rather than
				// recording a "rejected digest" because we don't have
				// a 32-byte digest to put in the slot. Clients
				// shipping malformed protobuf is a bug, not a
				// content-level rejection.
				return status.Error(codes.InvalidArgument, "blob header missing 32-byte digest")
			}
			active = &blobInProgress{header: h, hasher: blake3.New()}

		case *gen.BlobChunk_Data:
			if active == nil {
				return status.Error(codes.InvalidArgument, "data chunk before any blob header")
			}
			if active.rejected {
				// We've already decided to discard this blob; drain
				// the remaining data chunks until the next header.
				continue
			}
			if int64(active.buf.Len())+int64(len(body.Data)) > maxSourceBlobBytes {
				active.rejected = true
				active.buf.Reset() // free buffered bytes immediately
				continue
			}
			active.hasher.Write(body.Data)
			active.buf.Write(body.Data)

		default:
			return status.Error(codes.InvalidArgument, fmt.Sprintf("unknown BlobChunk body %T", body))
		}
	}
}
