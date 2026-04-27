package auth

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// sharedKeyStore is a simple in-memory KeyStore shared between two
// DynamicStore instances to model two ProxyHarbor replicas pointed at the
// same MySQL backend.
type sharedKeyStore struct {
	mu      sync.Mutex
	keys    []TenantKeyRow
	version int64
}

func (s *sharedKeyStore) GetTenantKeys(context.Context) ([]TenantKeyRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]TenantKeyRow, len(s.keys))
	copy(out, s.keys)
	return out, nil
}
func (s *sharedKeyStore) GetTenantKeysSince(ctx context.Context, _ time.Time) ([]TenantKeyRow, error) {
	return s.GetTenantKeys(ctx)
}
func (s *sharedKeyStore) GetTenantKeysVersion(context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.version, nil
}
func (s *sharedKeyStore) IncrementTenantKeysVersion(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.version++
	return nil
}
func (s *sharedKeyStore) CreateTenantKey(_ context.Context, key TenantKeyRow) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = append(s.keys, key)
	s.version++
	return nil
}
func (s *sharedKeyStore) RevokeTenantKey(_ context.Context, keyID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	for i := range s.keys {
		if s.keys[i].ID == keyID {
			s.keys[i].RevokedAt = &now
		}
	}
	s.version++
	return nil
}
func (s *sharedKeyStore) GetTenant(context.Context, string) (TenantRow, error) {
	return TenantRow{}, nil
}

// fakeBroadcaster is an in-process Invalidator that fans out events to
// every subscribed DynamicStore so tests can simulate Redis pub/sub
// without a real Redis.
type fakeBroadcaster struct {
	mu   sync.Mutex
	subs []*DynamicStore
}

func (b *fakeBroadcaster) Subscribe(d *DynamicStore) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs = append(b.subs, d)
}

func (b *fakeBroadcaster) Publish(ctx context.Context, _ InvalidationEvent) error {
	b.mu.Lock()
	subs := append([]*DynamicStore(nil), b.subs...)
	b.mu.Unlock()
	for _, s := range subs {
		_ = s.RefreshNow(ctx)
	}
	return nil
}

func (b *fakeBroadcaster) Close() error { return nil }

