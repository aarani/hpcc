# hpcc

**A distributed compiler cache that a bank's security team will actually approve.**

---

## Why?

`ccache` is great on your laptop. `sccache` adds a daemon and a remote cache.
`distcc` farms compiles across machines. They all share one assumption:
**the worker is trusted shared-kernel infrastructure.**

That assumption is where the conversation ends in a regulated enterprise.
Bank security review isn't asking *"is namespace isolation technically
sufficient?"* — they're asking *"is this a boundary auditors recognize?"*
A bwrap sandbox is not. A KVM boundary is.

hpcc is built on a different assumption: **the worker is hostile-by-default,
multi-tenant, and on the audit trail.**

- **One Firecracker microVM per tenant session.** Separate kernel, KVM
  boundary, snapshot-on-idle so you pay ~10ms restore instead of ~125ms cold
  boot. No competing OSS distributed compiler does this — sccache-dist runs
  bwrap, distcc runs nothing.
- **The VM has no NIC.** There is no exfiltration argument to have, because
  there is no network device. Full stop.
- **The container image digest *is* the toolchain identity.** No "hash the
  gcc binary" dance. 50 developers sharing one image produce one cache
  bucket; CI and laptops cannot silently diverge.
- **Server-side preprocessing in CAS mode** (Bazel/RBE-style): client sends
  digests, worker materializes the include closure from a shared blob store.
  Cross-developer hit rates that client-preprocessing tools can't reach.
- **Auto-injected reproducibility flags** (`-Werror=date-time`,
  `-ffile-prefix-map`, `-frandom-seed`) plus pinned locale/timezone/hostname
  inside the VM. Byte-identical outputs by default, not by ceremony.
- **Per-job audit row** — `(image_digest, source_digest, flags, output_digest,
  tenant, worker, vm, duration, exit)` — reproducible from a single line.
  This is the table format banks want to see.
- **Structured miss explanations.** `hpcc explain <file>` names *which
  header* or *which flag* changed. Not a debug log you have to grep.
- **Per-call zstd on the wire.** Preprocessed C++ compresses 5–10×; this is
  the single largest perf lever and it's on by default.
- **Paranoid mode** (`paranoid = true`): cache reads and writes happen
  only on the worker — clients never touch the cache stores, never hold
  remote-store credentials. A compromised laptop cannot poison the cache.
- **Hyper-V isolated Windows containers** behind the same `Runtime`
  interface — MSVC on shared workers with a kernel boundary, which is
  unsolved in OSS today.

The cache loop and the daemon are table stakes; sccache does those well.
hpcc's bet is that the *next* place compiler-distribution has to go — into
regulated, multi-tenant, auditable environments — is a place none of the
existing tools can follow without rebuilding their isolation model from
scratch.

---

## Roadmap

Full plan in [docs/plan.md](docs/plan.md).

| Phase | Description | Status |
|-------|-------------|--------|
| [Phase 1](docs/plan.md#phase-1-core-compiler-wrapping) | Core Compiler Wrapping | Done |
| [Phase 2](docs/plan.md#phase-2-daemon-architecture) | Daemon Architecture | Done |
| [Phase 3](docs/plan.md#phase-3-remote-cache) | Remote Cache (S3) | Not started |
| [Phase 4](docs/plan.md#phase-4-distributed-compilation-in-per-tenant-vms) | Distributed Compilation in Per-Tenant Firecracker VMs | Not started |
| [Phase 5](docs/plan.md#phase-5-observability--polish) | Observability & Polish | Not started |

### Phase 1 — Core Compiler Wrapping ✅
Two-grammar (GNU + MSVC) spec-table parser, compiler detection from
`argv[0]`, preprocess- and manifest-mode hashing, content-addressable disk
cache, drop-in symlink wrapper, `hpcc wrap / stats / clean`.

### Phase 2 — Daemon Architecture ✅
Long-running foreground process over loopback TCP with a per-daemon auth
token, length-prefixed protobuf (not gRPC — the wrapper is on the hot
path), in-flight deduplication by cache key, daemon-down fallback.
`hpcc start` runs the daemon in the foreground; lifecycle is managed by
the user's terminal or a process supervisor (systemd, launchd, etc.).

### Phase 3 — Remote Cache
S3-compatible blob store as a `Store` implementation (AWS S3, MinIO, R2,
GCS-via-S3). Multi-tier lookup with backfill, configurable timeouts, AWS
credential chain. No custom server binary.

### Phase 4 — Distributed Compilation in Per-Tenant VMs
The differentiated phase. Firecracker microVMs on Linux (Hyper-V containers
on Windows, follow-up). One VM per tenant session, snapshot on idle, LRU
eviction. OCI image → ext4 rootfs conversion cached by digest. Server-side
preprocessing (`shared_root` / `cas` / `preprocessed` modes). Route-only
scheduler (returns a worker address + TLS trust info, never touches compile
payloads); client dials the worker directly over gRPC with per-call zstd,
mTLS, and cancellation. Per-job audit log.

### Phase 5 — Observability & Polish
`hpcc inspect <hash>` and `hpcc explain <file>` with structured miss
reasons. Prometheus endpoints on daemon, scheduler, worker. TOML config
resolved via `os.UserConfigDir()`. LRU eviction for cache, rootfs blobs,
and VM snapshots.

---

## Status

Phases 1 and 2 are implemented. Phase 3 is next.
