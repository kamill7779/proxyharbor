<div align="center">
  <img src="docs/logo.png" alt="ProxyHarbor Logo" width="480"/>
</div>

<div align="center">

**适用于小型业务接入的轻量云原生代理池**

[![Go Version](https://img.shields.io/badge/Go-1.23+-00ADD8?logo=go)](go.mod)
[![License](https://img.shields.io/badge/License-MIT-orange)](LICENSE)
[![English](https://img.shields.io/badge/README-English-blue)](README.en.md)

</div>

---

ProxyHarbor 是一个单 binary 的代理池控制面与网关。它提供代理库存管理、动态租户 Key、租约颁发、HTTP/HTTPS 网关校验和 SQLite 单体持久化。产品边界保持 **single-first**：本地和小规模部署默认不需要 MySQL、Redis 或手工密钥配置；需要高可用时再切换到 MySQL + Redis。

## 功能特性

- **单体优先**：默认 `role=all`、`storage=sqlite`、`selector=local`，一个进程即可完成控制面与网关。
- **零配置本地启动**：未提供 `PROXYHARBOR_ADMIN_KEY` / `PROXYHARBOR_KEY_PEPPER` 时自动生成并持久化到 `secrets.env`。
- **全局代理库存**：Admin 管理 Provider / Proxy；租户只拿租约，不读取代理 endpoint 列表。
- **动态租户 Key**：Admin API 签发、撤销租户 Key；明文 Key 只在签发响应中返回一次。
- **租约网关**：租约绑定 `resource_ref`，网关请求目标必须匹配租约资源。
- **本地平滑加权轮询**：单体模式使用进程内 selector；HA 模式可使用 Redis-backed zfair。
- **SQLite 运维闭环**：内置 `doctor`、`init`、`backup`、`restore`、`retention`。
- **Go SDK**：支持租约缓存、自动续租/重建，并提供 `WithLocalDefaults()` 读取本地单体默认配置。

## 快速开始

> 推荐本地和小规模部署使用默认 Docker Compose：SQLite 持久化 + 本地 selector + 自动生成本地 secrets。

### 单体最短路径

ProxyHarbor 的单体默认路径只需要三步：

```bash
docker compose up -d --build
mkdir -p data
docker compose exec -T proxyharbor cat /var/lib/proxyharbor/secrets.env > data/secrets.env
```

然后在业务代码里引入 SDK，直接使用本地默认配置：

```go
client, err := proxyharbor.New(proxyharbor.WithLocalDefaults())
```

这条路径默认使用 `sqlite` 持久化、进程内 selector、自动生成的本地 Admin Key / pepper，不需要 MySQL、Redis 或手写密钥。完整示例见下方步骤 4-5。

### 1. 启动单体服务

```bash
docker compose up -d --build
```

默认启动内容：
- `proxyharbor`：控制面 + 网关一体进程
- SQLite 数据库：`/var/lib/proxyharbor/proxyharbor.db`
- 自动生成的本地密钥：`/var/lib/proxyharbor/secrets.env`

### 2. 检查就绪状态

```bash
curl http://localhost:18080/readyz
```

### 3. 准备 Go SDK 本地 secrets

默认单体启动会在容器内生成 `/var/lib/proxyharbor/secrets.env`。把它复制到当前业务工程的 `data/secrets.env`，SDK 就能用 `WithLocalDefaults()` 自动读取：

```bash
mkdir -p data
docker compose exec -T proxyharbor cat /var/lib/proxyharbor/secrets.env > data/secrets.env
```

PowerShell：

```powershell
New-Item -ItemType Directory -Force data | Out-Null
docker compose exec -T proxyharbor cat /var/lib/proxyharbor/secrets.env | Out-File -Encoding utf8 data/secrets.env
```

### 4. 引入 Go SDK

```bash
go get github.com/kamill7779/proxyharbor/sdks/go/proxyharbor
```

### 5. 添加代理并直接使用

下面是全 default 最短闭环：默认连接 `http://localhost:18080`，从 `data/secrets.env` 读取本地 Admin Key，用 SDK 添加一个代理，再获取可用于 HTTP client 的代理 URL。把示例 endpoint 换成你的真实代理；这里使用 IP 字面量，避免文档示例被本机 DNS 污染影响。

```go
package main

import (
    "context"
    "fmt"
    "log"

    "github.com/kamill7779/proxyharbor/sdks/go/proxyharbor"
)

func main() {
    ctx := context.Background()

    client, err := proxyharbor.New(proxyharbor.WithLocalDefaults())
    if err != nil {
        log.Fatal(err)
    }

    if _, err := client.AddProxy(ctx, "http://203.0.113.10:8080"); err != nil {
        log.Fatal(err)
    }

    proxyURL, err := client.GetProxyURL(ctx)
    if err != nil {
        log.Fatal(err)
    }

    fmt.Println(proxyURL)
}
```

> 本地测试回环地址或内网代理 endpoint 时，启动前设置 `PROXYHARBOR_ALLOW_INTERNAL_PROXY_ENDPOINT=true`。该开关只放开 proxy endpoint 注册，不放开租约 target 的 SSRF 校验。

### 6. 显式指定本地配置（可选）

```go
client, err := proxyharbor.New(
    proxyharbor.WithBaseURL("http://localhost:18080"),
    proxyharbor.WithSecretsFile("./data/secrets.env"),
)
```
## API Shape

| 资源 | 说明 | 典型接口 |
| --- | --- | --- |
| Tenant | 租户身份边界 | `POST /admin/tenants` |
| Tenant Key | 租户访问凭证 | `POST /admin/tenants/{id}/keys` |
| Provider | 全局代理来源 | `POST /v1/providers` |
| Proxy | 全局代理节点 | `POST /v1/proxies` |
| Lease | 租户可使用的代理租约 | `POST /v1/leases` |
| Gateway Validate | 网关校验租约与目标 | `GET /v1/gateway/validate` |

## 启动模式

### 单体 SQLite（默认）

```bash
docker compose up -d --build
```

适合本地开发、小规模部署、单节点服务。默认不需要 Redis；`PROXYHARBOR_ADMIN_KEY` 与 `PROXYHARBOR_KEY_PEPPER` 缺失时自动生成。

### 外部 MySQL + Redis（HA）

```bash
export PROXYHARBOR_ADMIN_KEY=$(openssl rand -hex 32)
export PROXYHARBOR_KEY_PEPPER=$(openssl rand -hex 32)
export PROXYHARBOR_MYSQL_DSN='ph:REPLACE_ME@tcp(mysql.svc:3306)/proxyharbor?parseTime=true&loc=UTC'
export PROXYHARBOR_REDIS_ADDR='redis:6379'
docker compose -f docker-compose.ha.yaml up -d --build
```

HA 模式需要显式 Secret，不自动生成默认密钥。

### HA 本机验证路径

需要重复验证 3 实例 + MySQL + Redis + LB 的本机 HA 拓扑时，使用正式 runner，而不是临时脚本。完整 release-candidate 矩阵见 [v1.0.0 release runbook](docs/runbooks/release-v1.0.0.md)；首页只保留核心入口：

```bash
docker build --pull=false -t proxyharbor:ha-test .
go run ./tools/haruntimecheck -docker -docker-skip-build -timeout 8m
go run ./tools/hapressure -docker -docker-skip-build -docker-internal -mode soak -concurrency 500 -duration 10m -warmup-leases 500 -timeout 20m
```

`-docker-internal` 会把压测 worker 放进 compose 网络，避免 Docker Desktop / Windows / macOS 上宿主机端口映射的连接拒绝噪声。完整 HA 压测命令、压力分项和记录格式见 [HA 压测 runbook](docs/runbooks/ha-pressure.md)。

当前 HA 热路径在本机 Docker 拓扑（3 个 ProxyHarbor + MySQL + Redis + nginx，`-docker-internal`）下的可用性证据：

| 场景 | 结果 |
| --- | --- |
| `500` 并发、`10m` mixed soak | `1,805,539` 次控制面请求，约 `3.0k req/s`，错误率 `0.252%`，达到 `<0.5%` soak 门槛 |
| 状态分布 | `200=1,200,818`，`201=600,165`，`409=2,121`，`500=52`，`504=2,383`，无 `502` |

这些数字只衡量租约创建、续租和网关校验等控制面热路径，不代表真实代理数据流吞吐。当前 `500` 并发、`10m` mixed soak 可用性门槛已达成；严格的单操作 p95/p99 延迟门槛仍未完全达成，作为 P1 性能工作跟进。完整证据见 [v0.5.5 记录](docs/versions/v0.5.5.md) 和 [v1.0.0 readiness](docs/versions/v1.0.0.md)。

### 直接运行二进制

```bash
go run ./cmd/proxyharbor \
  -storage sqlite \
  -sqlite-path data/proxyharbor.db
```

## CLI 运维

```bash
proxyharbor doctor [flags]
proxyharbor init [flags]
proxyharbor backup [flags]
proxyharbor restore [flags]
proxyharbor retention [flags]
```

- `doctor`：检查存储、SQLite schema、Redis 要求、admin key / pepper、路径权限。
- `init`：初始化 SQLite schema，幂等。
- `backup` / `restore`：SQLite 单体备份与恢复。
- `retention`：清理审计和用量事件，默认 dry-run，`--execute` 才会删除。

## Helm Secret

Helm 仍然坚持生产 Secret-first，不在模板中生成或渲染明文密钥：

```bash
kubectl create secret generic proxyharbor-credentials \
  --from-literal=admin-key="$(openssl rand -hex 32)" \
  --from-literal=pepper="$(openssl rand -hex 32)"

helm install proxyharbor charts/proxyharbor \
  --set auth.existingSecret=proxyharbor-credentials
```

HA 示例使用：

```bash
helm install proxyharbor charts/proxyharbor -f charts/proxyharbor/examples/dynamic-ha-values.yaml
```

## 数据模型

### Provider（提供商）

平台全局资源，表示代理来源或数据中心。Proxy 不传 `provider_id` 时归入默认 Provider。

### Proxy（代理节点）

平台全局资源，包含 endpoint、weight、health_score、熔断状态与健康反馈字段。租户接口不暴露代理 endpoint 列表。

### Policy（策略）

当前以默认策略为主；创建租约时可省略 `policy_id`。

### Lease（租约）

租户资源，绑定 subject、resource_ref、proxy_id、过期时间和不可逆 password hash。明文 password 只在创建租约响应中返回一次。

## 调度与健康模型

### local selector

单体默认 selector。进程内维护平滑加权轮询状态，只选择健康、正权重、正健康分、未处于熔断窗口的代理。

### zfair selector

HA 模式可使用 Redis ZSET + Lua 原子选择，结合权重、延迟和健康状态做共享调度。

### 健康评分

成功会提升健康分；连接失败、超时、认证/协议失败会降低健康分；连续失败会触发熔断与冷却。

## 配置参考

| 变量 | 说明 |
| --- | --- |
| `PROXYHARBOR_STORAGE` | `sqlite` 单体默认；`mysql` 用于 HA；`memory` 仅 dev/demo/CI |
| `PROXYHARBOR_SQLITE_PATH` | SQLite 数据库路径 |
| `PROXYHARBOR_SECRETS_FILE` | 本地 env-style secrets 文件；默认与 SQLite DB 同目录 |
| `PROXYHARBOR_AUTO_SECRETS` | SQLite 单体缺密钥时自动生成，默认 `true` |
| `PROXYHARBOR_ADMIN_KEY` | Bootstrap admin key，至少 32 bytes；单体可自动生成 |
| `PROXYHARBOR_KEY_PEPPER` | 租户 Key hash pepper，至少 32 bytes；单体可自动生成 |
| `PROXYHARBOR_MYSQL_DSN` | MySQL DSN，`storage=mysql` 时必填 |
| `PROXYHARBOR_REDIS_ADDR` | Redis 地址；HA/zfair 需要 |
| `PROXYHARBOR_SELECTOR` | `local` 或 `zfair` |
| `PROXYHARBOR_SELECTOR_REDIS_REQUIRED` | 是否强制 selector 依赖 Redis |
| `PROXYHARBOR_AUTH_REFRESH_INTERVAL` | 动态 Key 刷新间隔，最大 5s |
| `PROXYHARBOR_ALLOW_INTERNAL_PROXY_ENDPOINT` | 是否允许注册 private/loopback proxy endpoint |

## 打包与部署

```bash
docker build -t proxyharbor:local .
helm install proxyharbor charts/proxyharbor --set auth.existingSecret=proxyharbor-credentials
```

## 贡献指南

Issues 和 PRs 都欢迎。提交前请先看 [CONTRIBUTING.md](CONTRIBUTING.md)。单体能力优先保持轻量、可复制、可验证；HA 能力坚持显式 Secret 和云原生部署边界。

## 联系方式

- GitHub Issues: <https://github.com/kamill7779/proxyharbor/issues>
- Author: Kamill

## 许可证

This project is licensed under the [MIT License](LICENSE).

