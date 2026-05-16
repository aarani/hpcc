## Phase 4: Distributed Compilation in Per-Tenant VMs

**Progress so far:**

- **Done:** §4.1.1 Runtime abstraction (`internal/worker/runtime`,
  with `DangerouslyExecOnHost` as the dev backend and a real
  `Firecracker` driver in production); §4.3 image→rootfs pipeline
  (streaming OCI tar → in-tree clean-room Go squashfs writer, no
  host staging dir, no `tar`/`mkfs.*` shell-outs, on-wire format
  validated in CI via `unsquashfs` round-trip; agent injection +
  standard mountpoints handled inline); §4.4 VM layout under jailer
  (vsock device, no NIC, kernel + rootfs staged into the chroot);
  §4.4.1 in-VM `hpcc-agent` (separate Go module, PID-1 init that
  mounts `/proc` `/sys` `/dev` `/tmp` `/run` + zombie reaping +
  bidi-streaming gRPC `AgentService.Exec` over AF_VSOCK port 17727);
  shared `proto/agent` module so the runner and agent compile against
  one wire schema; §4.8 route-only scheduler; §4.9 worker (Compile
  RPC, image catalogue + idle eviction, per-tenant container pool
  with idle/session TTLs, Ed25519 task-JWT verification); §4.10
  client→worker gRPC compile path with per-call zstd; §4.12 per-job
  audit records; §4.13 paranoid-mode plumbing on the worker. An
  integration suite (`firecracker_e2e_test.go`, runs in CI under
  `sudo` on Ubuntu with KVM) downloads firecracker + jailer, builds
  a real chainguard `gcc-glibc` rootfs, and compiles a one-line C
  source end-to-end through the full vsock + agent pipeline.

