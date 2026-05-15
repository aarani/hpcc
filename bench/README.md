# Kernel-build benchmarks

Benchmark pipelines that compile a real Linux kernel through hpcc and
report cache hit rate, cold-vs-warm wall time, and entry counts. They
cover every shipped dispatch path:

| Script                     | Mode                              | hpcc surface exercised                                       |
|----------------------------|-----------------------------------|--------------------------------------------------------------|
| `kernel-bench.sh`          | local                             | `hpcc wrap` → in-process compile → disk cache                |
| `kernel-bench-fc.sh`       | firecracker (P4), source mode picked by `HPCC_BENCH_SOURCE_MODE` | `hpcc wrap` → dispatch → scheduler → worker → FC VM |
| `kernel-bench-fc-both.sh`  | firecracker, runs once per source mode | wraps `kernel-bench-fc.sh`; prints side-by-side summary |

The FC bench defaults to `source_mode = "preprocessed"`; pass
`HPCC_BENCH_SOURCE_MODE=cas` (or use the `-both.sh` wrapper) to
exercise the CAS dispatch path. CAS mode also drops a `.hpcc` marker
at the kernel root so manifest paths normalize project-relative — the
gate for cross-worker / cross-developer compile-result hits.

Each script:

1. Builds the `hpcc` binary from source.
2. Materializes the right config (a single disk cache for local
   mode; a `remote = enabled` TOML pointing at the in-process Phase 4
   stack for FC mode).
3. Shallow-clones a pinned Linux tag (`v7.0` by default) and runs the
   chosen kconfig target.
4. Builds the kernel once cold, once warm (`make clean` between, so
   only object files vanish — `.config` and the prepared sources
   stay).
5. Writes a per-run `report.md` + `stats.json` under
   `bench/results/<timestamp>-<mode>/` plus the raw build logs.
6. Fails non-zero if the hit-rate / warm-vs-cold threshold isn't met.

## Running locally

### Local mode

```sh
./bench/kernel-bench.sh
```

Linux-only (needs to build `vmlinux`). Reasonable on any host with
`gcc`, `make`, `git`, `awk`, and `go` on PATH. Defaults to
`make defconfig` and uses `nproc` jobs.

Knobs (env vars, all optional):

| Var                              | Default       | Meaning |
|----------------------------------|---------------|---------|
| `HPCC_BENCH_KERNEL_TAG`          | `v7.0`        | Git tag in torvalds/linux to clone |
| `HPCC_BENCH_JOBS`                | `nproc`       | `make -j` parallelism |
| `HPCC_BENCH_CONFIG`              | `defconfig`   | kconfig target |
| `HPCC_BENCH_TARGET`              | `vmlinux`     | Top-level `make` target |
| `HPCC_BENCH_MIN_HIT_RATE`        | `95`          | Required cache hit rate (%) |
| `HPCC_BENCH_MAX_WARM_PCT`        | `50`          | Max acceptable median warm wall time as % of cold |
| `HPCC_BENCH_WARM_RUNS`           | `3`           | Warm-build repeats after the cold pass (median is the headline number) |
| `HPCC_BENCH_KEEP`                | `0`           | `1` preserves the work dir for inspection |

### Firecracker mode

```sh
sudo -E \
  HPCC_FIRECRACKER_BIN=/usr/local/bin/firecracker \
  HPCC_JAILER_BIN=/usr/local/bin/jailer \
  HPCC_TEST_KERNEL=/tmp/fcassets/vmlinux \
  ./bench/kernel-bench-fc.sh
```

Linux + KVM + root only. The supervisor binary
([`bench/cmd/fcstack/main.go`](cmd/fcstack/main.go)) brings up the
scheduler, worker, IdP, and pre-stages a rootfs from a public
toolchain OCI image; the shell script then drives the `make` dance
through it.

**Cache configuration:** the FC bench runs the worker in **paranoid
mode** with a worker-side disk cache at `bench/work/firecracker/stack-<ts>/cache`.
This is both the regulated-environment posture the README leads with
and the only configuration that meaningfully benchmarks the Phase 4
path — in non-paranoid mode every warm compile would short-circuit
on a client-side cache and never re-exercise scheduler/worker/FC
dispatch. The bench reads entry counts directly from that worker
cache directory; `hpcc stats` against the paranoid-mode client is
a no-op because clients have no local stores by design.

