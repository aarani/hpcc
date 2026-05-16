# VM Density — Design Sub-Plan

Re-opens the "VMs are sized for many cores but absorb one compile
at a time" gap in §4.4 of
[plan/phase-4-distributed.md](plan/phase-4-distributed.md), and the
aspirational-but-wrong comment in [worker.go](../internal/worker/worker.go)
that claims "A single VM can absorb VM.VCPUs concurrent Task.Execs."
Today it can't — the pool pops the container on Start and re-parks
it on Stop, so the second concurrent compile for the same
`(tenant, image)` finds the slot empty and boots a fresh VM. The
fix is small, orthogonal to multi-tenant, and lives here so the
two stacks don't entangle.

## Why now

1. **Bursty wide builds are the common case.** N parallel
   compiles for the same toolchain image come in waves (the
   kernel build can fire 32+ TUs in seconds). N concurrent
   `Compile` RPCs today → N live VMs, each holding
   `VM.Memory` regardless of whether the in-VM gcc is using more
   than one core. RSS scales linearly with concurrency instead
   of with vCPU count.
2. **The pieces already exist.** The agent's `Exec` is a per-
   call streaming RPC with a unique `exec_id`; staging dirs
   land at `/run/hpcc/src/<exec_id>/` and
   `/run/hpcc/out/<exec_id>/` by design (see
   [agent.proto](../proto/agent/agent.proto)). The host runtime
   already passes a per-`ExecRequest` `ExecID` through
   ([firecracker.go:349](../internal/worker/runtime/firecracker.go)).
   The only thing serializing compiles is `PooledRuntime.Start`
   removing the container from the pool, forcing one Exec at a
   time per VM.
3. **Compiler concurrency floor is one core.** A single TU
   compile is mostly single-threaded for `gcc`/`clang`. Sizing
   VMs to `VM.VCPUs = 1` would waste the rest of the host;
   sizing them to N and never running N concurrent Execs in one
   VM wastes them in the guest. Multi-Exec-per-VM is what makes
   `VM.VCPUs > 1` actually useful.

## Threat / correctness model

- Two Execs sharing one VM must not see each other's source or
  output. Already covered by the agent's per-`exec_id` staging
  paths — concurrent Execs land in disjoint subtrees by
  construction. No host-side mount sharing.
- Cancelling Exec A (client disconnect, scheduler timeout) must
  not affect Exec B in the same VM. The agent receives a
  per-Exec stream; aborting the stream kills only A's process
  group inside the guest.
- A crashing or hung Exec must not pin the VM forever. The
  existing `maxLifetime` (hard session timeout) still applies;
  it now triggers only when the container's refcount has been
  zero past the deadline, not when the *first* compile crossed
  it.
- A buggy compile that fills `/tmp` inside the VM degrades
  every concurrent Exec sharing that VM. Acceptable for v1 —
  guest disk pressure is already a per-VM concern even at
  refcount = 1, and the size cap is on rootfs not on RAM.

## Design

The change is one runtime layer and a small worker-side
concurrency cap. Three pieces:

### 1. Pool: refcount, not pop

[`PooledRuntime.Start`](../internal/worker/runtime/pool.go) is
rewritten from "pop entry, return wrapper, park on Stop" to
"acquire entry (refcount++), return wrapper, release on Stop
(refcount--)." A `pooledEntry` gains:

```go
type pooledEntry struct {
    container Container
    createdAt time.Time
    refs      int        // active Execs currently using this container
    maxRefs   int        // VM.VCPUs — the per-VM concurrency cap
    lastFreeT time.Time  // last moment refs hit 0; idle reaper anchor
}
```

`Start` looks for an entry under the `(tenant, image)` key with
`refs < maxRefs`. Hit → `refs++`, return wrapper. Miss → start
a fresh container, register it with `refs = 1, maxRefs =
VM.VCPUs`, return wrapper. The entry stays in the pool the
whole time — there's no pop. The reaper evicts entries with
`refs == 0` past `idleTTL` (or any refs past `maxLifetime`).
Same lifetime contract as today; refcount is just the new
gating signal for "is this container still in use."

