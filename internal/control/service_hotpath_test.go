package control

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kamill7779/proxyharbor/internal/shared/domain"
	"github.com/kamill7779/proxyharbor/internal/storage"
)

// stubResolver counts LookupIP calls and returns either a public or private
// IP based on the host name. It implements the same interface that
// net.Resolver exposes through Service.resolver.
type stubResolver struct {
	calls atomic.Int64
}

// resolverWrapper wraps net.Resolver but with a configurable Dial that lets
// us feed deterministic responses without going to real DNS.
//
// The control service uses *net.Resolver. To intercept lookups we install a
// resolver whose Dial routes to an in-memory DNS responder. That is more work
// than necessary for these tests; instead, drive Service through its public
// safe-host helper and assert that subsequent calls within the TTL do not
// invoke the resolver. To do that we replace s.resolver with a custom
// implementation that satisfies the relevant call site by setting Dial to a
// function returning an error so an unexpected lookup is observable. We then
// rely on a host literal that bypasses DNS for the public-IP fast path test
// and on the host safety cache for the cached-decision test.

// countingStore wraps a Store and counts GetLease invocations so tests can
// assert that ValidateLease short-circuits on the local truth cache.
type countingStore struct {
	storage.Store
	getLeaseCalls atomic.Int64
}

func (s *countingStore) GetLease(ctx context.Context, tenantID, id string) (domain.Lease, error) {
	s.getLeaseCalls.Add(1)
	return s.Store.GetLease(ctx, tenantID, id)
}

// captureCache wraps the cacheStub but records the most recently stored lease
// so tests can simulate stale cache states across calls.
type captureCache struct {
	mu      sync.Mutex
	lease   domain.Lease
	hit     bool
	gets    atomic.Int64
	puts    atomic.Int64
	invalid atomic.Int64
}

func (c *captureCache) GetLease(_ context.Context, _ string, _ string) (domain.Lease, bool, error) {
	c.gets.Add(1)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lease, c.hit, nil
}

func (c *captureCache) PutLease(_ context.Context, lease domain.Lease, _ time.Duration) error {
	c.puts.Add(1)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lease = lease
	c.hit = true
	return nil
}

func (c *captureCache) InvalidateLease(_ context.Context, _ string, _ string) error {
	c.invalid.Add(1)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hit = false
	c.lease = domain.Lease{}
	return nil
}

func (c *captureCache) GetCatalog(context.Context) (domain.Catalog, bool, error) {
	return domain.Catalog{}, false, nil
}
func (c *captureCache) PutCatalog(context.Context, domain.Catalog, time.Duration) error { return nil }
func (c *captureCache) InvalidateCatalog(context.Context) error                         { return nil }
func (c *captureCache) Close() error                                                    { return nil }

// withTracingResolver swaps the Service resolver with one whose Dial increments
// a counter every time the resolver tries to issue a network query. This lets
// tests detect whether a hostname was looked up (cache miss) versus served
// from the in-process safety cache.
func withTracingResolver(t *testing.T, svc *Service) *atomic.Int64 {
	t.Helper()
	var calls atomic.Int64
	svc.resolver = &net.Resolver{
		PreferGo: true,
		Dial: func(_ context.Context, _, _ string) (net.Conn, error) {
			calls.Add(1)
			return nil, errors.New("dns disabled in test")
		},
	}
	return &calls
}

func TestSafeTargetCachesHostDecisionWithinTTL(t *testing.T) {
	store := storage.NewMemoryStore()
	svc := NewService(store, "http://gateway.local")
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }
	calls := withTracingResolver(t, svc)

	// Pre-warm the cache with a positive decision the way the production
	// path would after a successful resolution.
	svc.storeHostSafety("example.com", true)

	for i := 0; i < 5; i++ {
		if !svc.safeTarget("https://example.com/resource") {
			t.Fatalf("safeTarget(%d) = false, want true", i)
		}
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("resolver dials = %d, want 0 within TTL", got)
	}
}

