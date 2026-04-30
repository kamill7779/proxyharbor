# 04 · 租约生命周期

> 目标：理解 `GetProxy` 背后什么时候复用、什么时候续租、什么时候重新拿代理。

SDK 把代理使用抽象成 lease。你不需要每次请求前都创建新 lease；更常见的做法是用 sticky key 复用同一个租约，让 SDK 在快过期时自动 renew。

## 生命周期总览

```text
GetProxy(key)
├── 本地有可用 lease              → 直接返回
├── 本地 lease 快过期 + AutoRenew  → 调 Renew
├── 本地 lease 已过期 + AutoReacquire → 重新 Create
└── 无缓存 / ForceNew             → Create 新 lease
```

服务端仍然是权威。SDK 缓存只是减少调用次数，不代表 lease 永久有效。

## sticky key

sticky key 是 SDK 本地缓存 lease 的索引。建议用业务上稳定但不敏感的 ID：

```go
proxy, err := client.GetProxy(ctx, proxyharbor.WithKey("account-a"))
```

适合：

- 同一账号希望稳定出口。
- 同一 worker 希望减少代理切换。
- 同一采集任务希望在一个 TTL 内复用连接。

不适合：

- 每次请求都必须换 IP。
- 多进程之间强一致共享同一个本地缓存。

## renew

默认 `AutoRenew=true`。当 lease 剩余时间小于 `RenewSkew` 时，SDK 会优先调用 renew：

```go
client, _ := proxyharbor.New(proxyharbor.WithLeasePolicy(proxyharbor.LeasePolicy{
    AutoRenew:     true,
    AutoReacquire: true,
    RenewSkew:     30 * time.Second,
}))
```

renew 成功后，SDK 更新本地缓存并返回新的过期时间。

## reacquire

默认 `AutoReacquire=true`。如果本地 lease 已经过期，SDK 会重新创建一个 lease。

如果你希望过期就是错误，而不是自动换代理：

```go
client, _ := proxyharbor.New(proxyharbor.WithLeasePolicy(proxyharbor.LeasePolicy{
    AutoRenew:     true,
    AutoReacquire: false,
}))
```

此时过期会返回 `ErrLeaseExpired`，可以用 `proxyharbor.IsLeaseExpired(err)` 判断。

## force new

如果你明确要换出口，绕过缓存：

```go
proxy, err := client.GetProxy(ctx, proxyharbor.WithKey("account-a"), proxyharbor.WithForceNew())
```

这会创建新 lease，不复用旧 lease。旧 lease 不会自动撤销，除非你主动 `Release` 或等待过期。

## release

主动释放 keyed lease：

```go
err := client.Release(ctx, proxyharbor.WithReleaseKey("account-a"))
```

释放指定 lease ID：

```go
err := client.Release(ctx, proxyharbor.WithReleaseLeaseID("lease_xxx"))
```

release 是优化动作，不是必须动作。短任务可以不释放，依赖 TTL 到期回收。

## 常见策略

| 场景 | 推荐策略 |
|------|----------|
| 长跑 worker | `WithKey(workerID)` + 默认 renew/reacquire |
| 账号粘性 | `WithKey(accountID)` + 较长 TTL |
| 反爬后换出口 | `WithForceNew()` |
| 严格会话 | `AutoReacquire=false`，过期由业务显式处理 |

下一步：如果你要把外部代理源持续入池，看 [05 mining/pool](./05-mining-pool.md)。

