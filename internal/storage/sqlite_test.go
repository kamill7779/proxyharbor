package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/kamill7779/proxyharbor/internal/auth"
	"github.com/kamill7779/proxyharbor/internal/shared/domain"
)

func TestSQLiteLeaseCreateGetAndIdempotency(t *testing.T) {
	store := newTestSQLiteStore(t)
	ctx := context.Background()
	scope := testScope("idem-1")
	lease := testLease("lease-1")
	lease.Password = "plaintext-secret"

	created, err := store.CreateLease(ctx, scope, lease)
	if err != nil {
		t.Fatalf("CreateLease() error = %v", err)
	}
	if created.Password != "" {
		t.Fatalf("CreateLease() persisted/returned plaintext password %q", created.Password)
	}

	got, err := store.GetLease(ctx, lease.TenantID, lease.ID)
	if err != nil {
		t.Fatalf("GetLease() error = %v", err)
	}
	if got.Password != "" {
		t.Fatalf("GetLease() returned plaintext password %q", got.Password)
	}
	if got.PasswordHash != lease.PasswordHash || got.Subject.ID != lease.Subject.ID {
		t.Fatalf("GetLease() = %+v, want hash/subject from created lease", got)
	}

	replayed, ok, err := store.GetLeaseByIdempotency(ctx, scope)
	if err != nil || !ok {
		t.Fatalf("GetLeaseByIdempotency() = ok %t err %v", ok, err)
	}
	if replayed.ID != lease.ID || replayed.Password != "" {
		t.Fatalf("GetLeaseByIdempotency() = %+v", replayed)
	}

	createdAgain, err := store.CreateLease(ctx, scope, testLease("lease-other"))
	if err != nil {
		t.Fatalf("CreateLease() replay error = %v", err)
	}
	if createdAgain.ID != lease.ID || createdAgain.Password != "" {
		t.Fatalf("CreateLease() replay = %+v", createdAgain)
	}
}

