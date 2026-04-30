# 04 · 单体数据模型

> 目标：理解 ProxyHarbor 为什么区分 admin 库存和 tenant lease。

## 核心对象

| 对象 | 谁管理 | 谁使用 | 说明 |
|------|--------|--------|------|
| Tenant | admin | tenant key 归属方 | 权限边界，不直接代表一个进程 |
| Tenant Key | admin | SDK / 业务进程 | 租户侧访问凭证，明文只返回一次 |
| Provider | admin | selector 间接使用 | 代理来源，如 IDC、家庭宽带、第三方池 |
| Proxy | admin | selector 间接使用 | 真实代理 endpoint、权重、健康状态 |
| Lease | tenant | SDK / gateway | 短期可用权，绑定 subject 与 resource_ref |
| Audit Event | 服务端 | admin / 运维 | 记录关键管理动作 |

## 权限边界

租户永远不直接读取 provider/proxy 明细。租户只能创建 lease，并拿到一个可用于网关认证的代理 URL。

admin 可以维护 provider/proxy，也可以在必要时通过 `X-On-Behalf-Of` 代某个 tenant 调用租户 API。SDK 在本地默认模式下会用 `AdminKey + X-On-Behalf-Of: default` 帮你跑通最短路径。

## Lease 不是永久代理

lease 是短期授权，不是库存记录：

- 到期后需要 renew 或重新 acquire。
- revoke 后不能继续使用。
- 绑定 `resource_ref`，目标不匹配时网关校验应拒绝。
- SDK 会缓存 keyed lease，但缓存只是客户端优化，服务端状态才是权威。

