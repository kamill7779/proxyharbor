package storage

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/kamill7779/proxyharbor/internal/shared/domain"
)

type MemoryStore struct {
	mu          sync.RWMutex
	leases      map[string]domain.Lease
	idempotency map[string]string
	policies    map[string]domain.Policy
	providers   map[string]domain.Provider
	proxies     map[string]domain.Proxy
	catalogs    map[string]domain.Catalog
	audit       map[string]domain.AuditEvent
	usage       map[string]domain.UsageEvent
}

func NewMemoryStore() *MemoryStore {
	now := time.Now().UTC()
	store := &MemoryStore{
		leases:      map[string]domain.Lease{},
		idempotency: map[string]string{},
		policies:    map[string]domain.Policy{},
		providers:   map[string]domain.Provider{},
		proxies:     map[string]domain.Proxy{},
		catalogs:    map[string]domain.Catalog{},
		audit:       map[string]domain.AuditEvent{},
		usage:       map[string]domain.UsageEvent{},
	}

	_, _ = store.UpsertPolicy(context.Background(), domain.Policy{ID: "default", TenantID: "default", Version: 1, Name: "Default allow policy", Enabled: true, TTLSeconds: 1800, CreatedAt: now, UpdatedAt: now})
	_, _ = store.UpsertProvider(context.Background(), domain.Provider{ID: "static-dev", TenantID: "default", Type: "static", Name: "Static dev provider", Enabled: true, CreatedAt: now, UpdatedAt: now})
	_, _ = store.UpsertProxy(context.Background(), domain.Proxy{ID: "proxy-dev", TenantID: "default", ProviderID: "static-dev", Endpoint: "http://127.0.0.1:18080", Healthy: true, Weight: 1, LastSeenAt: now})
	return store
}

func key(tenantID, id string) string { return tenantID + "/" + id }

func (s *MemoryStore) GetLeaseByIdempotency(_ context.Context, scope IdempotencyScope) (domain.Lease, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	id, ok := s.idempotency[scope.String()]
	if !ok {
		return domain.Lease{}, false, nil
	}
	lease, ok := s.leases[key(scope.TenantID, id)]
	return copyLease(lease), ok, nil
}

func (s *MemoryStore) CreateLease(_ context.Context, scope IdempotencyScope, lease domain.Lease) (domain.Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	scopeKey := scope.String()
	if existing, ok := s.idempotency[scopeKey]; ok {
		return copyLease(s.leases[key(scope.TenantID, existing)]), nil
	}
	// 不持久化明文密码。
	lease.Password = ""
	s.leases[key(lease.TenantID, lease.ID)] = copyLease(lease)
	s.idempotency[scopeKey] = lease.ID
	return copyLease(lease), nil
}

func (s *MemoryStore) GetLease(_ context.Context, tenantID, id string) (domain.Lease, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	lease, ok := s.leases[key(tenantID, id)]
	if !ok {
		return domain.Lease{}, domain.ErrNotFound
	}
	return copyLease(lease), nil
}

func (s *MemoryStore) UpdateLease(_ context.Context, lease domain.Lease) (domain.Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	leaseKey := key(lease.TenantID, lease.ID)
	if _, ok := s.leases[leaseKey]; !ok {
		return domain.Lease{}, domain.ErrNotFound
	}
	lease.Password = ""
	s.leases[leaseKey] = copyLease(lease)
	return copyLease(lease), nil
}

func (s *MemoryStore) RevokeLease(_ context.Context, tenantID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	leaseKey := key(tenantID, id)
	lease, ok := s.leases[leaseKey]
	if !ok {
		return domain.ErrNotFound
	}
	lease.Revoked = true
	lease.UpdatedAt = time.Now().UTC()
	s.leases[leaseKey] = lease
	return nil
}

func (s *MemoryStore) ListActiveLeases(_ context.Context, tenantID string) ([]domain.Lease, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	now := time.Now().UTC()
	leases := make([]domain.Lease, 0)
	for _, lease := range s.leases {
		if lease.TenantID == tenantID && !lease.Revoked && lease.ExpiresAt.After(now) {
			leases = append(leases, copyLease(lease))
		}
	}
	sort.Slice(leases, func(i, j int) bool { return leases[i].ID < leases[j].ID })
	return leases, nil
}

