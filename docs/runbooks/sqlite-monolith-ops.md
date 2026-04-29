# ProxyHarbor Monolith Operations Runbook

This runbook covers the v0.4.5 single-node SQLite operations path.

## Scope

- Single ProxyHarbor process or an explicitly stopped process.
- SQLite for small monolith deployments; MySQL + Redis remains the HA path.
- Backup/restore for one SQLite database file on one host.
- MySQL + Redis remains the HA path; this runbook does not define automatic SQLite to MySQL migration.

## Init

1. Choose a persistent DB path on local disk or a mounted volume/PVC.
2. Set the SQLite path with either `--sqlite-path` on ops commands or `PROXYHARBOR_SQLITE_PATH`.
3. Run `proxyharbor init --storage sqlite --sqlite-path /var/lib/proxyharbor/proxyharbor.db` before first start.
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

Backup refuses to overwrite an existing output path and writes the DB file with mode `0600` plus a sidecar metadata file:

```bash
proxyharbor backup --sqlite-path /var/lib/proxyharbor/proxyharbor.db --output /var/backups/proxyharbor/proxyharbor-$(date +%F).db
```

Rules:

- If `-wal` or `-shm` sidecar files exist, stop ProxyHarbor or checkpoint WAL before running this command.
- Do not copy a live SQLite file with generic `cp` while writes may be active.
- The command refuses to overwrite an existing backup path.
- The metadata sidecar is `<backup>.metadata.json` and includes source path, schema version, creation time, backup path, and SHA-256 checksum.
- `--offline` remains accepted as an explicit operator assertion; the same sidecar safety checks still apply.

## Restore

Restore replaces the target DB, so it requires an explicit confirmation flag:

```bash
systemctl stop proxyharbor
proxyharbor restore --input /var/backups/proxyharbor/pre-upgrade.db --sqlite-path /var/lib/proxyharbor/proxyharbor.db --force
proxyharbor restore --input /var/backups/proxyharbor/pre-upgrade.db --sqlite-path /var/lib/proxyharbor/verify.db --force --doctor
systemctl start proxyharbor
```

Rules:

- Stop ProxyHarbor first.
- `--force` is mandatory.
- The command refuses to restore over a target that has `-wal` or `-shm` sidecars.
- Restore to a different temp path first if you need manual inspection.
- Use `--doctor` after restore to validate that the restored DB opens and has the expected schema version.

## Retention

Retention defaults to dry-run to avoid surprising data loss. It only targets `proxy_audit_events` and `proxy_usage_events`; it never deletes tenants, keys, proxies, policies, leases, or catalog rows.

Configuration:

- `PROXYHARBOR_AUDIT_RETENTION_DAYS=30`
- `PROXYHARBOR_USAGE_RETENTION_DAYS=7`
- `PROXYHARBOR_RETENTION_INTERVAL=1h`

The reusable SQL builder only deletes from:

- `proxy_audit_events` by `occurred_at`
- `proxy_usage_events` by `occurred_at`

Preview row counts without deleting:

```bash
proxyharbor retention --sqlite-path /var/lib/proxyharbor/proxyharbor.db --audit-days 30 --usage-days 7
```

Execute in one transaction and print deleted rows:

```bash
proxyharbor retention --sqlite-path /var/lib/proxyharbor/proxyharbor.db --audit-days 30 --usage-days 7 --execute
```

If `--sqlite-path` is omitted, the command prints the SQL skeleton for compatibility and does not connect to a DB.

## Doctor

Run doctor before upgrades, after restore, and when investigating local storage issues:

```bash
proxyharbor doctor --storage sqlite --sqlite-path /var/lib/proxyharbor/proxyharbor.db
```

Doctor checks:

- SQLite path is configured.
- Parent directory exists and accepts writes.
- DB file exists, is regular, and reports file size.
- Schema version matches the binary expectation.
- Suspicious `-wal` / `-shm` sidecars are absent for maintenance operations.
- Basic write probe succeeds as a local disk-space/permission signal.

## Container Volumes and PVCs

- Mount the DB directory, not just the file, so SQLite sidecar files can be created safely.
- Keep backups outside the writable DB directory when possible.
- Use filesystem-level snapshots only when the process is stopped or when the storage layer provides a consistent snapshot primitive.

## SQLite to MySQL Manual Path

ProxyHarbor does not provide automatic SQLite to MySQL migration in v0.4.5. Use a manual, operator-reviewed path:

1. Stop writes to the SQLite monolith.
2. Take an offline SQLite backup and keep it immutable.
3. Export domain tables to CSV or SQL using a trusted SQLite client.
4. Load into MySQL tables created from `migrations/mysql/init.sql`.
5. Verify tenant, proxy, policy, lease, audit, and usage row counts.
6. Start ProxyHarbor with `PROXYHARBOR_STORAGE=mysql`, `PROXYHARBOR_MYSQL_DSN`, Redis, admin key, and key pepper.
7. Keep the SQLite backup until the MySQL deployment has passed an operational soak period.
