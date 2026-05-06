# hpcc — Implementation Plan

hpcc is a distributed compilation and caching tool. A drop-in compiler wrapper
that caches build artifacts locally and remotely, and optionally distributes
compilation across a cluster of workers — with a strong tenant-isolation story
suitable for environments (banks, regulated enterprises) where running
user-supplied compilation on shared hardware is politically untenable.

---

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

**Manifest mode (faster, used by server-side preprocessing in Phase 4):**
Run dependency generation (`-M`/`-MM`) to discover the include closure. Hash
`(source_digest + sorted_dep_digests + relevant_flags + toolchain_id)` without
ever materializing preprocessed bytes. ccache's `depend_mode` is the reference.

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

### Milestone

`hpcc wrap gcc -c foo.c -o foo.o` compiles on first run, returns the cached
result on second run with no recompilation.

---

## Phase 2: Daemon Architecture

Move the cache logic into a long-running daemon so multiple concurrent compiler
invocations share state efficiently.

### 2.1 Daemon Process

- `hpcc start` — launch the daemon in the background, listening on a Unix socket
  (`$XDG_RUNTIME_DIR/hpcc/sock` or `/tmp/hpcc.sock`).
- `hpcc stop` — gracefully shut down the daemon.
- `hpcc status` — report whether the daemon is running, uptime, active
  compilations, cache stats.

### 2.2 Client-Server Protocol

**Length-prefixed protobuf over the Unix socket** — *not* gRPC. The wrapper
binary is invoked thousands of times per build and must start fast and stay
small; pulling in the gRPC runtime is overkill for local IPC. Same `.proto`
files as the Phase 4 control plane, just a thinner client.

Messages:
- `CompileRequest` — compiler path, args, working directory, environment subset.
- `CompileResponse` — cache hit/miss, output artifact bytes (or path),
  stdout, stderr, exit code.
- `StatsRequest` / `StatsResponse`
- `CleanRequest` / `CleanResponse`

### 2.3 Deduplication

If two concurrent invocations have the same cache key, the daemon should only
run the compiler once and serve the result to both. Track in-flight
compilations by cache key; use channels/waitgroups to block duplicates until
the first completes.

### 2.4 Fallback

If the daemon is not running, the client falls back to compiling directly
(with local cache still available in-process). Never fail a build because the
daemon is down.

### Milestone

`make -j16` with the daemon running deduplicates identical translation units
and reports stats from a single process.

---

## Phase 3: Remote Cache

Share cached artifacts across machines so a team doesn't recompile the same
code.

### 3.1 Cache Backend Interface

```go
type RemoteCache interface {
    Get(ctx context.Context, key string) (*CacheEntry, error)
    Put(ctx context.Context, key string, entry *CacheEntry) error
    Contains(ctx context.Context, key string) (bool, error)
}
```

Implement at least two backends:
- **hpcc server** — a dedicated HTTP cache server (simple blob store).
- **S3-compatible** — works with AWS S3, MinIO, R2, etc.

### 3.2 Why HTTP, not gRPC

The cache is blob movement keyed by content hash. **HTTP `GET`/`PUT`/`HEAD` on
a CAS key is the exact shape S3, MinIO, R2, GCS, and every CDN already speak.**
Switching to gRPC here means your `hpcc-server` has to proxy every blob and
becomes a permanent scaling bottleneck. Wins from HTTP:

- Drop-in S3 compatibility, no protocol code.
- Signed URLs so clients pull blobs directly from object storage.
- HTTP/2 multiplexing if you want it.
- `curl` debugging.
- Range requests, ETag dedup, conditional GETs.

The one credible alternative is the **Bazel Remote Execution API** (gRPC +
bytestream). Pick that only if interop with Bazel/Buck2/BuildBuddy is a goal.

### 3.3 Lookup Order

1. Local disk cache
2. Remote cache
3. Compile (locally or distributed — see Phase 4)
4. Push result to both local and remote

### 3.4 Failure Handling

- Remote cache operations have a configurable timeout (default: 2s reads, 5s
  writes).
- On timeout or error: log it, skip remote, compile. Never stall the build.
- Write failures are non-fatal — the build succeeds, the artifact just isn't
  shared.

### 3.5 Authentication

- hpcc server: mTLS or bearer token.
- S3: standard AWS credential chain.

