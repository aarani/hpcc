# bench/lib.sh — shared helpers for the kernel-build benchmarks.
#
# Sourced by kernel-bench.sh (local mode) and kernel-bench-fc.sh
# (Firecracker mode). Both scripts perform the same outer dance —
# clone the kernel, configure, build twice, parse stats, write a
# report — and differ only in how `hpcc wrap` is configured.

# shellcheck shell=bash

# Pin to a recent stable LTS so two benchmark runs months apart still
# compare apples to apples. Bump when intentionally upgrading. The
# tag must exist on kernel.org git mirror.
HPCC_BENCH_KERNEL_TAG="${HPCC_BENCH_KERNEL_TAG:-v7.0}"

HPCC_BENCH_JOBS="${HPCC_BENCH_JOBS:-}"
if [[ -z "${HPCC_BENCH_JOBS}" ]]; then
    if command -v nproc >/dev/null 2>&1; then
        HPCC_BENCH_JOBS="$(nproc)"
    elif command -v sysctl >/dev/null 2>&1; then
        HPCC_BENCH_JOBS="$(sysctl -n hw.ncpu 2>/dev/null || echo 4)"
    else
        HPCC_BENCH_JOBS=4
    fi
fi

# Default thresholds. Override by exporting before invoking the
# benchmark — useful for the FC mode where per-TU dispatch overhead
# means the speedup is smaller and the hit rate is what matters.
#
#   MIN_HIT_RATE — minimum acceptable cache hit rate, in percent.
#                  Below this the warm build is doing too many real
#                  compiles (i.e., the cache lookup is missing for
#                  reasons that aren't source changes).
#   MAX_WARM_PCT — maximum acceptable warm-build wall time, as a
#                  percentage of cold. 50 means "warm must be at
#                  most 50% of cold" (i.e., 2× speedup).
HPCC_BENCH_MIN_HIT_RATE="${HPCC_BENCH_MIN_HIT_RATE:-95}"
HPCC_BENCH_MAX_WARM_PCT="${HPCC_BENCH_MAX_WARM_PCT:-50}"

# Build target. `vmlinux` is the canonical kernel artifact; switch to
# `bzImage` or `modules` to exercise a different mix of TUs.
HPCC_BENCH_TARGET="${HPCC_BENCH_TARGET:-vmlinux}"

# bench::die <msg> — print to stderr and exit non-zero.
bench::die() {
    printf 'bench: %s\n' "$*" >&2
    exit 1
}

# bench::info <msg> — green-tagged informational line on stderr so it
# doesn't poison stdout that callers may redirect.
bench::info() {
    printf '\033[1;32m[bench]\033[0m %s\n' "$*" >&2
}

# bench::warn <msg>
bench::warn() {
    printf '\033[1;33m[bench]\033[0m %s\n' "$*" >&2
}

# bench::require_linux — bail with an explanation on non-Linux hosts.
# Both benchmarks build a real `vmlinux`, which assumes Linux headers,
# elf-format object files, and a working `make` configured for the
# host. Building a Linux kernel inside a darwin or WSL bash from
# darwin isn't worth supporting just for benchmark coverage.
bench::require_linux() {
    case "$(uname -s)" in
        Linux) ;;
        *)
            bench::warn "kernel benchmark requires Linux; skipping on $(uname -s)"
            exit 0
            ;;
    esac
}

