#!/usr/bin/env bash
#
# bench/kernel-bench-fc.sh — Firecracker-mode kernel-build benchmark.
#
# Brings up the full Phase 4 stack via bench/cmd/fcstack (in-process
# IdP + scheduler + worker with the firecracker runtime, real rootfs
# built from a public toolchain image), then builds the kernel twice
# under CC="hpcc wrap gcc" with remote dispatch enabled in the
# client config. Reports the same metrics as kernel-bench.sh.
#
# Required env vars (the suite.yml firecracker-runtime job already
# sets all three for its e2e tests):
#   HPCC_FIRECRACKER_BIN  — path to the firecracker binary
#   HPCC_JAILER_BIN       — path to the jailer binary
#   HPCC_TEST_KERNEL      — path to a vmlinux for the microVMs
#
# Knobs:
#   HPCC_BENCH_CONFIG     — kernel config target; default `tinyconfig`
#                           (defconfig works but each TU pays per-TU
#                           FC dispatch overhead, so the run is hours)
#   HPCC_BENCH_VM_MEMORY  — per-VM memory, default 2GB
#   HPCC_BENCH_VM_VCPUS   — per-VM vCPUs, default 2
#   HPCC_BENCH_POOL_MAX   — max concurrent VMs per tenant, default 8
#   HPCC_BENCH_TOOLCHAIN_IMAGE — OCI ref for toolchain image
#                           default cgr.dev/chainguard/gcc-glibc:latest-dev
#
# Run as root (jailer needs CAP_SYS_ADMIN + chroot). The CI workflow
# invokes via `sudo -E`.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
# shellcheck source=bench/lib.sh
source "${SCRIPT_DIR}/lib.sh"

bench::require_linux
bench::require_cmd git make gcc awk go

# FC mode defaults differ from local mode: tinyconfig + relaxed
# thresholds, because per-TU dispatch overhead means warm builds
# never reach the same speedup multiplier as on-host caching.
HPCC_BENCH_CONFIG="${HPCC_BENCH_CONFIG:-tinyconfig}"
HPCC_BENCH_MAX_WARM_PCT="${HPCC_BENCH_MAX_WARM_PCT:-75}"
HPCC_BENCH_MIN_HIT_RATE="${HPCC_BENCH_MIN_HIT_RATE:-90}"
HPCC_BENCH_VM_MEMORY="${HPCC_BENCH_VM_MEMORY:-2GB}"
HPCC_BENCH_VM_VCPUS="${HPCC_BENCH_VM_VCPUS:-2}"
HPCC_BENCH_POOL_MAX="${HPCC_BENCH_POOL_MAX:-8}"
HPCC_BENCH_TOOLCHAIN_IMAGE="${HPCC_BENCH_TOOLCHAIN_IMAGE:-cgr.dev/chainguard/gcc-glibc:latest-dev}"
HPCC_BENCH_KEEP="${HPCC_BENCH_KEEP:-0}"
# FC builds are much slower per-iteration than local, so default to
# fewer warm repeats. Override with HPCC_BENCH_WARM_RUNS to trade
# wall-time budget for less noise.
HPCC_BENCH_WARM_RUNS="${HPCC_BENCH_WARM_RUNS:-2}"

if [[ "${EUID}" -ne 0 ]]; then
    bench::die "FC-mode bench must run as root (jailer); invoke via sudo -E"
fi

: "${HPCC_FIRECRACKER_BIN:?must point at firecracker binary}"
: "${HPCC_JAILER_BIN:?must point at jailer binary}"
: "${HPCC_TEST_KERNEL:?must point at a vmlinux}"

if [[ ! -e /dev/kvm ]]; then
    bench::die "/dev/kvm not present; cannot run FC-mode bench"
fi

# Out + work dirs. Same layout as the local bench so report
# consumers don't have to special-case modes.
TS="$(date -u +%Y%m%dT%H%M%SZ)"
OUT_DIR="${REPO_ROOT}/bench/results/${TS}-firecracker"
WORK_DIR="${REPO_ROOT}/bench/work/firecracker"
STACK_DIR="${WORK_DIR}/stack-${TS}"
KERNEL_DIR="${WORK_DIR}/linux"
CLIENT_CFG="${OUT_DIR}/hpcc-config.toml"
FCSTACK_LOG="${OUT_DIR}/fcstack.log"
# The worker runs in paranoid mode with its disk cache living
# inside the stack dir at this fixed path. fcstack creates it and
# wires the [[cache]] block; we read entry counts from here
# directly since the client has no cache surface in paranoid mode
# (and therefore `hpcc stats` against HPCC_CONFIG returns nothing).
WORKER_CACHE_DIR="${STACK_DIR}/cache"
mkdir -p "${OUT_DIR}" "${WORK_DIR}" "${STACK_DIR}"