### 3.6 hpcc Server

- Standalone binary or subcommand:
  `hpcc server --listen :9090 --storage /var/lib/hpcc`.
- API: `PUT /cache/<key>`, `GET /cache/<key>`, `HEAD /cache/<key>`.
- Storage: local filesystem (same CAS layout as the client) with configurable
  max size and LRU eviction.

### Milestone

Developer A compiles a file. Developer B on a different machine gets a remote
cache hit for the same file without compiling.

---

## Phase 4: Distributed Compilation in Per-Tenant VMs

Farm out compilation to remote workers, isolated in **Firecracker microVMs**,
to parallelize beyond local CPU count and provide a defensible isolation
boundary for regulated environments.

### 4.1 Why Firecracker

The target deployment is regulated enterprises (banks, finance) where the
security review isn't asking *"is this technically sufficient?"* — it's
asking *"is this a boundary auditors recognize?"* Firecracker gives you:

- A separate kernel + KVM boundary. Hard to argue with.
- Clean per-tenant isolation: Alice's compile cannot touch Bob's source via
  shared `/proc`, page-cache side channels, or kernel CVE.
- Trivial network policy: **the VM has no NIC.** No exfiltration argument
  to have.
- Per-VM lifecycle events make a clean audit trail.

Namespace-based sandboxes (bwrap, nsjail, etc.) are explicitly out of scope.

### 4.2 VM Lifecycle: One VM per Tenant Session, Not per Job

Booting a fresh Firecracker per compile (~125ms) destroys throughput on a
build with thousands of invocations. Instead:

- **One VM per active tenant session**, reused across many compiles.
- **Idle timeout** (e.g. 5–15 min) → snapshot to disk via Firecracker's
  snapshot/restore (~10–50ms restore vs. ~125ms cold boot).
- **LRU eviction** of snapshots under disk pressure.
- **Hard session timeout** (e.g. shift change, N hours) → blow the VM away.
  Long-lived per-tenant state accumulates and someone will eventually ask
  what's in it.

### 4.3 Container Image as the Build Environment

The user supplies their toolchain by handing hpcc a **container image**.
hpcc converts it to a Firecracker rootfs once, caches the converted rootfs
by image digest, and boots VMs from it.

- Conversion path: OCI image → flatten layers → `mkfs.ext4` → rootfs blob.
  Tools to crib from: `firecracker-containerd`, Weaveworks Ignite, Kata
  Containers. Or DIY in ~50 lines of shell.
- **Image digest is the toolchain identity** for cache keys. Same image used
  by 50 developers → one rootfs on disk, one toolchain identity in the cache.
- Conversion is seconds-to-minutes for a fat C++ toolchain image; cache
  aggressively, pre-warm on push if possible.
- The kernel is **hpcc's**, not the image's — Firecracker boots with a kernel
  you provide.

### 4.4 VM Layout

For each running VM:

- **Read-only base rootfs** (per image digest, shared across VMs).
- **Per-VM ephemeral overlay** for `/tmp`, `/var`, build scratch.
- **virtio-fs** mount of the project source tree (read-only) and output dir
  (read-write).
- **virtio-fs** mount of the shared compile cache (read-write, `noexec`) so
  cache hits work across tenants without exposing other tenants' source.
- **vsock** for the control plane — a small static `hpcc-agent` binary baked
  into the rootfs receives `Compile(...)` RPCs and runs the toolchain.
- **No virtio-net.** Compiles don't need network. No exfiltration argument.

### 4.5 Server-Side Preprocessing

Because the VM has the toolchain and system headers, preprocessing should
happen *there*, not on the client. Wins:

- **Bandwidth.** Original source is ~10 KB; preprocessed source is 1–50 MB.
- **Cross-developer cache hits.** Client-preprocessed output bakes in
  `__FILE__` paths and other locals; server-side preprocessing produces
  canonical bytes, raising cache hit rate dramatically.
- **CPU offload** from developer laptops to the build farm.

Three modes negotiated per-job:

1. **`shared_root`** — worker VM mounts the same source tree the client sees
   (via virtio-fs from a shared volume). Client sends `(source_path, flags,
   cwd)`; worker preprocesses + compiles. Realistic in container-pinned
   bank environments.
