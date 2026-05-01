# 02 · 单体配置参考

> 目标：理解哪些配置必须关心，哪些配置保持默认即可。

## 最小配置

单体 Docker 默认不需要手写配置。真正的最小模型是：

| 配置 | 默认值 | 说明 |
|------|--------|------|
| `PROXYHARBOR_ROLE` | `all` | 控制面和网关在同一个进程内 |
| `PROXYHARBOR_STORAGE` | `sqlite` | 单体持久化后端 |
| `PROXYHARBOR_SELECTOR` | `local` | 进程内平滑加权轮询 |
| `PROXYHARBOR_AUTO_SECRETS` | `true` | 缺少 admin key / pepper 时自动生成 |
| `PROXYHARBOR_SECRETS_FILE` | 与 SQLite 同目录 | 保存自动生成的本地 secrets |

## 建议显式设置的配置

| 配置 | 什么时候设置 |
|------|--------------|
| `PROXYHARBOR_HOST_PORT` | 本机 `18080` 被占用时改端口 |
| `PROXYHARBOR_SQLITE_PATH` | 裸 binary 启动时指定数据库位置 |
| `PROXYHARBOR_GATEWAY_URL` | 服务不通过 localhost 访问时指定外部 URL |
| `PROXYHARBOR_ALLOW_INTERNAL_PROXY_ENDPOINT` | 本地测试 loopback / 内网代理时开启 |

## 自动 secrets

单体模式下，`PROXYHARBOR_ADMIN_KEY` 和 `PROXYHARBOR_KEY_PEPPER` 可以不手写。服务会在首次启动时生成并写入 `secrets.env`。

优先级是：

```text
显式环境变量 / flag > secrets 文件 > 自动生成
```

如果你传了 `-secrets-file` 或 `PROXYHARBOR_SECRETS_FILE`，服务会尊重这个路径，不会再改写到默认路径。

## 不建议的配置

- 单体模式不要配置 Redis，只会让部署变复杂。
- 不要把 `memory` 当生产存储，它只适合 demo / CI。
- 不要多实例共享同一个 SQLite 文件；需要多实例请切到 distributed 路径。