func TestSafeTargetCacheExpiryForcesReResolve(t *testing.T) {
	store := storage.NewMemoryStore()
	svc := NewService(store, "http://gateway.local")
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }
	calls := withTracingResolver(t, svc)
	svc.storeHostSafety("example.com", true)

	if !svc.safeTarget("https://example.com/resource") {
		t.Fatal("first safeTarget = false, want true (cached)")
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("dials before expiry = %d, want 0", got)
	}

	// Advance well past the cache TTL.
	svc.now = func() time.Time { return now.Add(2 * svc.hostSafetyTTL) }
	// The expired cache entry should now force a resolver call. The stub
	// resolver always errors, so safeTarget must fail closed.
	if svc.safeTarget("https://example.com/resource") {
		t.Fatal("safeTarget after expiry = true, want false on resolver error")
	}
	if got := calls.Load(); got == 0 {
		t.Fatal("dials after expiry = 0, want >=1")
	}
}

func TestSafeTargetPublicIPSkipsResolver(t *testing.T) {
	store := storage.NewMemoryStore()
	svc := NewService(store, "http://gateway.local")
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }
	calls := withTracingResolver(t, svc)

	if !svc.safeTarget("https://1.1.1.1/resource") {
		t.Fatal("safeTarget for public IP = false, want true")
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("public IP dialed resolver %d times, want 0", got)
	}
}

func TestSafeTargetUnsafeHostsRemainRejected(t *testing.T) {
	store := storage.NewMemoryStore()
	svc := NewService(store, "http://gateway.local")
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }

	for _, host := range []string{
		"http://localhost/resource",
		"http://127.0.0.1/resource",
		"http://10.0.0.5/resource",
		"http://192.168.1.10/resource",
		"http://169.254.169.254/resource",
		"http://100.64.0.1/resource",
		"",
	} {
		if svc.safeTarget(host) {
			t.Fatalf("safeTarget(%q) = true, want false", host)
		}
	}
}

func TestCreateLeaseDoesNotPersistResolvedIP(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemoryStore()
	if _, err := store.UpsertProxy(ctx, domain.Proxy{ID: "proxy-a", Endpoint: "http://proxy.local:8080", Healthy: true, Weight: 1}); err != nil {
		t.Fatalf("UpsertProxy() error = %v", err)
	}
	svc := NewService(store, "http://gateway.local")
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }
	svc.storeHostSafety("example.com", true)

	created, err := svc.CreateLease(ctx, domain.Principal{TenantID: "tenant-a"}, "idem-domain", CreateLeaseRequest{
		Subject:     domain.Subject{Type: "user", ID: "user-a"},
		ResourceRef: domain.ResourceRef{Kind: "url", ID: "https://example.com/resource"},
	})
	if err != nil {
		t.Fatalf("CreateLease() error = %v", err)
	}
	if created.ResourceRef.ID != "https://example.com/resource" {
		t.Fatalf("created.ResourceRef.ID = %q, want hostname URL preserved", created.ResourceRef.ID)
	}
}

func TestValidateLeaseSkipsStoreOnFreshTruth(t *testing.T) {
	ctx := context.Background()
	mem := storage.NewMemoryStore()
	if _, err := mem.UpsertProxy(ctx, domain.Proxy{ID: "proxy-a", Endpoint: "http://proxy.local:8080", Healthy: true, Weight: 1}); err != nil {
		t.Fatalf("UpsertProxy() error = %v", err)
	}
	counter := &countingStore{Store: mem}
	cc := &captureCache{}
	svc := NewService(counter, "http://gateway.local")
	svc.SetCache(cc, time.Minute)
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }
	svc.storeHostSafety("example.com", true)

	created, err := svc.CreateLease(ctx, domain.Principal{TenantID: "tenant-a"}, "idem-truth", CreateLeaseRequest{
		Subject:     domain.Subject{Type: "user", ID: "user-a"},
		ResourceRef: domain.ResourceRef{Kind: "url", ID: "https://example.com/resource"},
	})
	if err != nil {
		t.Fatalf("CreateLease() error = %v", err)
	}
	password := created.Password

	// First validate: cache empty, must call store and stamp truth.
	if _, err := svc.ValidateLease(ctx, created.TenantID, created.ID, password, "https://example.com/resource"); err != nil {
		t.Fatalf("first ValidateLease error = %v", err)
	}
	if got := counter.getLeaseCalls.Load(); got != 1 {
		t.Fatalf("after first validate getLease=%d, want 1", got)
	}

	// Second validate: cache hit + fresh truth stamp matching fingerprint
	// must skip store entirely.
	if _, err := svc.ValidateLease(ctx, created.TenantID, created.ID, password, "https://example.com/resource"); err != nil {
		t.Fatalf("second ValidateLease error = %v", err)
	}
	if got := counter.getLeaseCalls.Load(); got != 1 {
		t.Fatalf("after second validate getLease=%d, want still 1 (truth fast path)", got)
	}

	// Advance past truth TTL: next validate must recheck store.
	svc.now = func() time.Time { return now.Add(2 * svc.validateTruthTTL) }
	if _, err := svc.ValidateLease(ctx, created.TenantID, created.ID, password, "https://example.com/resource"); err != nil {
		t.Fatalf("third ValidateLease error = %v", err)
	}
	if got := counter.getLeaseCalls.Load(); got != 2 {
		t.Fatalf("after expired truth getLease=%d, want 2", got)
	}
}

