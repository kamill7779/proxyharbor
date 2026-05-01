# 03 · 管理侧用法

> 目标：用 `PROXYHARBOR_ADMIN_KEY` 维护全局 provider / proxy / 租户库存。

管理侧 API 以「平台资源所有者」视角操作。**租户接口永远不暴露 provider/proxy 明细**；admin 是唯一能看见 endpoint、health、weight 的角色。

## 鉴权

```bash
export PROXYHARBOR_BASE_URL=http://proxyharbor:8080
export PROXYHARBOR_ADMIN_KEY=$(openssl rand -hex 32)
```

```go
client, err := proxyharbor.New()  // 自动读取上述两个 env
```

或显式：

```go
client, err := proxyharbor.New(
    proxyharbor.WithBaseURL("http://proxyharbor:8080"),
    proxyharbor.WithAdminKey(os.Getenv("PROXYHARBOR_ADMIN_KEY")),
)
```

> 单体 + `AutoSecrets=true` 模式下，admin key 由服务端自动生成并持久化在 `data/secrets.env`。SDK 用 `WithLocalDefaults()` 即可读到。

## 思路：分两层

- **「最简 admin」shortcut**：`AddProxy`、`AddProvider`、`AddProxyWithProvider`，签名最短，适合 95% 的入池场景。
- **底层 `*API` 命名空间**：`Providers.*`、`Proxies.*`、`Leases.*`，可以传完整 DTO，适合需要 `weight`、`labels`、`policy_id` 的高级场景。

## 入池：最短闭环

### 添加单个代理

```go
admin, _ := proxyharbor.New()

// 只需 endpoint
admin.AddProxy(ctx, "http://1.2.3.4:3128")

// 显式归属到某个 provider
admin.AddProxyWithProvider(ctx, "http://5.6.7.8:8080", "datacenter-cn")
```

`AddProxy` 在内部走 `Proxies.Upsert`，归属默认 `provider_id="default"`（来自 `Config.DefaultProviderID`，可用 `WithDefaultProviderID` 覆盖）。

### 添加 provider

```go
admin.AddProvider(ctx, "datacenter-cn")
```

或用底层 API 设全字段：

```go
admin.Providers.Create(ctx, proxyharbor.ProviderDTO{
    ID:      "datacenter-cn",
    Name:    "中国 IDC 出口",
    Type:    "static",
    Enabled: true,
    Labels:  map[string]string{"region": "cn-east-1"},
})
```

## 库存维护

### 列举与查询

> 当前 SDK shortcut 主要覆盖 Get/Upsert/Delete。完整列表 API（`GET /v1/providers`、`GET /v1/proxies`）属于 admin 后台用途，可以直接通过 `client.do` 或者 HTTP 调用。

```go
provider, err := admin.Providers.Get(ctx, "datacenter-cn")
proxy,    err := admin.Proxies.Get(ctx, "proxy-id")
```

### 修改

```go
admin.Providers.Update(ctx, "datacenter-cn", proxyharbor.ProviderDTO{
    ID:      "datacenter-cn",
    Name:    "中国 IDC 出口（已扩容）",
    Enabled: true,
})

admin.Proxies.Upsert(ctx, proxyharbor.ProxyDTO{
    ID:         "proxy-id",
    ProviderID: "datacenter-cn",
    Endpoint:   "http://1.2.3.4:3128",
    Healthy:    true,
    Weight:     20,
    Labels:     map[string]string{"tier": "premium"},
})
```

`Proxies.Upsert` 的语义：

- 带 `ID` 时先 `PUT /v1/proxies/{id}`，404 时回退到 `POST /v1/proxies`。
- 不带 `ID` 时直接 `POST /v1/proxies`。

### 删除

```go
admin.Proxies.Delete(ctx, "proxy-id")
admin.Providers.Delete(ctx, "datacenter-cn")
```

## 健康与权重模型

> Service 端有完整的健康反馈与熔断机制（参见 [设计文档 / 健康评分](../../design/standalone-internals.md#健康评分)）。Admin 视角只需要管理「初始 weight + 是否启用」，运行时数据由服务端维护。

| 字段 | 类型 | 说明 |
|------|------|------|
| `Endpoint` | string | 代理 endpoint，如 `http://host:port`、`socks5://host:port` |
| `Weight` | int | 平滑加权轮询权重，默认 `1` |
| `Healthy` | bool | 入池时一般置 `true`；运行时由服务端评分覆盖 |
| `Labels` | map | 任意 K/V，用于策略匹配 |
| `ProviderID` | string | 隶属 provider；未填则归默认 |

## 关于 admin 调租户接口

ProxyHarbor 同时**允许 admin key 直接调用租户 API**：

- 普通租户调用 `POST /v1/leases` 时不需要 `X-On-Behalf-Of`。
- Admin key 调租户 API 时**必须**带 `X-On-Behalf-Of: <tenant_id>` 表明在替哪个租户操作。
- SDK 的 `applyAuth` 在「`authTenant` + 没配 `TenantKey` + 配了 `AdminKey`」时**自动**注入 `X-On-Behalf-Of: default`，方便单体场景一把 admin key 走到底。需要替别的租户操作时显式管理 HTTP header（高级用法，目前 SDK 不直接暴露）。

## 与租户 lifecycle 的关系

Admin 不应该频繁调用 `client.GetProxy`：

- `GetProxy` 走 lease 缓存，是为长跑租户设计。
- 真正想验证某个 endpoint 是否 OK，可以走 admin 视角的 `Catalog`、`Validate`、`/v1/gateway/validate` 等服务端能力。

## 常见反模式

- ❌ **把 admin key 下发给业务进程**：业务侧只需要租户 key。Admin key 应该只放在控制面或者 ops 工具里。
- ❌ **入池后立刻 GetProxy 验证可用性**：lease 走的是租户视角，不是「联通性测试」。请用服务端 `/v1/gateway/validate` 或专门的健康脚本。
- ❌ **同一 endpoint 反复 `Upsert` 不带 `ID`**：会创建多条记录。要么生成稳定 ID，要么先 `Get` 拿到再 `PUT`。

下一步：

- 把 lease 寿命管理吃透 → [04 lease lifecycle](./04-lease-lifecycle.md)
- 把外部代理源接进来 → [05 mining/pool](./05-mining-pool.md)