# bench::require_cmd <name> [<name>...] — fail if any of the listed
# binaries aren't on PATH.
bench::require_cmd() {
    local missing=()
    for cmd in "$@"; do
        if ! command -v "$cmd" >/dev/null 2>&1; then
            missing+=("$cmd")
        fi
    done
    if ((${#missing[@]} > 0)); then
        bench::die "missing required commands: ${missing[*]}"
    fi
}

# bench::clone_kernel <dest> — shallow clone of the pinned kernel tag
# into dest. Uses the kernel.org git mirror on github for resilience
# (kernel.org gitiana sometimes rate-limits CI ranges; github mirror is
# what most CI uses anyway).
bench::clone_kernel() {
    local dest="$1"
    if [[ -d "${dest}/.git" ]]; then
        bench::info "kernel checkout already present at ${dest}"
        return 0
    fi
    bench::info "cloning Linux ${HPCC_BENCH_KERNEL_TAG} into ${dest} (shallow)"
    git clone \
        --depth 1 \
        --branch "${HPCC_BENCH_KERNEL_TAG}" \
        --single-branch \
        https://github.com/torvalds/linux.git \
        "${dest}"
}

# bench::configure_kernel <kernel_dir> <config_name>
#
# Runs `make <config_name>` and pins build-time identity variables so
# the produced TUs are bit-identical across runs. SOURCE_DATE_EPOCH=0
# plus a fixed BUILD_USER/HOST/TIMESTAMP eliminates the build-time
# fingerprints that would otherwise force misses on a warm rebuild
# even when source content is identical.
bench::configure_kernel() {
    local kdir="$1"
    local cfg="$2"

    bench::info "configuring kernel: make ${cfg}"
    (
        cd "${kdir}"
        make "${cfg}"
    )

    # Also disable a few KCONFIG knobs that hash unique build data
    # into the object file (CONFIG_DEBUG_INFO embeds paths, randstruct
    # embeds a per-build seed). Use scripts/config so we don't have to
    # know the file layout.
    (
        cd "${kdir}"
        ./scripts/config --disable DEBUG_INFO || true
        ./scripts/config --disable GCC_PLUGIN_RANDSTRUCT || true
        ./scripts/config --disable LOCALVERSION_AUTO || true
        make olddefconfig
    )
}

# bench::kbuild_env — env vars that pin kernel build identity. Echo
# from a subshell, sourced into make's environment.
bench::kbuild_env() {
    cat <<'EOF'
export KBUILD_BUILD_TIMESTAMP=@0
export KBUILD_BUILD_USER=bench
export KBUILD_BUILD_HOST=bench
export SOURCE_DATE_EPOCH=0
EOF
}

# bench::time_make <kernel_dir> <cc_command> <log_path>
#
# Runs the kernel build with CC set to <cc_command>, capturing wall
# time, exit status, and full output. Returns wall-time seconds (as a
# fractional float) on stdout. On failure, dumps the tail of the log
# to stderr and exits non-zero.
#
# We avoid GNU `/usr/bin/time -v` because its format differs from BSD
# time (we want this to be at-least-runnable on darwin for syntax
# checking even though the bench itself bails earlier). The Bash
# builtin `time` writes to stderr — capture by redirecting fd 2 from
# a subshell.
bench::time_make() {
    local kdir="$1"
    local cc="$2"
    local log="$3"

    local start end seconds
    start=$(date +%s.%N 2>/dev/null || date +%s)

    set +e
    (
        eval "$(bench::kbuild_env)"
        cd "${kdir}"
        make -j"${HPCC_BENCH_JOBS}" CC="${cc}" "${HPCC_BENCH_TARGET}"
    ) >"${log}" 2>&1
    local rc=$?
    set -e

    end=$(date +%s.%N 2>/dev/null || date +%s)

    if [[ "${start}" == *.* && "${end}" == *.* ]]; then
        seconds=$(awk -v s="${start}" -v e="${end}" 'BEGIN { printf "%.3f", e - s }')
    else
        seconds=$(( end - start ))
    fi

    if [[ $rc -ne 0 ]]; then
        bench::warn "build failed (exit ${rc}); last 40 lines of log:"
        tail -n 40 "${log}" >&2
        bench::die "kernel build failed under CC='${cc}'"
    fi

    printf '%s\n' "${seconds}"
}

# bench::stats <op> <values...> — `op` is min|max|median; emits the
# selected statistic over the (numeric, possibly fractional) values
# on stdout. Implemented in awk so we can keep this portable across
# the runners that ship gawk vs busybox awk.
bench::stats() {
    local op="$1"
    shift
    [[ $# -gt 0 ]] || { printf '0\n'; return; }
    printf '%s\n' "$@" | awk -v op="${op}" '
        { v[NR] = $1 + 0 }
        END {
            if (NR == 0) { print 0; exit }
            # Selection sort is fine; N is tiny (≤ HPCC_BENCH_WARM_RUNS+1).
            for (i = 1; i < NR; i++) {
                for (j = i + 1; j <= NR; j++) {
                    if (v[j] < v[i]) { t = v[i]; v[i] = v[j]; v[j] = t }
                }
            }
            if (op == "min")    { printf "%.3f\n", v[1] }
            else if (op == "max") { printf "%.3f\n", v[NR] }
            else if (op == "median") {
                if (NR % 2 == 1) printf "%.3f\n", v[(NR + 1) / 2]
                else             printf "%.3f\n", (v[NR/2] + v[NR/2 + 1]) / 2
            } else {
                print "bench::stats: unknown op " op > "/dev/stderr"
                exit 1
            }
        }
    '
}

# bench::hpcc_stats <hpcc_bin> — emit `entries size_bytes` on stdout
# by invoking `hpcc stats` against the configured cache. Sums over
# all configured stores (we only ever configure one in these
# benchmarks, but the loop is cheap insurance).
bench::hpcc_stats() {
    local hpcc="$1"
    local raw
    raw="$("${hpcc}" stats 2>/dev/null || true)"

    local entries=0
    local size_bytes=0

    # `hpcc stats` output:
    #   Cache 0: /tmp/hpcc
    #     Entries: 1234
    #     Size:    1.5 GB
    #
    # The size is human-formatted by `formatSize` in cmd/stats.go —
    # parse the float + unit and re-multiply. We can't change the
    # format without breaking other consumers, so parse it here.
    while IFS= read -r line; do
        case "${line}" in
            *Entries:*)
                entries=$(( entries + $(echo "${line}" | awk '{print $NF}') ))
                ;;
            *Size:*)
                local num unit b
                num=$(echo "${line}" | awk '{print $(NF-1)}')
                unit=$(echo "${line}" | awk '{print $NF}')
                case "${unit}" in
                    B)  b=$(awk -v n="${num}" 'BEGIN { printf "%d", n }') ;;
                    KB) b=$(awk -v n="${num}" 'BEGIN { printf "%d", n * 1024 }') ;;
                    MB) b=$(awk -v n="${num}" 'BEGIN { printf "%d", n * 1024 * 1024 }') ;;
                    GB) b=$(awk -v n="${num}" 'BEGIN { printf "%d", n * 1024 * 1024 * 1024 }') ;;
                    *)  b=0 ;;
                esac
                size_bytes=$(( size_bytes + b ))
                ;;
        esac
    done <<<"${raw}"

    printf '%d %d\n' "${entries}" "${size_bytes}"
}

