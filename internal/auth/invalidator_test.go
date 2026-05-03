package auth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestInvalidationMessageRoundTripLowCardinality(t *testing.T) {
	msg := InvalidationMessage{
		Cache:     CacheAuth,
		Action:    ActionRefresh,
		Version:   42,
		Reason:    "tenant_key_revoke",
		Instance:  "instance-a",
		EmittedAt: time.Unix(100, 0).UTC(),
	}
	payload, err := EncodeInvalidationMessage(msg)
	if err != nil {
		t.Fatalf("EncodeInvalidationMessage() error = %v", err)
	}
	if string(payload) == "" {
		t.Fatal("encoded payload is empty")
	}
	if !stringContains(string(payload), `"schema_version":1`) {
		t.Fatalf("payload missing schema_version: %s", payload)
	}
	for _, forbidden := range []string{"tenant_id", "key_id", "proxy_id", "lease_id", "request_id"} {
		if containsBytes(payload, forbidden) {
			t.Fatalf("payload contains high-cardinality field %q: %s", forbidden, payload)
		}
	}
	got, err := DecodeInvalidationMessage(payload)
	if err != nil {
		t.Fatalf("DecodeInvalidationMessage() error = %v", err)
	}
	if got.SchemaVersion != InvalidationSchemaVersion || got.Cache != msg.Cache || got.Action != msg.Action || got.Version != msg.Version || got.Reason != msg.Reason || got.Instance != msg.Instance {
		t.Fatalf("decoded message = %+v, want %+v", got, msg)
	}
}

func TestDecodeInvalidationMessageRejectsUnknownValues(t *testing.T) {
	if _, err := DecodeInvalidationMessage([]byte(`{"cache":"auth","action":"refresh"}`)); err != nil {
		t.Fatalf("DecodeInvalidationMessage(valid) error = %v", err)
	}
	if _, err := DecodeInvalidationMessage([]byte(`{"cache":"tenant-123","action":"refresh"}`)); !errors.Is(err, ErrInvalidationBadCache) {
		t.Fatalf("bad cache error = %v, want %v", err, ErrInvalidationBadCache)
	}
	if _, err := DecodeInvalidationMessage([]byte(`{"cache":"auth","action":"tenant-123"}`)); !errors.Is(err, ErrInvalidationBadAction) {
		t.Fatalf("bad action error = %v, want %v", err, ErrInvalidationBadAction)
	}
	if _, err := DecodeInvalidationMessage([]byte(`{"schema_version":2,"cache":"auth","action":"refresh"}`)); !errors.Is(err, ErrInvalidationBadVersion) {
		t.Fatalf("bad version error = %v, want %v", err, ErrInvalidationBadVersion)
	}
	if _, err := DecodeInvalidationMessage([]byte(`{`)); !errors.Is(err, ErrInvalidationDecode) {
		t.Fatalf("decode error = %v, want %v", err, ErrInvalidationDecode)
	}
}

