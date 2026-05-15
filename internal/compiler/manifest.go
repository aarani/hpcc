package compiler

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/zeebo/blake3"
)

// projectMarkerFile is the filename `FindProjectRoot` walks up to
// find. Sibling pattern to `.git`, `.editorconfig`, `.tool-versions`:
// devs drop one at the logical project root and CAS-mode dispatch
// gains cross-developer cache hits because two checkouts at
// different absolute paths (`/home/alice/proj` vs
// `/home/bob/proj`) produce identical manifest digests.
const projectMarkerFile = ".hpcc"

// FindProjectRoot walks up the directory tree starting at startDir,
// returning the first directory that contains a `.hpcc` marker file.
// Returns empty string if no marker is found before reaching the
// filesystem root.
//
// Errors are swallowed silently — discovery is best-effort and the
// caller falls back to absolute paths. A non-existent or unreadable
// path is treated the same as "no marker found": cross-developer
// cache hits don't fire until the user adds the marker, but nothing
// breaks.
func FindProjectRoot(startDir string) string {
	dir, err := filepath.Abs(startDir)
	if err != nil {
		return ""
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, projectMarkerFile)); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// normalizeManifestPath returns p re-expressed relative to projectRoot
// when p lives inside the project tree, or unchanged (absolute) when
// it lives outside. The split is what gives CAS its two-tier
// path-handling model: project files travel with the source closure
// and materialize relative to a per-exec staging dir; system files
// (toolchain headers) stay at their original locations inside the
// container image and need no transport.
//
// An empty projectRoot disables normalization and is the right
// behaviour when no `.hpcc` marker is found.
func normalizeManifestPath(p, projectRoot string) string {
	if projectRoot == "" {
		return p
	}
	absP, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	rel, err := filepath.Rel(projectRoot, absP)
	if err != nil {
		return p
	}
	// filepath.Rel returns a cleaned relative path; ".." can only
	// appear as a leading segment if absP is outside projectRoot.
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return p
	}
	return rel
}

// BlobRef names a single file in a source-closure manifest by path
// and content digest. CAS-mode dispatch (plan/phase-4-distributed.md
// §4.5 / cas.md) ships
// these to the worker; the worker materializes the closure inside the
// VM by fetching each blob by digest and writing it to Path.
type BlobRef struct {
	Path   string   // path as seen by the compiler; not normalized here
	Digest [32]byte // BLAKE3-256 of the file's bytes
	Size   int64    // file size in bytes; advisory (worker re-checks on fetch)
}

// Manifest is the source-closure summary for a single compile. Digest
// is a stable function of the (path, content-digest) pairs of every
// blob; two Manifests with the same set of (path, content) pairs
// produce the same Digest regardless of FindDependencies' output
// ordering, because Blobs is sorted by Path before aggregation.
type Manifest struct {
	Digest [32]byte
	Blobs  []BlobRef
}

// BuildManifest runs the compiler's dependency discovery, hashes every
// file in the closure (inputs + headers), and returns a Manifest. The
// manifest digest is what CAS-mode cache keys mix in instead of
// preprocessed source bytes.
//
// Path handling: if `inv.Cwd` is set and a `.hpcc` marker file is
// found by walking up from it (see `FindProjectRoot`), paths under
// that root are rewritten relative to the root before being mixed
// into the manifest digest. Paths outside the project tree
// (typically toolchain headers under `/usr/include`, `/opt/...`)
// stay absolute. Files keyed by content digest hash the same bytes
// either way — only `BlobRef.Path` differs.
//
// No marker → no normalization → absolute paths. Cross-developer
// cache hits don't fire in that case, but the same-developer
// behaviour is preserved.
func BuildManifest(inv *Invocation, ctx *Context) (*Manifest, error) {
	if len(inv.Inputs) == 0 {
		return nil, fmt.Errorf("no input files in invocation")
	}

	deps, err := ctx.Compiler.FindDependencies(inv)
	if err != nil {
		return nil, fmt.Errorf("find dependencies: %w", err)
	}

	// Collect inputs + deps under unique paths. -M usually omits the
	// source itself for MSVC and includes it for GNU; we don't care
	// either way — dedupe and proceed.
	seen := make(map[string]struct{}, len(inv.Inputs)+len(deps))
	paths := make([]string, 0, len(inv.Inputs)+len(deps))
	for _, p := range inv.Inputs {
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		paths = append(paths, p)
	}
	for _, p := range deps {
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		paths = append(paths, p)
	}

	var projectRoot string
	if inv.Cwd != "" {
		projectRoot = FindProjectRoot(inv.Cwd)
	}

	blobs := make([]BlobRef, 0, len(paths))
	for _, p := range paths {
		ref, err := hashFileBlob(p)
		if err != nil {
			return nil, err
		}
		ref.Path = normalizeManifestPath(p, projectRoot)
		blobs = append(blobs, ref)
	}

	slices.SortFunc(blobs, func(a, b BlobRef) int {
		if a.Path < b.Path {
			return -1
		}
		if a.Path > b.Path {
			return 1
		}
		return 0
	})

	return &Manifest{Digest: AggregateManifestDigest(blobs), Blobs: blobs}, nil
}

// hashFileBlob streams a file through BLAKE3 and returns its BlobRef.
// Streaming avoids loading large generated headers fully into memory
// just to hash them.
func hashFileBlob(path string) (BlobRef, error) {
	f, err := os.Open(path)
	if err != nil {
		return BlobRef{}, fmt.Errorf("open %q: %w", path, err)
	}
	defer f.Close()

	h := blake3.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return BlobRef{}, fmt.Errorf("read %q: %w", path, err)
	}

	var d [32]byte
	copy(d[:], h.Sum(nil))
	return BlobRef{Path: path, Digest: d, Size: n}, nil
}

// AggregateManifestDigest computes the manifest digest from a
// path-sorted blob list. Encoding per blob:
//
//	u64-BE(len(path)) || path || digest[32]
//
// Length-prefixing the path keeps concatenation unambiguous (two
// distinct (path, digest) sequences cannot collide via boundary
// shifting). Big-endian to match the convention in Invocation.CacheKey.
//
// Callers must sort blobs by Path before calling. Exported so the
// worker can re-verify a client-supplied manifest using the exact
// same algorithm BuildManifest uses on the client side; drift between
// the two would silently break cross-developer cache hits.
func AggregateManifestDigest(blobs []BlobRef) [32]byte {
	h := blake3.New()
	var lenbuf [8]byte
	for _, b := range blobs {
		binary.BigEndian.PutUint64(lenbuf[:], uint64(len(b.Path)))
		h.Write(lenbuf[:])
		h.Write([]byte(b.Path))
		h.Write(b.Digest[:])
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}
