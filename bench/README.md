# Kernel-build benchmarks

Two benchmark pipelines that compile a real Linux kernel through hpcc
and report cache hit rate, cold-vs-warm wall time, and entry counts.
They cover the two modes hpcc actually ships:

| Script                  | Mode             | hpcc surface exercised                             |
|-------------------------|------------------|----------------------------------------------------|
| `kernel-bench.sh`       | local            | `hpcc wrap` → in-process compile → disk cache      |
| `kernel-bench-fc.sh`    | firecracker (P4) | `hpcc wrap` → dispatch → scheduler → worker → FC VM |

Each script:

1. Builds the `hpcc` binary from source.
2. Materializes the right config (a single disk cache for local
   mode; a `remote = enabled` TOML pointing at the in-process Phase 4
   stack for FC mode).
3. Shallow-clones a pinned Linux tag (`v6.6` by default) and runs the
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
| `HPCC_BENCH_KERNEL_TAG`          | `v6.6`        | Git tag in torvalds/linux to clone |
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

Defaults differ from local mode because per-TU FC dispatch overhead
means warm builds never hit the same speedup multiplier:

| Var                              | Default       |
|----------------------------------|---------------|
| `HPCC_BENCH_CONFIG`              | `tinyconfig` (defconfig is hours) |
| `HPCC_BENCH_MAX_WARM_PCT`        | `75` |
| `HPCC_BENCH_MIN_HIT_RATE`        | `90` (not enforced yet — see TODO below) |
| `HPCC_BENCH_VM_MEMORY`           | `2GB` |
| `HPCC_BENCH_VM_VCPUS`            | `2` |
| `HPCC_BENCH_POOL_MAX`            | `8` (concurrent VMs per tenant) |
| `HPCC_BENCH_TOOLCHAIN_IMAGE`     | `cgr.dev/chainguard/gcc-glibc:latest-dev` |
| `HPCC_BENCH_WARM_RUNS`           | `2` (fewer than local — each FC build is much slower) |

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

The success signal in local mode is a high cache hit rate combined
with a large cold/warm wall-time delta. The success signal in FC
mode is the wall-time delta alone, because the in-VM cache lookup
isn't yet exposed as a stats RPC.

## TODO

- **Worker-side stats in FC mode.** The local bench reads cache
  entry deltas via `hpcc stats`, which only knows about client-side
  caches. In FC mode the cache lives on the worker, and there's no
  RPC to fetch its tally; the bench currently asserts on wall-time
  only. Once §4 lands a worker stats endpoint, plumb it here.

- **`make modules` target.** `vmlinux` covers the bulk of the TU
  count but excludes most of `drivers/`. Adding a second pass with
  `make modules` would round out the workload.

