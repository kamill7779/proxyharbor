package control

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/kamill7779/proxyharbor/internal/cache"
	"github.com/kamill7779/proxyharbor/internal/shared/domain"
	"github.com/kamill7779/proxyharbor/internal/storage"
)

type Service struct {
	store      storage.Store
	cache      cache.Cache
	cacheTTL   time.Duration
	now        func() time.Time
	gatewayURL string
}

func NewService(store storage.Store, gatewayURL string) *Service {
	return &Service{store: store, cache: cache.Noop{}, cacheTTL: time.Minute, now: time.Now, gatewayURL: gatewayURL}
}

// SetCache 注入热路径缓存。传 nil 等同于关闭缓存。
func (s *Service) SetCache(c cache.Cache, ttl time.Duration) {
	if c == nil {
		s.cache = cache.Noop{}
	} else {
		s.cache = c
	}
	if ttl > 0 {
		s.cacheTTL = ttl
	}
}

type CreateLeaseRequest struct {
	Subject     domain.Subject     `json:"subject"`
	ResourceRef domain.ResourceRef `json:"resource_ref"`
	PolicyID    string             `json:"policy_id,omitempty"`
	TTLSeconds  int64              `json:"ttl_seconds,omitempty"`
}

func (s *Service) CreateLease(ctx context.Context, principal domain.Principal, key string, req CreateLeaseRequest) (domain.Lease, error) {
	if key == "" {
		return domain.Lease{}, domain.ErrIdempotencyConflict
	}
	if req.Subject.ID == "" || req.Subject.Type == "" {
		return domain.Lease{}, domain.ErrSubjectNotEligible
	}
	if req.ResourceRef.ID == "" || req.ResourceRef.Kind == "" {
		return domain.Lease{}, domain.ErrPolicyDenied
	}
	if !safeResource(req.ResourceRef) {
		return domain.Lease{}, domain.ErrUnsafeDestination
	}
	idem := storage.IdempotencyScope{TenantID: principal.TenantID, StableSubjectID: req.Subject.StableID(), ResourceRef: req.ResourceRef.StableID(), RequestKind: "create_lease", Key: key}
	if lease, ok, err := s.store.GetLeaseByIdempotency(ctx, idem); err != nil {
		return domain.Lease{}, err
	} else if ok {
		return lease, nil
	}
	policy, err := s.pickPolicy(ctx, principal.TenantID, req)
	if err != nil {
		return domain.Lease{}, err
	}
	proxy, err := s.store.ChooseHealthyProxy(ctx, principal.TenantID)
	if err != nil {
		return domain.Lease{}, err
	}
	now := s.now()
	ttl := time.Duration(policy.TTLSeconds) * time.Second
	if req.TTLSeconds > 0 && req.TTLSeconds < policy.TTLSeconds {
		ttl = time.Duration(req.TTLSeconds) * time.Second
	}
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	lease := domain.Lease{ID: "lease_" + randomHex(12), TenantID: principal.TenantID, Generation: 1, Subject: req.Subject, ResourceRef: req.ResourceRef, PolicyRef: domain.PolicyRef{ID: policy.ID, Version: policy.Version, Hash: fmt.Sprintf("v%d", policy.Version)}, GatewayURL: s.gatewayURL, Username: "lease", ProxyID: proxy.ID, ExpiresAt: now.Add(ttl), RenewBefore: now.Add(ttl / 2), CatalogVersion: "memory", CandidateSetID: "healthy", CreatedAt: now, UpdatedAt: now}
	lease.Password = "lease_" + randomHex(24)
	return s.store.CreateLease(ctx, idem, lease)
}

func (s *Service) RenewLease(ctx context.Context, principal domain.Principal, leaseID string) (domain.Lease, error) {
	lease, err := s.store.GetLease(ctx, principal.TenantID, leaseID)
	if err != nil {
		return domain.Lease{}, err
	}
	if lease.Revoked {
		return domain.Lease{}, domain.ErrLeaseRevoked
	}
	lease.Generation++
	lease.ExpiresAt = s.now().Add(30 * time.Minute)
	lease.RenewBefore = s.now().Add(15 * time.Minute)
	lease.UpdatedAt = s.now()
	updated, err := s.store.UpdateLease(ctx, lease)
	if err == nil {
		_ = s.cache.InvalidateLease(ctx, updated.TenantID, updated.ID)
	}
	return updated, err
}

