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
	defaultPolicySeedExists(ctx context.Context) (bool, error)
	defaultProviderSeedExists(ctx context.Context) (bool, error)
}

func ensureDynamicAuthSchema(ctx context.Context, inspector schemaInspector) error {
	required := []schemaRequirement{
		{table: "tenants", columns: []string{"id", "status", "deleted_at"}},
		{table: "tenant_keys", columns: []string{"id", "tenant_id", "key_hash", "revoked_at", "updated_at"}},
		{table: "tenant_keys_version", columns: []string{"id", "version"}},
		{table: "proxy_sources", columns: []string{"source_id", "kind", "enabled"}},
		{table: "proxies", columns: []string{"proxy_id", "source_id", "endpoint", "healthy", "weight", "health_score"}},
		{table: "policies", columns: []string{"policy_id", "version", "enabled", "ttl_seconds"}},
		{table: "proxy_leases", columns: []string{"lease_id", "tenant_id", "policy_ref_json", "proxy_id", "password_hash"}},
		{table: "proxy_idempotency_keys", columns: []string{"idempotency_key", "tenant_id", "lease_id"}},
	}
	for _, req := range required {
		exists, err := inspector.tableExists(ctx, req.table)
		if err != nil {
			return fmt.Errorf("dynamic auth schema check: %s: %w", req.table, err)
		}
		if !exists {
			return fmt.Errorf("schema check: missing table %q (apply migrations/mysql/init.sql)", req.table)
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
			return fmt.Errorf("schema check: table %q missing column(s) %s (apply migrations/mysql/init.sql)", req.table, strings.Join(missing, ", "))
		}
	}
	seedExists, err := inspector.tenantKeysVersionSeedExists(ctx)
	if err != nil {
		return fmt.Errorf("dynamic auth schema check: tenant_keys_version seed: %w", err)
	}
	if !seedExists {
		return fmt.Errorf("schema check: missing tenant_keys_version seed row id=1 (apply migrations/mysql/init.sql)")
	}
	defaultExists, err := inspector.defaultPolicySeedExists(ctx)
	if err != nil {
		return fmt.Errorf("schema check: default policy seed: %w", err)
	}
	if !defaultExists {
		return fmt.Errorf("schema check: missing enabled policies.default seed (apply migrations/mysql/init.sql)")
	}
	providerExists, err := inspector.defaultProviderSeedExists(ctx)
	if err != nil {
		return fmt.Errorf("schema check: default provider seed: %w", err)
	}
	if !providerExists {
		return fmt.Errorf("schema check: missing enabled proxy_sources.default seed (apply migrations/mysql/init.sql)")
	}
	return nil
}

type schemaRequirement struct {
	table   string
	columns []string
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

func (s sqlInspector) defaultPolicySeedExists(ctx context.Context) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM policies WHERE policy_id = 'default' AND enabled = TRUE`).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s sqlInspector) defaultProviderSeedExists(ctx context.Context) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM proxy_sources WHERE source_id = 'default' AND enabled = TRUE`).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