# Make sure the unprivileged side can read our output (running as
# root via sudo, but humans inspect these files afterward).
SUDO_UID_REAL="${SUDO_UID:-0}"
SUDO_GID_REAL="${SUDO_GID:-0}"

FCSTACK_PID=""
DAEMON_PID=""
cleanup() {
    if [[ -n "${DAEMON_PID}" ]] && kill -0 "${DAEMON_PID}" 2>/dev/null; then
        bench::info "stopping hpcc daemon (pid ${DAEMON_PID})"
        kill -TERM "${DAEMON_PID}" 2>/dev/null || true
        wait "${DAEMON_PID}" 2>/dev/null || true
    fi
    if [[ -n "${FCSTACK_PID}" ]] && kill -0 "${FCSTACK_PID}" 2>/dev/null; then
        bench::info "stopping fcstack (pid ${FCSTACK_PID})"
        kill -TERM "${FCSTACK_PID}" 2>/dev/null || true
        wait "${FCSTACK_PID}" 2>/dev/null || true
    fi
    if [[ "${HPCC_BENCH_KEEP}" != "1" ]]; then
        rm -rf "${STACK_DIR}"
    fi
    chown -R "${SUDO_UID_REAL}:${SUDO_GID_REAL}" "${OUT_DIR}" 2>/dev/null || true
    chown -R "${SUDO_UID_REAL}:${SUDO_GID_REAL}" "${WORK_DIR}" 2>/dev/null || true
}
trap cleanup EXIT

# Isolate the daemon's discovery file under the stack dir so the
# bench never collides with a developer's system-wide hpcc daemon and
# leaves no stray state on EXIT. hpcc's daemon and `hpcc wrap` both
# resolve daemon.json through os.UserConfigDir, which honours
# $XDG_CONFIG_HOME — set it here and both halves point at the same
# bench-local path.
export XDG_CONFIG_HOME="${STACK_DIR}/xdg"
mkdir -p "${XDG_CONFIG_HOME}/hpcc"

# 1. Build hpcc + fcstack.
HPCC_BIN="${WORK_DIR}/hpcc"
FCSTACK_BIN="${WORK_DIR}/fcstack"
bench::info "building hpcc → ${HPCC_BIN}"
(cd "${REPO_ROOT}" && go build -o "${HPCC_BIN}" .)
bench::info "building fcstack → ${FCSTACK_BIN}"
(cd "${REPO_ROOT}" && go build -o "${FCSTACK_BIN}" ./bench/cmd/fcstack)

# 2. Start fcstack. It prints the client config path on stdout once
#    everything is wired up; until then stdout blocks. We capture
#    via a FIFO so we can both read the ready signal and keep its
#    stderr in the per-run log.
READY_FIFO="${STACK_DIR}/ready.fifo"
mkfifo "${READY_FIFO}"

bench::info "starting fcstack supervisor"
"${FCSTACK_BIN}" \
    --stack-dir="${STACK_DIR}" \
    --client-config="${CLIENT_CFG}" \
    --firecracker-bin="${HPCC_FIRECRACKER_BIN}" \
    --jailer-bin="${HPCC_JAILER_BIN}" \
    --kernel="${HPCC_TEST_KERNEL}" \
    --image-ref="${HPCC_BENCH_TOOLCHAIN_IMAGE}" \
    --vm-memory="${HPCC_BENCH_VM_MEMORY}" \
    --vm-vcpus="${HPCC_BENCH_VM_VCPUS}" \
    --pool-max-active="${HPCC_BENCH_POOL_MAX}" \
    >"${READY_FIFO}" 2>"${FCSTACK_LOG}" &
FCSTACK_PID=$!

# Read the first line of stdout (the client config path) with a
# bounded timeout — the rootfs pull is the slowest step and can
# take a minute on a cold runner; give it 5.
if ! READY_LINE=$(timeout 300 head -n 1 "${READY_FIFO}"); then
    bench::warn "fcstack failed to become ready in 5 minutes; last log:"
    tail -n 80 "${FCSTACK_LOG}" >&2
    bench::die "fcstack never reported ready"
fi
if [[ "${READY_LINE}" != "${CLIENT_CFG}" ]]; then
    bench::die "fcstack ready line was ${READY_LINE}, expected ${CLIENT_CFG}"
fi
bench::info "fcstack ready; client config at ${CLIENT_CFG}"

export HPCC_CONFIG="${CLIENT_CFG}"

# 3. Start the hpcc daemon. This is load-bearing: runner.Run only
#    dispatches to the scheduler+worker when a daemon is running —
#    the in-process path (no daemon) ignores remote.enabled and just
#    runs the compile locally. Without this step the entire FC stack
#    sits idle and every "remote" compile is silently a host gcc.
DAEMON_LOG="${OUT_DIR}/daemon.log"
DAEMON_FILE="${XDG_CONFIG_HOME}/hpcc/daemon.json"
bench::info "starting hpcc daemon (HPCC_CONFIG=${CLIENT_CFG})"
"${HPCC_BIN}" start --force >"${DAEMON_LOG}" 2>&1 &
DAEMON_PID=$!

