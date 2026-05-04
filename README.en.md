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

ProxyHarbor is a single-binary proxy-pool control plane and gateway. It provides global Provider / Proxy management, dynamic tenant keys, lease issuance, HTTP/HTTPS gateway validation, and SQLite-backed single-node persistence. The product boundary remains **single-first**: local and small deployments do not require MySQL, Redis, or hand-written secrets by default; switch to MySQL + Redis only when HA is needed.

## Features

- **Single-first runtime**: defaults to `role=all`, `storage=sqlite`, and `selector=local` in one process.
- **Zero-config local start**: missing `PROXYHARBOR_ADMIN_KEY` / `PROXYHARBOR_KEY_PEPPER` values are generated once and persisted to `secrets.env`.
- **Global proxy inventory**: Admin manages Providers and Proxies; tenants receive leases and never read proxy endpoint lists.
- **Dynamic tenant keys**: Admin APIs issue and revoke tenant keys; plaintext keys are returned only once.
- **Lease gateway**: leases bind to `resource_ref`; gateway targets must match the leased resource.
- **Local smooth weighted round-robin**: single-node mode uses an in-process selector; HA can use Redis-backed zfair.
- **SQLite operations loop**: built-in `doctor`, `init`, `backup`, `restore`, and `retention` commands.
- **Go SDK**: lease caching, auto-renew/reacquire, and `WithLocalDefaults()` for local single-node setup.

## Quick Start

> The recommended local and small-deployment path is the default Docker Compose profile: SQLite persistence, local selector, and automatically generated local secrets.

### Shortest Single-Node Path

The default single-node path takes only three commands:

```bash
docker compose up -d --build
mkdir -p data
docker compose exec -T proxyharbor cat /var/lib/proxyharbor/secrets.env > data/secrets.env
```

Then import the SDK in your application and use local defaults:

```go
client, err := proxyharbor.New(proxyharbor.WithLocalDefaults())
```

This path uses `sqlite` persistence, an in-process selector, and generated local Admin Key / pepper by default. It does not require MySQL, Redis, or hand-written secrets. See steps 4-5 for the complete SDK example.

### 1. Start the single-node service

```bash
docker compose up -d --build
```

This starts:
- `proxyharbor`: combined control plane and gateway
- SQLite database: `/var/lib/proxyharbor/proxyharbor.db`
- generated local secrets: `/var/lib/proxyharbor/secrets.env`

### 2. Check readiness

```bash
curl http://localhost:18080/readyz
```

### 3. Prepare local secrets for the Go SDK

The default single-node service generates `/var/lib/proxyharbor/secrets.env` inside the container. Copy it to `data/secrets.env` in your application working directory so the SDK can read it with `WithLocalDefaults()`:

```bash
mkdir -p data
docker compose exec -T proxyharbor cat /var/lib/proxyharbor/secrets.env > data/secrets.env
```

PowerShell:

```powershell
New-Item -ItemType Directory -Force data | Out-Null
docker compose exec -T proxyharbor cat /var/lib/proxyharbor/secrets.env | Out-File -Encoding utf8 data/secrets.env
```

### 4. Add the Go SDK

```bash
go get github.com/kamill7779/proxyharbor/sdks/go/proxyharbor
```

### 5. Add a proxy and use it

This is the shortest all-default loop: connect to `http://localhost:18080` by default, read the local Admin Key from `data/secrets.env`, add one proxy with the SDK, then get a proxy URL ready for an HTTP client.

```go
package main

import (
    "context"
    "fmt"
    "log"

    "github.com/kamill7779/proxyharbor/sdks/go/proxyharbor"
)

func main() {
    ctx := context.Background()

    client, err := proxyharbor.New(proxyharbor.WithLocalDefaults())
    if err != nil {
        log.Fatal(err)
    }

    if _, err := client.AddProxy(ctx, "http://proxy1.example.com:8080"); err != nil {
        log.Fatal(err)
    }

    proxyURL, err := client.GetProxyURL(ctx)
    if err != nil {
        log.Fatal(err)
    }

    fmt.Println(proxyURL)
}
```

> For loopback or private-network proxy endpoints in local testing, set `PROXYHARBOR_ALLOW_INTERNAL_PROXY_ENDPOINT=true` before startup.

### 6. Explicit local configuration (optional)

