package storage

import (
	"context"
	"strings"
	"testing"
)

type fakeInspector struct {
	tables     map[string]map[string]struct{}
	seedExists bool
	tableEr    error
	colEr      error
	seedEr     error
}

func (f fakeInspector) tableExists(_ context.Context, table string) (bool, error) {
	if f.tableEr != nil {
		return false, f.tableEr
	}
	_, ok := f.tables[table]
	return ok, nil
}

func (f fakeInspector) columns(_ context.Context, table string) (map[string]struct{}, error) {
	if f.colEr != nil {
		return nil, f.colEr
	}
	cols, ok := f.tables[table]
	if !ok {
		return map[string]struct{}{}, nil
	}
	return cols, nil
}

func (f fakeInspector) tenantKeysVersionSeedExists(context.Context) (bool, error) {
	if f.seedEr != nil {
		return false, f.seedEr
	}
	return f.seedExists, nil
}

func cols(names ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(names))
	for _, n := range names {
		out[strings.ToLower(n)] = struct{}{}
	}
	return out
}

func fullSchema() map[string]map[string]struct{} {
	return map[string]map[string]struct{}{
		"tenants":             cols("id", "display_name", "status", "created_at", "deleted_at"),
		"tenant_keys":         cols("id", "tenant_id", "key_hash", "key_fp", "revoked_at", "updated_at"),
		"tenant_keys_version": cols("id", "version", "updated_at"),
	}
}

func TestEnsureDynamicAuthSchema_OK(t *testing.T) {
	if err := ensureDynamicAuthSchema(context.Background(), fakeInspector{tables: fullSchema(), seedExists: true}); err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
}

func TestEnsureDynamicAuthSchema_MissingTenants(t *testing.T) {
	tables := fullSchema()
	delete(tables, "tenants")
	err := ensureDynamicAuthSchema(context.Background(), fakeInspector{tables: tables, seedExists: true})
	if err == nil || !strings.Contains(err.Error(), "tenants") || !strings.Contains(err.Error(), "003_tenants.sql") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEnsureDynamicAuthSchema_MissingTenantKeysVersion(t *testing.T) {
	tables := fullSchema()
	delete(tables, "tenant_keys_version")
	err := ensureDynamicAuthSchema(context.Background(), fakeInspector{tables: tables, seedExists: true})
	if err == nil || !strings.Contains(err.Error(), "tenant_keys_version") || !strings.Contains(err.Error(), "004_tenant_keys.sql") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEnsureDynamicAuthSchema_MissingColumn(t *testing.T) {
	tables := fullSchema()
	tables["tenant_keys"] = cols("id", "tenant_id", "key_hash") // missing revoked_at + updated_at
	err := ensureDynamicAuthSchema(context.Background(), fakeInspector{tables: tables, seedExists: true})
	if err == nil || !strings.Contains(err.Error(), "missing column") {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(err.Error(), "revoked_at") || !strings.Contains(err.Error(), "updated_at") {
		t.Fatalf("expected revoked_at and updated_at in error, got: %v", err)
	}
}

func TestEnsureDynamicAuthSchema_MissingTenantKeysVersionSeed(t *testing.T) {
	err := ensureDynamicAuthSchema(context.Background(), fakeInspector{tables: fullSchema()})
	if err == nil || !strings.Contains(err.Error(), "tenant_keys_version seed row id=1") || !strings.Contains(err.Error(), "004_tenant_keys.sql") {
		t.Fatalf("unexpected error: %v", err)
	}
}
