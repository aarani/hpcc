#!/usr/bin/env bash
#
# bench/kernel-bench.sh — local-mode kernel-build benchmark.
#
# Configures hpcc with a single on-disk cache (no scheduler, no
# workers, no Firecracker), builds the kernel twice under
# CC="hpcc wrap gcc", and reports:
#   - cold vs. warm wall time
#   - new misses on the warm rebuild (≈0 is the success signal)
#   - cache hit rate, size, and entry count
#
# Output: bench/results/<timestamp>-local/{report.md,stats.json,
#         build-cold.log, build-warm.log, hpcc-config.toml}
#
# Knobs (env vars; defaults shown in bench/lib.sh):
#   HPCC_BENCH_KERNEL_TAG, HPCC_BENCH_JOBS, HPCC_BENCH_CONFIG,
#   HPCC_BENCH_TARGET, HPCC_BENCH_MIN_HIT_RATE,
#   HPCC_BENCH_MIN_SPEEDUP_PERCENT, HPCC_BENCH_KEEP

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
# shellcheck source=bench/lib.sh
source "${SCRIPT_DIR}/lib.sh"

bench::require_linux
bench::require_cmd git make gcc awk

HPCC_BENCH_CONFIG="${HPCC_BENCH_CONFIG:-defconfig}"
HPCC_BENCH_KEEP="${HPCC_BENCH_KEEP:-0}"
# Warm-build repeats. One cold + N warm; median of warm vs single
# cold is what the threshold checks. 3 is a reasonable noise filter
# without doubling CI wall time over the 1-warm baseline.
HPCC_BENCH_WARM_RUNS="${HPCC_BENCH_WARM_RUNS:-3}"
# Source mode — picks the cache-key algorithm in local mode (and,
# under remote dispatch, also the wire protocol; this bench is
# local-only). "cas" walks the dep closure (cheap); "preprocessed"
# runs the full preprocessor. Same cache contents either way.
# kernel-bench-both.sh runs both legs back to back and prints a
# side-by-side.
HPCC_BENCH_SOURCE_MODE="${HPCC_BENCH_SOURCE_MODE:-cas}"
case "${HPCC_BENCH_SOURCE_MODE}" in
    cas|preprocessed) ;;
    *)
        echo "HPCC_BENCH_SOURCE_MODE must be 'cas' or 'preprocessed' (got '${HPCC_BENCH_SOURCE_MODE}')" >&2
        exit 2
        ;;
esac

# Output directory — under bench/results/ so it's gitignored alongside
# CI artifacts. Timestamped so repeated runs accumulate rather than
# clobbering each other. Suffix includes the source mode so paired
# runs from kernel-bench-both.sh land in distinct dirs.
TS="$(date -u +%Y%m%dT%H%M%SZ)"
OUT_DIR="${REPO_ROOT}/bench/results/${TS}-local-${HPCC_BENCH_SOURCE_MODE}"
mkdir -p "${OUT_DIR}"

# Work area — kernel checkout + per-run cache + per-run hpcc config.
# Put it under bench/work/ rather than /tmp so CI can cache the
# kernel clone across runs by caching that directory.
WORK_DIR="${REPO_ROOT}/bench/work/local"
KERNEL_DIR="${WORK_DIR}/linux"
CACHE_DIR="${WORK_DIR}/cache-${TS}-${HPCC_BENCH_SOURCE_MODE}"
CONFIG_TOML="${OUT_DIR}/hpcc-config.toml"
mkdir -p "${WORK_DIR}" "${CACHE_DIR}"

cleanup() {
    if [[ "${HPCC_BENCH_KEEP}" == "1" ]]; then
        bench::info "HPCC_BENCH_KEEP=1; preserving work dirs at ${WORK_DIR}"
        return
    fi
    rm -rf "${CACHE_DIR}"
}
trap cleanup EXIT

# 1. Build the hpcc binary we'll wrap with. Use a dedicated output
#    path so we don't depend on $GOBIN being on PATH.
HPCC_BIN="${WORK_DIR}/hpcc"
bench::info "building hpcc → ${HPCC_BIN}"
(
    cd "${REPO_ROOT}"
    go build -o "${HPCC_BIN}" .
)

