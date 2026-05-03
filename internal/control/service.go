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
	"sync"
	"time"

	"github.com/kamill7779/proxyharbor/internal/cache"
	"github.com/kamill7779/proxyharbor/internal/control/selector"
	"github.com/kamill7779/proxyharbor/internal/metrics"
	"github.com/kamill7779/proxyharbor/internal/shared/domain"
	"github.com/kamill7779/proxyharbor/internal/storage"
)

const (
	hostSafetyCacheTTL    = 15 * time.Second
	hostSafetyCacheMax    = 4096
	validateTruthCacheTTL = 250 * time.Millisecond
	validateTruthCacheMax = 8192
)

type hostSafetyEntry struct {
	safe      bool
	expiresAt time.Time
}

type validateTruthEntry struct {
	fingerprint string
	verifiedAt  time.Time
}

type ipResolver interface {
	LookupIP(ctx context.Context, network, host string) ([]net.IP, error)
}

type Service struct {
	store                      storage.Store
	cache                      cache.Cache
	cacheTTL                   time.Duration
	now                        func() time.Time
	gatewayURL                 string
	allowInternalProxyEndpoint bool
	resolver                   ipResolver
	selector                   selector.ProxySelector
	selectorMode               string
	logger                     *slog.Logger
	hostSafetyMu               sync.RWMutex
	hostSafety                 map[string]hostSafetyEntry
	hostSafetyTTL              time.Duration
	validateTruthMu            sync.RWMutex
	validateTruth              map[string]validateTruthEntry
	validateTruthTTL           time.Duration
}

func NewService(store storage.Store, gatewayURL string) *Service {
	return &Service{
		store:            store,
		cache:            cache.Noop{},
		cacheTTL:         time.Minute,
		now:              time.Now,
		gatewayURL:       gatewayURL,
		resolver:         net.DefaultResolver,
		selector:         selector.NewLocal(),
		selectorMode:     selector.NameLocal,
		hostSafety:       make(map[string]hostSafetyEntry),
		hostSafetyTTL:    hostSafetyCacheTTL,
		validateTruth:    make(map[string]validateTruthEntry),
		validateTruthTTL: validateTruthCacheTTL,
	}
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
		s.selector = selector.NewLocal()
		s.selectorMode = selector.NameLocal
		return
	}
	s.selector = sel
	s.selectorMode = selector.Name(sel)
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
	if !s.safeResource(req.ResourceRef) {
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
	selectorLatency := float64(time.Since(started).Milliseconds())
	metrics.SelectorLatencyMS.Observe(selectorLatency)
	s.observeSelectorLatency(selectorLatency)
	if err != nil {
		s.logSelectorError(principal.TenantID, len(candidates), err)
		metrics.SelectorErrors.Inc()
		s.incrementSelectorError(err)
		return domain.Lease{}, err
	}
	s.incrementSelectorSelected()
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
		s.invalidateValidateTruth(updated.TenantID, updated.ID)
	}
	return updated, err
}

func (s *Service) RevokeLease(ctx context.Context, principal domain.Principal, leaseID string) error {
	err := s.store.RevokeLease(ctx, principal.TenantID, leaseID)
	if err == nil {
		_ = s.cache.InvalidateLease(ctx, principal.TenantID, leaseID)
		s.invalidateValidateTruth(principal.TenantID, leaseID)
	}
	return err
}

