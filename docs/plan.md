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

### 3.1 S3 Store

A new `Store` implementation (`internal/cache/store/s3.go`) that speaks the
S3 API. Works with AWS S3, MinIO, R2, GCS (via S3 compatibility), etc.

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

**Progress so far:**

- **Wired up:** §4.1.1 Runtime abstraction (`internal/worker/runtime`,
  with `DangerouslyExecOnHost` as the dev backend and `Firecracker` as
  the production backend); §4.3 image→rootfs pipeline (`rootfs.Store`
  pulls user images, injects `/.hpcc/agent`, writes ext4 with a
  copy-fallback for hardlink-heavy images); §4.4 *partial* — Firecracker
  boots under jailer with hpcc's kernel, the prepared rootfs as
  `/dev/vda`, no NIC; §4.8 route-only scheduler; §4.9 worker (Compile
  RPC, image catalogue + idle eviction, per-tenant container pool with
  idle/session TTLs); §4.13 paranoid-mode plumbing on the worker.
  Boot is covered by an integration test that downloads firecracker +
  jailer, applies the standard `/dev/kvm` udev relax, and runs under
  `sudo` on the GitHub Actions Ubuntu runner.
- **Next:** §4.3.1 + §4.4 vsock — write the in-VM `hpcc-agent` and the
  host-side client, configure a vsock device on the Firecracker VM, and
  replace `Firecracker.Exec`'s "not implemented" stub. Without this,
  real compiles still go through `DangerouslyExecOnHost`.
- **After that:** §4.4 source/output drives (per-RPC `/src` and `/out`
  attached as virtio-blk at Exec time); §4.2 snapshot/restore on idle
  timeout; §4.5 server-side preprocessing dispatched through the agent;
  §4.1.1 the Windows hcsshim path; §4.11 VM-crash reaping with
  scheduler reroute.


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

The trade is real. We take on:

- **OCI image → rootfs pipeline.** Pull layers (`go-containerregistry`),
  flatten, `mkfs.ext4` onto a sparse file. Bounded scope, ~few hundred lines.
  See §4.3.
- **In-VM agent over vsock.** A tiny static binary inside the guest that
  speaks one RPC: "exec this argv with this env in this cwd, stream stdio
  back, return exit code, support cancellation." Replaces what
  firecracker-containerd's guest agent did. The pause binary already shipped
  (§4.3.1) collapses into the same agent. See §4.4.
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
  builder (§4.3), VMM lifecycle, and the in-VM agent (§4.4). Primary
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

**Out of scope for now** — only the Linux/Firecracker backend is implemented
in v1. Windows support is an explicit follow-up: when it lands, it slots in
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
  Firecracker microVM with the hpcc-agent (§4.3.1) as PID 1, blocking on
  the agent loop with zero CPU between compiles. On Windows this is a
  Hyper-V utility VM under hcsshim, with the pause binary as PID 1.
- **Per-compile work** is dispatched as one Exec into the running VM —
  Linux via the in-VM agent over vsock, Windows via containerd `Task.Exec`
  — `execve`-ing the toolchain directly with a fully-resolved argv. No
  shell required in the user's image.
- **Idle timeout** (e.g. 5–15 min) → snapshot the VM and unload it from
  memory; ~10ms restore on next demand. Driving Firecracker directly makes
  this practical (under firecracker-containerd it had been downgraded to
  "warm in RAM, cold-boot on resume" because the snapshot/restore story was
  rough). The Hyper-V backend uses its own equivalent state-save mechanism
  or falls back to cold boot.
- **LRU eviction** of warm/snapshotted VMs under memory or disk pressure.
- **Hard session timeout** (e.g. shift change, N hours) → blow the VM away,
  discard the snapshot. Long-lived per-tenant state accumulates and someone
  will eventually ask what's in it.

### 4.3 Container Image as the Build Environment

The user supplies their toolchain by handing hpcc a **container image**
(any OCI-compatible registry: Docker Hub, GHCR, ECR, internal mirrors).

On the **Linux/Firecracker** path the worker pulls layers (via
`go-containerregistry`), flattens them in order, and materializes the
result as an ext4 filesystem on a sparse file — a Firecracker rootfs drive.
No containerd, no devmapper. The prepared rootfs is cached on disk keyed by
`(user image digest + injected agent layer digest)`. On the
**Windows/hcsshim** path containerd's image pull + snapshotter does the
equivalent.

