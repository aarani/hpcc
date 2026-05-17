## Phase 5: Observability & Polish

Make it easy to understand what hpcc is doing and why.

**Progress so far:**

- **Done — process-wide structured logging:** every binary (daemon,
  scheduler, worker, agent, bench/fcstack) is on `go.uber.org/zap`
  through `internal/logging` (and a mirror inside the agent module,
  which can't import `internal/`). `HPCC_LOG_LEVEL` and
  `HPCC_LOG_FORMAT` env vars pick level and console-vs-JSON output;
  default is info/console to stderr.
- **Done — §5.5 security-event channel (structured-log half):**
  `logging.Security(event, msg, fields...)` at every misbehaving-
  client validation site across daemon auth, worker `Compile` /
  `ProbeCompileCache` / `FindMissingBlobs` / `UploadBlobs`, scheduler
  `Authenticate` / `Route` / `RegisterWorker` / `Heartbeat`, and
  agent `Exec`. Each entry carries `category=security`,
  `severity=critical`, an `event=<kebab-case>` tag, `rpc=<method>`,
  and the tenant / worker / image identifiers in context; JWT-
  validation events also attach the unverified claims payload under
  `jwt_claims_unverified` for forensics without ever logging the raw
  bearer token. Prometheus counters and the durable-sidecar half of
  §5.5 are still open.
- **Done — §5.8 OTel tracing on the worker compile pipeline:**
  `internal/tracing` boots an OTLP/gRPC exporter when
  `OTEL_EXPORTER_OTLP_ENDPOINT` is set (no-op otherwise);
  `otelgrpc.NewServerHandler` is installed on the worker gRPC
  server so each inbound RPC gets a root span with the upstream
  `traceparent` honoured. `Worker.Compile` emits child spans for
  `verify_manifest`, `ensure_image`, `stage_source`,
  `runtime_start`, `cache_lookup`, `invoke`, `collect_extras`, and
  `cache_store`, with per-phase attributes (image digest, vCPU
  count, cache_hit, exit_code, duration_ms) so a slow or failing
  compile shows the failing phase directly in the trace UI.
- **Done — §5.8 scheduler tracing + worker→agent propagation:**
  `otelgrpc.NewServerHandler` is now installed on the scheduler
  gRPC server, so `Authenticate` / `Route` / `RegisterWorker` /
  `Heartbeat` each produce a root span when the scheduler has an
  OTLP endpoint configured. `ExecHeader` (proto/agent/agent.proto)
  grew `traceparent` + `tracestate` fields; both Firecracker and
  agent-over-vsock runtimes inject the worker's current trace
  context via `propagation.TextMapPropagator.Inject` when sending
  the header. The in-VM agent stays dep-light — it parses the
  traceparent with a hand-written extractor and stamps the trace
  ID onto its zap log entries so operators can grep agent records
  by trace alongside worker spans. Real in-VM spans are still a
  follow-up (would require pulling the OTel SDK into the agent
  rootfs binary); CAS RPC per-blob spans on the worker, and
  daemon→scheduler/worker propagation, also remain open.
- **Done — §5.1 OTel-backed metrics surface:** new
  `internal/metrics` package wraps the OTel metrics SDK. Scheduler
  and worker get an always-on Prometheus reader exposed via a new
  `metrics_listen` TOML field (separate HTTP listener, plain
  HTTP, not on the gRPC port — empty disables). All three
  binaries — daemon, scheduler, worker — additionally push via
  OTLP/gRPC when `OTEL_EXPORTER_OTLP_ENDPOINT` (or the
  metric-specific variant) is set; the daemon has no Prometheus
  listener because client machines aren't typically scrape
  targets. Instruments wired so far:
  `hpcc.daemon.compiles_total{result}` +
  `hpcc.daemon.compile_duration_seconds`,
  `hpcc.worker.compiles_total{tenant_id,result}` +
  `hpcc.worker.compile_duration_seconds`,
  `hpcc.worker.cas_{bytes,blobs}_total{direction}`,
  `hpcc.scheduler.{auth,routes,heartbeats}_total`, and the
  cross-binary `hpcc.security_events_total{component,event,tenant_id}`
  registered into `logging.Security` via a hook in
  `internal/logging` so every existing call site fires a counter
  without touching the call site.
- **Done — §5.3 `hpcc explain <source-file>`:** new
  `internal/explain` package owns a JSON-file-per-source disk store
  under `$os.UserCacheDir/hpcc/explain/` (LRU eviction at 10k
  records, atomic rename on Put). The daemon writes one record on
  every compile attempt — hit, miss, bypass, error — carrying
  hex-SHA-256 sub-hashes of the inputs the cache key was built
  from: compiler identity (from `Compiler.Identity()`), canonical
  flags (from the new `compiler.CacheKeyFlagsBytes` accessor — same
  bytes the cache key consumes), source content, per-header content
  (paths parsed out of the `.d` file the compile produced, either
  on disk locally or shipped back as an extra by the worker), and
  the dispatcher's image digest when remote. The diff against the
  prior record is computed at write time (the daemon has both in
  hand) and embedded into the new record's `diffs` field, so the
  CLI is a pure read. `hpcc explain <source>` renders the latest
  outcome and the named change list:
  `compiler` / `flags` / `source` / `header <path>` / `image`.
  MSVC per-header attribution waits on capturing the
  `/showIncludes` stream into the record; output-path lookup
  (`hpcc explain foo.o`) waits on a second index.
- **Done — §5.1 observable gauges:** `internal/metrics/gauges.go`
  exposes `RegisterDaemonInflight` /
  `RegisterWorkerInflight` / `RegisterWorkerContainers` /
  `RegisterSchedulerWorkers`; each binary's main() hands in a
  snapshot callback right after `metrics.Init`. The callbacks read
  from `DefaultDaemon.Inflight()` (atomic counter bumped in
  `handleRequest`), `Worker.Inflight()` (existing `inflight`
  atomic exposed for the gauge), `PooledRuntime.EntriesByTenant()`
  (new snapshot of pool entries bucketed by tenant_id), and
  `Scheduler.RegisteredWorkers()` (sync.Map range). Cache-bytes
  gauges (local disk, rootfs) are still open — they need a cheap
  size-snapshot path on the store backends to avoid scanning on
  every scrape.

### 5.1 Stats & Metrics

**Status:** core wiring shipped (see "Progress so far" above).
`internal/metrics` boots the OTel metrics SDK with two readers —
an always-on Prometheus reader on scheduler and worker behind
their respective `metrics_listen` HTTP listeners, and an
optional OTLP push exporter on every binary gated by
`OTEL_EXPORTER_OTLP_ENDPOINT`. The daemon uses OTLP push only
(no scrape listener) because client machines aren't typical
Prometheus targets — see the "use OTEL please" thread on the
daemon question.

- `hpcc stats` — hit rate (local/remote/distributed), miss reasons, cache
  size, active VMs, compilation time saved. *Not yet wired to the
  metrics surface — `stats` still reads on-disk cache state.*
- Per-build summary printed at build end. *Open.*
- Observable gauges shipped for in-flight compiles (daemon +
  worker), active container pool entries by tenant (worker), and
  registered worker count (scheduler). Cache-bytes gauges (local
  disk, rootfs) are still open — they need a cheap size-snapshot
  path on the store backends to avoid scanning on every scrape.

### 5.2 Cache Inspection

- `hpcc inspect <hash>` — show metadata: what was compiled, when, which flags,
  what the inputs hashed to, where the result came from.
- `hpcc inspect <file>` — show what cache key would be computed for a file
  with the current flags.

### 5.3 Miss Reasons

**Status:** shipped (see "Progress so far" above). One JSON
record per source path under `$os.UserCacheDir/hpcc/explain/`,
written by the daemon on every compile attempt. The diff against
the prior record is computed at write time and embedded into the
new record, so `hpcc explain` is a pure read with no two-record
history requirement.

Categories surfaced today:
- `compiler` — compiler binary's identity changed (rebuilt,
  replaced, symlink moved)
