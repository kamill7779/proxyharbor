# ProxyHarbor v1.0.0 Release Readiness Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Prepare ProxyHarbor for a credible v1.0.0 release by closing documentation, version-contract, validation, and community-readiness gaps without adding new product scope.

**Architecture:** Treat v1.0.0 as a release-hardening milestone rather than a feature milestone. Keep the current single-node SQLite and MySQL + Redis HA architecture unchanged; document only verified paths and explicitly defer new performance work beyond release gating. `SECURITY.md` is intentionally last because it is a public maintenance commitment that must match the final version/support policy.

**Tech Stack:** Go 1.23, Docker / Docker Compose, Helm, GitHub Actions, OpenAPI 3.1, GitHub issue forms.

---

## Scope Boundaries

### In Scope

- Complete community contribution surfaces except `SECURITY.md`.
- Align release documentation around the current v0.5.5 HA evidence and v1.0.0 readiness path.
- Add a v1.0.0 release checklist and release runbook.
- Audit and update documentation references that still describe v0.4.x or v0.5.4 as the current release baseline.
- Identify version/API metadata gaps that must be fixed before v1.0.0.
- Keep changes documentation-first and reviewable.

### Out of Scope

- Do not implement new product features.
- Do not change HA hot-path algorithms in this PR.
- Do not change MySQL/Redis/SQLite behavior in this PR unless a tiny version metadata fix is explicitly approved.
- Do not create `SECURITY.md` until the final release-readiness PR, after supported versions and private disclosure flow are confirmed.
- Do not create a v1.0.0 release or tag from this branch.

## Release-Critical Findings To Carry Forward

- `internal/server/server.go` still reports `Version = "0.5.3"` and `/version` returns `stability=release-candidate`.
- `charts/proxyharbor/Chart.yaml` still has `version: 0.5.3` and `appVersion: 0.5.3`.
- `api/openapi.yaml` still has `info.version: 0.4.6`.
- `docs/versions/v0.5.5.md` records that the final HA soak availability gate is met, but strict per-operation p95/p99 latency gates are not fully met.
- `docs/deployment/distributed/README.md`, `docs/runbooks/ha-pressure.md`, and `charts/proxyharbor/README.md` still contain v0.5.4 release-baseline wording.
- GitHub community health still needs issue templates, PR template, and a fuller `CONTRIBUTING.md`; `CODE_OF_CONDUCT.md` already exists on `origin/main`.

## Task 1: Community Contribution Surfaces

**Files:**

- Modify: `CONTRIBUTING.md`
- Create: `.github/ISSUE_TEMPLATE/bug_report.yml`
- Create: `.github/ISSUE_TEMPLATE/feature_request.yml`
- Create: `.github/ISSUE_TEMPLATE/config.yml`
- Create: `.github/PULL_REQUEST_TEMPLATE.md`

**Design:**

- Keep the WindsurfAPI-inspired style: bilingual, concise, practical.
- Ask for reproducible evidence instead of long process.
- Include ProxyHarbor-specific fields: deployment mode, storage backend, HA/SQLite/SDK/gateway impact, logs with secret redaction, verification commands.
- In issue templates, do not link to `SECURITY.md` until that file exists. Use general warning text that security-sensitive details must not be posted publicly.

**Verification:**

```bash
python - <<'PY'
import pathlib, yaml
for p in pathlib.Path('.github/ISSUE_TEMPLATE').glob('*.yml'):
    yaml.safe_load(p.read_text(encoding='utf-8'))
    print(p, 'ok')
PY
git diff --check
go test ./...
```

## Task 2: v1.0.0 Release Documentation

**Files:**

- Create: `docs/versions/v1.0.0.md`
- Create: `docs/runbooks/release-v1.0.0.md`
- Modify: `docs/README.md`

**Design:**

- `docs/versions/v1.0.0.md` is a release-readiness document, not a marketing page.
- Explicitly define supported capabilities:
  - SQLite single-node profile.
  - MySQL + Redis HA profile.
  - Redis zfair selector / cache coordination.
  - Go SDK baseline.
  - Docker, Docker Compose, and Helm install paths.
- Explicitly define non-goals:
  - No SQLite multi-instance shared state.
  - No MySQL/Redis HA orchestration.
  - No Raft/etcd/Consul/self-built consensus.
  - No cloud-vendor benchmark claims.
- Carry forward v0.5.5 evidence:
  - 500 concurrency / 10m mixed soak error rate is below 0.5%.
  - No sustained 502 cascade.
  - Per-operation p95/p99 latency gates are still above target under heavy mixed write pressure.