- **Image digest is the toolchain identity** for cache keys. Same image used
  by 50 developers → one prepared rootfs on the worker, one toolchain
  identity in the cache.
- The kernel is **hpcc's**, not the image's — the worker boots the microVM
  with a `vmlinux` hpcc provides, ignoring whatever kernel the image might
  ship.

#### 4.3.1 Pause-and-Agent Injection

The VM needs a long-running PID 1 that (a) stays alive between compiles
and (b) accepts Exec requests from the worker host. User images can be
anything — distroless, scratch — so hpcc cannot rely on `sh`, `sleep`, or
any other host-provided binary being present.

Instead, hpcc injects one tiny static binary into every prepared image,
which fills both roles:

- **Linux**: a statically-linked `hpcc-agent` (~1MB). Acts as the long-running
  PID 1 (blocks on the agent loop, exits cleanly on SIGTERM, reaps zombies),
  and listens on a vsock port for `Exec(argv, env, cwd)` requests from the
  worker host. Forks/execs the toolchain, streams stdout/stderr back over
  the same vsock connection, returns the exit code, supports cancellation.
  Replaces what firecracker-containerd's guest agent did under the previous
  design; the standalone pause binary collapses into this same agent.
- **Windows**: `hpcc-pause.exe` only — Windows uses containerd `Task.Exec`
  via hcsshim, so no in-VM agent is needed; the pause binary plays the
  Kubernetes-style "stay alive as PID 1" role.

Injection happens at image-prep time on the worker: hpcc writes
`/.hpcc/agent` (or `/.hpcc/pause.exe`) into the prepared rootfs and sets
the VM's init/entrypoint to it. The user's image bytes are not modified —
on Linux the prepared rootfs is a separate artifact built per
`(user image digest, agent digest)` pair; on Windows the same digest-based
identity rule applies via the snapshotter content store. Either way, the
worker-side image digest folds in the injected binary so two workers
running the "same" user image with hpcc's injection produce the same
effective digest.

This makes the design **image-agnostic**: hpcc works with any OCI image
the user wants to bring, including minimal/distroless images that have no
shell at all.

### 4.4 VM Layout

On **Linux/Firecracker** each per-tenant microVM is configured directly via
the Firecracker VMM API:

- **Rootfs drive** — the prepared rootfs from §4.3, attached read-only.
  Per-VM scratch (writable upper for `/tmp`, build scratch) is a separate
  in-memory tmpfs or a per-VM thin overlay.
- **Source drive** — the project source + build tree, exposed inside the
  VM at `/src` (read-only). Packaged as a separate virtio-blk drive (a
  worker-built ext4 or squashfs image, rebuilt when the source tree
  changes).
- **Output drive** — `/out` inside the VM, a writable virtio-blk volume the
  worker reads artifacts from after each compile.
- **Init** — `/.hpcc/agent` (the pause+agent binary, §4.3.1), kept alive
  for the life of the VM and serving Exec requests over vsock.
- **Vsock** — one virtio-vsock device. The worker's host-side agent client
  speaks to the in-VM agent over a CID/port pair. Vsock is the only
  host↔guest channel.
- **No network.** No virtio-net device is configured; there is nothing for
  the guest to talk to off-host. No exfiltration argument to have.

On **Windows/hcsshim** the equivalent OCI runtime spec is handed to
containerd: the user's image (with `hpcc-pause.exe` injected) as the
container rootfs, source/output mounted as Hyper-V container volumes at
`C:\src` and `C:\out` (see §4.1.1 caveats — staged onto a local volume),
`hpcc-pause.exe` as the entrypoint, `--network none`-equivalent.

The control plane between the worker host and inside-the-VM compile
processes is:

- **Linux**: hpcc-agent vsock RPC. One method, `Exec(argv, env, cwd)`,
  with stdio streaming, exit code, and cancellation. Owned by hpcc end-to-end.
- **Windows**: containerd's task API (`Task.Exec` against an hcsshim-managed
  Hyper-V container) — process spawn, stdio capture, exit codes, signals.
  hpcc does not ship a Windows in-VM agent.

### 4.5 Server-Side Preprocessing

Because the VM has the toolchain and system headers, preprocessing should
happen *there*, not on the client. Wins:

- **Bandwidth.** Original source is ~10 KB; preprocessed source is 1–50 MB.
- **Cross-developer cache hits.** Client-preprocessed output bakes in
  `__FILE__` paths and other locals; server-side preprocessing produces
  canonical bytes, raising cache hit rate dramatically.
