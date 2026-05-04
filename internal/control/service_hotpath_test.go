package control

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	cachepkg "github.com/kamill7779/proxyharbor/internal/cache"
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

type cancelFirstResolver struct {
	calls    atomic.Int64
	started  chan int
	release  chan struct{}
	fallback chan struct{}
}

func newStubResolver() *stubResolver {
	return &stubResolver{
		ips:  map[string][]net.IP{},
		errs: map[string]error{},
	}
}

func newBlockingResolver() *blockingResolver {
	return &blockingResolver{release: make(chan struct{})}
}

func newCancelFirstResolver() *cancelFirstResolver {
	return &cancelFirstResolver{
		started:  make(chan int, 4),
		release:  make(chan struct{}),
		fallback: make(chan struct{}),
	}
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

func (r *cancelFirstResolver) LookupIP(ctx context.Context, _, _ string) ([]net.IP, error) {
	call := int(r.calls.Add(1))
	r.started <- call
	if call == 1 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-r.fallback:
			return nil, errors.New("forced release")
		}
	}
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

type countingCreatePrereqStore struct {
	*storage.MemoryStore
	getPolicyCalls           atomic.Int64
	listSelectableProxyCalls atomic.Int64
}

func (s *countingCreatePrereqStore) GetPolicy(ctx context.Context, id string) (domain.Policy, error) {
	s.getPolicyCalls.Add(1)
	return s.MemoryStore.GetPolicy(ctx, id)
}

func (s *countingCreatePrereqStore) ListSelectableProxies(ctx context.Context) ([]domain.Proxy, error) {
	s.listSelectableProxyCalls.Add(1)
	return s.MemoryStore.ListSelectableProxies(ctx)
}

type blockingCreatePrereqStore struct {
	*storage.MemoryStore
	policyStarted  chan struct{}
	policyRelease  chan struct{}
	policyCalls    atomic.Int64
	policySnapshot domain.Policy

	proxiesStarted  chan struct{}
	proxiesRelease  chan struct{}
	proxiesCalls    atomic.Int64
	proxiesSnapshot []domain.Proxy
}

func (s *blockingCreatePrereqStore) GetPolicy(context.Context, string) (domain.Policy, error) {
	s.policyCalls.Add(1)
	if s.policyStarted != nil {
		select {
		case s.policyStarted <- struct{}{}:
		default:
		}
	}
	policy := s.policySnapshot
	if s.policyRelease != nil {
		<-s.policyRelease
	}
	return policy, nil
}

func (s *blockingCreatePrereqStore) ListSelectableProxies(context.Context) ([]domain.Proxy, error) {
	s.proxiesCalls.Add(1)
	if s.proxiesStarted != nil {
		select {
		case s.proxiesStarted <- struct{}{}:
		default:
		}
	}
	proxies := append([]domain.Proxy(nil), s.proxiesSnapshot...)
	if s.proxiesRelease != nil {
		<-s.proxiesRelease
	}
	return proxies, nil
}

// captureCache wraps the cacheStub but records the most recently stored lease
// so tests can simulate stale cache states across calls.
type captureCache struct {
	mu       sync.Mutex
	lease    domain.Lease
	hit      bool
	gets     atomic.Int64
	puts     atomic.Int64
	invalid  atomic.Int64
	leaseVer atomic.Uint64
	afterPut func()
	truth    string
	truthHit bool
	truthExp time.Time
	now      func() time.Time
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
	if c.afterPut != nil {
		c.afterPut()
	}
	return nil
}

func (c *captureCache) GetValidateTruth(_ context.Context, tenantID, leaseID string) (string, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.truthHit && !c.truthExp.IsZero() && !c.truthExp.After(c.nowTime()) {
		c.truth = ""
		c.truthHit = false
		c.truthExp = time.Time{}
	}
	return c.truth, c.truthHit, nil
}

func (c *captureCache) PutValidateTruth(_ context.Context, tenantID, leaseID, fingerprint string, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.truth = fingerprint
	c.truthHit = true
	if ttl > 0 {
		c.truthExp = c.nowTime().Add(ttl)
	} else {
		c.truthExp = time.Time{}
	}
	return nil
}

func (c *captureCache) InvalidateValidateTruth(_ context.Context, tenantID, leaseID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.truth = ""
	c.truthHit = false
	c.truthExp = time.Time{}
	return nil
}

