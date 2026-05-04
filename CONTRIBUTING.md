# 贡献指南 / Contributing

感谢你愿意改进 ProxyHarbor。请保持改动小、证据清楚、边界明确。

Thanks for improving ProxyHarbor. Keep changes small, evidence-backed, and clearly scoped.

## 简体中文

### 开始之前

- 修 bug、补测试、改文档可以直接提 PR。
- 新功能、API 行为变化、存储/缓存/selector/HA/SDK 语义变化，请先开 issue 讨论。
- 一个 PR 只解决一件事；不要把性能实验、重构、文档整理混在一起。
- 不要提交 secret、tenant key、DSN、真实代理凭据或可利用的安全细节。

### 开发环境

项目使用 Go 1.23+。常用本地检查：

```bash
go test ./...
go vet ./...
go build ./cmd/proxyharbor
docker build --pull=false -t proxyharbor:ha-test .
```

HA 相关改动常用检查：

```bash
go run ./tools/haruntimecheck -docker -docker-skip-build -timeout 8m
go run ./tools/hacorrect -docker -timeout 6m
go run ./tools/hacachecheck -docker -docker-skip-build -timeout 6m
go -C tools/hasdkcheck run . -docker -samples 500 -disable-samples 100 -concurrency 16 -timeout 8m
```

### 代码风格

- Go 代码必须 `gofmt`，命名和包边界跟现有代码保持一致。
- 标准库优先；新增依赖必须说明为什么现有代码或标准库不够。
- 控制面、gateway、storage、cache、selector 改动要小而可验证。
- 不做无关重构；如果必须重构，单独开 PR。
- 日志和测试输出不得泄露 key、DSN、真实代理 endpoint 或用户目标。

### 测试

- 普通代码改动至少跑 `go test ./...`。
- storage/cache/runtime/HA 改动要补充对应 correctness 工具结果。
- gateway validate、lease create、lease renew 热路径改动要贴 pressure/soak 证据，包括 p95、p99、error rate、status distribution。
- SQLite 单体性能改动要贴 `tools/singlebench` 参数和结果。
- 只改文档也请说明没有代码路径变化。

### Commit & PR

- commit 格式：`type: short description`。
- 常用 type：`feat` / `fix` / `docs` / `test` / `refactor` / `chore`。
- PR 标题写改了什么，正文写为什么改；具体怎么改由 diff 表达。
- PR 描述必须包含实际验证命令和结果，不要只写 "tested"。

---

## English

### Before you start

- Bug fixes, tests, and documentation updates can go straight to a PR.
- Open an issue first for new features, API behavior changes, or storage/cache/selector/HA/SDK semantic changes.
- One PR should address one concern. Do not mix performance experiments, refactors, and documentation cleanup.
- Do not commit secrets, tenant keys, DSNs, real proxy credentials, or exploitable security details.

### Development

ProxyHarbor uses Go 1.23+. Common local checks:

```bash
go test ./...
go vet ./...
go build ./cmd/proxyharbor
docker build --pull=false -t proxyharbor:ha-test .
```

Common HA checks:

```bash
go run ./tools/haruntimecheck -docker -docker-skip-build -timeout 8m
go run ./tools/hacorrect -docker -timeout 6m
go run ./tools/hacachecheck -docker -docker-skip-build -timeout 6m
go -C tools/hasdkcheck run . -docker -samples 500 -disable-samples 100 -concurrency 16 -timeout 8m
```

### Code Style

- Go code must be `gofmt`-formatted and follow existing package boundaries.
- Prefer the standard library. Any new dependency must explain why existing code or the standard library is not enough.
- Keep control-plane, gateway, storage, cache, and selector changes small and verifiable.
- Avoid unrelated refactors. If a refactor is required, submit it separately.
- Logs and test output must not leak keys, DSNs, real proxy endpoints, or user targets.

### Testing

- Run at least `go test ./...` for normal code changes.
- For storage/cache/runtime/HA changes, include the relevant correctness tool results.
- For gateway validate, lease create, or lease renew hot-path changes, include pressure/soak evidence with p95, p99, error rate, and status distribution.
- For SQLite single-node performance changes, include the `tools/singlebench` parameters and results.
- For docs-only changes, state that no code path changed.

### Commits & PRs

- Commit format: `type: short description`.
- Common types: `feat` / `fix` / `docs` / `test` / `refactor` / `chore`.
- PR title = what changed. PR body = why it changed. The diff shows how.
- PR descriptions must include the actual verification commands and results, not just "tested".