# bench::worker_cache_stats <dir> — emit `entries size_bytes` on stdout
# by walking a worker-side disk cache directly. The on-disk layout is
# <dir>/<hh>/<full-hex-key>/ with one file per blob; each entry is one
# leaf directory at depth 2. We count those and sum the byte sizes of
# every regular file under <dir>. Mirrors what hpcc.DiskCacheStore.Stats
# returns for the same path, but doesn't require a hpcc binary
# configured against the worker's cache (the bench's client runs in
# paranoid mode and has no [[cache]] block).
bench::worker_cache_stats() {
    local dir="$1"
    if [[ ! -d "${dir}" ]]; then
        printf '0 0\n'
        return
    fi
    local entries size
    entries=$(find "${dir}" -mindepth 2 -maxdepth 2 -type d 2>/dev/null | wc -l | tr -d ' ')
    # Sum file sizes via awk-on-find output. `wc -c` works portably
    # for a single file but not aggregated. GNU `find -printf` would
    # be cleaner but isn't on BSD/darwin, so we go through `ls -l`
    # which the awk parser can sum on both platforms.
    size=$(find "${dir}" -type f -exec wc -c {} + 2>/dev/null \
        | awk '/^[[:space:]]*[0-9]+[[:space:]]/ && $2 != "total" {s+=$1} END {print s+0}')
    : "${entries:=0}"
    : "${size:=0}"
    printf '%d %d\n' "${entries}" "${size}"
}

# bench::fmt_bytes <n> — human-readable bytes, matches `hpcc stats` style.
bench::fmt_bytes() {
    awk -v n="$1" 'BEGIN {
        if (n >= 1073741824) printf "%.1f GB", n / 1073741824
        else if (n >= 1048576) printf "%.1f MB", n / 1048576
        else if (n >= 1024)    printf "%.1f KB", n / 1024
        else                   printf "%d B", n
    }'
}