func TestSQLiteLeaseRenewCASFailures(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(context.Context, *SQLiteStore, domain.Lease)
		want   error
	}{
		{
			name:   "stale",
			mutate: func(context.Context, *SQLiteStore, domain.Lease) {},
			want:   domain.ErrStaleLease,
		},
		{
			name: "revoked",
			mutate: func(ctx context.Context, store *SQLiteStore, lease domain.Lease) {
				if err := store.RevokeLease(ctx, lease.TenantID, lease.ID); err != nil {
					t.Fatalf("RevokeLease() error = %v", err)
				}
			},
			want: domain.ErrLeaseRevoked,
		},
		{
			name: "expired",
			mutate: func(ctx context.Context, store *SQLiteStore, lease domain.Lease) {
				_, err := store.db.ExecContext(ctx, `UPDATE proxy_leases SET expires_at = ? WHERE tenant_id = ? AND lease_id = ?`, time.Now().UTC().Add(-time.Minute), lease.TenantID, lease.ID)
				if err != nil {
					t.Fatalf("expire lease: %v", err)
				}
			},
			want: domain.ErrLeaseExpired,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestSQLiteStore(t)
			ctx := context.Background()
			created, err := store.CreateLease(ctx, testScope("idem-"+tc.name), testLease("lease-"+tc.name))
			if err != nil {
				t.Fatalf("CreateLease() error = %v", err)
			}
			tc.mutate(ctx, store, created)
			renewed := created
			renewed.Generation++
			renewed.ExpiresAt = time.Now().UTC().Add(2 * time.Hour)
			if tc.name == "stale" {
				renewed.Generation++
			}
			_, err = store.UpdateLease(ctx, renewed)
			if !errors.Is(err, tc.want) {
				t.Fatalf("UpdateLease() error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestSQLiteRevokeLeaseIsIdempotentForExistingLease(t *testing.T) {
	store := newTestSQLiteStore(t)
	ctx := context.Background()
	lease := testLease("lease-revoke-idempotent")
	if _, err := store.CreateLease(ctx, testScope("idem-revoke-idempotent"), lease); err != nil {
		t.Fatalf("CreateLease() error = %v", err)
	}
	if err := store.RevokeLease(ctx, lease.TenantID, lease.ID); err != nil {
		t.Fatalf("RevokeLease() first error = %v", err)
	}
	if err := store.RevokeLease(ctx, lease.TenantID, lease.ID); err != nil {
		t.Fatalf("RevokeLease() second error = %v", err)
	}
}

func TestSQLiteAuditListStableOrder(t *testing.T) {
	store := newTestSQLiteStore(t)
	ctx := context.Background()
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	err := store.AppendAuditEvents(ctx, []domain.AuditEvent{
		{EventID: "a", TenantID: "tenant-a", Action: "first", Resource: "r", OccurredAt: now},
		{EventID: "c", TenantID: "tenant-a", Action: "third", Resource: "r", OccurredAt: now.Add(time.Second)},
		{EventID: "b", TenantID: "tenant-a", Action: "second", Resource: "r", OccurredAt: now.Add(time.Second)},
	})
	if err != nil {
		t.Fatalf("AppendAuditEvents() error = %v", err)
	}
	events, err := store.ListAuditEvents(ctx, "tenant-a", 0)
	if err != nil {
		t.Fatalf("ListAuditEvents() error = %v", err)
	}
	ids := []string{events[0].EventID, events[1].EventID, events[2].EventID}
	want := []string{"c", "b", "a"}
	for idx := range want {
		if ids[idx] != want[idx] {
			t.Fatalf("ListAuditEvents() ids = %v, want %v", ids, want)
		}
	}
}

func TestSQLiteTenantKeyVersion(t *testing.T) {
	store := newTestSQLiteStore(t)
	ctx := context.Background()
	admin := store.AdminStore()
	if err := admin.CreateTenant(ctx, domain.Tenant{ID: "tenant-a", Name: "Tenant A", Enabled: true}); err != nil {
		t.Fatalf("CreateTenant() error = %v", err)
	}
	version, err := store.GetTenantKeysVersion(ctx)
	if err != nil {
		t.Fatalf("GetTenantKeysVersion() error = %v", err)
	}
	if version != 1 {
		t.Fatalf("initial version = %d, want 1", version)
	}
	if err := admin.CreateTenantKey(ctx, auth.TenantKey{ID: "key-a", TenantID: "tenant-a", KeyHash: "001122", KeyFP: "fp-a", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("CreateTenantKey() error = %v", err)
	}
	version, err = store.GetTenantKeysVersion(ctx)
	if err != nil {
		t.Fatalf("GetTenantKeysVersion() after create error = %v", err)
	}
	if version != 2 {
		t.Fatalf("version after create = %d, want 2", version)
	}
	if err := admin.RevokeTenantKey(ctx, "tenant-a", "key-a"); err != nil {
		t.Fatalf("RevokeTenantKey() error = %v", err)
	}
	version, err = store.GetTenantKeysVersion(ctx)
	if err != nil {
		t.Fatalf("GetTenantKeysVersion() after revoke error = %v", err)
	}
	if version != 3 {
		t.Fatalf("version after revoke = %d, want 3", version)
	}
}

func TestSQLiteProxyRoundTripScansLastSeenAt(t *testing.T) {
	store := newTestSQLiteStore(t)
	ctx := context.Background()
	proxy, err := store.UpsertProxy(ctx, domain.Proxy{ID: "proxy-a", Endpoint: "http://proxy.local:8080", Healthy: true, Weight: 1})
	if err != nil {
		t.Fatalf("UpsertProxy() error = %v", err)
	}
	if proxy.LastSeenAt.IsZero() {
		t.Fatalf("LastSeenAt is zero after UpsertProxy(): %+v", proxy)
	}
	proxies, err := store.ListCatalogProxies(ctx)
	if err != nil {
		t.Fatalf("ListCatalogProxies() error = %v", err)
	}
	if len(proxies) != 1 || proxies[0].ID != "proxy-a" || proxies[0].LastSeenAt.IsZero() {
		t.Fatalf("ListCatalogProxies() = %+v, want proxy-a with LastSeenAt", proxies)
	}
}

func TestSQLiteUpsertProxyPreservesExistingHealthStateWhenUnset(t *testing.T) {
	store := newTestSQLiteStore(t)
	ctx := context.Background()
	if _, err := store.UpsertProxy(ctx, domain.Proxy{ID: "proxy-a", Endpoint: "http://proxy.local:8080", Healthy: true, Weight: 1, HealthScore: 100}); err != nil {
		t.Fatalf("UpsertProxy() error = %v", err)
	}
	observedAt := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		if err := store.RecordProxyOutcome(ctx, "proxy-a", ProxyHealthDelta{ObservedAt: observedAt, Success: false, MaxConsecutiveFailure: 1, BaseCooldown: time.Minute, MaxCooldown: 2 * time.Minute}); err != nil {
			t.Fatalf("RecordProxyOutcome() error = %v", err)
		}
	}
	before, err := store.GetProxy(ctx, "proxy-a")
	if err != nil {
		t.Fatalf("GetProxy() error = %v", err)
	}
	updated, err := store.UpsertProxy(ctx, domain.Proxy{ID: "proxy-a", Endpoint: "http://proxy.local:9090", Healthy: true, Weight: 2})
	if err != nil {
		t.Fatalf("UpsertProxy() update error = %v", err)
	}
	if updated.HealthScore != before.HealthScore || updated.ConsecutiveFailures != before.ConsecutiveFailures || !updated.CircuitOpenUntil.Equal(before.CircuitOpenUntil) {
		t.Fatalf("health state was not preserved: before=%+v after=%+v", before, updated)
	}
}

func TestSQLiteProxyFailureUsesCappedCooldown(t *testing.T) {
	store := newTestSQLiteStore(t)
	ctx := context.Background()
	if _, err := store.UpsertProxy(ctx, domain.Proxy{ID: "proxy-a", Endpoint: "http://proxy.local:8080", Healthy: true, Weight: 1}); err != nil {
		t.Fatalf("UpsertProxy() error = %v", err)
	}
	observedAt := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if err := store.RecordProxyOutcome(ctx, "proxy-a", ProxyHealthDelta{ObservedAt: observedAt, Success: false, MaxConsecutiveFailure: 1, BaseCooldown: time.Minute, MaxCooldown: 2 * time.Minute}); err != nil {
			t.Fatalf("RecordProxyOutcome() error = %v", err)
		}
	}
	proxy, err := store.GetProxy(ctx, "proxy-a")
	if err != nil {
		t.Fatalf("GetProxy() error = %v", err)
	}
	want := observedAt.Add(2 * time.Minute)
	if !proxy.CircuitOpenUntil.Equal(want) {
		t.Fatalf("CircuitOpenUntil = %s, want capped %s", proxy.CircuitOpenUntil, want)
	}
}

func TestSQLiteTenantKeyHashUnique(t *testing.T) {
	store := newTestSQLiteStore(t)
	ctx := context.Background()
	admin := store.AdminStore()
	if err := admin.CreateTenant(ctx, domain.Tenant{ID: "tenant-a", Name: "Tenant A", Enabled: true}); err != nil {
		t.Fatalf("CreateTenant() error = %v", err)
	}
	key := auth.TenantKey{ID: "key-a", TenantID: "tenant-a", KeyHash: "001122", KeyFP: "fp-a", CreatedAt: time.Now().UTC()}
	if err := admin.CreateTenantKey(ctx, key); err != nil {
		t.Fatalf("CreateTenantKey() error = %v", err)
	}
	key.ID = "key-b"
	key.KeyFP = "fp-b"
	if err := admin.CreateTenantKey(ctx, key); !errors.Is(err, domain.ErrIdempotencyConflict) {
		t.Fatalf("CreateTenantKey() duplicate error = %v, want ErrIdempotencyConflict", err)
	}
}

func TestSQLiteAdminStoreCreateTenantKeyRejectsDisabledTenant(t *testing.T) {
	store := newTestSQLiteStore(t)
	ctx := context.Background()
	admin := store.AdminStore()
	if err := admin.CreateTenant(ctx, domain.Tenant{ID: "tenant-a", Name: "Tenant A", Enabled: true}); err != nil {
		t.Fatalf("CreateTenant() error = %v", err)
	}
	status := "disabled"
	if err := admin.UpdateTenant(ctx, "tenant-a", nil, &status); err != nil {
		t.Fatalf("UpdateTenant(disabled) error = %v", err)
	}
	err := admin.CreateTenantKey(ctx, auth.TenantKey{ID: "key-a", TenantID: "tenant-a", KeyHash: "001122", KeyFP: "fp-a", CreatedAt: time.Now().UTC()})
	if !errors.Is(err, domain.ErrTenantDisabled) {
		t.Fatalf("CreateTenantKey() error = %v, want ErrTenantDisabled", err)
	}
}

func TestSQLiteRevokeTenantKeyMissingDoesNotAdvanceVersion(t *testing.T) {
	store := newTestSQLiteStore(t)
	ctx := context.Background()
	version, err := store.GetTenantKeysVersion(ctx)
	if err != nil {
		t.Fatalf("GetTenantKeysVersion() error = %v", err)
	}
	err = store.RevokeTenantKey(ctx, "missing")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("RevokeTenantKey() error = %v, want ErrNotFound", err)
	}
	after, err := store.GetTenantKeysVersion(ctx)
	if err != nil {
		t.Fatalf("GetTenantKeysVersion() after revoke error = %v", err)
	}
	if after != version {
		t.Fatalf("version after missing revoke = %d, want %d", after, version)
	}
}

func TestSQLitePathWithQueryCharacters(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "proxy#harbor&single.db")
	store, err := NewSQLiteStore(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	_ = store.Close()
}

func newTestSQLiteStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store, err := NewSQLiteStore(context.Background(), t.TempDir()+"/proxyharbor.db")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func testScope(key string) IdempotencyScope {
	return IdempotencyScope{TenantID: "tenant-a", StableSubjectID: "workload:subject-a", ResourceRef: "proxy:default:resource-a", RequestKind: "create_lease", Key: key}
}

func testLease(id string) domain.Lease {
	now := time.Now().UTC()
	return domain.Lease{
		ID: id, TenantID: "tenant-a", Generation: 1,
		Subject:     domain.Subject{Type: "workload", ID: "subject-a"},
		ResourceRef: domain.ResourceRef{Kind: "proxy", Scope: "default", ID: "resource-a"},
		PolicyRef:   domain.PolicyRef{ID: "default", Version: 1, Hash: "hash"},
		GatewayURL:  "http://localhost:8080", Username: "user", PasswordHash: "hash", ProxyID: "proxy-a",
		ExpiresAt: now.Add(time.Hour), RenewBefore: now.Add(30 * time.Minute), CatalogVersion: "catalog", CandidateSetID: "candidate",
		CreatedAt: now, UpdatedAt: now,
	}
}

func TestSQLitePerformancePragmasAndIndexes(t *testing.T) {
	store := newTestSQLiteStore(t)
	ctx := context.Background()

	pragmaChecks := map[string]int{
		"foreign_keys": 1,
		"busy_timeout": 5000,
		"synchronous":  1,
		"temp_store":   2,
	}
	for name, want := range pragmaChecks {
		var got int
		if err := store.DB().QueryRowContext(ctx, "PRAGMA "+name).Scan(&got); err != nil {
			t.Fatalf("PRAGMA %s error = %v", name, err)
		}
		if got != want {
			t.Fatalf("PRAGMA %s = %d, want %d", name, got, want)
		}
	}

	wantIndexes := []string{
		"idx_proxy_leases_active",
		"idx_proxy_leases_renew_cas",
		"idx_proxy_leases_proxy_active",
		"idx_proxy_idempotency_tenant_created",
		"idx_proxy_usage_events_tenant_order",
		"idx_proxies_selectable",
		"idx_proxy_catalog_snapshots_fresh",
		"idx_tenant_keys_active_refresh",
	}
	for _, name := range wantIndexes {
		var count int
		if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, name).Scan(&count); err != nil {
			t.Fatalf("index lookup %s error = %v", name, err)
		}
		if count != 1 {
			t.Fatalf("index %s count = %d, want 1", name, count)
		}
	}
}