`Close` drains as before, but waits for `refs == 0` on every
entry (or kills outstanding Execs and tears down — depending
on the close mode the worker picks).

### 2. Worker concurrency cap stays where it is

`Worker.inflight` already caps **worker-wide** concurrent
compiles at `Pool.MaxActive * VM.VCPUs` worth of slots. With
multi-Exec VMs this cap matches reality for the first time —
today it's an upper bound the worker rarely reaches because
each Compile takes a whole VM. No worker change beyond the
refcount semantics propagating through.

### 3. Host-side staging stays per-Compile

The host's temporary `srcDir` / `outDir` (created in
[staging.go](../internal/worker/staging.go)) remain per-Compile.
They're host-side scratch the worker assembles bytes into
before streaming them to the agent via the per-Exec
`exec_id`-scoped channel. Concurrent Compiles get unique
tempdirs from `os.MkdirTemp` already; nothing collides because
those bytes are streamed and never mounted into the VM. The
in-VM destination paths (`/run/hpcc/src/<exec_id>/…`) are
disjoint by construction.

The only worker-side wiring change is dropping any lingering
assumption that "container ID equals exec ID" — they were
already separate fields on `ExecRequest`, but spot-check the
audit-row population, the staging path printer, and any test
helpers that conflated them.

## Phasing

Single feature branch, two reviewable commits:

1. **Refcounted pool.** `PooledRuntime.Start/Stop` rewritten,
   `pooledEntry` gains `refs`/`maxRefs`/`lastFreeT`, reaper
   updated to gate on `refs == 0`. New tests:
   - Two concurrent `Start` calls for the same key get the same
     container (refcount == 2).
   - Third concurrent call hits `maxRefs` → starts a fresh
     container.
   - Reaper does not evict an entry with `refs > 0` regardless
     of idle/lifetime deadlines past.
   - `Close` blocks until all outstanding Execs release.
2. **Wire `maxRefs = VM.VCPUs` and update audit row formatting**
   so concurrent Execs in one VM emit distinct audit rows that
   share the VM ID but carry different cache keys. The audit row
   already keys on `vm_id` — no schema change, just confirm the
   join is still sensible when N > 1 rows reference the same
   `vm_id`.

## Open follow-ups

- **Per-VM disk pressure budget.** The guest's writable layer is
  capped by rootfs sizing today; under multi-Exec the cap is
  shared by all concurrent compiles. If pathological TUs surface
  (huge `-save-temps`, massive `.d` outputs), we add a per-Exec
  tmpfs quota in the agent. Not v1.
- **VCPU pinning.** Today the guest scheduler decides which
  vCPU each in-VM process runs on. We could pin each Exec to a
  specific vCPU for cleaner CPU accounting, but the cgroup
  controls inside the guest are limited (depends on kernel
  options) and the win is marginal for cache-friendliness.
  Deferred.
- **Multi-tenant safety.** The pool key is already
  `(tenant, image)` — a VM is never shared across tenants, so
  multi-Exec stays within one tenant's namespace. No
  interaction with [multi-tenant.md](multi-tenant.md). If a
  future deployment wants stricter "one VM per
  `(tenant, image)` regardless of capacity," set
  `Pool.MaxRefs = 1` (or omit `VM.VCPUs > 1`) and the pool
  collapses back to today's pop/park behaviour.
- **Audit-row keying.** §4.12's audit row keys on `vm_id`.
  Under multi-Exec the same `vm_id` will recur across compiles.
  Either rename to `(vm_id, exec_id)` for the primary key, or
  drop the pretense and key on `(cache_key, tenant_id, ts)` —
  decide when the audit-row work lands.

## Out of scope

- **Cross-tenant VM sharing.** Stays forbidden by pool key,
  always. The win from this change is intra-tenant; cross-tenant
  is a security boundary, not a fairness lever.
- **Live VM migration / hot rebuild.** Not on the table —
  containers are still cattle, just less promiscuously
  replaced.
- **Pool size autoscaling.** `MaxActive` remains operator-set.
  Auto-tuning to host RAM/CPU is plausibly next but orthogonal.