# bench::write_report <out_dir> <mode> <cold_s> <entries_cold>
#                    <entries_warm> <size_bytes> <warm_s...>
#
# mode is "local" or "firecracker". Writes report.md and stats.json.
# Takes one cold sample and one-or-more warm samples; reports
# min/median/max over the warm runs. The hit-rate metric is computed
# from the entry-count delta between the cold snapshot and the
# entries-after-final-warm-run snapshot — every TU that re-hit the
# cache adds zero entries, so a perfect cache gives ew == ec.
bench::write_report() {
    local out_dir="$1"
    local mode="$2"
    local cold_s="$3"
    local ec="$4"   # entries after cold
    local ew="$5"   # entries after final warm
    local sz="$6"   # bytes after final warm
    shift 6
    local warm_samples=("$@")

    [[ ${#warm_samples[@]} -gt 0 ]] || bench::die "write_report: need at least one warm sample"
    mkdir -p "${out_dir}"

    local warm_min warm_median warm_max
    warm_min=$(bench::stats min "${warm_samples[@]}")
    warm_median=$(bench::stats median "${warm_samples[@]}")
    warm_max=$(bench::stats max "${warm_samples[@]}")

    local new_entries=$(( ew - ec ))
    if (( new_entries < 0 )); then new_entries=0; fi

    local hit_rate_pct
    if (( ec > 0 )); then
        hit_rate_pct=$(awk -v new="${new_entries}" -v total="${ec}" \
            'BEGIN { printf "%.2f", (1 - new / total) * 100 }')
    else
        hit_rate_pct="0.00"
    fi

    # Compare median warm against the single cold sample. Median is
    # the right summary statistic for a noisy CI runner: it ignores
    # the occasional GC pause / I/O burst that would skew a mean.
    local warm_pct_of_cold
    warm_pct_of_cold=$(awk -v c="${cold_s}" -v w="${warm_median}" \
        'BEGIN { if (c > 0) printf "%.1f", (w / c) * 100; else printf "0.0" }')

    local size_human
    size_human=$(bench::fmt_bytes "${sz}")

    # JSON-encode the warm samples array. Always a clean float list.
    local warm_json
    warm_json=$(printf '%s\n' "${warm_samples[@]}" | awk '
        BEGIN { first = 1; printf "[" }
        { if (!first) printf ", "; first = 0; printf "%s", $1 }
        END { printf "]" }
    ')

    cat >"${out_dir}/stats.json" <<EOF
{
  "mode": "${mode}",
  "kernel_tag": "${HPCC_BENCH_KERNEL_TAG}",
  "target": "${HPCC_BENCH_TARGET}",
  "jobs": ${HPCC_BENCH_JOBS},
  "warm_runs": ${#warm_samples[@]},
  "cold_seconds": ${cold_s},
  "warm_seconds_samples": ${warm_json},
  "warm_seconds_min": ${warm_min},
  "warm_seconds_median": ${warm_median},
  "warm_seconds_max": ${warm_max},
  "warm_median_percent_of_cold": ${warm_pct_of_cold},
  "entries_after_cold": ${ec},
  "entries_after_warm": ${ew},
  "new_entries_on_warm": ${new_entries},
  "cache_hit_rate_percent": ${hit_rate_pct},
  "cache_size_bytes": ${sz}
}
EOF

    # Pretty-print each warm sample on its own line in the report.
    local warm_lines=""
    local i=0
    for ws in "${warm_samples[@]}"; do
        i=$((i + 1))
        warm_lines+="| Warm run ${i}          | ${ws} s |"$'\n'
    done

    cat >"${out_dir}/report.md" <<EOF
# hpcc kernel-build benchmark — ${mode} mode

| Field                | Value |
|----------------------|-------|
| Kernel tag           | \`${HPCC_BENCH_KERNEL_TAG}\` |
| Target               | \`${HPCC_BENCH_TARGET}\` |
| Parallel jobs (\`-j\`) | ${HPCC_BENCH_JOBS} |
| Warm runs            | ${#warm_samples[@]} |
| Cold build           | ${cold_s} s |
${warm_lines}| Warm min / median / max | ${warm_min} / **${warm_median}** / ${warm_max} s |
| Warm median % of cold | ${warm_pct_of_cold}% (lower is better) |
| Cached TUs (cold)    | ${ec} |
| New misses (warm)    | ${new_entries} |
| **Cache hit rate**   | **${hit_rate_pct}%** |
| Cache size           | ${size_human} |

Generated by \`bench/kernel-bench$([ "${mode}" = "firecracker" ] && echo "-fc").sh\` against
\`${HPCC_BENCH_TARGET}\` on $(date -u +%Y-%m-%dT%H:%M:%SZ).
EOF

    bench::info "wrote ${out_dir}/report.md"
    bench::info "wrote ${out_dir}/stats.json"
    cat "${out_dir}/report.md"
}

# bench::assert_thresholds <hit_rate_pct> <warm_pct_of_cold>
#
# warm_pct_of_cold: warm wall time as a percentage of cold (lower is
# better). Compared against HPCC_BENCH_MAX_WARM_PCT.
bench::assert_thresholds() {
    local hit_rate="$1"
    local warm_pct="$2"

    local warm_ok
    warm_ok=$(awk -v w="${warm_pct}" -v m="${HPCC_BENCH_MAX_WARM_PCT}" \
        'BEGIN { if (w <= m) print "1"; else print "0" }')
    if [[ "${warm_ok}" != "1" ]]; then
        bench::die "warm build too slow: ${warm_pct}% of cold > max ${HPCC_BENCH_MAX_WARM_PCT}%"
    fi

    local hit_rate_ok
    hit_rate_ok=$(awk -v h="${hit_rate}" -v m="${HPCC_BENCH_MIN_HIT_RATE}" \
        'BEGIN { if (h >= m) print "1"; else print "0" }')
    if [[ "${hit_rate_ok}" != "1" ]]; then
        bench::die "cache hit rate below threshold: ${hit_rate}% < ${HPCC_BENCH_MIN_HIT_RATE}%"
    fi

    bench::info "thresholds met (hit_rate=${hit_rate}%, warm=${warm_pct}% of cold)"
}
