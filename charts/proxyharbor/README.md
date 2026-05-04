# ProxyHarbor Helm chart

The default chart values are optimized for a lightweight single-instance deployment:

- `replicaCount: 1`
- `storage: sqlite`
- `sqlite.persistence.enabled: true`
- Redis is optional by default (`redis.selectorRedisRequired: false`)

This profile is intended for one Pod with a `ReadWriteOnce` PVC. It is not a shared-state multi-replica mode.

For HA, use `examples/dynamic-ha-values.yaml` as the stable v0.5.5 / v1.0-readiness starting point. It keeps MySQL as shared storage, Redis as the required zfair/cache coordination layer, and enables the cluster heartbeat/maintenance loop used by the current HA correctness runners.

The verified HA shape is:

- `replicaCount: 3`
- `storage: mysql`
- `redis.addr` configured
- `redis.selectorRedisRequired: true`
- `cluster.enabled: true`
- explicit Secrets for `admin-key`, `pepper`, and `mysql-dsn`

Use `examples/multi-instance-values.yaml` only if you are intentionally exploring a different profile and can validate it separately; it is not the documented v0.5.5 / v1.0-readiness release baseline.

Helm CI should stay lightweight: `helm lint` plus an HA example smoke render with `charts/proxyharbor/examples/dynamic-ha-values.yaml` are enough. Runtime correctness and soak evidence belong to the formal HA runners described in [`docs/runbooks/ha-pressure.md`](../../docs/runbooks/ha-pressure.md), not to chart CI. The verified HA evidence currently covers the `500` concurrency / `10m` mixed-soak availability gate; strict p95/p99 latency closure remains a P1 follow-up.

`storage: memory` remains available for dev, demo, and CI only. It is not durable and should not be used as a formal deployment profile.
