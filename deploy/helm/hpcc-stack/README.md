# hpcc-stack

Demo umbrella chart that brings up [hpcc](https://github.com/aarani/hpcc) with a
Grafana-flavoured observability stack in a single `helm install`:

| Concern  | Backend |
| -------- | ------- |
| Metrics  | Prometheus (via `kube-prometheus-stack`) |
| Traces   | Tempo |
| Logs     | Loki (single-binary) + Promtail (DaemonSet) |
| Routing  | OpenTelemetry Collector (contrib) |
| Dashboards | Grafana, with Prometheus + Loki + Tempo datasources pre-provisioned |
| OIDC IdP | Keycloak (off by default — needs realm seeding) |

Plus the two in-tree hpcc charts as sub-dependencies:
- `hpcc-scheduler` (Deployment + Service)
- `hpcc-worker` (privileged DaemonSet, `/dev/kvm`)

Not for production. Use it to evaluate hpcc end-to-end in a single
cluster. For production, run `hpcc-scheduler` and `hpcc-worker`
directly against your existing platform.

## Install

```bash
# 1. Register the external chart repos (one-time per machine).
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm repo add grafana               https://grafana.github.io/helm-charts
helm repo add open-telemetry        https://open-telemetry.github.io/opentelemetry-helm-charts
helm repo add bitnami               https://charts.bitnami.com/bitnami
helm repo update

# 2. Pull the dependencies pinned by Chart.lock. (Use `dependency update`
#    instead to ignore the lock and resolve fresh — useful for bumping.)
helm dependency build ./deploy/helm/hpcc-stack

# 3. Install. Self-signed TLS + a random worker_token are generated at
#    template time and persisted in Secrets across upgrades.
helm install demo ./deploy/helm/hpcc-stack \
  --namespace hpcc --create-namespace
```

First install is heavy — kube-prometheus-stack alone has ~10 pods, plus
loki/tempo/promtail/otel-collector/scheduler/worker on top. Allow a
minute or two for everything to settle.

## What gets wired up

- hpcc emits **Prometheus metrics** on `/metrics`. The `ServiceMonitor`
  resources in the hpcc subcharts target the Prometheus that
  kube-prometheus-stack installs (selector wildcards are loosened so
  any ServiceMonitor in the namespace is picked up).
- hpcc emits **OTLP traces + metrics** over gRPC to
  `otel-collector-opentelemetry-collector:4317`. The Collector exports
  traces to Tempo and metrics to Prometheus via `remote_write`
  (Prometheus has `enableRemoteWriteReceiver: true`).
- **Container logs** are tailed by Promtail (DaemonSet) and shipped to
  Loki. Grafana's Loki datasource is pre-provisioned; filter to this
  release with `{app_kubernetes_io_instance="demo"}`.
- The scheduler and worker share an **auto-generated worker_token** via
  the `hpcc-shared-auth` Secret.
- TLS material is auto-generated: one self-signed CA
  (`hpcc-stack-ca`), one leaf for the scheduler (with cluster-DNS
  SANs), one for the worker. Workers mount the CA at
  `/etc/hpcc/ca/ca.crt` and reference it via `scheduler.ca_file`.

## Access

```bash
# Grafana (admin / admin)
kubectl -n hpcc port-forward svc/kps-grafana 3000:80

# Scheduler (TLS, gRPC)
kubectl -n hpcc port-forward svc/scheduler 9091:9091

# Pull the generated CA for client trust
kubectl -n hpcc get secret hpcc-stack-ca \
  -o jsonpath='{.data.ca\.crt}' | base64 -d > hpcc-stack-ca.pem
```

## Restricting workers to KVM-capable nodes

Workers will CrashLoopBackOff on nodes without `/dev/kvm`. Label the
nodes that do have it and set the selector:

```yaml
hpcc-worker:
  nodeSelector:
    hpcc.io/kvm: "true"
```

## Customising

The full surface area of each subchart is reachable through values.
Override per-subchart at the umbrella level:

```yaml
hpcc-scheduler:
  config:
    tenants:
      - id: acme
        issuer: https://...

kube-prometheus-stack:
  grafana:
    adminPassword: a-real-password

tempo:
  enabled: false   # disable an entire backend
```

## Keycloak realm seeding

Keycloak comes up with a pre-imported `hpcc-demo` realm — the umbrella
ships [files/hpcc-demo-realm.json](files/hpcc-demo-realm.json) and
mounts it into bitnami's `keycloak-config-cli` job, which imports it
once Keycloak is healthy. The hpcc-scheduler chart's tenant block is
pre-wired to match.

What's in the realm:

| Subject | Value |
| ------- | ----- |
| Client ID | `hpcc-cli` |
| Client secret | `hpcc-demo-secret-change-me` (rotate before exposing) |
| User | `demo` / `demo` |
| Audience mapper | adds `aud=hpcc` to every access token |
| Hardcoded claim | `tenant_id=hpcc-demo` in every access token |

`KC_HOSTNAME` is pinned to the in-cluster service name so the token's
`iss` claim is stable regardless of how clients reach Keycloak.

### Acquiring a token

Run from inside the cluster — that way the token endpoint Keycloak
returns matches the issuer hpcc-scheduler validates against:

```bash
kubectl -n hpcc run -it --rm token --image=curlimages/curl --restart=Never -- \
  sh -c 'curl -s -X POST http://keycloak/realms/hpcc-demo/protocol/openid-connect/token \
    -d grant_type=password -d username=demo -d password=demo \
    -d client_id=hpcc-cli -d client_secret=hpcc-demo-secret-change-me'
```

For headless flows (CI, scripted tests) use `grant_type=client_credentials`
with just the client_id + client_secret.

### Editing the realm

Edit [files/hpcc-demo-realm.json](files/hpcc-demo-realm.json) and run
`helm upgrade`. The keycloak-config-cli job re-runs and reconciles the
live realm against the file — adding clients, rotating secrets,
granting roles all flow through.

To disable Keycloak entirely, set `keycloak.enabled: false` and replace
`hpcc-scheduler.config.tenants` with at least one entry pointing at
your own IdP (hpcc requires at least one tenant to start).

## Limitations

- `enableRemoteWriteReceiver: true` on the demo Prometheus is fine for
  evaluation but not how you'd ingest metrics in production — prefer
  the OTel Collector → Prometheus federation, or Mimir/Cortex.
- The Loki SingleBinary deployment has no persistence — pod restart
  drops logs.
- TLS hostname coverage on the worker cert is loose (it can't
  enumerate node IPs at install time). External clients dialing
  workers directly need an `InsecureSkipVerify` dialer or a custom
  trust chain.
- Heavy: kube-prometheus-stack alone needs a few CPU cores and ~2 GiB
  RAM headroom on the cluster. Single-node kind clusters work but
  expect tight resource pressure.