func (s *Service) ValidateLease(ctx context.Context, tenantID, leaseID, password, target string) (domain.Lease, error) {
	if !s.safeTarget(target) {
		return domain.Lease{}, domain.ErrUnsafeDestination
	}
	if cached, hit, _ := s.cache.GetLease(ctx, tenantID, leaseID); hit {
		if s.hasFreshValidateTruth(cached) {
			if err := s.validateLeaseFields(cached, password, target); err != nil {
				return domain.Lease{}, err
			}
			return cached, nil
		}
	}
	lease, err := s.store.GetLease(ctx, tenantID, leaseID)
	if err != nil {
		s.invalidateValidateTruth(tenantID, leaseID)
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
	s.stampValidateTruth(lease)
	if err := s.validateLeaseFields(lease, password, target); err != nil {
		return domain.Lease{}, err
	}
	return lease, nil
}

func (s *Service) validateLeaseFields(lease domain.Lease, password, target string) error {
	if !verifyLeasePassword(lease.ID, lease.PasswordHash, password) {
		return domain.ErrAuthFailed
	}
	if lease.Revoked {
		return domain.ErrLeaseRevoked
	}
	if !s.now().Before(lease.ExpiresAt) {
		return domain.ErrLeaseExpired
	}
	if !resourceMatchesTarget(lease.ResourceRef, target) {
		return domain.ErrPolicyDenied
	}
	return nil
}

func validateTruthKey(tenantID, leaseID string) string {
	return tenantID + "|" + leaseID
}

func leaseFingerprint(lease domain.Lease) string {
	revoked := 0
	if lease.Revoked {
		revoked = 1
	}
	return fmt.Sprintf("%d|%d|%d|%s|%s:%s|%s",
		lease.Generation,
		revoked,
		lease.ExpiresAt.UnixNano(),
		lease.PasswordHash,
		lease.ResourceRef.Kind, lease.ResourceRef.ID,
		lease.ProxyID,
	)
}

func (s *Service) hasFreshValidateTruth(lease domain.Lease) bool {
	ttl := s.validateTruthTTL
	if ttl <= 0 {
		return false
	}
	key := validateTruthKey(lease.TenantID, lease.ID)
	s.validateTruthMu.RLock()
	entry, ok := s.validateTruth[key]
	s.validateTruthMu.RUnlock()
	if !ok {
		return false
	}
	if !entry.verifiedAt.Add(ttl).After(s.now()) {
		return false
	}
	return entry.fingerprint == leaseFingerprint(lease)
}

func (s *Service) stampValidateTruth(lease domain.Lease) {
	ttl := s.validateTruthTTL
	if ttl <= 0 {
		return
	}
	key := validateTruthKey(lease.TenantID, lease.ID)
	now := s.now()
	entry := validateTruthEntry{fingerprint: leaseFingerprint(lease), verifiedAt: now}
	s.validateTruthMu.Lock()
	defer s.validateTruthMu.Unlock()
	if len(s.validateTruth) >= validateTruthCacheMax {
		cutoff := now.Add(-ttl)
		for k, v := range s.validateTruth {
			if !v.verifiedAt.After(cutoff) {
				delete(s.validateTruth, k)
			}
		}
		if len(s.validateTruth) >= validateTruthCacheMax {
			for k := range s.validateTruth {
				delete(s.validateTruth, k)
				break
			}
		}
	}
	s.validateTruth[key] = entry
}

func (s *Service) invalidateValidateTruth(tenantID, leaseID string) {
	key := validateTruthKey(tenantID, leaseID)
	s.validateTruthMu.Lock()
	delete(s.validateTruth, key)
	s.validateTruthMu.Unlock()
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
		"tenant_present", tenantID != "",
		"candidate_count", candidateCount,
		"error", domain.ErrorCode(err),
		"error_kind", string(domain.ErrorKindOf(err)),
		"reason", domain.ErrorReason(err),
	)
}

func (s *Service) observeSelectorLatency(latencyMS float64) {
	switch s.selectorMode {
	case selector.NameZFair:
		metrics.SelectorZFairLatency.Observe(latencyMS)
	default:
		metrics.SelectorLocalLatency.Observe(latencyMS)
	}
}

