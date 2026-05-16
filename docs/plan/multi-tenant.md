# Multi-Tenant Isolation — Design Sub-Plan

Re-opens §4.5 / §4.8 / §4.13 of
[phase-4-distributed.md](phase-4-distributed.md) and the
"CAS probe is cross-tenant disclosive" Limitations bullet in the
README. The phase-4 doc treats `tenant_id` as a JWT-borne label
threaded through routing, the container pool, and the audit row;
this file promotes it from a label to a **namespace boundary** —
storage, identity, and quotas all scope by `tenant_id`, and the
cross-tenant read path that exists today closes by construction.

## Why now

1. **Cross-tenant probe disclosure is real.** §4.5 ships with the
   acknowledged property that any tenant who guesses another
   tenant's manifest digest can fetch their compile output via
   `ProbeCompileCache`. Same property Bazel has, but the regulated
   deployments hpcc is built for can't ship with it.
2. **One scheduler IdP doesn't scale to multi-org.** The v1 OAuth
   story handles "your company's IdP" cleanly, but assumes one
   IdP per scheduler. A scheduler serving acme-corp and globex
   today would have to pick one IdP for both, which is exactly
   the deployment shape the per-tenant pitch needs to support.
3. **Per-tenant CAS upload quota is on the Limitations list.**
   Multi-tenant storage isolation and per-tenant quotas share
   most of their wiring (tenants table, distributed to workers,
   keyed by `tenant_id`); bundling them is cheaper than two
   passes.

## Threat model

- Hostile tenant A wants to read B's compile outputs, source
  blobs, probe results, or audit rows.
- Hostile tenant A wants to present an A-issued JWT and have the
  scheduler accept it as tenant B.
- Compromised A laptop under paranoid mode (§4.13) cannot escape
  A's namespace even with worker-side credentials in play.
- The scheduler operator is trusted. An IdP operator is trusted
  **only for the tenant whose `[[tenant]]` entry names it** —
  the scheduler never delegates validation of tenant B's tokens
  to tenant A's IdP, regardless of what claims A's tokens carry.

## Identity: per-tenant IdP

Client-side OAuth already supports any password-grant IdP — the
v1 story doesn't change. The new piece is making the scheduler
validate against the **right** IdP per tenant.

Scheduler holds a static tenants TOML loaded at startup
(reload on SIGHUP; admin RPC is an open follow-up):

```toml
[[tenant]]
id        = "acme-corp"
issuer    = "https://okta.acme.com"
jwks_url  = "https://okta.acme.com/oauth2/v1/keys"
token_url = "https://okta.acme.com/oauth2/v1/token"
audience  = "hpcc"
client_id = "hpcc-acme"     # OAuth client registered with the IdP
scope     = "hpcc"
upload_bytes_per_window = "5GB"
upload_window           = "1h"

[[tenant]]
id        = "globex"
issuer    = "https://keycloak.globex.io/realms/eng"
jwks_url  = "https://keycloak.globex.io/realms/eng/protocol/openid-connect/certs"
token_url = "https://keycloak.globex.io/realms/eng/protocol/openid-connect/token"
audience  = "hpcc"
client_id = "hpcc-globex"
scope     = "hpcc"
```

At least one `[[tenant]]` entry is required; the legacy single
`[auth.jwks]` block is gone (see *Pre-alpha break* below).

`tenant_id` rides on the `AuthRequest` message, not as a JWT
claim. The client is saying *"I'm from tenant A, here's my
token"*; the scheduler picks A's IdP from the tenants table and
asks **only that one** to verify the signature. The JWT itself
need not (and does not) carry a tenant claim, since it's already
implicitly scoped by the tenant whose IdP issued it.

JWT validation on every `Authenticate` call:

1. Read `tenant_id` from `AuthRequest`. Empty → reject.
2. Look up `tenants[tenant_id]`. Absent → reject (single opaque
   error; no distinguishing "no such tenant" from anything else,
   to keep tenant_id enumeration noisy).
3. Validate signature against `tenant.jwks_url` (cached, refreshed
   on a clock or on signature failure).
4. Confirm `iss == tenant.issuer` and `aud` contains
   `tenant.audience`.
5. On success, the scheduler-issued session token is bound to
   `tenant_id`. `Route` checks `request.tenant_id ==
   session.tenant_id` — an A session cannot route as B even with
   a swapped `tenant_id` field.

**Cross-IdP spoof prevention falls out by construction.** An
attacker holding an A-issued JWT can label its `AuthRequest` as
`tenant_id = B`, but only B's JWKS is consulted for tenant B and
it won't verify A's signature. The scheduler never asks A's IdP
about B's tokens — even reading A's JWKS in service of a tenant-B
request would be a trust-model violation.

## Identity discovery (client side)

Client config carries only `tenant_id` + scheduler URL and
the per-user credentials (`username`/`password`/`client_secret`).
**No `token_url`, `client_id`, or `scope`** — those are
tenant-level settings the scheduler hands back on demand:

