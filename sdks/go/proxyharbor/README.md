# ProxyHarbor Go SDK

`github.com/kamill7779/proxyharbor/sdks/go/proxyharbor` 是 ProxyHarbor 服务的官方 Go SDK。

SDK 只做一件事：**让用户用最少的代码拿到一个可用代理 URL**。

## 安装

```bash
go get github.com/kamill7779/proxyharbor/sdks/go/proxyharbor
```

## 快速开始

### 1. 配置环境变量

```bash
export PROXYHARBOR_BASE_URL=http://proxyharbor:8080
export PROXYHARBOR_TENANT_KEY=tenant_key_xxx
```

### 2. 一行代码拿代理

```go
import "github.com/kamill7779/proxyharbor/sdks/go/proxyharbor"

url, err := proxyharbor.GetProxyURL(ctx)
```

返回的 URL 格式为 `http://username:password@host:port`，直接作为 HTTP 客户端代理设置即可。

---

## 租户侧用法

### 全默认路径

环境变量配好后，一行代码：

```go
import "github.com/kamill7779/proxyharbor/sdks/go/proxyharbor"

// 只要 URL
url, err := proxyharbor.GetProxyURL(ctx)

// 需要更多信息（LeaseID、ProxyID、过期时间）
proxy, err := proxyharbor.GetProxy(ctx)
```

### 复用 Client（推荐）

进程启动时创建一个长生命周期 Client，全局复用：

```go
client, err := proxyharbor.New()
if err != nil {
    return err
}
defer client.Close(ctx)

// 以后到处用
proxy, err := client.GetProxy(ctx)
```

### 粘性用法

不同业务场景用不同 key，SDK 自动维护 key → lease 的粘性：

```go
// 为 account-a 分配一个代理，后续调用尽量复用同一个 lease
proxy, err := client.GetProxy(ctx, proxyharbor.WithKey("account-a"))

// account-b 独立分配
proxy, err = client.GetProxy(ctx, proxyharbor.WithKey("account-b"))
```

### 显式配置

```go
client, err := proxyharbor.New(
    proxyharbor.WithBaseURL("http://proxyharbor:8080"),
    proxyharbor.WithTenantKey("tenant_key_xxx"),
    proxyharbor.WithTimeout(5 * time.Second),
)
```

### 释放代理

主动撤销租约：

```go
// 释放 default key 的 lease
client.Release(ctx)

// 释放 account-a 的 lease
client.Release(ctx, proxyharbor.WithReleaseKey("account-a"))
```

---

## 管理侧用法

管理侧配好环境变量：

```bash
export PROXYHARBOR_BASE_URL=http://proxyharbor:8080
export PROXYHARBOR_ADMIN_KEY=admin_key_xxx
```

### 最简 admin

```go
c, _ := proxyharbor.New()

// 代理入池：只需 ctx + endpoint
c.AddProxy(ctx, "http://1.2.3.4:3128")
```

### 固定 provider 入池

```go
c.AddProvider(ctx, "my-dc")
c.AddProxyWithProvider(ctx, "http://1.2.3.4:3128", "my-dc")
```

### 底层 API（高级用法）

需要完整字段时用底层 API：

```go
c.Providers.Create(ctx, proxyharbor.ProviderDTO{
    ID:   "my-dc",
    Name: "数据中心出口",
    Type: "static",
})
c.Proxies.Upsert(ctx, proxyharbor.ProxyDTO{
    ProviderID: "my-dc",
    Endpoint:   "http://1.2.3.4:3128",
    Weight:     10,
})
```

### 管理代理

```go
// 获取
p, _ := c.Proxies.Get(ctx, "proxy-id")

// 删除
c.Proxies.Delete(ctx, "proxy-id")
```

---

## 自定义参数

### Client 配置选项

| Option | 说明 | 环境变量 |
|--------|------|----------|
| `WithBaseURL(url)` | ProxyHarbor 地址 | `PROXYHARBOR_BASE_URL` |
| `WithTenantKey(key)` | 租户 key | `PROXYHARBOR_TENANT_KEY` |
| `WithAdminKey(key)` | 管理 key | `PROXYHARBOR_ADMIN_KEY` |
| `WithDefaultKey(key)` | 默认 sticky key | - |
| `WithDefaultProviderID(id)` | 默认 provider ID | - |
| `WithDefaultTarget(target)` | 默认请求目标 | - |
| `WithTimeout(d)` | HTTP 超时 | - |
| `WithUserAgent(ua)` | User-Agent | - |
| `WithHTTPClient(hc)` | 自定义 http.Client | - |

