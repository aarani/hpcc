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
  compiles in a per-tenant pool, torn down on idle. **gVisor was considered and
  rejected:** it's a userspace kernel intercepting syscalls, not the
  kernel+KVM boundary a regulated security review actually recognises. No
  competing OSS distributed compiler ships hardware-virtualised
  per-tenant isolation — sccache-dist runs bwrap, distcc runs nothing.
- **The VM has no NIC.** There is no exfiltration argument to have, because
  there is no network device. Full stop. The host↔guest channel is one
  vsock device carrying a single bidirectional gRPC stream.
- **No SMB across the partition boundary** (Windows side). The
  default Microsoft-blessed way to share host paths into a
  Hyper-V-isolated container is VSMB — same SMB protocol family
  that's absorbed EternalBlue and a two-decade tail of kernel-mode
  RCEs. Stapling that across a boundary whose entire pitch is
  "auditors recognise this kernel+VM line" rebuilds the threat
  model in software. The Windows runtime mirrors the Linux side
  instead: `hpcc-agent` over HvSocket (the Hyper-V analogue of
  vsock) with a small protobuf wire we own (`Exec`, `Put`, `Get`)
  — not an industry-standard filesystem protocol with a
  CVE-of-the-month history.
- **The container image digest *is* the toolchain identity.** No "hash the
  gcc binary" dance. 50 developers sharing one image produce one cache
  bucket; CI and laptops cannot silently diverge.
- **CAS-mode dispatch** (Bazel/RBE-style, the default
  `source_mode = "cas"`): client builds a content-addressed manifest,
  probes the worker's compile cache by manifest digest (1 RPC, ~32
  bytes), and only streams missing source blobs on miss. Probe-hit is
  the common path on incremental builds — including cross-developer
  hits via a `.hpcc` project marker that normalizes paths so two
  checkouts at different absolute paths produce identical manifest
  digests. The worker re-hashes every uploaded blob with BLAKE3 and
  stores under the recomputed digest (a malicious client cannot
  poison cache content). The same `source_mode` field also picks
  the local cache-key algorithm, so client and worker compute
  matching keys without a second knob. `"preprocessed"` remains
  selectable for the inline-bytes fallback. See
  [docs/plan/cas.md](docs/plan/cas.md).
- **Auto-injected reproducibility flags**, family-aware: GCC/Clang
  get `-ffile-prefix-map=/src=.` + `-Werror=date-time`; MSVC gets
  `/d1trimfile:/src` + `/PDBSourcePath:/src`. The per-Exec staging
  dir gets stripped from `.o`/`.obj`/`.pdb` embedded paths so two
  Execs of the same source produce byte-identical outputs. Pinned
  locale/timezone/hostname inside the VM round it out.
- **Per-job audit row** — `(image_digest, source_digest, flags, output_digest,
  tenant, worker, vm, duration, exit)` — reproducible from a single line.
  This is the table format regulated audit teams want to see.
- **Per-tenant OAuth2 IdP.** Each tenant in the scheduler config
  declares its own IdP (Okta, Keycloak, Auth0, anything
  OAuth2-compliant) — token URL, JWKS, audience, client. The
  client knows only its `tenant_id` + scheduler URL; an
  unauthenticated `GetTenantIdP` RPC returns the OAuth endpoints
  for that tenant so laptops never hardcode IdP coordinates.
  Scheduler validates the password-grant JWT against the named
  tenant's JWKS — an IdP configured for tenant A is never asked
  to verify a token labeled as tenant B — then signs a short-lived
  routing token tenant-, image-, and worker-scoped that the client
  dials the worker with over pinned TLS. Same `tenant_id` scopes
  the storage namespace, the per-job audit row, and (when wired)
  the per-tenant upload quota.
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
  unsolved in OSS today. Under Hyper-V isolation hpcc bind-mounts
  `hpcc-agent.exe` as PID 1 of the utility VM and dispatches every
  Exec over the same bidi-stream `AgentService.Exec` RPC the Linux
  side uses, just terminated by HvSocket (the Hyper-V analogue of
  vsock) instead. Process isolation is a CI / dev fallback for hosts
  without nested virtualization; both paths share the same image
  pull, OCI spec, and `Container.Exec` surface.

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
| [Phase 1](docs/plan/phase-1-compiler-wrapping.md) | Core Compiler Wrapping | Done |
| [Phase 2](docs/plan/phase-2-daemon.md) | Daemon Architecture | Done |
| [Phase 3](docs/plan/phase-3-remote-cache.md) | Remote Cache (S3) | Done |
| [Phase 4](docs/plan/phase-4-distributed.md) | Distributed Compilation in Per-Tenant Firecracker VMs | Done |
| [Phase 5](docs/plan/phase-5-observability.md) | Observability & Polish | In progress |

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

