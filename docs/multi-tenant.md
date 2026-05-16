# Multi-Tenant Isolation — Design Sub-Plan

Re-opens §4.5 / §4.8 / §4.13 of
[plan/phase-4-distributed.md](plan/phase-4-distributed.md) and the
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
- Hostile tenant A wants to forge a JWT claiming
  `tenant_id = B`.
- Compromised A laptop under paranoid mode (§4.13) cannot escape
  A's namespace even with worker-side credentials in play.
- The scheduler operator is trusted. An IdP operator is trusted
  **only for the tenants whose `issuer` claim is that IdP** —
  IdP-A cannot mint credentials valid for tenant B.

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
upload_bytes_per_window = "5GB"
upload_window           = "1h"

[[tenant]]
id        = "globex"
issuer    = "https://keycloak.globex.io/realms/eng"
jwks_url  = "https://keycloak.globex.io/realms/eng/protocol/openid-connect/certs"
token_url = "https://keycloak.globex.io/realms/eng/protocol/openid-connect/token"
audience  = "hpcc"

[[tenant]]
id        = "default"   # catches pre-migration data, see Migration
issuer    = "..."
# ... etc
```

JWT validation on every `Route` call:

1. Read `tenant_id` claim from the JWT.
2. Look up `tenants[tenant_id]`. Absent → reject (single opaque
   error; no distinguishing "no such tenant" from anything else,
   to keep tenant_id enumeration noisy).
3. Validate signature against `tenant.jwks_url` (cached, refreshed
   on a clock or on signature failure).
4. Confirm `iss == tenant.issuer` and `aud` contains
   `tenant.audience`.

**Cross-IdP spoof prevention falls out by construction.** An
attacker at IdP-A can mint JWTs signed by A, but those will
never verify against B's JWKS, so any JWT claiming
`tenant_id = B` and signed by A is rejected at step 3. The
scheduler never holds tenant IdP secrets — only public keys.

## Identity discovery (client side)

Client config carries only `tenant_id` + scheduler URL. The
daemon's first action of a session is an unauthenticated
discovery call:

```proto
service Scheduler {
  rpc GetTenantIdP(GetTenantIdPRequest) returns (GetTenantIdPResponse);
}
message GetTenantIdPRequest  { string tenant_id = 1; }
message GetTenantIdPResponse {
  string issuer    = 1;
  string token_url = 2;
  string audience  = 3;
}
```

The daemon then runs the existing password-grant flow against the
returned `token_url`, and proceeds to `Route` with the resulting
JWT.

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
| S3 compile cache | `cache/<digest>` | `cache/<tenant_id>/<digest>` |
| Worker CAS source | `cas/<digest>` | `cas/<tenant_id>/<digest>` |
| Probe / ActionCache | `probe/<manifest_digest>` | `probe/<tenant_id>/<manifest_digest>` |
| Local daemon cache | `<digest>` | `<tenant_id>/<digest>` |
| Prepared rootfs | per `image_digest` | unchanged (toolchain bytes, no tenant data) |
| Audit row | `tenant_id`-tagged column | unchanged |

The probe-prefix change is the §4.5 cross-tenant disclosure fix
promoted from optional to mandatory. The Bazel-shaped property
hpcc inherits is closed: tenant A probing with tenant B's
manifest digest hits a different key and misses cleanly.

## Worker enforcement

Every RPC the worker exposes (`Compile`, `ProbeCompileCache`,
`FindMissingBlobs`, `UploadBlobs`) verifies:

1. JWT signature + claims via the per-tenant JWKS cache, which
   the worker hydrates at `RegisterWorker` from the scheduler.
2. `route_token.tenant_id == JWT.tenant_id == request.tenant_id`.
3. Every cache key, blob digest, manifest digest the request
   references resolves under `tenants[tenant_id]`'s namespace.

Any mismatch = reject + security-log event (§5.5). The container
pool keying is already per-tenant — no change there.

## Per-tenant quota (bundled)

Token bucket on bytes/window keyed by `tenant_id`, enforced at
the worker on `UploadBlobs`. Two parameters live in the tenants
table (`upload_bytes_per_window`, `upload_window`) and are pushed
to workers in the scheduler's tenants distribution
(heartbeat-piggybacked, no separate RPC). On overrun:

- Worker rejects the `UploadBlobs` call with a typed
  `QuotaExceeded` gRPC status carrying retry-after.
- Daemon catches the status, logs a one-shot yellow notice, and
  falls back to local compile for the remainder of the window —
  same client-side fallback shape as the §4.11 worker-unreachable
  path.
- Security event log (§5.5) gets a `cas-quota-tripped` record
  keyed by `tenant_id`.

A tenant entry with no quota fields = unlimited (single-tenant CI
keeps working). Quotas can be edited live via SIGHUP.

## Migration: hard cutover with a `"default"` tenant

Existing single-tenant deployments need a story. We pick the
clean one:

- Ship a new subcommand: `hpcc migrate-tenants --to default`.
  Renames every `cache/<digest>` → `cache/default/<digest>`,
  `cas/<digest>` → `cas/default/<digest>`, etc. Idempotent;
  logs entries renamed vs. entries already in shape.
- Existing daemons that don't yet pass `tenant_id` resolve to
  `"default"` server-side for one release, then are rejected.
  Documented as a deprecation.
- Stores write only to the new namespaced paths in steady state.
  No fallback reader carrying dead code.

## Phasing

Single feature branch, but the commits stack so each step is
reviewable on its own:

1. **Tenants table + JWT validation in scheduler.** Plumbing
   only. Default tenant catches existing JWTs. No storage
   changes yet — both stores read/write the old flat paths, but
   the JWT path becomes per-tenant-IdP-aware.
2. **`GetTenantIdP` discovery RPC.** Scheduler side first; then
   wire the client to call it before the OAuth exchange.
3. **Storage namespace prefix across all four stores.**
   Mechanical, biggest diff. `hpcc migrate-tenants` ships in the
   same commit. Stores stop reading the flat path.
4. **Worker per-RPC tenant enforcement.** Closes the cross-tenant
   read path on the worker side. JWKS hydration from scheduler at
   registration; cache key/blob digest scoping in every handler.
5. **Per-tenant quota.** Token bucket on `UploadBlobs`,
   distribution via heartbeat, client fallback. Updates the
   README Limitations list (drops the quota bullet *and* the
   probe-disclosure bullet).
6. **Docs.** Phase 4 §4.5 / §4.8 / §4.13 reference this file;
   README's "OAuth IdP integration" pitch is rewritten as
   "per-tenant IdP" with the same length budget.

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
