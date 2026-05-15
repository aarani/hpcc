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

### 5.5 Eviction

- LRU with max size (default 10GB) for local cache.
- Watermark-gated eviction for S3 cache (§3.5) — already implemented.
- LRU for converted rootfs blobs and VM snapshots.
- `hpcc clean --max-size 5G`, `hpcc clean --max-age 30d`.
- Daemon runs periodic eviction in the background.