- State the v1.0 release gate honestly: availability/correctness/readiness are P0; extreme latency remains P1 follow-up unless a later PR provides direct evidence.

**Verification:**

```bash
rg -n "v1.0.0|500|10m|soak|SQLite|MySQL|Redis|SECURITY" docs/versions/v1.0.0.md docs/runbooks/release-v1.0.0.md docs/README.md
git diff --check
```

## Task 3: Deployment And Runbook Consolidation

**Files:**

- Modify: `docs/deployment/distributed/README.md`
- Modify: `docs/runbooks/ha-pressure.md`
- Modify: `charts/proxyharbor/README.md`
- Modify: `README.md`
- Modify: `README.en.md`

**Design:**

- Replace stale v0.5.4-baseline wording with v0.5.5/v1.0-readiness wording where appropriate.
- Preserve README structure and visual formatting.
- Keep the performance section honest:
  - 500/10m mixed soak gate is met.
  - p95/p99 latency gates are not yet fully met.
  - Control-plane pressure numbers do not represent external proxy data-plane throughput.
- Make Chinese and English README conceptually aligned.
- Keep Helm docs clear:
  - default chart values are single-node SQLite.
  - HA must use MySQL + Redis and `examples/dynamic-ha-values.yaml`.
  - `multi-instance-values.yaml` is exploratory unless separately validated.

**Verification:**

```bash
rg -n "v0.5.4|v0.5.5|v1.0|release baseline|release evidence" README.md README.en.md docs/deployment/distributed/README.md docs/runbooks/ha-pressure.md charts/proxyharbor/README.md
git diff --check
```

## Task 4: Version And API Contract Audit

**Files:**

- Inspect: `internal/server/server.go`
- Inspect: `charts/proxyharbor/Chart.yaml`
- Inspect: `api/openapi.yaml`
- Inspect: `cmd/proxyharbor/main.go`
- Update documentation references only in this PR unless approved.

**Design:**

- Produce a short release-blocker checklist in `docs/versions/v1.0.0.md` or `docs/runbooks/release-v1.0.0.md`.
- Do not silently bump code version in a docs-only PR unless explicitly approved.
- The required follow-up code/API PR must align:
  - `/version`
  - `server.Version`
  - Helm `version` and `appVersion`
  - OpenAPI `info.version`
  - release workflow tag/changelog behavior.

**Verification:**

```bash
rg -n 'Version = "0.5.3"|version: 0.5.3|appVersion: 0.5.3|version: 0.4.6|release-candidate' internal/server/server.go charts/proxyharbor/Chart.yaml api/openapi.yaml .github/workflows/release.yaml
```

Expected: the command may still find blockers after this PR; they must be documented as pre-v1.0 follow-up if not fixed here.

## Task 5: Final Review And PR Readiness

**Files:**

- All files changed in this branch.

**Review Requirements:**

- No `SECURITY.md` in this PR.
- No product behavior changes.
- No broad README restructuring.
- No false claim that v1.0.0 is already released.
- No false claim that all strict p95/p99 latency gates are met.
- All commands in docs must be runnable from repo root unless otherwise stated.

**Verification:**

```bash
git diff --check
go test ./...
go vet ./...
```

If only docs/templates changed, full HA soak is not required for this PR. The release runbook must still list the full matrix that a v1.0.0 release candidate must run.

## `SECURITY.md` Final-Phase Design

Do this after the current documentation/readiness work lands.

**File:**

- Create: `SECURITY.md`

**Design:**

- Supported versions:
  - v1.0.x receives security fixes.
  - v0.x support is best-effort unless explicitly promised.
- Reporting:
  - Prefer GitHub private vulnerability reporting if enabled.
  - If not enabled, provide a private contact path before linking issue forms.
- Public issue policy:
  - Do not post PoCs, auth bypass payloads, SSRF details, secrets, tokens, DSNs, or real proxy credentials publicly.
- Security scope:
  - auth bypass
  - tenant isolation failure
  - SSRF / unsafe target validation bypass
  - secret leakage
  - gateway validation bypass
  - stale cache security impact
- Out of scope:
  - local resource exhaustion from intentional stress tests
  - third-party proxy quality
  - leaked user-provided credentials
  - intentionally unsafe non-default deployments.
- Response expectations:
  - acknowledge receipt
  - triage
  - fix or mitigation
  - advisory/release notes when appropriate.

**Verification:**

```bash
rg -n "SECURITY|Security|v1.0.x|v0.x|private vulnerability" SECURITY.md .github/ISSUE_TEMPLATE
```
