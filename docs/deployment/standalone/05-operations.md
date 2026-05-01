# 05 · 单体运维操作

单体模式的运维目标是简单、可复制、可恢复。不要为了一个 SQLite 文件引入重型运维系统。

## doctor

`doctor` 用来检查配置、依赖和常见误用。它不会输出 secret 原文。

```bash
proxyharbor doctor -storage=sqlite -sqlite-path=/var/lib/proxyharbor/proxyharbor.db
```

## init

`init` 用来初始化 SQLite schema。Docker 默认启动路径会自动完成，裸 binary 场景可以显式运行。

```bash
proxyharbor init -storage=sqlite -sqlite-path=./data/proxyharbor.db
```

## backup / restore

备份和恢复面向 SQLite 单体。建议在低流量窗口执行，并把 `secrets.env` 和数据库一起纳入备份策略。

```bash
proxyharbor backup  -sqlite-path=./data/proxyharbor.db -output=./backup/proxyharbor.db
proxyharbor restore -sqlite-path=./data/proxyharbor.db -input=./backup/proxyharbor.db --force
```

## retention

retention 用来清理审计和使用记录。默认不强制清理，生产可以按磁盘容量设置保留天数。

```bash
proxyharbor retention -sqlite-path=./data/proxyharbor.db -audit-days=30 -usage-days=30 --execute
```

## 升级建议

- 先备份 SQLite DB 和 `secrets.env`。
- 再替换 binary / image。
- 最后访问 `/readyz` 和 `/metrics` 确认服务恢复。
