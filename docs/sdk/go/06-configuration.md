# 06 · 配置参考

> 目标：知道 SDK 从哪里读取配置，以及什么时候该显式覆盖默认值。

## 优先级

SDK 配置优先级是：

```text
显式 Option > 环境变量 > secrets 文件 > SDK 默认值
```

## 常用 Option

| Option | 用途 |
|--------|------|
| `WithBaseURL` | ProxyHarbor 服务地址 |
| `WithTenantKey` | 租户侧 lease API 凭证 |
| `WithAdminKey` | admin 侧库存 API 凭证 |
| `WithSecretsFile` | 读取 env-style secrets 文件 |
| `WithLocalDefaults` | 本地单体默认：`localhost:18080` + `data/secrets.env` |
| `WithDefaultKey` | 默认 sticky lease key |
| `WithDefaultProviderID` | admin 入池默认 provider |
| `WithTimeout` | 单次 HTTP 请求超时 |
| `WithRetry` | SDK 传输重试策略 |
| `WithHTTPClient` | 注入自定义 HTTP client |
| `WithLeasePolicy` | renew / reacquire 策略 |
| `WithDefaultTarget` | 默认 lease resource target |

## 环境变量

| 变量 | 说明 |
|------|------|
| `PROXYHARBOR_BASE_URL` | 服务地址，例如 `http://localhost:18080` |
| `PROXYHARBOR_TENANT_KEY` | 租户 key |
| `PROXYHARBOR_ADMIN_KEY` | admin key |
| `PROXYHARBOR_SECRETS_FILE` | 本地 secrets 文件 |

## 本地默认

单体 Docker 模式推荐：

```go
client, err := proxyharbor.New(proxyharbor.WithLocalDefaults())
```

它会默认使用：

- `BaseURL=http://localhost:18080`
- `data/secrets.env`

如果环境变量已经显式设置，环境变量仍然优先。

## RetryConfig

```go
client, _ := proxyharbor.New(proxyharbor.WithRetry(proxyharbor.RetryConfig{
    MaxAttempts: 3,
    MinBackoff:  100 * time.Millisecond,
    MaxBackoff:  2 * time.Second,
}))
```

SDK 只重试 transport error 和明显可重试 HTTP 状态，例如 408、429、5xx。400、401、403、404 不会重试。

## LeasePolicy

默认策略：

```go
proxyharbor.LeasePolicy{
    AutoRenew:           true,
    AutoReacquire:       true,
    BackgroundKeepAlive: false,
}
```

`BackgroundKeepAlive` 当前预留给后续后台保活能力。当前主路径是在 `GetProxy` 调用时按需 renew / reacquire。

