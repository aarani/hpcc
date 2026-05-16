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
  [docs/cas.md](../cas.md) §"Trust"); `FindMissingBlobs` /
  `UploadBlobs` against a manifest the tenant isn't authorized
  for; per-tenant upload quota tripped (once §4.5 wires it).
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

- **Structured log** to the same writer the rest of the
  component uses, with `level = "warn"` or `"error"` depending
  on severity. Auth failures and CAS abuse default to `warn` (a
  single occurrence is a misconfigured client; a flood is
  something else); wire violations and tar-bomb caps default to
  `error` (no client should ever produce one).
- **Prometheus counters** labelled by `(component, kind,
  tenant_id)` so dashboards can alert on rate. `tenant_id` is
  bounded cardinality in any realistic deployment; if it isn't,
  drop it from the label set and keep the structured-log version
  for forensics.
- **Durable sidecar.** Same sink as the §4.12 audit trail —
  whichever durable target the operator wires up should receive
  both streams so an investigator doesn't have to join across
  systems.

### 5.6 Eviction

- LRU with max size (default 10GB) for local cache.
- Watermark-gated eviction for S3 cache (§3.5) — already implemented.
- LRU for converted rootfs blobs and VM snapshots.
- `hpcc clean --max-size 5G`, `hpcc clean --max-age 30d`.
- Daemon runs periodic eviction in the background.
