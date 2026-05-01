# 01 · 单体快速开始

> 目标：三步启动 ProxyHarbor 单体服务，然后让 Go SDK 用本地默认配置拿到代理。

## 1. 启动服务

```bash
docker compose up -d --build
```

默认启动后会得到：

- 控制面 + 网关一体进程：`proxyharbor`
- SQLite 数据库：`/var/lib/proxyharbor/proxyharbor.db`
- 本地 secrets：`/var/lib/proxyharbor/secrets.env`

## 2. 检查就绪

```bash
curl http://localhost:18080/readyz
```

如果返回 ready，就说明 store、auth cache、selector 等关键依赖都可用。

## 3. 给 SDK 准备本地 secrets

```bash
mkdir -p data
docker compose exec -T proxyharbor cat /var/lib/proxyharbor/secrets.env > data/secrets.env
```

PowerShell：

```powershell
New-Item -ItemType Directory -Force data | Out-Null
docker compose exec -T proxyharbor cat /var/lib/proxyharbor/secrets.env | Out-File -Encoding utf8 data/secrets.env
```

然后业务代码里使用：

```go
client, err := proxyharbor.New(proxyharbor.WithLocalDefaults())
```

完整 SDK 用法见 [Go SDK 快速开始](../../sdk/go/01-getting-started.md)。

## 本地代理 endpoint

默认情况下，ProxyHarbor 会拒绝 loopback / private endpoint，防止把内网地址误暴露给租户。如果只是本地测试，启动前显式打开：

```bash
PROXYHARBOR_ALLOW_INTERNAL_PROXY_ENDPOINT=true docker compose up -d --build
```

这个开关不建议在生产环境默认开启。

