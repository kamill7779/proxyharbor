# 07 · 单体可观测性

## 健康检查

| Endpoint | 用途 |
|----------|------|
| `/healthz` | 进程是否存活 |
| `/readyz` | store、auth cache、selector 等依赖是否可服务 |
| `/metrics` | Prometheus text format 指标 |

## readyz 怎么看

`/readyz` 是部署系统应该使用的入口。单体模式下，重点看：

- SQLite 是否可读写。
- auth cache 是否刷新成功。
- selector 是否有可用候选。

## metrics 怎么看

单体模式建议至少关注：

- lease create / renew / revoke 数量和错误。
- gateway validate 成功/失败。
- selector success / no candidate。
- auth refresh success / failure。
- audit write failure。

指标 label 应保持低基数。tenant ID、proxy ID 不应该直接作为 Prometheus label。

## 日志建议

- 默认用 JSON 日志，便于收集。
- 本地调试可以切 `PROXYHARBOR_LOG_FORMAT=text`。
- 不要在日志中打印 admin key、tenant key 或 pepper。

