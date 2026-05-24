# hpcc test rootfs — Debian-slim base with the dep set needed to
# compile a modern Linux kernel. Intended for use as the worker's
# toolchain image (passed to the daemon via remote.image_ref /
# image_digest) when dispatching kernel-build TUs.
#
# This is deliberately not a production-hardened image: it pulls
# whatever apt resolves on build day, the resulting image is sealed
# into a squashfs rootfs by the worker, and the digest pinning that
# protects against drift happens at the hpcc layer (client.toml's
# image_digest), not inside the Dockerfile.
#
# Why Debian-slim over chainguard/gcc-glibc: the kernel needs more
# than just gcc + libc — libelf-dev for objtool, libssl-dev for
# module signing, bison/flex/bc/cpio/kmod for the build system,
# pahole for BTF generation. Chainguard's minimal images don't ship
# these and layering them on top of a distroless base is more work
# than starting from a distro that has them packaged.
#
# Triggers a rebuild via .github/workflows/test-image.yml on every
# push that touches this file.

FROM debian:bookworm-slim

# Single RUN keeps the layer count down and the apt cache out of the
# final image. --no-install-recommends prunes documentation and
# weak-dep pulls that would balloon the rootfs without helping a
# kernel build.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      gcc \
      binutils \
      make \
      bison \
      flex \
      bc \
      cpio \
      kmod \
      libelf-dev \
      libssl-dev \
      pahole \
      perl \
      python3 \
      ca-certificates \
 && rm -rf /var/lib/apt/lists/*

# No ENTRYPOINT — hpcc-agent execs the user's compiler argv directly,
# so the image just needs a populated rootfs. CMD likewise unused.
