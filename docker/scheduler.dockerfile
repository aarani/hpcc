# hpcc scheduler — runs `hpcc scheduler --config /etc/hpcc/scheduler.toml`.
#
# Prepare a config on the host with `hpcc init scheduler` (see README), then
# bind-mount the resulting scheduler.toml plus the TLS material it points at
# into the container. Example:
#
#   docker run -d --name hpcc-scheduler \
#     -v /path/to/scheduler.toml:/etc/hpcc/scheduler.toml:ro \
#     -v /path/to/scheduler.crt:/etc/hpcc/scheduler.crt:ro \
#     -v /path/to/scheduler.key:/etc/hpcc/scheduler.key:ro \
#     -p 9091:9091 -p 9191:9191 \
#     ghcr.io/aarani/hpcc-scheduler:latest

FROM cgr.dev/chainguard/go:latest-dev AS builder
ADD . /app
WORKDIR /app
# dist-linux-amd64 is the narrowest cross-compile target — the full `make
# dist` matrix would also build windows/darwin and arm64 binaries we'd
# throw away.
RUN make dist-linux-amd64

FROM cgr.dev/chainguard/wolfi-base:latest AS final
USER nonroot
COPY --chown=nonroot:nonroot --from=builder /app/dist/linux-amd64/hpcc /usr/local/bin/hpcc

# gRPC (clients + workers) and Prometheus /metrics. Defaults match
# docs/scheduler.toml; if you change `listen` / `metrics_listen` in the
# mounted config, update the publish flags accordingly.
EXPOSE 9091 9191

ENTRYPOINT ["/usr/local/bin/hpcc", "scheduler", "--config", "/etc/hpcc/scheduler.toml"]
