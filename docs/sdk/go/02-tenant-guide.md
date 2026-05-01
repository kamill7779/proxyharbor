# 02 · 租户侧用法

> 目标：用 `PROXYHARBOR_TENANT_KEY` 安全、稳定、复用地拿到代理 URL。

## 心智模型：sticky key + lease 缓存

SDK 把每一次 `GetProxy` 看成「为某个 sticky key 申请或复用一个 lease」。

- **key**：粘性槽位，默认 `"default"`。可以是「业务侧账号 / 任务 ID / 子任务名」。同一个 key 在多次调用时**会优先复用同一个 lease**，因此后端代理 IP 会保持稳定（粘性）。
- **lease**：服务端颁发的临时凭证，包含 `gateway_url + username/password + expires_at`。

每个 Client 内部有一份 `sync.Map[key]→leaseEntry`，配合 per-key mutex 保证「同一个 key 的并发 `GetProxy` 不会重复创建 lease」。

## 决策树（SDK 内部）

```
GetProxy(key)
├── 缓存里有 lease 且 leaseUsable() 为真 → 直接复用       (reuse)
├── 缓存里有 lease 但 leaseShouldRenew() 为真且 AutoRenew → POST :renew (renew)
├── lease 已过期且 AutoReacquire = true → POST /v1/leases (reacquire)
└── 否则 → 返回 ErrLeaseExpired                          (fail-fast)
```

判定参数：

- `leaseUsable`：`time.Until(ExpiresAt) > LeasePolicy.RenewSkew`。
- `leaseShouldRenew`：到了 `RenewBefore` 或剩余时间 ≤ `RenewSkew`。
- `RenewSkew` 默认 30 秒。

## 标准用法

### 用法 1 · 长生命周期 Client（推荐）

```go
client, err := proxyharbor.New()
if err != nil {
    return err
}
defer client.Close(ctx)

proxy, err := client.GetProxy(ctx)         // key="default"
url, err := client.GetProxyURL(ctx)        // 只要 URL
```

应用启动时构建一次，放进依赖注入容器或全局变量；进程退出前 `Close`。

### 用法 2 · 包级快捷方式

```go
url, err := proxyharbor.GetProxyURL(ctx)
```

内部 lazy 创建 `Default()` Client。适合脚本、CLI、单元测试。**长跑服务请用用法 1**，避免 `proxyharbor.SetDefault` 之外的全局可变状态。

### 用法 3 · 粘性 key（多账号 / 多任务）

```go
proxy, _ := client.GetProxy(ctx, proxyharbor.WithKey("account-a"))
proxy2, _ := client.GetProxy(ctx, proxyharbor.WithKey("account-b"))
```

不同 key 各自维护独立 lease 与代理 IP。同一个 key 多次调用复用同一个 lease。

### 用法 4 · 自定义请求目标

ProxyHarbor 服务端会**校验 lease 的 resource_ref**：网关收到的请求目标必须匹配 lease 申请时的 `target`。SDK 默认 `target=https://example.com`（占位），实际生产请按业务 URL 传：

```go
proxy, _ := client.GetProxy(ctx,
    proxyharbor.WithKey("crawler-1"),
    proxyharbor.WithTarget("https://api.example.com"),
)
```

或者全局给一个默认值：

```go
client, _ := proxyharbor.New(
    proxyharbor.WithDefaultTarget("https://api.example.com"),
)
```

> 服务端默认拒绝私有网段、loopback 等不安全 target。开发本地回环测试需要在服务端开启 `PROXYHARBOR_ALLOW_INTERNAL_PROXY_ENDPOINT=true`。

### 用法 5 · 自定义 TTL / 策略 / 标签

```go
proxy, _ := client.GetProxy(ctx,
    proxyharbor.WithKey("crawler-1"),
    proxyharbor.WithTTL(10*time.Minute),
    proxyharbor.WithPolicyID("crawler-default"),
    proxyharbor.WithSubjectLabels(map[string]string{"job": "scrape-listing"}),
)
```

### 用法 6 · 强制新建（绕开缓存）