func (c *captureCache) InvalidateLease(_ context.Context, _ string, _ string) error {
	c.invalid.Add(1)
	c.leaseVer.Add(1)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hit = false
	c.lease = domain.Lease{}
	c.truth = ""
	c.truthHit = false
	c.truthExp = time.Time{}
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

func (c *captureCache) nowTime() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now().UTC()
}

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

func TestSafeTargetSequentialSafeLookupsRecheckResolver(t *testing.T) {
	store := storage.NewMemoryStore()
	svc := NewService(store, "http://gateway.local")
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }
	resolver := newStubResolver()
	resolver.ips["example.com"] = []net.IP{net.ParseIP("1.1.1.1")}
	svc.resolver = resolver

	if !svc.safeTarget(context.Background(), "https://example.com/resource") {
		t.Fatal("first safeTarget = false, want true")
	}
	if !svc.safeTarget(context.Background(), "https://example.com/resource") {
		t.Fatal("second safeTarget = false, want true")
	}
	if got := resolver.calls.Load(); got != 2 {
		t.Fatalf("resolver lookups = %d, want 2 for sequential safe rechecks", got)
	}
}

func TestSafeTargetFollowerRetriesAfterCanceledOwner(t *testing.T) {
	store := storage.NewMemoryStore()
	svc := NewService(store, "http://gateway.local")
	resolver := newCancelFirstResolver()
	svc.resolver = resolver

	ownerCtx, cancelOwner := context.WithCancel(context.Background())
	defer cancelOwner()
	ownerDone := make(chan bool, 1)
	go func() {
		ownerDone <- svc.safeTarget(ownerCtx, "https://example.com/resource")
	}()

	select {
	case call := <-resolver.started:
		if call != 1 {
			t.Fatalf("first resolver call = %d, want 1", call)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for owner resolver call")
	}

	followerDone := make(chan bool, 1)
	go func() {
		followerDone <- svc.safeTarget(context.Background(), "https://example.com/resource")
	}()

	cancelOwner()

	select {
	case result := <-ownerDone:
		if result {
			t.Fatal("canceled owner safeTarget = true, want false")
		}
	case <-time.After(200 * time.Millisecond):
		close(resolver.fallback)
		t.Fatal("owner lookup did not unblock after caller cancellation")
	}

	select {
	case call := <-resolver.started:
		if call != 2 {
			t.Fatalf("second resolver call = %d, want 2 after canceled owner retry", call)
		}
	case <-time.After(time.Second):
		close(resolver.fallback)
		t.Fatal("follower did not retry lookup after canceled owner")
	}

	close(resolver.release)

	select {
	case result := <-followerDone:
		if !result {
			t.Fatal("follower safeTarget = false, want true after retry")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for follower result")
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

func TestSafeTargetNilContextDoesNotPanic(t *testing.T) {
	store := storage.NewMemoryStore()
	svc := NewService(store, "http://gateway.local")
	resolver := newStubResolver()
	resolver.ips["example.com"] = []net.IP{net.ParseIP("1.1.1.1")}
	svc.resolver = resolver

	if !svc.safeTarget(nil, "https://example.com/resource") {
		t.Fatal("safeTarget(nil, hostname) = false, want true")
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
		"http://198.18.0.1/resource",
		"http://240.0.0.1/resource",
		"http://255.255.255.255/resource",
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

func TestCreateLeaseReusesFreshPolicyAndSelectableProxyReads(t *testing.T) {
	ctx := context.Background()
	store := &countingCreatePrereqStore{MemoryStore: storage.NewMemoryStore()}
	if _, err := store.UpsertProxy(ctx, domain.Proxy{ID: "proxy-a", Endpoint: "http://proxy.local:8080", Healthy: true, Weight: 1}); err != nil {
		t.Fatalf("UpsertProxy() error = %v", err)
	}
	svc := NewService(store, "http://gateway.local")
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }
	resolver := newStubResolver()
	resolver.ips["example.com"] = []net.IP{net.ParseIP("1.1.1.1")}
	svc.resolver = resolver

	create := func(idem string) {
		t.Helper()
		if _, err := svc.CreateLease(ctx, domain.Principal{TenantID: "tenant-a"}, idem, CreateLeaseRequest{
			Subject:     domain.Subject{Type: "user", ID: idem},
			ResourceRef: domain.ResourceRef{Kind: "url", ID: "https://example.com/resource"},
		}); err != nil {
			t.Fatalf("CreateLease(%q) error = %v", idem, err)
		}
	}

	create("idem-hot-1")
	create("idem-hot-2")

	if got := store.getPolicyCalls.Load(); got != 1 {
		t.Fatalf("GetPolicy() calls = %d, want 1 within hot TTL", got)
	}
	if got := store.listSelectableProxyCalls.Load(); got != 1 {
		t.Fatalf("ListSelectableProxies() calls = %d, want 1 within hot TTL", got)
	}

	now = now.Add(51 * time.Millisecond)
	create("idem-hot-3")

	if got := store.getPolicyCalls.Load(); got != 2 {
		t.Fatalf("GetPolicy() calls after TTL = %d, want 2", got)
	}
	if got := store.listSelectableProxyCalls.Load(); got != 2 {
		t.Fatalf("ListSelectableProxies() calls after TTL = %d, want 2", got)
	}
}

func TestDefaultPolicyForCreateDoesNotRepopulateStaleEntryAfterInvalidation(t *testing.T) {
	store := &blockingCreatePrereqStore{
		MemoryStore:    storage.NewMemoryStore(),
		policyStarted:  make(chan struct{}, 1),
		policyRelease:  make(chan struct{}),
		policySnapshot: domain.Policy{ID: "default", Enabled: true, TTLSeconds: 300, Version: 1},
	}
	svc := NewService(store, "http://gateway.local")
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }

	type result struct {
		policy domain.Policy
		err    error
	}
	firstDone := make(chan result, 1)
	go func() {
		policy, err := svc.defaultPolicyForCreate(context.Background())
		firstDone <- result{policy: policy, err: err}
	}()

	select {
	case <-store.policyStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first policy lookup")
	}

	svc.clearDefaultPolicy()
	store.policySnapshot = domain.Policy{ID: "default", Enabled: true, TTLSeconds: 600, Version: 2}
	close(store.policyRelease)

	first := <-firstDone
	if first.err != nil {
		t.Fatalf("first defaultPolicyForCreate() error = %v", first.err)
	}
	if first.policy.Version != 1 {
		t.Fatalf("first policy version = %d, want 1", first.policy.Version)
	}

	second, err := svc.defaultPolicyForCreate(context.Background())
	if err != nil {
		t.Fatalf("second defaultPolicyForCreate() error = %v", err)
	}
	if second.Version != 2 {
		t.Fatalf("second policy version = %d, want 2 after invalidation", second.Version)
	}
	if got := store.policyCalls.Load(); got != 2 {
		t.Fatalf("GetPolicy() calls = %d, want 2", got)
	}
}

func TestListSelectableProxiesDoesNotRepopulateStaleEntryAfterInvalidation(t *testing.T) {
	store := &blockingCreatePrereqStore{
		MemoryStore:    storage.NewMemoryStore(),
		proxiesStarted: make(chan struct{}, 1),
		proxiesRelease: make(chan struct{}),
		proxiesSnapshot: []domain.Proxy{
			{ID: "proxy-v1", Healthy: true, Weight: 1},
		},
	}
	svc := NewService(store, "http://gateway.local")
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }

	type result struct {
		proxies []domain.Proxy
		err     error
	}
	firstDone := make(chan result, 1)
	go func() {
		proxies, err := svc.listSelectableProxies(context.Background())
		firstDone <- result{proxies: proxies, err: err}
	}()

	select {
	case <-store.proxiesStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first proxy lookup")
	}

	svc.clearSelectableProxies()
	store.proxiesSnapshot = []domain.Proxy{{ID: "proxy-v2", Healthy: true, Weight: 1}}
	close(store.proxiesRelease)

	first := <-firstDone
	if first.err != nil {
		t.Fatalf("first listSelectableProxies() error = %v", first.err)
	}
	if len(first.proxies) != 1 || first.proxies[0].ID != "proxy-v1" {
		t.Fatalf("first proxies = %#v, want proxy-v1 snapshot", first.proxies)
	}

	second, err := svc.listSelectableProxies(context.Background())
	if err != nil {
		t.Fatalf("second listSelectableProxies() error = %v", err)
	}
	if len(second) != 1 || second[0].ID != "proxy-v2" {
		t.Fatalf("second proxies = %#v, want proxy-v2 after invalidation", second)
	}
	if got := store.proxiesCalls.Load(); got != 2 {
		t.Fatalf("ListSelectableProxies() calls = %d, want 2", got)
	}
}

func TestCreateLeaseSeedsValidateTruthFastPath(t *testing.T) {
	ctx := context.Background()
	mem := storage.NewMemoryStore()
	if _, err := mem.UpsertProxy(ctx, domain.Proxy{ID: "proxy-a", Endpoint: "http://proxy.local:8080", Healthy: true, Weight: 1}); err != nil {
		t.Fatalf("UpsertProxy() error = %v", err)
	}
	counter := &countingStore{Store: mem}
	cc := &captureCache{}
	svc := NewService(counter, "http://gateway.local")
	svc.SetCache(cc, time.Minute)
	cc.now = func() time.Time { return svc.now() }
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

	if got := cc.puts.Load(); got != 1 {
		t.Fatalf("after CreateLease cache puts=%d, want 1", got)
	}

	// First validate: the creating process already knows store truth, so the
	// hot cache and local truth stamp should skip the initial store recheck.
	if _, err := svc.ValidateLease(ctx, created.TenantID, created.ID, password, "https://example.com/resource"); err != nil {
		t.Fatalf("first ValidateLease error = %v", err)
	}
	if got := counter.getLeaseCalls.Load(); got != 0 {
		t.Fatalf("after first validate getLease=%d, want 0 because CreateLease seeded truth", got)
	}
	if got := cc.gets.Load(); got != 0 {
		t.Fatalf("after first validate cache gets=%d, want 0 because fresh local truth should bypass Redis", got)
	}

	// Second validate: cache hit + fresh truth stamp matching fingerprint
	// must skip store entirely.
	if _, err := svc.ValidateLease(ctx, created.TenantID, created.ID, password, "https://example.com/resource"); err != nil {
		t.Fatalf("second ValidateLease error = %v", err)
	}
	if got := counter.getLeaseCalls.Load(); got != 0 {
		t.Fatalf("after second validate getLease=%d, want still 0 (truth fast path)", got)
	}
	if got := cc.gets.Load(); got != 0 {
		t.Fatalf("after second validate cache gets=%d, want still 0 while local truth is fresh", got)
	}

	// Advance past truth TTL: next validate must recheck store.
	svc.now = func() time.Time { return now.Add(2 * svc.validateTruthTTL) }
	if _, err := svc.ValidateLease(ctx, created.TenantID, created.ID, password, "https://example.com/resource"); err != nil {
		t.Fatalf("third ValidateLease error = %v", err)
	}
	if got := counter.getLeaseCalls.Load(); got != 1 {
		t.Fatalf("after expired truth getLease=%d, want 1", got)
	}
}

func TestValidateLeaseLocalTruthRequiresSharedTruthPresence(t *testing.T) {
	ctx := context.Background()
	server := miniredis.RunT(t)
	cacheA, err := cachepkg.NewRedis(ctx, server.Addr(), "", 0)
	if err != nil {
		t.Fatalf("NewRedis(cacheA) error = %v", err)
	}
	cacheB, err := cachepkg.NewRedis(ctx, server.Addr(), "", 0)
	if err != nil {
		t.Fatalf("NewRedis(cacheB) error = %v", err)
	}
	t.Cleanup(func() {
		_ = cacheA.Close()
		_ = cacheB.Close()
	})

	mem := storage.NewMemoryStore()
	if _, err := mem.UpsertProxy(ctx, domain.Proxy{ID: "proxy-a", Endpoint: "http://proxy.local:8080", Healthy: true, Weight: 1}); err != nil {
		t.Fatalf("UpsertProxy() error = %v", err)
	}
	principal := domain.Principal{TenantID: "tenant-a"}

	svcA := NewService(mem, "http://gateway.local")
	svcA.SetCache(cacheA, time.Minute)
	svcB := NewService(mem, "http://gateway.local")
	svcB.SetCache(cacheB, time.Minute)

	resolver := newStubResolver()
	resolver.ips["example.com"] = []net.IP{net.ParseIP("1.1.1.1")}
	svcA.resolver = resolver
	svcB.resolver = resolver

	created, err := svcA.CreateLease(ctx, principal, "idem-shared-presence", CreateLeaseRequest{
		Subject:     domain.Subject{Type: "user", ID: "user-a"},
		ResourceRef: domain.ResourceRef{Kind: "url", ID: "https://example.com/resource"},
	})
	if err != nil {
		t.Fatalf("CreateLease() error = %v", err)
	}
	if _, err := svcB.ValidateLease(ctx, created.TenantID, created.ID, created.Password, "https://example.com/resource"); err != nil {
		t.Fatalf("priming ValidateLease() error = %v", err)
	}

	if err := svcA.RevokeLease(ctx, principal, created.ID); err != nil {
		t.Fatalf("RevokeLease() error = %v", err)
	}
	if _, err := svcB.ValidateLease(ctx, created.TenantID, created.ID, created.Password, "https://example.com/resource"); !errors.Is(err, domain.ErrLeaseRevoked) {
		t.Fatalf("post-revoke ValidateLease() error = %v, want ErrLeaseRevoked", err)
	}
}

func TestValidateLeaseUsesSharedTruthWithoutLocalStamp(t *testing.T) {
	ctx := context.Background()
	mem := storage.NewMemoryStore()
	if _, err := mem.UpsertProxy(ctx, domain.Proxy{ID: "proxy-a", Endpoint: "http://proxy.local:8080", Healthy: true, Weight: 1}); err != nil {
		t.Fatalf("UpsertProxy() error = %v", err)
	}
	counter := &countingStore{Store: mem}
	cc := &captureCache{}
	svc := NewService(counter, "http://gateway.local")
	svc.SetCache(cc, time.Minute)
	cc.now = func() time.Time { return svc.now() }
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }
	resolver := newStubResolver()
	resolver.ips["example.com"] = []net.IP{net.ParseIP("1.1.1.1")}
	svc.resolver = resolver

	created, err := svc.CreateLease(ctx, domain.Principal{TenantID: "tenant-a"}, "idem-shared-truth", CreateLeaseRequest{
		Subject:     domain.Subject{Type: "user", ID: "user-a"},
		ResourceRef: domain.ResourceRef{Kind: "url", ID: "https://example.com/resource"},
	})
	if err != nil {
		t.Fatalf("CreateLease() error = %v", err)
	}
	svc.invalidateValidateTruth(created.TenantID, created.ID)

	if _, err := svc.ValidateLease(ctx, created.TenantID, created.ID, created.Password, "https://example.com/resource"); err != nil {
		t.Fatalf("ValidateLease error = %v", err)
	}
	if got := counter.getLeaseCalls.Load(); got != 0 {
		t.Fatalf("after shared truth validate getLease=%d, want 0", got)
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
	cc.now = func() time.Time { return svc.now() }
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
	svc.clearValidateTruth(ctx, created.TenantID, created.ID)
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
	cc.now = func() time.Time { return svc.now() }
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
	cc.now = func() time.Time { return svc.now() }
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
	svc.clearValidateTruth(ctx, created.TenantID, created.ID)

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

func TestValidateLeaseRemoteMutationSharedTruthInvalidatesFreshTruth(t *testing.T) {
	ctx := context.Background()
	mem := storage.NewMemoryStore()
	if _, err := mem.UpsertProxy(ctx, domain.Proxy{ID: "proxy-a", Endpoint: "http://proxy.local:8080", Healthy: true, Weight: 1}); err != nil {
		t.Fatalf("UpsertProxy() error = %v", err)
	}
	counter := &countingStore{Store: mem}
	cc := &captureCache{}
	svc := NewService(counter, "http://gateway.local")
	svc.SetCache(cc, time.Minute)
	cc.now = func() time.Time { return svc.now() }
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
	svc.clearValidateTruth(ctx, created.TenantID, created.ID)

	if _, err := svc.ValidateLease(ctx, created.TenantID, created.ID, created.Password, "https://example.com/resource"); err != nil {
		t.Fatalf("priming ValidateLease error = %v", err)
	}
	if got := counter.getLeaseCalls.Load(); got != 1 {
		t.Fatalf("after priming getLease=%d, want 1", got)
	}

	if err := mem.RevokeLease(ctx, created.TenantID, created.ID); err != nil {
		t.Fatalf("remote RevokeLease() error = %v", err)
	}
	if err := cc.InvalidateValidateTruth(ctx, created.TenantID, created.ID); err != nil {
		t.Fatalf("InvalidateValidateTruth() error = %v", err)
	}

	if _, err := svc.ValidateLease(ctx, created.TenantID, created.ID, created.Password, "https://example.com/resource"); !errors.Is(err, domain.ErrLeaseRevoked) {
		t.Fatalf("post-remote-mutation ValidateLease error = %v, want ErrLeaseRevoked", err)
	}
	if got := counter.getLeaseCalls.Load(); got != 2 {
		t.Fatalf("after remote mutation getLease=%d, want 2 because missing shared truth must bypass stale truth", got)
	}
}

func TestValidateLeaseDoesNotStampTruthAcrossVersionRace(t *testing.T) {
	ctx := context.Background()
	mem := storage.NewMemoryStore()
	if _, err := mem.UpsertProxy(ctx, domain.Proxy{ID: "proxy-a", Endpoint: "http://proxy.local:8080", Healthy: true, Weight: 1}); err != nil {
		t.Fatalf("UpsertProxy() error = %v", err)
	}
	counter := &countingStore{Store: mem}
	cc := &captureCache{}
	svc := NewService(counter, "http://gateway.local")
	svc.SetCache(cc, time.Minute)
	cc.now = func() time.Time { return svc.now() }
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }
	resolver := newStubResolver()
	resolver.ips["example.com"] = []net.IP{net.ParseIP("1.1.1.1")}
	svc.resolver = resolver

	created, err := svc.CreateLease(ctx, domain.Principal{TenantID: "tenant-a"}, "idem-version-race", CreateLeaseRequest{
		Subject:     domain.Subject{Type: "user", ID: "user-a"},
		ResourceRef: domain.ResourceRef{Kind: "url", ID: "https://example.com/resource"},
	})
	if err != nil {
		t.Fatalf("CreateLease() error = %v", err)
	}
	svc.clearValidateTruth(ctx, created.TenantID, created.ID)
	cc.afterPut = cc.bumpLeaseInvalidationVersion

	if _, err := svc.ValidateLease(ctx, created.TenantID, created.ID, created.Password, "https://example.com/resource"); err != nil {
		t.Fatalf("first ValidateLease error = %v", err)
	}
	if got := counter.getLeaseCalls.Load(); got != 1 {
		t.Fatalf("after first validate getLease=%d, want 1", got)
	}

	if _, err := svc.ValidateLease(ctx, created.TenantID, created.ID, created.Password, "https://example.com/resource"); err != nil {
		t.Fatalf("second ValidateLease error = %v", err)
	}
	if got := counter.getLeaseCalls.Load(); got != 2 {
		t.Fatalf("after version race getLease=%d, want 2 because truth must not stamp across lease version change", got)
	}
}

func TestCreateLeaseCanceledDuringHostLookupReturnsContextCanceled(t *testing.T) {
	ctx := context.Background()
	mem := storage.NewMemoryStore()
	if _, err := mem.UpsertProxy(ctx, domain.Proxy{ID: "proxy-a", Endpoint: "http://proxy.local:8080", Healthy: true, Weight: 1}); err != nil {
		t.Fatalf("UpsertProxy() error = %v", err)
	}
	svc := NewService(mem, "http://gateway.local")
	resolver := newCancelFirstResolver()
	svc.resolver = resolver

	requestCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		_, err := svc.CreateLease(requestCtx, domain.Principal{TenantID: "tenant-a"}, "idem-cancel-create", CreateLeaseRequest{
			Subject:     domain.Subject{Type: "user", ID: "user-a"},
			ResourceRef: domain.ResourceRef{Kind: "url", ID: "https://example.com/resource"},
		})
		errCh <- err
	}()

	select {
	case call := <-resolver.started:
		if call != 1 {
			t.Fatalf("resolver call = %d, want 1", call)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for create DNS lookup")
	}

	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("CreateLease error = %v, want context.Canceled", err)
		}
	case <-time.After(200 * time.Millisecond):
		close(resolver.fallback)
		t.Fatal("CreateLease did not return after context cancellation")
	}
}
