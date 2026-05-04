<!-- 感谢贡献 / Thanks for contributing -->

## 改了什么 / What changed

<!-- 一两句话说明本 PR 改了什么 / One or two sentences describing the change -->

## 为什么 / Why

<!-- 修哪个 bug、加哪个功能、关联哪个 issue / Which bug, feature, or issue does this address? -->

## 影响范围 / Impact

- [ ] Admin API / tenant keys
- [ ] Lease create / renew
- [ ] Gateway validate
- [ ] SQLite single-node
- [ ] MySQL + Redis HA
- [ ] Redis cache / selector
- [ ] Go SDK
- [ ] Helm / Docker
- [ ] Docs only

## 测试 / Testing

<!-- 粘贴实际跑过的命令和结果，不要只写 "tested"。
Paste the actual commands and results. Do not write only "tested".

Examples:
- go test ./...
- go vet ./...
- go build ./cmd/proxyharbor
- go run ./tools/haruntimecheck -docker -docker-skip-build -timeout 8m
- go run ./tools/hacorrect -docker -timeout 6m
- go run ./tools/hacachecheck -docker -docker-skip-build -timeout 6m
- go -C tools/hasdkcheck run . -docker -samples 500 -disable-samples 100 -concurrency 16 -timeout 8m
- go run ./tools/hapressure ...
- go run ./tools/singlebench ...
-->

## Checklist

- [ ] 代码风格和现有 Go 代码一致 / Code style matches existing Go code
- [ ] 没有引入不必要的新依赖 / No unnecessary new dependencies
- [ ] storage/cache/selector 改动有 correctness 测试 / Storage/cache/selector changes include correctness tests
- [ ] HA 热路径改动有 pressure/soak 证据 / HA hot-path changes include pressure/soak evidence
- [ ] 配置、API 或行为变化已更新文档 / Config, API, or behavior changes are documented
- [ ] 没有提交 secret、token、DSN 或真实代理凭据 / No secrets, tokens, DSNs, or real proxy credentials are committed
