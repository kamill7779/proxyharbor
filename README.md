# ProxyHarbor

ProxyHarbor is a lightweight proxy-pool gateway for small cloud-native workloads. It provides admin-managed providers/proxies, tenant keys, lease APIs, and HTTP/HTTPS gateway validation in one binary.

## Deployment Profiles

ProxyHarbor v0.4.1 is single-first:

- **Single instance (default)**: one `proxyharbor` process, `role=all`, `storage=sqlite`, no Redis requirement. This is the recommended local and small deployment shape.
- **HA**: multiple instances with shared MySQL plus Redis for zfair selector coordination and auth/cache invalidation.
- **Memory**: dev/demo/CI only. It is non-durable and is not a formal deployment profile.

## Quick Start: Single Instance

Start the lightweight single-node profile with one command. It uses SQLite persistence, does not require MySQL or Redis, and maps the service to localhost:18080 to avoid common local port conflicts.

**PowerShell:**

```powershell
$env:PROXYHARBOR_HOST_PORT="18080"; $env:PROXYHARBOR_ADMIN_KEY="dev-admin-key-min-32-chars-long!!!!"; $env:PROXYHARBOR_KEY_PEPPER="dev-key-pepper-min-32-bytes-random!!!!"; docker compose up -d --build --pull never; Invoke-RestMethod http://localhost:18080/readyz
```

**bash:**

```bash
PROXYHARBOR_HOST_PORT=18080 PROXYHARBOR_ADMIN_KEY=dev-admin-key-min-32-chars-long!!!! PROXYHARBOR_KEY_PEPPER=dev-key-pepper-min-32-bytes-random!!!! docker compose up -d --build --pull never
curl http://localhost:18080/readyz
```

Expected readiness:

```json
{"status":"ready","reasons":{"auth_cache":"ok","sqlite":"ok"}}
```

The default `docker-compose.yaml` runs one `proxyharbor` service and stores SQLite data at `/var/lib/proxyharbor/proxyharbor.db` in the `proxyharbor-data` volume. For local loopback/private proxy tests, add `PROXYHARBOR_ALLOW_INTERNAL_PROXY_ENDPOINT=true` before `docker compose up`.

For HA, use the MySQL+Redis compose file instead of the single-instance default:

```bash
docker compose -f docker-compose.ha.yaml up -d --build
```

## Basic API Flow

Create a tenant and issue a tenant key:

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

Add a proxy and create a lease:

```bash
curl -H "ProxyHarbor-Key: $PROXYHARBOR_ADMIN_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"id":"proxy-1","endpoint":"http://proxy1.example.com:8080","healthy":true,"weight":10}' \
  http://localhost:8080/v1/proxies

curl -H "ProxyHarbor-Key: $TENANT_KEY" \
  -H 'Idempotency-Key: demo-lease-1' \
  -H 'Content-Type: application/json' \
  -d '{"subject":{"subject_type":"workload","subject_id":"app-1"},"resource_ref":{"kind":"url","id":"https://example.com"}}' \
  http://localhost:8080/v1/leases
```

For loopback or private proxy endpoints in local tests, set `PROXYHARBOR_ALLOW_INTERNAL_PROXY_ENDPOINT=true`.

## CLI

```bash
proxyharbor doctor [flags]
proxyharbor init [flags]
proxyharbor [server flags]
```

`doctor` checks storage driver selection, required environment/config, MySQL DSN presence, Redis required settings, admin key/pepper presence and length, and SQLite path parent writability without printing secret values.

`init` initializes the SQLite schema and is idempotent. It does not generate admin keys and does not write secrets to logs. MySQL migrations remain explicit via `migrations/mysql/init.sql`.

## Helm

The chart defaults to a single replica with SQLite persistence enabled:

```bash
helm install proxyharbor charts/proxyharbor \
  --set auth.existingSecret=proxyharbor-credentials
```

Create `proxyharbor-credentials` with `admin-key` and `pepper`. For HA, use MySQL+Redis examples:

```bash
helm install proxyharbor charts/proxyharbor -f charts/proxyharbor/examples/dynamic-ha-values.yaml
```

HA examples require a Secret with `admin-key`, `pepper`, `mysql-dsn`, and optional `redis-password`.

## Configuration Reference

| Variable | Description |
| --- | --- |
| `PROXYHARBOR_STORAGE` | `sqlite` for single instance, `mysql` for HA, `memory` for dev/demo/CI |
| `PROXYHARBOR_SQLITE_PATH` | SQLite database path for single-instance storage |
| `PROXYHARBOR_MYSQL_DSN` | MySQL DSN, required when `storage=mysql` |
| `PROXYHARBOR_REDIS_ADDR` | Redis address; required when zfair Redis is enforced |
| `PROXYHARBOR_SELECTOR_REDIS_REQUIRED` | Whether selector startup requires Redis |
| `PROXYHARBOR_ADMIN_KEY` | Bootstrap admin key, at least 32 bytes |
| `PROXYHARBOR_KEY_PEPPER` | Tenant key hash pepper, at least 32 bytes |
| `PROXYHARBOR_AUTH_REFRESH_INTERVAL` | Dynamic key refresh interval, max 5s |
| `PROXYHARBOR_ALLOW_INTERNAL_PROXY_ENDPOINT` | Whether private/loopback proxy endpoints can be registered |

## License

This project is licensed under the [MIT License](LICENSE).
