<div align="center">
  <img src="docs/logo.png" alt="ProxyHarbor Logo" width="480"/>
</div>

<div align="center">

**A lightweight proxy pool for small-business integration**

[![Go Version](https://img.shields.io/badge/Go-1.23+-00ADD8?logo=go)](go.mod)
[![License](https://img.shields.io/badge/License-MIT-orange)](LICENSE)
[![中文](https://img.shields.io/badge/README-中文-blue)](README.md)

</div>

---

ProxyHarbor is a lightweight proxy pool service designed for small-business scenarios. It combines a **control plane** (proxy registration, policy management, lease issuance) with a **data plane** (HTTP forward proxy + CONNECT tunnel) in a single process. The only runtime dependencies are **MySQL + Redis**, keeping the deployment footprint minimal.

## Features

- **Control-plane API** — manage Providers, Proxies, Policies, and Leases; read the Catalog
- **HTTP gateway** — supports both plain HTTP forward-proxy requests and CONNECT tunnels (HTTPS)
- **zfair fair scheduling** — Redis ZSET + atomic Lua scripts for weight- and health-aware lease distribution
- **Health scoring** — success +5, connection failure −10, timeout −15, auth/protocol failure −30; circuit breaker trips after 3 consecutive failures with 30 s – 5 min exponential back-off cooldown
- **Lease system** — time-limited credentials; plaintext password returned only once at creation, stored as an irreversible SHA-256 hash; idempotency key deduplication
- **Redis caching** — Catalog and Lease dual-cache to reduce MySQL hot-path pressure
- **Role separation** — `all` / `controller` / `gateway` startup roles for split deployment

## Quick Start

> Docker Compose is the recommended local path — MySQL and Redis are bundled, no extra setup needed.

### 1. Start all services

```bash
docker compose --profile bundle up -d --build
```

This starts:
- `mysql` — proxy catalog and health-state persistence
- `redis` — zfair scheduling state + Lease/Catalog cache
- `proxyharbor` — combined controller and gateway process

Default development API key:

```env
PROXYHARBOR_AUTH_KEY=change-me
```

**Before exposing the service externally, set a strong key:**

```bash
cp .env.example .env
```

```env
PROXYHARBOR_AUTH_KEY=replace-with-a-random-secret
PROXYHARBOR_MYSQL_DSN=proxyharbor:proxyharbor@tcp(mysql:3306)/proxyharbor?parseTime=true&loc=UTC
PROXYHARBOR_REDIS_ADDR=redis:6379
```

### 2. Check readiness

```bash
curl http://localhost:8080/readyz
```

```json
{"role":"all","status":"ready"}
```

### 3. Register a provider

```bash
curl -H 'ProxyHarbor-Key: change-me' \
  -H 'Content-Type: application/json' \
  -d '{"id":"static-main","type":"static","name":"My proxy pool","enabled":true}' \
  http://localhost:8080/v1/providers
```

### 4. Add a proxy

```bash
curl -H 'ProxyHarbor-Key: change-me' \
  -H 'Content-Type: application/json' \
  -d '{"id":"proxy-1","provider_id":"static-main","endpoint":"http://proxy1.example.com:8080","healthy":true,"weight":10}' \
  http://localhost:8080/v1/proxies
```

> For loopback or private-network endpoints during local testing, set `PROXYHARBOR_ALLOW_INTERNAL_PROXY_ENDPOINT=true` first.

Bulk add (Bash):

```bash
for item in \
  'proxy-1 http://proxy1.example.com:8080 100' \
  'proxy-2 http://proxy2.example.com:8080 80' \
  'proxy-3 http://proxy3.example.com:8080 50'
do
  set -- $item
  curl -H 'ProxyHarbor-Key: change-me' \
    -H 'Content-Type: application/json' \
    -d "{\"id\":\"$1\",\"provider_id\":\"static-main\",\"endpoint\":\"$2\",\"healthy\":true,\"weight\":$3}" \
    http://localhost:8080/v1/proxies
done
```

Bulk add (PowerShell):

```powershell
$proxies = @(
  @{ id = 'proxy-1'; endpoint = 'http://proxy1.example.com:8080'; weight = 100 },
  @{ id = 'proxy-2'; endpoint = 'http://proxy2.example.com:8080'; weight = 80 },
  @{ id = 'proxy-3'; endpoint = 'http://proxy3.example.com:8080'; weight = 50 }
)
foreach ($proxy in $proxies) {
  $body = @{ id=$proxy.id; provider_id='static-main'; endpoint=$proxy.endpoint; healthy=$true; weight=$proxy.weight } | ConvertTo-Json -Compress
  Invoke-RestMethod -Method Post -Uri 'http://localhost:8080/v1/proxies' -Headers @{'ProxyHarbor-Key'='change-me'} -ContentType 'application/json' -Body $body
}
```

### 5. Create a policy

A lease requires at least one enabled policy:

```bash
curl -H 'ProxyHarbor-Key: change-me' \
  -H 'Content-Type: application/json' \
  -d '{"id":"default","name":"Default policy","enabled":true,"ttl_seconds":1800}' \
  http://localhost:8080/v1/policies
```

### 6. Create a lease

```bash
curl -H 'ProxyHarbor-Key: change-me' \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: demo-lease-1' \
  -d '{"subject":{"subject_type":"user","subject_id":"local-dev"},"resource_ref":{"kind":"url","id":"https://example.com"},"ttl_seconds":600}' \
  http://localhost:8080/v1/leases
```

Use the returned `lease_id` as the proxy username and `password` as the proxy password (**returned only once — save it immediately**).

## Startup Modes

### Bundled dependencies (recommended for local dev)

```bash
docker compose --profile bundle up -d --build
```

### External MySQL + Redis

```bash
cp .env.example .env
# edit .env with your external connection details
docker compose --profile app up -d --build
```

### Local binary

```bash
PROXYHARBOR_AUTH_KEY=replace-with-a-random-secret \
PROXYHARBOR_STORAGE=mysql \
PROXYHARBOR_MYSQL_DSN='proxyharbor:proxyharbor@tcp(127.0.0.1:3306)/proxyharbor?parseTime=true&loc=UTC' \
PROXYHARBOR_REDIS_ADDR='127.0.0.1:6379' \
PROXYHARBOR_SELECTOR=zfair \
go run ./cmd/proxyharbor
```

## Data Model

### Provider

Groups proxies from the same source. Use `type: static` for a manually managed pool:

```json
{"id":"static-main","type":"static","name":"My proxy pool","enabled":true}
```

### Proxy

Describes a single upstream proxy endpoint:

| Field | Description |
| --- | --- |
| `id` | Unique ID within the tenant |
| `provider_id` | Owning provider |
| `endpoint` | Upstream proxy URL |
| `healthy` | Whether the proxy is eligible for scheduling |
| `weight` | Relative scheduling weight for zfair — higher means more leases |
| `health_score` | Health score maintained automatically by gateway feedback |
| `circuit_open_until` | Circuit-breaker recovery timestamp |
| `latency_ewma_ms` | Latency exponential weighted moving average (ms) |
| `labels` | Extension labels for future policy filtering |

### Lease

Each lease binds **one proxy node** and issues time-limited credentials for gateway authentication:

- Password stored as SHA-256 hash; plaintext returned only in `CreateLease` response
- Idempotency key prevents duplicate issuance
- `RenewLease` extends expiry by 30 minutes; `RevokeLease` revokes immediately

## Scheduling & Health Model

### zfair Scheduler

- Maintains **ready** and **delayed** queues as Redis ZSETs
- All operations (candidate registration, cooldown promotion, proxy selection, virtual-runtime update) are atomic via Lua scripts
- Distributes leases fairly under concurrency according to weight and health signals
- **Refuses to start** when Redis is unavailable in production — no silent fallback

### Health Scoring

| Event | Score delta |
| --- | --- |
| Request success | +5 |
| Unknown failure | −5 |
| Connection failure | −10 |
| Timeout | −15 |
| Auth failure | −30 |
| Protocol error | −30 |

Three consecutive failures trip the circuit breaker. Base cooldown is 30 s, maximum is 5 min (exponential back-off). Use `PROXYHARBOR_SCORING_PROFILE` to switch between `default`, `aggressive`, and `lenient` profiles.

## Configuration Reference

| Variable | Description | Default |
| --- | --- | --- |
| `PROXYHARBOR_AUTH_KEY` | Shared key for control-plane API calls | **required** |
| `PROXYHARBOR_ROLE` | Process role: `all` / `controller` / `gateway` | `all` |
| `PROXYHARBOR_STORAGE` | Storage driver: `mysql` / `memory` | `mysql` |
| `PROXYHARBOR_MYSQL_DSN` | MySQL connection string | empty |
| `PROXYHARBOR_REDIS_ADDR` | Redis address | empty |
| `PROXYHARBOR_SELECTOR` | Proxy selector name | `zfair` |
| `PROXYHARBOR_SELECTOR_REDIS_REQUIRED` | Refuse startup when zfair has no Redis | `true` |
| `PROXYHARBOR_SCORING_PROFILE` | Health scoring profile: `default` / `aggressive` / `lenient` | `default` |
| `PROXYHARBOR_ZFAIR_QUANTUM` | Base virtual-runtime increment | `1000` |
| `PROXYHARBOR_ZFAIR_DEFAULT_LATENCY_MS` | Default latency for proxies without EWMA data (ms) | `200` |
| `PROXYHARBOR_ZFAIR_MAX_PROMOTE` | Max delayed proxies promoted before each selection | `128` |
| `PROXYHARBOR_ALLOW_INTERNAL_PROXY_ENDPOINT` | Allow loopback/private addresses (testing only) | `false` |

See `.env.example` for a fully commented template.

## Packaging & Deployment

- `Dockerfile` — builds a static Go binary
- `docker-compose.yaml` — local all-in-one or app-only development
- `charts/proxyharbor` — Helm chart for Kubernetes
- `migrations/mysql/` — database initialization SQL

## Contributing

Issues and pull requests are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines.

## Contact

| Channel | Address |
| --- | --- |
| Telegram | [@kamill7779](https://t.me/kamill7779) |
| Email | [kamill7779@outlook.com](mailto:kamill7779@outlook.com) |

## License

This project is licensed under the [MIT License](LICENSE).