- `flags` — the cache-key-relevant flag set changed (added /
  removed / reordered flags)
- `source` — the source file's bytes changed
- `header <path>` — a specific transitively-included header
  changed, was added, or was removed
- `image` — the OCI toolchain image digest the dispatcher pins
  changed

Still open:
- MSVC per-header attribution. gcc / clang dep emission lands in
  `.d` files the daemon already collects; MSVC writes the same
  data to stderr via `/showIncludes`, which the daemon doesn't
  capture today.
- Output-path lookup (`hpcc explain foo.o`). Source path only in
  the MVP — adding it needs a second index file mapping output →
  source.
- Worker-only compiles. Compiles that never traversed the daemon
  (e.g. CI calling the wrapper without a running daemon) leave
  no explain record. A worker-side explain feed merged into the
  daemon view is a follow-up.

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
source_mode = "cas"  # "cas" | "preprocessed"

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

### 5.5 Security Event Log

**Status:** structured-log half shipped (see "Progress so far"
above). Prometheus counters and the durable-sidecar sink remain
open.

The §4.12 audit trail is the *success* table — one row per
completed compile, reproducible by digest. The security event log
is its complement: one record per *rejected* or *anomalous*
interaction, where the question auditors ask is "who tried what
and why was it refused." Separate stream so an investigator can
read it without filtering through millions of green-path audit
rows.