- **Open:**
  - §4.1.1 Windows hcsshim path — in flight. The
    `runhcs-wcow-hypervisor` handler now boots through
    `runtime.Hcsshim` against a real containerd daemon: per-tenant
    container creation with optional Hyper-V isolation (selectable
    via `runtime.hcsshim.isolation = "hyperv" | "process"`,
    process-mode for CI without nested virtualization), pause-binary
    PID 1 via a runtime-managed bind mount at `C:\.hpcc` (no OCI
    layer injection, see "Why we don't inject pause as a Windows
    OCI layer" below), `Task.Exec`-based compile dispatch, and
    per-Exec copy-in/copy-out staging at `C:\src` / `C:\out`.
    Cross-compiles green on darwin and windows/amd64; unit tests
    cover option validation, argv path rewrites and the copyTree
    helper. **Still open**: an actual containerd-on-windows
    integration job (the unit-test job in `windows-build` only
    proves compile + path logic), an hpcc-agent over HvSocket so
    we can drop the per-Exec copy without falling back to VSMB
    (see "Why not VSMB" below), and validating the
    path-canonicalization gotchas the §4.1.1 caveats list
    (MAX_PATH, UNC vs mapped drive, directory junctions).
  - §4.11 VM-crash reaping with scheduler reroute — partial today
    (the runtime surfaces process exit, but the worker doesn't yet
    notify the scheduler to drop the dead VM from routing).
  - §4.14 structural hardening is done — the streaming squashfs
    rewrite collapsed the tar shell-out, on-host staging dir, and
    e2fsprogs trust surface, and the residual tar-bomb size/entry
    caps now fire in the streaming reader.

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
  `mutate.Extract`, stream the resulting tar through an in-tree
  clean-room Go squashfs writer (`squashfs/`). No host staging
  directory, no `tar -xpf`/`mkfs.*` shell-outs, no GPL deps in the
  build path. Bounded scope, ~1,500 lines including format encoder
  and the streaming tar→squashfs bridge. CI validates the produced
  images against `unsquashfs` on every build so format regressions
  are caught at PR time rather than in the firecracker boot path.
  See §4.3 and §4.14.
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

- **Tighter control over kernel + boot config.** No shim opinions to fight.
- **One fewer moving part on the worker host** — no containerd daemon, no
  shim, no devmapper pool to babysit on Linux.

Snapshot/restore was on the candidate list here too, but the warm-VM
pool already lands compiles into a running guest with zero boot tax —
Firecracker's cold boot is ~125 ms, the session timeout is hours, and
the steady-state cost of a snapshotted-on-idle restore vs. a fresh
boot is rounding noise against the compile itself. The warm pool plus
hard session timeout is the production answer; snapshot/restore is
deferred indefinitely.

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
  paths in objects/PDBs don't poison the hash. **Mostly shipped**:
  `normalizeManifestPath` strips `\\?\` (and `\\?\UNC\`) extended-length
  prefixes, emits project-relative paths with forward slashes (Linux and
  Windows clients of the same `.hpcc`-rooted project produce byte-identical
  manifest digests), and **rejects UNC paths outright** rather than passing
  them through silently — UNC↔mapped-drive resolution would need runtime
  lookup of share mappings, so the safer behaviour is to error up front
  with a clear message ("map the share to a drive letter or compile from
  a local copy") so the user can't accidentally produce two divergent
  cache keys for the same file. `AggregateManifestDigest` ASCII-folds
  path bytes to lowercase before hashing so a Windows client whose
  `/showIncludes` emits `src\foo.h` and a Linux client whose `gcc -M`
  emits `src/Foo.h` hit the same cache key; `BlobRef.Path` stays
  original-case for the worker so materialization opens the exact file
  the compile expects. MSVC reproducibility flags
  (`/d1trimfile:/src`, `/PDBSourcePath:/src`) are auto-injected by the
  dispatcher for `MSVCFamily` compiles — the runtime translator
  rewrites `/src` to the per-Exec staging dir so cl.exe gets the
  concrete prefix at compile time, and `.obj` / `.pdb` outputs become
  byte-identical across Execs. GNU/Clang reproducibility flags
  (`-ffile-prefix-map=/src=.`, `-Werror=date-time`) are now also
  auto-injected by the dispatcher for `GNUFamily` — both the
  client-side `rewritePrefix` and the worker-side `rewriteRoot`
  recognise `=` as a path boundary in addition to `/`, so the `/src`
  inside the `-ffile-prefix-map` payload translates to the per-Exec
  staging dir the same way bare path arguments do.
- **MAX_PATH (260) limit.** Monorepo builds blow past this routinely. Workers
  need `LongPathsEnabled` registry, and the worker may rewrite paths to
  `\\?\` form before invoking `cl.exe`.
- **Source mounting into Hyper-V-isolated containers.** Hyper-V containers
  can't bind-mount host paths the way Linux containers do; SMB-into-container
  has identity/auth quirks (virtual accounts can't authenticate to shares).
  Current shape: under `--isolation=process` (CI / dev-without-nested-virt
  only) the silo bind-mounts of `C:\src` and `C:\out` work directly. Under
  `--isolation=hyperv` we plan to use an `hpcc-agent`-over-HvSocket file
  transport — see "Why not VSMB" below.
- **Symlinks vs directory junctions.** Different semantics; pick a
  resolve-or-don't policy and stick to it for cache-key path canonicalization.

#### Why not VSMB (no SMB across the boundary)

The default Microsoft-blessed way to share host directories into a
Hyper-V-isolated container is **VSMB** — the same SMB protocol that
parses messages from untrusted-ish peers in a kernel-mode SMB server
component. SMB has eaten EternalBlue, the periodic Patch-Tuesday SMB
RCE, and a long tail of CVEs spanning two decades; putting an
unauthenticated message parser on the privileged side of a "this is
the boundary auditors recognise" partition rebuilds in software the
very threat model the kernel + KVM/Hyper-V boundary is meant to
deny. A regulated security review that recognises the partition
boundary will not recognise an SMB parser stapled across it.

The Linux side already avoids this: every host↔guest payload rides
one vsock device terminated by `hpcc-agent` (§4.4.1). The Windows
side mirrors that: a Windows build of `hpcc-agent` listens on
HvSocket (the Hyper-V analogue of vsock) and gets bind-mounted into
every container the way `pause.exe` is. The wire is the existing
`AgentService.Exec` bidi-stream from `proto/agent/agent.proto` —
header + input-file chunks one way, stdio + result + output-file
chunks back — not an industry-standard filesystem protocol with two
decades of CVEs.

**Status:** both sides of the transport are built.

*In-VM agent:* the Windows agent binary builds (`make
build-agent-windows`). The cross-platform server logic is shared
with the Linux build; only the listener differs (vsock vs. HvSocket
via `github.com/Microsoft/go-winio`'s `ListenHvsock` +
`VsockServiceID(17727)` so both transports share the same numeric
"port"). Init drops to mkdir'ing `C:\hpcc\src` / `C:\hpcc\out`
because HCS already wires the container's kernel filesystems.

*Host-side client:* `internal/worker/runtime/agent.go` carries
`execViaAgent` — a cross-platform helper that opens the Exec
bidi-stream, ships an ExecHeader + every regular file under
`SrcHostPath` as InputFile chunks, half-closes, then drains
stdio / result / OutputFile frames back out, writing outputs under
`OutHostPath` and forwarding stdio to caller writers.
`internal/worker/runtime/agent_dial_windows.go` carries
`dialAgentHvsock` — the Windows-only HvSocket dialer that takes a
utility VM GUID and returns a `*grpc.ClientConn`. Unit tests
exercise the helper against an in-process gRPC server so the
protocol shape is covered without nested-virt access.

*Still open:* the hcsshim runtime needs to (a) stage
`hpcc-agent.exe` alongside `pause.exe`, (b) override the container
entrypoint to the agent rather than pause when isolation is
Hyper-V, (c) look up the utility VM's GUID from the
`containerd.Container` handle (hcsshim-internals call), (d) call
`dialAgentHvsock` after container Start and store the connection
on the container, (e) replace the Task.Exec + copyTree path in
`Container.Exec` with `execViaAgent`. (c)–(e) need nested
virtualization to end-to-end-test, which GitHub-hosted runners
don't expose; landing the full integration will need a self-hosted
Windows runner or local repro on a Hyper-V host.

Process-isolation containers (CI, dev) keep using silo bind mounts
because there's no partition boundary to cross; the §4.1 security
claim is also already void in that mode, so VSMB doesn't enter the
picture.

#### Why we don't inject pause.exe as a Windows OCI layer

The cleaner-looking design would be: prepare a per-image variant
where `cdimage.Store` stacks a one-file OCI layer containing
`pause.exe` on top of the user image and registers the result as the
prepared image. That's exactly what cdimage does on Linux.

It doesn't work on Windows. `hcsshim.ImportLayer` (which the
`windows` snapshotter dispatches to for every non-base layer)
expects PAX-tagged tar entries with backup-stream security
descriptors, Windows file-attribute records, and the legacy
`Hives/`/`UtilityVM/` scaffolding — a synthesised one-binary layer
with plain `archive/tar` entries fails apply with
`ERROR_PATH_NOT_FOUND` before the first file lands.

Instead: on Windows the prepared image record is a label-only alias
of the base manifest (same target descriptor, just registered under
`prepared.hpcc.local/img:<digest>` with the user-digest label).
`pause.exe` is staged once at `NewHcsshim` time under
`<RunDir>\.hpcc-pause-mount\pause.exe` and bind-mounted read-only at
`C:\.hpcc` into every container; the OCI spec overrides
`Process.Args` to point at `C:\.hpcc\pause.exe`. Same end state as
the Linux layer approach (pause is PID 1, image content is
unchanged) without the legacy-tar-format trust surface.

The same machinery is what `hpcc-agent.exe` will use when the
HvSocket transport (above) lands — same staging dir, same mount,
same entrypoint pivot.

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
- **Idle timeout** (e.g. 5–15 min) → tear the VM down. The pool keeps
  the VM warm in RAM until the timer fires, then frees memory; the
  next demand from that tenant cold-boots a fresh VM (~125 ms, hidden
  by build setup time on any non-trivial build). Snapshot/restore was
  on the table when driving Firecracker directly came back into scope;
  in practice the warm-pool window covers the active-build case at
  zero latency tax, and outside that window Firecracker boots fast
  enough that snapshot/restore isn't worth its operational surface.
  Deferred indefinitely.
- **LRU eviction** of warm VMs under memory pressure.
- **Hard session timeout** (e.g. shift change, N hours) → blow the VM
  away. Long-lived per-tenant state accumulates and someone will
  eventually ask what's in it. Already enforced by `PooledRuntime`
  via `vm.session_timeout`.

### 4.3 Container Image as the Build Environment

The user supplies their toolchain by handing hpcc a **container image**
(any OCI-compatible registry: Docker Hub, GHCR, ECR, internal mirrors).

On the **Linux/Firecracker** path the worker pulls layers (via
`go-containerregistry`), flattens them via `mutate.Extract`, and
streams the resulting tar through an in-tree clean-room Go squashfs
writer (`github.com/aarani/hpcc/squashfs`) that produces a sealed
`.sqsh` rootfs file in a single pass. The bridge in
`internal/worker/image/rootfs/` validates each tar entry's path
(rejecting `..`, abs-paths-in-archive, NUL bytes, "." segments)
before dispatching to one of the squashfs writer's typed Create*
methods — regular file, directory, symlink, hardlink, char/block
device, FIFO, socket. `/.hpcc/agent` and the standard mountpoints
(`/proc /sys /dev /tmp /run`, with `/tmp` set sticky) are written
inline against the same writer. No host staging directory exists at
any point. The prepared rootfs is cached on disk keyed by the
user-image digest.

Why squashfs? It's naturally read-only (matching how the kernel
mounts it inside the guest), compresses well in the production path,
and lets us encode the entire on-disk image directly from a
streaming tar — no intermediate filesystem tree on the host, no
shell-outs. The squashfs format is also simple enough to encode
clean-room from a reverse-engineered writeup, so the whole pipeline
stays in code we own with no GPL trust boundary in the build path
(important for the licensing posture in §1).

Why not the previous ext4 + `mkfs.ext4 -d` approach? It worked but
forced a `tar -xpf` shell-out and an on-host staging directory full
of attacker-controlled file content (see §4.14), and depended on
`mkfs.ext4` / `e2fsprogs` being present on every worker host. The
streaming squashfs writer removes both surfaces in one change.

The production compressor is gzip via stdlib `compress/zlib` (the
squashfs "gzip" compressor ID expects an RFC 1950 zlib stream, not
the RFC 1952 gzip-with-header stream — `compress/gzip` would
produce something the kernel decoder rejects). Gzip is the most
universally compiled-in squashfs decompressor across Linux kernel
builds, including the minimal Firecracker reference kernels, so
it's the v1 choice over faster-but-less-universal zstd. Switching
to zstd later is a one-file adapter against the same `Compressor`
interface. A `StoredCompressor` (every block raw, gzip declared in
the superblock so the kernel never invokes a decoder) ships in the
package for tests and for callers that explicitly want
uncompressed output.

Sealed images are padded out to a 4 KiB boundary in `Writer.Close`,
matching `mksquashfs`'s default. `bytes_used` stays at the
unpadded logical size; the padding exists so the kernel's
`sb_bread` can read the last 1 KiB logical block of squashfs data
without short-reading past the block-device's reported size. (We
discovered this empirically: without padding, Firecracker's
virtio_blk reported the file size exactly, the kernel's last
read failed with `-EIO`, and `fill_super` returned without logging
because the failure path bails before the `SQUASHFS error:` print.)

On the **Windows/hcsshim** path containerd's image pull + snapshotter
does the equivalent.

- **Image digest is the toolchain identity** for cache keys. Same image
  used by 50 developers → one prepared rootfs on the worker, one
  toolchain identity in the cache. **Caveat**: this only holds when
  the dispatch path actually uses the image's toolchain end-to-end.
  Under PREPROCESSED the preprocessor runs client-side against
  whatever gcc the developer's machine has, so a cache-hit from one
  developer's preprocess + a worker compile against the image gcc
  would only be byte-identical if the two compilers agreed. In
  practice this means PREPROCESSED deployments need to enforce
  *toolchain parity* — either by building the image from a known
  apt source or by asserting `gcc --version` parity at
  session-open. **CAS mode sidesteps this**: the worker uses the
  image gcc for both preprocess and compile (one-step), and the
  client never preprocesses, so client/image compiler skew is
  irrelevant. The bench currently pins a specific patch-level
  Docker tag as a workaround for the PREPROCESSED path.
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
  conventional flag set; a periodic `Wait4(-1, WNOHANG)` reap loop
  (every 5s, no SIGCHLD subscription) drains zombies from orphaned
  compiler-helper grandchildren without racing Go's exec.Cmd waits on
  direct children; a gRPC server on AF_VSOCK port 17727 serves
  `AgentService.Exec` (§4.4.1). Lives in the `agent/`
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
  outputs back. Tradeoff is memory pressure for translation units with
  large preprocessed payloads — the guest needs RAM to hold the working
  set. Easy escape hatch (virtio-fs over vsock for host-dir passthrough)
  if any workload makes this an issue.
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
translation units with multi-MB preprocessed payloads. Cancellation
is free: the runner's `ctx.Cancel` closes the gRPC stream, which fires
the agent's `cmd.Run` context, which kills the in-guest compiler.

Path-traversal guard on incoming output paths in the runner (the agent
is trusted, but a kernel CVE or corrupted frame shouldn't translate
into the runner clobbering `/etc/passwd` on the host).

### 4.5 Source Modes: PREPROCESSED and CAS

Two source-staging strategies ship in v1, selected per-tenant via
the `remote.source_mode` config:

- **PREPROCESSED** (default): client runs `gcc -E` locally and ships
  the resulting bytes inline in
  `CompileRequest.descriptor.preprocessed`. The worker compiles them
  in-VM. Simple, works for every well-shaped C/C++ workload, but
  ships 1–50 MB of preprocessed text per TU and can't represent
  GAS `.incbin` (the .incbin'd bytes aren't in the preprocessor
  output).
- **CAS** (`source_mode = "cas"`): client builds a content-addressed
  manifest of the source closure, probes the worker's compile cache
  by manifest digest (`ProbeCompileCache`), and only streams the
  source blobs the worker doesn't already have (`FindMissingBlobs` +
  `UploadBlobs`) on miss. Wins on cross-worker AND cross-developer
  cache-hit rate (compile-result sharing via the existing
  `CompileCache` S3 store), bandwidth (probe = 32 bytes;
  upload-on-miss only ships project deltas), and unlocks `.S` /
  `.incbin` workloads that PREPROCESSED can't represent.

The full CAS design lives in [docs/plan/cas.md](cas.md). Key
properties:

- **Probe-then-upload.** Mirrors Bazel's
  `ActionCache.GetActionResult`. Incremental-build cache-hits land
  in one RPC (~32 bytes up, .o down).
- **`.hpcc` project marker.** Path normalization for cross-developer
  hits keys on a `.hpcc` file at the project root. Two developers
  with the same checkout at different absolute paths produce
  identical manifest digests.
- **Paranoid-mode invariant preserved.** Client never writes to the
  shared cache. Worker re-hashes uploaded bytes with BLAKE3
  incrementally; stores under the recomputed digest, never the
  client-claimed one.
- **Worker-local source storage.** Source blobs live on worker disk
  only, evict via the existing `DiskCacheStore` LRU. No S3 mirror
  for source — cross-worker source-blob sharing was rejected as a
  worse trade than a small extra upload after worker rotation.
  Cross-worker sharing of *compile results* is the high-value
  cross-worker property and is preserved via the existing
  CompileCache chain.
- **`.S` carve-out split.** `Invocation.Cacheable()` still excludes
  `.S` (local cache key would be unsound), but a new
  `Invocation.DispatchableUnderCAS()` allows it through the dispatch
  path. Daemon under `source_mode = "cas"` skips local cache for
  `.S` inputs and dispatches to the worker, where the manifest
  captures the full closure (`.incbin`'d files included).

(A `shared_root` mode that mounted the host tree directly into the
worker container was scoped earlier and dropped — too coupled to a
specific deployment topology, and the trust story for "client
filesystem appears in the worker's VM" was hard to defend in a
multi-tenant setup.)

The cross-tenant probe disclosure called out in this section's
history is closed by promoting `tenant_id` to a storage namespace
boundary on every cache and CAS store — see
[docs/plan/multi-tenant.md](multi-tenant.md). The matching
per-tenant upload quota is deferred to
[docs/plan/phase-5-observability.md §5.7](phase-5-observability.md)
since its overrun event is a security-event-log row that lands
with the rest of that infrastructure.

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

The single-IdP assumption baked into this section is lifted in
[docs/plan/multi-tenant.md](multi-tenant.md): the scheduler holds a
per-tenant IdP table, exposes an unauthenticated `GetTenantIdP`
discovery RPC, and validates each incoming JWT against the IdP
named by the `AuthRequest.tenant_id` field (not a JWT claim — so
an IdP configured for tenant A is never asked to verify a token
labeled as tenant B). The same doc adds per-RPC worker
enforcement: every `FindMissingBlobs` / `UploadBlobs` header now
carries the scheduler-issued task token, and the stream is
pinned to its first header's tenant.

### 4.9 Worker

- `hpcc worker --scheduler <addr>` — connects to the scheduler, drives
  per-tenant sandboxes via the `Runtime` interface.
  - **Linux**: raw Firecracker driver. Owns image pull, rootfs build,
    Firecracker VMM lifecycle, and the host-side end of the agent
    gRPC stream.
  - **Windows**: containerd + hcsshim with Hyper-V isolation (follow-up).
- Maintains per-tenant VMs, tears them down on idle timeout, evicts
  under memory pressure.
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

Paranoid mode is also where the multi-tenant work in
[docs/plan/multi-tenant.md](multi-tenant.md) earns the most: with
the cache prefixed by `tenant_id` and worker CAS streams
authenticated by a tenant-bound scheduler token, a compromised
tenant-A laptop cannot read or poison tenant-B's artifacts even
when worker-side credentials are in play.

### 4.14 Rootfs Extraction Hardening

The §4.3 image→rootfs pipeline runs against **attacker-controlled OCI
image bytes** — the entire multi-tenancy story assumes the tenant is
hostile, so every byte flowing through `crane.Pull` → `mutate.Extract`
→ the squashfs writer is on the threat surface.

Where the threats used to live, before the streaming squashfs
rewrite, were a `tar -xpf` shell-out, a host staging directory full
of materialized attacker content, and a `mkfs.ext4 -d` call against
that directory. That structure produced six concrete worry items —
path traversal via `../../etc/foo`, symlink racing across the
extraction window, hardlink targets resolving outside the
extraction tree, suid/sgid binaries briefly resident on the host
filesystem, mknod of arbitrary device nodes on the host, and
unbounded staging-directory size from uncompressed flattened OCI
tars — plus the kernel-level e2fsprogs trust surface for the
`mkfs.ext4` binary itself.

The streaming squashfs rewrite (`squashfs/` +
`internal/worker/image/rootfs/`) collapses that surface:

- **No `tar -xpf`.** OCI layer bytes are read with Go's
  `archive/tar` inside the worker process. Path validation
  (`normalizeTarPath`) rejects empty names, NUL bytes, absolute
  paths inside the archive, and any `.` or `..` segment at parse
  time, before the entry name reaches the squashfs writer.
- **No host staging directory.** Every tar entry becomes a typed
  `squashfs.Create*` call against an in-process writer that
  produces the final `.sqsh` file directly. Suid binaries, device
  nodes, and symlinks named by the user image only ever exist
  inside the sealed squashfs file — which is mounted read-only with
  `nodev,nosuid` inside the guest, neutralizing them at the place
  they would be evaluated.
- **No `mkfs.ext4` shell-out.** e2fsprogs is no longer in the
  trust boundary. The squashfs writer is clean-room Go in-tree code
  we own end-to-end, written against a non-GPL format reference.
- **Symlink-racing window is closed by construction.** There's no
  filesystem step where an entry can be opened by name and another
  entry races to redirect that name; entries are serialized into
  the squashfs file as the tar is read.
- **Hardlink validation.** `CreateHardlink` requires its target to
  have been created earlier as a regular file (`ErrHardlinkTarget`
  otherwise), so `linkname` referring to an off-image path or to a
  directory is rejected at writer time rather than silently
  resolving against the host.
- **`/.hpcc` namespace is stripped.** A hostile image cannot
  pre-empt the agent injection path by shipping its own
  `/.hpcc/agent` — those entries are dropped during the tar
  stream and the worker writes its agent afterwards. Tested.
- **On-wire format is externally validated.** `unsquashfs` from
  squashfs-tools is invoked against produced images in CI on every
  build, so a writer-side regression cannot silently emit a
  malformed but locally-self-consistent rootfs that only fails when
  the kernel tries to mount it.

#### Tar-bomb caps

The streaming writer enforces `(maxTarTotalBytes, maxTarEntryCount)`
ceilings inside `streamTarToSquashfs` — header-declared size that
already overshoots the cap is rejected before the body reads, and a
header that lies about Size is caught by an `io.LimitReader` around
the body copy. Either condition returns `ErrTarTotalBytesExceeded`
or `ErrTarEntryCountExceeded` wrapping a descriptive message. v1
caps are deliberately loose (16 GiB / ~1M entries) — the point is
unbounded growth, not policing image size.

A future optimization — *not* hardening — is swapping the
production compressor from gzip to zstd for smaller cache
footprint. The `Compressor` interface accommodates this without
any pipeline change.

### Milestone

A 16-core machine effectively compiles with `-j64` by distributing to
3 other machines in the cluster. Each tenant's compiles run in their own
Firecracker VM (driven directly by hpcc), reused across the build, torn
down on idle, with no network access from inside the VM.