func TestClassifyInvalidationError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "nil", err: nil, want: "none"},
		{name: "decode", err: ErrInvalidationDecode, want: "decode"},
		{name: "bad_cache", err: ErrInvalidationBadCache, want: "bad_cache"},
		{name: "bad_action", err: ErrInvalidationBadAction, want: "bad_action"},
		{name: "bad_version", err: ErrInvalidationBadVersion, want: "bad_version"},
		{name: "timeout", err: context.DeadlineExceeded, want: "timeout"},
		{name: "redis", err: redis.Nil, want: "redis"},
		{name: "backend", err: errors.New("boom"), want: "backend"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyInvalidationError(tt.err); got != tt.want {
				t.Fatalf("ClassifyInvalidationError() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRedisInvalidatorPublishesGenericMessage(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	sub := client.Subscribe(context.Background(), DefaultInvalidationChannel)
	t.Cleanup(func() { _ = sub.Close() })
	if _, err := sub.ReceiveTimeout(context.Background(), time.Second); err != nil {
		t.Fatalf("subscribe confirm error = %v", err)
	}
	invalidator := NewRedisInvalidator(client, DefaultInvalidationChannel, nil)
	if err := invalidator.Publish(context.Background(), InvalidationMessage{Cache: CacheCatalog, Action: ActionInvalidate, Reason: "proxy_update"}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	msg, err := sub.ReceiveMessage(contextWithTimeout(t, time.Second))
	if err != nil {
		t.Fatalf("ReceiveMessage() error = %v", err)
	}
	decoded, err := DecodeInvalidationMessage([]byte(msg.Payload))
	if err != nil {
		t.Fatalf("DecodeInvalidationMessage() error = %v", err)
	}
	if decoded.Cache != CacheCatalog || decoded.Action != ActionInvalidate || decoded.Reason != "proxy_update" {
		t.Fatalf("decoded message = %+v", decoded)
	}
}

func TestSubscribeCacheInvalidationsAppliesAuthCatalogLease(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	store := &memoryKeyStore{version: 1, rows: []TenantKeyRow{{ID: "key-1", TenantID: "tenant-a", KeyHash: sha256Pepper([]byte("pepper-12345678901234567890123456"), "secret"), Purpose: "test"}}}
	dynamic, err := NewDynamicStore(store, []byte("pepper-12345678901234567890123456"), time.Hour)
	if err != nil {
		t.Fatalf("NewDynamicStore() error = %v", err)
	}
	hot := &recordingHotCache{}
	status := NewStatusTracker("redis", "configured")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go SubscribeCacheInvalidationsWithStatus(ctx, client, DefaultInvalidationChannel, dynamic, hot, status, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := waitUntil(5*time.Second, func() bool { return status.InvalidationStatus().State == "subscribed" }); err != nil {
		t.Fatalf("subscriber not ready: %v status=%+v", err, status.InvalidationStatus())
	}
	store.setRows(2, nil)
	invalidator := NewRedisInvalidator(client, DefaultInvalidationChannel, nil)
	if err := invalidator.Publish(context.Background(), InvalidationMessage{Cache: CacheAuth, Action: ActionRefresh}); err != nil {
		t.Fatalf("Publish(auth) error = %v", err)
	}
	if err := waitUntil(5*time.Second, func() bool { _, ok := dynamic.Lookup("secret"); return !ok }); err != nil {
		t.Fatalf("auth refresh did not apply: %v", err)
	}
	if err := invalidator.Publish(context.Background(), InvalidationMessage{Cache: CacheCatalog, Action: ActionInvalidate}); err != nil {
		t.Fatalf("Publish(catalog) error = %v", err)
	}
	if err := invalidator.Publish(context.Background(), InvalidationMessage{Cache: CacheLease, Action: ActionInvalidate}); err != nil {
		t.Fatalf("Publish(lease) error = %v", err)
	}
	if err := waitUntil(5*time.Second, func() bool { return hot.catalogInvalidations.Load() > 0 && hot.leaseInvalidations.Load() > 0 }); err != nil {
		t.Fatalf("hot cache invalidation did not apply: %v", err)
	}
}

type memoryKeyStore struct {
	mu         sync.RWMutex
	version    int64
	versionErr error
	rows       []TenantKeyRow
}

func (s *memoryKeyStore) GetTenantKeys(context.Context) ([]TenantKeyRow, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]TenantKeyRow(nil), s.rows...), nil
}
func (s *memoryKeyStore) GetTenantKeysSince(context.Context, time.Time) ([]TenantKeyRow, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]TenantKeyRow(nil), s.rows...), nil
}
func (s *memoryKeyStore) GetTenantKeysVersion(context.Context) (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.versionErr != nil {
		return 0, s.versionErr
	}
	return s.version, nil
}
func (s *memoryKeyStore) IncrementTenantKeysVersion(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.version++
	return nil
}
func (s *memoryKeyStore) CreateTenantKey(context.Context, TenantKeyRow) error { return nil }
func (s *memoryKeyStore) RevokeTenantKey(context.Context, string) error       { return nil }
func (s *memoryKeyStore) GetTenant(context.Context, string) (TenantRow, error) {
	return TenantRow{}, nil
}
func (s *memoryKeyStore) setRows(version int64, rows []TenantKeyRow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.version = version
	s.rows = append([]TenantKeyRow(nil), rows...)
}

type recordingHotCache struct {
	catalogInvalidations atomic.Int64
	leaseInvalidations   atomic.Int64
}

func (h *recordingHotCache) InvalidateCatalogLocal(context.Context) error {
	h.catalogInvalidations.Add(1)
	return nil
}

func (h *recordingHotCache) InvalidateLeaseLocal(context.Context, string, string) error { return nil }

func (h *recordingHotCache) InvalidateAllLeases(context.Context) error {
	h.leaseInvalidations.Add(1)
	return nil
}

func waitUntil(timeout time.Duration, fn func() bool) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if fn() {
			return nil
		}
		select {
		case <-deadline.C:
			return context.DeadlineExceeded
		case <-ticker.C:
		}
	}
}

func containsBytes(payload []byte, needle string) bool {
	return len(needle) > 0 && stringContains(string(payload), needle)
}

func stringContains(s, needle string) bool {
	for i := 0; i+len(needle) <= len(s); i++ {
		if s[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func contextWithTimeout(t *testing.T, timeout time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	return ctx
}