Each event is a structured record with at least: `ts`, `actor`
(`tenant_id` if a verified JWT was attached, else the peer's
TLS-cert fingerprint and `remote_addr`), `component`
(`scheduler` / `worker` / `agent` / `daemon`), `kind` (one of
the categories below), `reason` (free-form, but stable enough to
group on), and `request_id` so a single client retry storm
collates into one investigation.

Categories worth logging:

- **Auth failures.**
  - OAuth token-exchange rejected at the IdP (`token_url`
    returned non-200) — scheduler.
  - JWT signature invalid, expired, or claims missing required
    fields (`tenant_id`, `image_digest`, `worker_id`) — both
    scheduler (incoming) and worker (route-token verification).
  - TLS cert fingerprint mismatch when a client dials a worker —
    client-side, but worth logging so the scheduler can correlate
    against routing decisions it made for that
    `(tenant_id, worker_id)`.
- **Authorization mismatches.** Verified JWT but the request
  doesn't line up: wrong `worker_id` for the worker that received
  it, image digest the worker doesn't have prepared,
  `tenant_id` not registered with the scheduler.
- **Wire-protocol violations.** Compile RPC missing the route
  token; `ExecHeader` not the first agent frame; `OutputFile`
  path failing the runner's path-traversal guard. These should
  not happen from a well-formed client; one occurrence is a
  client bug or a probe.
- **CAS abuse.** Worker BLAKE3 recompute disagrees with the
  client-claimed digest (cache-poison attempt — see
  [docs/plan/cas.md](cas.md) §"Trust"); `FindMissingBlobs` /
  `UploadBlobs` against a manifest the tenant isn't authorized
  for; missing or wrong-tenant `scheduler_token` on a CAS
  stream header (step 4 of [docs/plan/multi-tenant.md](multi-tenant.md)
  rejects today with `codes.Unauthenticated` but doesn't yet
  emit a record — that hook lands here); per-tenant upload quota
  tripped (see §5.7 below).
- **Image hardening events.** Tar-bomb caps tripped
  (`ErrTarTotalBytesExceeded` / `ErrTarEntryCountExceeded`);
  tar-path rejects (`..`, NUL, absolute-in-archive); hardlink
  target outside the image; OCI digest mismatch at pull time.