Defaults differ from local mode because per-TU FC dispatch overhead
means warm builds never hit the same speedup multiplier:

| Var                              | Default       |
|----------------------------------|---------------|
| `HPCC_BENCH_CONFIG`              | `tinyconfig` (defconfig is hours) |
| `HPCC_BENCH_MAX_WARM_PCT`        | `75` |
| `HPCC_BENCH_MIN_HIT_RATE`        | `90` |
| `HPCC_BENCH_VM_MEMORY`           | `2GB` |
| `HPCC_BENCH_VM_VCPUS`            | `2` |
| `HPCC_BENCH_POOL_MAX`            | `8` (concurrent VMs per tenant) |
| `HPCC_BENCH_TOOLCHAIN_IMAGE`     | `docker.io/library/gcc:13.2.0` (matched to ubuntu-latest's apt gcc patch version) |
| `HPCC_BENCH_WARM_RUNS`           | `2` (fewer than local — each FC build is much slower) |
| `HPCC_BENCH_SOURCE_MODE`         | `preprocessed` (also accepts `cas`) |

### Both source modes back-to-back

```sh
sudo -E \
  HPCC_FIRECRACKER_BIN=/usr/local/bin/firecracker \
  HPCC_JAILER_BIN=/usr/local/bin/jailer \
  HPCC_TEST_KERNEL=/tmp/fcassets/vmlinux \
  ./bench/kernel-bench-fc-both.sh
```

Runs `kernel-bench-fc.sh` once with `source_mode=preprocessed`, once
with `source_mode=cas`, and prints a side-by-side table of cold
seconds, warm-median, hit-rate, and cache size. Each leg gets its
own `bench/results/<ts>-firecracker-<mode>/` directory and its own
isolated stack (separate worker cache, daemon discovery file, xdg
config root) so the runs don't share state.

## What's measured

Each run consists of **one cold build plus N warm builds** (default
`HPCC_BENCH_WARM_RUNS=3` for local, `=2` for FC). The cache is
populated by the cold build and reused across all warm runs; `make
clean` between each run wipes object files but keeps `.config`. The
report surfaces the cold time, every warm sample, and the
min/median/max over the warm samples. The threshold check uses the
**median** warm time against cold — robust to a single noisy run on
a shared CI machine.

Output at `bench/results/<timestamp>-<mode>/`:

- `report.md` — markdown table with the headline numbers
- `stats.json` — same numbers + raw warm-sample array in JSON
- `build-cold.log`, `build-warm-1.log`, `build-warm-2.log`, … — raw `make` output per run
- `hpcc-config.toml` — the exact client config used (for repro)
- `fcstack.log` *(FC mode only)* — supervisor stderr

The success signal in both modes is a high cache hit rate combined
with a large cold/warm wall-time delta. Local mode reads hit-rate
from `hpcc stats` (single client-side disk cache); FC mode walks the
worker's paranoid-mode disk cache dir directly since the paranoid
client has no `hpcc stats` surface.

## TODO

- **`make modules` target.** `vmlinux` covers the bulk of the TU
  count but excludes most of `drivers/`. Adding a second pass with
  `make modules` would round out the workload.

- **Toolchain-identity parity between local and FC modes.** Today
  the local bench uses the host's apt `gcc` (gcc 13.2.0 on
  ubuntu-latest) and the FC bench uses whatever the configured OCI
  image ships. We pin `gcc:13.2.0` as a workaround so the two
  versions match, but that's a manual chase — every kernel-tag bump
  or `gcc:13` retag could break it again. The real fix is making
  the FC stack's toolchain provably the same as the host's: snapshot
  the host gcc/binutils/libc into a custom image at fcstack startup,
  or run a startup probe that asserts `gcc --version` parity inside
  vs outside the VM and refuses to run on mismatch. This is also
  what makes the README's "image digest IS the toolchain identity"
  pitch (plan §4) meaningful for cross-developer hit rates — without
  parity, two developers with nominally identical hpcc setups can
  produce bit-different `.o` outputs depending on which dispatch
  path their compiles took.

