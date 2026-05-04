# ProxyHarbor v1.0 API Contract Audit Runbook

This runbook tracks the v1.0 API and version contract audit. The current release-hardening PR is allowed to update version metadata, release automation, and API contract metadata. `SECURITY.md` remains out of scope until the final release phase.

## Current RC Contract State

The first v1 release-candidate contract is now explicit:

- `internal/server/server.go` defaults to `Version = "1.0.0-rc.1"` and `Stability = "release-candidate"`.
- Release builds can override both values through Go `-ldflags`.
- Docker builds accept `VERSION` and `STABILITY` build args and inject them into `/version`.
- `charts/proxyharbor/Chart.yaml` reports `version: 1.0.0-rc.1` and `appVersion: 1.0.0-rc.1`.
- `api/openapi.yaml` reports `info.version: 1.0.0-rc.1` and `x-stability: release-candidate`.

Do not claim final v1.0 API/version readiness until the final release commit promotes these values to `1.0.0` and `stable`.

## API Surfaces To Audit Before v1.0

Check implementation, OpenAPI coverage, auth behavior, status codes, and error response shape for these surfaces:

- Public probes: `GET /healthz`, `GET /readyz`, and `GET /version`.
- Admin cluster: `GET /admin/cluster`.
- Tenants and keys: `/admin/tenants`, `/admin/tenants/{id}`, `/admin/tenants/{id}/keys`, and `/admin/tenants/{id}/keys/{kid}`.
- Providers, proxies, and policies: `/v1/providers`, `/v1/providers/{id}`, `/v1/proxies`, `/v1/proxies/{id}`, `/v1/proxies/{id}:health`, `/v1/policies`, and `/v1/policies/{id}`.
- Leases: `POST /v1/leases`, `POST /v1/leases/{lease_id}:renew`, and `DELETE /v1/leases/{lease_id}`.
- Gateway validation: `GET /v1/gateway/validate`, including rejection of lease password in the query string and use of `ProxyHarbor-Password`.
- Internal gateway ingestion: `POST /v1/internal/usage-events:batch` and `POST /v1/internal/gateway-feedback:batch`.
- Error response shape: JSON must expose stable `error` codes and only include `reason` when safe; unknown internal errors must not leak sensitive detail.

## Follow-up Contract PR Verification

Run these from the repository root after any version/API metadata change.

Static RC version alignment:

```bash
rg -n '1\.0\.0-rc\.1|release-candidate' internal/server/server.go charts/proxyharbor/Chart.yaml api/openapi.yaml .github/workflows/release.yaml Dockerfile
```

Expected RC result: the command shows intentional RC metadata in server, chart, OpenAPI, Dockerfile defaults, and release workflow defaults.

Static final version alignment:

```bash
rg -n 'Version\s*=\s*"1\.0\.0"|Stability\s*=\s*"stable"|version: 1\.0\.0|appVersion: 1\.0\.0|x-stability: stable' internal/server/server.go charts/proxyharbor/Chart.yaml api/openapi.yaml
if rg -n '1\.0\.0-rc\.1|release-candidate' internal/server/server.go charts/proxyharbor/Chart.yaml api/openapi.yaml; then
  echo "final metadata still contains RC markers"
  exit 1
fi
```

Expected final result: the first command shows final metadata; the second command has no hits in core version contract files.

OpenAPI and implementation surface check:

```bash
rg -n '/healthz|/readyz|/version|/admin/cluster|/admin/tenants|/v1/leases|/v1/providers|/v1/proxies|/v1/policies|/v1/gateway/validate|/v1/internal/usage-events:batch|/v1/internal/gateway-feedback:batch|Error:' api/openapi.yaml
rg -n 'HandleFunc\("/healthz"|HandleFunc\("/readyz"|HandleFunc\("/version"|HandleFunc\("/admin/cluster"|HandleFunc\("/v1/leases"|HandleFunc\("/v1/providers"|HandleFunc\("/v1/proxies"|HandleFunc\("/v1/policies"|HandleFunc\("/v1/gateway/validate"|usage-events:batch|gateway-feedback:batch|map\[string\]any\{"error"' internal/server
```

Runtime smoke, with a local SQLite profile and a non-production admin key. PowerShell example:

```powershell
New-Item -ItemType Directory -Force .tmp | Out-Null
go run ./cmd/proxyharbor init -storage=sqlite -sqlite-path .tmp/proxyharbor-contract.db
$env:PROXYHARBOR_ADMIN_KEY = "contract-admin-key-with-at-least-thirty-two-bytes"
$env:PROXYHARBOR_KEY_PEPPER = "contract-key-pepper-with-at-least-thirty-two-bytes"
$proc = Start-Process -FilePath "go" -ArgumentList @("run","./cmd/proxyharbor","-storage=sqlite","-sqlite-path",".tmp/proxyharbor-contract.db","-addr","127.0.0.1:18080") -PassThru -WindowStyle Hidden
try {
  Start-Sleep -Seconds 2
  curl.exe -fsS http://127.0.0.1:18080/healthz
  curl.exe -fsS http://127.0.0.1:18080/readyz
  curl.exe -fsS http://127.0.0.1:18080/version
  curl.exe -fsS -H "ProxyHarbor-Key: contract-admin-key-with-at-least-thirty-two-bytes" http://127.0.0.1:18080/admin/cluster
  curl.exe -i -H "ProxyHarbor-Key: bad-key" http://127.0.0.1:18080/v1/providers
} finally {
  Stop-Process -Id $proc.Id -Force
}
```

Expected smoke result after the RC contract PR: `/version` returns the aligned RC version and release-candidate stability value; admin-only routes reject invalid credentials with the documented error response shape.

Regression suite:

```bash
go test ./...
go vet ./...
git diff --check
```

## SECURITY.md Scope

`SECURITY.md` is intentionally out of scope for this audit/runbook and remains a last-phase release-readiness item. Do not create or edit `SECURITY.md` until supported versions, disclosure flow, and final v1.0 support policy are confirmed.
