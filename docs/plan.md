# hpcc — Implementation Plan

hpcc is a distributed compilation and caching tool. A drop-in compiler wrapper
that caches build artifacts locally and remotely, and optionally distributes
compilation across a cluster of workers — with a strong tenant-isolation story
suitable for environments (banks, regulated enterprises) where running
user-supplied compilation on shared hardware is politically untenable.

---

## Status

| Phase | Description | Status |
|-------|-------------|--------|
| [Phase 1](#phase-1-core-compiler-wrapping) | Core Compiler Wrapping | Done |
| [Phase 2](#phase-2-daemon-architecture) | Daemon Architecture | Done |
| [Phase 3](#phase-3-remote-cache) | Remote Cache | Not started |
| [Phase 4](#phase-4-distributed-compilation-in-per-tenant-vms) | Distributed Compilation in Per-Tenant VMs | Not started |
| [Phase 5](#phase-5-observability--polish) | Observability & Polish | Not started |

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

### Milestone ✅

`hpcc wrap gcc -c foo.c -o foo.o` compiles on first run, returns the cached
result on second run with no recompilation.

---

## Phase 2: Daemon Architecture

Move the cache logic into a long-running daemon so multiple concurrent compiler
invocations share state efficiently.

### 2.1 Daemon Process

- `hpcc start` — run the daemon as a **foreground process**, listening on a
  **TCP socket bound to loopback** (`127.0.0.1:<port>`, default `:9080`).
  Foreground execution keeps the process model simple: the user (or a
  process supervisor like systemd, launchd, or a container entrypoint)
  owns the lifecycle. TCP keeps the daemon portable across Linux, macOS,
  and Windows — Unix domain sockets exist on Windows 10+ but
  tooling/library support is uneven, and the wrapper has to run on every
  dev machine. Loopback-only binding keeps the surface area equivalent to
  a Unix socket: no off-host reachability.
- A small **port handshake file** at `<UserConfigDir>/hpcc/daemon.json`
  (resolved via Go's `os.UserConfigDir()` —
  `~/.config/hpcc/daemon.json` on Linux,
  `~/Library/Application Support/hpcc/daemon.json` on macOS,
  `%AppData%\hpcc\daemon.json` on Windows) records `{port, pid,
  auth_token}` so the wrapper can find the daemon without a fixed port.
  Same lookup path as the config file (§5.4) — one directory per user, no
  separate runtime-vs-config split. File permissions: `0600` (Unix) /
  current-user ACL (Windows).
- Per-connection auth: wrapper reads the token from the handshake file and
  presents it on connect. Cheap defense against another local user
  connecting to the loopback port on a shared machine.
- Graceful shutdown via `SIGINT` / `SIGTERM` (Unix) or `Ctrl-C` (all
  platforms). No separate `stop` command needed — the process supervisor
  or the user's terminal handles it.

### 2.2 Client-Server Protocol

**Length-prefixed protobuf over a loopback TCP connection** — *not* gRPC. The
wrapper binary is invoked thousands of times per build and must start fast
and stay small; pulling in the gRPC runtime is overkill for local IPC. Same
`.proto` files as the Phase 4 control plane, just a thinner client.

Connection setup: wrapper reads `daemon.json`, dials `127.0.0.1:<port>`,
sends a one-byte version + the auth token as the first framed message, then
proceeds with normal request/response. Disable Nagle (`TCP_NODELAY`) — these
are short, latency-sensitive RPCs.

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

### Milestone ✅

`make -j16` with the daemon running deduplicates identical translation units
and reports stats from a single process.

---

## Phase 3: Remote Cache

Share cached artifacts across machines so a team doesn't recompile the same
code. The remote cache is just another `Store` implementation — no custom
server binary. Point hpcc at an S3-compatible bucket and it works.

### 3.1 S3 Store

A new `Store` implementation (`internal/cache/store/s3.go`) that speaks the
S3 A
A new `Store` implementation (`internal/cache/store/s3.go`) that speaks the
S3 API. Works with AWS S3, MinIO, R2, GCS (via S3 compatibility), etc.PI. Works with AWS S3, MinIO, R2, GCS (via S3 compatibility), etc.

Key layout mirrors the local disk store: `<first 2 hex>/<full key>/<name>`
as object keys inside the configured bucket/prefix.

Operations map directly:
- `Get` → `GetObject`
- `Put` → `PutObject`
- `Has` → `HeadObject`

No custom protocol, no proxy server, no extra binary. The user already
runs their object store; hpcc just writes to it.

### 3.2 Lookup Order

The runner walks stores in config order (typically local disk first, then
S3). On hit from a remote store, backfill to earlier (local) stores.

1. Local disk cache
2. S3 remote cache
3. Compile (locally or distributed — see Phase 4)
4. Push result to all configured stores

### 3.3 Failure Handling

- Remote cache operations have a configurable timeout (default: 2s reads, 5s
  writes).
- On timeout or error: log it, skip remote, compile. Never stall the build.
- Write failures are non-fatal — the build succeeds, the artifact just isn't
  shared.

### 3.4 Authentication

Standard AWS credential chain (env vars, `~/.aws`, instance profile, etc.).
No hpcc-specific auth layer.

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

### 4.1.1 Worker Runtime Abstraction (Linux vs. Windows)

The worker isolates each tenant in a microVM, but the *implementation* of
"microVM" depends on the host OS:

- **Linux hosts** → Firecracker microVMs running Linux guests. This is the
  primary target and what the rest of Phase 4 describes.
- **Windows hosts** → Windows Server containers with **Hyper-V isolation**.
  Each container runs in its own utility VM (Hyper-V partition), giving the
  same kernel-boundary property KVM gives us on Linux. Required for MSVC /
  legacy Windows-only projects where running the toolchain on Linux isn't
  an option.

A small `Runtime` abstraction in the worker selects the backend based on the
host OS and the requested image type (Linux OCI image vs. Windows base
image). Both backends expose the same operations to the rest of hpcc:
materialize image → boot/restore VM → exec compile job over a control
channel → snapshot/destroy on idle. The scheduler routes jobs to workers
that advertise a matching runtime.

**Out of scope for now** — only Linux/Firecracker is implemented in v1.
Windows support is an explicit follow-up: when it lands, it slots in behind
the same `Runtime` interface and the gRPC compile RPC, scheduler, cache,
audit log, and image-digest cache key all stay unchanged.

**Windows gotchas to plan for** when the Windows runtime lands (none of these
affect v1 or the parser):

- **Path normalization for cache keys.** UNC vs mapped drive (`\\fs\src\foo.cpp`
  vs `Z:\src\foo.cpp`) and `\\?\` extended-length forms must canonicalize to
  the same string in the hasher, or two developers compiling the same source
  via different mounts produce different keys. Couple with `/d1trimfile:` and
  `/PDBSourcePath:` (MSVC equivalents of `-ffile-prefix-map`) so embedded
  paths in objects/PDBs don't poison the hash.
- **MAX_PATH (260) limit.** Monorepo builds blow past this routinely. Workers
  need `LongPathsEnabled` registry, and the worker may rewrite paths to
  `\\?\` form before invoking `cl.exe`.
- **Source mounting into Hyper-V-isolated containers.** Hyper-V containers
  can't bind-mount host paths the way Linux containers do; SMB-into-container
  has identity/auth quirks (virtual accounts can't authenticate to shares).
  Likely shape: stage source onto a local volume the container mounts, with a
  stable in-container path (e.g. `C:\src`) decoupled from the host path.
  `--isolation=process` would sidestep this but loses the Hyper-V boundary
  the bank case demands.
- **Symlinks vs directory junctions.** Different semantics; pick a
  resolve-or-don't policy and stick to it for cache-key path canonicalization.

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

Firecracker supports **virtio-blk** and **vsock** — it does not support
virtio-fs. Source and cache are passed into the VM as block devices.

For each running VM:

- **`/dev/vda`** — Read-only base rootfs (ext4, per image digest, shared
  across VMs) + per-VM **overlayfs** writable upper (tmpfs) for `/tmp`,
  `/var`, build scratch.
- **`/dev/vdb`** — **squashfs** image of the project source + build tree
  (read-only). Rebuilt by the worker when the source tree changes.
  `mksquashfs` is fast on typical source trees (sub-second for most C++
  projects) and squashfs is compressed, so the block device stays small.
  Mounted read-only at `/src` inside the guest.
- **`/dev/vdc`** — ext4 scratch volume for build outputs (read-write).
  Mounted at `/out` inside the guest. The worker reads artifacts from
  here after the compile completes.
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

1. **`shared_root`** — worker packages the source tree into a squashfs
   image and attaches it as a virtio-blk device. Client sends
   `(source_path, flags, cwd)`; worker preprocesses + compiles.
   Realistic in container-pinned bank environments where workers have
   access to the same source volume.
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

All those files live in `build/` on disk. The worker packages both the
source and build trees into the squashfs image attached to the VM as
`/dev/vdb`. **No special handling needed.** The only edge case is
`add_custom_command` that needs network during build — rare and
considered bad practice in any sandboxed setup.

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

### 4.13 Paranoid Mode (Server-Side-Only Caching)

In the default model the client daemon has read-write access to the cache
stores (local disk, S3). A compromised or malicious client can poison the
cache — write a trojaned `.o` under a valid cache key and every subsequent
consumer of that key gets the bad artifact.

**`paranoid = true`** (config flag) moves all cache reads and writes to the
worker side of the boundary:

- The **client never reads or writes the cache.** It sends the compile
  request to the scheduler and receives the artifact bytes back in the
  `CompileResponse`. That's it.
- The **worker** (inside the VM or on the worker host, outside the VM) is
  the only process that calls `Store.Get` / `Store.Put`. Cache stores are
  configured on the worker, not the client.
- Cache keys are computed **server-side** from server-side preprocessing
  output, so the client cannot influence what key an artifact is stored
  under.
- The S3 / remote store credentials live on the worker fleet, never on
  developer laptops.

Trade-offs:
- Every compile is a network round-trip — no local cache hits, higher
  latency for repeated builds on the same machine.
- Workers need enough bandwidth to return artifacts to every client.
- Falls back to a local uncached compile if the scheduler is unreachable
  (same as §4.11).

This is the mode banks will run. In lower-trust environments it can be
combined with **signed artifacts**: the worker signs the `(cache_key,
output_digest)` tuple and the client verifies the signature before
writing the `.o` to disk, so even the wire path is tamper-evident.

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

**Format: TOML.** Picked over YAML because:
- No whitespace-sensitivity (YAML's "indent silently broke parsing"
  failure mode bites teams).
- No "Norway problem" (`country = no` parsing as boolean).
- Comments first-class.
- Familiar from Cargo, ruff, ripgrep, pyproject.toml.

Picked over JSON because JSON has no comments and is verbose for config.

Config file is loaded from, in order:
1. `$HPCC_CONFIG` if set (used for tests and dev overrides).
2. The OS user-config dir: `os.UserConfigDir()` resolves to
   `~/.config/hpcc/config.toml` on Linux,
   `~/Library/Application Support/hpcc/config.toml` on macOS,
   `%AppData%\hpcc\config.toml` on Windows.
3. (Future) `/etc/hpcc/config.toml` for system-wide defaults.

A missing file is fine — defaults apply. A malformed file is a hard
error so a typo isn't silently ignored.

Eventual settings, organized as TOML tables:

```toml
preprocessing_mode = "local"  # "local" | "remote"

[cache]
dir = "~/.cache/hpcc"
max_size = "10GB"
compression = 3

[remote]
backend = "s3"           # "hpcc-server" | "s3" | "redis"
url = "..."
timeout_read = "2s"
timeout_write = "5s"

[scheduler]
url = "..."
paranoid = false         # true = cache only on workers, never on client

[vm]
image = "..."
idle_timeout = "10m"
session_timeout = "8h"
memory = "2GB"
vcpus = 4

[determinism]
auto_inject_flags = true

[log]
level = "info"           # "debug" | "info" | "warn" | "error"
file = "..."
```

Implemented today: only `preprocessing_mode`. The rest land as their
phases do.

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
  start.go          — hpcc start (foreground daemon)
  stats.go          — hpcc stats
  clean.go          — hpcc clean
  inspect.go        — hpcc inspect
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
    store/
      store.go      — Store interface
      disk.go       — local disk cache (CAS)
      s3.go         — S3-compatible remote store
  daemon/
    daemon.go       — main loop, loopback TCP listener
    handshake.go    — write/read daemon.json (port, pid, auth token)
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

**Hardware-virtualized boundary, always.** Namespace-based sandboxes (bwrap,
nsjail, gVisor) are out of scope — the deployment target is regulated
environments where the kernel boundary is the boundary auditors recognize.

A `Runtime` abstraction in the worker picks the backend by host OS:

- **Linux** → Firecracker microVMs (primary target, v1).
- **Windows** → Hyper-V-isolated Windows containers (follow-up; required
  for MSVC and legacy Windows-only projects).

The scheduler matches jobs to workers by runtime + image digest. Everything
above the runtime layer (gRPC compile RPC, cache, audit, image-digest
identity) is OS-agnostic.

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
| Wrapper ↔ daemon (local) | Length-prefixed protobuf over loopback TCP (`127.0.0.1`) with a per-daemon auth token | Wrapper invoked thousands of times per build; gRPC runtime is too heavy for the hot path. TCP (over Unix sockets) for portability — the wrapper has to run on Windows too |
| Cache (Phase 3) | S3 API (`GetObject`/`PutObject`/`HeadObject`) | Direct to object storage, no proxy; works with AWS S3, MinIO, R2, GCS |
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

- Daemon listens on loopback TCP only (`127.0.0.1`); a per-daemon auth token
  in a `0600` handshake file gates connections.
- Remote cache uses S3 auth (AWS credential chain).
- Workers run compilations in Firecracker VMs with no network device.
- Image rootfs is read-only, optionally dm-verity signed.
- Per-VM audit log: image digest, source digest, flag set, output digest,
  duration, exit code, tenant/scheduler/worker/VM IDs.
- **Paranoid mode** (`paranoid = true`): all cache reads/writes happen on
  the worker side only — clients never touch the cache stores and never
  hold remote-store credentials. Prevents cache poisoning by compromised
  developer machines (§4.13).

---

## Build Order

### Done so far

1. **GNU + MSVC parsers** — `internal/compiler/grammar.go`,
   `grammar_msvc.go`, `parser.go`, `parser_msvc.go` (+ `parser_test.go`,
   `parser_msvc_test.go`). Flag spec tables driving both grammars.
2. **Compiler detection** — `internal/compiler/detect.go` (+
   `detect_test.go`). Identify the toolchain from `argv[0]`.
3. **Compiler/Invocation types** — `internal/compiler/compiler.go`,
   `invocation.go`, `clang.go` (+ `clang_test.go`), `cl.go`. Stateless
   strategy + parsed-argv data shape.
4. **Preprocess / dep-gen** — `internal/compiler/preprocess.go` (+
   `preprocess_test.go`). `-E` and `-M` invocations.
5. **Cache-key hashing** — `internal/compiler/cache_key.go` (+
   `cache_key_test.go`). Preprocess-mode hashing.
6. **Drop-in wrapper CLI** — `main.go`, `cmd/root.go`, `cmd/wrap.go`.
   `hpcc wrap <compiler> [args...]` and symlinked-name dispatch.
7. **Context plumbing** — `internal/compiler/context.go`. `context.Context`
   threaded through compiler entry points.
8. **Cache configuration + enums** — `internal/config.go`,
   `internal/filesize.go`, `internal/cache/cache.go`, `internal/cache/v1.go`,
   `internal/enum/{cache_type,family,invocation_mode,language,preprocessing_mode}.go`.
9. **Disk cache store** — `internal/cache/store/store.go`,
   `internal/cache/store/disk.go`. CAS layout on disk.
10. **Runner** — `internal/runner/runner.go`, `stores.go`, `context.go`
    (+ `runner_test.go`). Glue between parsed invocation, hasher, cache
    store, and the real compiler.
11. **Stats + clean commands** — `cmd/stats.go`, `cmd/clean.go`.
12. **Wire protocol** — `internal/protocol/compile.proto` (+
    `gen/compile.pb.go`). Length-prefixed protobuf messages for the
    daemon ↔ client path.
13. **Daemon + client** — `internal/daemon/daemon.go` (+
    `daemon_test.go`), `internal/daemon/client/client.go`. Loopback TCP
    listener, handshake file, per-connection auth, singleflight-based
    deduplication of identical in-flight compilations (§2.3),
    `TCP_NODELAY` on all connections. `cmd/start.go` launches the daemon.

### Remaining

14. `internal/cache/store/s3.go` — S3-compatible remote store.
15. Manifest-mode hashing alongside `internal/compiler/cache_key.go`.
16. `internal/worker/image/` — OCI → rootfs conversion + cache.
17. `internal/worker/vmpool/` — Firecracker lifecycle, snapshot, eviction.
18. `internal/worker/agent/` — in-VM gRPC agent over vsock.
19. `internal/scheduler/` — gRPC, tenant routing, image matching.
20. Polish: inspect, explain, eviction, Prometheus endpoints.
