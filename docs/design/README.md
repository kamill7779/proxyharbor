# ProxyHarbor 设计文档

设计文档面向维护者和贡献者。它解释“为什么这样做”，不是替代快速开始。

## 文档列表

| 文档 | 内容 |
|------|------|
| [standalone-internals](./standalone-internals.md) | SQLite 单体内部设计、selector、auth cache、lease 生命周期 |
| [distributed-roadmap](./distributed-roadmap.md) | MySQL + Redis 多实例路线、正确性、selector、缓存失效、运维 |

## 设计原则

- 单体路径轻，不引入 Redis / MySQL。
- 分布式路径明确，不把 SQLite 扩成集群。
- 数据库是最终权威，缓存和 Pub/Sub 都只是优化。
- domain error 稳定，HTTP `reason` 可被 SDK 和测试工具断言。
- 指标低基数，避免把 tenant / proxy 维度直接打进 Prometheus label。

