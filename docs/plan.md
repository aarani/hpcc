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
| [Phase 3](#phase-3-remote-cache) | Remote Cache | Done |
| [Phase 4](#phase-4-distributed-compilation-in-per-tenant-vms) | Distributed Compilation in Per-Tenant VMs | In progress |
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

### 3.1 S3 Store ✅

`internal/cache/store/s3.go` speaks the S3 API via `aws-sdk-go-v2`. Works
with AWS S3, MinIO, R2, GCS-via-S3-compat. Object layout:

```
cache/<first-2-hex>/<full-hex-key>/<name>
```

The leading `cache/` prefix is non-negotiable — it lets the bucket be
shared with other tools (telemetry, lifecycle audit logs, anything an
operator already has in there) without scan loops tripping on stray
objects. The two-char shard prefix is historical; modern S3 doesn't
need it for partition-throughput reasons but it matches the disk
store's layout and keeps any external tooling that pivots on prefixes
happy.

Operations map directly:
- `Get` → `GetObject` (returns `nil, nil` on `NoSuchKey` — the cache-miss
  convention shared with the disk store)
- `Put` → `PutObject` (followed by watermark-gated eviction; see §3.5)
- `Has` → `ListObjectsV2(prefix=entryPrefix, MaxKeys=1)` — one round-trip,
  early-terminating, doesn't require object-level HEAD permissions on a
  specific filename

### 3.2 Lookup Order

The runner walks stores in config order (typically local disk first, then
S3). On hit from a remote store, backfill to earlier (local) stores.

1. Local disk cache
2. S3 remote cache
3. Compile (locally or distributed — see Phase 4)
4. Push result to all configured stores

### 3.3 Failure Handling

Per-call deadlines synthesised from struct-level timeouts (the disk-store
`Store` interface predates context plumbing): 2s reads, 5s writes, 2s
existence checks, 30s for paginated list ops, 10s for the init
smoke-test. A stuck request fails fast with a clear error rather than
hanging the build.

On any S3 error: log it, skip remote, compile. Never stall the build.
Write failures are non-fatal — the build succeeds, the artifact just
isn't shared.

`io.LimitReader` caps Get bodies at 1 GiB by default (configurable via
`S3Options.MaxBlobBytes`). A malicious or runaway upload past the cap
errors loudly instead of OOMing the worker.

### 3.4 Authentication

Standard AWS credential chain (env vars, `~/.aws`, instance profile,
IRSA, etc.). `access_key`/`secret_key` in TOML override the chain for
dev/MinIO setups but production deployments mount creds via the chain
and leave the config fields empty.

For non-AWS endpoints (MinIO, Ceph, R2 with custom URLs), set
`endpoint` to the URL; the SDK switches to path-style addressing
automatically (`http://host/bucket/key`) since virtual-hosted style
needs DNS for the bucket name and most local setups don't have it.

`region` semantics:
- Empty for AWS (the credential chain supplies the default).
- `"auto"` for Cloudflare R2.
- Any non-empty string for MinIO/Ceph (the value is only used to
  satisfy the SDK's signing requirement on custom endpoints).

### 3.5 Eviction

In-memory size estimate, watermark-gated. Bumped on every `Put`; a full
bucket scan + LRU delete only fires once the estimate exceeds
`max_size + max_size/10`. Reduces eviction frequency from "one bucket
scan per Put" to "one per `max_size/10` of writes" — a 10× cost
reduction that matters when a `make -j32` produces thousands of cache
misses.

Eviction is single-flighted across goroutines via an `atomic.Bool`;
concurrent `Put`s past the watermark just bump the estimate and trust
the in-flight eviction. A duplicate `DeleteObjects` from a racing
worker is a no-op (`NoSuchKey` is fine), so the design is tolerant of
multi-worker eviction races without needing distributed locking.

`max_size` is optional — leave it empty in TOML for "no in-process
eviction." That's the recommended production setting: configure S3
lifecycle policies for retention by age (S3-side, no scan cost) and let
hpcc just write through.

### 3.6 Bucket Provisioning

`auto_create = true` (default false) attempts `CreateBucket` if the
bucket isn't reachable at startup. Sane only for local MinIO/dev
setups: in production the bucket is provisioned by infra-team-only
and a worker shouldn't even hold `s3:CreateBucket` IAM permissions.
Init smoke-test uses a prefix-scoped `ListObjectsV2` rather than
`HeadBucket` so the worker works with object-level IAM scoped to
`arn:aws:s3:::<bucket>/cache/*`.

### Milestone ✅

Developer A compiles a file. Developer B on a different machine gets a
remote cache hit for the same file without compiling.

---

## Phase 4: Distributed Compilation in Per-Tenant VMs

**Progress so far:**

- **Done:** §4.1.1 Runtime abstraction (`internal/worker/runtime`,
  with `DangerouslyExecOnHost` as the dev backend and a real
  `Firecracker` driver in production); §4.3 image→rootfs pipeline
  (`tar -xpf` + `mkfs.ext4 -d`, post-extract agent injection,
  pre-created standard mountpoints for distroless-style images);
  §4.4 VM layout under jailer (vsock device, no NIC, kernel + rootfs
  staged into the chroot); §4.4.1 in-VM `hpcc-agent` (separate Go
  module, PID-1 init that mounts `/proc` `/sys` `/dev` `/tmp` `/run`
  + zombie reaping + bidi-streaming gRPC `AgentService.Exec` over
  AF_VSOCK port 17727); shared `proto/agent` module so the runner and
  agent compile against one wire schema; §4.8 route-only scheduler;
  §4.9 worker (Compile RPC, image catalogue + idle eviction, per-tenant
  container pool with idle/session TTLs, Ed25519 task-JWT verification);
  §4.10 client→worker gRPC compile path with per-call zstd; §4.12
  per-job audit records; §4.13 paranoid-mode plumbing on the worker.
  An integration suite (`firecracker_e2e_test.go`, runs in CI under
  `sudo` on Ubuntu with KVM) downloads firecracker + jailer, builds
  a real chainguard `gcc-glibc` rootfs, and compiles a one-line C
  source end-to-end through the full vsock + agent pipeline.

- **Open:**
  - §4.2 snapshot/restore — today the pool just keeps warm VMs in RAM
    on idle; cold-restart on resume. Driving Firecracker directly
    means snapshot/restore is on the table; not yet wired.
  - §4.5 CAS-mode source staging on the worker — only `PREPROCESSED`
    works end-to-end. CAS needs blob-list materialization (reuse the
    Phase 3 store as the CAS).
  - §4.1.1 Windows hcsshim path — runtime interface ready; backend
    not implemented.
  - §4.11 VM-crash reaping with scheduler reroute — partial today
    (the runtime surfaces process exit, but the worker doesn't yet
    notify the scheduler to drop the dead VM from routing).
  - §4.14 rootfs extraction hardening — the `tar -xpf` shell-out is
    the soft underbelly of the image pipeline against
    attacker-controlled images. Plan in §4.14.

Farm out compilation to remote workers, isolated in **raw Firecracker
microVMs driven directly by hpcc**, to parallelize beyond local CPU count
and provide a defensible isolation boundary for regulated environments.

### 4.1 Why Raw Firecracker

The target deployment is regulated enterprises (banks, finance) where the
security review isn't asking *"is this technically sufficient?"* — it's
asking *"is this a boundary auditors recognize?"* Firecracker gives you:

- A separate kernel + KVM boundary. Hard to argue with.
- Clean per-tenant isolation: Alice's compile cannot touch Bob's source via
  shared `/proc`, page-cache side channels, or kernel CVE.
- Trivial network policy: **the VM has no NIC.** No virtio-net device is
  attached, so there is no exfiltration argument to have.
- Per-VM lifecycle events make a clean audit trail.

#### Why not gVisor

gVisor is a userspace kernel intercepting syscalls. Technically strong, but
it is **not** the boundary a bank security review recognises. The
differentiator hpcc sells — *separate kernel, KVM boundary* — is exactly what
gVisor is not, regardless of the engineering merits. Switching to gVisor
would torch the pitch. Namespace-based sandboxes (bwrap, nsjail) are out for
the same reason.

#### Why not firecracker-containerd

firecracker-containerd is an AWS-published shim that drives Firecracker via
containerd. On paper it would buy hpcc:

- OCI image → ext4 rootfs via the devmapper snapshotter — no rootfs builder
  to own.
- `Task.Exec` per compile, with a guest agent already handling stdio + exit
  codes — no bespoke vsock RPC server inside the VM.
- A "swap shim for hcsshim and the Windows backend just works" story.

In practice the project has stagnated: the devmapper snapshotter is creaky,
release cadence is effectively dead, and depending on it now is depending on
unmaintained infrastructure. For a project whose entire bet is *long-lived,
auditable, defensible in regulated environments,* building on stagnant
orchestration is the wrong direction. Better to own a small amount of code
we control than carry someone else's abandonware.

#### What raw Firecracker costs us

The trade is real. We took on:

- **OCI image → rootfs pipeline.** `crane.Pull`, flatten via
  `mutate.Extract`, shell out to `tar -xpf` + `mkfs.ext4 -d` (the
  go-diskfs ext4 writer is unusable on real-sized rootfs — it
  fails extent-tree promotion past ~4 inline extents and can't
  initialize journals on multi-GB images). Bounded scope, ~250
  lines. See §4.3.
- **In-VM agent over vsock.** A tiny static binary (~10 MB stripped)
  inside the guest that speaks one bidi-streaming gRPC: header in,
  input file chunks in, stdio out, result out, output file chunks
  out. Replaces what firecracker-containerd's guest agent did. The
  pause binary kept around for the Windows containerd path; the
  Linux equivalent is now its own `agent/` Go module. See §4.4.1.
- **Direct Firecracker VMM API driver.** `vmlinux` boot, drives, vsock —
  nothing exotic. No CNI, no network device.
- **No "containerd everywhere" framing for Windows.** Windows still uses
  containerd + hcsshim (Hyper-V isolation); the unification point between
  the two backends moves from *the containerd API* to *hpcc's `Runtime`
  interface* (§4.1.1).

What we keep — and these are the things that matter — is the entire security
pitch: KVM boundary, no-NIC story, per-job audit trail,
image-digest-as-toolchain-identity. A bank's security review is asking about
the boundary, not who orchestrates it.

#### What raw Firecracker buys back

Items that were downgraded under firecracker-containerd come back:

- **Snapshot/restore for VM warm-up.** Driving Firecracker directly puts the
  §4.2 "snapshot-on-idle, ~10ms restore" approach back on the table instead
  of the "warm in RAM, cold-boot on resume" fallback.
- **Tighter control over kernel + boot config.** No shim opinions to fight.
- **One fewer moving part on the worker host** — no containerd daemon, no
  shim, no devmapper pool to babysit on Linux.

### 4.1.1 Worker Runtime Abstraction (Linux vs. Windows)

The two host OSes get different drivers, unified behind hpcc's `Runtime`
interface (`internal/worker/runtime/runtime.go`):

- **Linux hosts** → raw Firecracker driver. Each per-tenant container is
  one Firecracker microVM with a Linux guest. hpcc owns the image→rootfs
  builder (§4.3), VMM lifecycle, and the in-VM agent (§4.4.1). Primary
  target for v1.
- **Windows hosts** → containerd + **hcsshim runtime** with
  `--isolation=hyperv` → each container runs in its own utility VM
  (Hyper-V partition), giving the same kernel-boundary property KVM gives
  us on Linux. Required for MSVC / legacy Windows-only projects.

The unification point is hpcc's `Runtime` interface (Start container, Exec,
Stop) — not the orchestrator. The two backends share nothing below that
interface, but everything above (gRPC compile RPC, scheduler, cache, audit
log, pause+agent injection, image-digest cache key) is OS-agnostic. The
scheduler routes jobs to workers that advertise a matching runtime + image
digest.

**Out of scope for v1** — only the Linux/Firecracker backend is implemented.
Windows support is an explicit follow-up: when it lands, it slots in
behind the same `Runtime` interface and nothing above the runtime layer
changes.

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

- **One long-running VM per active tenant session.** On Linux this is a
  Firecracker microVM with the hpcc-agent (§4.4.1) as PID 1, blocking on
  the gRPC server with zero CPU between compiles. On Windows this is a
  Hyper-V utility VM under hcsshim, with the pause binary as PID 1.
- **Per-compile work** is dispatched as one Exec into the running VM —
  Linux via the in-VM agent's bidi gRPC stream over vsock, Windows via
  containerd `Task.Exec`. No shell required in the user's image.
- **Idle timeout** (e.g. 5–15 min) → snapshot the VM and unload it from
  memory; ~10ms restore on next demand. Driving Firecracker directly
  makes this practical (under firecracker-containerd it had been
  downgraded to "warm in RAM, cold-boot on resume" because the
  snapshot/restore story was rough). The pool today implements the
  warm-in-RAM half; snapshot/restore is open follow-up.
- **LRU eviction** of warm/snapshotted VMs under memory or disk pressure.
- **Hard session timeout** (e.g. shift change, N hours) → blow the VM away,
  discard the snapshot. Long-lived per-tenant state accumulates and someone
  will eventually ask what's in it. Already enforced by `PooledRuntime`
  via `vm.session_timeout`.

### 4.3 Container Image as the Build Environment

The user supplies their toolchain by handing hpcc a **container image**
(any OCI-compatible registry: Docker Hub, GHCR, ECR, internal mirrors).

On the **Linux/Firecracker** path the worker pulls layers (via
`go-containerregistry`), flattens them via `mutate.Extract` into a
single tar spool, shells out to `tar -xpf` to materialize the tree
under a staging directory, drops `/.hpcc/agent` (and pre-creates
`/proc /sys /dev /tmp /run` mountpoints for distroless-style images
that omit them), then runs `mkfs.ext4 -d <staging> <output.ext4>` to
seal the rootfs. `mkfs.ext4 -d` populates the ext4 directly from the
staging tree in one pass — no mount required, no root/loop dance,
handles hardlinks/symlinks/modes natively. The prepared rootfs is
cached on disk keyed by the user-image digest.

Why shell out instead of using a pure-Go ext4 writer (go-diskfs)?
go-diskfs's ext4 implementation is incomplete on the write path — it
fails extent-tree promotion on multi-MB files (the agent itself is
~10 MB) and journal initialization on rootfs sizes typical of real
toolchain images (~1 GB and up). e2fsprogs' `mkfs.ext4` has been the
canonical implementation for two decades and is on every Linux that
could host a worker; the dependency cost is one stable, package-managed
binary.

On the **Windows/hcsshim** path containerd's image pull + snapshotter
does the equivalent.

- **Image digest is the toolchain identity** for cache keys. Same image
  used by 50 developers → one prepared rootfs on the worker, one
  toolchain identity in the cache.
- The kernel is **hpcc's**, not the image's — the worker boots the microVM
  with a `vmlinux` hpcc provides, ignoring whatever kernel the image might
  ship.

#### 4.3.1 Pause-and-Agent Injection

The VM needs a long-running PID 1 that (a) stays alive between compiles,
(b) brings up the writable kernel filesystems the rootfs is read-only
about, and (c) accepts Exec requests from the worker host. User images
can be anything — distroless, scratch — so hpcc cannot rely on `sh`,
`sleep`, or any other host-provided binary being present.

Instead, hpcc injects one tiny static binary into every prepared image,
which fills all three roles:

- **Linux**: a statically-linked `hpcc-agent` (~10 MB stripped). Acts as
  the long-running PID 1: `setupInit` mounts `/proc`, `/sys`, `/dev`
  (devtmpfs), `/dev/pts`, `/dev/shm`, `/tmp`, and `/run` with the
  conventional flag set; a SIGCHLD-driven reap loop drains zombies the
  compiler's helper processes leave behind; a gRPC server on AF_VSOCK
  port 17727 serves `AgentService.Exec` (§4.4.1). Lives in the `agent/`
  module of the repo as a separate Go module so the in-VM binary doesn't
  drag in the worker's heavy dependency graph.
- **Windows**: `hpcc-pause.exe` only — Windows uses containerd `Task.Exec`
  via hcsshim, so no in-VM agent is needed; the pause binary plays the
  Kubernetes-style "stay alive as PID 1" role.

Injection happens at image-prep time on the worker: hpcc writes
`/.hpcc/agent` (or `/.hpcc/pause.exe`) into the prepared rootfs and the
kernel boot args set `init=/.hpcc/agent`. The user's image bytes are not
modified — the prepared rootfs is a separate artifact built per
user-image-digest, so the worker-side rootfs identity always pins the
agent that will run inside it.

This makes the design **image-agnostic**: hpcc works with any OCI image
the user wants to bring, including minimal/distroless images that have no
shell at all.

### 4.4 VM Layout

On **Linux/Firecracker** each per-tenant microVM is configured directly via
the Firecracker VMM API:

- **Rootfs drive** — the prepared rootfs from §4.3, attached read-only at
  `/dev/vda`. Inside the guest the agent mounts a tmpfs over `/tmp`,
  `/run`, `/dev/shm` so everything writable is RAM-backed; per-Exec
  staging dirs (`/run/hpcc/src/<exec_id>/`, `/run/hpcc/out/<exec_id>/`)
  live there.
- **No source/output drives.** The original design had per-RPC virtio-blk
  drives mounted at `/src` and `/out`. Replaced by inline file chunks
  carried over the agent's gRPC stream (§4.4.1): much simpler, no
  hot-attach dance, the agent stages bytes into its tmpfs and streams
  outputs back. Tradeoff is memory pressure for huge include closures
  in CAS mode — the guest needs RAM to hold the working set. Easy
  escape hatch (virtio-fs over vsock for host-dir passthrough) if any
  workload makes this an issue.
- **Init** — `/.hpcc/agent`, kept alive for the life of the VM and
  serving the bidi `Exec` stream over vsock (§4.4.1).
- **Vsock** — one virtio-vsock device. The host reaches the agent via
  Firecracker's UDS bridge (`CONNECT <port>\n` handshake on a fresh
  UDS connection). Vsock is the only host↔guest channel.
- **No network.** No virtio-net device is configured; there is nothing for
  the guest to talk to off-host. No exfiltration argument to have.

Two operational details worth recording:

- **Mount-namespace path reach.** Jailer 1.10+ unshares
  `CLONE_NEWNS` and mounts tmpfs over `/run` inside the chroot. From
  outside that namespace, `<chrootRoot>/run/firecracker.socket` doesn't
  point at the actual socket. We use `/proc/<firecracker-pid>/root/...`
  to dereference through firecracker's mount namespace — works whether
  jailer unshares or not.
- **Cleanup on Stop.** With `MS_SHARED` propagation on the host root
  (default on most distros), jailer's tmpfs over `/run` can leak back
  into the host's mount namespace. `cleanupJailerMounts` runs
  `MNT_DETACH` lazy unmounts on exit so leaked mountpoints don't pile
  up across VM restarts and poison the propagation graph for later VMs.

On **Windows/hcsshim** the equivalent OCI runtime spec is handed to
containerd: the user's image (with `hpcc-pause.exe` injected) as the
container rootfs, source/output mounted as Hyper-V container volumes at
`C:\src` and `C:\out` (see §4.1.1 caveats — staged onto a local volume),
`hpcc-pause.exe` as the entrypoint, `--network none`-equivalent.

The control plane between the worker host and inside-the-VM compile
processes is:

- **Linux**: agent gRPC bidi stream over vsock. See §4.4.1.
- **Windows**: containerd's task API (`Task.Exec` against an hcsshim-managed
  Hyper-V container) — process spawn, stdio capture, exit codes, signals.
  hpcc does not ship a Windows in-VM agent.