```go
client, err := proxyharbor.New(
    proxyharbor.WithBaseURL("http://localhost:18080"),
    proxyharbor.WithSecretsFile("./data/secrets.env"),
)
```
## API Shape

| Resource | Description | Typical endpoint |
| --- | --- | --- |
| Tenant | Tenant identity boundary | `POST /admin/tenants` |
| Tenant Key | Tenant credential | `POST /admin/tenants/{id}/keys` |
| Provider | Global proxy source | `POST /v1/providers` |
| Proxy | Global proxy node | `POST /v1/proxies` |
| Lease | Tenant-scoped proxy lease | `POST /v1/leases` |
| Gateway Validate | Validate lease and target | `GET /v1/gateway/validate` |

## Startup Modes

### SQLite single-node (default)

```bash
docker compose up -d --build
```

Best for local development, small deployments, and single-node services. Redis is not required. Missing `PROXYHARBOR_ADMIN_KEY` and `PROXYHARBOR_KEY_PEPPER` values are generated automatically.

### External MySQL + Redis (HA)

```bash
export PROXYHARBOR_ADMIN_KEY=$(openssl rand -hex 32)
export PROXYHARBOR_KEY_PEPPER=$(openssl rand -hex 32)
export PROXYHARBOR_MYSQL_DSN='ph:REPLACE_ME@tcp(mysql.svc:3306)/proxyharbor?parseTime=true&loc=UTC'
export PROXYHARBOR_REDIS_ADDR='redis:6379'
docker compose -f docker-compose.ha.yaml up -d --build
```

HA mode requires explicit secrets and does not auto-generate defaults.

### Local HA verification path (v0.5.5)

To repeatedly validate the local 3 instance + MySQL + Redis + LB HA topology, use `docker-compose.ha-test.yaml` and the formal runners instead of ad-hoc scripts:

```bash
docker build --pull=false -t proxyharbor:ha-test .
go run ./tools/haruntimecheck -docker -docker-skip-build -timeout 8m
go run ./tools/hacorrect -docker -timeout 6m
go run ./tools/hacachecheck -docker -docker-skip-build -timeout 6m
go -C tools/hasdkcheck run . -docker -samples 500 -disable-samples 100 -concurrency 16 -timeout 8m
go run ./tools/hapressure -docker -docker-skip-build -docker-internal -mode pressure -operations gateway_validate -concurrency 500 -samples-per-op 500 -warmup-leases 500 -timeout 20m
go run ./tools/hapressure -docker -docker-skip-build -docker-internal -mode pressure -operations lease_create -concurrency 500 -samples-per-op 500 -warmup-leases 500 -timeout 20m
go run ./tools/hapressure -docker -docker-skip-build -docker-internal -mode pressure -operations lease_renew -concurrency 500 -samples-per-op 500 -warmup-leases 500 -timeout 20m
go run ./tools/hapressure -docker -docker-skip-build -docker-internal -mode soak -concurrency 500 -duration 10m -warmup-leases 500 -timeout 20m
```

`-docker-internal` runs the pressure worker inside the compose network and avoids host port-publishing connection-refusal noise on Docker Desktop / Windows / macOS. See [HA pressure runbook](docs/runbooks/ha-pressure.md) for pressure and soak result formatting.

Current HA hot-path evidence from the local Docker topology (3 ProxyHarbor instances + MySQL + Redis + nginx, `-docker-internal`):

| Scenario | Result |
| --- | --- |
| `500` concurrency, `10m` mixed soak | `1,805,539` control-plane requests, about `3.0k req/s`, error rate `0.252%`, meeting the `<0.5%` soak gate |
| Status distribution | `200=1,200,818`, `201=600,165`, `409=2,121`, `500=52`, `504=2,383`, no `502` |
| `500` concurrency, `2m` mixed soak | `554,842` control-plane requests, about `4.6k req/s`, error rate `0.139%`, no `500/502` |
| `lease_create` 500 concurrency burst | `500/500` succeeded, p50/p95/p99 = `256/318/322ms` |

These numbers measure only control-plane hot paths such as lease creation, renewal, and gateway validation. They do not represent external proxy data-plane throughput. The `500` concurrency, `10m` mixed-soak availability gate is met; strict per-operation p95/p99 latency gates remain a v1.0 follow-up P1 performance item.

