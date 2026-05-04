# ProxyHarbor v1.0 API Contract Audit Runbook

This runbook tracks the v1.0 API and version contract audit. It is documentation-only: do not edit code, `api/openapi.yaml`, Helm metadata, release files, README files, GitHub templates, or `SECURITY.md` from this task.

## Current Pre-v1 Blockers

These are known blockers and must stay visible until a follow-up contract PR fixes and verifies them:

- `internal/server/server.go` still sets `server.Version` to `0.5.3`.
- `/version` still reports `stability=release-candidate`; v1.0 must explicitly decide and verify the final stability value.
- `charts/proxyharbor/Chart.yaml` still has `version: 0.5.3` and `appVersion: 0.5.3`.
- `api/openapi.yaml` still has `info.version: 0.4.6`.

Do not claim v1.0 API/version readiness while any of these blockers remain.

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

Run these from the repository root in the follow-up PR that actually changes version/API metadata.

Static version alignment:

```bash
rg -n 'Version = "1\.0\.0"|version: 1\.0\.0|appVersion: 1\.0\.0|version: 1\.0\.0' internal/server/server.go charts/proxyharbor/Chart.yaml api/openapi.yaml
if rg -n 'Version = "0\.5\.3"|version: 0\.5\.3|appVersion: 0\.5\.3|version: 0\.4\.6|release-candidate' internal/server/server.go charts/proxyharbor/Chart.yaml api/openapi.yaml .github/workflows/release.yaml; then
  echo "stale version/API blocker found"
  exit 1
fi
```

Expected result after the contract PR: the first command shows the intentional v1.0.0 metadata; the second command has no stale blocker hits unless a documented exception is added in the same PR.

OpenAPI and implementation surface check:

```bash
rg -n '/healthz|/readyz|/version|/admin/cluster|/admin/tenants|/v1/leases|/v1/providers|/v1/proxies|/v1/policies|/v1/gateway/validate|/v1/internal/usage-events:batch|/v1/internal/gateway-feedback:batch|Error:' api/openapi.yaml
rg -n 'HandleFunc\("/healthz"|HandleFunc\("/readyz"|HandleFunc\("/version"|HandleFunc\("/admin/cluster"|HandleFunc\("/v1/leases"|HandleFunc\("/v1/providers"|HandleFunc\("/v1/proxies"|HandleFunc\("/v1/policies"|HandleFunc\("/v1/gateway/validate"|usage-events:batch|gateway-feedback:batch|map\[string\]any\{"error"' internal/server
```

Runtime smoke, with a local SQLite profile and a non-production admin key. PowerShell example:

```powershell
mkdir -p .tmp
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

Expected smoke result after the contract PR: `/version` returns the aligned v1.0.0 version and final stability value; admin-only routes reject invalid credentials with the documented error response shape.

Regression suite:

```bash
go test ./...
go vet ./...
git diff --check
```

## SECURITY.md Scope

`SECURITY.md` is intentionally out of scope for this audit/runbook and remains a last-phase release-readiness item. Do not create or edit `SECURITY.md` until supported versions, disclosure flow, and final v1.0 support policy are confirmed.