### 4.4.1 Agent Wire Schema

Schema lives in `proto/agent/agent.proto` (own module so both the runner
and the in-VM agent compile against one definition without dragging in
the other side's deps). One bidirectional streaming RPC per compile:

```proto
service AgentService {
  rpc Exec(stream ExecClientFrame) returns (stream ExecServerFrame);
}
```

Client stream (runner → agent):

1. First frame must carry an `ExecHeader` (`exec_id`, `argv`, `env`,
   `cwd`, `outputs`).
2. Subsequent frames carry `InputFile` chunks: `(path, chunk, eof)`.
   Files may interleave; the agent demultiplexes by path. Half-close
   signals "no more inputs."

Server stream (agent → runner):

1. `StdioChunk` frames as the compiler runs.
2. One `ExecResult` (`exit_code`).
3. `OutputFile` chunks for everything the agent finds under
   `/run/hpcc/out/<exec_id>/`.

Streaming both ends keeps peak agent RAM bounded by chunk size (256 KiB
inputs, 256 KiB outputs) rather than by sum-of-sizes — important for
CAS mode where the include closure can be hundreds of MB. Cancellation
is free: the runner's `ctx.Cancel` closes the gRPC stream, which fires
the agent's `cmd.Run` context, which kills the in-guest compiler.

Path-traversal guard on incoming output paths in the runner (the agent
is trusted, but a kernel CVE or corrupted frame shouldn't translate
into the runner clobbering `/etc/passwd` on the host).

### 4.5 Server-Side Preprocessing

Because the VM has the toolchain and system headers, preprocessing should
happen *there*, not on the client. Wins:

- **Bandwidth.** Original source is ~10 KB; preprocessed source is 1–50 MB.
- **Cross-developer cache hits.** Client-preprocessed output bakes in
  `__FILE__` paths and other locals; server-side preprocessing produces
  canonical bytes, raising cache hit rate dramatically.
- **CPU offload** from developer laptops to the build farm.

Two modes negotiated per-job:

1. **`cas`** — preferred, **not yet implemented**. Client runs `gcc -M`
   to discover the include closure, digests each input, sends
   `(source_digest, header_digests[], flags)`. Worker pulls missing
   digests from the shared CAS, materializes a synthetic input root,
   compiles. Reuses the Phase 3 blob store as the CAS. Produces
   canonical bytes server-side, so the cache key is portable across
   developers.
2. **`preprocessed`** — fallback, **working today**. Client preprocesses
   locally and ships bytes. Same RPC, just a populated
   `preprocessed_source` field. Used when the client can't reach the
   CAS or doesn't trust it.

(A `shared_root` mode that mounted the host tree directly into the
worker container was scoped earlier and dropped — too coupled to a
specific deployment topology, and the trust story for "client
filesystem appears in the worker's VM" was hard to defend in a
multi-tenant setup. CAS gives us the same canonicalization win
without the mount-shape constraint.)

### 4.6 Build-System Compatibility (CMake/ninja/make)

CMake configure runs on the **driving machine**, not in the VM. By the time
hpcc intercepts a compile invocation:
- `FetchContent` has already fetched.
- Configure-generated headers (`config.h.in` → `config.h`) exist.
- Code-generator outputs (protoc, moc, flex) exist (they're produced by
  prior ninja steps on the driving machine).

All those files live in `build/` on disk. The agent-streaming RPC ships
the source + build trees the compile needs as input file chunks (§4.4.1);
the agent reassembles them under `/run/hpcc/src/<exec_id>/`. **No special
handling needed.** The only edge case is `add_custom_command` that needs
network during build — rare and considered bad practice in any sandboxed
setup.

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

### 4.8 Scheduler (Route-Only)

The scheduler is a **lookup service**, not a relay. It never touches
compile request payloads or artifact bytes. The hot path (compile RPC)
goes directly from the client daemon to the worker.

- `hpcc scheduler --listen :9091`.
- Workers register on startup (`RegisterWorker`) and send periodic
  heartbeats (`Heartbeat`) — both unary RPCs. The scheduler never
  initiates a connection to a worker.
- Client daemon calls `scheduler.Route(RouteRequest)` — a lightweight RPC
  containing `(tenant_id, image_digest)`. Scheduler returns a
  `RouteResponse` with:
  - **`worker_address`** — where to dial.
  - **`token`** — a JWT signed by the scheduler containing the task
    claims (`tenant_id`, `image_digest`, `worker_id`, `exp`).
  - **`cert_fingerprint`** — SHA-256 of the worker's TLS certificate
    (registered by the worker at startup). The client pins this when
    dialing.
- Client daemon dials the worker directly, presents the JWT (as gRPC
  metadata), and calls `worker.Compile(CompileRequest)`.
- Worker verifies the JWT signature against the scheduler's public key,
  which it receives in the `AuthResponse` when it first authenticates to
  the scheduler. The signing keypair (Ed25519) is generated randomly at
  scheduler startup — no key file to manage, no deployment step. On
  scheduler restart, workers re-authenticate and get the new public key;
  in-flight JWTs from the old key expire naturally (short TTL). After
  authentication, no ongoing scheduler↔worker connection is required for
  authorization — the JWT is self-contained.
- Routing decisions based on:
  - Tenant → VM affinity (sticky routing — same tenant lands on the same VM).
  - Image digest match (worker has the rootfs).
  - Current load / available capacity.
  - Optional: network proximity (RTT-based scoring).

This keeps the scheduler off the data path. It handles ~1 KB route
lookups, not multi-MB artifact transfers. A single scheduler can serve
thousands of concurrent compiles without becoming a bottleneck.

### 4.9 Worker

- `hpcc worker --scheduler <addr>` — connects to the scheduler, drives
  per-tenant sandboxes via the `Runtime` interface.
  - **Linux**: raw Firecracker driver. Owns image pull, rootfs build,
    Firecracker VMM lifecycle, and the host-side end of the agent
    gRPC stream.
  - **Windows**: containerd + hcsshim with Hyper-V isolation (follow-up).
- Maintains per-tenant VMs, snapshots on idle timeout (Linux, follow-up),
  evicts under memory pressure.
- Receives `Compile` RPCs **directly from client daemons** (not via the
  scheduler). For each job, sends an Exec into the right per-tenant VM
  with a fully-resolved argv; captures stdio + exit code via the agent
  stream (Linux) or containerd's process API (Windows).
- Prepares user images on first use: pulls the OCI image, flattens
  layers, builds the rootfs with the agent injected (§4.3.1), computes
  the worker-side image digest.
- Reports state to the scheduler via the heartbeat stream (load, active
  VMs, image digests).

### 4.10 Wire Protocol: gRPC for the Control Plane

Two distinct hops on the host plane plus one inside the VM:

**Scheduler (routing):** `scheduler.Route(RouteRequest) returns
(RouteResponse)` — lightweight lookup, no compile payload.

**Worker (compile):** `worker.Compile(CompileRequest) returns
(CompileResponse)` — client dials the worker directly. gRPC reasons
specific to this hop:

- Per-RPC zstd compression. Preprocessed C++ (when the `preprocessed`
  fallback mode kicks in) compresses ~5–10×. **This is the largest single
  perf lever; flip it on.**
- HTTP/2 multiplexing. Hundreds of concurrent in-flight jobs over one
  connection per worker.
- First-class cancellation — `ctx.Cancel()` propagates through the worker
  to the in-VM agent stream (§4.4.1) which kills the compiler in the
  guest. Build aborts feel snappy.
- Server-auth TLS in one line; cert-fingerprint pinning is the client's
  job (§4.8). Auditor-friendly without the operational tax of a
  client-cert PKI — the JWT carried in metadata is the actual auth.
- Per-call deadlines map to the job timeout cleanly.
- Don't stream stdout/stderr to the *client*. Compilers don't produce
  progressive output that's meaningful to a build driver; buffer at the
  worker and return at the end. (The agent → worker stream *does* stream
  stdio so the worker can apply the per-call timeout to in-VM hangs, but
  that's a transport detail.)

**Worker → in-VM agent:** bidi-streaming gRPC over vsock. Schema in
§4.4.1. Same protobuf module everywhere; no special wire dialect.

Explicitly **not** Kafka. Kafka is a log/streaming system, not RPC. Per-job
broker round-trips, large-message friction, request-reply via reply-topics,
and durability you don't want — wrong tool. (Kafka is fine, later, as a
telemetry/audit sidecar — not in v1.)

### 4.11 Failure Handling

- No workers available / scheduler unreachable: compile locally.
- Worker fails mid-job: client retries via `scheduler.Route()` to get a
  different worker; falls back to local compile as last resort.
- Per-job timeout (configurable, default 60s).
- VM crash mid-job: the runtime surfaces a process-exit event (Firecracker
  process dies or the agent stream errors); the worker reaps the dead VM,
  cold-boots a new one for the tenant, and the client retries the RPC.
  Today the worker reaps locally but doesn't yet broadcast the dead VM
  to the scheduler — heartbeat covers it on the next tick, but a
  dedicated failure RPC is open follow-up.

### 4.12 Audit Trail

For each compile job, log:
- VM image digest, vCPU/mem caps.
- Source digest, flag set, cache key.
- Output digest, exit code, duration.
- Tenant ID, scheduler ID, worker ID, VM ID.

This is the table format banks want to see — every artifact is reproducible
from its row. Implemented today on every `CompileResponse` (BLAKE3 input
+ output digests, full audit row mirrored to the worker log); a durable
sidecar sink is open follow-up work.

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

### 4.14 Rootfs Extraction Hardening (Follow-up)

The §4.3 image→rootfs pipeline runs against **attacker-controlled OCI
image bytes** — the entire multi-tenancy story assumes the tenant is
hostile, so the bytes flowing through `crane.Pull` → `mutate.Extract`
→ tar extraction → `mkfs.ext4 -d` are not trusted. The current
implementation (`internal/worker/image/rootfs/rootfs.go`) is mostly
safe but has known gaps that should be closed before this lands in a
production deployment.

Threat surface, by stage:

- **`crane.Pull` + `mutate.Extract`** (in-process Go, vetted by the
  ecosystem). Low risk.
- **`tar -xpf` of the spooled flattened tar.** The soft underbelly.
  Depending on which `tar` implementation is in `$PATH`, a malicious
  image can attempt:
  - **Path traversal** — entries named `../../etc/foo`. GNU tar 1.30+
    blocks by default; older tars and some BSD/busybox variants
    don't.
  - **Symlink racing** — first entry creates `etc -> /etc`, second
    writes `etc/passwd`. Modern GNU tar refuses to follow; older
    versions had bugs here.
  - **Hardlink attacks** — `linkname=/etc/shadow`. GNU tar checks
    that link targets resolve inside the extraction tree.
  - **suid/sgid escalation** — `tar -xpf` preserves modes. A
    root-owned suid binary briefly living under our staging dir is a
    local-priv-escalation vector for any sibling process under the
    worker user.
  - **Device-node creation** — TypeChar/TypeBlock entries with
    mknod. As root, tar will create them under staging.
  - **Tar bombs** — uncompressed flattened OCI tars can be
    arbitrarily large. We don't currently cap.
- **`mkfs.ext4 -d`.** Narrow surface — libext2fs reads our (now
  populated) staging dir and writes a fresh ext4. Doesn't exec
  staging contents, doesn't interpret tar headers. e2fsprogs CVEs
  cluster on the *parse* side (mounting/fsck'ing malicious ext4),
  not the format side. Worst plausible failure: malformed ext4 that
  fails to mount in the guest — noisy, not silent compromise.

Mitigations, ranked by impact / cost:

1. **Deployment-side**: mount the rootfs cache dir's parent on tmpfs
   with `nosuid,nodev,noexec`. Even if tar is tricked into dropping
   a suid binary or device node under staging, it can't be
   exploited. mkfs.ext4 only reads from staging — `noexec` doesn't
   block that. Cheap, document as a deployment requirement.
2. **Cap extracted size** before invoking mkfs.ext4. A `du -sb
   staging` against a configured ceiling rejects 100 GB tar bombs
   cheaply. ~10 lines.
3. **Replace `exec.Command("tar", ...)` with a Go-native
   extractor.** The real fix. An `archive/tar` reader with explicit
   traversal/symlink/hardlink validation, optional suid stripping,
   no foreign tar implementation in the trust boundary. The ~150
   lines of validated extractor code replace one `tar` shell-out
   with semantics we own end-to-end.

Containerd's snapshotter, BuildKit, and Docker's image pull all use
Go-native extraction with explicit safety wrappers for exactly these
reasons. v1 ships with the shell-out path because it's the smallest
correct change after dropping go-diskfs; (3) is the planned
follow-up before regulated-environment deployment.

### Milestone

A 16-core machine effectively compiles with `-j64` by distributing to
3 other machines in the cluster. Each tenant's compiles run in their own
Firecracker VM (driven directly by hpcc), reused across the build,
snapshotted on idle timeout, with no network access from inside the VM.

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

[[cache]]
type     = "disk"
location = "~/.cache/hpcc"
max_size = "10GB"

[[cache]]
type        = "s3"
bucket      = "hpcc-cache"
region      = ""
endpoint    = ""
max_size    = ""              # empty = no in-process eviction (use S3 lifecycle)
auto_create = false

[scheduler]
url     = "..."
paranoid = false              # true = cache only on workers, never on client

[vm]
image = "..."
idle_timeout    = "10m"
session_timeout = "8h"
memory          = "2GB"
vcpus           = 4

[determinism]
auto_inject_flags = true

[log]
level = "info"                # "debug" | "info" | "warn" | "error"
file  = "..."
```

### 5.5 Eviction

- LRU with max size (default 10GB) for local cache.
- Watermark-gated eviction for S3 cache (§3.5) — already implemented.
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
  compiler/         — flag parsing, invocation, preprocess, cache-key
  cache/
    store/
      store.go      — Store interface
      disk.go       — local disk cache (CAS)
      s3.go         — S3-compatible remote store (Phase 3)
      factory.go    — config → []Store, shared by client and worker
  daemon/           — client-side daemon (loopback TCP)
  protocol/         — host-plane wire schema (compile, scheduler, worker, audit)
  scheduler/        — route-only coordinator, worker registry, JWT signer
  worker/
    worker.go       — host-side worker (Compile RPC, image catalogue, pool)
    runtime/        — Runtime interface; raw Firecracker driver (Linux),
                      jailer setup, vsock dial, mount cleanup,
                      DangerouslyExecOnHost dev backend
    image/
      rootfs/       — OCI pull → flatten → tar -xpf → mkfs.ext4 ext4 builder (Linux)
      cdimage/      — containerd image prep (Windows path, follow-up)
agent/              — separate Go module: in-VM hpcc-agent (Linux) — PID-1 init,
                      mount setup, zombie reaping, AgentService.Exec gRPC server
                      over AF_VSOCK
pause/              — separate Go module: tiny static PID-1 binary, used as
                      hpcc-pause.exe on the Windows containerd path
proto/              — separate Go module: shared agent↔runner wire schema.
                      proto/agent/agent.proto + generated .pb.go. Imported by
                      both the main module and the agent module without
                      dragging either side's heavy deps in the other direction
firecracker/        — generated Firecracker VMM API client (go-swagger)
go.work             — multi-module workspace tying all four modules together
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
S3 mirrors the layout under a `cache/` prefix so the bucket can be shared.

### Compression

zstd for cached artifacts (level 3 default). zstd on every gRPC call
carrying preprocessed source — flip the flag.

### Concurrency

Daemon handles concurrent requests with goroutines. Deduplication is a
mutex-protected map of `chan struct{}`. S3 eviction is single-flighted
across goroutines via `atomic.Bool` so only one bucket scan runs at a time.

### Sandbox Model

**Hardware-virtualized boundary, always.** Namespace-based sandboxes (bwrap,
nsjail) and userspace-kernel sandboxes (gVisor) are out of scope — the
deployment target is regulated environments where the kernel + KVM boundary
is the boundary auditors recognize, and gVisor's intercept-syscalls model
is not.

Two backends behind hpcc's `Runtime` interface:

- **Linux** → raw Firecracker driver. hpcc owns the rootfs builder, VMM
  lifecycle, and an in-VM agent over vsock. (Primary target, v1.)
- **Windows** → containerd + hcsshim with Hyper-V isolation → utility-VM
  Windows containers (follow-up; required for MSVC and legacy Windows-only
  projects).

The scheduler matches jobs to workers by runtime + image digest. Everything
above the runtime layer (gRPC compile RPC, cache, audit, image-digest
identity, pause+agent injection) is OS-agnostic.

We chose raw Firecracker over firecracker-containerd because the latter has
stagnated (devmapper snapshotter rough, release cadence effectively dead).
The trade — owning a small image→rootfs pipeline and a one-method gRPC
agent — is preferable to depending on unmaintained infra for a project that
needs to live in regulated environments long-term. See §4.1 for the full
rationale.

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
| Wrapper ↔ daemon (local) | Length-prefixed protobuf over loopback TCP (`127.0.0.1`) with a per-daemon auth token | Wrapper invoked thousands of times per build; gRPC runtime is too heavy for the hot path. TCP for portability — the wrapper has to run on Windows too |
| Cache (Phase 3) | S3 API (`GetObject`/`PutObject`/`ListObjectsV2`) | Direct to object storage, no proxy; works with AWS S3, MinIO, R2, GCS |
| Daemon → scheduler (routing) | gRPC unary `Route` | Lightweight lookup (~1 KB), returns worker address. Scheduler never touches compile payloads |
| Daemon → worker (compile) | gRPC unary `Compile` | Client dials worker directly. Per-call zstd, multiplexing, cancellation, deadlines. Server-auth TLS with cert-fingerprint pinning + scheduler-signed JWT in metadata; no client-cert PKI to operate (§4.8) |
| Worker → in-VM agent | gRPC bidi-streaming `AgentService.Exec` over vsock | Streamed inputs/outputs/stdio, single connection per VM, free cancellation. Schema in `proto/agent/agent.proto` (§4.4.1) |
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
- Remote cache uses S3 auth (AWS credential chain). Per-call timeouts +
  bounded body reads protect against stuck/oversized objects.
- Workers run compilations in Firecracker VMs (driven directly by hpcc)
  with no virtio-net device attached.
- Image rootfs is read-only inside the VM; everything writable is tmpfs
  mounted by the agent (so per-compile state can't persist across Execs
  via the rootfs). The pause+agent binary hpcc injects (§4.3.1) is the
  only worker-controlled content; user image bytes are not modified.
- Per-VM audit log: image digest, source digest, flag set, output digest,
  duration, exit code, tenant/scheduler/worker/VM IDs.
- **Client→scheduler**: mTLS. Client trusts the scheduler's CA.
- **Client→worker**: scheduler-issued JWT in the `RouteResponse`. Worker
  verifies the signature against the scheduler's public key — no
  scheduler↔worker connection needed for auth. Client pins the worker's
  TLS cert via the SHA-256 fingerprint included in the `RouteResponse`
  (registered by the worker at startup). No separate PKI needed — the
  scheduler is the trust root (§4.8).
- **Worker→agent**: insecure transport credentials over vsock. Correct
  here: the channel is a host↔guest pipe inside one VMM, no network
  exposure, TLS would burn cycles for no security gain.
- **Paranoid mode** (`paranoid = true`): all cache reads/writes happen on
  the worker side only — clients never touch the cache stores and never
  hold remote-store credentials. Prevents cache poisoning by compromised
  developer machines (§4.13).
- **Open hardening item**: §4.14 — replace the `exec.Command("tar", ...)`
  shell-out in the rootfs builder with a Go-native, traversal-validating
  extractor before regulated deployment.

---

## Build Order

### Done so far

1. **GNU + MSVC parsers** — `internal/compiler/grammar.go`,
   `grammar_msvc.go`, `parser.go`, `parser_msvc.go` (+ tests). Flag
   spec tables driving both grammars.
2. **Compiler detection** — `internal/compiler/detect.go`. Identify
   the toolchain from `argv[0]`.
3. **Compiler/Invocation types** — `compiler.go`, `invocation.go`,
   `clang.go`, `cl.go`. Stateless strategy + parsed-argv data shape.
4. **Preprocess / dep-gen** — `internal/compiler/preprocess.go`. `-E`
   and `-M` invocations.
5. **Cache-key hashing** — `internal/compiler/cache_key.go`.
   Preprocess-mode + manifest-mode, with a precomputed-digest
   short-circuit (`Invocation.PreprocessedDigest *[32]byte`) so paranoid
   mode produces stable cache keys for PREPROCESSED-source compiles
   without re-running the preprocessor.
6. **Drop-in wrapper CLI** — `main.go`, `cmd/root.go`, `cmd/wrap.go`.
   `hpcc wrap <compiler> [args...]` and symlinked-name dispatch.
7. **Cache configuration + enums** — `internal/config/`, including
   `CacheConfig` shared by client and worker.
8. **Disk cache store** — `internal/cache/store/store.go`,
   `disk.go`, `factory.go`. CAS layout on disk; `FromConfig` is the
   single factory both runner and worker call.
9. **Runner** — `internal/runner/runner.go`, `stores.go`, `context.go`.
   Glue between parsed invocation, hasher, cache store, and the real
   compiler.
10. **Stats + clean commands** — `cmd/stats.go`, `cmd/clean.go`.
11. **Wire protocol** — `internal/protocol/*.proto`. Length-prefixed
    protobuf messages for the daemon ↔ client path; gRPC services for
    the scheduler and worker.
12. **Daemon + client** — `internal/daemon/`. Loopback TCP listener,
    handshake file, per-connection auth, singleflight-based
    deduplication, `TCP_NODELAY`. `cmd/start.go` launches the daemon.
13. **Scheduler** — `internal/scheduler/`. gRPC `Authenticate` /
    `Route` / `RegisterWorker` / `Heartbeat`, JWKS-backed user auth,
    static-token worker auth, scheduler-signed Ed25519 task JWTs,
    sticky-tenant + image-digest + load-aware routing.
14. **Pause binary** — `pause/` (separate Go module). Static PID-1 +
    SIGCHLD reaping. Used as the Windows-side `hpcc-pause.exe`.
15. **Worker compile pipeline** — `internal/worker/worker.go`,
    `runtime_executor.go`, `staging.go`, `config.go`. End-to-end
    `Compile` handler: token validation → host-path argv sanity →
    per-RPC src/out staging → `runtime.Start` → executor adapter →
    cache lookup/store in paranoid mode → response. Scheduler liaison
    loop (auth, register, heartbeat) lives on the same `Worker` value.
16. **Client-side argv rewriting** — `internal/compiler/rewrite.go`.
    `RewritePathPrefix`, `Compiler.RewriteForPreprocessed`,
    `ValidateNoHostPaths`. Both compiler families implement
    `RewriteForPreprocessed` — GNU walker (clang/clang++/cc/c++) and
    cl.exe walker.
17. **Worker runtime interface + dev backend** —
    `internal/worker/runtime/runtime.go` (interface + `ContainerSpec`,
    `ExecRequest`/`ExecResult`), `dangerous.go`
    (`DangerouslyExecOnHost`, gated behind `runtime.handler =
    "really_really_dangerous"`), `select.go`.
18. **Per-tenant container pool** — `internal/worker/runtime/pool.go`
    (`PooledRuntime`). Keyed on `(TenantID, ImageDigest)`; idle
    timeout reaper + hard `vm.session_timeout` ceiling preserved
    across park/pop.
19. **Image catalogue + pull-on-miss** — `Worker.ensureImage`
    singleflight'd by digest, with idle eviction at
    `image.idle_timeout` cadence.
20. **`cmd/worker.go` + daemon dispatch** — `cmd/worker.go` runs the
    gRPC server + scheduler liaison loop with TLS;
    `internal/daemon/dispatch` authenticates via OAuth password
    grant, calls `scheduler.Route`, dials the worker pinned by cert
    fingerprint, ships PREPROCESSED-mode `CompileRequest`s; daemon
    falls back to local on any remote-side error.
21. **§4.12 audit records** — populated on every `CompileResponse`
    (tenant/worker/vm/image, source + output BLAKE3 digests, cache
    key, flags, exit code, duration, timestamp); mirrored to the
    worker log. Durable sidecar sink remains follow-up.
22. **S3 cache store (Phase 3)** — `internal/cache/store/s3.go`.
    `cache/`-prefixed object layout, per-call timeouts (2s/5s/30s),
    bounded body reads, watermark-gated eviction with
    single-flighted bucket scan, opt-in `auto_create`,
    object-level-IAM-friendly init smoke-test.
23. **Image→rootfs pipeline (Linux Firecracker)** —
    `internal/worker/image/rootfs/`. `crane.Pull` →
    `mutate.Extract` → `tar -xpf` → `mkfs.ext4 -d` with
    pre-created standard mountpoints and post-extract agent
    injection. go-diskfs's ext4 writer turned out to be unusable on
    real-sized rootfs (extent-tree promotion, journal init bugs);
    shelling out to e2fsprogs's `mkfs.ext4` is the v1 answer.
    Hardening tracked in §4.14.
24. **Raw Firecracker driver** —
    `internal/worker/runtime/firecracker.go` + `firecracker_e2e_test.go`.
    Jailer launch, kernel/rootfs staging, vsock device config,
    `/proc/<pid>/root/...` socket reach (jailer 1.10+ unshares
    `CLONE_NEWNS`), lazy-unmount cleanup of host-leaked tmpfs
    mounts, agent gRPC dial. Integration suite in CI: downloads
    firecracker + jailer, applies `/dev/kvm` udev relax, builds a
    real chainguard `gcc-glibc:latest-dev` rootfs, compiles a one-line
    C source end-to-end through the full path.
25. **In-VM hpcc-agent** — `agent/` (separate Go module). `setupInit`
    mounts kernel filesystems + tmpfses for `/tmp`, `/run`, `/dev/shm`;
    SIGCHLD reap loop; bidi-streaming gRPC server on AF_VSOCK port
    17727 implementing `AgentService.Exec` (header → input file
    chunks → stdio → result → output file chunks).
26. **Shared `proto/` module** — `proto/agent/agent.proto` + generated
    bindings. Imported by both the main module (runner-side gRPC
    client) and the agent module (server-side handler) without
    cross-pollinating dep graphs. Workspace-tied via top-level
    `go.work`.

### Remaining

27. **VM snapshot/restore** (§4.2). `PooledRuntime` keeps warm VMs
    in RAM today; the snapshot-on-idle, ~10ms-restore path that raw
    Firecracker enables (vs. the firecracker-containerd "warm in RAM,
    cold-boot on resume" downgrade) is unwritten. Hooks into the
    pool's reaper tick and the Firecracker `CreateSnapshot` /
    `LoadSnapshot` API calls.
28. **CAS-mode source staging** in `internal/worker/staging.go` —
    currently returns "not implemented"; only PREPROCESSED works
    end-to-end. CAS needs blob-list materialization (reuse the Phase 3
    store as the CAS), mode-pivot in `dispatch.Dispatch` to call
    `RewritePathPrefix` for CAS and populate the matching
    `RemoteDescriptor` oneof, and a worker-side cache-key strategy
    pivot keyed on `descriptor.source_mode`.
29. **Windows hcsshim path** (§4.1.1). Container start with the
    `hpcc-pause.exe` entrypoint, mount setup via Hyper-V volume,
    `Task.Exec` per compile.
30. **VM-crash → scheduler reroute** (§4.11). Today the worker reaps
    locally; the scheduler learns about the dead VM only on the next
    heartbeat tick. A direct failure RPC would make client retries
    pick up a fresh worker faster.
31. **Rootfs extraction hardening** (§4.14). Replace the `tar -xpf`
    shell-out with a Go-native, traversal-validating `archive/tar`
    extractor; cap extracted size; document the `nosuid,nodev,noexec`
    tmpfs deployment requirement. Required before any
    regulated-environment deployment.
32. **Phase 5 polish**. `hpcc inspect`, `hpcc explain`, local-cache
    LRU eviction under `max_size`, Prometheus endpoints on daemon /
    worker / scheduler, per-build summary, durable audit-record
    sidecar.
