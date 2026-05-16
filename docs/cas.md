# CAS Source Staging — Design Sub-Plan

Re-opens §4.5 of [plan/phase-4-distributed.md](plan/phase-4-distributed.md).
The original section is preserved
unchanged as the historical record of why this was deferred; this file
is the live design.

## Why now

1. **`.S` / `.incbin` carve-out.** PREPROCESSED can't represent GAS files
   that reference sibling paths at assemble time. Today
   `Invocation.Cacheable()` falls these back to local invoke; CAS lets
   the worker materialize the full source closure and assemble inside
   the VM like a normal build.
2. **Cross-developer cache hits.** Preprocessed bytes bake in
   `__FILE__` paths and other locals even with `-ffile-prefix-map`.
   CAS keys are computed from canonical, server-side-derived bytes,
   so two developers building the same tree converge on the same key.

## Trust model (centerpiece — everything else follows from this)

**The client never writes to S3.** Bytes flow client → worker;
the worker re-hashes incrementally (BLAKE3) and is the only thing
that calls `Store.Put`. S3 credentials live on the worker fleet, not
on developer laptops. This is the same boundary §4.13 paranoid mode
already draws for cache reads/writes, extended to source blobs.

Consequences that drop out of the trust model:
- Client-supplied digests are *advisory* until the worker verifies them.
  The worker computes BLAKE3 as bytes arrive and rejects the upload
  on mismatch. A client cannot poison cache content under a key it
  chose, because the worker stores by recomputed hash and indexes
  blobs by recomputed hash.
- The cache key for a CAS-mode compile is computed **server-side**
  from the recomputed manifest digest plus flag-key. Paranoid mode's
  "client cannot influence what key an artifact is stored under"
  invariant is preserved.
- `BlobRef.path` is client-supplied data, treated as untrusted: every
  path is re-validated at materialization time (no leading `/`, no
  `..` segments, no NUL bytes, no symlink components in the
  materialization target).

## Wire flow

The dispatch path the client takes for one CAS-mode compile. The
probe short-circuits the upload dance on cache-hit (most TUs in an
incremental build); a miss fans out to the full sequence.

```
Client                                      Worker
  │                                            │
  ├── ProbeCompileCache(CompileProbe) ────────►│   Step 2a
  │                                            │   compute cache_key, check CompileCache
  │                                            │
  ◄──────── ProbeResponse{hit} ────────────────┤   most TUs end here
  │                                            │
  │   (on miss only)                           │
  ├── FindMissingBlobs (bidi) ────────────────►│   Step 3
  ◄────────────────────────────────────────────┤
  │                                            │
  ├── UploadBlobs (client-stream) ────────────►│   Step 4
  ◄──── UploadResult ──────────────────────────┤
  │                                            │
  ├── Compile(CasDescriptor{...}) ────────────►│   Step 6
  ◄──── CompileResponse ───────────────────────┤
```

## Build order

Each step lands independently and leaves the tree in a working state.

### Step 1 — Manifest-mode cache key

`internal/compiler/cache_key.go` today hashes preprocessed bytes.
Manifest mode needs to hash the source-closure digest instead, so the
worker can compute the same key from the CAS manifest without ever
seeing preprocessed source.

- Add `Invocation.ManifestDigest *[32]byte` (parallels the existing
  `PreprocessedDigest`).
- `CacheKey(inv)` picks the input-digest source by priority:
  `ManifestDigest` → `PreprocessedDigest` → preprocess-then-hash
  fallback.
- Manifest computation (new function, `compiler.BuildManifest`):
  - Run `-M` to discover the header closure (already wrapped in
    `preprocess.go`).
  - For each path in `(main_source ∪ headers)`, hash file contents
    with BLAKE3.
  - For `.S` / `.s` inputs, run a recursive `.incbin` / `.include`
    closure scanner over the input AND each cpp-discovered header:
    GAS directives are invisible to `gcc -M`, but `.incbin "foo.bin"`
    and `.include "bar.S"` still pull files into the build. The
    walker is worklist+seen-set, recurses through `.include` chains
    (not `.incbin` binaries), and resolves paths against the
    invocation's cwd. Without this, kernel `usr/initramfs_data.S`
    and `arch/x86/realmode/rmpiggy.S` cause the worker compile to
    fail on missing files even though `gcc -M` reported a clean
    closure.
  - Aggregate as `BLAKE3( sort_by_path(path || ":" || file_digest)
    joined by "\x00" )`.
  - Return manifest digest + the sorted `[]BlobRef{path, digest, size}`.
