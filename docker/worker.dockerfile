# hpcc worker — runs `hpcc worker --config /etc/hpcc/worker.toml`.
#
# Bundles everything the worker needs to spawn microvms: the hpcc
# binary, the in-VM agent (staged into each rootfs by the worker at
# runtime), a microvm-compatible vmlinux, and the firecracker+jailer
# pair. firecracker/jailer and the kernel are downloaded at build
# time from upstream releases; the version pins are build-args so a
# single dockerfile services every supported (firecracker, kernel)
# combination.
#
# The image must run privileged with /dev/kvm exposed — jailer needs
# CAP_SYS_ADMIN to set up the cgroup hierarchy and pivot_root before
# it drops to the configured (uid, gid) and exec's firecracker. See
# deploy/helm/hpcc-worker for a working DaemonSet.

ARG FC_VERSION=v1.15.1
ARG HPCC_VERSION=v0.1.0-alpha
ARG KERNEL_VERSION=6.1

FROM cgr.dev/chainguard/go:latest-dev AS builder
ARG TARGETARCH
ADD . /app
WORKDIR /app
# dist-linux-${TARGETARCH} produces dist/linux-<arch>/{hpcc,hpcc-agent,hpcc-pause}.
# The buildx-supplied TARGETARCH lines up with the make matrix
# (amd64/arm64), so the same dockerfile services both platforms.
RUN make dist-linux-${TARGETARCH}

# Fetcher stage downloads everything that we don't build ourselves.
# Splitting it out keeps the final image free of curl/tar and lets
# buildx cache the (usually-unchanged) downloads independently from
# the go build above. Alpine over chainguard/wolfi-base here because
# the chainguard apk repo doesn't ship a tar package — wolfi expects
# tar via busybox, which complicates the pipe-into-tar one-liner below.
FROM alpine:3.20 AS fetcher
ARG TARGETARCH
ARG FC_VERSION
ARG HPCC_VERSION
ARG KERNEL_VERSION
RUN apk add --no-cache curl tar
WORKDIR /staging
# firecracker uses x86_64/aarch64 in its tarball naming; the rest of
# the project (and TARGETARCH) uses amd64/arm64. Map across once.
RUN case "$TARGETARCH" in \
      amd64) FC_ARCH=x86_64  ;; \
      arm64) FC_ARCH=aarch64 ;; \
      *) echo "unsupported TARGETARCH=$TARGETARCH" >&2; exit 1 ;; \
    esac && \
    curl -fsSL "https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}/firecracker-${FC_VERSION}-${FC_ARCH}.tgz" \
      | tar -xz -C . && \
    install -m 0755 "release-${FC_VERSION}-${FC_ARCH}/firecracker-${FC_VERSION}-${FC_ARCH}" firecracker && \
    install -m 0755 "release-${FC_VERSION}-${FC_ARCH}/jailer-${FC_VERSION}-${FC_ARCH}"      jailer && \
    rm -rf "release-${FC_VERSION}-${FC_ARCH}" && \
    curl -fsSL -o vmlinux "https://github.com/aarani/hpcc/releases/download/${HPCC_VERSION}/vmlinux-${KERNEL_VERSION}-${TARGETARCH}" && \
    chmod 0644 vmlinux

FROM alpine:3.20 AS final
ARG TARGETARCH
# Alpine over chainguard/wolfi-base because the helm chart's init
# containers (`kvm-perms`, `render-config`) exec sed/chmod via this
# same image, and alpine ships busybox + a real /bin/sh out of the
# box. The image runs privileged anyway, so wolfi-base's hardening
# wouldn't buy much here.
#
# Jailer requires a non-root (uid, gid) to drop to before exec'ing
# firecracker; we bake one in so the default worker.toml shipped by
# the helm chart works out of the box. GID 36 matches the de-facto
# kvm group on Debian/Ubuntu hosts — operators running a host with a
# different kvm gid should add an initContainer that chmods /dev/kvm
# to 0666 (see chart docs).
# alpine-baselayout ships a `kvm` group whose gid drifts between
# releases (3.20 currently has it at 34, not 36). The chart's
# `runtime.firecracker.gid: 36` is the Debian/Ubuntu convention and
# what we want jailer to drop to, so delete whatever alpine pre-baked
# and recreate at the exact gid we ship in values.yaml.
RUN delgroup kvm 2>/dev/null; \
    addgroup -S -g 36 kvm && \
    adduser  -S -D -H -h /var/lib/hpcc -s /sbin/nologin -G kvm -u 1000 hpcc && \
    install -d -o hpcc -g kvm -m 0755 /var/lib/hpcc /var/lib/hpcc/rootfs /srv/jailer && \
    install -d -m 0755 /etc/hpcc

COPY --from=builder /app/dist/linux-${TARGETARCH}/hpcc       /usr/local/bin/hpcc
COPY --from=builder /app/dist/linux-${TARGETARCH}/hpcc-agent /var/lib/hpcc/hpcc-agent-linux-${TARGETARCH}
COPY --from=fetcher /staging/firecracker /usr/bin/firecracker
COPY --from=fetcher /staging/jailer      /usr/bin/jailer
COPY --from=fetcher /staging/vmlinux     /var/lib/hpcc/vmlinux

# gRPC (clients/scheduler) and Prometheus /metrics. Defaults match
# the worker.toml template emitted by `hpcc init worker`.
EXPOSE 9092 9192

# Runs as root: jailer manipulates cgroups and pivot_roots before
# dropping privileges itself. The pod's securityContext must allow
# CAP_SYS_ADMIN (privileged) and expose /dev/kvm.
ENTRYPOINT ["/usr/local/bin/hpcc", "worker", "--config", "/etc/hpcc/worker.toml"]
