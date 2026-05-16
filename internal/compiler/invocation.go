package compiler

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/aarani/hpcc/internal/enum"
	"github.com/zeebo/blake3"
)

// Invocation is the parsed form of one compile command. It is pure data,
// produced by a parser and consumed by a Compiler implementation.
type Invocation struct {
	Mode enum.InvocationMode

	Inputs []string // positional input files (.c/.cpp/.o/.a/...)
	Output string   // -o value, if any

	Includes       []string          // -I
	SystemIncludes []string          // -isystem, -iquote
	Defines        map[string]string // -D NAME[=VALUE]
	Undefines      []string          // -U
	Libraries      []string          // -l
	LibraryDirs    []string          // -L
	Std            string            // -std=...
	Optim          string            // -O...
	Debug          string            // -g... ("" if absent, "0"/"2"/etc otherwise)
	Warnings       []string          // -W... (e.g. "all", "no-foo")
	Features       []string          // -f... (e.g. "PIC", "no-rtti")
	Machine        []string          // -m... (e.g. "avx2", "arch=x86-64")
	Language       string            // -x value (forced language)

	// Passthrough holds flags we recognize but do nothing structured with —
	// linker/assembler/preprocessor passthrough, dep-info flags, -include, etc.
	Passthrough []string

	// Unknown holds flags that started with "-" but didn't match any spec.
	// They must be re-emitted verbatim when invoking the real compiler.
	Unknown []string

	// RawArgs is the argv we were given, after @file expansion.
	RawArgs []string

	// Cwd is the working directory the compiler should run from.
	// Empty means "inherit hpcc's own cwd" — correct for the in-process
	// `hpcc wrap` path (the wrapper is invoked from the right place by
	// make/etc.). The daemon sets this to the client's cwd so spawned
	// gcc/cl invocations resolve joined-form `-Iinclude`, auto-derived
	// depfile paths, and other relative arguments the way they would
	// if the user had run the compiler directly. NOT part of CacheKey
	// — only file content matters, not where it was compiled from.
	Cwd string

	// PreprocessedDigest, if non-nil, is the BLAKE3-256 of the
	// already-preprocessed source bytes. CacheKey treats it as a
	// substitute for running the preprocessor: skip FindDependencies,
	// skip Preprocess, mix this digest in directly. Set by the worker
	// for PREPROCESSED-mode requests where the client shipped
	// preprocessed bytes inline; nil otherwise.
	PreprocessedDigest *[32]byte

	// ManifestDigest, if non-nil, is the BLAKE3-256 of a source-closure
	// manifest (see Manifest.Digest in manifest.go). CacheKey treats it
	// as a substitute for running the preprocessor or walking the dep
	// closure locally: skip FindDependencies, skip Preprocess, mix this
	// digest in directly. Set by the worker for CAS-mode requests where
	// the client shipped a CasDescriptor; nil otherwise. Takes priority
	// over PreprocessedDigest when both are set (CAS-mode requests
	// can't also carry preprocessed bytes, but the precedence keeps
	// the contract explicit).
	ManifestDigest *[32]byte
}

// NewInvocation returns an Invocation with maps initialized.
func NewInvocation() *Invocation {
	return &Invocation{Defines: map[string]string{}}
}

// ReadsStdin reports whether the invocation reads its source from
// the wrapper's stdin (`-` or `/dev/stdin` as an input). The runner
// uses this to keep stdin-reading compiles on the in-process path —
// the daemon protocol carries parsed argv but no stdin bytes, so a
// daemon-dispatched stdin invocation would silently see an empty
// file. The clearest symptom is the Linux kernel's
// `scripts/cc-version.sh` probe (`gcc -E -P -x c -` with the C
// source heredoc'd in) producing empty preprocessor output and the
// build aborting with "unknown C compiler."
func (inv *Invocation) ReadsStdin() bool {
	for _, in := range inv.Inputs {
		if in == "-" || in == "/dev/stdin" {
			return true
		}
	}
	return false
}