- Tests: parity check — preprocess-hash and manifest-hash of the same
  TU produce *different* keys (different schemes) but each is stable
  across runs.

This step is valuable standalone: it gives paranoid-mode a way to
key compiles without making the daemon preprocess locally, and
de-risks the keying decision before any wire work.

### Step 2 — Proto changes

Un-reserve tags, define messages, regenerate.

```proto
// compile.proto
enum SourceMode {
  PREPROCESSED = 0;
  CAS          = 1;   // un-reserved
}

message RemoteDescriptor {
  ...
  oneof source_settings {
    PreprocessedDescriptor preprocessed = 6;
    CasDescriptor          cas          = 7;   // un-reserved
  }
}

message CasDescriptor {
  bytes manifest_digest    = 1;   // BLAKE3(sorted path||digest list)
  repeated BlobRef blobs   = 2;
  string entry_path        = 3;   // which blob is the TU root
}

message BlobRef {
  bytes digest = 1;   // BLAKE3-256
  string path  = 2;   // path inside the materialization root
  uint64 size  = 3;
}
```

New RPCs on `WorkerService` (same gRPC server the client already
dials post-`Route`, same auth, same pinned-cert path):

```proto
service WorkerService {
  rpc Compile(CompileRequest) returns (CompileResponse);
  // CAS additions:
  rpc ProbeCompileCache(CompileProbe) returns (ProbeResponse);
  rpc FindMissingBlobs(stream BlobDigest) returns (stream BlobDigest);
  rpc UploadBlobs(stream BlobChunk) returns (UploadResult);
}

// Cache short-circuit. Client sends just the manifest_digest + args
// + image; worker derives the cache key and probes the compile cache
// (local + S3). Hit returns the artifact; miss tells the client to
// proceed with the upload dance. Mirrors Bazel's
// ActionCache.GetActionResult.
message CompileProbe {
  bytes manifest_digest = 1;
  repeated string args = 2;       // worker derives flag-key from these
  string tenant_id = 3;
  string image_digest = 4;
  string scheduler_token = 5;
}

message ProbeResponse {
  oneof result {
    CompileResponse hit  = 1;
    ProbeMiss       miss = 2;
  }
}

message ProbeMiss {}

message BlobDigest { bytes digest = 1; uint64 size = 2; }

message BlobChunk {
  // First message in a per-blob run carries the header; subsequent
  // messages carry data only. Worker tracks the active blob via
  // stream state.
  oneof body {
    BlobDigest header = 1;   // digest the client claims; worker verifies
    bytes      data   = 2;
  }
}

message UploadResult {
  uint64 blobs_received = 1;
  uint64 bytes_received = 2;
  repeated bytes rejected_digests = 3;  // hash mismatch or quota fail
}
```

### Step 2a — `ProbeCompileCache` handler

The probe is the dispatch path's hot route. On a typical incremental
build, most TUs are cache-hits — the probe terminates with the
artifact in one RPC, no source upload at all. This isn't just a
latency win for the same developer: with the existing `CompileCache`
S3 store as a layer of the compile cache chain, worker B's probe
hits a result that worker A put there, so **cross-worker AND
cross-developer compile-result sharing falls out of the probe
mechanism** (subject to manifest-digest parity, which Step 5a
addresses via path normalization).

Worker behavior on `ProbeCompileCache(CompileProbe)`:

1. Validate `scheduler_token` (same JWT path as `Compile`).
2. Parse `args` → `Invocation`, derive flag-key.
3. Compute `cache_key = BLAKE3(manifest_digest || flag_key || image_digest)`.
4. `CompileCache.Lookup(cache_key)` — this already chains local disk +
   S3 in the standard configuration.
5. Hit → `ProbeResponse{hit: CompileResponse{output_artifact, ...}}`.
6. Miss → `ProbeResponse{miss: {}}`.

