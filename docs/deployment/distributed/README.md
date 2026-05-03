# ProxyHarbor 分布式 / HA 部署与验证

本文档只描述当前已经有正式 runner 支持、并适合作为 v0.5.4 发布收口依据的 HA 路径。它不是云厂商级 benchmark 指南，也不覆盖未验证的拓扑玩法。

## 稳定边界

- HA 使用 `storage=mysql`；不要把 SQLite 当成多实例共享存储。
- Redis 负责 selector / cache 协调；建议 `selector=zfair` 且 `selectorRedisRequired=true`。
- HA 必须显式提供 `admin key` 和 `key pepper`，不自动生成 secrets。
- 本机重复验证使用 `docker-compose.ha-test.yaml`；Kubernetes 使用 Helm HA values 示例。
- CI 只保留轻量 guard，不把 10 分钟 soak test 放进 CI。

## 目标拓扑

```text
LB / Ingress
├── proxyharbor-0
├── proxyharbor-1
└── proxyharbor-2
     │
     ├── MySQL  权威数据：tenant / key / provider / proxy / lease / cluster state
     └── Redis  zfair selector / cache coordination
```

## 本机 Docker HA 验证

先构建本地 HA 镜像：

```bash
docker build --pull=false -t proxyharbor:ha-test .
```

然后按下面顺序运行正式 runner：

```bash
go run ./tools/haruntimecheck -docker -docker-skip-build -timeout 8m
go run ./tools/hacorrect -docker -timeout 6m
go run ./tools/hacachecheck -docker -docker-skip-build -timeout 6m
go -C tools/hasdkcheck run . -docker -samples 500 -disable-samples 100 -concurrency 16 -timeout 8m
```

这些命令会复用 `docker-compose.ha-test.yaml` 的 3 实例 + MySQL + Redis + LB 拓扑，分别覆盖：

| runner | 作用 |
| --- | --- |
| `haruntimecheck` | HA 拓扑启动、就绪、基础运行态探测 |
| `hacorrect` | 多实例写入语义、selector 分布与禁用节点 correctness |
| `hacachecheck` | 多实例 auth/cache 失效传播 correctness |
| `hasdkcheck` | Go SDK 的 HA 基线路径：Admin 侧代理写入、租户 key 签发、SDK `GetProxy` 分布与禁用节点路径 |

## Pressure / soak 记录口径

v0.5.4 的性能记录必须使用正式 runner。`tools/hapressure` 是预留命令位；当它合入当前分支后，把真实命令和结果填到发布 PR 中。不要提交一次性的压测脚本，也不要在 PR 描述里写“手工压了一下”。

如果当前分支还没有 `tools/hapressure`，PR 里至少要明确：

- correctness / runtime / cache / SDK runner 已通过；
- pressure runner 计划使用的正式命令；
- 未引用任何 ad-hoc 脚本或未版本化命令。

## Helm HA 起步配置

Helm 只保留当前稳定的 HA 起步路径：

```bash
kubectl create secret generic proxyharbor-credentials \
  --from-literal=admin-key="$(openssl rand -hex 32)" \
  --from-literal=pepper="$(openssl rand -hex 32)" \
  --from-literal=mysql-dsn='ph:REPLACE_ME@tcp(mysql.svc:3306)/proxyharbor?parseTime=true&loc=UTC'

helm install proxyharbor charts/proxyharbor \
  -f charts/proxyharbor/examples/dynamic-ha-values.yaml
```

这个示例要求：

- `replicaCount=3`
- `storage=mysql`
- `redis.addr` 已配置
- `redis.selectorRedisRequired=true`
- `cluster.enabled=true`

示例值文件见 [charts/proxyharbor/examples/dynamic-ha-values.yaml](../../../charts/proxyharbor/examples/dynamic-ha-values.yaml)，图表说明见 [charts/proxyharbor/README.md](../../../charts/proxyharbor/README.md)。

## PR 描述模板

把下面模板直接填进 v0.5.4 HA PR 描述：

```md
## HA pressure / release evidence

- Machine / environment:
- Image / commit:
- Topology: 3 x proxyharbor + MySQL + Redis + LB (`docker-compose.ha-test.yaml`, local Docker)
- Commands:
  - `docker build --pull=false -t proxyharbor:ha-test .`
  - `go run ./tools/haruntimecheck -docker -docker-skip-build -timeout 8m`
  - `go run ./tools/hacorrect -docker -timeout 6m`
  - `go run ./tools/hacachecheck -docker -docker-skip-build -timeout 6m`
  - `go -C tools/hasdkcheck run . -docker -samples 500 -disable-samples 100 -concurrency 16 -timeout 8m`
  - `go run ./tools/hapressure ...`  <!-- 仅在正式 runner 合入后填写真实命令 -->
- gateway validate: p95= / p99=
- lease create: p95= / p99=
- lease renew: p95= / p99=
- soak error rate:
- Threshold met: yes / no
- Notes / gaps:
```

## CI guard 边界

当前 CI 只需要：

- `helm lint charts/proxyharbor`
- HA example smoke render: `helm template ph charts/proxyharbor -f charts/proxyharbor/examples/dynamic-ha-values.yaml`
- `haruntimecheck`
- `hacachecheck`
- `hacorrect`
- `hasdkcheck`

不要把长时间 soak test 直接塞进 CI。详细路线仍以 [设计 / 分布式路线](../../design/distributed-roadmap.md) 和 [v0.5.4 计划](../../versions/v0.5.4.md) 为准。

