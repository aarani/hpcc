// Package explain records per-compile outcome metadata so `hpcc
// explain <source>` can answer "why did my last compile not hit the
// cache." Phase 5 §5.3.
//
// On every compile the daemon writes a Record (one JSON file under
// the user's cache dir, keyed by sha256 of the source's abspath)
// describing the inputs the cache key was built from: compiler
// identity, canonical flags, source-content hash, per-header content
// hashes, image digest (if dispatched remotely), and the outcome
// (local_hit, remote, local_invoke, bypass, error).
//
// On a later compile of the same source path, comparing the new
// inputs against the prior record yields a structured Diff that
// names exactly what changed — "this header's bytes changed," "this
// flag was added," "the compiler binary was rebuilt." That diff is
// the user-facing payload `hpcc explain` prints.
//
// The Record schema is intentionally a superset of what the cache
// key cares about: a hit doesn't need any of this, but the *next*
// miss needs the hit's record to have something to compare against.
// So we write on every compile, not just on misses.
package explain

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// recordVersion is the on-disk schema version. Bump on any
// breaking change to Record's JSON shape; the store discards entries
// with a different version on Get so a downgrade can't surface
// garbage data.
const recordVersion = 1

// Outcome is what the daemon did with this compile. Stable strings so
// the on-disk records remain readable across versions.
type Outcome string

const (
	OutcomeLocalHit    Outcome = "local_hit"
	OutcomeRemote      Outcome = "remote"
	OutcomeLocalInvoke Outcome = "local_invoke"
	OutcomeBypass      Outcome = "bypass"
	OutcomeError       Outcome = "error"
)

// Record is one compile's worth of explain-relevant metadata.
//
// All hash fields are hex-encoded SHA-256 (chosen for human
// readability and for the ability to grep records by digest; the
// cache itself uses BLAKE3 for its key, recorded verbatim in
// CacheKey).
type Record struct {
	Version   int       `json:"version"`
	Timestamp time.Time `json:"ts"`

	SourcePath string  `json:"source_path"`           // abspath
	OutputPath string  `json:"output_path,omitempty"` // abspath; empty when none
	Outcome    Outcome `json:"outcome"`

	// CacheKey is the full hex-encoded cache key the daemon computed
	// for this compile. Recorded so the user can correlate an
	// explain record with the corresponding cache entry.
	CacheKey string `json:"cache_key,omitempty"`

	// Sub-hashes the diff engine pulls apart on the next miss. Any
	// of these may be empty when the daemon couldn't compute it
	// (bypass paths skip work, remote-dispatched compiles don't
	// always materialise headers locally, etc.).
	CompilerIdentityHash string            `json:"compiler_identity_hash,omitempty"`
	FlagsHash            string            `json:"flags_hash,omitempty"`
	SourceContentHash    string            `json:"source_content_hash,omitempty"`
	HeaderHashes         map[string]string `json:"header_hashes,omitempty"` // header path → sha256
	ImageDigest          string            `json:"image_digest,omitempty"`

	// Human-readable context for `hpcc explain`'s output.
	CompilerBinary string   `json:"compiler_binary,omitempty"`
	Args           []string `json:"args,omitempty"`

	// Diffs is the set of named reasons this compile's cache key
	// differs from the previous compile's cache key for the same
	// source. Computed at write time by the daemon (where both
	// records are in hand) and embedded here so `hpcc explain` is
	// a pure read of the latest record — no need to retain the
	// prior. Empty when this is the first compile for the source,
	// when the outcome was a hit, or when nothing observable
	// changed.
	Diffs []Diff `json:"diffs,omitempty"`
}

// DiffKind enumerates the categories `hpcc explain` reports. Stable
// strings; matched on by the CLI's renderer.
type DiffKind string

const (
	DiffCompiler DiffKind = "compiler"
	DiffFlags    DiffKind = "flags"
	DiffSource   DiffKind = "source"
	DiffHeader   DiffKind = "header"
	DiffImage    DiffKind = "image"
)