**Paranoid-mode invariant.** Client sends `args`, not a pre-baked
flag-key. The worker re-parses and computes flag-key itself, so the
client cannot influence what cache_key the worker probes. The
client *can* send a fake `manifest_digest` — but the worst case is
a wasted probe (miss → client proceeds to upload dance → worker
materializes for real and computes the *real* cache_key on the way
out). The probe is not a cache-write path; it cannot poison.

**Content-addressed disclosure caveat.** A client that knows another
tenant's source closure can fetch that tenant's compile output via
the probe — same property Bazel has. Mitigation if desired: a
paranoid-extra config knob that mixes `tenant_id` into the cache
key. Off by default (kills cross-developer sharing); separate
follow-up, not part of this rollout.

### Step 3 — Worker-side lookup (single-layer)

`FindMissingBlobs` handler in `internal/worker/`. One-line handler:
for each incoming `BlobDigest`, reply with the digest iff
`sourceStore.Has(digest)` returns false. 1:1 bidi so the client can
pipeline `FindMissingBlobs` with `UploadBlobs` without waiting for the
full probe set.

Storage is a plain `store.Store` namespaced to `"source"` at worker
startup — no dedicated `SourceStore` facade. Forwarding wrapper
wouldn't earn its keep: every method would be a one-line delegate
to the underlying namespaced store. Blob bytes live under a fixed
name within each digest's entry (a `blobData` constant in the
worker package) so the wrapper-free `store.Put(digest, blobData,
bytes)` call site is explicit about which named slot it touches.

No in-memory confirmed-set. No S3 probing. Both were considered and
dropped:

- **No L2 (confirmed-set).** `os.Stat` is cheap (microseconds). A
  hot kernel build does at most low-thousands of probes per compile;
  the LRU bookkeeping isn't worth the saved syscalls. Revisit only
  if profiling shows stat overhead dominating.
- **No L3 (S3 probing).** S3 isn't in the source-blob picture at
  all — see Step 4. Without S3 there's nothing to probe.

### Step 4 — Worker-side upload + verification

`UploadBlobs` handler. **Source blobs are worker-local only.** No S3
write-through; blobs are ephemeral and die with the worker. The
client is the source of truth for source bytes — a worker that
doesn't have a blob locally tells the client via `FindMissingBlobs`,
and the client uploads.

- Per stream-blob: open a BLAKE3 hasher, stream incoming `data`
  chunks through it, tee to the local store's temp file.
- On end-of-blob (next header or stream close): compare hash to
  header digest. Match → atomic-rename into the local source store
  (`store.Put(recomputed_digest, blobData, bytes)`). Mismatch →
  discard, append to `rejected_digests`.
- Per-tenant write quota: token bucket on bytes/sec and bytes/window
  keyed by `tenant_id` (already in the JWT). Over quota → reject
  remaining blobs in the stream with a typed error; client falls
  back to local compile.

Cross-worker sharing of source: **none at upload time.** A tenant
re-routed to a new worker re-uploads the source closure once. The
cost is bounded by sticky-tenant routing — most builds hit the same
worker and skip upload entirely (Step 3 reports nothing missing). The
big cross-worker win — compile-result sharing — happens via the
existing `CompileCache` S3 store and is unaffected.

### Step 5a — Path normalization via `.hpcc` project marker

For cross-developer compile-result hits via `ProbeCompileCache` to
actually fire, two developers' `manifest_digest`s have to agree on
the same source closure. Today `BuildManifest` uses paths as `-M`
emits them (typically absolute: `/home/alice/proj/include/foo.h`),
so dev A and dev B with the same checkout at different paths get
different manifest digests.

The fix: paths in `BlobRef.path` (and therefore in the
manifest-digest computation) become **relative to the project
root**, discovered via a `.hpcc` marker file.

- **Discovery.** Walk up from the client's cwd; the first directory
  containing `.hpcc` is the project root. If none is found, fall
  back to absolute paths (current behavior — cross-worker still
  works for the same developer, cross-dev does not).
- **File format.** An empty `.hpcc` file is a valid marker. Optional
  TOML contents hold project-scoped config (exclude globs,
  paranoid override, mode, etc.). Sibling pattern to `.editorconfig`
  / `.tool-versions`.
