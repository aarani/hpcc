package daemon

import (
	"encoding/hex"
	"path/filepath"

	"github.com/aarani/hpcc/internal/compiler"
	"github.com/aarani/hpcc/internal/explain"
	"go.uber.org/zap"
)

// recordExplain builds and persists one explain.Record for the just-
// completed compile. Best-effort — every error is logged at debug and
// swallowed; the compile result is already on its way back to the
// client and explain is opportunistic metadata, not part of the
// contract.
//
// Sub-hash sources:
//
//   - Compiler identity: compiler.Compiler.Identity() bytes,
//     SHA-256-hashed for the on-disk record (Identity itself is
//     already a stable digest of "compiler path + binary bytes" but
//     not necessarily a fixed-width hex string).
//   - Flags: compiler.CacheKeyFlagsBytes(inv), the same canonical
//     encoding the cache key consumes — so a "flags changed" diff
//     here lines up exactly with what the cache key saw.
//   - Source content: the first input file's bytes.
//   - Headers: lifted straight from the CAS source-closure manifest
//     when one was built (the default; CAS-mode local-cache lookup
//     always builds one). Manifest blobs carry per-file BLAKE3
//     digests already, so explain pays nothing for them — no extra
//     preprocess pass, no per-flag dependence. Records mark
//     `header_hash_algo = "blake3"` so the diff renderer stays
//     algorithm-agnostic and a future SHA-256 source can coexist
//     without colliding.
//
// manifest may be nil; the worker's ManifestDigest /
// PreprocessedDigest short-circuits and the PREPROCESSED-mode local
// path don't produce one, and the bypass path doesn't go through
// cache-key plumbing at all. Header tracking is best-effort and gets
// skipped in those cases.
func (d *DefaultDaemon) recordExplain(
	ctx *compiler.Context,
	inv *compiler.Invocation,
	cacheKey string,
	outcome explain.Outcome,
	manifest *compiler.Manifest,
	imageDigest string,
) {
	if d.explainStore == nil {
		return
	}
	if len(inv.Inputs) == 0 {
		return
	}
	srcAbs := absOrSelf(inv.Inputs[0], inv.Cwd)
	rec := &explain.Record{
		SourcePath:     srcAbs,
		OutputPath:     absOrSelf(inv.Output, inv.Cwd),
		Outcome:        outcome,
		CacheKey:       cacheKey,
		ImageDigest:    imageDigest,
		Args:           inv.RawArgs,
		CompilerBinary: ctx.Compiler.Name(),
	}

	if id, err := ctx.Compiler.Identity(); err == nil {
		rec.CompilerIdentityHash = explain.HashBytes(id)
	}
	rec.FlagsHash = explain.HashBytes(compiler.CacheKeyFlagsBytes(inv))
	if h, err := explain.HashFile(srcAbs); err == nil {
		rec.SourceContentHash = h
	}
	if manifest != nil {
		rec.HeaderHashes, rec.HeaderHashAlgo = headerHashesFromManifest(manifest, srcAbs, inv.Cwd)
	}

	// Compute the structured diff against the prior record while we
	// still have access to both — the Put below overwrites the
	// prior. `hpcc explain` then renders rec.Diffs straight from the
	// latest record without needing two-record history.
	//
	// Only attach diffs on a miss; for a cache hit the entry-on-disk
	// was already used to serve, and the diff would just be "no
	// change since last hit" which isn't actionable.
	prior, getErr := d.explainStore.Get(srcAbs)
	if getErr != nil {
		zap.S().Debugf("daemon: explain.Get %q: %v", srcAbs, getErr)
	} else if prior != nil && outcome != explain.OutcomeLocalHit && outcome != explain.OutcomeRemote {
		rec.Diffs = rec.CompareTo(prior)
	}

	if err := d.explainStore.Put(rec); err != nil {
		zap.S().Debugf("daemon: explain.Put %q: %v", srcAbs, err)
	}
}

// headerHashesFromManifest turns the closure manifest's blob list
// into the explain record's header-hash map. The source file itself
// is excluded (it lives in record.SourceContentHash); everything else
// in the closure is treated as a header.
//
// Manifest paths are as-spelled (potentially project-relative when a
// `.hpcc` marker normalises them, otherwise absolute). For diff
// stability we resolve each to an absolute path against inv.Cwd —
// same convention as record.SourcePath — so two builds of the same
// TU from different cwds collapse onto comparable header keys.
//
// Returned algorithm string is "blake3" since manifest digests are
// BLAKE3-256; baked into the record so a later SHA-256 source can
// coexist without the diff engine mixing algorithms.
func headerHashesFromManifest(m *compiler.Manifest, srcAbs, cwd string) (map[string]string, string) {
	if m == nil || len(m.Blobs) == 0 {
		return nil, ""
	}
	out := make(map[string]string, len(m.Blobs))
	for _, b := range m.Blobs {
		abs := absOrSelf(b.Path, cwd)
		if abs == srcAbs {
			continue
		}
		out[abs] = hex.EncodeToString(b.Digest[:])
	}
	if len(out) == 0 {
		return nil, ""
	}
	return out, "blake3"
}

// absOrSelf resolves p against cwd when p is relative and cwd is non-
// empty. Returns p as-is on failure or when both inputs are unusable.
// Daemon callers always pass abs paths or paths that need cwd join,
// so this is the right default.
func absOrSelf(p, cwd string) string {
	if p == "" {
		return p
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	if cwd == "" {
		abs, err := filepath.Abs(p)
		if err != nil {
			return p
		}
		return abs
	}
	return filepath.Clean(filepath.Join(cwd, p))
}