- **CPU offload** from developer laptops to the build farm.

Two modes negotiated per-job:

1. **`cas`** — preferred. Client runs `gcc -M` to discover the
   include closure, digests each input, sends `(source_digest,
   header_digests[], flags)`. Worker pulls missing digests from the shared
   CAS, materializes a synthetic input root, compiles. Reuses the Phase 3
   blob store as the CAS. Produces canonical bytes server-side, so the
   cache key is portable across developers.
2. **`preprocessed`** — fallback. Client preprocesses locally and ships
   bytes. Same RPC, just a populated `preprocessed_source` field. Used
   when the client can't reach the CAS or doesn't trust it.

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

All those files live in `build/` on disk. The worker packages both the
source and build trees into the source volume mounted at `/src`
(§4.4). **No special handling needed.** The only edge case is
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
    Firecracker VMM lifecycle, and the host-side end of the vsock agent
    channel.
  - **Windows**: containerd + hcsshim with Hyper-V isolation (follow-up).
- Maintains per-tenant VMs, snapshots on idle timeout (Linux), evicts
  under memory pressure.
- Receives `Compile` RPCs **directly from client daemons** (not via the
  scheduler). For each job, sends an Exec into the right per-tenant VM
  with a fully-resolved argv; captures stdio + exit code via the agent
  channel (Linux) or containerd's process API (Windows).
- Prepares user images on first use: pulls the OCI image, flattens
  layers, builds the rootfs with the agent injected (§4.3.1), computes
  the worker-side image digest.
- Reports state to the scheduler via the heartbeat stream (load, active
  VMs, image digests).

### 4.10 Wire Protocol: gRPC for the Control Plane

Two distinct hops:

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
  to the in-VM Exec (vsock RPC on Linux, `Task.Exec` context on Windows),
  which kills the compiler in the guest. Build aborts feel snappy.
- Server-auth TLS in one line; cert-fingerprint pinning is the client's
  job (§4.8). Auditor-friendly without the operational tax of a
  client-cert PKI — the JWT carried in metadata is the actual auth.
- Per-call deadlines map to the job timeout cleanly.
- Don't stream stdout/stderr. Compilers don't produce progressive output;
  buffer and return at the end.

Explicitly **not** Kafka. Kafka is a log/streaming system, not RPC. Per-job
broker round-trips, large-message friction, request-reply via reply-topics,
and durability you don't want — wrong tool. (Kafka is fine, later, as a
telemetry/audit sidecar — not in v1.)

### 4.11 Failure Handling

- No workers available / scheduler unreachable: compile locally.
- Worker fails mid-job: client retries via `scheduler.Route()` to get a
  different worker; falls back to local compile as last resort.
- Per-job timeout (configurable, default 60s).
- VM crash mid-job: the runtime surfaces a VM-exit event (Firecracker VMM
  on Linux, containerd task-exit on Windows); the worker reaps the dead
  VM, cold-boots a new one for the tenant, and the client retries the RPC.

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
   extractor.** The real fix. We previously had a `copyTarToFS`
   built on `archive/tar` with explicit traversal/symlink/hardlink
   validation; it was deleted when go-diskfs's ext4 writer turned
   out to be unusable, but the *tar reading* logic was correct.
   Bring it back, write into a real Linux directory (no go-diskfs
   bugs to dodge), then `mkfs.ext4 -d` from there. The ~150 lines
   of validated extractor code replace one `tar` shell-out with
   semantics we own end-to-end — no "trusting `/usr/bin/tar`'s
   defaults" question, no implementation drift between dev hosts
   and CI runners.

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
    scheduler.go    — route-only coordinator, worker registry
    routing.go      — tenant→VM affinity, image-digest matching
  worker/
    worker.go       — host-side worker
    runtime/        — Runtime interface; raw Firecracker driver (Linux),
                      containerd+hcsshim (Windows, follow-up)
    rootfs/         — OCI pull → flatten → ext4 image builder (Linux)
    agent/          — host-side vsock client for the in-VM agent (Linux)
    sessionpool/    — per-tenant VM lifecycle, snapshot + eviction
    image/          — image prep: agent injection, worker-side digest accounting
  agent/            — tiny static `hpcc-agent` binary: PID-1 + vsock Exec
                      server inside the VM (Linux). Windows uses the smaller
                      `hpcc-pause.exe` instead. Separate go module so it
                      can be built standalone (linux/amd64, linux/arm64,
                      windows/amd64) without dragging in the rest of the tree.
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
The trade — owning a small image→rootfs pipeline and a one-method vsock
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
| Wrapper ↔ daemon (local) | Length-prefixed protobuf over loopback TCP (`127.0.0.1`) with a per-daemon auth token | Wrapper invoked thousands of times per build; gRPC runtime is too heavy for the hot path. TCP (over Unix sockets) for portability — the wrapper has to run on Windows too |
| Cache (Phase 3) | S3 API (`GetObject`/`PutObject`/`HeadObject`) | Direct to object storage, no proxy; works with AWS S3, MinIO, R2, GCS |
| Daemon → scheduler (routing) | gRPC unary `Route` | Lightweight lookup (~1 KB), returns worker address. Scheduler never touches compile payloads |
| Daemon → worker (compile) | gRPC unary `Compile` | Client dials worker directly. Per-call zstd, multiplexing, cancellation, deadlines. Server-auth TLS with cert-fingerprint pinning + scheduler-signed JWT in metadata; no client-cert PKI to operate (§4.8) |
| Worker → in-VM compile | Linux: hpcc-agent vsock RPC. Windows: containerd `Task.Exec` (hcsshim) | One method (`Exec(argv, env, cwd)` with stdio streaming + exit code + cancel). hpcc owns the Linux side end-to-end; Windows reuses containerd's task API |
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
- Workers run compilations in Firecracker VMs (driven directly by hpcc)
  with no virtio-net device attached.