- **Sandbox health.** VM crash mid-job, agent stream errored
  before `ExecResult`, jailer cleanup left mountpoints behind.
  Lower-severity than the categories above (these are bugs or
  the kernel's fault, not adversarial), but the same row format
  so the same query surfaces them.

Surface:

- **Structured log** (shipped via `logging.Security` in
  `internal/logging`): zap-format entries at ERROR level —
  bumped from the original `warn`/`error` split — every event
  carries `category=security`, `severity=critical`,
  `event=<kebab-case identifier>` (e.g. `worker-token-invalid`,
  `agent-path-traversal`, `worker-manifest-digest-mismatch`),
  `rpc=<method>`, plus the tenant / worker / image / remote-addr
  context known at the call site. JWT-validation events attach
  the unverified claims under `jwt_claims_unverified` for
  forensics; raw token bytes are never logged. Field-name
  reconciliation against this section's original sketch:
  `category` collapses what the sketch called `component+kind`,
  `event` is the row's stable group-by key, the
  per-call identifiers stand in for `actor` / `request_id` until
  a real request-ID propagation lands.
- **Prometheus counters** labelled by `(component, kind,
  tenant_id)` so dashboards can alert on rate — **not yet
  shipped**. `tenant_id` is bounded cardinality in any realistic
  deployment; if it isn't, drop it from the label set and keep
  the structured-log version for forensics.
- **Durable sidecar** — **not yet shipped.** Same sink as the
  §4.12 audit trail — whichever durable target the operator
  wires up should receive both streams so an investigator doesn't
  have to join across systems.

### 5.6 Eviction

- LRU with max size (default 10GB) for local cache.
- Watermark-gated eviction for S3 cache (§3.5) — already implemented.
- LRU for converted rootfs blobs.
- `hpcc clean --max-size 5G`, `hpcc clean --max-age 30d`.
- Daemon runs periodic eviction in the background.

### 5.7 Per-tenant CAS upload quota

Carved out of phase 4 ([docs/plan/multi-tenant.md](multi-tenant.md)
*Per-tenant quota — deferred*) because it's a fairness property,
not a security one, and because its overrun event is a row in
the §5.5 security event log — so it lands here once that log
exists.

Two new optional fields on each `[[tenant]]` entry in the
scheduler config: `upload_bytes_per_window` (size, e.g.
`"5GB"`) and `upload_window` (duration, e.g. `"1h"`). Unset
means unlimited — single-tenant CI keeps working unchanged.

The scheduler distributes the tenants table back to workers via
the `HeartbeatResponse` (new `tenants` field on that message;
worker rebuilds its quota map on every heartbeat — cheap,
eventually consistent). The worker keeps an atomic per-tenant
token bucket and debits at `UploadBlobs` commit time. On
overrun it returns `codes.ResourceExhausted` with a
`retry-after` metadata header carrying seconds until the next
window reset. The daemon catches it, logs a one-shot yellow
notice keyed by `(tenant_id, window_start)`, and falls back to
local compile for the rest of the window — same client-side
fallback shape as the §4.11 worker-unreachable path. The
security event log gets a `cas-quota-tripped` record keyed by
`tenant_id`.

Open at land-time:

- **Window shape.** Fixed window (reset at top of every
  `upload_window` from worker startup) is dirt simple and bursty
  at boundaries. Sliding window (last N seconds) is fairer but
  costs a small ring buffer per tenant. Pick one explicitly when
  the work lands.
- **Worker-restart resets the bucket.** Acceptable since this is
  best-effort fair-sharing, not a billing meter. If real billing
  is ever wanted, push the meter to the scheduler.

### 5.8 Distributed Tracing (OpenTelemetry)

**Status:** worker compile path shipped (see "Progress so far"
above). Daemon, scheduler, and agent are follow-ups.

The §5.1 Prometheus surface tells you *that* something is slow;
distributed tracing tells you *which phase of which compile* is
slow — and propagates the trace across the client → scheduler →
worker → agent hops so a P95 regression isn't a guessing game
across four log files.

Wire: OTel SDK + OTLP/gRPC exporter, activated only when
`OTEL_EXPORTER_OTLP_ENDPOINT` (or the trace-specific variant) is
set. Without it, `internal/tracing.Init` returns a no-op shutdown
and the global tracer stays noop — instrumentation in the rest of
the codebase compiles and runs but emits nothing, so unconfigured
deployments don't need a collector. W3C `traceparent` + baggage
propagators are installed unconditionally so an incoming
`traceparent` header is honoured even when this process isn't
exporting.

gRPC servers install `otelgrpc.NewServerHandler()` as a
`StatsHandler`, which auto-creates a root span per inbound RPC
and extracts the upstream `traceparent`. Per-phase child spans
are opened by hand inside the RPC handler so trace UIs render
the actual hot phases (image pull vs cold runtime start vs
in-VM compile) rather than a single opaque RPC bar.

Phase coverage today:

- **Worker `Compile`** — `verify_manifest` (CAS only),
  `ensure_image`, `stage_source`, `runtime_start`, `cache_lookup`,
  `invoke`, `collect_extras`, `cache_store`. Attributes:
  `hpcc.tenant_id`, `hpcc.image_digest`, `hpcc.source_mode`,
  `hpcc.container_id`, `hpcc.vm.vcpus`, `hpcc.cache_hit`,
  `hpcc.exit_code`, `hpcc.duration_ms`, `hpcc.output_bytes`,
  `hpcc.extra_count`. Errors are recorded via
  `span.RecordError` + `Error` status so a failing compile shows
  the failing phase highlighted in the trace UI.

Follow-ups:

- **Worker CAS RPCs** (`ProbeCompileCache`, `FindMissingBlobs`,
  `UploadBlobs`) — root spans land via otelgrpc but per-blob /
  per-frame child spans are not wired yet. Worth a span per blob
  on the upload commit path so a slow CAS upload narrows to
  "which blob" instantly.
- **Scheduler RPCs** (`Authenticate`, `Route`, `RegisterWorker`,
  `Heartbeat`) — otelgrpc is one-line to add and ties scheduler
  routing decisions into the same trace as the compile they
  served.
- **Daemon → worker dispatch** — the daemon's
  [`dispatch.Dispatcher`](../../internal/daemon/dispatch/) opens a
  gRPC client connection but doesn't yet install
  `otelgrpc.NewClientHandler()`. Once it does, a client compile
  produces one connected trace across daemon → scheduler →
  worker (and on into the agent once §4.4.1 picks up the
  propagator). Until then the worker spans stand alone.
- **Agent in-VM `Exec`** — the host-side runtime calls
  `Container.Exec`; threading the trace context into the agent
  requires plumbing a `traceparent` field through
  `proto/agent/agent.proto`'s `ExecHeader`. Cheap on the wire,
  defer until daemon-side propagation lands.
- **Sampling.** Default is `AlwaysSample` (i.e. whatever the SDK
  defaults to). Production with high RPS will want
  `ParentBased(TraceIDRatioBased(0.01))` or similar — wire via
  the standard `OTEL_TRACES_SAMPLER` env var when this becomes a
  problem; no code changes needed.
