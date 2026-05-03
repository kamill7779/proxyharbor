# HA Pressure / Release Runbook

This runbook defines the stable v0.5.4 HA release evidence path. It documents the repeatable local topology, the formal runners that are already part of the repository, and the exact fields that must appear in the release PR.

## Scope

Use this runbook for:

- local Docker verification of the 3 instance + MySQL + Redis + LB HA topology;
- release evidence collection for runtime / correctness / cache / SDK checks;
- pressure and soak result recording once the formal `tools/hapressure` runner is present in the branch.

Do not use this runbook to justify ad-hoc scripts, cloud-vendor benchmark claims, or unversioned command lines.

## Stable HA topology

The repeatable local test topology is defined by `docker-compose.ha-test.yaml`:

- 3 `proxyharbor` instances
- 1 MySQL instance as shared storage
- 1 Redis instance for selector / cache coordination
- 1 load-balancer entrypoint exposed to the runners

Build the image once before running the checks:

```bash
docker build --pull=false -t proxyharbor:ha-test .
```

## Formal release runners

Run the supported HA runners in this order:

```bash
go run ./tools/haruntimecheck -docker -docker-skip-build -timeout 8m
go run ./tools/hacorrect -docker -timeout 6m
go run ./tools/hacachecheck -docker -docker-skip-build -timeout 6m
go -C tools/hasdkcheck run . -docker -samples 500 -disable-samples 100 -concurrency 16 -timeout 8m
```

Coverage summary:

| runner | proves |
| --- | --- |
| `haruntimecheck` | topology boot, readiness, and baseline HA runtime behavior |
| `hacorrect` | multi-instance correctness for writes, selector distribution, and disabled-node behavior |
| `hacachecheck` | cross-instance cache / auth invalidation correctness |
| `hasdkcheck` | Go SDK HA baseline through admin proxy upsert, tenant key issuance, and SDK `GetProxy` acquisition paths |

## Pressure runner

`tools/hapressure` is the formal v0.5.4 pressure / soak runner. Only record results produced by the checked-in runner under `tools/`.

Use `-docker-internal` when validating the local HA compose topology. This runs the worker inside the compose network and avoids host port-publishing connection refusals on Docker Desktop / Windows / macOS.

Record the three interface latency runs separately, then run the mixed soak:

```bash
go run ./tools/hapressure -docker -docker-skip-build -docker-internal -mode pressure -operations gateway_validate -concurrency 500 -samples-per-op 500 -warmup-leases 500 -timeout 20m
go run ./tools/hapressure -docker -docker-skip-build -docker-internal -mode pressure -operations lease_create -concurrency 500 -samples-per-op 500 -warmup-leases 500 -timeout 20m
go run ./tools/hapressure -docker -docker-skip-build -docker-internal -mode pressure -operations lease_renew -concurrency 500 -samples-per-op 500 -warmup-leases 500 -timeout 20m
go run ./tools/hapressure -docker -docker-skip-build -docker-internal -mode soak -concurrency 500 -duration 10m -warmup-leases 500 -timeout 20m
```

## PR evidence template

Every v0.5.4 HA release PR should contain:

```md
## HA pressure / release evidence

- Machine / environment:
- Image / commit:
- Topology: 3 x proxyharbor + MySQL + Redis + LB
- Commands:
  - `docker build --pull=false -t proxyharbor:ha-test .`
  - `go run ./tools/haruntimecheck -docker -docker-skip-build -timeout 8m`
  - `go run ./tools/hacorrect -docker -timeout 6m`
  - `go run ./tools/hacachecheck -docker -docker-skip-build -timeout 6m`
  - `go -C tools/hasdkcheck run . -docker -samples 500 -disable-samples 100 -concurrency 16 -timeout 8m`
  - `go run ./tools/hapressure -docker -docker-skip-build -docker-internal -mode pressure -operations gateway_validate -concurrency 500 -samples-per-op 500 -warmup-leases 500 -timeout 20m`
  - `go run ./tools/hapressure -docker -docker-skip-build -docker-internal -mode pressure -operations lease_create -concurrency 500 -samples-per-op 500 -warmup-leases 500 -timeout 20m`
  - `go run ./tools/hapressure -docker -docker-skip-build -docker-internal -mode pressure -operations lease_renew -concurrency 500 -samples-per-op 500 -warmup-leases 500 -timeout 20m`
  - `go run ./tools/hapressure -docker -docker-skip-build -docker-internal -mode soak -concurrency 500 -duration 10m -warmup-leases 500 -timeout 20m`
- gateway validate: p95= / p99=
- lease create: p95= / p99=
- lease renew: p95= / p99=
- soak error rate:
- Threshold met: yes / no
- Notes / gaps:
```

## CI boundary

The CI guard stays intentionally narrow:

- `helm lint charts/proxyharbor`
- HA example smoke render: `helm template ph charts/proxyharbor -f charts/proxyharbor/examples/dynamic-ha-values.yaml`
- `haruntimecheck`
- `hacachecheck`
- `hacorrect`
- `hasdkcheck`

Do not add 10 minute soak execution to CI. Long-running pressure evidence belongs to local release verification and the PR description.
