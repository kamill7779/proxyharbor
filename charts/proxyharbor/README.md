# ProxyHarbor Helm chart

The default chart values are optimized for a lightweight single-instance deployment:

- `replicaCount: 1`
- `storage: sqlite`
- `sqlite.persistence.enabled: true`
- Redis is optional by default (`redis.selectorRedisRequired: false`)

This profile is intended for one Pod with a `ReadWriteOnce` PVC. It is not a shared-state multi-replica mode.

For HA, use `examples/dynamic-ha-values.yaml` or `examples/multi-instance-values.yaml`; both keep MySQL as shared storage and Redis as the required zfair/cache coordination layer.

`storage: memory` remains available for dev, demo, and CI only. It is not durable and should not be used as a formal deployment profile.
