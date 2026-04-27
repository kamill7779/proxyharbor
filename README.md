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

ProxyHarbor v0.2.0 是一次 breaking reset：以 **MySQL + Redis** 为轻量依赖，提供全局代理库存、动态租户 Key、租约颁发和 HTTP/HTTPS 网关转发能力。Provider、Proxy、Policy 属于平台全局资源；租户只能通过动态 Key 创建和使用租约，不可读取代理 endpoint 列表。

## 功能特性

- **全局代理库存**：Admin 管理 Provider / Proxy；租户共享全局健康代理池。
- **动态租户 Key**：Admin API 签发、撤销、轮换租户 Key；明文 Key 只在签发时返回一次。
- **默认策略**：MVP 只允许 `default` policy；请求不传 `policy_id` 时自动使用 `default`。
- **默认 Provider**：Proxy 不传 `provider_id` 时自动归入 `default` provider。
- **租约网关**：租约绑定 `resource_ref`，网关请求目标必须匹配租约资源 host。
- **zfair 调度**：Redis ZSET + Lua 原子选择，结合权重、延迟和健康状态做公平调度。
- **Secret-first 部署**：Helm 不渲染明文密钥，生产环境必须引用已有 Kubernetes Secret。

## 快速开始

> 本地推荐使用 Docker Compose；MySQL 会从 `migrations/mysql/init.sql` 初始化。

### 1. 启动全部服务

```bash
export PROXYHARBOR_ADMIN_KEY=$(openssl rand -hex 32)
export PROXYHARBOR_KEY_PEPPER=$(openssl rand -hex 32)
docker compose up -d --build
```

启动内容：
- `mysql`：租户 Key、全局代理库存、租约、健康状态持久化
- `redis`：zfair 调度状态与热路径缓存
- `proxyharbor`：控制面 + 网关一体进程

### 2. 检查服务就绪状态

```bash
curl http://localhost:8080/readyz
```

### 3. 创建租户并签发租户 Key

```bash
curl -H "ProxyHarbor-Key: $PROXYHARBOR_ADMIN_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"id":"tenant-a","display_name":"Tenant A"}' \
  http://localhost:8080/admin/tenants

curl -H "ProxyHarbor-Key: $PROXYHARBOR_ADMIN_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"label":"app-1","purpose":"platform_container"}' \
  http://localhost:8080/admin/tenants/tenant-a/keys
```

### 4. 添加代理节点

```bash
curl -H "ProxyHarbor-Key: $PROXYHARBOR_ADMIN_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"id":"proxy-1","endpoint":"http://proxy1.example.com:8080","healthy":true,"weight":10}' \
  http://localhost:8080/v1/proxies
```

> 本地测试回环地址或内网 IP 时，需先设置 `PROXYHARBOR_ALLOW_INTERNAL_PROXY_ENDPOINT=true`。

### 5. 创建租约

```bash
curl -H "ProxyHarbor-Key: $TENANT_KEY" \
  -H 'Idempotency-Key: demo-lease-1' \
  -H 'Content-Type: application/json' \
  -d '{"subject":{"subject_type":"workload","subject_id":"app-1"},"resource_ref":{"kind":"url","id":"https://example.com"}}' \
  http://localhost:8080/v1/leases
```

## API Shape

Admin-only:

- `POST|PATCH|DELETE /admin/tenants...`
- `POST|DELETE /admin/tenants/{id}/keys...`
- `GET|POST|PUT|DELETE /v1/providers...`
- `GET|POST|PUT|DELETE /v1/proxies...`
- `GET|POST|PUT|DELETE /v1/policies/default`
- `GET /v1/catalog/latest`
- `POST /v1/internal/usage-events:batch`
- `POST /v1/internal/gateway-feedback:batch`
- `GET /v1/gateway/validate`

Tenant:

- `POST /v1/leases`
- `POST /v1/leases/{id}:renew`
- `DELETE /v1/leases/{id}`

## 启动模式

### 内置依赖（推荐本地开发）

```bash
docker compose up -d --build
```

### 外部 MySQL + Redis

```bash
cp .env.example .env
# 编辑 .env 填写外部连接信息
docker compose up -d --build proxyharbor
```

### 直接运行二进制

```bash
go run ./cmd/proxyharbor \
  -admin-key "$PROXYHARBOR_ADMIN_KEY" \
  -key-pepper "$PROXYHARBOR_KEY_PEPPER" \
  -mysql-dsn "$PROXYHARBOR_MYSQL_DSN"
```

## Helm Secret

Chart 采用 Secret-first，不生成明文密钥。安装前先创建引用的 Secret：

```bash
kubectl create secret generic proxyharbor-credentials \
  --from-literal=admin-key="$(openssl rand -hex 32)" \
  --from-literal=pepper="$(openssl rand -hex 32)" \
  --from-literal=mysql-dsn='ph:REPLACE_ME@tcp(mysql.svc:3306)/proxyharbor?parseTime=true&loc=UTC'
```

## 数据模型

### Provider（提供商）

全局平台资源。MVP 默认 seed `default` provider，代理节点不传 `provider_id` 时自动使用它。

### Proxy（代理节点）

全局平台资源。包含 endpoint、weight、health_score、熔断状态和健康反馈字段；租户 API 不暴露代理 endpoint 列表。

### Policy（策略）

MVP 只允许 `default` policy。非 `default` 创建、更新、删除会被拒绝。

### Lease（租约）

租户资源。保留 `tenant_id`，绑定 subject、resource_ref、proxy_id、过期时间和不可逆密码哈希；明文密码只在创建响应返回一次。

## 调度与健康模型

### zfair 调度器

`selector=zfair` 使用 Redis ZSET 和 Lua 维护 ready/delayed 队列。多实例部署时建议开启 `PROXYHARBOR_SELECTOR_REDIS_REQUIRED=true`，避免调度状态分裂。

### 健康评分

成功提升分数；连接失败、超时、认证/协议失败降低分数；连续失败触发熔断并进入指数退避冷却。

## 配置参考

| 变量 | 说明 |
| --- | --- |
| `PROXYHARBOR_ADMIN_KEY` | Bootstrap admin key，至少 32 字节 |
| `PROXYHARBOR_KEY_PEPPER` | 租户 Key 哈希 pepper，至少 32 字节 |
| `PROXYHARBOR_MYSQL_DSN` | MySQL DSN，必填 |
| `PROXYHARBOR_REDIS_ADDR` | Redis 地址；zfair 强制 Redis 时必填 |
| `PROXYHARBOR_AUTH_REFRESH_INTERVAL` | 动态 Key 刷新间隔，最大 5s |
| `PROXYHARBOR_SELECTOR_REDIS_REQUIRED` | 是否强制 selector 依赖 Redis |
| `PROXYHARBOR_ALLOW_INTERNAL_PROXY_ENDPOINT` | 是否允许注册内网/回环代理 endpoint |

## 打包与部署

```bash
docker build -t proxyharbor:0.2.0-alpha .
helm install proxyharbor charts/proxyharbor -f charts/proxyharbor/examples/dynamic-ha-values.yaml
```

## 贡献指南

欢迎提交 Issue 与 PR。v0.2.0 是 breaking reset，不提供旧 migration 链路。

## 联系方式

- GitHub Issues: <https://github.com/kamill7779/proxyharbor/issues>
- Author: Kamill

## 许可证

本项目基于 [MIT License](LICENSE) 开源。
