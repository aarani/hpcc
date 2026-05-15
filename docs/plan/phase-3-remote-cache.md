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