func (s *Service) incrementSelectorError(err error) {
	switch s.selectorMode {
	case selector.NameZFair:
		metrics.SelectorZFairErrors.Inc()
		switch domain.ErrorKindOf(err) {
		case domain.ErrorKindSelectorNoCandidates:
			metrics.SelectorZFairNoCandidateErrors.Inc()
		case domain.ErrorKindSelectorNoEligible:
			metrics.SelectorZFairNoEligibleErrors.Inc()
		case domain.ErrorKindSelectorRedis:
			metrics.SelectorZFairRedisErrors.Inc()
		case domain.ErrorKindSelectorMalformedResult:
			metrics.SelectorZFairMalformedErrors.Inc()
		case domain.ErrorKindSelectorStaleResult:
			metrics.SelectorZFairStaleErrors.Inc()
		case domain.ErrorKindSelectorEmptyResult:
			metrics.SelectorZFairEmptyErrors.Inc()
		case domain.ErrorKindSelectorReadyRebuild:
			metrics.SelectorZFairRebuildErrors.Inc()
		default:
			metrics.SelectorZFairUnknownErrors.Inc()
		}
	default:
		metrics.SelectorLocalErrors.Inc()
		switch domain.ErrorKindOf(err) {
		case domain.ErrorKindSelectorNoCandidates:
			metrics.SelectorLocalNoCandidateErrors.Inc()
		case domain.ErrorKindSelectorNoEligible:
			metrics.SelectorLocalNoEligibleErrors.Inc()
		default:
			metrics.SelectorLocalUnknownErrors.Inc()
		}
	}
}

func (s *Service) incrementSelectorSelected() {
	switch s.selectorMode {
	case selector.NameZFair:
		metrics.SelectorZFairSelected.Inc()
		metrics.SelectorZFairSelectedResult.Inc()
	default:
		metrics.SelectorLocalSelected.Inc()
	}
}

func (s *Service) safeResource(resource domain.ResourceRef) bool {
	return s.safeTarget(resource.ID)
}

func resourceMatchesTarget(resource domain.ResourceRef, target string) bool {
	return extractHost(resource.ID) == extractHost(target)
}

func (s *Service) safeTarget(target string) bool {
	host := extractHost(target)
	return s.isSafeHost(host)
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

// isSafeHost answers whether host can be used as a lease target. It preserves
// the original fail-closed semantics for empty hosts, well-known loopback
// names, internal IP literals, and DNS lookup failures. Only resolved unsafe
// hostname decisions are cached for a short TTL; safe hostname resolutions are
// rechecked on every call so DNS rebinding cannot ride a cached positive
// decision. Public IP literals skip the resolver entirely.
func (s *Service) isSafeHost(host string) bool {
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
	if decision, ok := s.lookupHostSafety(host); ok {
		return decision
	}
	resolver := s.resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ips, err := resolver.LookupIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return false
	}
	for _, ip := range ips {
		if isInternalIP(ip) {
			s.storeHostSafety(host, false)
			return false
		}
	}
	return true
}

func (s *Service) lookupHostSafety(host string) (bool, bool) {
	s.hostSafetyMu.RLock()
	entry, ok := s.hostSafety[host]
	s.hostSafetyMu.RUnlock()
	if !ok {
		return false, false
	}
	if !entry.expiresAt.After(s.now()) {
		return false, false
	}
	if entry.safe {
		return false, false
	}
	return entry.safe, true
}

func (s *Service) storeHostSafety(host string, safe bool) {
	ttl := s.hostSafetyTTL
	if ttl <= 0 {
		return
	}
	s.hostSafetyMu.Lock()
	defer s.hostSafetyMu.Unlock()
	if len(s.hostSafety) >= hostSafetyCacheMax {
		now := s.now()
		for k, v := range s.hostSafety {
			if !v.expiresAt.After(now) {
				delete(s.hostSafety, k)
			}
		}
		if len(s.hostSafety) >= hostSafetyCacheMax {
			for k := range s.hostSafety {
				delete(s.hostSafety, k)
				break
			}
		}
	}
	s.hostSafety[host] = hostSafetyEntry{safe: safe, expiresAt: s.now().Add(ttl)}
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
	return s.isSafeHost(strings.ToLower(u.Hostname()))
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
