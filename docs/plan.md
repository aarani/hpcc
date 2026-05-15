# hpcc — Implementation Plan

hpcc is a distributed compilation and caching tool. A drop-in compiler wrapper
that caches build artifacts locally and remotely, and optionally distributes
compilation across a cluster of workers — with a strong tenant-isolation story
suitable for environments (banks, regulated enterprises) where running
user-supplied compilation on shared hardware is politically untenable.

This document is the index. Each phase, plus the cross-cutting structure
and design-decisions sections, lives in its own file under `plan/`. What's
done vs. what's open per phase lives in each phase doc — `git log` is the
authoritative chronology.

---

## Status

| Phase | Description | Status |
|-------|-------------|--------|
| [Phase 1](plan/phase-1-compiler-wrapping.md) | Core Compiler Wrapping | Done |
| [Phase 2](plan/phase-2-daemon.md) | Daemon Architecture | Done |
| [Phase 3](plan/phase-3-remote-cache.md) | Remote Cache | Done |
| [Phase 4](plan/phase-4-distributed.md) | Distributed Compilation in Per-Tenant VMs | In progress |
| [Phase 5](plan/phase-5-observability.md) | Observability & Polish | Not started |

---

## Contents

- [Phase 1: Core Compiler Wrapping](plan/phase-1-compiler-wrapping.md) —
  flag parsing, input hashing, local disk cache, drop-in wrapper.
- [Phase 2: Daemon Architecture](plan/phase-2-daemon.md) — loopback-TCP
  daemon, client-server protocol, dedup, fallback.
- [Phase 3: Remote Cache](plan/phase-3-remote-cache.md) — S3 store, lookup
  order, failure handling, eviction, bucket provisioning.
- [Phase 4: Distributed Compilation in Per-Tenant VMs](plan/phase-4-distributed.md)
  — raw Firecracker, runtime abstraction, VM lifecycle, image-as-environment,
  agent wire schema, scheduler, worker, wire protocol, audit, paranoid mode,
  rootfs hardening.
- [Phase 5: Observability & Polish](plan/phase-5-observability.md) — stats,
  cache inspection, miss reasons, configuration, eviction.
- [Project Structure](plan/project-structure.md) — repo layout.
- [Design Decisions](plan/design-decisions.md) — hashing, storage, sandbox
  model, wire protocols, security.

Related sub-plans:

- [CAS-mode source dispatch](cas.md) — the §4.5 content-addressed
  source-staging path. Shipped in v1 as an opt-in `source_mode = "cas"`
  alongside the default PREPROCESSED. Probe-then-upload, `.hpcc`
  project marker for cross-developer hits, paranoid-mode-friendly
  trust boundary, `.S`/`.incbin` support.
