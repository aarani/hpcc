#!/usr/bin/env bash
#
# bench/kernel-bench-fc-both.sh — runs kernel-bench-fc.sh twice, once
# per source mode, and prints a short side-by-side summary.
#
# Each mode gets its own OUT_DIR / STACK_DIR / xdg (see
# kernel-bench-fc.sh for the partitioning scheme), so the two runs
# don't share daemon discovery files, worker compile caches, or
# anything else stateful. The kernel checkout is reused — make clean
# zeroes build state between runs.
#
# Pass through any of the standard HPCC_BENCH_* env vars; this
# wrapper only sets HPCC_BENCH_SOURCE_MODE for each leg.
#
# Run as root (jailer requirement, same as the underlying bench).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Strict order: PREPROCESSED first so the path-normalization-free
# baseline runs against a clean .hpcc-less kernel checkout. The CAS
# leg adds the marker; the next PREPROCESSED leg (if anyone re-runs)
# would still work but report a stray .hpcc — easier to interpret
# results if we always pick this order.
MODES=(preprocessed cas)

declare -A REPORT_DIRS=()

for mode in "${MODES[@]}"; do
    echo
    echo "==> kernel-bench-fc source_mode=${mode}"
    echo
    HPCC_BENCH_SOURCE_MODE="${mode}" "${SCRIPT_DIR}/kernel-bench-fc.sh"

    # Discover the OUT_DIR the inner run wrote to — newest
    # `*-firecracker-${mode}` directory under bench/results.
    latest=$(ls -1dt "${SCRIPT_DIR}/results/"*"-firecracker-${mode}" 2>/dev/null | head -n 1 || true)
    if [[ -z "${latest}" || ! -d "${latest}" ]]; then
        echo "warn: could not locate results dir for mode=${mode}" >&2
        continue
    fi
    REPORT_DIRS["${mode}"]="${latest}"
done

echo
echo "==> side-by-side"
echo

# Pull the key numbers out of each mode's stats.json. jq is the
# obvious tool but lib.sh's other reporters don't depend on it; stick
# to grep+awk so this wrapper has the same prerequisite footprint as
# the underlying bench.
extract() {
    local file="$1" key="$2"
    grep -E "\"${key}\"" "${file}" | head -n 1 |
        awk -F: '{ gsub(/[, ]/, "", $2); print $2 }'
}

printf "%-14s  %12s  %14s  %12s  %16s\n" mode cold_seconds warm_median_s hit_rate% cache_size_bytes
for mode in "${MODES[@]}"; do
    dir="${REPORT_DIRS[$mode]:-}"
    if [[ -z "${dir}" ]]; then continue; fi
    stats="${dir}/stats.json"
    if [[ ! -f "${stats}" ]]; then continue; fi
    cold=$(extract "${stats}" cold_seconds)
    warm=$(extract "${stats}" warm_seconds_median)
    hit=$(extract "${stats}" cache_hit_rate_percent)
    size=$(extract "${stats}" cache_size_bytes)
    printf "%-14s  %12s  %14s  %12s  %16s\n" "${mode}" "${cold}" "${warm}" "${hit}" "${size}"
done
