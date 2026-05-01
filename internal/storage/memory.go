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
	catalog     domain.Catalog
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
		audit:       map[string]domain.AuditEvent{},
		usage:       map[string]domain.UsageEvent{},
	}

	_, _ = store.UpsertPolicy(context.Background(), domain.Policy{ID: "default", Version: 1, Name: "Default allow policy", Enabled: true, TTLSeconds: 1800, CreatedAt: now, UpdatedAt: now})
	_, _ = store.UpsertProvider(context.Background(), domain.Provider{ID: "default", Type: "static", Name: "Default provider", Enabled: true, CreatedAt: now, UpdatedAt: now})
	return store
}

func (s *MemoryStore) CheckDependencies(context.Context) map[string]error {
	return map[string]error{"memory": nil}
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
	// Do not persist plaintext lease passwords.
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
	existing, ok := s.leases[leaseKey]
	if !ok {
		return domain.Lease{}, domain.ErrNotFound
	}
	if lease.Generation <= 1 || existing.Generation != lease.Generation-1 {
		return domain.Lease{}, domain.ErrStaleLease
	}
	if existing.Revoked {
		return domain.Lease{}, domain.ErrLeaseRevoked
	}
	if !time.Now().UTC().Before(existing.ExpiresAt) {
		return domain.Lease{}, domain.ErrLeaseExpired
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
	if lease.Revoked {
		return nil
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

func (s *MemoryStore) ChooseHealthyProxy(_ context.Context) (domain.Proxy, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	proxies := s.selectableProxiesLocked(time.Now().UTC())
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

func (s *MemoryStore) ListSelectableProxies(_ context.Context) ([]domain.Proxy, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.selectableProxiesLocked(time.Now().UTC()), nil
}

func (s *MemoryStore) RecordProxyOutcome(_ context.Context, proxyID string, delta ProxyHealthDelta) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	proxy, ok := s.proxies[key("", proxyID)]
	if !ok {
		return domain.ErrNotFound
	}
	observedAt := delta.ObservedAt.UTC()
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	proxy.LastCheckedAt = observedAt
	if proxy.HealthScore == 0 && proxy.ConsecutiveFailures == 0 && proxy.LastSuccessAt.IsZero() && proxy.LastFailureAt.IsZero() {
		proxy.HealthScore = 100
	}
	if delta.Success {
		reward := delta.Reward
		if reward <= 0 {
			reward = 1
		}
		proxy.ConsecutiveFailures = 0
		proxy.HealthScore = minInt(100, proxy.HealthScore+reward)
		proxy.LastSuccessAt = observedAt
		if delta.LatencyMS > 0 {
			if proxy.LatencyEWMAms <= 0 {
				proxy.LatencyEWMAms = delta.LatencyMS
			} else {
				proxy.LatencyEWMAms = int(float64(proxy.LatencyEWMAms)*0.8 + float64(delta.LatencyMS)*0.2)
			}
		}
		if !proxy.CircuitOpenUntil.IsZero() && !proxy.CircuitOpenUntil.After(observedAt) {
			proxy.CircuitOpenUntil = time.Time{}
		}
	} else {
		penalty := delta.Penalty
		if penalty <= 0 {
			penalty = 10
		}
		proxy.ConsecutiveFailures++
		proxy.HealthScore = maxInt(0, proxy.HealthScore-penalty)
		proxy.LastFailureAt = observedAt
		proxy.FailureHint = delta.FailureHint
		threshold := delta.MaxConsecutiveFailure
		if threshold <= 0 {
			threshold = 3
		}
		baseCooldown := delta.BaseCooldown
		if baseCooldown <= 0 {
			baseCooldown = 30 * time.Second
		}
		maxCooldown := delta.MaxCooldown
		if maxCooldown <= 0 {
			maxCooldown = 5 * time.Minute
		}
		if proxy.ConsecutiveFailures >= threshold {
			cooldown := time.Duration(proxy.ConsecutiveFailures) * baseCooldown
			if cooldown > maxCooldown {
				cooldown = maxCooldown
			}
			proxy.CircuitOpenUntil = observedAt.Add(cooldown)
		}
	}
	s.proxies[key("", proxyID)] = copyProxy(proxy)
	return nil
}

func (s *MemoryStore) ListProviders(_ context.Context) ([]domain.Provider, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	providers := make([]domain.Provider, 0)
	for _, provider := range s.providers {
		providers = append(providers, copyProvider(provider))
	}
	sort.Slice(providers, func(i, j int) bool { return providers[i].ID < providers[j].ID })
	return providers, nil
}

func (s *MemoryStore) GetProvider(_ context.Context, id string) (domain.Provider, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	provider, ok := s.providers[key("", id)]
	if !ok {
		return domain.Provider{}, domain.ErrNotFound
	}
	return copyProvider(provider), nil
}

func (s *MemoryStore) UpsertProvider(_ context.Context, provider domain.Provider) (domain.Provider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
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
	s.providers[key("", provider.ID)] = copyProvider(provider)
	return copyProvider(provider), nil
}

func (s *MemoryStore) DeleteProvider(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.providers, key("", id))
	return nil
}

func (s *MemoryStore) LatestCatalog(_ context.Context) (domain.Catalog, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.catalog.Version != "" {
		return copyCatalog(s.catalog), nil
	}
	proxies := s.catalogProxiesLocked()
	now := time.Now().UTC()
	return domain.Catalog{Version: "memory", Proxies: proxies, Generated: now, ExpiresAt: now.Add(time.Minute)}, nil
}

func (s *MemoryStore) UpsertProxy(_ context.Context, proxy domain.Proxy) (domain.Proxy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if proxy.ID == "" {
		proxy.ID = "proxy-" + time.Now().UTC().Format("20060102150405.000000000")
	}
	if proxy.ProviderID == "" {
		proxy.ProviderID = "default"
	}
	if proxy.Weight == 0 {
		proxy.Weight = 1
	}
	if proxy.HealthScore == 0 && proxy.ConsecutiveFailures == 0 && proxy.LastSuccessAt.IsZero() && proxy.LastFailureAt.IsZero() {
		proxy.HealthScore = 100
	}
	if proxy.LastSeenAt.IsZero() {
		proxy.LastSeenAt = time.Now().UTC()
	}
	s.proxies[key("", proxy.ID)] = copyProxy(proxy)
	return copyProxy(proxy), nil
}

func (s *MemoryStore) GetProxy(_ context.Context, id string) (domain.Proxy, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	proxy, ok := s.proxies[key("", id)]
	if !ok {
		return domain.Proxy{}, domain.ErrNotFound
	}
	return copyProxy(proxy), nil
}

func (s *MemoryStore) DeleteProxy(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.proxies, key("", id))
	return nil
}

func (s *MemoryStore) SaveCatalogSnapshot(_ context.Context, catalog domain.Catalog) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.catalog = copyCatalog(catalog)
	return nil
}

