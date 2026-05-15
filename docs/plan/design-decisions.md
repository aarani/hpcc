## Design Decisions

### Hashing Strategy

Two strategies ship in v1, selected by `source_mode`:

- **Preprocess-then-hash** (PREPROCESSED): run the preprocessor
  locally, hash the resulting bytes. Default. Simple, works for
  every well-shaped C/C++ workload.
- **Manifest-mode** (CAS): `-M`-based dep discovery + per-file
  BLAKE3 digest aggregation. Used when `source_mode = "cas"` so the
  worker can compute the same cache key from the manifest without
  ever seeing preprocessed source. Path normalization via a
  `.hpcc` marker file produces identical digests across developers
  with the same checkout at different absolute paths.

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

