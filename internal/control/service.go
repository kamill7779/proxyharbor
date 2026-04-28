package control

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/kamill7779/proxyharbor/internal/cache"
	"github.com/kamill7779/proxyharbor/internal/control/selector"
	"github.com/kamill7779/proxyharbor/internal/metrics"
	"github.com/kamill7779/proxyharbor/internal/shared/domain"
	"github.com/kamill7779/proxyharbor/internal/storage"
)

type Service struct {
	store                      storage.Store
	cache                      cache.Cache
	cacheTTL                   time.Duration
	now                        func() time.Time
	gatewayURL                 string
	allowInternalProxyEndpoint bool
	resolver                   *net.Resolver
	selector                   selector.ProxySelector
	logger                     *slog.Logger
}

func NewService(store storage.Store, gatewayURL string) *Service {
	return &Service{store: store, cache: cache.Noop{}, cacheTTL: time.Minute, now: time.Now, gatewayURL: gatewayURL, resolver: net.DefaultResolver, selector: selector.FirstSelectable{}}
}

func (s *Service) SetLogger(logger *slog.Logger) {
	if logger == nil {
		s.logger = nil
		return
	}
	s.logger = logger
}

func (s *Service) SetAllowInternalProxyEndpoint(allow bool) { s.allowInternalProxyEndpoint = allow }

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

