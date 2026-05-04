# v1.0.0 Release Runbook

This runbook defines the release-candidate-first workflow for ProxyHarbor v1.0.0. Do not tag final `v1.0.0` until a release candidate has passed this matrix and the evidence has been reviewed.

## Release Rule

- Cut and validate a release-candidate tag first, for example `v1.0.0-rc.1`.
- Never create or push final `v1.0.0` from an unvalidated branch.
- Final `v1.0.0` is allowed only after RC evidence proves availability, correctness, packaging, API/version consistency, and documentation readiness.
- Do not claim all p95/p99 latency gates are met unless fresh RC evidence proves it. The current known state is: `500` concurrency / `10m` soak availability is met; strict latency remains a P1 follow-up.

## Pre-RC Checks

Before tagging a release candidate:

- confirm `/version`, `internal/server/server.go`, Helm `Chart.yaml`, and OpenAPI `info.version` are aligned to the RC version;
- confirm release notes describe this as a release candidate, not a final v1.0.0 release;
- confirm `SECURITY.md` handling is intentional: it is deferred until the final phase after supported versions and private disclosure flow are confirmed;
- confirm no product code or OpenAPI changes are mixed into docs-only release-readiness PRs unless separately approved.

## Verification Matrix

Run all commands from the repository root unless the command itself changes directory.

### Build and Static Checks

Required result: all commands pass.

```bash
go test ./...
go vet ./...
go build ./cmd/proxyharbor
docker build --pull=false -t proxyharbor:ha-test .
helm lint charts/proxyharbor
helm template ph charts/proxyharbor -f charts/proxyharbor/examples/dynamic-ha-values.yaml
python -c "import yaml; yaml.safe_load(open('api/openapi.yaml', encoding='utf-8')); print('openapi ok')"
```

### Version and Contract Checks

Required result: no stale final-release blockers remain before final `v1.0.0`.

```bash
if rg -n 'Version = "0.5.3"|version: 0.5.3|appVersion: 0.5.3|version: 0.4.6|release-candidate' internal/server/server.go charts/proxyharbor/Chart.yaml api/openapi.yaml .github/workflows/release.yaml; then
  echo "stale final-release blocker found"
  exit 1
fi
```

### HA Correctness Checks

Required result: all commands pass.

```bash
go run ./tools/haruntimecheck -docker -docker-skip-build -timeout 8m
go run ./tools/hacorrect -docker -timeout 6m
go run ./tools/hacachecheck -docker -docker-skip-build -timeout 6m
go -C tools/hasdkcheck run . -docker -samples 500 -disable-samples 100 -concurrency 16 -timeout 8m
```

### HA Pressure and Soak Checks

Required result: record p95/p99 for every pressure run; the mixed soak error rate must stay below `0.5%` with no sustained `502` cascade.

```bash
go run ./tools/hapressure -docker -docker-skip-build -docker-internal -mode pressure -operations gateway_validate -concurrency 500 -samples-per-op 500 -warmup-leases 500 -timeout 20m
go run ./tools/hapressure -docker -docker-skip-build -docker-internal -mode pressure -operations lease_create -concurrency 500 -samples-per-op 500 -warmup-leases 500 -timeout 20m
go run ./tools/hapressure -docker -docker-skip-build -docker-internal -mode pressure -operations lease_renew -concurrency 500 -samples-per-op 500 -warmup-leases 500 -timeout 20m
go run ./tools/hapressure -docker -docker-skip-build -docker-internal -mode soak -concurrency 500 -duration 10m -warmup-leases 500 -timeout 20m
```

The `500` concurrency / `10m` mixed soak availability gate is required for final release. Strict per-operation p95/p99 latency gates must be recorded honestly; if they remain above target, release notes must keep the limitation visible as P1 follow-up.

## RC Evidence Template

Use this template in the RC PR or release-candidate notes:

```md
## v1.0.0 release-candidate evidence

- RC tag:
- Commit:
- Machine / environment:
- Topology: 3 x proxyharbor + MySQL + Redis + LB
- Storage profile:
- Selector profile:
- `/version` output:
- Helm chart version / appVersion:
- OpenAPI info.version:

### Verification

- `go test ./...`:
- `go vet ./...`:
- `go build ./cmd/proxyharbor`:
- `docker build --pull=false -t proxyharbor:ha-test .`:
- `helm lint charts/proxyharbor`:
- `helm template ph charts/proxyharbor -f charts/proxyharbor/examples/dynamic-ha-values.yaml`:
- OpenAPI parse:
- version-blocker scan:
- `haruntimecheck`:
- `hacorrect`:
- `hacachecheck`:
- `hasdkcheck`:

### HA pressure / soak

- gateway_validate pressure p95 / p99:
- lease_create pressure p95 / p99:
- lease_renew pressure p95 / p99:
- `500` concurrency / `10m` soak total:
- soak success / failure:
- soak error rate:
- status distribution:
- sustained `502` cascade: yes / no
- strict latency gates met: yes / no
- latency follow-up required:

### Decision

- Promote this RC to final v1.0.0: yes / no
- Blockers:
- SECURITY.md final-phase status:
- Notes:
```

## Final Tag Procedure

Only after RC validation passes:

1. Update release notes from release-candidate wording to final wording.
2. Confirm version, Helm, OpenAPI, and changelog metadata are final.
3. Confirm `SECURITY.md` is present or explicitly approved by maintainers for the final phase.
4. Tag final `v1.0.0` from the reviewed commit.
5. Publish artifacts and attach the RC evidence summary.

If any P0 verification fails, do not tag final `v1.0.0`; cut another RC after the fix.

## Rollback Guidance

If the final release is published and a P0 regression appears:

- stop promoting the affected image, Helm chart, or binary artifact;
- announce the affected version, impact, and recommended previous stable version;
- revert the release commit or prepare `v1.0.1` with the minimal fix;
- preserve the failing RC/final evidence so the regression can be reproduced;
- re-run the full RC matrix before publishing the replacement.

For latency-only regressions that do not breach availability or correctness, keep the release notes honest, file a P1 follow-up, and avoid emergency rollback unless operator impact is severe.
