# hpcc-scheduler

Helm chart for the [hpcc](https://github.com/aarani/hpcc) scheduler — the gRPC
control plane that routes Compile RPCs from clients to workers.

## Install

```bash
helm install hpcc-scheduler ./deploy/helm/hpcc-scheduler \
  --namespace hpcc --create-namespace \
  --set auth.workerToken="$(openssl rand -hex 32)" \
  --set-file tls.cert=scheduler.crt \
  --set-file tls.key=scheduler.key
```

Workers and clients then dial
`hpcc-scheduler.hpcc.svc:9091` from inside the cluster. To expose externally,
switch `service.type` to `LoadBalancer` or front it with a gRPC-capable
ingress.

## Bring-your-own Secrets / ConfigMaps

Putting secret material in `values.yaml` is fine for dev. For production,
provision the Secret out of band and point the chart at it:

```yaml
tls:
  existingSecret: hpcc-scheduler-tls   # must have tls.crt + tls.key

auth:
  existingSecret: hpcc-scheduler-auth  # must have a key with the token
  existingSecretKey: WORKER_TOKEN
```

The scheduler config itself sits in a chart-rendered ConfigMap. If you'd
rather manage `scheduler.toml` yourself, set `config.existingConfigMap`.

## Multi-tenant OIDC

Tenant entries get rendered into `[[tenant]]` blocks one-for-one:

```yaml
config:
  tenants:
    - id: acme
      issuer: https://auth.acme.com/
      jwksUrl: https://auth.acme.com/.well-known/jwks.json
      tokenUrl: https://auth.acme.com/oauth/token
      audience: hpcc
```

For exotic config that isn't covered by the rendered template, use
`config.existingConfigMap` and supply the full TOML yourself.

## Pairing with workers

The worker chart needs the same `worker_token` value the scheduler is
configured with. After install, recover it from the secret:

```bash
kubectl -n hpcc get secret <release>-hpcc-scheduler-auth \
  -o jsonpath='{.data.WORKER_TOKEN}' | base64 -d
```

Feed that into `auth.workerToken` of the `hpcc-worker` chart (or share the
same Secret across both releases via `existingSecret`).

## Values reference

See [`values.yaml`](values.yaml) for the full list with defaults and
inline documentation.
