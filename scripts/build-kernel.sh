#!/usr/bin/env bash
#
# Build a microvm-compatible Linux kernel vmlinux for hpcc workers.
#
# Usage: scripts/build-kernel.sh <version> <arch> [out-dir]
#
#   version : 5.10 | 6.18    (mapped to an amazonlinux/linux branch)
#   arch    : amd64 | arm64  (matches Go's GOARCH naming)
#   out-dir : where to copy the resulting vmlinux (default: dist/kernel)
#
# Configs come from firecracker/kernel-configs/ (carbon copies of the
# matching microvm-kernel-ci-* configs from upstream firecracker, pinned
# into this repo so a release build doesn't depend on an external URL
# staying live). The kernel source comes from amazonlinux/linux at a
# microvm-kernel-* tag that matches firecracker's CI assumptions for
# that version. The output file follows the convention
# vmlinux-<version>-<arch> so the release workflow can attach all four
# variants to a GitHub Release without collisions.
#
# Cross-compile: building aarch64 from an amd64 runner requires the
# aarch64 GCC toolchain on PATH. The CI workflow installs
# `gcc-aarch64-linux-gnu`; for local builds, `apt install
# gcc-aarch64-linux-gnu` (Debian/Ubuntu) or the equivalent.

set -euo pipefail

if [ $# -lt 2 ]; then
  echo "usage: $0 <version> <arch> [out-dir]" >&2
  exit 64
fi

VERSION="$1"
ARCH_IN="$2"
OUT_DIR="${3:-dist/kernel}"

case "$VERSION" in
  5.10) TAG="microvm-kernel-5.10.260-301.1061.amzn2" ;;
  6.18) TAG="microvm-kernel-6.18.25-57.115.amzn2023" ;;
  *)
    echo "unknown version $VERSION (want 5.10 or 6.18)" >&2
    exit 64
    ;;
esac

case "$ARCH_IN" in
  amd64)
    CONFIG_ARCH=x86_64
    KERNEL_ARCH=x86_64
    CROSS_COMPILE=""
    ;;
  arm64)
    # Upstream firecracker names its configs aarch64; the Linux
    # kernel's build system uses ARCH=arm64; Go and our dist dirs
    # use arm64 too. We accept arm64 as the user-facing arg and
    # map to aarch64 only for the config-file lookup.
    CONFIG_ARCH=aarch64
    KERNEL_ARCH=arm64
    CROSS_COMPILE="aarch64-linux-gnu-"
    ;;
  *)
    echo "unknown arch $ARCH_IN (want amd64 or arm64)" >&2
    exit 64
    ;;
esac

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CONFIG="$REPO_ROOT/firecracker/kernel-configs/microvm-kernel-ci-${CONFIG_ARCH}-${VERSION}.config"
if [ ! -f "$CONFIG" ]; then
  echo "config not found: $CONFIG" >&2
  exit 1
fi

# Portable CPU count: nproc on Linux, sysctl on macOS/BSD, fall back to 2.
if command -v nproc >/dev/null 2>&1; then
  JOBS="$(nproc)"
elif command -v sysctl >/dev/null 2>&1; then
  JOBS="$(sysctl -n hw.ncpu 2>/dev/null || echo 2)"
else
  JOBS=2
fi

WORK="${WORK_DIR:-$(mktemp -d)}"
SRC="$WORK/linux-$VERSION-$ARCH_IN"

echo "==> cloning amazonlinux/linux @ $TAG into $SRC"
# --branch accepts a tag name too; we pin a tag rather than a moving
# branch so the kernel source is reproducible from one release to the
# next. --depth 1 keeps the clone under 2 GiB.
git clone --depth 1 --branch "$TAG" \
  https://github.com/amazonlinux/linux.git "$SRC"

echo "==> staging config"
install -m 0644 "$CONFIG" "$SRC/.config"

echo "==> make olddefconfig"
make -C "$SRC" ARCH="$KERNEL_ARCH" CROSS_COMPILE="$CROSS_COMPILE" olddefconfig

echo "==> building vmlinux with -j$JOBS"
make -C "$SRC" ARCH="$KERNEL_ARCH" CROSS_COMPILE="$CROSS_COMPILE" -j"$JOBS" vmlinux

mkdir -p "$OUT_DIR"
OUT="$OUT_DIR/vmlinux-$VERSION-$ARCH_IN"
install -m 0644 "$SRC/vmlinux" "$OUT"
echo "==> wrote $OUT ($(du -h "$OUT" | awk '{print $1}'))"

# Clean the source tree unless the caller pinned WORK_DIR (so a dev
# iterating on a config can avoid re-cloning the 1.5 GiB Amazon Linux
# tag every run).
if [ -z "${WORK_DIR:-}" ]; then
  rm -rf "$WORK"
fi