- **System headers stay absolute.** `/usr/include/...` paths are
  tied to the toolchain image, and image_digest is already in the
  cache key. Re-rooting them under the project would be wrong
  (they'd appear to "live in" the project tree).
- **Materialization.** Worker materializes blobs under
  `/run/hpcc/src/<exec_id>/<relative-path>` for project-relative
  paths, and at their original location for system paths (which
  already exist in the image).

Failure semantics on missing `.hpcc`: silent fallback to absolute
paths. Don't error — that breaks existing users mid-upgrade.
Cross-developer hits just don't fire until the marker is added.

### Step 5b — Client-side CAS dispatch

Client daemon flow when `source_mode == CAS` is selected:

1. Parse invocation → `compiler.BuildManifest(inv)` with project
   root resolved per Step 5a → manifest digest + `[]BlobRef`.
2. Dial worker (existing pinned-cert path).
3. `ProbeCompileCache(CompileProbe{manifest_digest, args, ...})`.
4. **Hit** → return artifact, done. No upload, no Compile RPC.
5. **Miss** → continue:
   a. `FindMissingBlobs` with the full blob list → missing subset.
   b. `UploadBlobs` streaming only the missing blobs.
   c. `Compile(CompileRequest{ descriptor: { source_mode: CAS,
      cas: CasDescriptor{ manifest_digest, blobs, entry_path } } })`.
   d. Handle `CompileResponse` as today.

On any remote-side error in steps 3–5, fall back to local compile
(per §4.11). The probe-and-fail path is no worse than today's
direct-Compile-and-fail.

Mode selection: top-level `source_mode` config field, default
`"cas"`. The same field drives the local cache-key derivation
algorithm so a client's local cache and a worker-fronted shared
cache (S3, paranoid mode) stay coherent without an extra knob —
see `enum.SourceMode` for the full story. `"preprocessed"` remains
selectable as the fallback / oracle path.

### Step 6 — Worker-side materialization

Reached only after a probe miss (Step 2a) and a successful upload
dance (Steps 3–4 + client). `Compile` handler with
`source_mode == CAS`:

1. Re-verify `manifest_digest == BLAKE3(sorted path||blob.digest)`
   from the `CasDescriptor`. Mismatch → reject before pulling any
   blobs.
2. Re-check the compile cache one more time. The probe at Step 2a
   was authoritative when it ran, but between probe and Compile
   another tenant may have compiled the same TU and populated the
   cache. Hit → return cached artifact, skip materialization.
3. For each `BlobRef`:
   - Re-validate `path` (no `/`, no `..`, no NUL, no symlink-component).
   - Load bytes via `store.Get(blob.Digest, blobData)` against the
     `"source"`-namespaced store (local only — no S3 fallback,
     source blobs are worker-local). If missing, fail the compile
     with a typed "blob unavailable" error; the client treats it as
     a signal to retry and re-upload the missing blob via Steps
     3–5b (the next probe round will hit, OR FindMissingBlobs will
     report it missing, OR the next upload will land it).
4. Stream blobs to the agent through the existing
   `AgentService.Exec` input-files channel (§4.4.1). Agent
   materializes under `/run/hpcc/src/<exec_id>/<blob.path>`,
   invokes the compiler with `entry_path` as the TU root.
5. Pre-create parent directories the compiler will need but the
   manifest doesn't carry as files:
   - Output parents (`mkdirOutputParents`): for every `/out/<path>`
     the argv mentions (joined `-o`, separate `-o`, embedded
     `-Wp,-MMD,<path>`, separate `-MF`), `mkdir -p` the parent
     under `outHostPath`. Compilers refuse to create missing
     parents for output files — they `open(...O_CREAT)` the leaf
     only — so without this, `-Wp,-MMD,/out/build/main.d` ENOENTs
     before clang produces the .o.
   - Search-path parents (`mkdirSearchPaths`): for every
     `-I` / `-iquote` / `-isystem` / `-idirafter` / `-L` argument
     that resolves under `/src/`, `mkdir -p` it under
     `srcHostPath` (empty if needed). The dep closure only ships
     files actually `#include`'d by this TU, so an `-I` dir whose
     contents this TU doesn't pull in never materializes — and
     `-Werror=missing-include-dirs` (kernel and many other builds
     enable it) fires before the compile starts.
   Both helpers also live on the agent side for the firecracker
   path, where argv arrives post-translation and srcHostPath is the
   agent's per-Exec staging dir.
6. Capture side-effect outputs (`.d` files etc.) the compiler wrote
   under `outHostPath` into `CompileResponse.extra_outputs`. The
   client's `dispatchCAS` promotes these onto `result.Extras`; the
   daemon's `writeCompileResult` materializes them under `inv.Cwd`.
   Cached the same way as `output_artifact` (a binary length-prefixed
   `extras` blob in the compile cache, NOT JSON), so warm hits replay
   the same `.d` files cold compiles produced — without this, the
   kernel's `fixdep` fails on the first warm rebuild after `make
   clean` (the .o comes back from cache, the .d doesn't).
7. On success, store output keyed by the manifest-derived cache key
   (same formula as Step 2a uses for the probe).

### Step 7 — GC

Almost nothing to do. Source blobs are worker-local-only and die with
the worker; there's no shared-store reference graph to maintain. The
only ongoing cost is local-disk growth on long-lived workers, which
the existing `DiskCacheStore` LRU (size-based eviction via the
`max_size` config knob) handles for the `source/` namespace just like
it does for `compile/`. Set a budget at construction time and the
existing eviction code does the rest.

No periodic sweep, no reference tracking, no audit-tied retention
window. If we re-introduce S3-backed source blobs later, this section
gets rewritten.

### Step 8 — Wire up `.S` files

The carve-out that fell assembly inputs back to local invoke is
split rather than removed: `Invocation.Cacheable()` still excludes
`.S/.s` (the local cache key derives from preprocessed bytes and
can't capture `.incbin`'d content), but a new
`Invocation.DispatchableUnderCAS()` allows them through the CAS
dispatch path (the manifest does capture the full closure).

The daemon's gate widens accordingly: invocations that are *only*
CAS-dispatchable (e.g. .S with the daemon configured for
`source_mode = "cas"`) skip the local cache and go straight to
`Dispatcher.Dispatch`. The worker's manifest-keyed compile cache
handles hit detection on the remote side.

This is the original forcing function. The actual end-to-end smoke
test — kernel `.S` with `.incbin` compiling through the full stack
— runs out of `bench/kernel-bench-fc.sh` against a real firecracker
setup, gated in CI by the `fc-bench` matrix in
`.github/workflows/kernel-bench.yml`. Unit coverage in this repo:

- `TestDispatchableUnderCAS` (internal/compiler/invocation_test.go) —
  pins the new gate's contents.
- `TestCacheableAndCASDivergeOnAssembly` — pins the divergence
  invariant: `Cacheable==false && DispatchableUnderCAS==true` for
  `.S` inputs.
- `TestCompile_CASReturnsDepFileAsExtraOutput`
  (internal/worker/worker_test.go) — pins that the worker collects
  `.d` files and surfaces them as `extra_outputs`, the wire piece
  of Step 6.

A separate carve-out exists for argv shapes the dispatch path can't
faithfully reproduce: `compiler.HasUncapturedSideEffectFlag`
recognises `-save-temps`, `-fdump-*`, `-fcallgraph-info`,
`-fprofile-generate`, `-fprofile-arcs`, `-ftest-coverage`,
`--coverage`, `-gsplit-dwarf`, and `-fdiagnostics-format=*-file`.
`Cacheable()` / `DispatchableUnderCAS()` both return false on a
match, the daemon invokes locally, and a one-shot yellow notice on
stderr explains why the cache/remote was bypassed for that compile.

## Open questions — resolved log

- ~~**Cache-key flag-key compatibility.**~~ **Resolved by store-layer
  namespacing.** Compile entries, source blobs, and manifests live in
  separate Store namespaces (`compile/`, `source/`, `manifest/`). A
  cache-key bit-collision between a ManifestDigest-derived key and a
  PreprocessedDigest-derived key can no longer land in the same
  store directory, so the scheme-version tag isn't needed. The
  related question of whether to expose key-derivation as a separate
  config knob (briefly shipped as `cache_key_mode`) was resolved by
  collapsing it back into `source_mode`: cache-key derivation and
  dispatch wire format always have to track the same value, so one
  field expresses both decisions.
- ~~**L2 confirmed-set / L3 S3 probe.**~~ **Both dropped.** Step 3
  collapses to `sourceStore.Has`. Source blobs are worker-local-only
  (no S3 storage for source), so there's nothing for L3 to probe; L2
  isn't worth the LRU bookkeeping when `os.Stat` is cheap.
- ~~**S3 source storage / materialization-time fallback.**~~ **Dropped.**
  Source blobs are ephemeral, worker-local. Cross-worker sharing
  happens only for compile *results* (via the existing `CompileCache`
  S3 store). Tenant rerouted to a fresh worker re-uploads the source
  closure once; sticky routing makes this rare.
- ~~**Cache-probe before upload?**~~ **Yes — added as Step 2a.**
  `ProbeCompileCache` lets the client short-circuit the entire
  upload dance when the compile result is already cached. Mirrors
  Bazel's `ActionCache.GetActionResult`. Single biggest dispatch-path
  optimization: 1 RPC for cache-hit TUs (~32 bytes up, .o down).
  Unlocks cross-worker AND cross-developer compile-result sharing
  (subject to Step 5a path normalization).
- ~~**Project root detection for path normalization?**~~ **`.hpcc`
  marker file.** Walk up from cwd, first match wins. No marker →
  silent fallback to absolute paths (preserves current behavior for
  unmigrated users). Empty file is valid; TOML contents hold
  project-scoped config. Sibling pattern to `.editorconfig`.
- ~~**Upload concurrency from one client.**~~ **Single stream**,
  per the lean. HTTP/2 framing multiplexes well enough that the
  kernel-bench numbers don't justify the quota-accounting headache
  of parallel streams. Revisit if profiling shows the single
  `UploadBlobs` stream saturating before the worker's compile pool
  does.
- **Per-tenant write quota.** Not yet implemented. The Step 4
  design calls for a token bucket on bytes/sec and bytes/window
  keyed by `tenant_id`, with hard-reject + client-side local
  fallback (vs. backpressure) so the build never blocks
  indefinitely. Deferred until a multi-tenant deployment actually
  needs it; single-tenant CI use is unbounded today.

## What this does *not* change

- §4.7 determinism flags still auto-injected.
- §4.13 paranoid mode invariants unchanged (in fact strengthened —
  the trust boundary is now identical for source and output).
- Local-cache and S3-store layout for *compile-output* artifacts
  unchanged. Source blobs use the same `Store` interface but live
  under a separate namespace (`source/<hh>/<full-hex>/...`) on
  disk only — no S3 mirror.
- Agent wire schema (§4.4.1) unchanged — input-files chunking
  already exists; CAS uses the same channel.

## Status

Shipped. All build-order steps have landed and CAS is the default
`source_mode`. End-to-end coverage:

- `internal/compiler/manifest.go` — Step 1 manifest computation +
  the `.S` / `.incbin` / `.include` closure scanner.
- `internal/protocol/compile.proto` — Step 2 wire types
  (`ProbeCompileCache`, `FindMissingBlobs`, `UploadBlobs`,
  `CasDescriptor`, `BlobRef`, `BlobChunk`, etc.).
- `internal/worker/cas.go` — Steps 2a / 3 / 4 server handlers,
  including BLAKE3-on-the-fly verification and the
  `recomputed-digest` storage key.
- `internal/daemon/dispatch/dispatch.go` `dispatchCAS` — Step 5b
  client flow.
- `internal/worker/worker.go` `Compile` (CAS branch) — Step 6
  materialization, plus `mkdirOutputParents` /
  `mkdirSearchPaths` for the parent-dir gaps the manifest doesn't
  carry and the `extras` round-trip for `.d` files.
- `internal/cache/compile.go` — Step 6 extras encoding (binary,
  length-prefixed; not JSON) and the `extras` cache blob.
- `bench/kernel-bench-fc.sh` + `bench/kernel-bench-fc-both.sh` +
  `.github/workflows/kernel-bench.yml` — CI exercises both
  `source_mode` legs against a real `v7.0` kernel build via
  firecracker.

Outstanding work tracked in **Open questions** above: per-tenant
write quota (Step 4 hook unimplemented), the tenant-isolated
content-addressed disclosure paranoid-knob (Step 2a caveat). Update
§4.5 in [plan/phase-4-distributed.md](plan/phase-4-distributed.md)
to point here.