- Image rootfs is read-only, optionally dm-verity signed. The pause+agent
  binary hpcc injects (§4.3.1) is the only worker-controlled content; user
  image bytes are not modified.
- Per-VM audit log: image digest, source digest, flag set, output digest,
  duration, exit code, tenant/scheduler/worker/VM IDs.
- **Client→scheduler**: mTLS. Client trusts the scheduler's CA.
- **Client→worker**: scheduler-issued JWT in the `RouteResponse`. Worker
  verifies the signature against the scheduler's public key — no
  scheduler↔worker connection needed for auth. Client pins the worker's
  TLS cert via the SHA-256 fingerprint included in the `RouteResponse`
  (registered by the worker at startup). No separate PKI needed — the
  scheduler is the trust root (§4.8).
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
14. **Manifest-mode hashing** — implemented in
    `internal/compiler/invocation.go` (`Invocation.CacheKey`, branching on
    `ctx.Config.PreprocessingMode`). Manifest path enumerates inputs +
    `FindDependencies` output (per-compiler `-M` runner in
    `internal/compiler/clang.go`, `cl.go`) and hashes file contents
    directly without materializing preprocessed bytes. The shared
    `cacheKeyFlags` canonical encoding lives in
    `internal/compiler/cache_key.go` (+ `cache_key_test.go`).
15. **Scheduler** — `internal/scheduler/scheduler.go` (+
    `scheduler_test.go`), `internal/scheduler/state.go`,
    `internal/scheduler/config.go`. gRPC `Authenticate` / `Route` /
    `RegisterWorker` / `Heartbeat`, JWKS-backed user auth, static-token
    worker auth, scheduler-signed Ed25519 task JWTs, sticky-tenant +
    image-digest + load-aware routing. `cmd/scheduler.go` launches it.
16. **Pause binary** — `pause/main.go`, `pause/reap_linux.go`,
    `pause/reap_other.go`. Tiny static PID-1 binary with cross-platform
    SIGTERM handling and Linux-specific zombie reaping; separate Go
    module so it can be built `linux/amd64 + linux/arm64 + windows/amd64`
    without dragging in the rest of the tree. **Under raw Firecracker the
    Linux build of this binary will absorb the in-VM agent role (§4.3.1)
    and become `hpcc-agent`; Windows keeps it as pause-only.**
17. **Worker image store** — `internal/worker/image/image.go` (+
    `image_test.go`, `image_integration_test.go`). Currently
    containerd-driven pull, pause-layer injection in the content store
    under a worker-side digest, `dev.hpcc/user-digest` label so prepared
    images are distinguishable from raw pulls. `Store.GetExistingImages`
    and `PullImage(path, expectedDigest)` form the surface the worker
    calls. **Under raw Firecracker this gets replaced (Linux) by a direct
    OCI pull + flatten + ext4-build pipeline; the digest-identity surface
    stays the same.**
