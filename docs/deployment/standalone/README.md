# ProxyHarbor 单体模式文档

单体模式是 ProxyHarbor v0.4.x 的默认路径：一个进程、一个 SQLite 文件、一个本地 selector。它的目标不是替代 HA，而是让小规模业务用最少配置拿到稳定代理能力。

## 阅读路线

| 章节 | 你能获得的能力 |
|------|----------------|
| [01 快速开始](./01-quickstart.md) | 用 Docker 三步启动，配合 Go SDK 拿到代理 |
| [02 配置参考](./02-configuration.md) | 理清最小配置、自动 secrets、SQLite 路径和安全开关 |
| [03 架构说明](./03-architecture.md) | 理解单 binary 内部的控制面、网关、store、selector |
| [04 数据模型](./04-data-model.md) | 理解 tenant、key、provider、proxy、lease 的边界 |
| [05 运维操作](./05-operations.md) | 备份、恢复、retention、doctor、init 的使用边界 |
| [06 最佳实践](./06-best-practices.md) | 单体模式下如何避免把简单部署用复杂 |
| [07 可观测性](./07-observability.md) | readyz、healthz、metrics 和日志应该怎么看 |

## 思路总览

单体模式只需要回答三个问题：

1. **数据放哪里**：默认 SQLite，路径通常是 `/var/lib/proxyharbor/proxyharbor.db`。
2. **密钥怎么来**：首次启动自动生成 `PROXYHARBOR_ADMIN_KEY` 和 `PROXYHARBOR_KEY_PEPPER`，写入 `secrets.env`。
3. **代理怎么选**：进程内 smooth weighted round-robin，只在单进程内维护状态。

如果你开始需要多副本、跨实例调度、leader election、Redis zfair，那就切到 distributed 路径，而不是把 SQLite 单体硬扩成集群。

