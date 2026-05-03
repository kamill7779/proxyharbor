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

type stubResolver struct {
	calls atomic.Int64
	mu    sync.Mutex
	ips   map[string][]net.IP
	errs  map[string]error
}

type blockingResolver struct {
	calls   atomic.Int64
	release chan struct{}
	once    sync.Once
}

func newStubResolver() *stubResolver {
	return &stubResolver{
		ips: map[string][]net.IP{},
		errs: map[string]error{},
	}
}

func newBlockingResolver() *blockingResolver {
	return &blockingResolver{release: make(chan struct{})}
}

func (r *stubResolver) LookupIP(_ context.Context, _, host string) ([]net.IP, error) {
	r.calls.Add(1)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err, ok := r.errs[host]; ok {
		return nil, err
	}
	ips, ok := r.ips[host]
	if !ok {
		return nil, errors.New("unexpected host lookup")
	}
	out := make([]net.IP, len(ips))
	for i, ip := range ips {
		out[i] = append(net.IP(nil), ip...)
	}
	return out, nil
}

func (r *blockingResolver) LookupIP(_ context.Context, _, _ string) ([]net.IP, error) {
	r.calls.Add(1)
	<-r.release
	return []net.IP{net.ParseIP("1.1.1.1")}, nil
}

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
	leaseVer atomic.Uint64
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
	c.leaseVer.Add(1)
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
func (c *captureCache) LeaseInvalidationVersion() uint64                                { return c.leaseVer.Load() }
func (c *captureCache) bumpLeaseInvalidationVersion()                                   { c.leaseVer.Add(1) }

func TestSafeTargetDoesNotTrustCachedSafeDecision(t *testing.T) {
	store := storage.NewMemoryStore()
	svc := NewService(store, "http://gateway.local")
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }
	resolver := newStubResolver()
	resolver.errs["example.com"] = errors.New("dns disabled in test")
	svc.resolver = resolver

	svc.storeHostSafety("example.com", true)
	if svc.safeTarget(context.Background(), "https://example.com/resource") {
		t.Fatal("safeTarget = true, want false after re-resolving cached safe host")
	}
	if got := resolver.calls.Load(); got != 1 {
		t.Fatalf("resolver lookups = %d, want 1 for cached safe recheck", got)
	}
}

func TestSafeTargetCachesUnsafeDecisionWithinTTL(t *testing.T) {
	store := storage.NewMemoryStore()
	svc := NewService(store, "http://gateway.local")
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }
	resolver := newStubResolver()
	resolver.ips["example.com"] = []net.IP{net.ParseIP("1.1.1.1")}
	svc.resolver = resolver
	svc.storeHostSafety("example.com", false)

	if svc.safeTarget(context.Background(), "https://example.com/resource") {
		t.Fatal("safeTarget = true, want cached unsafe decision to reject")
	}
	if got := resolver.calls.Load(); got != 0 {
		t.Fatalf("resolver lookups = %d, want 0 for cached unsafe decision", got)
	}
}

func TestSafeTargetResolverErrorsAreNotCached(t *testing.T) {
	store := storage.NewMemoryStore()
	svc := NewService(store, "http://gateway.local")
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }
	resolver := newStubResolver()
	resolver.errs["example.com"] = errors.New("temporary dns failure")
	svc.resolver = resolver

	if svc.safeTarget(context.Background(), "https://example.com/resource") {
		t.Fatal("first safeTarget = true, want false on resolver error")
	}
	resolver.mu.Lock()
	delete(resolver.errs, "example.com")
	resolver.ips["example.com"] = []net.IP{net.ParseIP("1.1.1.1")}
	resolver.mu.Unlock()
	if !svc.safeTarget(context.Background(), "https://example.com/resource") {
		t.Fatal("second safeTarget = false, want true after resolver recovery")
	}
	if got := resolver.calls.Load(); got != 2 {
		t.Fatalf("resolver lookups = %d, want 2 because resolver failures are not cached", got)
	}
}

func TestSafeTargetCoalescesConcurrentSafeLookups(t *testing.T) {
	store := storage.NewMemoryStore()
	svc := NewService(store, "http://gateway.local")
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }
	resolver := newBlockingResolver()
	svc.resolver = resolver

	const workers = 8
	results := make(chan bool, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			results <- svc.safeTarget(context.Background(), "https://example.com/resource")
		}()
	}

	for resolver.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	close(resolver.release)
	wg.Wait()
	close(results)

	if got := resolver.calls.Load(); got != 1 {
		t.Fatalf("resolver lookups = %d, want 1 for concurrent safe lookups", got)
	}
	for result := range results {
		if !result {
			t.Fatal("safeTarget = false, want true for coalesced safe lookup")
		}
	}
}