// Cacheable reports whether this invocation can be served from /
// recorded into the hpcc V1 cache. The callers (runner.Run and
// the daemon's compile handler) gate cache lookup and store on it
// so we never produce or consume entries that the cache shape
// can't represent correctly.
//
// False for:
//
//   - Non-compile modes (link/preprocess/dep-only/assemble). Link
//     inputs are .o/.a files that depend on too much external
//     state to key on source hash; the rest are either cheap or
//     write to stdout/temp files that don't fit a single-output
//     cache entry.
//
//   - Stdin-source compiles ("-" anywhere in Inputs, or the
//     /dev/stdin synonym). The cache key incorporates preprocessed
//     bytes via Preprocess(), which consumes stdin — by the time
//     Invoke runs the real compile, the FD is at EOF. Caching
//     stdin compiles would also be unsound: the bytes the key
//     captures are the bytes we couldn't read again, so any later
//     invocation with the same argv but different stdin content
//     would have no way to detect the mismatch.
//
//   - Multi-input compiles. gcc/cl write one object per input; the
//     CompileCache shape stores one output blob per entry, so caching
//     here would silently drop all but one .o.
//
// All three of these paths fall through to a direct Compiler.Invoke,
// which is correct: the wrapper passes the user's argv to the real
// compiler verbatim, the compiler writes whatever files it would
// write without hpcc in the picture, and the user sees normal
// behavior.
func (inv *Invocation) Cacheable() bool {
	if !inv.cacheableShape() {
		return false
	}
	if inv.isAssembly() {
		// Local/preprocessed cache key would be unsound: GAS
		// .incbin'd files aren't captured by the preprocessor
		// output. CAS-mode dispatch handles this — see
		// DispatchableUnderCAS.
		return false
	}
	return true
}

// DispatchableUnderCAS reports whether this invocation can be
// remotely dispatched in CAS mode. Same shape rules as Cacheable —
// single-input compile, no stdin, no /dev/null probe — but accepts
// assembly inputs because CAS ships the full source closure to the
// worker, so .incbin'd files are present at assemble time.
//
// Callers should reach for this in the daemon's dispatch gate when
// the configured source mode is CAS. The local-cache check
// (Cacheable) and the dispatch gate are intentionally separate:
// the local cache key for a .S input is still unsound (it would
// hash preprocessed text without the .incbin'd bytes), so the
// daemon dispatches without local caching when only this check
// returns true.
func (inv *Invocation) DispatchableUnderCAS() bool {
	return inv.cacheableShape()
}

// cacheableShape factors the shared "is this a single-source
// compile we can route through the CompileCache / dispatch path?"
// predicate out of Cacheable and DispatchableUnderCAS. Excludes the
// assembly carve-out — callers add or omit that check based on
// whether their downstream path can represent .S inputs soundly.
func (inv *Invocation) cacheableShape() bool {
	if inv.Mode != enum.CompileMode {
		return false
	}
	if len(inv.Inputs) != 1 {
		return false
	}
	if inv.ReadsStdin() {
		return false
	}
	if inv.isProbeInvocation() {
		return false
	}
	if HasUncapturedSideEffectFlag(inv.RawArgs) {
		return false
	}
	return true
}

// HasUncapturedSideEffectFlag reports whether argv carries a flag
// known to produce output files alongside the primary -o artifact
// that the dispatch/cache layer can't currently round-trip. Hitting
// any of these short-circuits Cacheable / DispatchableUnderCAS to
// false so the user's invocation falls through to a direct local
// Compiler.Invoke — they get exactly the files they asked for, just
// without remote dispatch or caching for that TU.
//
// The list is curated, not exhaustive: gcc has dozens of dump/trace
// flags and chasing them generically (parse every gcc option, know
// which write files) would be brittle across compiler versions.
// We catch the categories that actually surface in real builds and
// leave room to add more as they're reported. Common-case dep
// emission (-MD/-MMD/-MF) is handled by RewriteDepEmissionForCAS +
// the CompileResponse.extra_outputs pipeline, not by this opt-out.
//
// Recognised:
//   - -save-temps [=cwd|=obj]: dumps .i, .s, .o intermediates
//   - -fdump-*: every -fdump-tree-/rtl-/ipa-/passes/etc. variant
//   - -fcallgraph-info [=…]: writes <output>.ci
//   - -fprofile-generate [=…], -fprofile-arcs, -ftest-coverage,
//     --coverage: gcov instrumentation writes .gcno at compile time
//   - -gsplit-dwarf: writes <output>.dwo alongside the .o
//   - -fdiagnostics-format=sarif-file / =json-file: writes a
//     structured diagnostics file
func HasUncapturedSideEffectFlag(args []string) bool {
	for _, a := range args {
		switch {
		case a == "-save-temps" || strings.HasPrefix(a, "-save-temps="):
			return true
		case strings.HasPrefix(a, "-fdump-"):
			return true
		case a == "-fcallgraph-info" || strings.HasPrefix(a, "-fcallgraph-info="):
			return true
		case a == "-fprofile-generate" || strings.HasPrefix(a, "-fprofile-generate="):
			return true
		case a == "-fprofile-arcs" || a == "-ftest-coverage" || a == "--coverage":
			return true
		case a == "-gsplit-dwarf":
			return true
		case a == "-fdiagnostics-format=sarif-file" || a == "-fdiagnostics-format=json-file":
			return true
		}
	}
	return false
}

