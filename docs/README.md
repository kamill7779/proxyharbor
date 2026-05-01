# ProxyHarbor 文档入口

这里是 ProxyHarbor 的项目文档入口。README 保持最短使用路径，本目录负责展开 SDK、部署、设计、版本计划和运维手册。

## 阅读路线

| 目录 | 适合谁 | 你能获得的能力 |
|------|--------|----------------|
| [SDK / Go](./sdk/go/README.md) | 业务开发、平台开发 | 用 Go SDK 获取代理、维护代理库存、处理租约生命周期 |
| [Deployment / Standalone](./deployment/standalone/README.md) | 本地部署、小团队、单体服务 | 用 SQLite 单体模式完成零配置启动、配置、运维和观测 |
| [Deployment / Distributed](./deployment/distributed/README.md) | 准备 HA 的平台团队 | 了解后续 MySQL + Redis 多实例能力的目录占位和边界 |
| [Design](./design/README.md) | 维护者、贡献者 | 理解单体内部设计与分布式路线 |
| [Runbooks](./runbooks/) | 运维 | SQLite 单体性能、备份、保留策略等操作手册 |
| [Versions](./versions/) | 维护者、发布负责人 | 每个版本的目标、非目标、验收标准 |

## 两条正式部署路径

ProxyHarbor 只把两条路径作为正式产品方向：

- **Standalone**：`storage=sqlite`、`selector=local`、单 binary、自动本地 secrets。适合本地、小规模、单节点服务。
- **Distributed**：`storage=mysql`、`selector=zfair`、Redis 协调、多实例部署。适合 HA 和云原生生产环境。

`memory` 只用于 dev/demo/CI，不作为正式部署 profile。

## 文档维护原则

- 先写用户能跑通的最短路径，再解释完整配置。
- 先区分 admin / tenant 权限，再讲 API 细节。
- 单体文档写稳定事实；分布式文档先保留设计占位，等功能实测稳定后补完整教程。
- 测试代码和临时压测脚本不混进文档提交；正式工具放在 `tools/` 后再引用。

