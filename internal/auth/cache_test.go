package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDynamicStoreSnapshotClassifiesLastError(t *testing.T) {
	store := &cacheTestKeyStore{}
	d, err := NewDynamicStore(store, []byte("pepper-with-at-least-thirty-two-bytes"), time.Hour)
	if err != nil {
		t.Fatalf("NewDynamicStore() error = %v", err)
	}
	store.versionErr = errors.New("mysql password=super-secret-dsn failed")
	if err := d.RefreshNow(context.Background()); err == nil {
		t.Fatal("RefreshNow() error = nil, want store error")
	}
	snap := d.Snapshot()
	if snap.LastError == "" {
		t.Fatal("Snapshot().LastError = empty, want classified error")
	}
	if strings.Contains(snap.LastError, "super-secret-dsn") || strings.Contains(snap.LastError, "password=") {
		t.Fatalf("Snapshot().LastError leaked raw error: %q", snap.LastError)
	}
}

type cacheTestKeyStore struct {
	versionErr error
}

func (s *cacheTestKeyStore) GetTenantKeys(context.Context) ([]TenantKeyRow, error) {
	return nil, nil
}

func (s *cacheTestKeyStore) GetTenantKeysSince(context.Context, time.Time) ([]TenantKeyRow, error) {
	return nil, nil
}

func (s *cacheTestKeyStore) GetTenantKeysVersion(context.Context) (int64, error) {
	if s.versionErr != nil {
		return 0, s.versionErr
	}
	return 1, nil
}

func (s *cacheTestKeyStore) IncrementTenantKeysVersion(context.Context) error { return nil }

func (s *cacheTestKeyStore) CreateTenantKey(context.Context, TenantKeyRow) error { return nil }

func (s *cacheTestKeyStore) RevokeTenantKey(context.Context, string) error { return nil }

func (s *cacheTestKeyStore) GetTenant(context.Context, string) (TenantRow, error) {
	return TenantRow{}, nil
}