// isAssembly reports whether the invocation compiles an assembly
// source file. Used to gate Cacheable (and the preprocessed-mode
// dispatch path) but NOT DispatchableUnderCAS: the preprocess-on-
// client / compile-on-worker model assumes the preprocessor
// produces a self-contained translation unit, which is true for
// C/C++ but NOT for assembly. GAS directives like .incbin reference
// files that the assembler reads at assemble time, and those files
// don't exist on the worker side under PREPROCESSED.
//
// Concretely, the Linux kernel's usr/initramfs_data.S does
// `.incbin "usr/initramfs_inc_data"` and arch/x86/realmode/rmpiggy.S
// does `.incbin "arch/x86/realmode/rm/realmode.bin"`. CAS-mode
// dispatch fixes both: the manifest captures every file in the
// closure, so the worker materializes .incbin'd content alongside
// the .S itself and the assembler resolves the directive against
// the staged copy. See docs/plan/cas.md and Step 8.
//
// Local cache (Cacheable) still excludes assembly because its
// cache key derives from preprocessed bytes (no .incbin coverage).
// Daemon-mode CAS dispatch bypasses the local cache for .S inputs
// and relies on the worker's manifest-keyed compile cache instead.
func (inv *Invocation) isAssembly() bool {
	if inv.Language == "assembler" || inv.Language == "assembler-with-cpp" {
		return true
	}
	for _, in := range inv.Inputs {
		switch strings.ToLower(filepath.Ext(in)) {
		case ".s", ".S":
			return true
		}
	}
	return false
}

// isProbeInvocation matches the shape of build-system probes — small
// invocations that use the C compiler driver to ask questions, not to
// compile real translation units. The Linux kernel runs these by the
// hundred during kconfig and the build:
//
//   - scripts/as-version.sh: `gcc -Wa,--version -c -x assembler-with-cpp /dev/null -o /dev/null`
//     wants the assembler version banner on stdout. gcc short-circuits
//     and doesn't actually produce an object.
//
//   - $(cc-option) macro: `gcc -Werror <flag> -c -x c /dev/null -o tmp.o`
//     asks "does this gcc accept <flag>?" — exits non-zero if no.
//
// Treating these as cacheable compiles is wrong twice over: caching
// the result is meaningless (each probe is run once per build), and
// dispatching them through the FC path uploads empty preprocessed
// bytes to a VM that has no way to produce the host-specific
// diagnostic the kernel is asking for. Routing them to a direct
// Compiler.Invoke on the host runs the real driver, which is exactly
// what the probe is asking about.
//
// The heuristic is "/dev/null appears as input or output." Real
// translation units don't use /dev/null on either side; probes
// almost always do.
func (inv *Invocation) isProbeInvocation() bool {
	if inv.Output == "/dev/null" {
		return true
	}
	for _, in := range inv.Inputs {
		if in == "/dev/null" {
			return true
		}
	}
	return false
}

