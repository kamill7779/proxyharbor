<div align="center">
  <img src="docs/logo.png" alt="ProxyHarbor Logo" width="480"/>
</div>

<div align="center">

**A lightweight cloud-native proxy pool for small-business integration**

[![Go Version](https://img.shields.io/badge/Go-1.23+-00ADD8?logo=go)](go.mod)
[![License](https://img.shields.io/badge/License-MIT-orange)](LICENSE)
[![中文](https://img.shields.io/badge/README-中文-blue)](README.md)

</div>

---

ProxyHarbor v0.2.0 is a breaking reset. It keeps the runtime footprint light with **MySQL + Redis** while providing global proxy inventory, dynamic tenant keys, lease issuance, and an HTTP/HTTPS gateway. Providers, proxies, and policies are global platform resources. Tenants can only create and use leases through dynamic keys; they cannot read proxy endpoint lists.

## Features

- **Global proxy inventory**: Admin manages Providers and Proxies; tenants share the global healthy proxy pool.
- **Dynamic tenant keys**: Admin API issues, revokes, and rotates tenant keys; plaintext keys are returned only once.
- **Default policy**: MVP only allows the `default` policy; omitted `policy_id` resolves to `default`.
- **Default provider**: Proxies without `provider_id` are assigned to the `default` provider.
- **Lease gateway**: Leases bind to `resource_ref`; gateway targets must match the leased resource host.
- **zfair scheduling**: Redis ZSET + atomic Lua selection using weight, latency, and health state.
- **Secret-first deployment**: Helm never renders plaintext credentials; production deployments reference existing Kubernetes Secrets.

## Quick Start

> Docker Compose is the recommended local path. MySQL initializes from `migrations/mysql/init.sql`.

### 1. Start all services

```bash
export PROXYHARBOR_ADMIN_KEY=$(openssl rand -hex 32)
export PROXYHARBOR_KEY_PEPPER=$(openssl rand -hex 32)
docker compose up -d --build
```

This starts:
- `mysql`: tenant keys, global proxy inventory, leases, and health-state persistence
- `redis`: zfair scheduling state and hot-path cache
- `proxyharbor`: combined controller and gateway process

### 2. Check readiness

```bash
curl http://localhost:8080/readyz
```

### 3. Create a tenant and issue a tenant key

```bash
curl -H "ProxyHarbor-Key: $PROXYHARBOR_ADMIN_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"id":"tenant-a","display_name":"Tenant A"}' \
  http://localhost:8080/admin/tenants

curl -H "ProxyHarbor-Key: $PROXYHARBOR_ADMIN_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"label":"app-1","purpose":"platform_container"}' \
  http://localhost:8080/admin/tenants/tenant-a/keys
```

### 4. Add a proxy

```bash
curl -H "ProxyHarbor-Key: $PROXYHARBOR_ADMIN_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"id":"proxy-1","endpoint":"http://proxy1.example.com:8080","healthy":true,"weight":10}' \
  http://localhost:8080/v1/proxies
```

> For loopback or private-network endpoints during local testing, set `PROXYHARBOR_ALLOW_INTERNAL_PROXY_ENDPOINT=true` first.

### 5. Create a lease

```bash
curl -H "ProxyHarbor-Key: $TENANT_KEY" \
  -H 'Idempotency-Key: demo-lease-1' \
  -H 'Content-Type: application/json' \
  -d '{"subject":{"subject_type":"workload","subject_id":"app-1"},"resource_ref":{"kind":"url","id":"https://example.com"}}' \
  http://localhost:8080/v1/leases
```

## API Shape

Admin-only:

- `POST|PATCH|DELETE /admin/tenants...`
- `POST|DELETE /admin/tenants/{id}/keys...`
- `GET|POST|PUT|DELETE /v1/providers...`
- `GET|POST|PUT|DELETE /v1/proxies...`
- `GET|POST|PUT|DELETE /v1/policies/default`
- `GET /v1/catalog/latest`
- `POST /v1/internal/usage-events:batch`
- `POST /v1/internal/gateway-feedback:batch`
- `GET /v1/gateway/validate`

Tenant:

- `POST /v1/leases`
- `POST /v1/leases/{id}:renew`
- `DELETE /v1/leases/{id}`

## Startup Modes

### Bundled dependencies (recommended for local dev)

```bash
docker compose up -d --build
```

### External MySQL + Redis

```bash
cp .env.example .env
# edit .env with your external connection details
docker compose up -d --build proxyharbor
```

### Local binary

```bash
go run ./cmd/proxyharbor \
  -admin-key "$PROXYHARBOR_ADMIN_KEY" \
  -key-pepper "$PROXYHARBOR_KEY_PEPPER" \
  -mysql-dsn "$PROXYHARBOR_MYSQL_DSN"
```

## Helm Secret

The chart is Secret-first and does not render plaintext credentials. Create the referenced Secret before install:

```bash
kubectl create secret generic proxyharbor-credentials \
  --from-literal=admin-key="$(openssl rand -hex 32)" \
  --from-literal=pepper="$(openssl rand -hex 32)" \
  --from-literal=mysql-dsn='ph:REPLACE_ME@tcp(mysql.svc:3306)/proxyharbor?parseTime=true&loc=UTC'
```


### Kubernetes Multi-Instance Deployment

The Helm chart now defaults to an HA baseline: `replicaCount=2`, RollingUpdate `maxUnavailable=0` / `maxSurge=1`, PDB enabled, graceful termination, and optional HPA. Multi-instance deployments require shared MySQL. Redis/zfair is recommended; for production multi-instance installs, set `redis.selectorRedisRequired=true` and `auth.invalidation=redis`. Memory storage is for local development only and is not suitable for multiple Pods.

Example:

```bash
helm install proxyharbor charts/proxyharbor -f charts/proxyharbor/examples/multi-instance-values.yaml
```

## Data Model

### Provider

A global platform resource. The MVP seeds the `default` provider, and proxies without `provider_id` are assigned to it.

### Proxy

A global platform resource. It contains endpoint, weight, health_score, circuit-breaker state, and health feedback fields. Tenant APIs do not expose proxy endpoint lists.

### Policy

The MVP only allows the `default` policy. Creating, updating, or deleting non-`default` policies is rejected.

### Lease

A tenant resource. It keeps `tenant_id`, binds subject, resource_ref, proxy_id, expiry, and an irreversible password hash. The plaintext password is returned only once in the create response.

## Scheduling & Health Model

### zfair Scheduler

`selector=zfair` uses Redis ZSETs and Lua to maintain ready/delayed queues. In multi-instance deployments, set `PROXYHARBOR_SELECTOR_REDIS_REQUIRED=true` to avoid split scheduling state.

### Health Scoring

Success raises the score. Connection failures, timeouts, auth/protocol failures lower it. Consecutive failures trip the circuit breaker and enter exponential back-off cooldown.

## Configuration Reference

| Variable | Description |
| --- | --- |
| `PROXYHARBOR_ADMIN_KEY` | Bootstrap admin key, at least 32 bytes |
| `PROXYHARBOR_KEY_PEPPER` | Tenant key hash pepper, at least 32 bytes |
| `PROXYHARBOR_MYSQL_DSN` | MySQL DSN, required |
| `PROXYHARBOR_REDIS_ADDR` | Redis address; required when zfair Redis is enforced |
| `PROXYHARBOR_AUTH_REFRESH_INTERVAL` | Dynamic key refresh interval, max 5s |
| `PROXYHARBOR_SELECTOR_REDIS_REQUIRED` | Whether selector startup requires Redis |
| `PROXYHARBOR_ALLOW_INTERNAL_PROXY_ENDPOINT` | Whether private/loopback proxy endpoints can be registered |

## Packaging & Deployment

```bash
docker build -t proxyharbor:0.2.0-alpha .
helm install proxyharbor charts/proxyharbor -f charts/proxyharbor/examples/dynamic-ha-values.yaml
```

## Contributing

Issues and PRs are welcome. v0.2.0 is a breaking reset and does not keep the old migration chain.

## Contact

- GitHub Issues: <https://github.com/kamill7779/proxyharbor/issues>
- Author: Kamill

## License

This project is licensed under the [MIT License](LICENSE).
