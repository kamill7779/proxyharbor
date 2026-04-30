# 01 · 快速开始

> 目标：从零到拿到第一个可用代理 URL。预计 5 分钟。

## 前置假设

- Go 1.23 及以上。
- 已有一个 ProxyHarbor 实例（本地单体或远端服务）。本地单体启动方式见 [部署 / standalone / 快速开始](../../deployment/standalone/01-quickstart.md)。

## 1. 安装

```bash
go get github.com/kamill7779/proxyharbor/sdks/go/proxyharbor
```

## 2. 配置（任选其一）

SDK 解析配置的优先级是 **显式 Option > 环境变量 > secrets 文件 > SDK 默认值**。建议按以下其中一种姿势配齐：

### 方案 A · 环境变量（推荐 12-factor 服务）

```bash
export PROXYHARBOR_BASE_URL=http://proxyharbor:8080
export PROXYHARBOR_TENANT_KEY=phk_live_xxxxxxxxxxxx     # 租户 key
# 或者管理员场景：
# export PROXYHARBOR_ADMIN_KEY=xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
```

### 方案 B · 单体本地默认（零配置）

如果你用的是单体 ProxyHarbor + Docker Compose（`docker-compose.yaml`），ProxyHarbor 会在容器内自动生成 `/var/lib/proxyharbor/secrets.env`。把它拷贝到工作目录的 `data/secrets.env`，调用方加上 `WithLocalDefaults()` 即可：

```bash
mkdir -p data
docker compose exec -T proxyharbor cat /var/lib/proxyharbor/secrets.env > data/secrets.env
```

```go
client, err := proxyharbor.New(proxyharbor.WithLocalDefaults())
```

`WithLocalDefaults()` 做了三件事：

1. 把 `BaseURL` 默认设为 `http://localhost:18080`（与 Compose 暴露端口一致）。
2. 自动探测 `data/secrets.env` 与当前目录 `secrets.env`。
3. 不覆盖你已经显式提供的 Option 与环境变量。

### 方案 C · 显式注入

```go
client, err := proxyharbor.New(
    proxyharbor.WithBaseURL("http://proxyharbor:8080"),
    proxyharbor.WithTenantKey(os.Getenv("MY_TENANT_KEY")),
    proxyharbor.WithTimeout(5*time.Second),
)
```

## 3. 拿一个代理 URL

最短路径，三行：

```go
import "github.com/kamill7779/proxyharbor/sdks/go/proxyharbor"

url, err := proxyharbor.GetProxyURL(ctx)
// url 形如 http://username:password@gateway.host:port
```

`proxyharbor.GetProxyURL` 是包级快捷方式，内部 lazy-build 一个进程级 `Default()` Client。在长跑服务里**仍然推荐显式 `New()` 一个 Client 拿在手里**：复用连接、复用 lease 缓存、显式 `Close()`。

```go
client, err := proxyharbor.New()
if err != nil {
    log.Fatal(err)
}
defer client.Close(ctx)

proxy, err := client.GetProxy(ctx)
if err != nil {
    log.Fatal(err)
}

fmt.Println(proxy.URL)
fmt.Println(proxy.LeaseID, proxy.ExpiresAt)
```

## 4. 把代理塞进 `net/http`

```go
proxyURL, _ := url.Parse(proxy.URL)
hc := &http.Client{
    Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
    Timeout:   30 * time.Second,
}
resp, err := hc.Get("https://example.com")
```

更多 HTTP 客户端样板见 [08-recipes](./08-recipes.md)。

## 5. 干净结束

```go
defer func() {
    _ = client.Release(ctx)  // 主动撤销 default key 的 lease
    _ = client.Close(ctx)
}()
```

`Release` 不是必须的：lease 到期后服务端会自动回收。但是在「短任务、不希望占着代理」的场景里建议主动调用。

## 你已经掌握的事情

- 安装、配置、拿到代理 URL 的最短闭环。
- SDK 三种配置注入方式与优先级。
- 包级快捷方式 vs 长生命周期 Client 的取舍。

下一步：

- 想细看租户侧能力 → [02-tenant-guide](./02-tenant-guide.md)
- 想给平台加 provider/proxy → [03-admin-guide](./03-admin-guide.md)