```proto
service SchedulerService {
  rpc GetTenantIdP(GetTenantIdPRequest) returns (GetTenantIdPResponse);
}
message GetTenantIdPRequest  { string tenant_id = 1; }
message GetTenantIdPResponse {
  string issuer    = 1;
  string token_url = 2;
  string audience  = 3;
  string client_id = 4;
  string scope     = 5;
}
```

The daemon runs the password-grant flow against the returned
`token_url` (sending `client_id`/`scope` when non-empty), then
calls `Authenticate(tenant_id, jwt)` with the same `tenant_id`
it just discovered for. Letting the scheduler be authoritative
for everything except per-user credentials means ops can rotate
or relocate a tenant's IdP — or change the OAuth client/scope —
by editing scheduler config only. Laptops don't need to be
re-flashed.

Information disclosure: an attacker can enumerate `tenant_id`s
to discover which are configured. IdP URLs themselves aren't
secret in practice — `iss` is publicly visible in any issued JWT.
Acceptable.

## Storage isolation

Tenant becomes a **namespace prefix**, not part of the content
hash. Mixing into the hash would break intra-tenant dedup (two
of the same tenant's builds wouldn't share results); prefixing
the storage path gives full isolation while keeping
content-addressed dedup *within* a tenant.

| Store | Pre | Post |
|---|---|---|
| S3 compile cache | `cache/compile/<keyhash>/<blob>` | `cache/compile/<tenant_id>/<keyhash>/<blob>` |
| Worker CAS source | `cache/source/<digest>/<blob>` | `cache/source/<tenant_id>/<digest>/<blob>` |
| Probe / ActionCache | rides on compile cache | inherits compile-cache prefix |
| Local daemon cache | `compile/<keyhash>/<blob>` | `compile/<tenant_id>/<keyhash>/<blob>` |
| Prepared rootfs | per `image_digest` | unchanged (toolchain bytes, no tenant data) |
| Audit row | `tenant_id`-tagged column | unchanged |

The probe RPC and the Compile RPC share one cache key (manifest
digest mixes into the same `CacheKey` derivation), so a single
namespace prefix on the compile cache closes the probe path too.

**Wiring shape.** The low-level `store.Store` (disk + S3) stays
tenant-blind — it remains a content-addressed K/V keyed by
opaque byte keys. The tenant prefix is composed via the existing
`Namespace(prefix)` mechanism: every call site that owns a
tenant scope (worker handlers, daemon cache loop, runner) takes
`tenantID` as an explicit parameter and calls
`store.Namespace(tenantID)` before issuing the K/V op. The
runner-only local fast path passes the sentinel
`cache.TenantLocal` (= `"local"`) since a single developer's
machine has no namespace neighbours to isolate from; keeping
the layout uniform avoids "no-tenant" special cases in tooling
that walks the cache tree.

`CompileCache.Lookup(inv, tenantID)` and `Store(inv, res,
tenantID)` reject an empty `tenantID` — a caller that forgot to
pass one is a programming error, not a fallback condition. The
worker's CAS handlers (`FindMissingBlobs`, `UploadBlobs`,
`materializeCASBlobs`) read the tenant off each `BlobDigest`
header.

## Worker enforcement

The worker is the public-network surface: clients dial it
directly post-Route. Four RPCs need authentication.

**Compile / ProbeCompileCache.** Already covered by
`validateSchedulerToken`: the request descriptor carries a
`scheduler_token` (an ed25519-signed JWT minted by the scheduler
at `Route` time, 5-minute TTL); the worker verifies the
signature against the pubkey it received at `RegisterWorker` and
confirms `claims.tenant_id == request.tenant_id`,
`claims.image_digest == request.image_digest`, and
`claims.worker_id == w.workerID`. Any mismatch → reject.

**FindMissingBlobs / UploadBlobs.** Step 4 closed a real gap
here: these streams had no auth at all. Every `BlobDigest`
header on both streams now carries a `scheduler_token` field;
the worker validates it via the same scheduler-pubkey path used
by Compile, with `claims.tenant_id == header.tenant_id` and
`claims.worker_id == w.workerID`. The `image_digest` claim is
informational on these streams — source blobs are
content-addressed by tenant, not by image — so it's parsed but
not compared. A header missing the token, or carrying a token
whose `tenant_id` doesn't match the header's, is rejected with
`codes.Unauthenticated`.

**Stream tenant pinning.** Each stream is pinned to the first
header's `tenant_id`; a later header with a different tenant is
a protocol violation and the stream is closed with
`codes.InvalidArgument`. One client session belongs to one
tenant, and the strict read avoids partial-success states where
some blobs in the same stream landed under A and others under B.

