package control

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
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
	store                      storage.Store
	cache                      cache.Cache
	cacheTTL                   time.Duration
	now                        func() time.Time
	gatewayURL                 string
	allowInternalProxyEndpoint bool
	resolver                   *net.Resolver
}

func NewService(store storage.Store, gatewayURL string) *Service {
	return &Service{store: store, cache: cache.Noop{}, cacheTTL: time.Minute, now: time.Now, gatewayURL: gatewayURL, resolver: net.DefaultResolver}
}

// SetAllowInternalProxyEndpoint 控制上游代理 endpoint 是否允许指向 loopback/私网/link-local。
// 生产环境必须保持 false；本地 dev、单元测试可设 true。
func (s *Service) SetAllowInternalProxyEndpoint(allow bool) { s.allowInternalProxyEndpoint = allow }

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
	plaintext := "lease_" + randomHex(24)
	lease.Password = plaintext
	lease.PasswordHash = hashLeasePassword(lease.ID, plaintext)
	saved, err := s.store.CreateLease(ctx, idem, lease)
	if err != nil {
		return domain.Lease{}, err
	}
	// 幂等命中时 saved 已是历史 lease，明文必须保持为空；
	// 首次创建才将 plaintext 一次性贴回响应。
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
		remain := time.Until(lease.ExpiresAt)
		if remain > 0 {
			ttl := s.cacheTTL
			if remain < ttl {
				ttl = remain
			}
			_ = s.cache.PutLease(ctx, lease, ttl)
		}
	}
	if lease.Revoked {
		return domain.Lease{}, domain.ErrLeaseRevoked
	}
	if s.now().After(lease.ExpiresAt) {
		return domain.Lease{}, domain.ErrLeaseRevoked
	}
	if !verifyLeasePassword(lease.ID, lease.PasswordHash, password) {
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
	out, err := s.store.UpsertProvider(ctx, provider)
	if err == nil {
		_ = s.cache.InvalidateCatalog(ctx, principal.TenantID)
	}
	return out, err
}
func (s *Service) GetProvider(ctx context.Context, principal domain.Principal, id string) (domain.Provider, error) {
	return s.store.GetProvider(ctx, principal.TenantID, id)
}
func (s *Service) UpdateProvider(ctx context.Context, principal domain.Principal, id string, provider domain.Provider) (domain.Provider, error) {
	provider.ID = id
	provider.TenantID = principal.TenantID
	out, err := s.store.UpsertProvider(ctx, provider)
	if err == nil {
		_ = s.cache.InvalidateCatalog(ctx, principal.TenantID)
	}
	return out, err
}
func (s *Service) DeleteProvider(ctx context.Context, principal domain.Principal, id string) error {
	err := s.store.DeleteProvider(ctx, principal.TenantID, id)
	if err == nil {
		_ = s.cache.InvalidateCatalog(ctx, principal.TenantID)
	}
	return err
}
func (s *Service) ListProxies(ctx context.Context, tenantID string) ([]domain.Proxy, error) {
	return s.store.ListCatalogProxies(ctx, tenantID)
}
func (s *Service) CreateProxy(ctx context.Context, principal domain.Principal, proxy domain.Proxy) (domain.Proxy, error) {
	proxy.TenantID = principal.TenantID
	if proxy.Endpoint == "" || !s.safeProxyEndpoint(proxy.Endpoint) {
		return domain.Proxy{}, domain.ErrUnsafeDestination
	}
	out, err := s.store.UpsertProxy(ctx, proxy)
	if err == nil {
		_ = s.cache.InvalidateCatalog(ctx, principal.TenantID)
	}
	return out, err
}
func (s *Service) GetProxy(ctx context.Context, principal domain.Principal, id string) (domain.Proxy, error) {
	return s.store.GetProxy(ctx, principal.TenantID, id)
}
func (s *Service) UpdateProxy(ctx context.Context, principal domain.Principal, id string, proxy domain.Proxy) (domain.Proxy, error) {
	proxy.ID = id
	proxy.TenantID = principal.TenantID
	if proxy.Endpoint == "" || !s.safeProxyEndpoint(proxy.Endpoint) {
		return domain.Proxy{}, domain.ErrUnsafeDestination
	}
	out, err := s.store.UpsertProxy(ctx, proxy)
	if err == nil {
		_ = s.cache.InvalidateCatalog(ctx, principal.TenantID)
	}
	return out, err
}
func (s *Service) DeleteProxy(ctx context.Context, principal domain.Principal, id string) error {
	err := s.store.DeleteProxy(ctx, principal.TenantID, id)
	if err == nil {
		_ = s.cache.InvalidateCatalog(ctx, principal.TenantID)
	}
	return err
}
func (s *Service) UpdateProxyHealth(ctx context.Context, principal domain.Principal, id string, healthy bool, failureHint string) (domain.Proxy, error) {
	proxy, err := s.store.GetProxy(ctx, principal.TenantID, id)
	if err != nil {
		return domain.Proxy{}, err
	}
	proxy.Healthy = healthy
	proxy.FailureHint = failureHint
	proxy.LastSeenAt = s.now().UTC()
	out, err := s.store.UpsertProxy(ctx, proxy)
	if err == nil {
		_ = s.cache.InvalidateCatalog(ctx, principal.TenantID)
	}
	return out, err
}
func (s *Service) ListPolicies(ctx context.Context, tenantID string) ([]domain.Policy, error) {
	return s.store.ListPolicies(ctx, tenantID)
}
func (s *Service) CreatePolicy(ctx context.Context, principal domain.Principal, policy domain.Policy) (domain.Policy, error) {
	policy.TenantID = principal.TenantID
	out, err := s.store.UpsertPolicy(ctx, policy)
	if err == nil {
		_ = s.cache.InvalidateCatalog(ctx, principal.TenantID)
	}
	return out, err
}
func (s *Service) GetPolicy(ctx context.Context, principal domain.Principal, id string) (domain.Policy, error) {
	return s.store.GetPolicy(ctx, principal.TenantID, id)
}
func (s *Service) UpdatePolicy(ctx context.Context, principal domain.Principal, id string, policy domain.Policy) (domain.Policy, error) {
	policy.ID = id
	policy.TenantID = principal.TenantID
	out, err := s.store.UpsertPolicy(ctx, policy)
	if err == nil {
		_ = s.cache.InvalidateCatalog(ctx, principal.TenantID)
	}
	return out, err
}
func (s *Service) DeletePolicy(ctx context.Context, principal domain.Principal, id string) error {
	err := s.store.DeletePolicy(ctx, principal.TenantID, id)
	if err == nil {
		_ = s.cache.InvalidateCatalog(ctx, principal.TenantID)
	}
	return err
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

// safeTarget 拦截到入参表达的对外请求目标。对域名会解析 DNS 并对全部返回 IP 逐个检查，
// 拒绝 loopback / RFC1918 私网 / link-local / unspecified / multicast / CGNAT / cloud metadata。
// 这是针对 SSRF 和 DNS rebinding 的最低限度防御。
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
	// 域名：解析后逐个 IP 检查。解析失败也拒绝（fail-closed）。
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
	// CGNAT 100.64.0.0/10 以及常见云 metadata IP
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

// safeProxyEndpoint 同样拒绝上游代理指向内网。在 SetAllowInternalProxyEndpoint(true) 后
// 仅限 dev/测试场景下放行 loopback / 私网。
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

// hashLeasePassword 以 leaseID 为 salt 对明文计算 SHA-256，仅依赖标准库。
// Lease password 是 192-bit 随机令牌，无需额外 KDF；salt 防止跨租户 leaseID 同名碍碍。
func hashLeasePassword(leaseID, plaintext string) string {
	sum := sha256.Sum256([]byte(leaseID + ":" + plaintext))
	return hex.EncodeToString(sum[:])
}

// verifyLeasePassword 使用常量时间比较，防止计时侧信道泄露哈希。
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
