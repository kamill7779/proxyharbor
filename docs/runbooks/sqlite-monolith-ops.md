# ProxyHarbor Monolith Operations Runbook

This runbook covers the v0.4.3 single-node SQLite operations path. SQLite storage may be integrated by another v0.4.3 worker; until then, the operations primitives are safe to build and call independently.

## Scope

- Single ProxyHarbor process or an explicitly stopped process.
- SQLite for small monolith deployments; MySQL + Redis remains the HA path.
- File-level SQLite backup/restore only when the process is offline. Online backup is reserved for the SQLite Store integration.

## Init

1. Choose a persistent DB path on local disk or a mounted volume/PVC.
2. Set the future SQLite path with either `--sqlite-path` on ops commands or `PROXYHARBOR_SQLITE_PATH`.
3. Start the service only after the SQLite Store has created and migrated the DB.
4. Ensure the DB file and parent directory are readable/writable only by the service account.

Expected schema version table:

```sql
CREATE TABLE schema_version (
  version INTEGER NOT NULL,
  applied_at TEXT NOT NULL
);
```

## Upgrade

1. Stop ProxyHarbor before a file-level backup.
2. Run an offline backup:

```bash
proxyharbor backup --sqlite-path /var/lib/proxyharbor/proxyharbor.db --output /var/backups/proxyharbor/pre-upgrade.db --offline
```

3. Start the new binary. The migration runner applies ordered SQL migrations in a transaction.
4. If the DB `schema_version` is newer than the binary supports, do not start the old binary; deploy the matching newer binary or restore the pre-upgrade backup.

## Backup

Offline file-level backup is intentionally explicit:

```bash
proxyharbor backup --sqlite-path /var/lib/proxyharbor/proxyharbor.db --output /var/backups/proxyharbor/proxyharbor-$(date +%F).db --offline
```

Rules:

- Stop ProxyHarbor before running this command.
- Do not copy a live SQLite file with generic `cp` while writes may be active.
- The command refuses to overwrite an existing backup path.
- When SQLite Store exposes an online backup API, wire `proxyharbor backup` to that API and remove the offline-only requirement for online mode.

## Restore

Restore replaces the target DB, so it requires an explicit confirmation flag:

```bash
systemctl stop proxyharbor
proxyharbor restore --input /var/backups/proxyharbor/pre-upgrade.db --sqlite-path /var/lib/proxyharbor/proxyharbor.db --force
systemctl start proxyharbor
```

Rules:

- Stop ProxyHarbor first.
- Restore to a different temp path first if you need manual inspection.
- After restore, run the service health/doctor path once available for SQLite.

## Retention

Retention defaults to disabled to avoid surprising data loss.

Configuration:

- `PROXYHARBOR_AUDIT_RETENTION_DAYS=30`
- `PROXYHARBOR_USAGE_RETENTION_DAYS=7`
- `PROXYHARBOR_RETENTION_INTERVAL=1h`

The reusable SQL builder only deletes from:

- `proxy_audit_events` by `occurred_at`
- `proxy_usage_events` by `occurred_at`

Preview the SQL skeleton without connecting to a DB:

```bash
proxyharbor retention --audit-days 30 --usage-days 7
```

## Container Volumes and PVCs

- Mount the DB directory, not just the file, so SQLite sidecar files can be created safely.
- Keep backups outside the writable DB directory when possible.
- Use filesystem-level snapshots only when the process is stopped or when the storage layer provides a consistent snapshot primitive.

## SQLite to MySQL Manual Path

1. Stop writes to the SQLite monolith.
2. Take an offline SQLite backup and keep it immutable.
3. Export domain tables to CSV or SQL using a trusted SQLite client.
4. Load into MySQL tables created from `migrations/mysql/init.sql`.
5. Verify tenant, proxy, policy, lease, audit, and usage row counts.
6. Start ProxyHarbor with `PROXYHARBOR_STORAGE=mysql`, `PROXYHARBOR_MYSQL_DSN`, Redis, admin key, and key pepper.
7. Keep the SQLite backup until the MySQL deployment has passed an operational soak period.
