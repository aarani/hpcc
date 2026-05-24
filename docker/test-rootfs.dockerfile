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

# trixie ships gcc 14 as the default. Bookworm's gcc 12 lacks flags
# the upstream kernel's Kbuild probes for on recent gccs
# (-fmin-function-alignment, -fstrict-flex-arrays=3, ...); when the
# daemon's local gcc supports them and the worker's doesn't, every
# affected TU fails remotely with "unrecognized command-line option".
# Keeping daemon and worker on the same major version is the bar.
FROM debian:trixie-slim

# Single RUN keeps the layer count down and the apt cache out of the
# final image. --no-install-recommends prunes documentation and
# weak-dep pulls that would balloon the rootfs without helping a
# kernel build.
#
# gcc-14 is installed explicitly (rather than the unversioned `gcc`
# meta-package) so the image's compiler identity is obvious from the
# dockerfile alone — important for the hpcc invariant that the
# daemon's gcc and the worker's gcc agree. A future kernel that needs
# gcc-15 means bumping this line, not a silent transitive default
# change at apt-update time.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      gcc-14 \
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
 && ln -sf /usr/bin/gcc-14 /usr/bin/gcc \
 && ln -sf /usr/bin/gcc-14 /usr/bin/cc \
 && rm -rf /var/lib/apt/lists/*

# No ENTRYPOINT — hpcc-agent execs the user's compiler argv directly,
# so the image just needs a populated rootfs. CMD likewise unused.