func TestValidateLeaseFirstCacheHitWithoutTruthStillFetchesStore(t *testing.T) {
	ctx := context.Background()
	mem := storage.NewMemoryStore()
	if _, err := mem.UpsertProxy(ctx, domain.Proxy{ID: "proxy-a", Endpoint: "http://proxy.local:8080", Healthy: true, Weight: 1}); err != nil {
		t.Fatalf("UpsertProxy() error = %v", err)
	}
	counter := &countingStore{Store: mem}
	cc := &captureCache{}
	svc := NewService(counter, "http://gateway.local")
	svc.SetCache(cc, time.Minute)
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }
	svc.storeHostSafety("example.com", true)

	created, err := svc.CreateLease(ctx, domain.Principal{TenantID: "tenant-a"}, "idem-firsthit", CreateLeaseRequest{
		Subject:     domain.Subject{Type: "user", ID: "user-a"},
		ResourceRef: domain.ResourceRef{Kind: "url", ID: "https://example.com/resource"},
	})
	if err != nil {
		t.Fatalf("CreateLease() error = %v", err)
	}
	// Pre-populate the distributed cache with the lease but do NOT seed any
	// local truth stamp. The first validate after a process restart must
	// still fall through to the store.
	cached := created
	cached.Password = ""
	cc.mu.Lock()
	cc.lease = cached
	cc.hit = true
	cc.mu.Unlock()

	if _, err := svc.ValidateLease(ctx, created.TenantID, created.ID, created.Password, "https://example.com/resource"); err != nil {
		t.Fatalf("ValidateLease error = %v", err)
	}
	if got := counter.getLeaseCalls.Load(); got != 1 {
		t.Fatalf("getLease=%d, want 1 (cache hit without truth must still fetch store)", got)
	}
}

func TestRenewLeaseInvalidatesValidateTruth(t *testing.T) {
	ctx := context.Background()
	mem := storage.NewMemoryStore()
	if _, err := mem.UpsertProxy(ctx, domain.Proxy{ID: "proxy-a", Endpoint: "http://proxy.local:8080", Healthy: true, Weight: 1}); err != nil {
		t.Fatalf("UpsertProxy() error = %v", err)
	}
	counter := &countingStore{Store: mem}
	cc := &captureCache{}
	svc := NewService(counter, "http://gateway.local")
	svc.SetCache(cc, time.Minute)
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }
	svc.storeHostSafety("example.com", true)

	principal := domain.Principal{TenantID: "tenant-a"}
	created, err := svc.CreateLease(ctx, principal, "idem-renew-truth", CreateLeaseRequest{
		Subject:     domain.Subject{Type: "user", ID: "user-a"},
		ResourceRef: domain.ResourceRef{Kind: "url", ID: "https://example.com/resource"},
	})
	if err != nil {
		t.Fatalf("CreateLease() error = %v", err)
	}
	password := created.Password
	if _, err := svc.ValidateLease(ctx, created.TenantID, created.ID, password, "https://example.com/resource"); err != nil {
		t.Fatalf("priming ValidateLease error = %v", err)
	}
	beforeRenew := counter.getLeaseCalls.Load()

	svc.now = func() time.Time { return now.Add(time.Minute) }
	if _, err := svc.RenewLease(ctx, principal, created.ID); err != nil {
		t.Fatalf("RenewLease error = %v", err)
	}

	// After renew, the local truth must be invalidated so the next validate
	// fetches store again to see the new generation.
	if _, err := svc.ValidateLease(ctx, created.TenantID, created.ID, password, "https://example.com/resource"); err != nil {
		t.Fatalf("post-renew ValidateLease error = %v", err)
	}
	if counter.getLeaseCalls.Load() <= beforeRenew {
		t.Fatalf("getLease did not advance after renew (was %d)", counter.getLeaseCalls.Load())
	}
}