18. **Worker runtime interface + dev backend** —
    `internal/worker/runtime/runtime.go` (interface + `ContainerSpec`,
    `ExecRequest`/`ExecResult`), `dangerous.go`
    (`DangerouslyExecOnHost`: forks `os/exec` children with
    boundary-aware `/src` and `/out` path translation; gated behind
    the deliberately awful `runtime.handler = "really_really_dangerous"`
    config string), `select.go` (handler-string dispatcher with clear
    "not implemented yet" errors for `aws.firecracker` and
    `runhcs-wcow-hypervisor`). The real Firecracker /  hcsshim shims slot
    in behind this interface — see "Remaining."
19. **Client-side argv rewriting** —
    `internal/compiler/rewrite.go` (+ `rewrite_test.go`). Adds three
    pieces that the client uses to prepare a `CompileRequest` for the
    chosen source-delivery mode:
    - `RewritePathPrefix(inv, hostPrefix, vmPrefix)` — hand-written,
      boundary-aware substring substitution across `RawArgs` and
      structured fields. No regex; sibling dirs (`/proj-other` vs
      `/proj`) don't false-match. Used by CAS.
    - `Compiler.RewriteForPreprocessed(inv, srcPath)` — new interface
      method. Rebuilds argv as `-x <lang> -c <srcPath> [-o <out>]
      <kept-flags>`, dropping includes/defines/passthrough/linker-only
      flags. Implemented for clang (auto-detects `cpp-output` vs
      `c++-cpp-output` by compiler name, explicit `-x`, or input
      extension); MSVC stub returns "not implemented." Used by
      PREPROCESSED.
    - `ValidateNoHostPaths(args)` — substring blacklist of
      `/home/`, `/Users/`, `:\Users\` (any case). Worker calls this
      after token validation so a client that botched its rewrite
      surfaces loudly instead of silently failing inside the VM.
20. **Worker compile pipeline (handler-only)** —
    `internal/worker/worker.go`, `runtime_executor.go`, `staging.go`,
    `config.go` (+ `worker_test.go`). End-to-end Compile handler doing:
    descriptor + token validation (Ed25519 JWT against the scheduler's
    pubkey) → host-path argv sanity check → per-RPC src/out tmpdir
    staging (PREPROCESSED only; CAS returns a clear
    not-implemented error) → `runtime.Start` → `compiler.Detect` +
    `runtimeExecutor` adapter (translates `compiler.Executor` calls to
    `runtime.Container.Exec`, in-container `/out` paths to host paths
    for `ReadOutput`) → `compiler.Invoke` → cache lookup/store in
    paranoid mode → `CompileResponse{Stdout,Stderr,ExitCode,
    OutputArtifact,CacheKey}`. Per-RPC `compiler.Context` configured
    with `PreprocessLocal` so cache-key derivation goes through the
    in-VM preprocessor; `Invocation.PreprocessedDigest` short-circuits
    that for PREPROCESSED-mode requests. Worker config grew a `[[cache]]`
    section sharing `config.CacheConfig` with the client side; the
    factory is `cache/store.FromConfig` (one place, both callers).
    Scheduler liaison loop (auth, register, heartbeat) lives on the
    same `Worker` value. **No `cmd/worker.go` yet — the handler can't
    actually accept gRPC connections; it's only reachable from tests.**
21. **Cache-key precomputed-digest path** —
    `Invocation.PreprocessedDigest *[32]byte`; `CacheKey` short-circuits
    on it before consulting `Config.PreprocessingMode`. Lets paranoid
    mode produce stable cache keys for PREPROCESSED-source compiles
    where there are no deps to walk and the in-container source path
    doesn't translate via `os.ReadFile`.

### Remaining

The end-to-end remote path is now wired up: `cmd/worker.go` runs the
gRPC server + scheduler liaison loop with TLS; `internal/daemon/dispatch`
authenticates via OAuth password grant, calls `scheduler.Route`, dials
the worker pinned by cert fingerprint, and ships PREPROCESSED-mode
`CompileRequest`s; the daemon falls back to local on any remote-side
error. `internal/worker/runtime/pool.go` (`PooledRuntime`) gives
per-tenant container reuse — `NewWorker` wraps the inner runtime in
the pool, keyed on `(TenantID, ImageDigest)`, with `idle_timeout` and
`pool.max_active` driving the reaper / cap. Image-pull-on-miss in the
worker is in (`ensureImage`, singleflight'd by digest, with idle
eviction at `image.idle_timeout` cadence). Both compiler families now
implement `RewriteForPreprocessed` — the GNU walker (clang/clang++/cc/c++)
and the cl.exe walker (with the `/MD`-family runtime/EH carveout).
The `PooledRuntime` now also enforces §4.2's hard session timeout:
each entry tracks `createdAt` (preserved across park/pop), and a
configured `vm.session_timeout` evicts containers past that age at
both pop time and the reaper tick — independent of how recently they
were used. Per-job §4.12 audit records are populated on every
`CompileResponse` (tenant/worker/vm/image, source + output BLAKE3
digests, cache key, flags, exit code, duration, timestamp) and
mirrored to the worker log; a durable sidecar sink remains follow-up
work.

What's still open:

22. `internal/worker/runtime/` — real backends behind the existing
    interface:
    - **Linux: raw Firecracker driver.** Three pieces, all hpcc-owned:
      (a) image→rootfs builder (OCI pull via `go-containerregistry`,
      flatten layers, `mkfs.ext4` to a sparse file, cache by
      `(user image digest, agent digest)`); (b) Firecracker VMM driver
      (boot `vmlinux`, attach rootfs/source/output drives, vsock device,
      no virtio-net; snapshot/restore for §4.2 idle re-warm); (c) host-side
      vsock client to the in-VM `hpcc-agent` for Exec dispatch and stdio
      streaming. Replaces the firecracker-containerd path that was
      sketched in earlier drafts — see §4.1 for why.
    - **Windows: containerd + hcsshim Hyper-V.** Container start with the
      `hpcc-pause.exe` entrypoint, mount setup via Hyper-V volume,
      `Task.Exec` per compile. Follow-up to v1.
    - `select.go` still returns "not implemented yet" for both
      `aws.firecracker` and `runhcs-wcow-hypervisor`; only
      `really_really_dangerous` (host exec) works.
23. **CAS staging** in `internal/worker/staging.go` — currently
    returns `not implemented`; only PREPROCESSED works end-to-end. CAS
    needs blob-list materialization (reuse the Phase 3 store as the
    CAS). Cache-key derivation already works for it via
    `PreprocessLocal`; only the staging side is open. (SharedRoot
    mode was scoped earlier and dropped.)
24. **Client-side mode selection** — `dispatch.Dispatch` always
    preprocesses locally and sends `SourceMode_PREPROCESSED`. Once
    CAS staging exists on the worker, the daemon needs to pick a mode
    (config + per-job heuristics), call `RewritePathPrefix` for CAS,
    and populate the matching `RemoteDescriptor` oneof. The rewrite
    helpers exist (`internal/compiler/rewrite.go`); they have no
    caller for the non-PREPROCESSED mode yet.
25. `internal/cache/store/s3.go` — S3-compatible remote store
    (Phase 3). The worker's `[[cache]]` section already routes through
    `cache/store.FromConfig`, so the new backend slots in there.
26. **Worker descriptor-sourced cache-key strategy** — currently
    hardcoded to `PreprocessLocal` plus the precomputed-digest
    short-circuit. As source modes evolve (CAS digests as direct
    inputs, optional client-supplied keys) the worker's
    `compileContext` should pick its strategy from
    `descriptor.source_mode`.
27. Polish: `hpcc inspect`, `hpcc explain`, local-cache LRU eviction
    under `max_size`, Prometheus endpoints on daemon / worker /
    scheduler, per-build summary.
28. **Rootfs extraction hardening (§4.14)** —
    `internal/worker/image/rootfs/rootfs.go` currently shells out to
    `tar -xpf` and `mkfs.ext4 -d` on attacker-controlled OCI image
    bytes. The mkfs side is narrow; the tar side has well-known
    structural attack vectors (traversal, symlink racing, hardlinks
    to host paths, suid escalation under staging, device-node
    creation, tar bombs) whose mitigation today is "modern GNU tar's
    defaults are mostly OK." Plan: (a) document tmpfs +
    `nosuid,nodev,noexec` for the rootfs cache parent as a
    deployment requirement; (b) cap extracted size against a
    configured ceiling; (c) replace the `tar` shell-out with a
    Go-native extractor along the lines of containerd's snapshotter
    — explicit traversal/symlink/hardlink validation, optional suid
    stripping, no foreign tar implementation in the trust boundary.
    Required before this lands in a regulated-environment
    deployment.
