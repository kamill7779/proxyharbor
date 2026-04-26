<div align="center">
  <img src="docs/logo.png" alt="ProxyHarbor Logo" width="480"/>
</div>

<div align="center">

**适用于小型业务接入的轻量代理池**

[![Go Version](https://img.shields.io/badge/Go-1.23+-00ADD8?logo=go)](go.mod)
[![License](https://img.shields.io/badge/License-MIT-orange)](LICENSE)
[![English](https://img.shields.io/badge/README-English-blue)](README.en.md)

</div>

---

ProxyHarbor 是一个面向小型业务场景的轻量代理池服务，提供代理目录管理、租约调度与 HTTP/HTTPS 网关转发能力。它将控制面（代理注册、策略管理、租约颁发）与数据面（HTTP 正向代理 + CONNECT 隧道）合并在单一进程中，依赖栈仅为 **MySQL + Redis**，部署门槛极低。

## 功能特性

- **控制面 API**：管理 Provider、Proxy、Policy、Lease，支持 Catalog 查询
- **HTTP 网关**：同时支持普通 HTTP 正向代理请求和 CONNECT 隧道（HTTPS）
- **zfair 公平调度**：Redis ZSET + Lua 原子操作，按权重和健康信号公平分配租约
- **健康评分**：成功 +5 分，连接失败 -10、超时 -15、认证/协议失败 -30，连续 3 次失败触发熔断，冷却 30 s～5 min 指数退避
- **租约系统**：时间限制凭据，密码仅在创建时一次性返回，持久化层存储不可逆哈希，支持幂等键去重
- **Redis 缓存**：Catalog 和 Lease 双缓存，降低 MySQL 热点压力
- **角色分离**：支持 `all` / `controller` / `gateway` 三种启动角色，可独立拆分部署

## 快速开始

> 推荐使用 Docker Compose 一键启动，内置 MySQL 和 Redis 无需额外准备。

### 1. 启动全部服务

```bash
docker compose --profile bundle up -d --build
```

启动内容：
- `mysql`：代理目录与健康状态持久化
- `redis`：zfair 调度状态 + Lease/Catalog 缓存
- `proxyharbor`：控制面 + 网关一体进程

默认开发 API Key：

```env
PROXYHARBOR_TENANT_KEYS=default:changeme1234567890abcdef
```

**对外暴露前务必修改为强密钥**：

```bash
cp .env.example .env
```

```env
PROXYHARBOR_TENANT_KEYS=default:replace-with-a-random-secret-0001
PROXYHARBOR_MYSQL_DSN=proxyharbor:proxyharbor@tcp(mysql:3306)/proxyharbor?parseTime=true&loc=UTC
PROXYHARBOR_REDIS_ADDR=redis:6379
```

### 2. 检查服务就绪状态

```bash
curl http://localhost:8080/readyz
```

```json
{"role":"all","status":"ready"}
```

### 3. 注册 Provider

```bash
curl -H 'ProxyHarbor-Key: changeme1234567890abcdef' \
  -H 'Content-Type: application/json' \
  -d '{"id":"static-main","type":"static","name":"我的代理池","enabled":true}' \
  http://localhost:8080/v1/providers
```

### 4. 添加代理节点

```bash
curl -H 'ProxyHarbor-Key: changeme1234567890abcdef' \
  -H 'Content-Type: application/json' \
  -d '{"id":"proxy-1","provider_id":"static-main","endpoint":"http://proxy1.example.com:8080","healthy":true,"weight":10}' \
  http://localhost:8080/v1/proxies
```

> 本地测试回环地址或内网 IP 时，需先设置 `PROXYHARBOR_ALLOW_INTERNAL_PROXY_ENDPOINT=true`

批量添加（Bash）：

```bash
for item in \
  'proxy-1 http://proxy1.example.com:8080 100' \
  'proxy-2 http://proxy2.example.com:8080 80' \
  'proxy-3 http://proxy3.example.com:8080 50'
do
  set -- $item
  curl -H 'ProxyHarbor-Key: changeme1234567890abcdef' \
    -H 'Content-Type: application/json' \
    -d "{\"id\":\"$1\",\"provider_id\":\"static-main\",\"endpoint\":\"$2\",\"healthy\":true,\"weight\":$3}" \
    http://localhost:8080/v1/proxies
done
```

批量添加（PowerShell）：

```powershell
$proxies = @(
  @{ id = 'proxy-1'; endpoint = 'http://proxy1.example.com:8080'; weight = 100 },
  @{ id = 'proxy-2'; endpoint = 'http://proxy2.example.com:8080'; weight = 80 },
  @{ id = 'proxy-3'; endpoint = 'http://proxy3.example.com:8080'; weight = 50 }
)
foreach ($proxy in $proxies) {
  $body = @{ id=$proxy.id; provider_id='static-main'; endpoint=$proxy.endpoint; healthy=$true; weight=$proxy.weight } | ConvertTo-Json -Compress
  Invoke-RestMethod -Method Post -Uri 'http://localhost:8080/v1/proxies' -Headers @{'ProxyHarbor-Key'='changeme1234567890abcdef'} -ContentType 'application/json' -Body $body
}
```

### 5. 创建策略

租约颁发至少需要一条启用的策略：

```bash
curl -H 'ProxyHarbor-Key: changeme1234567890abcdef' \
  -H 'Content-Type: application/json' \
  -d '{"id":"default","name":"默认策略","enabled":true,"ttl_seconds":1800}' \
  http://localhost:8080/v1/policies
```

### 6. 创建租约

```bash
curl -H 'ProxyHarbor-Key: changeme1234567890abcdef' \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: demo-lease-1' \
  -d '{"subject":{"subject_type":"user","subject_id":"local-dev"},"resource_ref":{"kind":"url","id":"https://example.com"},"ttl_seconds":600}' \
  http://localhost:8080/v1/leases
```

返回的 `lease_id` 作为代理用户名，`password` 作为代理密码（**仅此一次返回，请立即保存**）。

## 启动模式

### 内置依赖（推荐本地开发）

```bash
docker compose --profile bundle up -d --build
```

### 外部 MySQL + Redis

```bash
cp .env.example .env
# 编辑 .env 填写外部连接信息
docker compose --profile app up -d --build
```

### 直接运行二进制

```bash
PROXYHARBOR_TENANT_KEYS=default:replace-with-a-random-secret-0001 \
PROXYHARBOR_STORAGE=mysql \
PROXYHARBOR_MYSQL_DSN='proxyharbor:proxyharbor@tcp(127.0.0.1:3306)/proxyharbor?parseTime=true&loc=UTC' \
PROXYHARBOR_REDIS_ADDR='127.0.0.1:6379' \
PROXYHARBOR_SELECTOR=zfair \
go run ./cmd/proxyharbor
```

## 租户身份模型

ProxyHarbor v0.1.4 推荐使用 `PROXYHARBOR_TENANT_KEYS`。每个条目形如 `tenant_id:key`，服务端在收到 `ProxyHarbor-Key` 后反查出可信的 `principal.TenantID`。

- 控制面调用只需要传 `ProxyHarbor-Key`，不需要再传 `ProxyHarbor-Tenant` 或 `tenant_id`。
- 如果旧客户端仍传 `ProxyHarbor-Tenant` 或 query `tenant_id`，它必须与 key 绑定的租户一致，否则返回 `403 tenant_mismatch`。
- `PROXYHARBOR_AUTH_KEY` 仅保留给 legacy 单 Key 部署；不要与 `PROXYHARBOR_TENANT_KEYS` 同时设置。

## 数据模型

### Provider（提供商）

将同一来源的代理节点归组管理，静态手动维护使用 `type: static`：

```json
{"id":"static-main","type":"static","name":"我的代理池","enabled":true}
```

### Proxy（代理节点）

描述一个上游代理端点：

| 字段 | 说明 |
| --- | --- |
| `id` | 租户内唯一 ID |
| `provider_id` | 归属的 Provider |
| `endpoint` | 上游代理 URL |
| `healthy` | 是否参与调度 |
| `weight` | zfair 调度相对权重，数值越大获得租约越多 |
| `health_score` | 健康分值，由网关反馈自动维护 |
| `circuit_open_until` | 熔断恢复时间 |
| `latency_ewma_ms` | 延迟指数加权移动平均（ms） |
| `labels` | 扩展标签，供策略过滤预留 |

### Lease（租约）

一次租约绑定 **一个代理节点**，颁发一组时效性凭据供网关鉴权：

- 密码使用 SHA-256 哈希存储，仅 `CreateLease` 响应中一次性返回明文
- 支持 `Idempotency-Key` 防止重复颁发
- 支持 `RenewLease` 续期（默认延长 30 分钟）和 `RevokeLease` 主动吊销

## 调度与健康模型

### zfair 调度器

- 使用 Redis ZSET 维护 **ready** 和 **delayed** 两个队列
- 全程通过 Lua 脚本原子执行：候选注册、冷却晋升、节点选取、虚拟运行时更新
- 按权重和健康信号公平分配，并发下不会出现调度倾斜
- 生产环境下 Redis 不可用时**拒绝启动**而非静默降级

### 健康评分

| 事件 | 分值变化 |
| --- | --- |
| 请求成功 | +5 |
| 未知失败 | -5 |
| 连接失败 | -10 |
| 超时 | -15 |
| 认证失败 | -30 |
| 协议错误 | -30 |

连续失败 3 次触发熔断，基础冷却 30 秒，最大冷却 5 分钟（指数退避）。评分配置可通过 `PROXYHARBOR_SCORING_PROFILE` 调整为 `aggressive` 或 `lenient`。

## 配置参考

| 环境变量 | 说明 | 默认值 |
| --- | --- | --- |
| `PROXYHARBOR_TENANT_KEYS` | 推荐模式：`tenant:key,tenant:key`；key 同时承载控制面鉴权与租户身份 | **推荐必填** |
| `PROXYHARBOR_TENANT_KEY_MIN_LEN` | Tenant key 最小长度 | `16` |
| `PROXYHARBOR_AUTH_KEY` | Legacy 单 Key 模式；仅在未设置 `PROXYHARBOR_TENANT_KEYS` 时可用 | 兼容旧部署 |
| `PROXYHARBOR_ROLE` | 进程角色：`all` / `controller` / `gateway` | `all` |
| `PROXYHARBOR_STORAGE` | 存储驱动：`mysql` / `memory` | `mysql` |
| `PROXYHARBOR_MYSQL_DSN` | MySQL 连接串 | 空 |
| `PROXYHARBOR_REDIS_ADDR` | Redis 地址 | 空 |
| `PROXYHARBOR_SELECTOR` | 调度器名称 | `zfair` |
| `PROXYHARBOR_SELECTOR_REDIS_REQUIRED` | zfair 无 Redis 时拒绝启动 | `true` |
| `PROXYHARBOR_SCORING_PROFILE` | 健康评分档位：`default` / `aggressive` / `lenient` | `default` |
| `PROXYHARBOR_ZFAIR_QUANTUM` | 虚拟运行时基础增量 | `1000` |
| `PROXYHARBOR_ZFAIR_DEFAULT_LATENCY_MS` | 无 EWMA 数据时的默认延迟（ms） | `200` |
| `PROXYHARBOR_ZFAIR_MAX_PROMOTE` | 每次选取前最多从 delayed 队列晋升的节点数 | `128` |
| `PROXYHARBOR_ALLOW_INTERNAL_PROXY_ENDPOINT` | 允许注册回环/内网地址（仅测试） | `false` |

完整配置模板见 `.env.example`。

## 打包与部署

- `Dockerfile`：构建静态 Go 二进制
- `docker-compose.yaml`：本地开发一键启动
- `charts/proxyharbor`：Helm Chart，用于 Kubernetes 部署
- `migrations/mysql/`：数据库初始化 SQL

## 贡献指南

欢迎提交 Issue 和 Pull Request，详见 [CONTRIBUTING.md](CONTRIBUTING.md)。

## 联系方式

| 渠道 | 地址 |
| --- | --- |
| Telegram | [@kamill7779](https://t.me/kamill7779) |
| Email | [kamill7779@outlook.com](mailto:kamill7779@outlook.com) |

## 许可证

本项目基于 [MIT License](LICENSE) 开源。
