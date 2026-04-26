package auth

import (
	"net/http"
	"strings"

	"github.com/kamill7779/proxyharbor/internal/shared/domain"
)

const (
	HeaderName       = "ProxyHarbor-Key"
	TenantHeaderName = "ProxyHarbor-Tenant"
	TenantQueryName  = "tenant_id"
	DefaultTenantID  = "default"
)

type Authenticator struct {
	Key string
}

func New(key string) *Authenticator {
	return &Authenticator{Key: key}
}

func (a *Authenticator) Authenticate(r *http.Request) (domain.Principal, error) {
	if a == nil || a.Key == "" {
		return domain.Principal{}, domain.ErrAuthFailed
	}
	if r.Header.Get(HeaderName) != a.Key {
		return domain.Principal{}, domain.ErrAuthFailed
	}
	tenantID, ok := ResolveTenantID(r)
	if !ok {
		return domain.Principal{}, domain.ErrAuthFailed
	}
	return domain.Principal{ID: "configured-key", Type: "header_key", TenantID: tenantID}, nil
}

func TenantIDFromRequest(r *http.Request) string {
	tenantID, _ := ResolveTenantID(r)
	return tenantID
}

func ResolveTenantID(r *http.Request) (string, bool) {
	if r == nil {
		return DefaultTenantID, true
	}
	if tenantID := strings.TrimSpace(r.Header.Get(TenantHeaderName)); tenantID != "" {
		return tenantID, ValidTenantID(tenantID)
	}
	if tenantID := strings.TrimSpace(r.URL.Query().Get(TenantQueryName)); tenantID != "" {
		return tenantID, ValidTenantID(tenantID)
	}
	return DefaultTenantID, true
}

func NormalizeTenantID(tenantID string) string {
	if tenantID = strings.TrimSpace(tenantID); tenantID != "" {
		return tenantID
	}
	return DefaultTenantID
}

func ValidTenantID(tenantID string) bool {
	if len(tenantID) == 0 || len(tenantID) > 64 {
		return false
	}
	for _, r := range tenantID {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}
