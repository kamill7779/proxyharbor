package control

import (
	"context"
	"testing"
	"time"

	"github.com/kamill7779/proxyharbor/internal/shared/domain"
	"github.com/kamill7779/proxyharbor/internal/storage"
)

func TestRenewLeaseUsesSingleGenerationBump(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemoryStore()
	_, err := store.UpsertProxy(ctx, domain.Proxy{ID: "proxy-a", Endpoint: "http://proxy.local:8080", Healthy: true, Weight: 1})
	if err != nil {
		t.Fatalf("UpsertProxy() error = %v", err)
	}
	now := time.Now().UTC()
	svc := NewService(store, "http://gateway.local")
	svc.now = func() time.Time { return now }

	principal := domain.Principal{TenantID: "tenant-a"}
	created, err := svc.CreateLease(ctx, principal, "idem-1", CreateLeaseRequest{
		Subject:     domain.Subject{Type: "user", ID: "user-a"},
		ResourceRef: domain.ResourceRef{Kind: "url", ID: "https://example.com/resource"},
	})
	if err != nil {
		t.Fatalf("CreateLease() error = %v", err)
	}
	if created.Generation != 1 {
		t.Fatalf("created generation = %d, want 1", created.Generation)
	}

	svc.now = func() time.Time { return created.CreatedAt.Add(time.Minute) }
	renewed, err := svc.RenewLease(ctx, principal, created.ID)
	if err != nil {
		t.Fatalf("RenewLease() error = %v", err)
	}
	if renewed.Generation != 2 {
		t.Fatalf("renewed generation = %d, want 2", renewed.Generation)
	}
}

func TestCreateLeaseReturnsTenantScopedGatewayUsername(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemoryStore()
	_, err := store.UpsertProxy(ctx, domain.Proxy{ID: "proxy-a", Endpoint: "http://proxy.local:8080", Healthy: true, Weight: 1})
	if err != nil {
		t.Fatalf("UpsertProxy() error = %v", err)
	}
	svc := NewService(store, "http://gateway.local")

	created, err := svc.CreateLease(ctx, domain.Principal{TenantID: "tenant-a"}, "idem-username", CreateLeaseRequest{
		Subject:     domain.Subject{Type: "user", ID: "user-a"},
		ResourceRef: domain.ResourceRef{Kind: "url", ID: "https://example.com/resource"},
	})
	if err != nil {
		t.Fatalf("CreateLease() error = %v", err)
	}
	if created.Username != "tenant-a|"+created.ID {
		t.Fatalf("created username = %q, want %q", created.Username, "tenant-a|"+created.ID)
	}
}

func TestCreateLeaseDefaultSelectorRotatesLocalProxies(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemoryStore()
	for _, proxy := range []domain.Proxy{
		{ID: "proxy-a", Endpoint: "http://proxy-a.local:8080", Healthy: true, Weight: 1},
		{ID: "proxy-b", Endpoint: "http://proxy-b.local:8080", Healthy: true, Weight: 1},
	} {
		if _, err := store.UpsertProxy(ctx, proxy); err != nil {
			t.Fatalf("UpsertProxy() error = %v", err)
		}
	}
	svc := NewService(store, "http://gateway.local")
	seen := map[string]bool{}

	for i := 0; i < 4; i++ {
		created, err := svc.CreateLease(ctx, domain.Principal{TenantID: "tenant-a"}, "idem-local-"+string(rune('a'+i)), CreateLeaseRequest{
			Subject:     domain.Subject{Type: "user", ID: "user-a"},
			ResourceRef: domain.ResourceRef{Kind: "url", ID: "https://example.com/resource"},
		})
		if err != nil {
			t.Fatalf("CreateLease() error = %v", err)
		}
		seen[created.ProxyID] = true
	}
	if !seen["proxy-a"] || !seen["proxy-b"] {
		t.Fatalf("seen proxies = %+v, want both proxy-a and proxy-b", seen)
	}
}

func TestValidateLeaseRechecksStoreTruthWhenCacheIsStale(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemoryStore()
	_, err := store.UpsertProxy(ctx, domain.Proxy{ID: "proxy-a", Endpoint: "http://proxy.local:8080", Healthy: true, Weight: 1})
	if err != nil {
		t.Fatalf("UpsertProxy() error = %v", err)
	}
	cache := cacheStub{}
	svc := NewService(store, "http://gateway.local")
	svc.SetCache(cache, time.Minute)

	created, err := svc.CreateLease(ctx, domain.Principal{TenantID: "tenant-a"}, "idem-stale-cache", CreateLeaseRequest{
		Subject:     domain.Subject{Type: "user", ID: "user-a"},
		ResourceRef: domain.ResourceRef{Kind: "url", ID: "https://example.com/resource"},
	})
	if err != nil {
		t.Fatalf("CreateLease() error = %v", err)
	}
	plaintext := created.Password
	cached := created
	cached.Revoked = false
	cached.Password = ""
	cache.lease = cached
	cache.hit = true

	if err := store.RevokeLease(ctx, created.TenantID, created.ID); err != nil {
		t.Fatalf("RevokeLease() error = %v", err)
	}
	if _, err := svc.ValidateLease(ctx, created.TenantID, created.ID, plaintext, "https://example.com/resource"); err != domain.ErrLeaseRevoked {
		t.Fatalf("ValidateLease() error = %v, want ErrLeaseRevoked", err)
	}
}

type cacheStub struct {
	lease domain.Lease
	hit   bool
}

func (c cacheStub) GetLease(context.Context, string, string) (domain.Lease, bool, error) {
	return c.lease, c.hit, nil
}
func (cacheStub) PutLease(context.Context, domain.Lease, time.Duration) error { return nil }
func (cacheStub) InvalidateLease(context.Context, string, string) error       { return nil }
func (cacheStub) GetCatalog(context.Context) (domain.Catalog, bool, error) {
	return domain.Catalog{}, false, nil
}
func (cacheStub) PutCatalog(context.Context, domain.Catalog, time.Duration) error { return nil }
func (cacheStub) InvalidateCatalog(context.Context) error                         { return nil }

func (cacheStub) Close() error { return nil }
