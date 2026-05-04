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
	"strconv"
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
	defaultPolicyTTL      = 50 * time.Millisecond
	selectableProxyTTL    = 50 * time.Millisecond
	selectorTimeout       = 75 * time.Millisecond
	validateTruthCacheTTL = 250 * time.Millisecond
	validateTruthCacheMax = 8192
)

type hostSafetyEntry struct {
	safe      bool
	expiresAt time.Time
}

type hostSafetyCall struct {
	safe  bool
	retry bool
	done  chan struct{}
}

type validateTruthEntry struct {
	fingerprint              string
	lease                    domain.Lease
	verifiedAt               time.Time
	leaseInvalidationVersion uint64
}

type policyEntry struct {
	policy    domain.Policy
	expiresAt time.Time
}

type policyCall struct {
	policy domain.Policy
	err    error
	done   chan struct{}
	epoch  uint64
}

type selectableProxyEntry struct {
	proxies   []domain.Proxy
	expiresAt time.Time
}

type selectableProxyCall struct {
	proxies []domain.Proxy
	err     error
	done    chan struct{}
	epoch   uint64
}

type ipResolver interface {
	LookupIP(ctx context.Context, network, host string) ([]net.IP, error)
}

type leaseInvalidationVersioner interface {
	LeaseInvalidationVersion() uint64
}

type validateTruthCache interface {
	GetValidateTruth(ctx context.Context, tenantID, leaseID string) (string, bool, error)
	PutValidateTruth(ctx context.Context, tenantID, leaseID, fingerprint string, ttl time.Duration) error
	InvalidateValidateTruth(ctx context.Context, tenantID, leaseID string) error
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
	localSelector              *selector.Local
	selectorMode               string
	selectorTimeout            time.Duration
	logger                     *slog.Logger
	hostSafetyMu               sync.RWMutex
	hostSafety                 map[string]hostSafetyEntry
	hostSafetyCalls            map[string]*hostSafetyCall
	hostSafetyTTL              time.Duration
	defaultPolicyMu            sync.RWMutex
	defaultPolicy              policyEntry
	defaultPolicyCall          *policyCall
	defaultPolicyEpoch         uint64
	defaultPolicyTTL           time.Duration
	selectableProxyMu          sync.RWMutex
	selectableProxyEntry       selectableProxyEntry
	selectableProxyCall        *selectableProxyCall
	selectableProxyEpoch       uint64
	selectableProxyTTL         time.Duration
	validateTruthMu            sync.RWMutex
	validateTruth              map[string]validateTruthEntry
	validateTruthTTL           time.Duration
}