**Why no per-tenant IdP JWKS hydration on the worker.** The
earlier draft of this section had the worker holding a JWKS
cache and re-verifying the user's IdP JWT on every RPC. We don't
need that: the scheduler already validates the user JWT against
the right per-tenant IdP (step 1) before issuing the
scheduler-signed task token. The trust chain
*client → IdP JWT → scheduler → scheduler-signed task token → worker*
is strictly tighter than asking the worker to second-guess the
scheduler against N JWKS endpoints it would have to keep warm.
The worker trusts the scheduler's signing pubkey; the scheduler
is responsible for who's a real tenant. One ed25519 verify per
header instead of an HTTPS JWKS roundtrip + signature.

**Security-event logging** of these rejections lands when phase
5's `SecurityEvent` log infrastructure exists; today the worker
returns the gRPC status code and that's it.

The container pool keying is already per-tenant — no change
there.

## Per-tenant quota — deferred

Per-tenant CAS upload quotas are a fairness property, not a
security one. The Step-4 enforcement above prevents *cross*-tenant
access; what's left is one tenant fairly sharing the worker's
local source-store budget with other tenants. That work is
phased into [phase-5-observability.md](phase-5-observability.md)
since it lands alongside the security-event log it generates a
record into. Sketch retained there: token bucket keyed by
`tenant_id`, two new fields on the `[[tenant]]` table
(`upload_bytes_per_window`, `upload_window`), scheduler pushes
the table back to workers via `HeartbeatResponse`, worker
returns `ResourceExhausted` with retry-after on overrun, daemon
catches and falls back to local compile for the rest of the
window.

## Pre-alpha break

hpcc is pre-alpha, so there is no migration path. The transition
is a hard break in two places:

- **Config.** `[auth.jwks]` is removed; the scheduler requires at
  least one `[[tenant]]` entry. A scheduler started against an
  old config fails validation with a pointer to this doc.
- **Storage.** Existing flat `cache/<digest>`, `cas/<digest>`,
  `probe/<manifest_digest>` entries are orphaned by the namespace
  prefix in step 3. Operators wipe the buckets and the local
  daemon cache; everything refills on next build. No
  `migrate-tenants` subcommand, no dual-read fallback, no dead
  code carrying the old shape.

Both breaks land in the same release; the changelog says "wipe
caches, rewrite scheduler config" and that is the entire upgrade
note.

## Phasing

Single feature branch, but the commits stack so each step is
reviewable on its own:

1. **Tenants table + JWT validation in scheduler.** Plumbing
   only. The legacy `[auth.jwks]` block is removed and replaced
   by `[[tenant]]` entries; the scheduler refuses to start
   without at least one. No storage changes yet — stores still
   read/write the old flat paths, but the JWT path becomes
   per-tenant-IdP-aware.
2. **`GetTenantIdP` discovery RPC.** Scheduler side first; then
   wire the client to call it before the OAuth exchange.
3. **Storage namespace prefix across all four stores.**
   Mechanical, biggest diff. `CompileCache.Lookup/Store` and
   worker CAS handlers take `tenantID` explicitly; `BlobDigest`
   gains a `tenant_id` field so streaming RPCs carry tenant per
   blob header. Existing flat entries are orphaned (see
   *Pre-alpha break*); stores read and write only the new
   namespaced path.
4. **Worker per-RPC tenant enforcement.** Closes the cross-tenant
   read path on the worker side. `BlobDigest` gains a
   `scheduler_token` field; FindMissingBlobs and UploadBlobs
   validate the token via the scheduler pubkey already hydrated
   at `RegisterWorker`, and each stream is pinned to its first
   header's tenant. No per-tenant IdP JWKS on the worker — the
   scheduler-signed task token already encodes the tenant the
   scheduler authorized.
5. **Docs.** Phase 4 §4.5 / §4.8 / §4.13 reference this file;
   README's "OAuth IdP integration" pitch is rewritten as
   "per-tenant IdP" with the same length budget. README's
   Limitations list drops the probe-disclosure bullet (closed by
   step 3) and the quota bullet stays until phase 5 lands the
   quota work.

Per-tenant CAS upload quota is deferred to phase 5 (see
*Per-tenant quota — deferred* above).

## Out of scope (open follow-ups)

- **Dynamic tenant admin RPC.** Static TOML at startup is the v1
  contract; live tenant add/remove via authenticated admin RPC
  is on the list once someone asks.
- **Dedicated worker affinity.** Tenants share workers by
  default; a "pin worker fleet X to tenant Y" knob is plausibly
  wanted in deeply paranoid deployments and slots in cleanly once
  the tenants table exists.
- **Per-tenant audit log encryption.** Audit rows stay
  plaintext-with-tenant-tag for v1. Encrypting per-tenant with a
  tenant-supplied key is a credible future ask but adds key
  management surface this doc isn't trying to solve.
- **Allowed-image policy.** A tenant entry could carry a list of
  permitted `image_digest`s, with `Route` rejecting anything
  off-list. Deferred — easy to add later, no shape change to the
  tenants table.
