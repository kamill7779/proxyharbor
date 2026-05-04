package control

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kamill7779/proxyharbor/internal/control/selector"
	"github.com/kamill7779/proxyharbor/internal/shared/domain"
	"github.com/kamill7779/proxyharbor/internal/storage"
)

type countingIdempotencyStore struct {
	*storage.MemoryStore
	getLeaseByIdempotencyCalls atomic.Int64
}

func (s *countingIdempotencyStore) GetLeaseByIdempotency(ctx context.Context, scope storage.IdempotencyScope) (domain.Lease, bool, error) {
	s.getLeaseByIdempotencyCalls.Add(1)
	return s.MemoryStore.GetLeaseByIdempotency(ctx, scope)
}

type failingSelector struct {
	err error
}

func (s failingSelector) Select(context.Context, []domain.Proxy, selector.SelectOptions) (domain.Proxy, error) {
	return domain.Proxy{}, s.err
}

type blockingSelector struct{}

func (s blockingSelector) Select(ctx context.Context, _ []domain.Proxy, _ selector.SelectOptions) (domain.Proxy, error) {
	<-ctx.Done()
	return domain.Proxy{}, ctx.Err()
}

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
	resolver := newStubResolver()
	resolver.ips["example.com"] = []net.IP{net.ParseIP("1.1.1.1")}
	svc.resolver = resolver

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
	resolver := newStubResolver()
	resolver.ips["example.com"] = []net.IP{net.ParseIP("1.1.1.1")}
	svc.resolver = resolver

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
	resolver := newStubResolver()
	resolver.ips["example.com"] = []net.IP{net.ParseIP("1.1.1.1")}
	svc.resolver = resolver
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

func TestCreateLeaseDoesNotPreflightIdempotencyLookup(t *testing.T) {
	ctx := context.Background()
	store := &countingIdempotencyStore{MemoryStore: storage.NewMemoryStore()}
	if _, err := store.UpsertProxy(ctx, domain.Proxy{ID: "proxy-a", Endpoint: "http://proxy.local:8080", Healthy: true, Weight: 1}); err != nil {
		t.Fatalf("UpsertProxy() error = %v", err)
	}
	svc := NewService(store, "http://gateway.local")
	resolver := newStubResolver()
	resolver.ips["example.com"] = []net.IP{net.ParseIP("1.1.1.1")}
	svc.resolver = resolver

	req := CreateLeaseRequest{
		Subject:     domain.Subject{Type: "user", ID: "user-a"},
		ResourceRef: domain.ResourceRef{Kind: "url", ID: "https://example.com/resource"},
	}
	first, err := svc.CreateLease(ctx, domain.Principal{TenantID: "tenant-a"}, "idem-no-preflight", req)
	if err != nil {
		t.Fatalf("first CreateLease() error = %v", err)
	}
	second, err := svc.CreateLease(ctx, domain.Principal{TenantID: "tenant-a"}, "idem-no-preflight", req)
	if err != nil {
		t.Fatalf("second CreateLease() error = %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("replayed lease id = %q, want %q", second.ID, first.ID)
	}
	if second.Password != "" {
		t.Fatalf("replayed lease password = %q, want empty replay response", second.Password)
	}
	if got := store.getLeaseByIdempotencyCalls.Load(); got != 0 {
		t.Fatalf("GetLeaseByIdempotency() calls = %d, want 0 because replay should come from store.CreateLease", got)
	}
}

func TestCreateLeaseUsesIdempotencyDerivedLeaseID(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemoryStore()
	if _, err := store.UpsertProxy(ctx, domain.Proxy{ID: "proxy-a", Endpoint: "http://proxy.local:8080", Healthy: true, Weight: 1}); err != nil {
		t.Fatalf("UpsertProxy() error = %v", err)
	}
	svc := NewService(store, "http://gateway.local")
	resolver := newStubResolver()
	resolver.ips["example.com"] = []net.IP{net.ParseIP("1.1.1.1")}
	svc.resolver = resolver

	req := CreateLeaseRequest{
		Subject:     domain.Subject{Type: "user", ID: "user-a"},
		ResourceRef: domain.ResourceRef{Kind: "url", ID: "https://example.com/resource"},
	}
	key := "idem-derived"
	created, err := svc.CreateLease(ctx, domain.Principal{TenantID: "tenant-a"}, key, req)
	if err != nil {
		t.Fatalf("CreateLease() error = %v", err)
	}
	scope := storage.IdempotencyScope{TenantID: "tenant-a", StableSubjectID: req.Subject.StableID(), ResourceRef: req.ResourceRef.StableID(), RequestKind: "create_lease", Key: key}
	if want := storage.LeaseIDForIdempotency(scope); created.ID != want {
		t.Fatalf("created lease id = %q, want idempotency-derived %q", created.ID, want)
	}
	if created.Username != "tenant-a|"+created.ID {
		t.Fatalf("created username = %q, want tenant-scoped lease id", created.Username)
	}
}

func TestCreateLeaseFallsBackToLocalSelectorWhenZFairReturnsEmpty(t *testing.T) {
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
	svc.SetSelector(failingSelector{
		err: domain.NewKindedError(domain.ErrNoHealthyProxy, domain.ErrorKindSelectorEmptyResult, "redis_empty_result", nil),
	})
	resolver := newStubResolver()
	resolver.ips["example.com"] = []net.IP{net.ParseIP("1.1.1.1")}
	svc.resolver = resolver

	created, err := svc.CreateLease(ctx, domain.Principal{TenantID: "tenant-a"}, "idem-selector-fallback", CreateLeaseRequest{
		Subject:     domain.Subject{Type: "user", ID: "user-a"},
		ResourceRef: domain.ResourceRef{Kind: "url", ID: "https://example.com/resource"},
	})
	if err != nil {
		t.Fatalf("CreateLease() error = %v, want local selector fallback success", err)
	}
	if created.ProxyID == "" {
		t.Fatal("CreateLease() returned empty proxy id after local selector fallback")
	}
}

func TestCreateLeaseFallsBackToLocalSelectorWhenZFairTimesOut(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	store := storage.NewMemoryStore()
	if _, err := store.UpsertProxy(ctx, domain.Proxy{ID: "proxy-a", Endpoint: "http://proxy-a.local:8080", Healthy: true, Weight: 1}); err != nil {
		t.Fatalf("UpsertProxy() error = %v", err)
	}
	svc := NewService(store, "http://gateway.local")
	svc.SetSelector(blockingSelector{})
	svc.selectorTimeout = time.Millisecond
	resolver := newStubResolver()
	resolver.ips["example.com"] = []net.IP{net.ParseIP("1.1.1.1")}
	svc.resolver = resolver

	created, err := svc.CreateLease(ctx, domain.Principal{TenantID: "tenant-a"}, "idem-selector-timeout", CreateLeaseRequest{
		Subject:     domain.Subject{Type: "user", ID: "user-a"},
		ResourceRef: domain.ResourceRef{Kind: "url", ID: "https://example.com/resource"},
	})
	if err != nil {
		t.Fatalf("CreateLease() error = %v, want local selector fallback on zfair timeout", err)
	}
	if created.ProxyID != "proxy-a" {
		t.Fatalf("CreateLease() ProxyID = %q, want local fallback proxy-a", created.ProxyID)
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
	resolver := newStubResolver()
	resolver.ips["example.com"] = []net.IP{net.ParseIP("1.1.1.1")}
	svc.resolver = resolver

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