func (s *MemoryStore) ListCatalogProxies(_ context.Context) ([]domain.Proxy, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.catalogProxiesLocked(), nil
}

func (s *MemoryStore) selectableProxiesLocked(now time.Time) []domain.Proxy {
	proxies := make([]domain.Proxy, 0)
	for _, proxy := range s.proxies {
		if !proxy.Healthy || proxy.Weight <= 0 || proxy.HealthScore <= 0 {
			continue
		}
		if !proxy.CircuitOpenUntil.IsZero() && proxy.CircuitOpenUntil.After(now) {
			continue
		}
		proxies = append(proxies, copyProxy(proxy))
	}
	sort.Slice(proxies, func(i, j int) bool { return proxies[i].ID < proxies[j].ID })
	return proxies
}

func (s *MemoryStore) ListPolicies(_ context.Context) ([]domain.Policy, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	policies := make([]domain.Policy, 0)
	for _, policy := range s.policies {
		policies = append(policies, copyPolicy(policy))
	}
	sort.Slice(policies, func(i, j int) bool { return policies[i].ID < policies[j].ID })
	return policies, nil
}

func (s *MemoryStore) GetPolicy(_ context.Context, id string) (domain.Policy, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	policy, ok := s.policies[key("", id)]
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
	existing, ok := s.policies[key("", policy.ID)]
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
	s.policies[key("", policy.ID)] = copyPolicy(policy)
	return copyPolicy(policy), nil
}

func (s *MemoryStore) DeletePolicy(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.policies, key("", id))
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
	sort.Slice(events, func(i, j int) bool {
		if events[i].OccurredAt.Equal(events[j].OccurredAt) {
			return events[i].EventID > events[j].EventID
		}
		return events[i].OccurredAt.After(events[j].OccurredAt)
	})
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

func (s *MemoryStore) catalogProxiesLocked() []domain.Proxy {
	proxies := make([]domain.Proxy, 0)
	for _, proxy := range s.proxies {
		proxies = append(proxies, copyProxy(proxy))
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

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
