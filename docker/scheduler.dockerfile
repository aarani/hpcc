FROM cgr.dev/chainguard/go:latest-dev AS builder
ADD . /app
WORKDIR /app
RUN make dist
RUN /bin/sh -c "cp dist/linux-amd64/hpcc /bin/scheduler"
FROM cgr.dev/chainguard/wolfi-base:latest AS final
USER nonroot
COPY --chown=nonroot:nonroot --from=builder /bin/scheduler /bin/scheduler
RUN chmod +x /bin/scheduler
EXPOSE 8080
ENTRYPOINT ["/bin/scheduler", "scheduler"]