### Phase 4 — Distributed Compilation in Per-Tenant VMs ✅
The differentiated phase. Raw Firecracker microVMs on Linux and
Hyper-V isolated containers on Windows, both driven directly by
hpcc behind a single `Runtime` interface. One long-running
VM/container per tenant session; compiles dispatch as one gRPC
bidi-streaming `AgentService.Exec` call over vsock (Linux) or
HvSocket (Windows Hyper-V). The user supplies an OCI image; on
Linux the worker pulls, flattens, and streams the layer tar through
an in-tree clean-room squashfs writer — no host staging dir, no
`tar -xpf` shell-out, no GPL deps in the build path — and on
Windows containerd + the runhcs shim handles the snapshot. Either
way `hpcc-agent` is injected as PID 1 so the VM stays alive across
compiles even on distroless / scratch / nanoserver images. This
replaces firecracker-containerd (stagnated upstream) with a small
image→rootfs pipeline and a one-method gRPC agent we own.
Route-only scheduler (signs JWTs, never touches payloads); client
authenticates to the scheduler via OAuth2 password grant against
any IdP (Okta / Keycloak / Auth0 / etc.), receives a short-lived
routing token, and dials the worker directly with per-call zstd,
that scheduler-signed token, and cancellation. Per-job audit log. See
[docs/plan/phase-4-distributed.md](docs/plan/phase-4-distributed.md)
for the full design and the **Limitations** section below for
known gaps.

**What shipped:**

*Linux/Firecracker:* end-to-end remote-compile path landed and
CI-tested — route-only scheduler, worker `Compile` RPC, per-tenant
container pool with idle/session TTLs, streaming image→squashfs
build (clean-room Go writer; no tar/mkfs shell-outs, on-wire format
validated in CI via `unsquashfs` round-trip), raw Firecracker driver
under jailer, in-VM `hpcc-agent` as PID 1 over vsock, and an
integration suite that boots a real toolchain rootfs and compiles
end-to-end on a GitHub Actions runner.

*Windows/hcsshim:* containerd + runhcs driver behind the same
`Runtime` interface, with two isolation modes wired and tested.
**Hyper-V isolation** (the §4.1 audit-recognised boundary) bind-mounts
`hpcc-agent.exe` as PID 1 of the utility VM and dispatches Exec calls
over HvSocket — no VSMB across the partition boundary, see
[docs/plan/phase-4-distributed.md](docs/plan/phase-4-distributed.md)
§4.1.1 "Why not VSMB". **Process isolation** (fallback for hosts
without nested virtualization) keeps `pause.exe` + `Task.Exec` +
copyTree. CI covers both: the GitHub-hosted `windows-runtime` job
exercises process isolation; a self-hosted `[self-hosted, nested]`
runner runs `windows-runtime-hyperv` against a real utility VM.