func (s *Service) SetSelector(sel selector.ProxySelector) {
	if sel == nil {
		s.selector = selector.FirstSelectable{}
		return
	}
	s.selector = sel
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
	policy, err := s.pickPolicy(ctx, req)
	if err != nil {
		return domain.Lease{}, err
	}
	candidates, err := s.store.ListSelectableProxies(ctx)
	if err != nil {
		return domain.Lease{}, err
	}
	started := time.Now()
	proxy, err := s.selector.Select(ctx, candidates, selector.SelectOptions{AffinityPolicy: selector.PolicyNone})
	metrics.SelectorLatencyMS.Observe(float64(time.Since(started).Milliseconds()))
	if err != nil {
		s.logSelectorError(principal.TenantID, len(candidates), err)
		metrics.SelectorErrors.Inc()
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
	leaseID := "lease_" + randomHex(12)
	lease := domain.Lease{ID: leaseID, TenantID: principal.TenantID, Generation: 1, Subject: req.Subject, ResourceRef: req.ResourceRef, PolicyRef: domain.PolicyRef{ID: policy.ID, Version: policy.Version, Hash: fmt.Sprintf("v%d", policy.Version)}, GatewayURL: s.gatewayURL, Username: principal.TenantID + "|" + leaseID, ProxyID: proxy.ID, ExpiresAt: now.Add(ttl), RenewBefore: now.Add(ttl / 2), CatalogVersion: "memory", CandidateSetID: "healthy", CreatedAt: now, UpdatedAt: now}
	plaintext := "lease_" + randomHex(24)
	lease.Password = plaintext
	lease.PasswordHash = hashLeasePassword(lease.ID, plaintext)
	saved, err := s.store.CreateLease(ctx, idem, lease)
	if err != nil {
		return domain.Lease{}, err
	}

	if saved.ID == lease.ID && saved.PasswordHash == lease.PasswordHash {
		saved.Password = plaintext
	}
	return saved, nil
}

func (s *Service) RenewLease(ctx context.Context, principal domain.Principal, leaseID string) (domain.Lease, error) {
	lease, err := s.store.GetLease(ctx, principal.TenantID, leaseID)
	if err != nil {
		return domain.Lease{}, err
	}
	now := s.now()
	if lease.Revoked {
		return domain.Lease{}, domain.ErrLeaseRevoked
	}
	if !now.Before(lease.ExpiresAt) {
		return domain.Lease{}, domain.ErrLeaseExpired
	}
	policy, err := s.store.GetPolicy(ctx, lease.PolicyRef.ID)
	if err != nil {
		return domain.Lease{}, err
	}
	ttl := time.Duration(policy.TTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	lease.Generation++
	lease.ExpiresAt = now.Add(ttl)
	lease.RenewBefore = now.Add(ttl / 2)
	lease.UpdatedAt = now
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
		remain := time.Until(lease.ExpiresAt)
		if remain > 0 {
			ttl := s.cacheTTL
			if remain < ttl {
				ttl = remain
			}
			_ = s.cache.PutLease(ctx, lease, ttl)
		}
	}
	if !verifyLeasePassword(lease.ID, lease.PasswordHash, password) {
		return domain.Lease{}, domain.ErrAuthFailed
	}
	if lease.Revoked {
		return domain.Lease{}, domain.ErrLeaseRevoked
	}
	if !s.now().Before(lease.ExpiresAt) {
		return domain.Lease{}, domain.ErrLeaseExpired
	}
	if !resourceMatchesTarget(lease.ResourceRef, target) {
		return domain.Lease{}, domain.ErrPolicyDenied
	}
	return lease, nil
}

func (s *Service) ValidateGatewayRequest(ctx context.Context, tenantID, leaseID, password, target string) (domain.Lease, domain.Proxy, error) {
	lease, err := s.ValidateLease(ctx, tenantID, leaseID, password, target)
	if err != nil {
		return domain.Lease{}, domain.Proxy{}, err
	}
	proxy, err := s.store.GetProxy(ctx, lease.ProxyID)
	if err != nil {
		return domain.Lease{}, domain.Proxy{}, err
	}
	if !proxySelectable(proxy, s.now()) {
		return domain.Lease{}, domain.Proxy{}, domain.ErrNoHealthyProxy
	}
	return lease, proxy, nil
}

func proxySelectable(proxy domain.Proxy, now time.Time) bool {
	if !proxy.Healthy || proxy.Weight <= 0 || proxy.HealthScore <= 0 {
		return false
	}
	return proxy.CircuitOpenUntil.IsZero() || !proxy.CircuitOpenUntil.After(now)
}

func (s *Service) Catalog(ctx context.Context) (domain.Catalog, error) {
	if cat, hit, _ := s.cache.GetCatalog(ctx); hit {
		return cat, nil
	}
	cat, err := s.store.LatestCatalog(ctx)
	if err == nil {
		_ = s.cache.PutCatalog(ctx, cat, s.cacheTTL)
	}
	return cat, err
}
func (s *Service) ListProviders(ctx context.Context) ([]domain.Provider, error) {
	return s.store.ListProviders(ctx)
}
func (s *Service) CreateProvider(ctx context.Context, principal domain.Principal, provider domain.Provider) (domain.Provider, error) {
	if principal.Type != "admin" {
		return domain.Provider{}, domain.ErrForbidden
	}
	out, err := s.store.UpsertProvider(ctx, provider)
	if err == nil {
		_ = s.cache.InvalidateCatalog(ctx)
	}
	return out, err
}
func (s *Service) GetProvider(ctx context.Context, principal domain.Principal, id string) (domain.Provider, error) {
	return s.store.GetProvider(ctx, id)
}
func (s *Service) UpdateProvider(ctx context.Context, principal domain.Principal, id string, provider domain.Provider) (domain.Provider, error) {
	if principal.Type != "admin" {
		return domain.Provider{}, domain.ErrForbidden
	}
	provider.ID = id
	out, err := s.store.UpsertProvider(ctx, provider)
	if err == nil {
		_ = s.cache.InvalidateCatalog(ctx)
	}
	return out, err
}
func (s *Service) DeleteProvider(ctx context.Context, principal domain.Principal, id string) error {
	if principal.Type != "admin" {
		return domain.ErrForbidden
	}
	err := s.store.DeleteProvider(ctx, id)
	if err == nil {
		_ = s.cache.InvalidateCatalog(ctx)
	}
	return err
}
func (s *Service) ListProxies(ctx context.Context) ([]domain.Proxy, error) {
	return s.store.ListCatalogProxies(ctx)
}
func (s *Service) CreateProxy(ctx context.Context, principal domain.Principal, proxy domain.Proxy) (domain.Proxy, error) {
	if principal.Type != "admin" {
		return domain.Proxy{}, domain.ErrForbidden
	}
	if proxy.Endpoint == "" || !s.safeProxyEndpoint(proxy.Endpoint) {
		return domain.Proxy{}, domain.ErrUnsafeDestination
	}
	out, err := s.store.UpsertProxy(ctx, proxy)
	if err == nil {
		_ = s.cache.InvalidateCatalog(ctx)
	}
	return out, err
}
func (s *Service) GetProxy(ctx context.Context, principal domain.Principal, id string) (domain.Proxy, error) {
	return s.store.GetProxy(ctx, id)
}
func (s *Service) UpdateProxy(ctx context.Context, principal domain.Principal, id string, proxy domain.Proxy) (domain.Proxy, error) {
	if principal.Type != "admin" {
		return domain.Proxy{}, domain.ErrForbidden
	}
	proxy.ID = id
	if proxy.Endpoint == "" || !s.safeProxyEndpoint(proxy.Endpoint) {
		return domain.Proxy{}, domain.ErrUnsafeDestination
	}
	out, err := s.store.UpsertProxy(ctx, proxy)
	if err == nil {
		_ = s.cache.InvalidateCatalog(ctx)
	}
	return out, err
}
func (s *Service) DeleteProxy(ctx context.Context, principal domain.Principal, id string) error {
	if principal.Type != "admin" {
		return domain.ErrForbidden
	}
	err := s.store.DeleteProxy(ctx, id)
	if err == nil {
		_ = s.cache.InvalidateCatalog(ctx)
	}
	return err
}
func (s *Service) UpdateProxyHealth(ctx context.Context, principal domain.Principal, id string, healthy bool, failureHint string) (domain.Proxy, error) {
	if principal.Type != "admin" {
		return domain.Proxy{}, domain.ErrForbidden
	}
	proxy, err := s.store.GetProxy(ctx, id)
	if err != nil {
		return domain.Proxy{}, err
	}
	proxy.Healthy = healthy
	proxy.FailureHint = failureHint
	proxy.LastSeenAt = s.now().UTC()
	out, err := s.store.UpsertProxy(ctx, proxy)
	if err == nil {
		_ = s.cache.InvalidateCatalog(ctx)
	}
	return out, err
}
func (s *Service) ListPolicies(ctx context.Context) ([]domain.Policy, error) {
	return s.store.ListPolicies(ctx)
}
func (s *Service) CreatePolicy(ctx context.Context, principal domain.Principal, policy domain.Policy) (domain.Policy, error) {
	if principal.Type != "admin" || policy.ID != "default" {
		return domain.Policy{}, domain.ErrForbidden
	}
	out, err := s.store.UpsertPolicy(ctx, policy)
	if err == nil {
		_ = s.cache.InvalidateCatalog(ctx)
	}
	return out, err
}
func (s *Service) GetPolicy(ctx context.Context, principal domain.Principal, id string) (domain.Policy, error) {
	return s.store.GetPolicy(ctx, id)
}
func (s *Service) UpdatePolicy(ctx context.Context, principal domain.Principal, id string, policy domain.Policy) (domain.Policy, error) {
	if principal.Type != "admin" || id != "default" {
		return domain.Policy{}, domain.ErrForbidden
	}
	policy.ID = id
	out, err := s.store.UpsertPolicy(ctx, policy)
	if err == nil {
		_ = s.cache.InvalidateCatalog(ctx)
	}
	return out, err
}
func (s *Service) DeletePolicy(ctx context.Context, principal domain.Principal, id string) error {
	if principal.Type != "admin" || id != "default" {
		return domain.ErrForbidden
	}
	err := s.store.DeletePolicy(ctx, id)
	if err == nil {
		_ = s.cache.InvalidateCatalog(ctx)
	}
	return err
}
func (s *Service) RecordUsage(ctx context.Context, events []domain.UsageEvent) error {
	return s.store.AppendUsageEvents(ctx, events)
}

func (s *Service) RecordGatewayFeedback(ctx context.Context, events []domain.AuditEvent) error {
	return s.store.AppendAuditEvents(ctx, events)
}

func (s *Service) pickPolicy(ctx context.Context, req CreateLeaseRequest) (domain.Policy, error) {
	if req.PolicyID != "" && req.PolicyID != "default" {
		return domain.Policy{}, domain.ErrPolicyDenied
	}
	policy, err := s.store.GetPolicy(ctx, "default")
	if err != nil {
		return domain.Policy{}, err
	}
	if !policy.Enabled {
		return domain.Policy{}, domain.ErrPolicyDenied
	}
	return policy, nil
}

func (s *Service) logSelectorError(tenantID string, candidateCount int, err error) {
	logger := s.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn("selector.select.failed",
		"tenant_id", tenantID,
		"candidate_count", candidateCount,
		"error", domain.ErrorCode(err),
		"error_kind", string(domain.ErrorKindOf(err)),
		"reason", domain.ErrorReason(err),
	)
}

func safeResource(resource domain.ResourceRef) bool { return safeTarget(resource.ID) }

func resourceMatchesTarget(resource domain.ResourceRef, target string) bool {
	return extractHost(resource.ID) == extractHost(target)
}

func safeTarget(target string) bool {
	host := extractHost(target)
	return isSafeHost(host)
}

func extractHost(target string) string {
	if target == "" {
		return ""
	}
	if u, err := url.Parse(target); err == nil && u.Host != "" {
		target = u.Hostname()
	}
	host := strings.Trim(strings.ToLower(target), "[]")
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = strings.Trim(h, "[]")
	}
	return host
}