// CacheKey computes the cache-key seed for this invocation: a 32-byte
// BLAKE3-256 digest mixing source content, compiler identity, and the
// cache-key-relevant flags.
//
// Source-content input is one of (in priority order):
//
//   - ManifestDigest: a precomputed source-closure manifest digest. The
//     worker uses this on CAS-mode requests after re-verifying the
//     client-supplied manifest. Mixed in directly; no preprocessor or
//     dep walk runs.
//   - PreprocessedDigest: the BLAKE3 of preprocessed source bytes the
//     caller already produced. The worker uses this on PREPROCESSED-mode
//     requests. Mixed in directly.
//   - Config.SourceMode = SourceModeCAS (default): run
//     FindDependencies locally, hash each input/dep into a Manifest
//     (manifest.go), mix the manifest digest in. Same encoding as
//     the ManifestDigest short-circuit, so a CAS-mode worker and a
//     local-mode daemon compute identical keys for the same TU.
//   - Config.SourceMode = SourceModePreprocessed: run the
//     preprocessor locally and hash the resulting source bytes (via
//     the digest already computed in PreprocessResult — single pass
//     over the bytes, not two). Pairs with PREPROCESSED dispatch on
//     the wire.
//
// Each chunk written to the hasher is length-prefixed so concatenation
// can't collide ("ab"+"c" hashes differently from "a"+"bc").
func (inv *Invocation) CacheKey(ctx *Context) ([]byte, error) {
	if len(inv.Inputs) == 0 {
		return nil, fmt.Errorf("no input files in invocation")
	}

	var compilerIdentity []byte
	if ctx.IdentityOverride != nil {
		compilerIdentity = ctx.IdentityOverride
	} else {
		id, err := ctx.Compiler.Identity()
		if err != nil {
			return nil, fmt.Errorf("get compiler identity: %w", err)
		}
		compilerIdentity = id
	}

	digest := blake3.New()
	writeChunk := func(data []byte) {
		var lenbuf [8]byte
		binary.BigEndian.PutUint64(lenbuf[:], uint64(len(data)))
		digest.Write(lenbuf[:])
		digest.Write(data)
	}

	switch {
	case inv.ManifestDigest != nil:
		// Short-circuit: caller already built and (on the worker side)
		// verified the source-closure manifest. Mix the manifest
		// digest in directly. CAS-mode worker path.
		writeChunk(inv.ManifestDigest[:])

	case inv.PreprocessedDigest != nil:
		// Short-circuit: the source was preprocessed elsewhere and the
		// caller already handed us the digest of those bytes. Mix it
		// in directly — no preprocessor invocation, no dep walking.
		// This is the worker path for PREPROCESSED source mode.
		writeChunk(inv.PreprocessedDigest[:])

	case ctx.Config.SourceMode == enum.SourceModePreprocessed:
		res, err := ctx.Compiler.Preprocess(inv)
		if err != nil {
			return nil, err
		}
		if res.ExitCode != 0 {
			return nil, fmt.Errorf("preprocessor exit %d: %s", res.ExitCode, res.Stderr)
		}
		// Source bytes are already digested into res.Digest; reuse it
		// instead of re-hashing the whole preprocessed source.
		writeChunk(res.Digest[:])

	default:
		// CAS (and SourceModeUnspecified, i.e. zero-valued config —
		// treat as the default so an empty Config still works in
		// tests and in users who never set the field). Walk the dep
		// closure, hash each file, mix the aggregate manifest digest
		// in. Produces the same key a CAS-mode worker would compute
		// from a request carrying the same (path, content) pairs.
		m, err := BuildManifest(inv, ctx)
		if err != nil {
			return nil, err
		}
		writeChunk(m.Digest[:])
	}

	writeChunk(compilerIdentity)
	writeChunk(cacheKeyFlags(inv))

	return digest.Sum(nil), nil
}

// ComputeHash runs the preprocessor and returns a hex-encoded BLAKE3-256
// digest of the preprocessed source. The digest is computed during
// preprocessing (PreprocessResult.Digest) so this is a single pass over the
// bytes, not two.
func (inv *Invocation) ComputeHash(ctx *Context) (string, error) {
	res, err := inv.CacheKey(ctx)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(res), nil
}

// InvocationResult is the captured output of a single compile run. It is
// what the cache stores on a miss and replays on a hit; the duration is
// recorded for metadata only and is not part of the cache key.
//
// Extras carries side-effect output files the compile produced besides
// the primary -o artifact — typically .d dep files written by
// -Wp,-MMD,<path> for incremental-build dep tracking. Keyed by path
// (relative to the per-RPC output staging dir). The cache stores it
// alongside Output so warm hits replay the same dep files the cold
// compile produced; without that, deleting a .d on disk would leave
// `make` re-firing the rule forever on cache-hit responses that
// returned the .o but no .d.
type InvocationResult struct {
	Output   []byte
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	Duration time.Duration
	Err      error
	Extras   map[string][]byte
}
