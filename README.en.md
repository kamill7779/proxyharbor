# ProxyHarbor

ProxyHarbor is a lightweight cloud-native proxy pool service. v0.2.0 is a breaking reset:

- Admin key manages tenants, tenant keys, providers, proxies, and the global `default` policy.
- Tenant keys can create, renew, revoke, and validate leases only.
- Providers and proxies are global platform inventory, not tenant-owned.
- Lease records keep `tenant_id`; scheduling reads from the global healthy proxy pool.
- MySQL is required. Redis is optional acceleration unless zfair Redis is explicitly required.

## Local Start

```bash
export PROXYHARBOR_ADMIN_KEY=$(openssl rand -hex 32)
export PROXYHARBOR_KEY_PEPPER=$(openssl rand -hex 32)
docker compose up -d --build
```

MySQL initializes from `migrations/mysql/init.sql`.

## Required Configuration

| Variable | Description |
| --- | --- |
| `PROXYHARBOR_ADMIN_KEY` | Bootstrap admin key, at least 32 bytes. |
| `PROXYHARBOR_KEY_PEPPER` | Tenant key hash pepper, at least 32 bytes. |
| `PROXYHARBOR_MYSQL_DSN` | MySQL DSN. |

Optional: `PROXYHARBOR_REDIS_ADDR`, `PROXYHARBOR_REDIS_PASSWORD`, `PROXYHARBOR_AUTH_REFRESH_INTERVAL`, `PROXYHARBOR_SELECTOR_REDIS_REQUIRED`.

## API Shape

Admin-only:

- `POST /admin/tenants`
- `POST /admin/tenants/{id}/keys`
- `POST|PUT|DELETE /v1/providers`
- `POST|PUT|DELETE /v1/proxies`
- `POST|PUT|DELETE /v1/policies/default`
- `GET /v1/catalog/latest`

Tenant:

- `POST /v1/leases`
- `POST /v1/leases/{id}:renew`
- `DELETE /v1/leases/{id}`

## Helm Secret

The chart is Secret-first and does not render plaintext credentials. Create the referenced Secret before install:

```bash
kubectl create secret generic proxyharbor-credentials \
  --from-literal=admin-key="$(openssl rand -hex 32)" \
  --from-literal=pepper="$(openssl rand -hex 32)" \
  --from-literal=mysql-dsn='ph:REPLACE_ME@tcp(mysql.svc:3306)/proxyharbor?parseTime=true&loc=UTC'
```

## Health

- `/healthz` is process liveness only.
- `/readyz` checks MySQL, schema seeds, auth cache initialization, and required selector dependencies.
- `/debug/auth-cache` and `/debug/auth-cache/metrics` are admin-only and never expose secrets.
