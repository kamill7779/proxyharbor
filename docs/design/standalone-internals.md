# Standalone Internals

> 目标：解释 ProxyHarbor 单体模式内部怎么工作，以及为什么它能保持轻量。

## 总体结构

单体模式运行 `role=all`，也就是控制面和网关校验能力在一个进程内：

```text
HTTP mux
├── admin handlers
├── lease handlers
├── gateway validate handlers
├── readyz / healthz / metrics
└── recover / respond error mapping

control service
├── auth boundary
├── lease lifecycle
├── selector call
└── audit events

sqlite store
├── tenant / tenant key
├── provider / proxy
├── lease
├── audit / usage
└── health events
```

## Auth cache

tenant key 明文只在签发时返回一次。服务端保存 hash，并通过 auth cache 加速校验。

单体模式下，auth cache 定期从 SQLite 刷新。刷新失败不会立刻让进程退出，但 `/readyz` 和 metrics 会暴露状态。

## Lease 生命周期

SDK 侧把 lease 当成短期授权：

1. 第一次 `GetProxy` 创建 lease。
2. sticky key 命中本地缓存时复用 lease。
3. 快过期时尝试 renew。
4. 已过期且允许 reacquire 时重新创建。
5. `Release` 主动 revoke。

服务端仍然是权威。SDK 缓存只减少请求次数，不改变安全边界。

## Local selector

local selector 使用 smooth weighted round-robin：

- 每个 proxy 有 `weight` 和运行时 `current`。
- 每轮给候选 `current += weight`。
- 选择 `current` 最大的候选。
- 被选中后 `current -= totalWeight`。

候选必须健康、正权重、健康分为正、且不处于熔断窗口。这个状态只在进程内维护，因此 local selector 是单体能力，不是分布式调度能力。

## 健康评分

服务端根据 gateway validate / health event 更新 proxy 健康状态。成功会提升可用性，连接失败、超时、协议错误会降低分数；连续失败会触发 circuit open，让 selector 暂时跳过该 proxy。

## 为什么不让 SQLite 多实例

SQLite 很适合单体持久化，但不适合作为 ProxyHarbor 的多实例共享协调层。多实例需要：

- 全局写入事务边界。
- 全局 selector 状态。
- leader election。
- 跨实例缓存失效。

这些能力应该由 MySQL + Redis 路径提供，而不是把单体模式变复杂。

