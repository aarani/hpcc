<h1 align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/logo-dark.svg">
    <img alt="hpcc — vault cube mark" src="docs/logo.svg" width="200" height="200">
  </picture>
  <br />
  hpcc
</h1>

<p align="center">
  <strong>A distributed compiler cache that a regulated security team will actually approve.</strong>
  <br />
  <em>Sandboxed remote compilation · per-tenant KVM boundary · auditable by row.</em>
</p>

<p align="center">
  <a href="https://github.com/aarani/hpcc/actions/workflows/suite.yml"><img alt="Build &amp; Test Suite" src="https://github.com/aarani/hpcc/actions/workflows/suite.yml/badge.svg?branch=main"></a>
  <a href="https://github.com/aarani/hpcc/blob/main/LICENSE"><img alt="License: AGPL-3.0" src="https://img.shields.io/badge/license-AGPL--3.0-blue.svg"></a>
  <a href="https://go.dev/"><img alt="Go 1.26+" src="https://img.shields.io/badge/go-1.26%2B-00ADD8?logo=go&amp;logoColor=white"></a>
  <a href="https://goreportcard.com/report/github.com/aarani/hpcc"><img alt="Go Report Card" src="https://goreportcard.com/badge/github.com/aarani/hpcc"></a>
  <a href="https://pkg.go.dev/github.com/aarani/hpcc"><img alt="Go Reference" src="https://pkg.go.dev/badge/github.com/aarani/hpcc.svg"></a>
  <a href="https://hpcc.dev"><img alt="hpcc.dev" src="https://img.shields.io/badge/site-hpcc.dev-0e1014"></a>
</p>

---

> ⚠️ **Work in progress.** hpcc is under active development and has not been audited.
> Do not rely on it for security-sensitive or production workloads yet.

## Quick start

```sh
git clone https://github.com/aarani/hpcc.git
cd hpcc && go build && go install

# wrap a compiler invocation
hpcc wrap cc -c hello.c -o hello.o

# or wire into a Makefile
make CC="hpcc wrap cc" CXX="hpcc wrap c++"

# start the daemon (foreground; supervise with systemd / launchd)
hpcc start
```

## Server quick start

Bring up the distributed pieces — one scheduler, N workers, M clients
— without hand-editing TOML. `hpcc init` writes the configs for you;
the generated files validate immediately.

**Scheduler.** Needs a TLS cert clients can trust (public CA or your
org's internal CA) and one tenant's IdP coordinates:

```sh
hpcc init scheduler \
  --cert-file /etc/hpcc/scheduler.crt \
  --key-file  /etc/hpcc/scheduler.key \
  --tenant-id acme \
  --issuer    https://idp.acme.example/ \
  --jwks-url  https://idp.acme.example/.well-known/jwks.json \
  --token-url https://idp.acme.example/oauth/token \
  --audience  hpcc

hpcc scheduler
```

The command prints a freshly-generated `worker_token`; copy it.

**Workers (Linux / Firecracker).** On each worker host, paste the
token from above and point at the scheduler. TLS material is
self-signed and minted in place — the scheduler pins by SHA-256
fingerprint at registration, so a real CA isn't needed worker-side:

```sh
hpcc init worker \
  --scheduler  scheduler.internal:9091 \
  --token      <paste from init scheduler> \
  --public-addr worker-1.internal:9092

# Host prerequisites (the init command tells you exactly these):
#   apt install firecracker                        # /usr/bin/firecracker, /usr/bin/jailer
#   curl -o /var/lib/hpcc/vmlinux <kernel-url>     # kernel image
#   curl -o /var/lib/hpcc/hpcc-agent-linux-amd64 \
#        <release-url>                             # in-VM agent

hpcc worker
```

The generated `worker.toml` points at the standard paths above; if you
have firecracker installed somewhere else, edit
`[runtime.firecracker]` to match. For a zero-isolation dev box, pass
`--runtime really_really_dangerous` (never production).

**Workers (Windows / Hyper-V).** Same paste-the-token flow, with the
hcsshim runtime selected:

```pwsh
hpcc init worker `
  --scheduler   scheduler.internal:9091 `
  --token       <paste from init scheduler> `
  --public-addr worker-win-1.internal:9092 `
  --runtime     runhcs-wcow-hypervisor

# Host prerequisites (the init command tells you exactly these):
#   Install-WindowsFeature Hyper-V -IncludeManagementTools  # reboot once
#   Start-Service vmcompute                                  # HCS
#   # containerd listening on \\.\pipe\containerd-containerd
#   # hpcc-agent.exe staged at C:\ProgramData\hpcc\hpcc-agent.exe

hpcc worker
```

Hyper-V isolation is the production value — each container runs in
its own utility VM, the kernel boundary a regulated security review
recognises. For hosts without nested virt (GitHub-hosted CI runners,
dev laptops), edit `runtime.hcsshim.isolation = "process"` in the
generated file; you lose the kernel boundary, so this is dev-only.

**Clients.** On each developer machine, point the client at the
scheduler and authenticate against the tenant IdP:

```sh
hpcc init client \
  --scheduler    scheduler.internal:9091 \
  --tenant       acme \
  --image-ref    ghcr.io/example/toolchain \
  --image-digest sha256:abc...

hpcc auth login   # prompts for username + password
hpcc start        # daemon; supervise with systemd / launchd
```

Then point your build at `hpcc wrap cc` / `hpcc wrap c++` as in the
client Quick start above. The daemon falls back to local execution on
any remote failure and prints a red warning, so a misconfigured client
never blocks a build.

## Why?

`ccache`, `sccache`, and `distcc` all assume the worker is trusted
shared-kernel infrastructure. That assumption ends the conversation in
a regulated enterprise. hpcc inverts it: every compile runs in a
per-tenant Firecracker microVM (Linux) or Hyper-V-isolated container
(Windows), the worker has no NIC, the container image digest *is* the
toolchain identity, and every job lands a single audit row.

Design, threat model, and what's shipped vs. open all live in
[**docs/plan.md**](docs/plan.md) and the per-phase docs under
[`docs/plan/`](docs/plan/). Config reference:
[`client.toml`](docs/client.toml) /
[`scheduler.toml`](docs/scheduler.toml) /
[`worker.toml`](docs/worker.toml).
