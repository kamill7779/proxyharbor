package auth

import (
	"context"
	"sync"
	"time"
)

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
