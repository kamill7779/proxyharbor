package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// EnsureDynamicAuthSchema verifies the MySQL schema contains the tables and
// columns that dynamic-keys auth mode depends on. See ensureDynamicAuthSchema
// for the actual checks; this thin wrapper adapts a *sql.DB into the
// inspector used by the tests.
func EnsureDynamicAuthSchema(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return errors.New("dynamic auth schema check: nil db")
	}
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return ensureDynamicAuthSchema(checkCtx, sqlInspector{db: db})
}

// schemaInspector abstracts the information_schema queries used by
// EnsureDynamicAuthSchema so the schema gate can be unit-tested without a
// live MySQL.
type schemaInspector interface {
	tableExists(ctx context.Context, table string) (bool, error)
	columns(ctx context.Context, table string) (map[string]struct{}, error)
	tenantKeysVersionSeedExists(ctx context.Context) (bool, error)
}

func ensureDynamicAuthSchema(ctx context.Context, inspector schemaInspector) error {
	required := []schemaRequirement{
		{table: "tenants", columns: []string{"id", "status", "deleted_at"}, migration: "003_tenants.sql"},
		{table: "tenant_keys", columns: []string{"id", "tenant_id", "key_hash", "revoked_at", "updated_at"}, migration: "004_tenant_keys.sql"},
		{table: "tenant_keys_version", columns: []string{"id", "version"}, migration: "004_tenant_keys.sql"},
	}
	for _, req := range required {
		exists, err := inspector.tableExists(ctx, req.table)
		if err != nil {
			return fmt.Errorf("dynamic auth schema check: %s: %w", req.table, err)
		}
		if !exists {
			return fmt.Errorf("dynamic auth schema check: missing table %q (apply migrations/mysql/%s)", req.table, req.migration)
		}
		present, err := inspector.columns(ctx, req.table)
		if err != nil {
			return fmt.Errorf("dynamic auth schema check: %s columns: %w", req.table, err)
		}
		var missing []string
		for _, want := range req.columns {
			if _, ok := present[strings.ToLower(want)]; !ok {
				missing = append(missing, want)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("dynamic auth schema check: table %q missing column(s) %s (apply migrations/mysql/%s)", req.table, strings.Join(missing, ", "), req.migration)
		}
	}
	seedExists, err := inspector.tenantKeysVersionSeedExists(ctx)
	if err != nil {
		return fmt.Errorf("dynamic auth schema check: tenant_keys_version seed: %w", err)
	}
	if !seedExists {
		return fmt.Errorf("dynamic auth schema check: missing tenant_keys_version seed row id=1 (apply migrations/mysql/004_tenant_keys.sql)")
	}
	return nil
}

type schemaRequirement struct {
	table     string
	columns   []string
	migration string
}

type sqlInspector struct {
	db *sql.DB
}

func (s sqlInspector) tableExists(ctx context.Context, table string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?`, table,
	).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s sqlInspector) columns(ctx context.Context, table string) (map[string]struct{}, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT column_name FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ?`, table,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]struct{})
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[strings.ToLower(name)] = struct{}{}
	}
	return out, rows.Err()
}

func (s sqlInspector) tenantKeysVersionSeedExists(ctx context.Context) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tenant_keys_version WHERE id = 1`).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