func NewService(store storage.Store, gatewayURL string) *Service {
	localSelector := selector.NewLocal()
	return &Service{
		store:              store,
		cache:              cache.Noop{},
		cacheTTL:           time.Minute,
		now:                time.Now,
		gatewayURL:         gatewayURL,
		resolver:           net.DefaultResolver,
		selector:           localSelector,
		localSelector:      localSelector,
		selectorMode:       selector.NameLocal,
		selectorTimeout:    selectorTimeout,
		hostSafety:         make(map[string]hostSafetyEntry),
		hostSafetyCalls:    make(map[string]*hostSafetyCall),
		hostSafetyTTL:      hostSafetyCacheTTL,
		defaultPolicyTTL:   defaultPolicyTTL,
		selectableProxyTTL: selectableProxyTTL,
		validateTruth:      make(map[string]validateTruthEntry),
		validateTruthTTL:   validateTruthCacheTTL,
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
	if s.localSelector == nil {
		s.localSelector = selector.NewLocal()
	}
	if sel == nil {
		s.selector = s.localSelector
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
	if !s.safeResource(ctx, req.ResourceRef) {
		return domain.Lease{}, unsafeDestinationErr(ctx)
	}
	idem := storage.IdempotencyScope{TenantID: principal.TenantID, StableSubjectID: req.Subject.StableID(), ResourceRef: req.ResourceRef.StableID(), RequestKind: "create_lease", Key: key}
	policy, err := s.pickPolicy(ctx, req)
	if err != nil {
		return domain.Lease{}, err
	}
	candidates, err := s.listSelectableProxies(ctx)
	if err != nil {
		return domain.Lease{}, err
	}
	proxy, err := s.selectLeaseProxy(ctx, principal.TenantID, candidates)
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
	leaseID := storage.LeaseIDForIdempotency(idem)
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
	s.cacheLease(ctx, saved)
	if s.hasLeaseInvalidationVersion() {
		s.rememberValidateTruth(ctx, saved, s.currentLeaseInvalidationVersion())
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
		s.clearValidateTruth(ctx, updated.TenantID, updated.ID)
	}
	return updated, err
}

func (s *Service) RevokeLease(ctx context.Context, principal domain.Principal, leaseID string) error {
	err := s.store.RevokeLease(ctx, principal.TenantID, leaseID)
	if err == nil {
		_ = s.cache.InvalidateLease(ctx, principal.TenantID, leaseID)
		s.clearValidateTruth(ctx, principal.TenantID, leaseID)
	}
	return err
}

func (s *Service) ValidateLease(ctx context.Context, tenantID, leaseID, password, target string) (domain.Lease, error) {
	if !s.safeTarget(ctx, target) {
		return domain.Lease{}, unsafeDestinationErr(ctx)
	}
	leaseVersion := s.currentLeaseInvalidationVersion()
	if lease, ok := s.localValidatedLease(ctx, tenantID, leaseID); ok {
		if err := s.validateLeaseFields(lease, password, target); err != nil {
			return domain.Lease{}, err
		}
		return lease, nil
	}
	if cached, hit, _ := s.cache.GetLease(ctx, tenantID, leaseID); hit {
		if s.hasFreshSharedValidateTruth(ctx, cached, leaseVersion) {
			if err := s.validateLeaseFields(cached, password, target); err != nil {
				return domain.Lease{}, err
			}
			return cached, nil
		}
	}
	lease, err := s.store.GetLease(ctx, tenantID, leaseID)
	if err != nil {
		s.clearValidateTruth(ctx, tenantID, leaseID)
		return domain.Lease{}, err
	}
	s.cacheLease(ctx, lease)
	if s.hasLeaseInvalidationVersion() && leaseVersion == s.currentLeaseInvalidationVersion() {
		s.rememberValidateTruth(ctx, lease, leaseVersion)
	} else {
		s.clearValidateTruth(ctx, tenantID, leaseID)
	}
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

func unsafeDestinationErr(ctx context.Context) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return domain.ErrUnsafeDestination
}

func (s *Service) cacheLease(ctx context.Context, lease domain.Lease) {
	remain := lease.ExpiresAt.Sub(s.now())
	if remain <= 0 {
		return
	}
	ttl := s.cacheTTL
	if ttl <= 0 || remain < ttl {
		ttl = remain
	}
	if ttl <= 0 {
		return
	}
	_ = s.cache.PutLease(ctx, lease, ttl)
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

func (s *Service) localValidatedLease(ctx context.Context, tenantID, leaseID string) (domain.Lease, bool) {
	entry, ok := s.freshLocalValidateTruth(tenantID, leaseID)
	if !ok {
		return domain.Lease{}, false
	}
	if !s.hasSharedValidateTruthFingerprint(ctx, entry.lease) {
		return domain.Lease{}, false
	}
	return entry.lease, true
}

func (s *Service) freshLocalValidateTruth(tenantID, leaseID string) (validateTruthEntry, bool) {
	ttl := s.validateTruthTTL
	if ttl <= 0 {
		return validateTruthEntry{}, false
	}
	key := validateTruthKey(tenantID, leaseID)
	s.validateTruthMu.RLock()
	entry, ok := s.validateTruth[key]
	s.validateTruthMu.RUnlock()
	if !ok {
		return validateTruthEntry{}, false
	}
	if !entry.verifiedAt.Add(ttl).After(s.now()) {
		return validateTruthEntry{}, false
	}
	return entry, true
}

func (s *Service) hasSharedValidateTruthFingerprint(ctx context.Context, lease domain.Lease) bool {
	cache, ok := s.cache.(validateTruthCache)
	if !ok {
		return false
	}
	raw, hit, err := cache.GetValidateTruth(ctx, lease.TenantID, lease.ID)
	if err != nil || !hit {
		return false
	}
	entry, ok := decodeValidateTruth(raw)
	if !ok {
		return false
	}
	return entry.fingerprint == leaseFingerprint(lease)
}

func (s *Service) hasFreshSharedValidateTruth(ctx context.Context, lease domain.Lease, leaseVersion uint64) bool {
	cache, ok := s.cache.(validateTruthCache)
	if !ok {
		return false
	}
	raw, hit, err := cache.GetValidateTruth(ctx, lease.TenantID, lease.ID)
	if err != nil || !hit {
		return false
	}
	entry, ok := decodeValidateTruth(raw)
	if !ok {
		return false
	}
	if entry.fingerprint != leaseFingerprint(lease) {
		return false
	}
	s.stampValidateTruth(lease, leaseVersion)
	return true
}

func (s *Service) rememberValidateTruth(ctx context.Context, lease domain.Lease, leaseVersion uint64) {
	s.stampValidateTruth(lease, leaseVersion)
	s.storeSharedValidateTruth(ctx, lease, leaseVersion)
}

func (s *Service) stampValidateTruth(lease domain.Lease, leaseVersion uint64) {
	ttl := s.validateTruthTTL
	if ttl <= 0 {
		return
	}
	key := validateTruthKey(lease.TenantID, lease.ID)
	now := s.now()
	entry := validateTruthEntry{
		fingerprint:              leaseFingerprint(lease),
		lease:                    lease,
		verifiedAt:               now,
		leaseInvalidationVersion: leaseVersion,
	}
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

func (s *Service) storeSharedValidateTruth(ctx context.Context, lease domain.Lease, leaseVersion uint64) {
	cache, ok := s.cache.(validateTruthCache)
	if !ok {
		return
	}
	ttl := s.validateTruthTTL
	if ttl <= 0 {
		return
	}
	_ = cache.PutValidateTruth(ctx, lease.TenantID, lease.ID, encodeValidateTruth(validateTruthEntry{
		fingerprint:              leaseFingerprint(lease),
		leaseInvalidationVersion: leaseVersion,
	}), ttl)
}

func (s *Service) currentLeaseInvalidationVersion() uint64 {
	if versioned, ok := s.cache.(leaseInvalidationVersioner); ok {
		return versioned.LeaseInvalidationVersion()
	}
	return 0
}

func (s *Service) hasLeaseInvalidationVersion() bool {
	_, ok := s.cache.(leaseInvalidationVersioner)
	return ok
}

func encodeValidateTruth(entry validateTruthEntry) string {
	return fmt.Sprintf("%d|%s", entry.leaseInvalidationVersion, entry.fingerprint)
}

func decodeValidateTruth(raw string) (validateTruthEntry, bool) {
	versionText, fingerprint, ok := strings.Cut(raw, "|")
	if !ok || fingerprint == "" {
		return validateTruthEntry{}, false
	}
	version, err := strconv.ParseUint(versionText, 10, 64)
	if err != nil {
		return validateTruthEntry{}, false
	}
	return validateTruthEntry{
		fingerprint:              fingerprint,
		leaseInvalidationVersion: version,
	}, true
}

func (s *Service) invalidateValidateTruth(tenantID, leaseID string) {
	key := validateTruthKey(tenantID, leaseID)
	s.validateTruthMu.Lock()
	delete(s.validateTruth, key)
	s.validateTruthMu.Unlock()
}

func (s *Service) clearValidateTruth(ctx context.Context, tenantID, leaseID string) {
	s.invalidateValidateTruth(tenantID, leaseID)
	cache, ok := s.cache.(validateTruthCache)
	if !ok {
		return
	}
	_ = cache.InvalidateValidateTruth(ctx, tenantID, leaseID)
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
		s.clearSelectableProxies()
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
		s.clearSelectableProxies()
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
		s.clearSelectableProxies()
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
	if proxy.Endpoint == "" || !s.safeProxyEndpoint(ctx, proxy.Endpoint) {
		return domain.Proxy{}, unsafeDestinationErr(ctx)
	}
	out, err := s.store.UpsertProxy(ctx, proxy)
	if err == nil {
		_ = s.cache.InvalidateCatalog(ctx)
		s.clearSelectableProxies()
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
	if proxy.Endpoint == "" || !s.safeProxyEndpoint(ctx, proxy.Endpoint) {
		return domain.Proxy{}, unsafeDestinationErr(ctx)
	}
	out, err := s.store.UpsertProxy(ctx, proxy)
	if err == nil {
		_ = s.cache.InvalidateCatalog(ctx)
		s.clearSelectableProxies()
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
		s.clearSelectableProxies()
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
		s.clearSelectableProxies()
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
		s.clearDefaultPolicy()
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
		s.clearDefaultPolicy()
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
		s.clearDefaultPolicy()
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
	policy, err := s.defaultPolicyForCreate(ctx)
	if err != nil {
		return domain.Policy{}, err
	}
	if !policy.Enabled {
		return domain.Policy{}, domain.ErrPolicyDenied
	}
	return policy, nil
}

func (s *Service) defaultPolicyForCreate(ctx context.Context) (domain.Policy, error) {
	ttl := s.defaultPolicyTTL
	if ttl <= 0 {
		return s.store.GetPolicy(ctx, "default")
	}
	now := s.now()
	if entry, ok := s.lookupDefaultPolicy(now); ok {
		return entry.policy, nil
	}
	for {
		call, owner := s.beginDefaultPolicyLookup(now)
		if !owner {
			if call.done == nil {
				return call.policy, call.err
			}
			select {
			case <-call.done:
				return call.policy, call.err
			case <-ctx.Done():
				return domain.Policy{}, ctx.Err()
			}
		}
		policy, err := s.store.GetPolicy(ctx, "default")
		s.finishDefaultPolicyLookup(now, call, policy, err)
		return policy, err
	}
}

func (s *Service) lookupDefaultPolicy(now time.Time) (policyEntry, bool) {
	if s.defaultPolicyTTL <= 0 {
		return policyEntry{}, false
	}
	s.defaultPolicyMu.RLock()
	entry := s.defaultPolicy
	s.defaultPolicyMu.RUnlock()
	if entry.policy.ID == "" || !entry.expiresAt.After(now) {
		return policyEntry{}, false
	}
	return entry, true
}

func (s *Service) beginDefaultPolicyLookup(now time.Time) (*policyCall, bool) {
	s.defaultPolicyMu.Lock()
	defer s.defaultPolicyMu.Unlock()
	if entry := s.defaultPolicy; entry.policy.ID != "" && entry.expiresAt.After(now) {
		return &policyCall{policy: entry.policy}, false
	}
	if call := s.defaultPolicyCall; call != nil && call.epoch == s.defaultPolicyEpoch {
		return call, false
	}
	call := &policyCall{done: make(chan struct{}), epoch: s.defaultPolicyEpoch}
	s.defaultPolicyCall = call
	return call, true
}

func (s *Service) finishDefaultPolicyLookup(now time.Time, call *policyCall, policy domain.Policy, err error) {
	s.defaultPolicyMu.Lock()
	call.policy = policy
	call.err = err
	if err == nil && s.defaultPolicyEpoch == call.epoch {
		s.defaultPolicy = policyEntry{policy: policy, expiresAt: now.Add(s.defaultPolicyTTL)}
	}
	if s.defaultPolicyCall == call {
		s.defaultPolicyCall = nil
	}
	close(call.done)
	s.defaultPolicyMu.Unlock()
}

func (s *Service) clearDefaultPolicy() {
	s.defaultPolicyMu.Lock()
	s.defaultPolicyEpoch++
	s.defaultPolicy = policyEntry{}
	s.defaultPolicyMu.Unlock()
}

func (s *Service) listSelectableProxies(ctx context.Context) ([]domain.Proxy, error) {
	ttl := s.selectableProxyTTL
	if ttl <= 0 {
		return s.store.ListSelectableProxies(ctx)
	}
	now := s.now()
	if entry, ok := s.lookupSelectableProxies(now); ok {
		return entry.proxies, nil
	}
	for {
		call, owner := s.beginSelectableProxyLookup(now)
		if !owner {
			if call.done == nil {
				return copyProxies(call.proxies), call.err
			}
			select {
			case <-call.done:
				return copyProxies(call.proxies), call.err
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		proxies, err := s.store.ListSelectableProxies(ctx)
		s.finishSelectableProxyLookup(now, call, proxies, err)
		return copyProxies(proxies), err
	}
}

func (s *Service) lookupSelectableProxies(now time.Time) (selectableProxyEntry, bool) {
	if s.selectableProxyTTL <= 0 {
		return selectableProxyEntry{}, false
	}
	s.selectableProxyMu.RLock()
	entry := s.selectableProxyEntry
	s.selectableProxyMu.RUnlock()
	if len(entry.proxies) == 0 || !entry.expiresAt.After(now) {
		return selectableProxyEntry{}, false
	}
	return selectableProxyEntry{proxies: copyProxies(entry.proxies), expiresAt: entry.expiresAt}, true
}

func (s *Service) beginSelectableProxyLookup(now time.Time) (*selectableProxyCall, bool) {
	s.selectableProxyMu.Lock()
	defer s.selectableProxyMu.Unlock()
	if entry := s.selectableProxyEntry; len(entry.proxies) > 0 && entry.expiresAt.After(now) {
		return &selectableProxyCall{proxies: copyProxies(entry.proxies)}, false
	}
	if call := s.selectableProxyCall; call != nil && call.epoch == s.selectableProxyEpoch {
		return call, false
	}
	call := &selectableProxyCall{done: make(chan struct{}), epoch: s.selectableProxyEpoch}
	s.selectableProxyCall = call
	return call, true
}

func (s *Service) finishSelectableProxyLookup(now time.Time, call *selectableProxyCall, proxies []domain.Proxy, err error) {
	s.selectableProxyMu.Lock()
	call.proxies = copyProxies(proxies)
	call.err = err
	if err == nil && len(proxies) > 0 && s.selectableProxyEpoch == call.epoch {
		s.selectableProxyEntry = selectableProxyEntry{proxies: copyProxies(proxies), expiresAt: now.Add(s.selectableProxyTTL)}
	}
	if s.selectableProxyCall == call {
		s.selectableProxyCall = nil
	}
	close(call.done)
	s.selectableProxyMu.Unlock()
}

func (s *Service) clearSelectableProxies() {
	s.selectableProxyMu.Lock()
	s.selectableProxyEpoch++
	s.selectableProxyEntry = selectableProxyEntry{}
	s.selectableProxyMu.Unlock()
}

func copyProxies(in []domain.Proxy) []domain.Proxy {
	if len(in) == 0 {
		return nil
	}
	out := make([]domain.Proxy, len(in))
	copy(out, in)
	return out
}

func (s *Service) selectLeaseProxy(ctx context.Context, tenantID string, candidates []domain.Proxy) (domain.Proxy, error) {
	started := time.Now()
	selectorCtx := ctx
	cancel := func() {}
	if timeout := s.selectorTimeout; timeout > 0 && s.selectorMode != selector.NameLocal {
		selectorCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	proxy, err := s.selector.Select(selectorCtx, candidates, selector.SelectOptions{AffinityPolicy: selector.PolicyNone})
	cancel()
	selectorLatency := float64(time.Since(started).Milliseconds())
	metrics.SelectorLatencyMS.Observe(selectorLatency)
	s.observeSelectorLatency(selectorLatency)
	if err == nil {
		s.incrementSelectorSelected()
		return proxy, nil
	}
	if selectorCtx.Err() != nil && ctx.Err() == nil {
		err = domain.NewKindedError(domain.ErrNoHealthyProxy, domain.ErrorKindSelectorRedis, "selector_timeout", err)
	}

	s.logSelectorError(tenantID, len(candidates), err)
	metrics.SelectorErrors.Inc()
	s.incrementSelectorError(err)
	if !s.shouldFallbackToLocalSelector(err) {
		return domain.Proxy{}, err
	}

	proxy, fallbackErr := s.localSelector.Select(ctx, candidates, selector.SelectOptions{AffinityPolicy: selector.PolicyNone})
	if fallbackErr != nil {
		return domain.Proxy{}, fallbackErr
	}
	s.logSelectorFallback(tenantID, len(candidates), err, proxy.ID)
	return proxy, nil
}

func (s *Service) shouldFallbackToLocalSelector(err error) bool {
	if s.selectorMode == selector.NameLocal || s.localSelector == nil {
		return false
	}
	switch domain.ErrorKindOf(err) {
	case domain.ErrorKindSelectorRedis,
		domain.ErrorKindSelectorEmptyResult,
		domain.ErrorKindSelectorMalformedResult,
		domain.ErrorKindSelectorStaleResult,
		domain.ErrorKindSelectorReadyRebuild:
		return true
	default:
		return false
	}
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

func (s *Service) logSelectorFallback(tenantID string, candidateCount int, cause error, proxyID string) {
	logger := s.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("selector.select.fallback_local",
		"tenant_present", tenantID != "",
		"candidate_count", candidateCount,
		"error", domain.ErrorCode(cause),
		"error_kind", string(domain.ErrorKindOf(cause)),
		"reason", domain.ErrorReason(cause),
		"proxy_id", proxyID,
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

func (s *Service) safeResource(ctx context.Context, resource domain.ResourceRef) bool {
	return s.safeTarget(ctx, resource.ID)
}

func resourceMatchesTarget(resource domain.ResourceRef, target string) bool {
	return extractHost(resource.ID) == extractHost(target)
}

func (s *Service) safeTarget(ctx context.Context, target string) bool {
	host := extractHost(target)
	return s.isSafeHost(ctx, host)
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
// rechecked on every new request batch so DNS rebinding cannot ride a cached
// positive decision. Concurrent lookups of the same hostname are coalesced so
// hot-path traffic does not fan out into one DNS request per goroutine. Public
// IP literals skip the resolver entirely.
func (s *Service) isSafeHost(ctx context.Context, host string) bool {
	if ctx == nil {
		ctx = context.Background()
	}
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
	for {
		call, owner := s.beginHostSafetyLookup(host)
		if !owner {
			select {
			case <-call.done:
				if call.retry && ctx.Err() == nil {
					continue
				}
				return call.safe
			case <-ctx.Done():
				return false
			}
		}
		safe, retry := s.resolveHostSafety(ctx, host)
		s.finishHostSafetyLookup(host, call, safe, retry)
		return safe
	}
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

func (s *Service) beginHostSafetyLookup(host string) (*hostSafetyCall, bool) {
	s.hostSafetyMu.Lock()
	defer s.hostSafetyMu.Unlock()
	if call, ok := s.hostSafetyCalls[host]; ok {
		return call, false
	}
	call := &hostSafetyCall{done: make(chan struct{})}
	s.hostSafetyCalls[host] = call
	return call, true
}

func (s *Service) finishHostSafetyLookup(host string, call *hostSafetyCall, safe, retry bool) {
	s.hostSafetyMu.Lock()
	call.safe = safe
	call.retry = retry
	delete(s.hostSafetyCalls, host)
	close(call.done)
	s.hostSafetyMu.Unlock()
}

func (s *Service) resolveHostSafety(ctx context.Context, host string) (bool, bool) {
	resolver := s.resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	lookupCtx := ctx
	cancel := func() {}
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 2*time.Second {
		lookupCtx, cancel = context.WithTimeout(ctx, 2*time.Second)
	}
	defer cancel()
	ips, err := resolver.LookupIP(lookupCtx, "ip", host)
	if ctx.Err() != nil {
		return false, true
	}
	if err != nil || len(ips) == 0 {
		return false, false
	}
	for _, ip := range ips {
		if isInternalIP(ip) {
			s.storeHostSafety(host, false)
			return false, false
		}
	}
	return true, false
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
		if v4[0] == 0 {
			return true
		}
		if v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
			return true
		}
		if v4.Equal(net.IPv4(169, 254, 169, 254)) {
			return true
		}
		if v4[0] == 198 && (v4[1] == 18 || v4[1] == 19) {
			return true
		}
		if v4[0] >= 240 {
			return true
		}
	}
	return false
}

func (s *Service) safeProxyEndpoint(ctx context.Context, endpoint string) bool {
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
	return s.isSafeHost(ctx, strings.ToLower(u.Hostname()))
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
