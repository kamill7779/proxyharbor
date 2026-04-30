# SQLite Single-Instance Performance Runbook

This runbook defines the v0.4.6 single-node SQLite performance baseline. It is intentionally lightweight: the load generator is a small Go tool under `tools/singlebench` and the wrapper scripts only require Go plus Docker Compose for full single-profile runs.

## One-command benchmark

PowerShell:

```powershell
./scripts/singlebench.ps1 -Requests 1000 -Concurrency 32 -Proxies 16 -Operation mixed -Output json -Out artifacts/singlebench-mixed.json
```

bash:

```bash
REQUESTS=1000 CONCURRENCY=32 PROXIES=16 OPERATION=mixed OUTPUT=json OUT=artifacts/singlebench-mixed.json ./scripts/singlebench.sh
```

By default the wrapper starts `docker-compose.yaml` with the single SQLite profile, waits for `/readyz`, creates a benchmark tenant/key, registers healthy local proxy endpoints, warms lease state, and then measures the selected operation. Use `-SkipDocker` or `SKIP_DOCKER=true` when the service is already running.

The wrapper keeps internal/private proxy endpoints disabled by default. If the benchmark proxies are loopback or private-network URLs, opt in explicitly with `-AllowInternal` on PowerShell or `ALLOW_INTERNAL=true` on bash.

## Covered operations

- `lease_create`: tenant-key authenticated `POST /v1/leases` with unique idempotency keys.
- `renew`: tenant-key authenticated `POST /v1/leases/{id}:renew` against warm leases, exercising the SQLite generation CAS path.
- `validate`: admin-authenticated `GET /v1/gateway/validate`, exercising lease lookup and password validation.
- `catalog`: admin-authenticated `GET /v1/catalog/latest`, exercising catalog/proxy scans.
- `mixed`: 30% create, 20% renew, 40% validate, 10% catalog.

## Output fields

Both JSON and CSV include:

- `total`, `success`, `failure`, `elapsed_ms`, `rps`.
- `p50_ms`, `p90_ms`, `p95_ms`, `p99_ms`, `max_ms`.
- HTTP status distribution.
- Proxy distribution for lease/proxy-bound operations.
- `started_at` and `finished_at` timestamps.

## SQLite hot-path baseline

The single profile keeps `journal_mode` unchanged to avoid conflicting with file-level backup/restore sidecar expectations. The v0.4.6 baseline uses connection-local pragmas only:

- `busy_timeout(5000)` to absorb short writer contention.
- `foreign_keys(1)` for existing integrity behavior.
- `synchronous(NORMAL)` and `temp_store(MEMORY)` for local single-process throughput without forcing WAL.

Hot-path indexes are present for:

- Lease create/idempotency: `proxy_idempotency_keys` primary key plus tenant/created index.
- Renew CAS and gateway validation lookup: `proxy_leases` primary key plus active/CAS/proxy-active indexes.
- Selectable proxy and catalog scans: `idx_proxies_selectable`, proxy primary key.
- Audit/usage writes and reads: audit tenant/order index, usage tenant/order index.
- Auth cache refresh: `idx_tenant_keys_active_refresh` plus key hash/fingerprint unique indexes.
- Catalog snapshot freshness: `idx_proxy_catalog_snapshots_fresh`.

## Capacity reference

Record a baseline on the target host before production use. Suggested initial acceptance for a small single-node deployment is:

- `mixed` at `CONCURRENCY=32` has `success / total >= 0.99`.
- `p95_ms` remains below the workload's lease acquisition SLO.
- `rps` remains stable across three consecutive runs with less than 20% variance.
- `/readyz` remains ready and logs show no sustained `database is locked` errors.

Treat the measured value as a local capacity reference, not a portable guarantee. SQLite single-instance throughput depends heavily on disk latency, container volume driver, CPU steal, and audit/usage volume.

## Signals to switch to HA

Move to the MySQL+Redis HA profile when any of these are sustained after reducing avoidable client bursts:

- `busy_timeout`/`database is locked` errors appear during normal traffic.
- `mixed` success ratio drops below 99% at required production concurrency.
- `p95_ms` or `p99_ms` exceeds the lease/gateway SLO for three consecutive runs.
- Audit/usage retention or backup windows compete with foreground lease traffic.
- A single process, single volume, or local disk is no longer an acceptable availability boundary.
- You need multiple controller/gateway replicas or cross-node auth invalidation guarantees.

For HA, use `docker-compose.ha.yaml` or Helm values backed by MySQL and Redis; do not enable WAL as a substitute for HA.
