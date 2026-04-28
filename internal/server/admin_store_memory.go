package server

import (
	"context"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/kamill7779/proxyharbor/internal/auth"
	"github.com/kamill7779/proxyharbor/internal/shared/domain"
)

type MemoryAdminStore struct {
	mu      sync.Mutex
	tenants map[string]domain.Tenant
	keys    map[string][]auth.TenantKey
	version int64
	audits  []domain.AuditEvent
}

func NewMemoryAdminStore() *MemoryAdminStore {
	return &MemoryAdminStore{
		tenants: make(map[string]domain.Tenant),
		keys:    make(map[string][]auth.TenantKey),
	}
}

func (s *MemoryAdminStore) GetTenant(ctx context.Context, id string) (domain.Tenant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tenant, ok := s.tenants[id]
	if !ok {
		return domain.Tenant{}, domain.ErrTenantNotFound
	}
	return tenant, nil
}

func (s *MemoryAdminStore) ListTenants(ctx context.Context) ([]domain.Tenant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]domain.Tenant, 0, len(s.tenants))
	for _, tenant := range s.tenants {
		out = append(out, tenant)
	}
	return out, nil
}

func (s *MemoryAdminStore) CreateTenant(ctx context.Context, tenant domain.Tenant) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if tenant.ID == "" {
		return domain.ErrBadRequest
	}
	if _, exists := s.tenants[tenant.ID]; exists {
		return errors.New("tenant_exists")
	}
	if tenant.CreatedAt.IsZero() {
		tenant.CreatedAt = time.Now().UTC()
	}
	s.tenants[tenant.ID] = tenant
	return nil
}

func (s *MemoryAdminStore) UpdateTenant(ctx context.Context, id string, displayName *string, status *string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tenant, ok := s.tenants[id]
	if !ok {
		return domain.ErrTenantNotFound
	}
	if displayName != nil {
		tenant.Name = *displayName
	}
	if status != nil {
		switch *status {
		case "active", "enabled":
			tenant.Enabled = true
		case "disabled", "deleted":
			tenant.Enabled = false
			for idx := range s.keys[id] {
				if s.keys[id][idx].RevokedAt == nil {
					now := time.Now().UTC()
					s.keys[id][idx].RevokedAt = &now
				}
			}
			s.version++
		default:
			return domain.ErrBadRequest
		}
	}
	s.tenants[id] = tenant
	return nil
}

func (s *MemoryAdminStore) SoftDeleteTenant(ctx context.Context, id string) error {
	status := "deleted"
	return s.UpdateTenant(ctx, id, nil, &status)
}

func (s *MemoryAdminStore) ListTenantKeys(ctx context.Context, tenantID string) ([]auth.TenantKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := s.keys[tenantID]
	out := make([]auth.TenantKey, len(keys))
	copy(out, keys)
	return out, nil
}

func (s *MemoryAdminStore) CreateTenantKey(ctx context.Context, key auth.TenantKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tenants[key.TenantID]; !ok {
		return domain.ErrTenantNotFound
	}
	s.keys[key.TenantID] = append(s.keys[key.TenantID], key)
	s.version++
	return nil
}

func (s *MemoryAdminStore) RevokeTenantKey(ctx context.Context, tenantID, keyID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	for idx := range s.keys[tenantID] {
		if s.keys[tenantID][idx].ID == keyID {
			s.keys[tenantID][idx].RevokedAt = &now
			s.version++
			return nil
		}
	}
	return domain.ErrNotFound
}

func (s *MemoryAdminStore) IncrementTenantKeysVersion(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.version++
	return nil
}

func (s *MemoryAdminStore) AppendAuditEvents(ctx context.Context, events []domain.AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audits = append(s.audits, events...)
	return nil
}

func (s *MemoryAdminStore) GetTenantKeys(ctx context.Context) ([]auth.TenantKeyRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tenantKeyRowsLocked(), nil
}

func (s *MemoryAdminStore) GetTenantKeysSince(ctx context.Context, since time.Time) ([]auth.TenantKeyRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tenantKeyRowsLocked(), nil
}

func (s *MemoryAdminStore) GetTenantKeysVersion(ctx context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.version, nil
}

func (s *MemoryAdminStore) tenantKeyRowsLocked() []auth.TenantKeyRow {
	var rows []auth.TenantKeyRow
	for _, keys := range s.keys {
		for _, key := range keys {
			var hash [32]byte
			decoded, err := hex.DecodeString(key.KeyHash)
			if err == nil && len(decoded) == len(hash) {
				copy(hash[:], decoded)
			}
			rows = append(rows, auth.TenantKeyRow{
				ID:        key.ID,
				TenantID:  key.TenantID,
				KeyHash:   hash,
				KeyFP:     key.KeyFP,
				Label:     key.Label,
				Purpose:   key.Purpose,
				CreatedBy: key.CreatedBy,
				CreatedAt: key.CreatedAt,
				ExpiresAt: key.ExpiresAt,
				RevokedAt: key.RevokedAt,
			})
		}
	}
	return rows
}