2. **`cas`** — heterogeneous case. Client runs `gcc -M` to discover the
   include closure, digests each input, sends `(source_digest,
   header_digests[], flags)`. Worker pulls missing digests from the shared
   CAS, materializes a synthetic input root, compiles. Reuses the Phase 3
   blob store as the CAS.
3. **`preprocessed`** — fallback. Client preprocesses locally and ships
   bytes. Same RPC, just a populated `preprocessed_source` field.

### 4.6 Build-System Compatibility (CMake/ninja/make)

CMake configure runs on the **driving machine**, not in the VM. By the time
hpcc intercepts a compile invocation:
- `FetchContent` has already fetched.
- Configure-generated headers (`config.h.in` → `config.h`) exist.
- Code-generator outputs (protoc, moc, flex) exist (they're produced by
  prior ninja steps on the driving machine).

All those files live in `build/` on disk. The VM sees them via virtio-fs
mount of the source+build tree. **No special handling needed.** The only
edge case is `add_custom_command` that needs network during build — rare
and considered bad practice in any sandboxed setup.

Document explicitly: "the hpcc VM has no internet access; configure and
codegen run on the driving machine before the build starts."

### 4.7 Determinism

Reproducible compilation gives byte-identical outputs for byte-identical
inputs, which raises cache hit rates and gives auditors clean
`(input_hash, output_hash, image_digest, run_id)` tuples.

Image-pinning gets you most of the way. Source-level non-determinism still
needs flag injection:

- `-Werror=date-time` — fail on `__DATE__`/`__TIME__`/`__TIMESTAMP__`.
- `-ffile-prefix-map=/build=.` — strip absolute paths from binaries.
- `-frandom-seed=<digest>` — pin GCC's symbol-name randomness.
- Pin locale, timezone, hostname inside the VM.
- Document caveats around LTO/PGO.

The Debian Reproducible Builds project has solved most of this; hpcc just
auto-injects the flags.

### 4.8 Scheduler

- Central coordinator: `hpcc scheduler --listen :9091`.
- Workers connect and register: image digests they have rootfs for, free vCPU,
  current load.
- Scheduler routes jobs based on:
  - Tenant → VM affinity (sticky routing — same tenant lands on the same VM).
  - Image digest match (worker has the rootfs).
  - Current load / available capacity.
  - Optional: network proximity (RTT-based scoring).
- Spinning up a new VM for an unseen `(tenant, image)` pair is the scheduler's
  responsibility, not the worker's.

### 4.9 Worker

- `hpcc worker --scheduler <addr>` — connects to the scheduler, manages local
  Firecracker pool.
- Maintains per-tenant VMs, snapshots on idle, evicts under pressure.
- Routes incoming jobs to the right VM via vsock to its `hpcc-agent`.
- Receives compile result back over vsock; returns it to the scheduler/client.

### 4.10 Wire Protocol: gRPC for the Control Plane

For scheduler↔worker, daemon↔scheduler, and client↔worker compile RPC: **gRPC
unary `Compile(CompileRequest) returns (CompileResponse)`.** Reasons specific
to this hop:

- Per-RPC zstd compression. Preprocessed C++ (when the `preprocessed`
  fallback mode kicks in) compresses ~5–10×. **This is the largest single
  perf lever; flip it on.**
- HTTP/2 multiplexing. Hundreds of concurrent in-flight jobs over one
  connection per worker.
- First-class cancellation — `ctx.Cancel()` propagates and kills the in-VM
  compiler. Build aborts feel snappy.
- mTLS in one line. Auditor-friendly.
- Per-call deadlines map to the job timeout cleanly.
- Don't stream stdout/stderr. Compilers don't produce progressive output;
  buffer and return at the end.

Explicitly **not** Kafka. Kafka is a log/streaming system, not RPC. Per-job
broker round-trips, large-message friction, request-reply via reply-topics,
and durability you don't want — wrong tool. (Kafka is fine, later, as a
telemetry/audit sidecar — not in v1.)

### 4.11 Failure Handling

- No workers available / scheduler unreachable: compile locally.
- Worker fails mid-job: scheduler reassigns to another worker; hpcc retries
  on local as last resort.
- Per-job timeout (configurable, default 60s).
- VM crash mid-job: the worker resurrects the VM from its snapshot (or cold
  boot) and the scheduler retries the job.

### 4.12 Audit Trail

