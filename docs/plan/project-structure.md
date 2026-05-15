## Project Structure

```
cmd/
  root.go           — base cobra command
  wrap.go           — hpcc wrap <compiler> [args...]
  start.go          — hpcc start (foreground daemon)
  stats.go          — hpcc stats
  clean.go          — hpcc clean
  inspect.go        — hpcc inspect
  scheduler.go      — hpcc scheduler
  worker.go         — hpcc worker
internal/
  compiler/         — flag parsing, invocation, preprocess, cache-key
  cache/
    store/
      store.go      — Store interface
      disk.go       — local disk cache (CAS)
      s3.go         — S3-compatible remote store (Phase 3)
      factory.go    — config → []Store, shared by client and worker
  daemon/           — client-side daemon (loopback TCP)
  protocol/         — host-plane wire schema (compile, scheduler, worker, audit)
  scheduler/        — route-only coordinator, worker registry, JWT signer
  worker/
    worker.go       — host-side worker (Compile RPC, image catalogue, pool)
    runtime/        — Runtime interface; raw Firecracker driver (Linux),
                      jailer setup, vsock dial, mount cleanup,
                      DangerouslyExecOnHost dev backend
    image/
      rootfs/       — OCI pull → flatten → streaming tar → squashfs (Linux)
      cdimage/      — containerd image prep (Windows path, follow-up)
agent/              — separate Go module: in-VM hpcc-agent (Linux) — PID-1 init,
                      mount setup, zombie reaping, AgentService.Exec gRPC server
                      over AF_VSOCK
pause/              — separate Go module: tiny static PID-1 binary, used as
                      hpcc-pause.exe on the Windows containerd path
proto/              — separate Go module: shared agent↔runner wire schema.
                      proto/agent/agent.proto + generated .pb.go. Imported by
                      both the main module and the agent module without
                      dragging either side's heavy deps in the other direction
squashfs/           — separate Go module: clean-room Go squashfs 4.0 writer.
                      Pluggable Compressor; in-tree, no GPL deps in the
                      build path. Format-validated in CI via unsquashfs.
firecracker/        — generated Firecracker VMM API client (go-swagger)
go.work             — multi-module workspace tying all five modules together
```