### Local binary

```bash
go run ./cmd/proxyharbor \
  -storage sqlite \
  -sqlite-path data/proxyharbor.db
```

## CLI Operations

```bash
proxyharbor doctor [flags]
proxyharbor init [flags]
proxyharbor backup [flags]
proxyharbor restore [flags]
proxyharbor retention [flags]
```

- `doctor`: checks storage, SQLite schema, Redis requirements, admin key / pepper, and path permissions.
- `init`: initializes SQLite schema idempotently.
- `backup` / `restore`: SQLite single-node backup and restore.
- `retention`: cleans audit and usage events; default is dry-run, `--execute` deletes rows.

## Helm Secret

Helm remains production Secret-first and never renders plaintext credentials in templates:

```bash
kubectl create secret generic proxyharbor-credentials \
  --from-literal=admin-key="$(openssl rand -hex 32)" \
  --from-literal=pepper="$(openssl rand -hex 32)"

helm install proxyharbor charts/proxyharbor \
  --set auth.existingSecret=proxyharbor-credentials
```

For HA examples:

```bash
helm install proxyharbor charts/proxyharbor -f charts/proxyharbor/examples/dynamic-ha-values.yaml
```

## Data Model

### Provider

A global platform resource representing a proxy source or datacenter. Proxies without `provider_id` are assigned to the default Provider.

### Proxy

A global platform resource containing endpoint, weight, health score, circuit-breaker state, and health feedback fields. Tenant APIs do not expose proxy endpoint lists.

### Policy

The current release focuses on the default policy. `policy_id` can be omitted when creating leases.

### Lease

A tenant resource binding subject, resource_ref, proxy_id, expiry, and an irreversible password hash. The plaintext password is returned only once in the create response.

## Scheduling & Health Model

### local selector

The default single-node selector. It keeps smooth weighted round-robin state in-process and only selects proxies that are healthy, positively weighted, have a positive health score, and are not in an open circuit window.

### zfair selector

HA deployments can use Redis ZSET + Lua atomic selection, combining weight, latency, and health state for shared scheduling.

### Health Scoring

Success raises the health score. Connection failures, timeouts, auth failures, and protocol failures lower it. Consecutive failures trip the circuit breaker and enter cooldown.

## Configuration Reference

| Variable | Description |
| --- | --- |
| `PROXYHARBOR_STORAGE` | `sqlite` by default for single-node, `mysql` for HA, `memory` for dev/demo/CI |
| `PROXYHARBOR_SQLITE_PATH` | SQLite database path |
| `PROXYHARBOR_SECRETS_FILE` | Env-style local secrets file; defaults next to the SQLite DB |
| `PROXYHARBOR_AUTO_SECRETS` | Generate missing admin key/pepper for SQLite single-node mode; default `true` |
| `PROXYHARBOR_ADMIN_KEY` | Bootstrap admin key, at least 32 bytes; auto-generated for single-node mode |
| `PROXYHARBOR_KEY_PEPPER` | Tenant key hash pepper, at least 32 bytes; auto-generated for single-node mode |
| `PROXYHARBOR_MYSQL_DSN` | MySQL DSN, required when `storage=mysql` |
| `PROXYHARBOR_REDIS_ADDR` | Redis address, required by HA/zfair |
| `PROXYHARBOR_SELECTOR` | `local` or `zfair` |
| `PROXYHARBOR_SELECTOR_REDIS_REQUIRED` | Whether selector startup requires Redis |
| `PROXYHARBOR_AUTH_REFRESH_INTERVAL` | Dynamic key refresh interval, max 5s |
| `PROXYHARBOR_ALLOW_INTERNAL_PROXY_ENDPOINT` | Whether private/loopback proxy endpoints can be registered |

## Packaging & Deployment

```bash
docker build -t proxyharbor:local .
helm install proxyharbor charts/proxyharbor --set auth.existingSecret=proxyharbor-credentials
```

## Contributing

Issues and PRs are welcome. Single-node capabilities should stay lightweight, reproducible, and easy to verify. HA capabilities should keep explicit secrets and clear cloud-native deployment boundaries.

## Contact

- GitHub Issues: <https://github.com/kamill7779/proxyharbor/issues>
- Author: Kamill

## License

This project is licensed under the [MIT License](LICENSE).

