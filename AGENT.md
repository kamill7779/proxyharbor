# Agent Notes

## Test File Organization

Keep test files close to the package they verify. Do not move Go unit tests into a global test directory when they need package-private helpers or behavior.

Use topic-oriented files:
- `runtime_ops_test.go` for startup, shutdown, readiness, liveness, runtime metrics, and lifecycle helpers.
- `gateway_test.go` for gateway HTTP/CONNECT behavior.
- `admin_contract_test.go` for admin API contract behavior.
- `cache_test.go`, `invalidator_test.go`, and similar names for package-specific units with clear boundaries.

Before adding a new `_test.go` file:
1. Check whether an existing topic file in the same package already fits.
2. Add shared setup to a package-local test helper only when it removes real duplication.
3. Prefer table-driven tests for closely related cases.
4. Avoid broad helper abstractions for one-off tests.
5. Do not expose production APIs just to centralize tests.

For runtime or HA checks:
- Keep repeatable Docker/HA probes under `tools/`.
- Keep lightweight helper tests beside the tool package.
- Use bounded retries with explicit timeouts.
- Do not add benchmark or load-test scope unless the task explicitly asks for it.

Verification expectation:
- Run the narrow package test after reorganizing files.
- Run `go test ./...` before final handoff.
