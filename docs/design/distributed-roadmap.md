# Distributed Roadmap

> 目标：把 v0.5.x 到 v1.0.0 前的分布式能力拆成可并行、可实测的小步。

## 方向

ProxyHarbor 的分布式模式采用 MySQL + Redis：

- MySQL 是权威数据源。
- Redis zfair 做全局 selector 协调。
- Redis Pub/Sub 作为缓存失效 hint。
- 多实例通过 heartbeat 和 leader lock 协作后台任务。

不做 Raft，不做自研一致性存储，不把 ProxyHarbor 做成重型控制面。

## v0.5.x 切分

| 版本 | 主题 | 重点 |
|------|------|------|
| v0.5.0 | 多实例写入正确性 | lease / tenant key / proxy 写路径事务、幂等、冲突 |
| v0.5.1 | 分布式 selector | Redis zfair、全局权重、公平性、strict fallback |
| v0.5.2 | 跨实例缓存失效 | invalidation bus、Redis Pub/Sub hint、TTL/polling 兜底 |
| v0.5.3 | 运维与 graceful runtime | `/admin/cluster`、leader failover、drain、rolling restart |
| v0.5.4 | 压测与发布收口 | correctness runner、pressure runner、性能门槛、文档补齐 |

## v1.0.0 前的硬门槛

- 单体路径稳定：SQLite + local selector。
- HA 路径稳定：MySQL + Redis + zfair。
- 多实例写入不静默覆盖。
- 缓存最终一致，关键安全状态不会长期脏读。
- selector 全局公平且可观测。
- SDK 在单体和 HA 下都能跑通。
- Docker 实机测试和压力测试可重复。

## 实测优先

分布式文档只写已经实测过的路径。每个能力进入用户文档前，都要先有：

- 本机 Docker 拓扑。
- correctness runner。
- pressure / soak test。
- 明确的失败语义和排障入口。