func isSafeHost(host string) bool {
	if host == "" {
		return false
	}
	switch host {
	case "localhost", "localhost.localdomain", "ip6-localhost", "ip6-loopback":
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return !isInternalIP(ip)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return false
	}
	for _, ip := range ips {
		if isInternalIP(ip) {
			return false
		}
	}
	return true
}

func isInternalIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	// CGNAT 100.64.0.0/10 娴犮儱寮风敮姝岊潌娴?metadata IP
	if v4 := ip.To4(); v4 != nil {
		if v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
			return true
		}
		if v4.Equal(net.IPv4(169, 254, 169, 254)) {
			return true
		}
	}
	return false
}

func (s *Service) safeProxyEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	if s.allowInternalProxyEndpoint {
		return true
	}
	return isSafeHost(strings.ToLower(u.Hostname()))
}

func hashLeasePassword(leaseID, plaintext string) string {
	sum := sha256.Sum256([]byte(leaseID + ":" + plaintext))
	return hex.EncodeToString(sum[:])
}

func verifyLeasePassword(leaseID, storedHash, plaintext string) bool {
	if storedHash == "" || plaintext == "" {
		return false
	}
	computed := hashLeasePassword(leaseID, plaintext)
	return subtle.ConstantTimeCompare([]byte(computed), []byte(storedHash)) == 1
}
func randomHex(bytes int) string {
	b := make([]byte, bytes)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
