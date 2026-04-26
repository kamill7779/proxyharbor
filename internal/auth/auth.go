package auth

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/kamill7779/proxyharbor/internal/shared/domain"
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

const (
	HeaderName       = "ProxyHarbor-Key"
	TenantHeaderName = "X-On-Behalf-Of"
	TenantQueryName  = "tenant_id"
	DefaultTenantID  = "default"
)

func Fingerprint(key string) string {
	h := sha256Pepper(nil, key)
	return fmt.Sprintf("%x", h[:4])
}

type AuthMode string

const (
	ModeLegacy      AuthMode = "legacy-single-key"
	ModeTenantKeys  AuthMode = "tenant-keys"
	ModeDynamicKeys AuthMode = "dynamic-keys"
)

type Authenticator struct {
	mode     AuthMode
	legacy   string
	static   map[string]string
	dynamic  *DynamicStore
	adminKey string
}

func New(key string) *Authenticator { return NewLegacy(key) }

func NewLegacy(key string) *Authenticator { return &Authenticator{mode: ModeLegacy, legacy: key} }

func NewTenantKeys(keys map[string]string) *Authenticator {
	return &Authenticator{mode: ModeTenantKeys, static: keys}
}

func NewDynamicKeys(dynamic *DynamicStore) *Authenticator {
	return &Authenticator{mode: ModeDynamicKeys, dynamic: dynamic}
}

func (a *Authenticator) WithAdminKey(adminKey string) *Authenticator {
	a.adminKey = adminKey
	return a
}

func (a *Authenticator) Authenticate(r *http.Request) (domain.Principal, error) {
	if a == nil {
		return domain.Principal{}, domain.ErrAuthFailed
	}
	presented := r.Header.Get(HeaderName)
	if presented == "" {
		return domain.Principal{}, domain.ErrAuthFailed
	}
	if a.adminKey != "" && subtle.ConstantTimeCompare([]byte(presented), []byte(a.adminKey)) == 1 {
		return domain.Principal{ID: "admin:" + Fingerprint(presented), Type: "admin"}, nil
	}
	switch a.mode {
	case ModeDynamicKeys:
		if a.dynamic == nil {
			return domain.Principal{}, domain.ErrAuthFailed
		}
		e, ok := a.dynamic.Lookup(presented)
		if !ok {
			return domain.Principal{}, domain.ErrAuthFailed
		}
		if claimed := strings.TrimSpace(r.Header.Get(TenantHeaderName)); claimed != "" && claimed != e.tenantID {
			return domain.Principal{}, domain.ErrTenantMismatch
		}
		return domain.Principal{ID: "tenant-key:" + Fingerprint(presented), Type: "tenant_key", TenantID: e.tenantID}, nil
	case ModeTenantKeys:
		tenantID, ok := a.static[presented]
		if !ok {
			return domain.Principal{}, domain.ErrAuthFailed
		}
		if claimed := strings.TrimSpace(r.Header.Get(TenantHeaderName)); claimed != "" && claimed != tenantID {
			return domain.Principal{}, domain.ErrTenantMismatch
		}
		return domain.Principal{ID: "tenant-key:" + Fingerprint(presented), Type: "tenant_key", TenantID: tenantID}, nil
	case ModeLegacy:
		if a.legacy == "" || subtle.ConstantTimeCompare([]byte(presented), []byte(a.legacy)) != 1 {
			return domain.Principal{}, domain.ErrAuthFailed
		}
		tenantID, ok := ResolveTenantID(r)
		if !ok {
			return domain.Principal{}, domain.ErrAuthFailed
		}
		return domain.Principal{ID: "configured-key", Type: "header_key", TenantID: tenantID}, nil
	default:
		return domain.Principal{}, domain.ErrAuthFailed
	}
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

func ParseTenantKeys(raw string) map[string]string {
	out := make(map[string]string)
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kv := strings.SplitN(part, ":", 2)
		if len(kv) != 2 {
			continue
		}
		out[kv[1]] = strings.TrimSpace(kv[0])
	}
	return out
}
