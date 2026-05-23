# hpcc-worker

Helm chart for the [hpcc](https://github.com/aarani/hpcc) worker — a
privileged DaemonSet that spawns Firecracker microvms for tenant compile
jobs.

## Prerequisites

- **`/dev/kvm` on every targeted node.** Bare-metal or a VM with nested
  virt enabled. `kubectl get node <name> -o jsonpath='{.status.allocatable}'`
  won't tell you; the easiest probe is to schedule a one-shot pod that
  runs `ls /dev/kvm`.
- **Cgroup v2.** Jailer's defaults target v2; v1 hosts need a code
  change to pass `--cgroup-version 1`.
- **AppArmor and seccomp unconfined.** Some K8s distros apply
  `runtime/default` even on privileged pods; jailer's `pivot_root` and
  cgroup writes get blocked. See the pod-level overrides in
  `values.yaml` / your PodSecurityPolicy / Pod Security Admission
  configuration.
- A **running scheduler** (the `hpcc-scheduler` chart works) and the
  **`worker_token`** it generated.

## Install

Sharing the auth Secret between the two charts is the cleanest pattern:

```bash
# 1. Install the scheduler with an auto-generated worker_token.
helm install scheduler ./deploy/helm/hpcc-scheduler \
  --namespace hpcc --create-namespace \
  --set auth.workerToken="$(openssl rand -hex 32)" \
  --set-file tls.cert=scheduler.crt \
  --set-file tls.key=scheduler.key

# 2. Point the worker at the scheduler's auth Secret and the worker's
#    own TLS material.
helm install worker ./deploy/helm/hpcc-worker \
  --namespace hpcc \
  --set scheduler.url=scheduler-hpcc-scheduler.hpcc.svc:9091 \
  --set auth.existingSecret=scheduler-hpcc-scheduler-auth \
  --set auth.existingSecretKey=WORKER_TOKEN \
  --set-file tls.cert=worker.crt \
  --set-file tls.key=worker.key \
  --set nodeSelector."hpcc\.io/kvm"=true
```

## How `public_addr` works

Clients dial workers directly (the scheduler just routes). Each pod
needs to advertise an address its clients can reach. The chart uses:

- `hostNetwork: true` and `hostPort` on the container — the worker
  listens on the node's IP at port 9092.
- An init container that renders `worker.toml` from a template at pod
  start, substituting `$NODE_IP` (downward-API `status.hostIP`) into the
  `public_addr` field.

If clients live inside the cluster they can reach `<node-ip>:9092`
directly. If they live outside, expose the worker's host port at the
node level (firewall, security group, or LoadBalancer-per-node).

## `/dev/kvm` permissions

Jailer drops to (uid=1000, gid=36) before exec'ing firecracker. After
the drop, firecracker has to open `/dev/kvm`. Two ways to make that
work:

1. **Default — `kvm.fixOwnership: true`.** An init container chmods
   `/dev/kvm` to `0666`. Pragmatic, blunt, fine for trusted nodes.
2. **A device plugin** that exposes `/dev/kvm` with explicit perms.
   Disable the init container and mount via the plugin's resource
   request.

## Customising the runtime

`values.yaml` exposes the firecracker block (`memory`, `vcpus`,
`pool.maxActive`, etc.). For anything richer than the rendered template
covers, set `config.existingConfigMap` and ship your own ConfigMap with
a key `worker.toml.tmpl` (the init container will still envsubst
`$NODE_IP`).

## Multi-arch nodes

The worker image ships per-architecture (`linux/amd64` and
`linux/arm64`). A single release picks the right manifest per node via
the manifest list. The ConfigMap sets both `agent_linux_amd64` and
`agent_linux_arm64` paths; the worker opens only the one matching its
own architecture at microvm-launch time, so the missing file on the
other arch is harmless.

## Values reference

See [`values.yaml`](values.yaml).