func (s *MemoryStore) DeleteExpiredLeases(_ context.Context, tenantID string, before time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	deleted := 0
	for leaseKey, lease := range s.leases {
		if lease.TenantID == tenantID && !lease.ExpiresAt.IsZero() && lease.ExpiresAt.Before(before) {
			delete(s.leases, leaseKey)
			deleted++
		}
	}
	return deleted, nil
}

func (s *MemoryStore) ChooseHealthyProxy(_ context.Context, tenantID string) (domain.Proxy, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	proxies := make([]domain.Proxy, 0)
	for _, proxy := range s.proxies {
		if proxy.TenantID == tenantID && proxy.Healthy {
			proxies = append(proxies, copyProxy(proxy))
		}
	}
	if len(proxies) == 0 {
		return domain.Proxy{}, domain.ErrNoHealthyProxy
	}
	sort.Slice(proxies, func(i, j int) bool {
		if proxies[i].Weight == proxies[j].Weight {
			return proxies[i].ID < proxies[j].ID
		}
		return proxies[i].Weight > proxies[j].Weight
	})
	return proxies[0], nil
}

func (s *MemoryStore) ListProviders(_ context.Context, tenantID string) ([]domain.Provider, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	providers := make([]domain.Provider, 0)
	for _, provider := range s.providers {
		if provider.TenantID == tenantID {
			providers = append(providers, copyProvider(provider))
		}
	}
	sort.Slice(providers, func(i, j int) bool { return providers[i].ID < providers[j].ID })
	return providers, nil
}

func (s *MemoryStore) GetProvider(_ context.Context, tenantID, id string) (domain.Provider, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	provider, ok := s.providers[key(tenantID, id)]
	if !ok {
		return domain.Provider{}, domain.ErrNotFound
	}
	return copyProvider(provider), nil
}

func (s *MemoryStore) UpsertProvider(_ context.Context, provider domain.Provider) (domain.Provider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	if provider.TenantID == "" {
		provider.TenantID = "default"
	}
	if provider.ID == "" {
		provider.ID = "provider-" + now.Format("20060102150405.000000000")
	}
	if provider.Type == "" {
		provider.Type = "static"
	}
	if provider.Name == "" {
		provider.Name = provider.ID
	}
	if provider.CreatedAt.IsZero() {
		provider.CreatedAt = now
	}
	provider.UpdatedAt = now
	s.providers[key(provider.TenantID, provider.ID)] = copyProvider(provider)
	return copyProvider(provider), nil
}

func (s *MemoryStore) DeleteProvider(_ context.Context, tenantID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.providers, key(tenantID, id))
	return nil
}

func (s *MemoryStore) LatestCatalog(_ context.Context, tenantID string) (domain.Catalog, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if catalog, ok := s.catalogs[tenantID]; ok {
		return copyCatalog(catalog), nil
	}
	proxies := s.proxiesForTenantLocked(tenantID)
	now := time.Now().UTC()
	return domain.Catalog{TenantID: tenantID, Version: "memory", Proxies: proxies, Generated: now, ExpiresAt: now.Add(time.Minute)}, nil
}

func (s *MemoryStore) UpsertProxy(_ context.Context, proxy domain.Proxy) (domain.Proxy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if proxy.TenantID == "" {
		proxy.TenantID = "default"
	}
	if proxy.ID == "" {
		proxy.ID = "proxy-" + time.Now().UTC().Format("20060102150405.000000000")
	}
	if proxy.ProviderID == "" {
		proxy.ProviderID = "static-dev"
	}
	if proxy.Weight == 0 {
		proxy.Weight = 1
	}
	if proxy.LastSeenAt.IsZero() {
		proxy.LastSeenAt = time.Now().UTC()
	}
	s.proxies[key(proxy.TenantID, proxy.ID)] = copyProxy(proxy)
	return copyProxy(proxy), nil
}

func (s *MemoryStore) GetProxy(_ context.Context, tenantID, id string) (domain.Proxy, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	proxy, ok := s.proxies[key(tenantID, id)]
	if !ok {
		return domain.Proxy{}, domain.ErrNotFound
	}
	return copyProxy(proxy), nil
}

func (s *MemoryStore) DeleteProxy(_ context.Context, tenantID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.proxies, key(tenantID, id))
	return nil
}

func (s *MemoryStore) SaveCatalogSnapshot(_ context.Context, catalog domain.Catalog) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.catalogs[catalog.TenantID] = copyCatalog(catalog)
	return nil
}

