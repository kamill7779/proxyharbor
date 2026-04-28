package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

var ErrSchemaVersionTooNew = errors.New("schema version is newer than this binary supports")

type Migration struct {
	Version int
	Name    string
	SQL     string
}

type MigrationDB interface {
	SchemaVersion(context.Context) (int, error)
	ApplyMigration(context.Context, Migration) error
}

func RunMigrations(ctx context.Context, db MigrationDB, migrations []Migration) (int, error) {
	ordered := append([]Migration(nil), migrations...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Version < ordered[j].Version })

	maxVersion := 0
	seen := map[int]struct{}{}
	for _, migration := range ordered {
		if migration.Version <= 0 {
			return 0, fmt.Errorf("migration %q has invalid version %d", migration.Name, migration.Version)
		}
		if _, ok := seen[migration.Version]; ok {
			return 0, fmt.Errorf("duplicate migration version %d", migration.Version)
		}
		seen[migration.Version] = struct{}{}
		maxVersion = migration.Version
	}

	current, err := db.SchemaVersion(ctx)
	if err != nil {
		return 0, err
	}
	if current > maxVersion {
		return current, ErrSchemaVersionTooNew
	}
	for _, migration := range ordered {
		if migration.Version <= current {
			continue
		}
		if err := db.ApplyMigration(ctx, migration); err != nil {
			return current, fmt.Errorf("apply migration %03d %s: %w", migration.Version, migration.Name, err)
		}
		current = migration.Version
	}
	return current, nil
}

type SQLMigrationDB struct {
	DB *sql.DB
}

func (db SQLMigrationDB) SchemaVersion(ctx context.Context) (int, error) {
	var version int
	err := db.DB.QueryRowContext(ctx, `SELECT version FROM schema_version ORDER BY applied_at DESC LIMIT 1`).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) || isMissingSchemaVersionTable(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return version, nil
}

func (db SQLMigrationDB) ApplyMigration(ctx context.Context, migration Migration) error {
	tx, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
		return err
	}
	if strings.TrimSpace(migration.SQL) != "" {
		if _, err := tx.ExecContext(ctx, migration.SQL); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version (version, applied_at) VALUES (?, ?)`, migration.Version, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return tx.Commit()
}

func isMissingSchemaVersionTable(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "no such table") || strings.Contains(message, "doesn't exist") || strings.Contains(message, "does not exist")
}

type RetentionPolicy struct {
	AuditRetentionDays int
	UsageRetentionDays int
}

type RetentionStatement struct {
	Kind          string
	Table         string
	TimeColumn    string
	RetentionDays int
	SQL           string
}

func BuildRetentionStatements(policy RetentionPolicy) []RetentionStatement {
	statements := make([]RetentionStatement, 0, 2)
	if policy.AuditRetentionDays > 0 {
		statements = append(statements, retentionStatement("audit", "proxy_audit_events", policy.AuditRetentionDays))
	}
	if policy.UsageRetentionDays > 0 {
		statements = append(statements, retentionStatement("usage", "proxy_usage_events", policy.UsageRetentionDays))
	}
	return statements
}

func retentionStatement(kind, table string, days int) RetentionStatement {
	return RetentionStatement{
		Kind:          kind,
		Table:         table,
		TimeColumn:    "occurred_at",
		RetentionDays: days,
		SQL:           fmt.Sprintf("DELETE FROM %s WHERE occurred_at < ?", table),
	}
}

type RetentionDB interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

type RetentionResult struct {
	Kind        string
	DeletedRows int64
}

type RetentionLogger func([]RetentionResult, error)

func RunRetention(ctx context.Context, db RetentionDB, now time.Time, policy RetentionPolicy) ([]RetentionResult, error) {
	statements := BuildRetentionStatements(policy)
	results := make([]RetentionResult, 0, len(statements))
	for _, statement := range statements {
		cutoff := now.UTC().AddDate(0, 0, -statement.RetentionDays)
		result, err := db.ExecContext(ctx, statement.SQL, cutoff)
		if err != nil {
			return results, fmt.Errorf("retention %s: %w", statement.Kind, err)
		}
		rows, _ := result.RowsAffected()
		results = append(results, RetentionResult{Kind: statement.Kind, DeletedRows: rows})
	}
	return results, nil
}

func RunRetentionWorker(ctx context.Context, db RetentionDB, interval time.Duration, policy RetentionPolicy, logger RetentionLogger) error {
	if interval <= 0 {
		return errors.New("retention interval must be positive")
	}
	if len(BuildRetentionStatements(policy)) == 0 {
		return nil
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		results, err := RunRetention(ctx, db, time.Now().UTC(), policy)
		if logger != nil {
			logger(results, err)
		}
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