func TestSafeTargetPublicIPSkipsResolver(t *testing.T) {
	store := storage.NewMemoryStore()
	svc := NewService(store, "http://gateway.local")
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }
	resolver := newStubResolver()
	svc.resolver = resolver

	if !svc.safeTarget(context.Background(), "https://1.1.1.1/resource") {
		t.Fatal("safeTarget for public IP = false, want true")
	}
	if got := resolver.calls.Load(); got != 0 {
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
		if svc.safeTarget(context.Background(), host) {
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
	resolver := newStubResolver()
	resolver.ips["example.com"] = []net.IP{net.ParseIP("1.1.1.1")}
	svc.resolver = resolver

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
	resolver := newStubResolver()
	resolver.ips["example.com"] = []net.IP{net.ParseIP("1.1.1.1")}
	svc.resolver = resolver

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
	resolver := newStubResolver()
	resolver.ips["example.com"] = []net.IP{net.ParseIP("1.1.1.1")}
	svc.resolver = resolver

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
	resolver := newStubResolver()
	resolver.ips["example.com"] = []net.IP{net.ParseIP("1.1.1.1")}
	svc.resolver = resolver

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

func TestValidateLeaseAuthFailureAfterStoreReadKeepsTruthStamp(t *testing.T) {
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
	resolver := newStubResolver()
	resolver.ips["example.com"] = []net.IP{net.ParseIP("1.1.1.1")}
	svc.resolver = resolver

	created, err := svc.CreateLease(ctx, domain.Principal{TenantID: "tenant-a"}, "idem-auth-failure", CreateLeaseRequest{
		Subject:     domain.Subject{Type: "user", ID: "user-a"},
		ResourceRef: domain.ResourceRef{Kind: "url", ID: "https://example.com/resource"},
	})
	if err != nil {
		t.Fatalf("CreateLease() error = %v", err)
	}

	if _, err := svc.ValidateLease(ctx, created.TenantID, created.ID, created.Password, "https://example.com/resource"); err != nil {
		t.Fatalf("priming ValidateLease error = %v", err)
	}
	if got := counter.getLeaseCalls.Load(); got != 1 {
		t.Fatalf("after priming getLease=%d, want 1", got)
	}

	svc.now = func() time.Time { return now.Add(2 * svc.validateTruthTTL) }
	if _, err := svc.ValidateLease(ctx, created.TenantID, created.ID, "wrong-password", "https://example.com/resource"); !errors.Is(err, domain.ErrAuthFailed) {
		t.Fatalf("ValidateLease wrong password error = %v, want ErrAuthFailed", err)
	}
	if got := counter.getLeaseCalls.Load(); got != 2 {
		t.Fatalf("after wrong-password getLease=%d, want 2", got)
	}

	svc.now = func() time.Time { return now.Add(2*svc.validateTruthTTL + 50*time.Millisecond) }
	if _, err := svc.ValidateLease(ctx, created.TenantID, created.ID, created.Password, "https://example.com/resource"); err != nil {
		t.Fatalf("post-auth-failure ValidateLease error = %v", err)
	}
	if got := counter.getLeaseCalls.Load(); got != 2 {
		t.Fatalf("after good password getLease=%d, want still 2 because auth failure kept truth stamp", got)
	}
}

func TestValidateLeaseRemoteMutationVersionInvalidatesFreshTruth(t *testing.T) {
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
	resolver := newStubResolver()
	resolver.ips["example.com"] = []net.IP{net.ParseIP("1.1.1.1")}
	svc.resolver = resolver

	created, err := svc.CreateLease(ctx, domain.Principal{TenantID: "tenant-a"}, "idem-remote-revoke", CreateLeaseRequest{
		Subject:     domain.Subject{Type: "user", ID: "user-a"},
		ResourceRef: domain.ResourceRef{Kind: "url", ID: "https://example.com/resource"},
	})
	if err != nil {
		t.Fatalf("CreateLease() error = %v", err)
	}

	if _, err := svc.ValidateLease(ctx, created.TenantID, created.ID, created.Password, "https://example.com/resource"); err != nil {
		t.Fatalf("priming ValidateLease error = %v", err)
	}
	if got := counter.getLeaseCalls.Load(); got != 1 {
		t.Fatalf("after priming getLease=%d, want 1", got)
	}

	if err := mem.RevokeLease(ctx, created.TenantID, created.ID); err != nil {
		t.Fatalf("remote RevokeLease() error = %v", err)
	}
	cc.bumpLeaseInvalidationVersion()

	if _, err := svc.ValidateLease(ctx, created.TenantID, created.ID, created.Password, "https://example.com/resource"); !errors.Is(err, domain.ErrLeaseRevoked) {
		t.Fatalf("post-remote-mutation ValidateLease error = %v, want ErrLeaseRevoked", err)
	}
	if got := counter.getLeaseCalls.Load(); got != 2 {
		t.Fatalf("after remote mutation getLease=%d, want 2 because invalidation version must bypass stale truth", got)
	}
}
