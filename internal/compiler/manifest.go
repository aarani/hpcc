package compiler

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
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

	// For .S/.s inputs, walk the GAS .include closure and capture
	// every .incbin / .include path. `gcc -M` is preprocessor-only —
	// it never sees these directives, which the assembler resolves
	// at assemble time. The walk:
	//   - scans the .S itself for direct directives.
	//   - scans every cpp-discovered header (a header pulled in via
	//     #include can itself contain .incbin; the preprocessed
	//     output gas sees inlines it).
	//   - recurses through nested .include chains (the .include'd
	//     file is itself assembly source).
	// Cycles are bounded by an internal seen-set. Without this, the
	// worker materializes only the .S and gas fails at assemble
	// time with "file not found" — see usr/initramfs_data.S,
	// arch/x86/realmode/rmpiggy.S, VDSO piggies, etc.
	for _, in := range inv.Inputs {
		if !isAssemblySource(in) {
			continue
		}
		rootAbs := in
		if !filepath.IsAbs(in) && inv.Cwd != "" {
			rootAbs = filepath.Join(inv.Cwd, in)
		}
		deps = append(deps, scanAssemblyClosure(rootAbs, deps, inv.Cwd)...)
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
		// FindDependencies (gcc -M) returns paths AS-WRITTEN: kernel-
		// style builds produce relative paths like
		// "include/generated/autoconf.h" because the kernel invokes
		// gcc with relative -I and -include flags. We need to open
		// these relative to inv.Cwd (the *client's* cwd where make
		// is running), not the daemon's process cwd (which is
		// wherever the user started `hpcc start`, often unrelated
		// to the build). Without this, every kernel TU's
		// BuildManifest errors with ENOENT and the build silently
		// falls back to local compile on every cacheable TU.
		full := p
		if !filepath.IsAbs(p) && inv.Cwd != "" {
			full = filepath.Join(inv.Cwd, p)
		}
		ref, err := hashFileBlob(full)
		if err != nil {
			return nil, err
		}
		ref.Path = normalizeManifestPath(full, projectRoot)
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

// isAssemblySource reports whether the path looks like a GAS source
// (.s lowercase = plain assembly, .S uppercase = with C preprocessor).
// Mirrors the extension check in Invocation.isAssembly but kept local
// to keep BuildManifest from depending on Invocation-method internals.
func isAssemblySource(p string) bool {
	switch filepath.Ext(p) {
	case ".s", ".S":
		return true
	}
	return false
}

// assemblyIncludeRe matches GAS .incbin and .include directives:
//
//	.incbin "path"
//	.include "path"
//
// One per line at any indentation. Capture 1 is the directive name
// ("incbin" or "include"); capture 2 is the quoted path. Anchored
// to start-of-line with MULTILINE so // and /* */ comments containing
// the literal text "incbin" don't false-positive — GAS line comments
// start with ';' or '#' depending on architecture, but those can't
// precede a directive at column 0.
var assemblyIncludeRe = regexp.MustCompile(`(?m)^[ \t]*\.(incbin|include)[ \t]+"([^"]+)"`)

// assemblyIncludes is the categorised output of scanAssemblyIncludes.
// Callers treat the two slices differently:
//   - Incbin paths point at binary blobs the assembler embeds verbatim;
//     they're inputs to the build but never themselves contain GAS
//     directives, so we hash them and stop.
//   - Include paths point at assembly source pulled in textually; those
//     can themselves contain .include / .incbin and participate in
//     scanAssemblyClosure's recursion.
type assemblyIncludes struct {
	Incbin  []string
	Include []string
}

// scanAssemblyIncludes parses `.incbin "path"` and `.include "path"`
// directives out of a GAS source file. Paths are returned verbatim as
// they appear in the source — gas resolves them against its working
// directory (and -I paths, which we don't currently parse), so the
// caller joins them against inv.Cwd before hashing or further
// scanning.
//
// Read failures (file missing, permission denied) are returned as
// errors so the caller can decide whether to treat them as fatal.
// BuildManifest treats them as fatal for the input .S itself but
// silent-skip for cpp-discovered deps (best-effort: not every header
// is a GAS source).
func scanAssemblyIncludes(filePath string) (assemblyIncludes, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return assemblyIncludes{}, err
	}
	var out assemblyIncludes
	for _, m := range assemblyIncludeRe.FindAllSubmatch(data, -1) {
		path := string(m[2])
		switch string(m[1]) {
		case "incbin":
			out.Incbin = append(out.Incbin, path)
		case "include":
			out.Include = append(out.Include, path)
		}
	}
	return out, nil
}

// scanAssemblyClosure walks the GAS .include closure rooted at the
// input .S plus the supplied cpp-discovered deps, returning every
// .incbin and .include path the assembler will need at assemble time.
//
// Three classes the bare `gcc -M` dep list misses, all handled here:
//  1. `.incbin` / `.include` directly in the .S source (the input).
//  2. `.incbin` / `.include` in a header pulled in by cpp's
//     `#include` (the header is in cppDeps; we scan it).
//  3. `.incbin` / `.include` reachable through any number of nested
//     `.include` chains (we recurse).
//
// Paths are returned in discovery order, deduped by resolved-to-cwd
// path. A `seen` set protects against cycles (.include "a"; .include
// "b"; where "b" .include's "a"). Read failures during the walk are
// silently skipped — the cpp-dep list may contain non-assembly files
// (.h with no GAS directives, generated files), and a real "missing
// file" surfaces immediately when hashFileBlob runs against the
// discovered path during manifest construction.
func scanAssemblyClosure(rootAbs string, cppDeps []string, cwd string) []string {
	resolve := func(p string) string {
		if filepath.IsAbs(p) || cwd == "" {
			return p
		}
		return filepath.Join(cwd, p)
	}

	scanned := map[string]bool{} // resolved paths we've already opened
	emitted := map[string]bool{} // resolved paths already in result
	var queue []string           // resolved paths still to scan
	var out []string             // original (as-spelled-in-source) paths

	enqueue := func(abs string) {
		if scanned[abs] {
			return
		}
		scanned[abs] = true
		queue = append(queue, abs)
	}

	// Seed with the root .S itself and every cpp-discovered dep.
	// Headers that don't contain GAS directives just produce no
	// matches; the regex scan is cheap.
	enqueue(rootAbs)
	for _, d := range cppDeps {
		enqueue(resolve(d))
	}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		incs, err := scanAssemblyIncludes(cur)
		if err != nil {
			// Best-effort. Real missing-file errors will surface
			// at hashFileBlob time when the discovered path is
			// loaded into the manifest.
			continue
		}
		for _, p := range incs.Incbin {
			abs := resolve(p)
			if emitted[abs] {
				continue
			}
			emitted[abs] = true
			out = append(out, p)
			// Don't recurse into binary blobs.
		}
		for _, p := range incs.Include {
			abs := resolve(p)
			if !emitted[abs] {
				emitted[abs] = true
				out = append(out, p)
			}
			// Recurse: the .include'd file is itself assembly
			// source that can carry more directives.
			enqueue(abs)
		}
	}
	return out
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