For each compile job, log:
- VM image digest, vCPU/mem caps.
- Source digest, flag set, cache key.
- Output digest, exit code, duration.
- Tenant ID, scheduler ID, worker ID, VM ID.

This is the table format banks want to see — every artifact is reproducible
from its row.

### Milestone

A 16-core machine effectively compiles with `-j64` by distributing to
3 other machines in the cluster. Each tenant's compiles run in their own
Firecracker VM, reused across the build, snapshotted on idle, with no
network access from inside the VM.

---

## Phase 5: Observability & Polish

Make it easy to understand what hpcc is doing and why.

### 5.1 Stats & Metrics

- `hpcc stats` — hit rate (local/remote/distributed), miss reasons, cache
  size, active VMs, compilation time saved.
- Prometheus endpoint on the daemon, server, and scheduler.
- Per-build summary printed at build end.

### 5.2 Cache Inspection

- `hpcc inspect <hash>` — show metadata: what was compiled, when, which flags,
  what the inputs hashed to, where the result came from.
- `hpcc inspect <file>` — show what cache key would be computed for a file
  with the current flags.

### 5.3 Miss Reasons

When a cache miss occurs, log exactly why:
- New file (never seen before)
- Source changed (diff the preprocessed output if previous version exists)
- Flags changed
- Toolchain image digest changed
- Header changed (identify which header)

`hpcc explain <file>` — show why the last compilation was a miss.

### 5.4 Configuration

Config file: `~/.config/hpcc/config.yaml` (or `/etc/hpcc/config.yaml` for
system-wide). Settings include:

- `cache.dir`, `cache.max_size`, `cache.compression`
- `remote.backend`, `remote.url`, `remote.timeout_read`, `remote.timeout_write`
- `scheduler.url`
- `vm.image` — default container image to use as the build environment
- `vm.idle_timeout`, `vm.session_timeout`, `vm.memory`, `vm.vcpus`
- `determinism.auto_inject_flags` (bool)
- `log.level`, `log.file`

### 5.5 Eviction

- LRU with max size (default 10GB) for local cache.
- LRU for converted rootfs blobs and VM snapshots.
- `hpcc clean --max-size 5G`, `hpcc clean --max-age 30d`.
- Daemon runs periodic eviction in the background.

---

## Project Structure

```
cmd/
  root.go           — base cobra command
  wrap.go           — hpcc wrap <compiler> [args...]
  start.go          — hpcc start (daemon)
  stop.go           — hpcc stop
  status.go         — hpcc status
  stats.go          — hpcc stats
  clean.go          — hpcc clean
  inspect.go        — hpcc inspect
  server.go         — hpcc server (remote cache server)
  scheduler.go      — hpcc scheduler
  worker.go         — hpcc worker
internal/
  compiler/
    detect.go       — identify compiler from argv[0] or path
    grammar.go      — GNU vs MSVC argument grammar tables
    parser.go       — Parse() → *Invocation
    invocation.go   — Invocation type (parsed argv)
    invoke.go       — run the compiler, capture output
    preprocess.go   — run -E or -M for manifest mode
  hasher/
    hasher.go       — compute cache keys (preprocess and manifest modes)
  cache/
    local.go        — local disk cache (CAS)
    remote.go       — RemoteCache interface
    s3.go           — S3 backend
    server.go       — hpcc-server backend (HTTP client)
    entry.go        — CacheEntry type
    eviction.go     — LRU eviction
  daemon/
    daemon.go       — main loop, Unix socket listener
    handler.go      — handle compile requests, stats, clean
    dedup.go        — in-flight deduplication
  protocol/
    compile.proto   — protobuf definitions (shared by daemon, scheduler,
                      worker, agent)
    *.pb.go         — generated code
  scheduler/
    scheduler.go    — job assignment, worker pool
    worker_conn.go  — connection to a worker
    routing.go      — tenant→VM affinity, image-digest matching
  worker/
    worker.go       — host-side worker
    vmpool/         — Firecracker pool: lifecycle, snapshot, eviction
    image/          — OCI image → rootfs conversion + cache
    agent/          — in-VM agent binary (separate go module, static)
  config/
    config.go
```

---

## Design Decisions

### Hashing Strategy

Start with **preprocess-then-hash**. Add **manifest mode** as a sibling for
Phase 4 server-side preprocessing — manifest mode is what enables hashing
without ever materializing preprocessed bytes on the client.