### GetProxy 调用选项

| Option | 说明 |
|--------|------|
| `WithKey(key)` | sticky key（默认 `"default"`） |
| `WithTarget(target)` | 请求目标 URL |
| `WithPolicyID(id)` | 指定策略 ID |
| `WithTTL(d)` | 指定 lease TTL |
| `WithSubjectID(id)` | 自定义 subject ID |
| `WithSubjectLabels(l)` | 附加 subject labels |
| `WithForceNew()` | 强制新建 lease（跳过缓存） |

### 租约策略

```go
proxyharbor.WithLeasePolicy(proxyharbor.LeasePolicy{
    AutoRenew:           true,                // 快过期时自动续约
    AutoReacquire:       true,                // 过期后自动重建
    BackgroundKeepAlive: false,               // 不后台保活
    RenewSkew:           30 * time.Second,    // 提前多久续约
})
```

关闭自动重建时，过期 lease 返回 `ErrLeaseExpired`：

```go
proxyharbor.WithLeasePolicy(proxyharbor.LeasePolicy{
    AutoRenew:     true,
    AutoReacquire: false,
})
```

### 重试配置

```go
proxyharbor.WithRetry(proxyharbor.RetryConfig{
    MaxAttempts: 3,
    MinBackoff:  100 * time.Millisecond,
    MaxBackoff:  2 * time.Second,
})
```

---

## 错误处理

```go
// 401 / 403
if proxyharbor.IsUnauthorized(err) { }

// 404
if proxyharbor.IsNotFound(err) { }

// 网络错误、5xx、429，适合上层重试
if proxyharbor.IsRetryable(err) { }

// AutoReacquire 已关闭且 lease 过期
if proxyharbor.IsLeaseExpired(err) { }

// 获取原始 API 错误
var apiErr *proxyharbor.APIError
if errors.As(err, &apiErr) {
    fmt.Println(apiErr.StatusCode, apiErr.Code)
}
```

---

## 配置优先级

```
显式 Option 参数 > 环境变量 > SDK 默认值
```

### 环境变量

| 变量 | 说明 |
|------|------|
| `PROXYHARBOR_BASE_URL` | ProxyHarbor 地址 |
| `PROXYHARBOR_TENANT_KEY` | 租户 key |
| `PROXYHARBOR_ADMIN_KEY` | 管理 key |

### SDK 默认值

| 字段 | 默认值 |
|------|--------|
| `DefaultKey` | `"default"` |
| `DefaultProviderID` | `"default"` |
| `DefaultTarget` | `"https://example.com"` |
| `Timeout` | `10s` |
| `UserAgent` | `"proxyharbor-go"` |
| `Retry.MaxAttempts` | `3` |
| `Retry.MinBackoff` | `100ms` |
| `Retry.MaxBackoff` | `2s` |
| `LeasePolicy.RenewSkew` | `30s` |

---

## 返回值

```go
type Proxy struct {
    URL       string     // 已嵌入凭据的网关 URL
    Key       string     // sticky key
    LeaseID   string     // 租约 ID
    ProxyID   string     // 底层代理 ID
    ExpiresAt time.Time  // 过期时间
}
```

---

## 完整示例

```go
package main

import (
    "context"
    "log"
    "net/http"
    "net/url"
    "time"

    "github.com/kamill7779/proxyharbor/sdks/go/proxyharbor"
)

func main() {
    ctx := context.Background()

    // 环境变量配好后直接 New()
    client, err := proxyharbor.New()
    if err != nil {
        log.Fatal(err)
    }
    defer client.Close(ctx)

    // 拿代理
    proxyURL, err := client.GetProxyURL(ctx, proxyharbor.WithKey("my-service"))
    if err != nil {
        log.Fatal(err)
    }

    // 直接塞进 http.Client
    proxy, _ := url.Parse(proxyURL)
    httpClient := &http.Client{
        Transport: &http.Transport{Proxy: http.ProxyURL(proxy)},
        Timeout:   30 * time.Second,
    }

    resp, err := httpClient.Get("https://example.com")
    if err != nil {
        log.Fatal(err)
    }
    defer resp.Body.Close()

    // 用完释放
    client.Release(ctx, proxyharbor.WithReleaseKey("my-service"))
}
```
