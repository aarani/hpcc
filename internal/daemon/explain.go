package daemon

import (
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
// sub-hashes are derived as follows:
//
//   - Compiler identity: compiler.Compiler.Identity() bytes,
//     SHA-256-hashed for the on-disk record (Identity itself is
//     already a stable digest of "compiler path + binary bytes" but
//     not necessarily a fixed-width hex string).
//   - Flags: compiler.CacheKeyFlagsBytes(inv), the same canonical
//     encoding the cache key consumes — so a "flags changed" diff
//     here lines up exactly with what the cache key saw.
//   - Source content: the first input file's bytes.
//   - Headers: parsed from the .d file the compile just produced
//     (whether locally or shipped back as an extra by the worker).
//
// cacheKey is the daemon's already-computed hex digest, or "" when
// we never computed one (bypass path).
func (d *DefaultDaemon) recordExplain(
	ctx *compiler.Context,
	inv *compiler.Invocation,
	cacheKey string,
	outcome explain.Outcome,
	result *compiler.InvocationResult,
	imageDigest string,
) {
	if d.explainStore == nil {
		return
	}
	if len(inv.Inputs) == 0 {
		return
	}
	// Absolute paths so two builds of the same source from
	// different cwds collapse onto one record (and `hpcc explain
	// ./foo.c` from either directory finds it).
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
	rec.HeaderHashes = collectHeaderHashes(inv, result)

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

// collectHeaderHashes finds the .d file the compiler produced and
// hashes each header it lists. Returns nil if no .d file is available
// (bypass paths, MSVC, or a TU with no -MMD on the cmdline).
//
// Two sources:
//   - result.Extras: dispatched compiles ship the .d back as a side
//     output. The bytes are in memory; parse directly.
//   - Local invoke: the .d file lives on disk. The daemon's main
//     flow already calls CollectDepEmissionExtras which reads it
//     into result.Extras, so this code path subsumes both.
//
// Header paths in the .d are resolved against inv.Cwd before
// hashing, so "../include/foo.h" finds the right file.
func collectHeaderHashes(inv *compiler.Invocation, result *compiler.InvocationResult) map[string]string {
	if result == nil || len(result.Extras) == 0 {
		return nil
	}
	out := map[string]string{}
	for _, content := range result.Extras {
		// .d files are short; skip the "is this a .d" classification
		// and just try to parse — ParseDepFile returns nil on
		// non-Make-rule content.
		deps := explain.ParseDepFile(content)
		// First dep is the source itself; skip it. Subsequent are
		// headers. Some compilers emit the source as a later entry
		// — keep the simple "skip srcAbs match" rule that handles
		// both layouts.
		srcAbs := absOrSelf(inv.Inputs[0], inv.Cwd)
		for _, dep := range deps {
			depAbs := absOrSelf(dep, inv.Cwd)
			if depAbs == srcAbs {
				continue
			}
			if _, ok := out[depAbs]; ok {
				continue
			}
			h, err := explain.HashFile(depAbs)
			if err != nil {
				continue
			}
			out[depAbs] = h
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
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
