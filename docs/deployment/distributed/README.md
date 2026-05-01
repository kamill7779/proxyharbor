# ProxyHarbor 分布式模式文档（占位）

分布式模式是 v0.5.x 之后的重点方向：MySQL 作为权威存储，Redis 负责 zfair selector、缓存失效 hint 和多实例协作。当前目录先保留结构占位，避免把未实测稳定的配置提前写成用户教程。

## 目标形态

```text
LB / Ingress
├── proxyharbor-0
├── proxyharbor-1
└── proxyharbor-2
     │
     ├── MySQL  权威数据：tenant / key / provider / proxy / lease / cluster state
     └── Redis  zfair selector / invalidation hint
```

## 后续章节规划

| 章节 | 计划内容 |
|------|----------|
| 01 quickstart | 本机 Docker HA 三实例启动 |
| 02 configuration | MySQL、Redis、selector、cluster 配置 |
| 03 architecture | leader election、heartbeat、maintenance、zfair |
| 04 correctness | 多实例写入语义、幂等、乐观锁、事务边界 |
| 05 operations | rolling restart、backup、restore、故障切换 |
| 06 performance | correctness runner、pressure runner、性能门槛 |
| 07 observability | `/admin/cluster`、metrics、配置漂移 |

## 当前边界

- 不支持 SQLite 多实例共享状态。
- HA 必须显式提供 admin key 和 key pepper，不自动生成 secrets。
- HA 推荐 `selector=zfair` 且 Redis required。
- MySQL / Redis 自身的高可用由部署方负责。

详细路线见 [设计 / 分布式路线](../../design/distributed-roadmap.md) 与 [v0.5.0 计划](../../versions/v0.5.0.md)。

