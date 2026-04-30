# 06 · 单体最佳实践

## 推荐做法

- 用 Docker Compose 默认路径启动，不要一开始就手写全部环境变量。
- 把 `secrets.env` 作为本地机密文件管理，不提交到版本库。
- 业务进程优先用 Go SDK 的 `WithLocalDefaults()`。
- 给不同业务 subject 使用不同 sticky key，例如账号、任务、worker ID。
- 定期备份 SQLite DB 和 `secrets.env`。
- 需要内网代理测试时，显式开启 `PROXYHARBOR_ALLOW_INTERNAL_PROXY_ENDPOINT=true`，测试结束关闭。

## 不推荐做法

- 不要把 admin key 下发给不可信业务进程。
- 不要多实例共享 SQLite。
- 不要为了单体模式额外引入 Redis。
- 不要把 long-running 业务绑定到一个永不过期 lease；应该依赖 SDK renew / reacquire。
- 不要绕过 lease 直接把 proxy endpoint 发给租户。

## 什么时候该切 distributed

出现以下任一情况，就应该计划切换到 distributed：

- 需要 2 个以上 ProxyHarbor 副本。
- 需要滚动升级期间不中断服务。
- 需要跨实例全局 selector 公平性。
- 单机 SQLite 写入、备份或磁盘成为瓶颈。
- 运维需要 `/admin/cluster`、leader election、Redis zfair 等能力。

