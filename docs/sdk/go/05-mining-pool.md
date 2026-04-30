# 05 · 挖矿 / 采集池

> 目标：把外部代理来源接入 ProxyHarbor，让 admin 侧持续把候选代理入池。

`Pool` 是 SDK 里的 admin 侧辅助工具。它不负责“使用代理”，只负责“发现代理并提交到 ProxyHarbor 库存”。

## 最短示例

```go
client, _ := proxyharbor.New(proxyharbor.WithAdminKey(os.Getenv("PROXYHARBOR_ADMIN_KEY")))

miner := proxyharbor.StaticMiner{
    Candidates: []proxyharbor.Candidate{
        {Endpoint: "http://1.2.3.4:3128"},
        {Endpoint: "http://5.6.7.8:8080", ProviderID: "datacenter-cn"},
    },
}

pool, _ := proxyharbor.NewPool(client, miner)
err := pool.Run(ctx)
```

## Candidate

`Candidate` 是外部代理源提交给 ProxyHarbor 的最小单位：

```go
proxyharbor.Candidate{
    Endpoint:   "http://1.2.3.4:3128",
    ProviderID: "datacenter-cn",
    Labels:     map[string]string{"source": "crawler"},
    Weight:     10,
}
```

endpoint 会经过 `NormalizeProxyEndpoint`，没有 scheme 时默认补 `http://`。

## 自定义 miner

如果你的代理来自文件、队列、第三方 API，可以实现 `Miner`：

```go
miner := proxyharbor.MinerFunc(func(ctx context.Context, sink proxyharbor.Sink) error {
    for _, endpoint := range endpointsFromVendor() {
        if err := sink.Submit(ctx, proxyharbor.Candidate{Endpoint: endpoint}); err != nil {
            return err
        }
    }
    return nil
})
```

## 去重

`Deduper` 用 endpoint 做本地去重，适合第三方源重复吐出相同代理的场景：

```go
pool, _ := proxyharbor.NewPool(client, miner)
deduped := &proxyharbor.Deduper{Sink: pool}
_ = miner.Mine(ctx, deduped)
```

## 定时采集

`IntervalMiner` 可以周期性运行一个 miner：

```go
miner := proxyharbor.IntervalMiner{
    Every:  time.Minute,
    Miner:  vendorMiner,
}
pool, _ := proxyharbor.NewPool(client, miner)
_ = pool.Run(ctx)
```

## 注意事项

- Pool 需要 admin key，不要放到普通业务进程里。
- Pool 只负责入池，不保证代理一定可用；健康评分由服务端维护。
- 本地测试 private / loopback endpoint 时，服务端需要显式开启 `PROXYHARBOR_ALLOW_INTERNAL_PROXY_ENDPOINT=true`。

