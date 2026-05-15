## Phase 1: Core Compiler Wrapping

The foundation. Get a single-machine cache loop working end-to-end.

### 1.1 Compiler Detection & Flag Parsing

Two clean layers, kept separate:

- **`Compiler`**: stateless strategy. One instance per toolchain detected on the
  system. Methods: `Name()`, `Family()`, `Parse(args) (*Invocation, error)`,
  `Preprocess(ctx, inv)`, `Invoke(ctx, inv)`, `Identity()`.
- **`Invocation`**: pure data — the parsed result of one argv. Holds inputs,
  output, mode, language, includes, defines, std, raw argv, etc.

Detection from `argv[0]` (or explicit flag) returns a `Compiler`. Parsing argv
returns an `Invocation`. The same `Invocation` type is what flows over the wire
in Phase 4 — so it must serialize cleanly.

There are effectively **two argument grammars**, not N:
- **GNU**: GCC, Clang, Intel icc/icx (Linux), nvcc, mpicc/mpicxx wrappers.
- **MSVC**: cl.exe, clang-cl, icx-cl, intel on Windows.

Each compiler implementation reuses one of two shared parsers (`gnuParser`,
`msvcParser`) driven by a flag spec table:

```go
type FlagSpec struct {
    Name     string
    Takes    ValueMode  // None | Joined | Separate | JoinedOrSeparate
    Category Category   // Output | Include | Define | Mode | Linker | Diag | Passthrough
}
```

Things the parser must handle:
- `@file` response files — expand recursively before classifying.
- Joined vs. separate values: `-I/path`, `-I /path`, `-isystem /path`, `/Fo:foo`.
- Linker/assembler passthrough: `-Wl,...`, `-Wa,...`, `-Xlinker`, `-Xcompiler`.
- Extension-based input detection: `.c .cc .cpp .cxx .C .c++ .cu .s .S .ll .bc .o .obj .a .lib .so .dll`.
- Mode classification: Preprocess (`-E`/`/E`/`/EP`), Compile (`-c`/`/c`),
  Assemble (`-S`), Link (default), Dependency-only (`-M` family,
  `/showIncludes`).
- Order-sensitive flags (`-l`, `-L`) preserved in `Raw` for emit.
- **Never drop unknown flags** — pass them through verbatim. New compiler
  versions add flags constantly.

### 1.2 Input Hashing

Two modes, both supported:

**Preprocess mode (default, correct):**
Run the compiler's preprocessor (`-E`) to resolve all `#include` directives and
macros into a single translation unit. Hash the preprocessed output.

**Manifest mode (used by CAS source mode, §4.5):**
Run dependency generation (`-M`/`-MM`) to discover the include closure. Hash
`(BLAKE3 of sorted (path, file_digest) pairs + relevant_flags + image_digest)`
without ever materializing preprocessed bytes. ccache's `depend_mode` is the
reference. Wired up when `source_mode = "cas"` so the worker can compute the
same cache key from a `CasDescriptor` without seeing preprocessed source. See
`compiler.BuildManifest` and [docs/cas.md](../cas.md).

In both modes, the cache key incorporates:
- **Toolchain identity**:
  - In bare-metal mode: hash of compiler binary + version output.
  - In container/VM mode (Phase 4): the image digest. This is cleaner — no
    "hash the gcc binary" dance, the digest already pins everything.
- Source content (preprocessed bytes or manifest digests).
- Relevant flags (strip flags that don't affect output: `-v`, color, parallelism).
- Target architecture / sysroot.

Use **xxhash or BLAKE3** for speed. BLAKE3 if you want a cryptographic hash for
audit defensibility (banks like that), xxhash if pure speed.

### 1.3 Local Disk Cache

Content-addressable storage under `~/.cache/hpcc/`.
Directory layout: `<first 2 hex chars>/<full hash>/` containing:
- `output.o` (or whatever the compiler output is)
- `stdout` and `stderr` captures
- `exit_code`
- `metadata.json` (timestamp, compiler, original command, input file, duration)

On cache hit: hardlink the cached `.o` to the requested output path, replay
stdout/stderr, exit with the cached exit code.
On cache miss: run the real compiler, store the results.

### 1.4 Drop-in Replacement

When `hpcc` is symlinked as `cc`, `gcc`, `clang`, `c++`, `g++`, etc., it should
detect the intended compiler from `argv[0]` and wrap it transparently.
Alternatively support explicit mode: `hpcc wrap gcc -c foo.c -o foo.o`.
Unsupported invocations (linking, assembly, unknown flags) pass through to the
real compiler with zero overhead.

### 1.5 CLI Commands (Phase 1)

- `hpcc wrap <compiler> [args...]` — manually wrap a compilation.
- `hpcc stats` — show local cache hit/miss counts, cache size on disk.
- `hpcc clean` — evict entries (by age, LRU, or to reach a target size).

### Milestone ✅

`hpcc wrap gcc -c foo.c -o foo.o` compiles on first run, returns the cached
result on second run with no recompilation.

