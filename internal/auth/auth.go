package auth

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/kamill7779/proxyharbor/internal/shared/domain"
)

const (
	HeaderName       = "ProxyHarbor-Key"
	TenantHeaderName = "ProxyHarbor-Tenant"
	OnBehalfOfHeader = "X-On-Behalf-Of"
)

type TenantKey struct {
	ID        string
	TenantID  string
	KeyHash   string
	KeyFP     string
	Label     string
	Purpose   string
	CreatedBy string
	CreatedAt time.Time
	ExpiresAt *time.Time
	RevokedAt *time.Time
}

type Authenticator struct {
	dynamic  *DynamicStore
	adminKey string
}

func NewDynamicKeys(dynamic *DynamicStore) *Authenticator {
	return &Authenticator{dynamic: dynamic}
}

func (a *Authenticator) WithAdminKey(adminKey string) *Authenticator {
	if a == nil {
		return nil
	}
	a.adminKey = adminKey
	return a
}

func (a *Authenticator) CacheEntries() int {
	if a == nil || a.dynamic == nil {
		return 0
	}
	return a.dynamic.Len()
}

func (a *Authenticator) DynamicStore() *DynamicStore {
	if a == nil {
		return nil
	}
	return a.dynamic
}

func (a *Authenticator) Authenticate(r *http.Request) (domain.Principal, error) {
	if a == nil || r == nil {
		return domain.Principal{}, domain.ErrAuthFailed
	}
	presented := r.Header.Get(HeaderName)
	if presented == "" {
		return domain.Principal{}, domain.ErrAuthFailed
	}
	if a.adminKey != "" && subtle.ConstantTimeCompare([]byte(presented), []byte(a.adminKey)) == 1 {
		return domain.Principal{ID: "admin:" + Fingerprint(presented), Type: "admin"}, nil
	}

	if a.dynamic == nil {
		return domain.Principal{}, domain.ErrAuthFailed
	}
	key, ok := a.dynamic.Lookup(presented)
	if !ok {
		return domain.Principal{}, domain.ErrAuthFailed
	}
	if claimed := claimedTenant(r); claimed != "" && claimed != key.tenantID {
		return domain.Principal{}, domain.ErrTenantMismatch
	}
	return domain.Principal{ID: "tenant-key:" + key.keyID, Type: "tenant_key", TenantID: key.tenantID}, nil
}

func claimedTenant(r *http.Request) string {
	if r == nil {
		return ""
	}
	if tenantID := strings.TrimSpace(r.Header.Get(OnBehalfOfHeader)); tenantID != "" {
		return tenantID
	}
	return strings.TrimSpace(r.Header.Get(TenantHeaderName))
}

func Fingerprint(key string) string {
	h := sha256Pepper(nil, key)
	return fmt.Sprintf("%x", h[:4])
}
