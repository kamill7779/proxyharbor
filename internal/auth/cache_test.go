package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"
)

// mockKeyStore is an in-memory KeyStore for testing.
type mockKeyStore struct {
	keys    []TenantKeyRow
	version int64
	err     error
}

func (m *mockKeyStore) GetTenantKeys(ctx context.Context) ([]TenantKeyRow, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.keys, nil
}
func (m *mockKeyStore) GetTenantKeysSince(ctx context.Context, since time.Time) ([]TenantKeyRow, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.keys, nil
}
func (m *mockKeyStore) GetTenantKeysVersion(ctx context.Context) (int64, error) {
	if m.err != nil {
		return 0, m.err
	}
	return m.version, nil
}
func (m *mockKeyStore) IncrementTenantKeysVersion(ctx context.Context) error        { return nil }
func (m *mockKeyStore) CreateTenantKey(ctx context.Context, key TenantKeyRow) error { return nil }
func (m *mockKeyStore) RevokeTenantKey(ctx context.Context, keyID string) error     { return nil }
func (m *mockKeyStore) GetTenant(ctx context.Context, tenantID string) (TenantRow, error) {
	return TenantRow{}, errors.New("not found")
}

func hashKey(pepper []byte, key string) [32]byte {
	h := sha256.New()
	h.Write(pepper)
	h.Write([]byte(key))
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// U1: startup full load N=1000, all hit, <100ms.
func TestDynamicStore_Load1000(t *testing.T) {
	pepper := []byte("test-pepper-32-bytes-long!!!!")
	var keys []TenantKeyRow
	for i := 0; i < 1000; i++ {
		k := "key_" + hex.EncodeToString([]byte{byte(i), byte(i >> 8), byte(i >> 16)})
		keys = append(keys, TenantKeyRow{ID: "k" + k, TenantID: "tnt", KeyHash: hashKey(pepper, k)})
	}
	store := &mockKeyStore{keys: keys, version: 1}
	start := time.Now()
	ds, err := NewDynamicStore(store, pepper, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatalf("startup load too slow: %v", time.Since(start))
	}
	for i := 0; i < 1000; i++ {
		k := "key_" + hex.EncodeToString([]byte{byte(i), byte(i >> 8), byte(i >> 16)})
		if _, ok := ds.Lookup(k); !ok {
			t.Fatalf("key %d miss", i)
		}
	}
}

// U2: revoke key, version bump, next tick miss.
func TestDynamicStore_RevokeMiss(t *testing.T) {
	pepper := []byte("test-pepper-32-bytes-long!!!!")
	k1 := "key_one"
	h1 := hashKey(pepper, k1)
	store := &mockKeyStore{
		keys:    []TenantKeyRow{{ID: "k1", TenantID: "tnt", KeyHash: h1}},
		version: 1,
	}
	ds, err := NewDynamicStore(store, pepper, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ds.Lookup(k1); !ok {
		t.Fatal("expected hit before revoke")
	}
	// simulate revoke: return revoked row and bump version
	now := time.Now()
	store.keys = []TenantKeyRow{{ID: "k1", TenantID: "tnt", KeyHash: h1, RevokedAt: &now}}
	store.version = 2
	ds.tick(context.Background())
	if _, ok := ds.Lookup(k1); ok {
		t.Fatal("expected miss after revoke")
	}
}

func TestDynamicStore_RevokeTwoInstancesWithinRefreshWindow(t *testing.T) {
	pepper := []byte("test-pepper-32-bytes-long!!!!")
	presented := "key_one"
	hash := hashKey(pepper, presented)
	store := &mockKeyStore{
		keys:    []TenantKeyRow{{ID: "k1", TenantID: "tnt", KeyHash: hash}},
		version: 1,
	}
	first, err := NewDynamicStore(store, pepper, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewDynamicStore(store, pepper, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := first.Lookup(presented); !ok {
		t.Fatal("first instance should hit before revoke")
	}
	if _, ok := second.Lookup(presented); !ok {
		t.Fatal("second instance should hit before revoke")
	}

	now := time.Now()
	store.keys = []TenantKeyRow{{ID: "k1", TenantID: "tnt", KeyHash: hash, RevokedAt: &now}}
	store.version = 2
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		first.tick(context.Background())
		second.tick(context.Background())
		_, firstOK := first.Lookup(presented)
		_, secondOK := second.Lookup(presented)
		if !firstOK && !secondOK {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("expected both instances to miss within 5s after version bump")
}

// U3: DB temporarily unavailable, loaded keys still hit, stale metric exported.
func TestDynamicStore_StaleFallback(t *testing.T) {
	pepper := []byte("test-pepper-32-bytes-long!!!!")
	k1 := "key_one"
	h1 := hashKey(pepper, k1)
	store := &mockKeyStore{
		keys:    []TenantKeyRow{{ID: "k1", TenantID: "tnt", KeyHash: h1}},
		version: 1,
	}
	ds, err := NewDynamicStore(store, pepper, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	before := StaleCount()
	store.err = errors.New("db down")
	ds.tick(context.Background())
	if StaleCount() <= before {
		t.Fatal("expected stale counter to increase")
	}
	if _, ok := ds.Lookup(k1); !ok {
		t.Fatal("expected hit while db unavailable")
	}
}

// U4: DB long unavailable + cold key -> miss.
func TestDynamicStore_ColdMiss(t *testing.T) {
	pepper := []byte("test-pepper-32-bytes-long!!!!")
	store := &mockKeyStore{err: errors.New("db down")}
	_, err := NewDynamicStore(store, pepper, time.Hour)
	if err == nil {
		t.Fatal("expected fail-fast on cold start with db down")
	}
}
