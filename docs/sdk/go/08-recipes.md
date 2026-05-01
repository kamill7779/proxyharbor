# 08 · Recipes

> 目标：把 SDK 接到真实业务代码里。

## net/http 使用代理

```go
proxyURL, err := client.GetProxyURL(ctx, proxyharbor.WithKey("worker-1"))
if err != nil {
    return err
}

u, _ := url.Parse(proxyURL)
httpClient := &http.Client{
    Transport: &http.Transport{Proxy: http.ProxyURL(u)},
    Timeout:   30 * time.Second,
}

req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.com", nil)
resp, err := httpClient.Do(req)
```

## 多账号粘性代理

```go
for _, account := range accounts {
    proxy, err := client.GetProxy(ctx, proxyharbor.WithKey(account.ID))
    if err != nil {
        return err
    }
    _ = runAccountJob(ctx, account, proxy.URL)
}
```

## 失败后强制换出口

```go
proxy, err := client.GetProxy(ctx,
    proxyharbor.WithKey("worker-1"),
    proxyharbor.WithForceNew(),
)
```

适合目标站点明确封禁当前出口的场景。不要把它作为普通重试的第一选择，否则会制造过多 lease。

## admin 入池后租户使用

```go
admin, _ := proxyharbor.New(proxyharbor.WithLocalDefaults())
_, _ = admin.AddProxy(ctx, "http://1.2.3.4:3128")

tenant, _ := proxyharbor.New(proxyharbor.WithLocalDefaults())
proxyURL, _ := tenant.GetProxyURL(ctx)
fmt.Println(proxyURL)
```

单体本地默认路径下，SDK 会用 admin key fallback 到 `X-On-Behalf-Of: default`，方便演示和本地开发。生产租户进程建议使用 tenant key。

## 长跑服务退出前释放

```go
client, _ := proxyharbor.New()
defer client.Close(context.Background())

proxy, _ := client.GetProxy(ctx, proxyharbor.WithKey("worker-1"))
_ = proxy
```

`Close` 会释放当前 client 缓存中的 lease。进程异常退出时也没关系，lease 会按 TTL 到期回收。