```go
proxy, _ := client.GetProxy(ctx, proxyharbor.WithKey("rotate"), proxyharbor.WithForceNew())
```

适合「就是要换出口 IP」的场景，例如反爬封禁后强制轮换。

### 用法 7 · 主动释放

```go
client.Release(ctx)                                        // default key
client.Release(ctx, proxyharbor.WithReleaseKey("account-a"))
client.Release(ctx, proxyharbor.WithReleaseLeaseID("lease_xxx"))
```

`Release` 不强制：到期会自动回收。不主动释放但希望快速失效，建议把 `WithTTL` 调短。

## 配置租约策略

```go
client, _ := proxyharbor.New(
    proxyharbor.WithLeasePolicy(proxyharbor.LeasePolicy{
        AutoRenew:           true,             // 快过期时自动 renew
        AutoReacquire:       true,             // 过期后自动重建
        BackgroundKeepAlive: false,            // 当前无后台保活协程，预留字段
        RenewSkew:           30 * time.Second, // 提前多久触发 renew 判定
    }),
)
```

| 字段 | 推荐取值 | 行为 |
|------|----------|------|
| `AutoRenew` | `true` | 仅当剩余 TTL 进入 skew 窗口才走 renew，**不**主动后台续约 |
| `AutoReacquire` | 长跑服务 `true`，需要严格手动控制时 `false` | `false` 时过期 lease 不会自动重建，调用方收到 `ErrLeaseExpired` |
| `RenewSkew` | 默认 `30s` | 太小会落入「刚过期才 renew」的窄窗，太大会高频续约 |
| `BackgroundKeepAlive` | 当前留作扩展 | 默认 `false`，SDK 不开后台协程 |

关闭 `AutoReacquire` 的典型用法是「让上层显式决定换号节奏」：

```go
proxy, err := client.GetProxy(ctx, proxyharbor.WithKey("k1"))
if errors.Is(err, proxyharbor.ErrLeaseExpired) {
    // 上层选择：报警、切流、换 key、显式重建
}
```

## 并发模型

- **一个 Client 跨多个 goroutine 并发安全**。
- **同一 key 的 `GetProxy` 串行**（per-key mutex），不同 key 并发独立。
- 缓存命中路径（`leaseUsable=true`）**不进入** mutex，即热路径无锁。

```go
var wg sync.WaitGroup
for i := 0; i < 100; i++ {
    wg.Add(1)
    go func(i int) {
        defer wg.Done()
        url, err := client.GetProxyURL(ctx, proxyharbor.WithKey(fmt.Sprintf("worker-%d", i%8)))
        _ = err
        _ = url
    }(i)
}
wg.Wait()
```

8 个 key、100 个 goroutine 并发：服务端最多创建 8 个 lease，热路径几乎不锁。

## 常见错误响应

| 错误 | 出现条件 | 推荐处理 |
|------|----------|----------|
| `ErrNoBaseURL` | 没设 `PROXYHARBOR_BASE_URL` | fail-fast，启动期检查 |
| `ErrNoTenantKey` | 没设 tenant/admin key | fail-fast，启动期检查 |
| `ErrLeaseExpired` | 关闭 `AutoReacquire` 且 lease 过期 | 上层决策：换 key / 重试 |
| `IsUnauthorized(err)` | 401/403 | 检查 key 是否被撤销、租户是否被禁用 |
| `IsRetryable(err)` | 网络/5xx/429 | SDK 已自动重试 3 次，仍失败时上层做兜底 |

完整错误处理章节见 [07-error-handling](./07-error-handling.md)。

## 反模式

- ❌ **每次请求都 `New()`**：浪费连接，丢失 lease 缓存。
- ❌ **多个 Client 共享同一 key**：互相不知道对方持有的 lease，会触发额外的服务端创建。
- ❌ **关闭 `AutoRenew` 又不主动释放**：lease 卡到过期才换，导致请求毛刺。
- ❌ **不传 `WithTarget`**：默认 target 是占位 `https://example.com`，网关校验时会失败。