# 2. Write the per-run hpcc config. Single disk cache, generous size
#    cap so eviction doesn't interfere with the warm-build hit-rate
#    measurement. source_mode picks the cache-key algorithm for the
#    leg under test.
cat >"${CONFIG_TOML}" <<EOF
source_mode = "${HPCC_BENCH_SOURCE_MODE}"

[[cache]]
type     = "disk"
location = "${CACHE_DIR}"
max_size = "100G"
EOF
export HPCC_CONFIG="${CONFIG_TOML}"
bench::info "wrote ${CONFIG_TOML}"

# 3. Kernel checkout + configure. Cached across runs at
#    bench/work/local/linux — first run clones, later runs reuse.
bench::clone_kernel "${KERNEL_DIR}"
bench::configure_kernel "${KERNEL_DIR}" "${HPCC_BENCH_CONFIG}"

# 4. Cold build. `hpcc wrap gcc` is a single argv we hand to make's
#    CC variable; the wrap subcommand is the public API for "use the
#    cache for this compile."
CC_CMD="${HPCC_BIN} wrap gcc"

bench::info "cold build: make -j${HPCC_BENCH_JOBS} ${HPCC_BENCH_TARGET}"
(cd "${KERNEL_DIR}" && make clean)
COLD_SECONDS="$(bench::time_make "${KERNEL_DIR}" "${CC_CMD}" "${OUT_DIR}/build-cold.log")"
bench::info "cold build: ${COLD_SECONDS}s"

# Snapshot stats so we can compute (warm - cold) new misses.
read -r ENTRIES_COLD SIZE_COLD < <(bench::hpcc_stats "${HPCC_BIN}")
bench::info "after cold: ${ENTRIES_COLD} entries, $(bench::fmt_bytes "${SIZE_COLD}")"

# 5. Warm builds. `make clean` deletes object files but keeps .config;
#    the build then re-runs every compile with the same source +
#    flags, so every TU should hit. We repeat HPCC_BENCH_WARM_RUNS
#    times so noise on a shared runner doesn't swing the threshold
#    check based on a single unlucky pass.
WARM_TIMES=()
for run in $(seq 1 "${HPCC_BENCH_WARM_RUNS}"); do
    (cd "${KERNEL_DIR}" && make clean)
    bench::info "warm build ${run}/${HPCC_BENCH_WARM_RUNS}: make -j${HPCC_BENCH_JOBS} ${HPCC_BENCH_TARGET}"
    ws="$(bench::time_make "${KERNEL_DIR}" "${CC_CMD}" "${OUT_DIR}/build-warm-${run}.log")"
    bench::info "warm build ${run}: ${ws}s"
    WARM_TIMES+=("${ws}")
done

read -r ENTRIES_WARM SIZE_WARM < <(bench::hpcc_stats "${HPCC_BIN}")
bench::info "after warm: ${ENTRIES_WARM} entries, $(bench::fmt_bytes "${SIZE_WARM}")"

# 6. Report + thresholds.
bench::write_report \
    "${OUT_DIR}" \
    "local-${HPCC_BENCH_SOURCE_MODE}" \
    "${COLD_SECONDS}" \
    "${ENTRIES_COLD}" \
    "${ENTRIES_WARM}" \
    "${SIZE_WARM}" \
    "${WARM_TIMES[@]}"

NEW_ENTRIES=$(( ENTRIES_WARM - ENTRIES_COLD ))
(( NEW_ENTRIES < 0 )) && NEW_ENTRIES=0

if (( ENTRIES_COLD > 0 )); then
    HIT_RATE=$(awk -v new="${NEW_ENTRIES}" -v total="${ENTRIES_COLD}" \
        'BEGIN { printf "%.2f", (1 - new / total) * 100 }')
else
    HIT_RATE="0.00"
fi

WARM_MEDIAN=$(bench::stats median "${WARM_TIMES[@]}")
WARM_PCT=$(awk -v c="${COLD_SECONDS}" -v w="${WARM_MEDIAN}" \
    'BEGIN { if (c > 0) printf "%.1f", (w / c) * 100; else printf "0.0" }')

bench::assert_thresholds "${HIT_RATE}" "${WARM_PCT}"

bench::info "kernel-bench (local) passed"