func (s *Service) RevokeLease(ctx context.Context, principal domain.Principal, leaseID string) error {
	err := s.store.RevokeLease(ctx, principal.TenantID, leaseID)
	if err == nil {
		_ = s.cache.InvalidateLease(ctx, principal.TenantID, leaseID)
	}
	return err
}

func (s *Service) ValidateLease(ctx context.Context, tenantID, leaseID, password, target string) (domain.Lease, error) {
	if !safeTarget(target) {
		return domain.Lease{}, domain.ErrUnsafeDestination
	}
	lease, hit, _ := s.cache.GetLease(ctx, tenantID, leaseID)
	if !hit {
		var err error
		lease, err = s.store.GetLease(ctx, tenantID, leaseID)
		if err != nil {
			return domain.Lease{}, err
		}
		ttl := s.cacheTTL
		if remain := time.Until(lease.ExpiresAt); remain > 0 && remain < ttl {
			ttl = remain
		}
		_ = s.cache.PutLease(ctx, lease, ttl)
	}
	if lease.Revoked {
		return domain.Lease{}, domain.ErrLeaseRevoked
	}
	if s.now().After(lease.ExpiresAt) {
		return domain.Lease{}, domain.ErrLeaseRevoked
	}
	if password == "" || lease.Password != password {
		return domain.Lease{}, domain.ErrAuthFailed
	}
	return lease, nil
}

func (s *Service) ValidateGatewayRequest(ctx context.Context, tenantID, leaseID, password, target string) (domain.Lease, domain.Proxy, error) {
	lease, err := s.ValidateLease(ctx, tenantID, leaseID, password, target)
	if err != nil {
		return domain.Lease{}, domain.Proxy{}, err
	}
	proxy, err := s.store.GetProxy(ctx, tenantID, lease.ProxyID)
	if err != nil {
		return domain.Lease{}, domain.Proxy{}, err
	}
	if !proxy.Healthy {
		return domain.Lease{}, domain.Proxy{}, domain.ErrNoHealthyProxy
	}
	return lease, proxy, nil
}