func (s *MemoryStore) ListCatalogProxies(_ context.Context, tenantID string) ([]domain.Proxy, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.proxiesForTenantLocked(tenantID), nil
}

func (s *MemoryStore) ListPolicies(_ context.Context, tenantID string) ([]domain.Policy, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	policies := make([]domain.Policy, 0)
	for _, policy := range s.policies {
		if policy.TenantID == tenantID {
			policies = append(policies, copyPolicy(policy))
		}
	}
	sort.Slice(policies, func(i, j int) bool { return policies[i].ID < policies[j].ID })
	return policies, nil
}

func (s *MemoryStore) GetPolicy(_ context.Context, tenantID, id string) (domain.Policy, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	policy, ok := s.policies[key(tenantID, id)]
	if !ok {
		return domain.Policy{}, domain.ErrNotFound
	}
	return copyPolicy(policy), nil
}

func (s *MemoryStore) UpsertPolicy(_ context.Context, policy domain.Policy) (domain.Policy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	if policy.ID == "" {
		policy.ID = "policy-" + now.Format("20060102150405.000000000")
	}
	if policy.TenantID == "" {
		policy.TenantID = "default"
	}
	existing, ok := s.policies[key(policy.TenantID, policy.ID)]
	if ok {
		policy.Version = existing.Version + 1
		policy.CreatedAt = existing.CreatedAt
	} else if policy.Version == 0 {
		policy.Version = 1
		policy.CreatedAt = now
	}
	if policy.TTLSeconds == 0 {
		policy.TTLSeconds = 1800
	}
	policy.UpdatedAt = now
	s.policies[key(policy.TenantID, policy.ID)] = copyPolicy(policy)
	return copyPolicy(policy), nil
}

func (s *MemoryStore) DeletePolicy(_ context.Context, tenantID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.policies, key(tenantID, id))
	return nil
}

func (s *MemoryStore) AppendAuditEvents(_ context.Context, events []domain.AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, event := range events {
		if event.EventID != "" {
			s.audit[key(event.TenantID, event.EventID)] = copyAuditEvent(event)
		}
	}
	return nil
}

func (s *MemoryStore) ListAuditEvents(_ context.Context, tenantID string, limit int) ([]domain.AuditEvent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	events := make([]domain.AuditEvent, 0)
	for _, event := range s.audit {
		if event.TenantID == tenantID {
			events = append(events, copyAuditEvent(event))
		}
	}
	sort.Slice(events, func(i, j int) bool { return events[i].OccurredAt.After(events[j].OccurredAt) })
	if limit > 0 && len(events) > limit {
		events = events[:limit]
	}
	return events, nil
}

func (s *MemoryStore) AppendUsageEvents(_ context.Context, events []domain.UsageEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, event := range events {
		if event.EventID != "" {
			s.usage[key(event.TenantID, event.EventID)] = event
		}
	}
	return nil
}

func (s *MemoryStore) proxiesForTenantLocked(tenantID string) []domain.Proxy {
	proxies := make([]domain.Proxy, 0)
	for _, proxy := range s.proxies {
		if proxy.TenantID == tenantID {
			proxies = append(proxies, copyProxy(proxy))
		}
	}
	sort.Slice(proxies, func(i, j int) bool { return proxies[i].ID < proxies[j].ID })
	return proxies
}

func copyLease(lease domain.Lease) domain.Lease {
	lease.Subject.Labels = copyStringMap(lease.Subject.Labels)
	lease.ResourceRef.Labels = copyStringMap(lease.ResourceRef.Labels)
	return lease
}

func copyPolicy(policy domain.Policy) domain.Policy {
	policy.Labels = copyStringMap(policy.Labels)
	return policy
}

func copyProxy(proxy domain.Proxy) domain.Proxy {
	proxy.Labels = copyStringMap(proxy.Labels)
	return proxy
}

func copyProvider(provider domain.Provider) domain.Provider {
	provider.Labels = copyStringMap(provider.Labels)
	return provider
}

func copyCatalog(catalog domain.Catalog) domain.Catalog {
	catalog.Proxies = append([]domain.Proxy(nil), catalog.Proxies...)
	for i := range catalog.Proxies {
		catalog.Proxies[i] = copyProxy(catalog.Proxies[i])
	}
	return catalog
}

func copyAuditEvent(event domain.AuditEvent) domain.AuditEvent {
	event.Metadata = copyStringMap(event.Metadata)
	return event
}

func copyStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
