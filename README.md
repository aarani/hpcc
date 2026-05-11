<h1 align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/logo-dark.svg">
    <img alt="hpcc — vault cube mark" src="docs/logo.svg" width="200" height="200">
  </picture>
  <br />
  hpcc
</h1>

<p align="center">
  <strong>A distributed compiler cache that a regulated security team will actually approve.</strong>
  <br />
  <em>Sandboxed remote compilation · per-tenant KVM boundary · auditable by row.</em>
</p>

<p align="center">
  <a href="https://github.com/aarani/hpcc/actions/workflows/suite.yml"><img alt="Build &amp; Test Suite" src="https://github.com/aarani/hpcc/actions/workflows/suite.yml/badge.svg?branch=main"></a>
  <a href="https://github.com/aarani/hpcc/blob/main/LICENSE"><img alt="License: AGPL-3.0" src="https://img.shields.io/badge/license-AGPL--3.0-blue.svg"></a>
  <a href="https://go.dev/"><img alt="Go 1.26+" src="https://img.shields.io/badge/go-1.26%2B-00ADD8?logo=go&amp;logoColor=white"></a>
  <a href="https://goreportcard.com/report/github.com/aarani/hpcc"><img alt="Go Report Card" src="https://goreportcard.com/badge/github.com/aarani/hpcc"></a>
  <a href="https://pkg.go.dev/github.com/aarani/hpcc"><img alt="Go Reference" src="https://pkg.go.dev/badge/github.com/aarani/hpcc.svg"></a>
  <a href="https://hpcc.dev"><img alt="hpcc.dev" src="https://img.shields.io/badge/site-hpcc.dev-0e1014"></a>
</p>

---

> ⚠️ **Work in progress.** hpcc is under active development and has not been audited.
> Do not rely on it for security-sensitive or production workloads yet.

## Quick start

```sh
git clone https://github.com/aarani/hpcc.git
cd hpcc && go build && go install

# wrap a compiler invocation
hpcc wrap cc -c hello.c -o hello.o

# or wire into a Makefile
make CC="hpcc wrap cc" CXX="hpcc wrap c++"

# start the daemon (foreground; supervise with systemd / launchd)
hpcc start
```

See [`docs/plan.md`](docs/plan.md) for the full design and roadmap, and
[`docs/client.toml`](docs/client.toml) /
[`docs/scheduler.toml`](docs/scheduler.toml) /
[`docs/worker.toml`](docs/worker.toml) for example configs.

---

## Why?

`ccache` is great on your laptop. `sccache` adds a daemon and a remote cache.
`distcc` farms compiles across machines. They all share one assumption:
**the worker is trusted shared-kernel infrastructure.**

That assumption is where the conversation ends in a regulated enterprise.
A regulated security review isn't asking *"is namespace isolation
technically sufficient?"* — they're asking *"is this a boundary auditors
recognize?"*
A bwrap sandbox is not. A KVM boundary is.

hpcc is built on a different assumption: **the worker is hostile-by-default,
multi-tenant, and on the audit trail.**

- **One Firecracker microVM per tenant session**, driven directly by hpcc
  (no firecracker-containerd dependency — that project has stagnated, and
  for something whose value proposition is "this lives in regulated
  environments for years," depending on unmaintained orchestration is the
  wrong direction). Separate kernel, KVM boundary; the VM stays warm across
  compiles, snapshotted on idle timeout. **gVisor was considered and
  rejected:** it's a userspace kernel intercepting syscalls, not the
  kernel+KVM boundary a regulated security review actually recognises. No
  competing OSS distributed compiler ships hardware-virtualised
  per-tenant isolation — sccache-dist runs bwrap, distcc runs nothing.
- **The VM has no NIC.** There is no exfiltration argument to have, because
  there is no network device. Full stop. The host↔guest channel is one
  vsock device carrying a single bidirectional gRPC stream.
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
  This is the table format regulated audit teams want to see.
- **Structured miss explanations.** `hpcc explain <file>` names *which
  header* or *which flag* changed. Not a debug log you have to grep.
- **Per-call zstd on the wire.** Preprocessed C++ compresses 5–10×; this is
  the single largest perf lever and it's on by default.
- **Paranoid mode** (`paranoid = true`): cache reads and writes happen
  only on the worker — clients never touch the cache stores, never hold
  remote-store credentials. A compromised laptop cannot poison the cache.