### Storage Format

Flat files on disk in a CAS layout. Avoid embedded databases for the local
cache — they add complexity and the filesystem is already a key-value store.
Consider badger/bolt only if metadata queries become a bottleneck.

### Compression

zstd for cached artifacts (level 3 default). zstd on every gRPC call
carrying preprocessed source — flip the flag.

### Concurrency

Daemon handles concurrent requests with goroutines. Deduplication is a
mutex-protected map of `chan struct{}`.

### Sandbox Model

**Firecracker, full stop.** Namespace-based sandboxes (bwrap, nsjail, gVisor)
are out of scope — the deployment target is regulated environments where the
KVM boundary is the boundary auditors recognize, and maintaining two
sandbox backends to support a use case we don't have isn't worth the
complexity.

### Compiler/Invocation Split

`Compiler` is a stateless strategy (one per detected toolchain).
`Invocation` is pure data (one per parsed argv). Don't conflate them — the
`Invocation` is what serializes over the wire to a worker.

### Argument Grammars

Two grammars, GNU and MSVC. clang-cl/icx-cl/etc. reuse MSVC. Drive the
parser from a flag spec table, not a giant switch.

### Wire Protocols

| Hop | Protocol | Why |
|---|---|---|
| Wrapper ↔ daemon (local) | Length-prefixed protobuf over Unix socket | Wrapper invoked thousands of times per build; gRPC runtime is too heavy for the hot path |
| Cache (Phase 3) | HTTP `GET`/`PUT`/`HEAD` | S3-compatible, signed URLs, CDN-friendly |
| Control plane (scheduler/worker/client compile) | gRPC unary `Compile` | Per-call zstd, multiplexing, cancellation, mTLS, deadlines |
| In-VM agent | gRPC over vsock | Same proto, no network device |
| Telemetry sidecar (later, optional) | Kafka | Append-only event log of compilations for dashboards/audit |

Explicitly **not** Kafka for the compile path. Wrong shape — pub/sub log,
not RPC.

### Image-as-Environment

The user declares their build environment by handing hpcc a container image.
The image digest *is* the toolchain identity for cache keying. Workers
maintain a digest-keyed rootfs cache; one image used by N developers means
one rootfs on disk.

### Determinism

Auto-inject the standard reproducible-build flags
(`-Werror=date-time`, `-ffile-prefix-map`, `-frandom-seed`). Pin locale,
timezone, hostname inside the VM. Document LTO/PGO caveats.

### Security

- Daemon listens on a Unix socket with filesystem permissions — no network.
- Remote cache server requires authentication.
- Workers run compilations in Firecracker VMs with no network device.
- Image rootfs is read-only, optionally dm-verity signed.
- Per-VM audit log: image digest, source digest, flag set, output digest,
  duration, exit code, tenant/scheduler/worker/VM IDs.

---

## Build Order

If you're working on this solo, this is the recommended order:

1. `internal/compiler/grammar.go` + `parser.go` — flag spec tables and parser
2. `internal/compiler/detect.go` — compiler detection
3. `internal/compiler/invocation.go` — Invocation type
4. `internal/compiler/preprocess.go` — `-E` and `-M` modes
5. `internal/compiler/invoke.go` — run compiler, capture output
6. `internal/hasher/hasher.go` — preprocess-mode hashing first
7. `internal/cache/entry.go`, `internal/cache/local.go` — local CAS
8. `cmd/wrap.go` — wire it together
9. End-to-end test: `hpcc wrap gcc -c foo.c -o foo.o`
10. `internal/protocol/compile.proto` — define the wire format once
11. `internal/daemon/` — daemon + Unix socket (length-prefixed proto)
12. `cmd/start.go`, `cmd/stop.go`, `cmd/status.go`
13. `internal/cache/remote.go` + `internal/cache/server.go` + `cmd/server.go`
14. Manifest-mode hashing in `internal/hasher/`
15. `internal/worker/image/` — OCI → rootfs conversion + cache
16. `internal/worker/vmpool/` — Firecracker lifecycle, snapshot, eviction
17. `internal/worker/agent/` — in-VM gRPC agent over vsock
18. `internal/scheduler/` — gRPC, tenant routing, image matching
19. Polish: stats, inspect, explain, config, eviction