func (s *Service) Catalog(ctx context.Context, tenantID string) (domain.Catalog, error) {
	if cat, hit, _ := s.cache.GetCatalog(ctx, tenantID); hit {
		return cat, nil
	}
	cat, err := s.store.LatestCatalog(ctx, tenantID)
	if err == nil {
		_ = s.cache.PutCatalog(ctx, cat, s.cacheTTL)
	}
	return cat, err
}
func (s *Service) ListProviders(ctx context.Context, tenantID string) ([]domain.Provider, error) {
	return s.store.ListProviders(ctx, tenantID)
}
func (s *Service) CreateProvider(ctx context.Context, principal domain.Principal, provider domain.Provider) (domain.Provider, error) {
	provider.TenantID = principal.TenantID
	return s.store.UpsertProvider(ctx, provider)
}
func (s *Service) GetProvider(ctx context.Context, principal domain.Principal, id string) (domain.Provider, error) {
	return s.store.GetProvider(ctx, principal.TenantID, id)
}
func (s *Service) UpdateProvider(ctx context.Context, principal domain.Principal, id string, provider domain.Provider) (domain.Provider, error) {
	provider.ID = id
	provider.TenantID = principal.TenantID
	return s.store.UpsertProvider(ctx, provider)
}
func (s *Service) DeleteProvider(ctx context.Context, principal domain.Principal, id string) error {
	return s.store.DeleteProvider(ctx, principal.TenantID, id)
}
func (s *Service) ListProxies(ctx context.Context, tenantID string) ([]domain.Proxy, error) {
	return s.store.ListCatalogProxies(ctx, tenantID)
}
func (s *Service) CreateProxy(ctx context.Context, principal domain.Principal, proxy domain.Proxy) (domain.Proxy, error) {
	proxy.TenantID = principal.TenantID
	if proxy.Endpoint == "" || !safeProxyEndpoint(proxy.Endpoint) {
		return domain.Proxy{}, domain.ErrUnsafeDestination
	}
	return s.store.UpsertProxy(ctx, proxy)
}
func (s *Service) GetProxy(ctx context.Context, principal domain.Principal, id string) (domain.Proxy, error) {
	return s.store.GetProxy(ctx, principal.TenantID, id)
}
func (s *Service) UpdateProxy(ctx context.Context, principal domain.Principal, id string, proxy domain.Proxy) (domain.Proxy, error) {
	proxy.ID = id
	proxy.TenantID = principal.TenantID
	if proxy.Endpoint == "" || !safeProxyEndpoint(proxy.Endpoint) {
		return domain.Proxy{}, domain.ErrUnsafeDestination
	}
	return s.store.UpsertProxy(ctx, proxy)
}
func (s *Service) DeleteProxy(ctx context.Context, principal domain.Principal, id string) error {
	return s.store.DeleteProxy(ctx, principal.TenantID, id)
}
func (s *Service) UpdateProxyHealth(ctx context.Context, principal domain.Principal, id string, healthy bool, failureHint string) (domain.Proxy, error) {
	proxy, err := s.store.GetProxy(ctx, principal.TenantID, id)
	if err != nil {
		return domain.Proxy{}, err
	}
	proxy.Healthy = healthy
	proxy.FailureHint = failureHint
	proxy.LastSeenAt = s.now().UTC()
	return s.store.UpsertProxy(ctx, proxy)
}
func (s *Service) ListPolicies(ctx context.Context, tenantID string) ([]domain.Policy, error) {
	return s.store.ListPolicies(ctx, tenantID)
}
func (s *Service) CreatePolicy(ctx context.Context, principal domain.Principal, policy domain.Policy) (domain.Policy, error) {
	policy.TenantID = principal.TenantID
	return s.store.UpsertPolicy(ctx, policy)
}
func (s *Service) GetPolicy(ctx context.Context, principal domain.Principal, id string) (domain.Policy, error) {
	return s.store.GetPolicy(ctx, principal.TenantID, id)
}
func (s *Service) UpdatePolicy(ctx context.Context, principal domain.Principal, id string, policy domain.Policy) (domain.Policy, error) {
	policy.ID = id
	policy.TenantID = principal.TenantID
	return s.store.UpsertPolicy(ctx, policy)
}
func (s *Service) DeletePolicy(ctx context.Context, principal domain.Principal, id string) error {
	return s.store.DeletePolicy(ctx, principal.TenantID, id)
}
func (s *Service) RecordUsage(ctx context.Context, events []domain.UsageEvent) error {
	return s.store.AppendUsageEvents(ctx, events)
}

func (s *Service) RecordGatewayFeedback(ctx context.Context, events []domain.AuditEvent) error {
	return s.store.AppendAuditEvents(ctx, events)
}

func (s *Service) pickPolicy(ctx context.Context, tenantID string, req CreateLeaseRequest) (domain.Policy, error) {
	if req.PolicyID != "" {
		return s.store.GetPolicy(ctx, tenantID, req.PolicyID)
	}
	policies, err := s.store.ListPolicies(ctx, tenantID)
	if err != nil {
		return domain.Policy{}, err
	}
	for _, policy := range policies {
		if policy.Enabled {
			return policy, nil
		}
	}
	return domain.Policy{}, domain.ErrPolicyDenied
}

func safeResource(resource domain.ResourceRef) bool { return safeTarget(resource.ID) }
func safeTarget(target string) bool {
	if target == "" {
		return false
	}
	u, err := url.Parse(target)
	if err == nil && u.Host != "" {
		target = u.Hostname()
	}
	host := strings.Trim(strings.ToLower(target), "[]")
	if splitHost, _, err := net.SplitHostPort(host); err == nil {
		host = strings.Trim(splitHost, "[]")
	}
	blocked := map[string]bool{"localhost": true, "127.0.0.1": true, "0.0.0.0": true, "::1": true}
	if blocked[host] {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified())
	}
	return host != ""
}

func safeProxyEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	return err == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https")
}
func randomHex(bytes int) string {
	b := make([]byte, bytes)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
