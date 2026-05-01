# 03 · 单体架构说明

单体模式把 ProxyHarbor 的主要能力放在一个 binary 里运行。它不是“简化版功能”，而是“少依赖的部署形态”。

## 组件视图

```text
HTTP Server
├── Admin API        管理 tenant / provider / proxy / key
├── Lease API        租户创建、续租、撤销 lease
├── Gateway Validate 网关校验 lease 与目标资源
├── Auth Cache       动态 key 缓存，定期从 store 刷新
├── Control Service  业务编排与 domain error
├── SQLite Store     单体持久化权威来源
└── Local Selector   进程内平滑加权轮询
```

## 请求路径

租户拿代理时，路径是：

```text
SDK → POST /v1/leases → auth → service → selector → store → lease response
```

业务真正使用代理时，SDK 拿到的是 `gateway_url + username/password`。网关侧通过 `/v1/gateway/validate` 校验 lease 是否仍然有效、目标是否匹配。

## Local selector

单体默认使用 smooth weighted round-robin。候选 proxy 必须满足：

- `healthy=true`
- `weight > 0`
- `health_score > 0`
- `circuit_open_until` 没有处于未来

selector 状态在进程内维护，因此它只适合一个 ProxyHarbor 实例。如果你启动多个实例，每个实例都会有自己的本地轮询状态，这不是全局调度。

## 单体和 HA 的分界线

| 问题 | 单体答案 | HA 答案 |
|------|----------|---------|
| 数据库 | SQLite | MySQL |
| selector | local | Redis zfair |
| secrets | 可自动生成 | 必须显式提供 |
| 副本数 | 1 | 2+ |
| 维护任务 | 本进程执行 | leader 执行 |