*Both source modes are wired:* CAS (the default — content-addressed
manifests with probe-then-upload, design in [docs/plan/cas.md](docs/plan/cas.md))
and PREPROCESSED (selectable fallback that ships preprocessed bytes
inline). Path normalization handles `\\?\` extended-length prefixes,
rejects UNC up front, case-folds in the digest for cross-platform
cache hits, and auto-injects family-aware reproducibility flags
(GCC `-ffile-prefix-map`/`-Werror=date-time`, MSVC `/d1trimfile:` /
`/PDBSourcePath:`). See **Limitations** below for known gaps.

### Phase 5 — Observability & Polish
`hpcc inspect <hash>` and `hpcc explain <file>` with structured miss
reasons. Prometheus endpoints on daemon, scheduler, worker. TOML config
resolved via `os.UserConfigDir()`. LRU eviction for cache and rootfs
blobs.

**What shipped so far:** every binary is on `go.uber.org/zap` through
`internal/logging`; `HPCC_LOG_LEVEL` / `HPCC_LOG_FORMAT` pick level
and console-vs-JSON output. The §5.5 security-event channel is wired
— `logging.Security` fires at every misbehaving-client validation
site (daemon auth, worker `Compile` + CAS RPCs, scheduler auth/route/
heartbeat, agent `Exec`) with `category=security`, `severity=critical`,
and a kebab-case `event` tag a log pipeline can filter and alert on.
JWT-validation events also attach the unverified claims for forensics
under `jwt_claims_unverified`; the raw bearer token is never logged.
OpenTelemetry tracing covers both server-side hops: `Worker.Compile`
emits per-phase child spans (`verify_manifest`, `ensure_image`,
`stage_source`, `runtime_start`, `cache_lookup`, `invoke`,
`collect_extras`, `cache_store`); the scheduler installs
`otelgrpc.NewServerHandler` so `Authenticate` / `Route` /
`RegisterWorker` / `Heartbeat` each become root spans; and
`ExecHeader` carries `traceparent` / `tracestate` so the in-VM agent
can stamp the trace ID onto its log records for cross-system
correlation. Both run behind the standard
`OTEL_EXPORTER_OTLP_ENDPOINT` env var (no-op without it). The §5.1
Prometheus / OTel metrics surface is live: `internal/metrics` wraps
the OTel metrics SDK with an always-on Prometheus reader on
scheduler and worker (new `metrics_listen` TOML field; separate
HTTP listener) and an OTLP push exporter on every binary including
the daemon (gated on the standard OTLP env vars, so dev laptops
stay quiet). Counters shipped: per-binary compile/auth/route/
heartbeat/CAS, plus a cross-binary `hpcc.security_events_total`
that's wired into `logging.Security` so every existing call site
emits a sample without touching the call site. Observable gauges,
durable security-event sidecar, daemon/agent tracing, and `hpcc
explain <file>` are the remaining Phase-5 work.

---

## Limitations

Known gaps and "won't currently do" — most are scheduled fixes, not
design dead-ends. Tagged with the plan section that owns the
follow-up.

- **PREPROCESSED dispatch demotes `-Werror[=*]`.** A two-step
  compile (client `gcc -E`, worker `gcc -x cpp-output -c`) loses
  gcc's macro-expansion warning-suppression heuristic. Stripping
  `-Werror` at the rewrite makes the worker compile match what
  local-mode gcc one-step would have produced; warnings still emit.
  One-shot yellow notice in the build log when this fires. **CAS
  mode (§4.5) is the workaround**: a CAS dispatch is a one-step
  compile on the worker, so `-Werror` survives intact.
- **Assembly (`.S` / `.s`) needs CAS mode for remote dispatch.**
  PREPROCESSED can't ship `.incbin`'d data; in PREPROCESSED-mode
  the assembly carve-out falls these back to local invoke. Under
  `source_mode = "cas"` the manifest captures the full closure
  and the worker assembles against it normally.
- **Stdin (`gcc -c -`) and multi-input compiles are not cacheable.**
  Stdin would need to be consumed twice (hash + compile); multi-
  input produces one `.o` per source that the single-output cache
  entry shape can't represent.
- **Windows Hyper-V isolation needs a runner that supports nested
  virt.** The runtime is shipped on both Linux/Firecracker and
  Windows/hcsshim, with two isolation modes on Windows. End-to-end
  CI for the Hyper-V path runs on a self-hosted runner labelled
  `[self-hosted, nested]`; GitHub-hosted `windows-2022` covers only
  the process-isolation path (no nested virt available there).
  Operators planning to deploy under Hyper-V isolation need a host
  with nested virt enabled in the hypervisor, the Hyper-V Windows
  feature installed, and `vmcompute` running.
- **Toolchain parity between local and remote is manual.** Local
  mode runs the host's gcc / cl.exe; remote mode runs the OCI image's
  toolchain. Different versions silently produce different objects
  for the same cache key, defeating cross-developer hit rates. Pin
  the image patch version (e.g. `gcc:13.2.0`, the VS Build Tools
  release for MSVC) to match the host until §4 ships an automatic
  parity check.
- **No per-tenant CAS upload quota.** Multi-tenant CAS is
  unbounded — one tenant's noisy CI can monopolize a worker's
  source store budget at the expense of every other tenant on
  that worker. Per-tenant IdP and storage-prefix isolation are
  both in place (see
  [docs/plan/multi-tenant.md](docs/plan/multi-tenant.md)); the missing
  piece is a token bucket on `UploadBlobs` keyed by `tenant_id`
  plus the matching client-side fallback. Deferred to
  [phase-5-observability.md §5.7](docs/plan/phase-5-observability.md)
  because its overrun event is a security-event-log row.
- **No `hpcc explain <file>`.** Structured cache-miss reasons are
  Phase 5.
