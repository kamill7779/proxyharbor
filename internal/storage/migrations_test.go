package storage

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"
	"time"
)

type migrationFakeDB struct {
	version int
	applied []string
}

func (db *migrationFakeDB) SchemaVersion(context.Context) (int, error) { return db.version, nil }

func (db *migrationFakeDB) ApplyMigration(_ context.Context, migration Migration) error {
	db.applied = append(db.applied, migration.Name)
	db.version = migration.Version
	return nil
}

func TestRunMigrationsAppliesOrderedMigrations(t *testing.T) {
	db := &migrationFakeDB{}
	migrations := []Migration{
		{Version: 2, Name: "second", SQL: "ALTER TABLE things ADD COLUMN name TEXT"},
		{Version: 1, Name: "first", SQL: "CREATE TABLE things (id INTEGER PRIMARY KEY)"},
	}

	version, err := RunMigrations(context.Background(), db, migrations)
	if err != nil {
		t.Fatalf("RunMigrations returned error: %v", err)
	}
	if version != 2 {
		t.Fatalf("version = %d, want 2", version)
	}
	if !reflect.DeepEqual(db.applied, []string{"first", "second"}) {
		t.Fatalf("applied = %#v", db.applied)
	}
}

func TestRunMigrationsRejectsFutureSchemaVersion(t *testing.T) {
	db := &migrationFakeDB{version: 3}

	_, err := RunMigrations(context.Background(), db, []Migration{{Version: 2, Name: "second", SQL: "SELECT 1"}})
	if !errors.Is(err, ErrSchemaVersionTooNew) {
		t.Fatalf("err = %v, want ErrSchemaVersionTooNew", err)
	}
}

func TestBuildRetentionStatements(t *testing.T) {
	statements := BuildRetentionStatements(RetentionPolicy{AuditRetentionDays: 30, UsageRetentionDays: 7})
	want := []RetentionStatement{
		{Kind: "audit", Table: "proxy_audit_events", TimeColumn: "occurred_at", RetentionDays: 30, SQL: "DELETE FROM proxy_audit_events WHERE occurred_at < ?"},
		{Kind: "usage", Table: "proxy_usage_events", TimeColumn: "occurred_at", RetentionDays: 7, SQL: "DELETE FROM proxy_usage_events WHERE occurred_at < ?"},
	}
	if !reflect.DeepEqual(statements, want) {
		t.Fatalf("statements = %#v, want %#v", statements, want)
	}
}

func TestBuildRetentionStatementsSkipsDisabledPolicies(t *testing.T) {
	statements := BuildRetentionStatements(RetentionPolicy{AuditRetentionDays: 0, UsageRetentionDays: -1})
	if len(statements) != 0 {
		t.Fatalf("statements = %#v, want empty", statements)
	}
}

type retentionFakeDB struct {
	queries []string
	args    []any
}

func (db *retentionFakeDB) ExecContext(_ context.Context, query string, args ...any) (sql.Result, error) {
	db.queries = append(db.queries, query)
	db.args = append(db.args, args...)
	return fakeSQLResult(2), nil
}

type fakeSQLResult int64

func (r fakeSQLResult) LastInsertId() (int64, error) { return 0, nil }
func (r fakeSQLResult) RowsAffected() (int64, error) { return int64(r), nil }

func TestRunRetentionDeletesAuditAndUsageByCutoff(t *testing.T) {
	db := &retentionFakeDB{}
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)

	results, err := RunRetention(context.Background(), db, now, RetentionPolicy{AuditRetentionDays: 30, UsageRetentionDays: 7})
	if err != nil {
		t.Fatalf("RunRetention returned error: %v", err)
	}
	wantQueries := []string{"DELETE FROM proxy_audit_events WHERE occurred_at < ?", "DELETE FROM proxy_usage_events WHERE occurred_at < ?"}
	if !reflect.DeepEqual(db.queries, wantQueries) {
		t.Fatalf("queries = %#v, want %#v", db.queries, wantQueries)
	}
	wantArgs := []any{now.AddDate(0, 0, -30), now.AddDate(0, 0, -7)}
	if !reflect.DeepEqual(db.args, wantArgs) {
		t.Fatalf("args = %#v, want %#v", db.args, wantArgs)
	}
	wantResults := []RetentionResult{{Kind: "audit", DeletedRows: 2}, {Kind: "usage", DeletedRows: 2}}
	if !reflect.DeepEqual(results, wantResults) {
		t.Fatalf("results = %#v, want %#v", results, wantResults)
	}
}