- **Hyper-V isolated Windows containers** behind the same `Runtime`
  interface (raw Firecracker driver on Linux, containerd + hcsshim on
  Windows) — MSVC on shared workers with a kernel boundary, which is
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
| [Phase 3](docs/plan.md#phase-3-remote-cache) | Remote Cache (S3) | Done |
| [Phase 4](docs/plan.md#phase-4-distributed-compilation-in-per-tenant-vms) | Distributed Compilation in Per-Tenant Firecracker VMs | In progress |
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

### Phase 3 — Remote Cache ✅
S3-compatible blob store as a `Store` implementation (AWS S3, MinIO, R2,
GCS-via-S3). Multi-tier lookup with backfill. Per-call timeouts (2s reads,
5s writes, 30s lists), bounded body reads (1 GiB cap), watermark-gated
eviction (full-bucket scan only fires when the in-memory size estimate
overshoots `max_size` by 10%, instead of on every Put). All cache objects
namespaced under a `cache/` prefix so the bucket can be shared with other
tools without scan loops tripping on stray objects. Bucket auto-creation
is opt-in via `auto_create = true` for local MinIO setups; production
deployments leave it false. Standard AWS credential chain; no hpcc-specific
auth layer.

### Phase 4 — Distributed Compilation in Per-Tenant VMs
The differentiated phase. Raw Firecracker microVMs on Linux, driven
directly by hpcc (Hyper-V-isolated containers via containerd + hcsshim
on Windows, follow-up). One long-running VM per tenant session;
per-compile work is dispatched as one gRPC bidi-streaming Exec call into
the VM over vsock — header + input file chunks in, stdio + result + output
file chunks back, all under a single `AgentService.Exec` stream. The user
supplies an OCI image; the worker pulls + flattens it into an ext4 rootfs
via `tar -xpf` + `mkfs.ext4 -d`, injects the agent binary as PID 1 so the
VM stays alive across compiles even for distroless/scratch images. We
chose this over firecracker-containerd because that project has
stagnated; we own a small image→rootfs pipeline and a one-method gRPC
agent in exchange for not depending on unmaintained infra. The KVM
boundary, no-NIC story, and audit pitch are unchanged. Server-side
preprocessing (`cas` / `preprocessed` modes). Route-only scheduler
(returns a worker address + TLS trust info, never touches compile
payloads); client dials the worker directly over gRPC with per-call zstd,
scheduler-signed JWT auth, and cancellation. Per-job audit log.

**Phase 4 status (today):** route-only scheduler, worker `Compile` RPC,
per-tenant container pool with idle/session TTLs, image→ext4 pipeline,
raw Firecracker driver under jailer (vsock device, no-NIC,
`/proc/<pid>/root` reach for the namespace-isolated socket, lazy-unmount
cleanup), in-VM `hpcc-agent` (separate Go module, PID-1 init + bidi gRPC
over vsock), shared `proto/agent` module for the runner↔agent wire schema,
and an integration suite that downloads firecracker + jailer, builds a
real chainguard `gcc-glibc` rootfs, and compiles a C source end-to-end
on a GitHub Actions Ubuntu runner. Compiles dispatched through the
Firecracker runtime work end-to-end on Linux. Still open: VM
snapshot/restore on idle (today the pool just keeps warm VMs in RAM),
CAS-mode source staging on the worker (today only `PREPROCESSED` mode
works end-to-end), the Windows hcsshim path, and the rootfs-extraction
hardening tracked in §4.14 (Go-native tar reader replacing the
`exec.Command("tar", ...)` shell-out).

### Phase 5 — Observability & Polish
`hpcc inspect <hash>` and `hpcc explain <file>` with structured miss
reasons. Prometheus endpoints on daemon, scheduler, worker. TOML config
resolved via `os.UserConfigDir()`. LRU eviction for cache, rootfs blobs,
and VM snapshots.

---

## Status

Phases 1, 2, and 3 are implemented. Phase 4 is in progress: the Linux
end-to-end remote compile path — scheduler routing, worker dispatch,
image→rootfs build, raw-Firecracker boot, vsock + agent, real-gcc
e2e — is landed and CI-tested. The remaining Phase 4 work is snapshot/
restore for idle VMs, CAS-mode staging, the Windows backend, and the
rootfs-extraction hardening called out as a v1 follow-up. Phase 5 is
unstarted.