# Daemon writes daemon.json once it's bound its listener and is
# ready to accept compile requests. Poll until it appears or the
# bounded window expires.
for _ in $(seq 1 60); do
    if [[ -f "${DAEMON_FILE}" ]]; then break; fi
    if ! kill -0 "${DAEMON_PID}" 2>/dev/null; then
        bench::warn "daemon exited before becoming ready; last log:"
        tail -n 40 "${DAEMON_LOG}" >&2
        bench::die "hpcc daemon failed to start"
    fi
    sleep 0.5
done
if [[ ! -f "${DAEMON_FILE}" ]]; then
    bench::warn "daemon never wrote ${DAEMON_FILE}; last log:"
    tail -n 40 "${DAEMON_LOG}" >&2
    bench::die "hpcc daemon never became ready"
fi
bench::info "hpcc daemon ready at ${DAEMON_FILE}"

# 4. Kernel checkout + configure.
bench::clone_kernel "${KERNEL_DIR}"
bench::configure_kernel "${KERNEL_DIR}" "${HPCC_BENCH_CONFIG}"

CC_CMD="${HPCC_BIN} wrap gcc"

# 5. Cold build. `make clean` first in case the cached checkout has
#    stale outputs from a previous run.
(cd "${KERNEL_DIR}" && make clean)
bench::info "cold build: make -j${HPCC_BENCH_JOBS} ${HPCC_BENCH_TARGET}"
COLD_SECONDS="$(bench::time_make "${KERNEL_DIR}" "${CC_CMD}" "${OUT_DIR}/build-cold.log")"
bench::info "cold build: ${COLD_SECONDS}s"

# Snapshot the worker cache after the cold build. In paranoid mode
# this is where every cacheable compile lands; the cold pass should
# have populated one entry per TU. Counting on the worker side (not
# `hpcc stats` against the paranoid client, which has no stores).
read -r ENTRIES_COLD SIZE_COLD < <(bench::worker_cache_stats "${WORKER_CACHE_DIR}")
bench::info "after cold: ${ENTRIES_COLD} worker-cache entries, $(bench::fmt_bytes "${SIZE_COLD}")"

# 6. Warm builds — HPCC_BENCH_WARM_RUNS of them. The worker-side
#    cache is reused across runs so all warm passes should land in
#    the same regime; multiple samples let us median over runner
#    noise (gRPC tail latency, FC start jitter).
WARM_TIMES=()
for run in $(seq 1 "${HPCC_BENCH_WARM_RUNS}"); do
    (cd "${KERNEL_DIR}" && make clean)
    bench::info "warm build ${run}/${HPCC_BENCH_WARM_RUNS}: make -j${HPCC_BENCH_JOBS} ${HPCC_BENCH_TARGET}"
    ws="$(bench::time_make "${KERNEL_DIR}" "${CC_CMD}" "${OUT_DIR}/build-warm-${run}.log")"
    bench::info "warm build ${run}: ${ws}s"
    WARM_TIMES+=("${ws}")
done

read -r ENTRIES_WARM SIZE_WARM < <(bench::worker_cache_stats "${WORKER_CACHE_DIR}")
bench::info "after warm: ${ENTRIES_WARM} worker-cache entries, $(bench::fmt_bytes "${SIZE_WARM}")"

# 7. Report. ENTRIES_WARM - ENTRIES_COLD is the count of TUs that
#    still missed on the warm pass — for a sound cache that delta is
#    zero, and the hit-rate field matches the local-mode bench.
bench::write_report \
    "${OUT_DIR}" \
    "firecracker" \
    "${COLD_SECONDS}" \
    "${ENTRIES_COLD}" \
    "${ENTRIES_WARM}" \
    "${SIZE_WARM}" \
    "${WARM_TIMES[@]}"

WARM_MEDIAN=$(bench::stats median "${WARM_TIMES[@]}")
WARM_PCT=$(awk -v c="${COLD_SECONDS}" -v w="${WARM_MEDIAN}" \
    'BEGIN { if (c > 0) printf "%.1f", (w / c) * 100; else printf "0.0" }')

NEW_ENTRIES=$(( ENTRIES_WARM - ENTRIES_COLD ))
(( NEW_ENTRIES < 0 )) && NEW_ENTRIES=0
if (( ENTRIES_COLD > 0 )); then
    HIT_RATE=$(awk -v new="${NEW_ENTRIES}" -v total="${ENTRIES_COLD}" \
        'BEGIN { printf "%.2f", (1 - new / total) * 100 }')
else
    HIT_RATE="0.00"
fi

bench::assert_thresholds "${HIT_RATE}" "${WARM_PCT}"

bench::info "kernel-bench (firecracker) passed"
