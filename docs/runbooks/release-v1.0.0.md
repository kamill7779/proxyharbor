# v1.0.0 Release Runbook

This runbook defines the release-candidate-first workflow for ProxyHarbor v1.0.0. Do not tag final `v1.0.0` until a release candidate has passed this matrix and the evidence has been reviewed.

## Release Rule

- Cut and validate a release-candidate tag first, for example `v1.0.0-rc.1`.
- Never create or push final `v1.0.0` from an unvalidated branch.
- Final `v1.0.0` is allowed only after RC evidence proves availability, correctness, packaging, API/version consistency, and documentation readiness.
- Do not claim all p95/p99 latency gates are met unless fresh RC evidence proves it. The current known state is: `500` concurrency / `10m` soak availability is met; strict latency remains a P1 follow-up.
- RC tags must publish prereleases and versioned images only. Stable final tags may update `latest`.

## Pre-RC Checks

Before tagging a release candidate:

- confirm `/version`, `internal/server/server.go`, Helm `Chart.yaml`, and OpenAPI `info.version` are aligned to the RC version;
- confirm release notes describe this as a release candidate, not a final v1.0.0 release;
- confirm the release workflow will attach Helm, binary, and checksum assets;
- confirm RC container publishing does not update `ghcr.io/kamill7779/proxyharbor:latest`;
- confirm `SECURITY.md` handling is intentional: it is deferred until the final phase after supported versions and private disclosure flow are confirmed;
- confirm no product behavior or HA algorithm changes are mixed into release-readiness PRs unless separately approved.

## Verification Matrix

Run all commands from the repository root unless the command itself changes directory.

### Build and Static Checks

Required result: all commands pass.

```bash
go test ./...
go vet ./...
go build -trimpath -ldflags="-s -w -X github.com/kamill7779/proxyharbor/internal/server.Version=1.0.0-rc.1 -X github.com/kamill7779/proxyharbor/internal/server.Stability=release-candidate" ./cmd/proxyharbor
docker build --pull=false -t proxyharbor:ha-test --build-arg VERSION=1.0.0-rc.1 --build-arg STABILITY=release-candidate .
cid=$(docker run --rm -d -p 18082:8080 proxyharbor:ha-test)
trap 'docker stop "$cid" >/dev/null 2>&1 || true' EXIT
ok=0
for i in $(seq 1 30); do
  if curl -fsS http://127.0.0.1:18082/version; then
    ok=1
    break
  fi
  sleep 1
done
test "$ok" = 1
docker stop "$cid"
trap - EXIT
helm lint charts/proxyharbor
helm template ph charts/proxyharbor -f charts/proxyharbor/examples/dynamic-ha-values.yaml
python -m pip install --user PyYAML
python -c "import yaml; yaml.safe_load(open('api/openapi.yaml', encoding='utf-8')); print('openapi ok')"
```

### Version and Contract Checks

Required RC result: all core metadata reports `1.0.0-rc.1` and `release-candidate`.

```bash
rg -n '1.0.0-rc.1|release-candidate' internal/server/server.go charts/proxyharbor/Chart.yaml api/openapi.yaml .github/workflows/release.yaml
python - <<'PY'
import pathlib, re, sys
server = pathlib.Path("internal/server/server.go").read_text(encoding="utf-8")
checks = [
    ("server.Version", r'Version\s*=\s*"1\.0\.0-rc\.1"'),
    ("server.Stability", r'Stability\s*=\s*"release-candidate"'),
    ("chart version", r'^version:\s*1\.0\.0-rc\.1$'),
    ("chart appVersion", r'^appVersion:\s*1\.0\.0-rc\.1$'),
    ("openapi version", r'^  version:\s*1\.0\.0-rc\.1$'),
    ("openapi stability", r'^  x-stability:\s*release-candidate$'),
]
texts = {
    "server": server,
    "chart": pathlib.Path("charts/proxyharbor/Chart.yaml").read_text(encoding="utf-8-sig"),
    "openapi": pathlib.Path("api/openapi.yaml").read_text(encoding="utf-8"),
}
for name, pattern in checks:
    haystack = "\n".join(texts.values())
    if not re.search(pattern, haystack, re.M):
        sys.exit(f"missing {name}: {pattern}")
print("rc metadata ok")
PY
```

Required final result before tagging `v1.0.0`: source metadata must be promoted to `1.0.0` and `stable`, and the final scan below must return no hits.

```bash
if rg -n '1\.0\.0-rc\.1|release-candidate' internal/server/server.go charts/proxyharbor/Chart.yaml api/openapi.yaml; then
  echo "final metadata still contains RC markers"
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

### Release Workflow Artifact Checks

Required result: the release workflow succeeds for the RC tag and the published artifacts match the tag.

Check after the workflow finishes:

```bash
gh release view v1.0.0-rc.1 --json isPrerelease,tagName,url
rm -rf .tmp/release-check
mkdir -p .tmp/release-check
gh release download v1.0.0-rc.1 --pattern 'checksums.txt' --dir .tmp/release-check
cat .tmp/release-check/checksums.txt
```

Expected RC artifacts:

- GitHub Release is marked prerelease.
- GHCR has `ghcr.io/kamill7779/proxyharbor:1.0.0-rc.1`.
- GHCR `latest` is not updated by the RC run.
- Release assets include the Helm chart package, cross-platform binary archives, and `checksums.txt`.

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
- Release workflow run:
- GHCR image tag:
- GitHub Release prerelease:
- Release assets / checksums:

### Verification

- `go test ./...`:
- `go vet ./...`:
- `go build ./cmd/proxyharbor`:
- `docker build --pull=false -t proxyharbor:ha-test .`:
- Docker `/version` smoke:
- `helm lint charts/proxyharbor`:
- `helm template ph charts/proxyharbor -f charts/proxyharbor/examples/dynamic-ha-values.yaml`:
- OpenAPI parse:
- RC/final metadata scan:
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
2. Promote source metadata from `1.0.0-rc.1` / `release-candidate` to `1.0.0` / `stable`.
3. Confirm `SECURITY.md` is present or explicitly approved by maintainers for the final phase.
4. Confirm version, Helm, OpenAPI, Docker, binary, and changelog metadata are final.
5. Tag final `v1.0.0` from the reviewed commit.
6. Publish artifacts and attach the RC evidence summary.

If any P0 verification fails, do not tag final `v1.0.0`; cut another RC after the fix.

## Rollback Guidance

If the final release is published and a P0 regression appears:

- stop promoting the affected image, Helm chart, or binary artifact;
- announce the affected version, impact, and recommended previous stable version;
- revert the release commit or prepare `v1.0.1` with the minimal fix;
- preserve the failing RC/final evidence so the regression can be reproduced;
- re-run the full RC matrix before publishing the replacement.

For latency-only regressions that do not breach availability or correctness, keep the release notes honest, file a P1 follow-up, and avoid emergency rollback unless operator impact is severe.