// Diff is one named reason the cache key changed. For headers, Path
// names the specific file; Before/After are hex hashes (or "" when
// the header was added or removed).
type Diff struct {
	Kind   DiffKind `json:"kind"`
	Path   string   `json:"path,omitempty"` // header path; empty for non-DiffHeader
	Before string   `json:"before,omitempty"`
	After  string   `json:"after,omitempty"`
}

// CompareTo returns the structured set of differences between r (the
// new compile) and prior (the previous compile for the same source).
// nil prior returns nil — the caller treats that as "no prior
// record; this source has never been compiled by this daemon."
//
// Returned diffs are sorted (compiler → flags → source → headers →
// image) for stable rendering.
func (r *Record) CompareTo(prior *Record) []Diff {
	if prior == nil {
		return nil
	}
	var out []Diff
	if r.CompilerIdentityHash != "" && prior.CompilerIdentityHash != "" &&
		r.CompilerIdentityHash != prior.CompilerIdentityHash {
		out = append(out, Diff{
			Kind: DiffCompiler, Before: prior.CompilerIdentityHash, After: r.CompilerIdentityHash,
		})
	}
	if r.FlagsHash != "" && prior.FlagsHash != "" && r.FlagsHash != prior.FlagsHash {
		out = append(out, Diff{
			Kind: DiffFlags, Before: prior.FlagsHash, After: r.FlagsHash,
		})
	}
	if r.SourceContentHash != "" && prior.SourceContentHash != "" &&
		r.SourceContentHash != prior.SourceContentHash {
		out = append(out, Diff{
			Kind: DiffSource, Before: prior.SourceContentHash, After: r.SourceContentHash,
		})
	}
	// Header diffs: walk both maps so added/removed entries show up
	// alongside changed ones. Sort the union for deterministic order.
	seen := map[string]struct{}{}
	for p := range r.HeaderHashes {
		seen[p] = struct{}{}
	}
	for p := range prior.HeaderHashes {
		seen[p] = struct{}{}
	}
	keys := make([]string, 0, len(seen))
	for p := range seen {
		keys = append(keys, p)
	}
	sort.Strings(keys)
	for _, p := range keys {
		now, hadNow := r.HeaderHashes[p]
		then, hadThen := prior.HeaderHashes[p]
		switch {
		case hadNow && hadThen && now != then:
			out = append(out, Diff{Kind: DiffHeader, Path: p, Before: then, After: now})
		case hadNow && !hadThen:
			out = append(out, Diff{Kind: DiffHeader, Path: p, After: now})
		case !hadNow && hadThen:
			out = append(out, Diff{Kind: DiffHeader, Path: p, Before: then})
		}
	}
	if r.ImageDigest != prior.ImageDigest && (r.ImageDigest != "" || prior.ImageDigest != "") {
		out = append(out, Diff{
			Kind: DiffImage, Before: prior.ImageDigest, After: r.ImageDigest,
		})
	}
	return out
}

// HashSourcePath returns the on-disk record file name for sourcePath.
// Exported so the CLI can resolve the file without instantiating a
// Store (useful for `hpcc explain --record-path foo.c`-style
// diagnostics if we ever add one).
func HashSourcePath(sourcePath string) string {
	abs, err := filepath.Abs(sourcePath)
	if err != nil {
		abs = sourcePath
	}
	sum := sha256.Sum256([]byte(abs))
	return hex.EncodeToString(sum[:]) + ".json"
}

// HashBytes returns hex-encoded SHA-256(data). Helper so call sites
// don't reach into crypto/sha256 directly.
func HashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// HashFile reads p and returns its hex SHA-256. Returns "" + an error
// the caller is expected to log and move past — explain is
// best-effort; failing here must not break the compile.
func HashFile(p string) (string, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return HashBytes(b), nil
}
