# ProxyHarbor Go SDK 文档

`github.com/kamill7779/proxyharbor/sdks/go/proxyharbor` 是 ProxyHarbor 的官方 Go SDK。SDK 只做一件事：**让上层代码用最少的字符拿到一个可用的代理 URL**。

文档按照「权限边界 → 生命周期 → 高级用法」分层组织。建议第一次接触 SDK 的同学按顺序阅读 01 → 03，需要专题信息再跳到对应章节。

## 阅读路线

| 章节 | 角色 | 你能获得的能力 |
|------|------|----------------|
| [01 快速开始](./01-getting-started.md) | 任何角色 | 三步跑通最短闭环：装 SDK → 配 secrets → 拿代理 |
| [02 租户侧用法](./02-tenant-guide.md) | **租户**（`PROXYHARBOR_TENANT_KEY`） | 拿代理、粘性 key、释放租约、跨进程并发的姿势 |
| [03 管理侧用法](./03-admin-guide.md) | **管理**（`PROXYHARBOR_ADMIN_KEY`） | 入池、provider 编排、库存维护、审计动作 |
| [04 租约生命周期](./04-lease-lifecycle.md) | 租户/进阶 | reuse / renew / reacquire 的判定路径与策略调优 |
| [05 挖矿/采集池](./05-mining-pool.md) | 管理 | 把外部爬虫/挖矿源接入 ProxyHarbor 的 `Pool` |
| [06 配置参考](./06-configuration.md) | 任何角色 | 全部 `Option`、`Config`、环境变量、默认值 |
| [07 错误处理与重试](./07-error-handling.md) | 任何角色 | `APIError`、判定函数、SDK 内部重试规则 |
| [08 食谱（Recipes）](./08-recipes.md) | 任何角色 | `net/http`、`resty`、并发抓取、热切换等典型样板 |

## 思路总览

ProxyHarbor 把代理体系拆成两套清晰的权限边界：

- **租户（tenant）权限**：通过租户 key 调用 lease 相关 API，得到一个**短期、绑定 subject 与 resource_ref** 的代理租约（lease）。租户**永远拿不到** provider/proxy 的明细，只拿到一个 `gateway_url + username/password` 组合。
- **管理（admin）权限**：通过 admin key 调用 provider/proxy/policy/tenant 等库存与编排 API，是**全局资源所有者**视角。

SDK 把这两类调用映射成两组同名 API：

```
Client.GetProxy / GetProxyURL / Release        ← 租户视角（authTenant）
Client.AddProxy / AddProvider / Proxies / Providers / Leases.Create  ← 管理视角（authAdmin）
```

底层每个 HTTP 调用都会走 `Client.do` → `applyAuth(mode)`，根据 `authTenant` / `authAdmin` 决定挂哪个 header。**租户调用在缺少 `TenantKey` 时会自动 fallback 到 `AdminKey + X-On-Behalf-Of: default`**，方便单体模式下零配置直接跑通。

## 三个最小起手式

零配置、租户最简、管理员最简：

```go
// 1) 全 default：依赖 PROXYHARBOR_BASE_URL + PROXYHARBOR_TENANT_KEY
url, _ := proxyharbor.GetProxyURL(ctx)

// 2) 租户长生命周期 Client（推荐）
client, _ := proxyharbor.New()
defer client.Close(ctx)
proxy, _ := client.GetProxy(ctx, proxyharbor.WithKey("account-a"))

// 3) 管理员入池
admin, _ := proxyharbor.New(proxyharbor.WithAdminKey(os.Getenv("PROXYHARBOR_ADMIN_KEY")))
admin.AddProxy(ctx, "http://1.2.3.4:3128")
```

## 与 SDK 自带 README 的关系

`sdks/go/proxyharbor/README.md` 是 SDK 包内自带的 godoc 风格摘要，篇幅短、用于 `go doc` 与 pkg.go.dev 展示。本目录是面向**最佳实践与运维场景**的扩展文档，覆盖：

- 完整的 lease 生命周期模型
- 单体 / HA 双模式下的配置取值策略
- 错误判定与重试组合的「正确做法」
- 落地到 `net/http`、并发抓取、长跑服务的样板代码

需要快速 API 索引去 SDK README，需要写实际业务代码以本目录为准。