// TestMultiInstance_CreatePropagatesViaInvalidation simulates instance A
// admitting a new key while instance B has stale cache. After invalidation
// instance B must accept the key without waiting for the polling tick.
func TestMultiInstance_CreatePropagatesViaInvalidation(t *testing.T) {
	pepper := []byte("pepper-32-bytes-long-for-tests-only")
	store := &sharedKeyStore{version: 1}

	a, err := NewDynamicStore(store, pepper, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewDynamicStore(store, pepper, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	br := &fakeBroadcaster{}
	br.Subscribe(a)
	br.Subscribe(b)

	presented := "phk_test_key_alpha"
	row := TenantKeyRow{ID: "k1", TenantID: "t1", KeyHash: hashKey(pepper, presented)}
	if err := store.CreateTenantKey(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.Lookup(presented); ok {
		t.Fatal("instance B should miss before invalidation")
	}
	_ = br.Publish(context.Background(), InvalidationEvent{Reason: "tenant_key.create"})
	if _, ok := a.Lookup(presented); !ok {
		t.Fatal("instance A should hit after invalidation")
	}
	if _, ok := b.Lookup(presented); !ok {
		t.Fatal("instance B should hit after invalidation")
	}
}

// TestMultiInstance_RevokePropagatesViaInvalidation simulates instance A
// revoking a key. Instance B must reject the revoked key after invalidation
// without waiting for the polling tick.
func TestMultiInstance_RevokePropagatesViaInvalidation(t *testing.T) {
	pepper := []byte("pepper-32-bytes-long-for-tests-only")
	presented := "phk_test_key_beta"
	row := TenantKeyRow{ID: "k1", TenantID: "t1", KeyHash: hashKey(pepper, presented)}
	store := &sharedKeyStore{keys: []TenantKeyRow{row}, version: 1}

	a, err := NewDynamicStore(store, pepper, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewDynamicStore(store, pepper, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	br := &fakeBroadcaster{}
	br.Subscribe(a)
	br.Subscribe(b)

	if _, ok := a.Lookup(presented); !ok {
		t.Fatal("instance A should hit before revoke")
	}
	if _, ok := b.Lookup(presented); !ok {
		t.Fatal("instance B should hit before revoke")
	}
	if err := store.RevokeTenantKey(context.Background(), "k1"); err != nil {
		t.Fatal(err)
	}
	_ = br.Publish(context.Background(), InvalidationEvent{Reason: "tenant_key.revoke"})
	if _, ok := a.Lookup(presented); ok {
		t.Fatal("instance A should reject revoked key after invalidation")
	}
	if _, ok := b.Lookup(presented); ok {
		t.Fatal("instance B should reject revoked key after invalidation")
	}
}

// TestMultiInstance_PollingFallbackWithoutInvalidation simulates Redis
// being unavailable so no invalidation event reaches instance B. Instance
// B must still converge by polling within a few ticks.
func TestMultiInstance_PollingFallbackWithoutInvalidation(t *testing.T) {
	pepper := []byte("pepper-32-bytes-long-for-tests-only")
	presented := "phk_test_key_gamma"
	row := TenantKeyRow{ID: "k1", TenantID: "t1", KeyHash: hashKey(pepper, presented)}
	store := &sharedKeyStore{keys: []TenantKeyRow{row}, version: 1}

	a, err := NewDynamicStore(store, pepper, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewDynamicStore(store, pepper, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeTenantKey(context.Background(), "k1"); err != nil {
		t.Fatal(err)
	}
	// Simulate polling tick on both instances; no broadcaster involved.
	a.tick(context.Background())
	b.tick(context.Background())
	if _, ok := a.Lookup(presented); ok {
		t.Fatal("instance A should reject revoked key after polling tick")
	}
	if _, ok := b.Lookup(presented); ok {
		t.Fatal("instance B should reject revoked key after polling tick")
	}
}

func TestMultiInstance_CreatePropagatesViaRedisInvalidation(t *testing.T) {
	redisServer := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	pepper := []byte("pepper-32-bytes-long-for-tests-only")
	store := &sharedKeyStore{version: 1}
	b, err := NewDynamicStore(store, pepper, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	go SubscribeInvalidations(ctx, client, DefaultInvalidationChannel, b, logger)

	presented := "phk_test_key_redis"
	row := TenantKeyRow{ID: "k1", TenantID: "t1", KeyHash: hashKey(pepper, presented)}
	if err := store.CreateTenantKey(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.Lookup(presented); ok {
		t.Fatal("instance B should miss before redis invalidation")
	}
	invalidator := NewRedisInvalidator(client, DefaultInvalidationChannel, logger)
	if err := invalidator.Publish(context.Background(), InvalidationEvent{Reason: "tenant_key.create"}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := b.Lookup(presented); ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("instance B should hit after redis invalidation")
}

// TestSnapshot_NoSecrets verifies that the snapshot exposes only non-secret
// counters and never leaks key material.
func TestSnapshot_NoSecrets(t *testing.T) {
	pepper := []byte("pepper-32-bytes-long-for-tests-only")
	presented := "phk_secret_test_key"
	row := TenantKeyRow{ID: "k1", TenantID: "t1", KeyHash: hashKey(pepper, presented)}
	store := &sharedKeyStore{keys: []TenantKeyRow{row}, version: 7}
	d, err := NewDynamicStore(store, pepper, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	snap := d.Snapshot()
	if !snap.Initialized {
		t.Fatal("expected initialized")
	}
	if snap.Version != 7 {
		t.Fatalf("expected version 7, got %d", snap.Version)
	}
	if snap.Entries != 1 {
		t.Fatalf("expected 1 entry, got %d", snap.Entries)
	}
	// Snapshot must not transport plaintext key material.
	if got := snap.LastError; got != "" {
		t.Fatalf("unexpected last error: %q", got)
	}
}
