package auth

import (
	"crypto/sha256"
	"encoding/hex"
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

// AuthMode reports the authentication strategy in effect.
type AuthMode string

const (
	// ModeLegacy: a single shared key proves possession; tenant id is taken
	// from the ProxyHarbor-Tenant header / tenant_id query (client-asserted).
	// Preserved for backward compatibility with v0.1.3 deployments.
	ModeLegacy AuthMode = "legacy-single-key"

	// ModeTenantKeys: the presented key is reverse-looked up to its tenant.
	// Client-asserted tenant headers may not override the bound tenant.
	ModeTenantKeys AuthMode = "tenant-keys"
)

// Authenticator validates incoming control-plane requests and returns a
// trusted Principal whose TenantID is server-decided when ModeTenantKeys is
// active.
type Authenticator struct {
	mode        AuthMode
	legacyKey   string
	keyToTenant map[string]string
	tenants     map[string]struct{}
}

// New constructs a legacy single-key authenticator. Use NewWithTenantKeys for
// strict tenant-bound auth.
func New(legacyKey string) *Authenticator {
	return &Authenticator{mode: ModeLegacy, legacyKey: legacyKey}
}

// NewWithTenantKeys constructs an authenticator that maps each presented key
// to its bound tenant. The provided map MUST already be validated (no empty
// keys/values, no duplicates) by the caller (config layer).
func NewWithTenantKeys(tenantKeys map[string]string) *Authenticator {
	cloned := make(map[string]string, len(tenantKeys))
	tenants := make(map[string]struct{}, len(tenantKeys))
	for k, t := range tenantKeys {
		cloned[k] = t
		tenants[t] = struct{}{}
	}
	return &Authenticator{mode: ModeTenantKeys, keyToTenant: cloned, tenants: tenants}
}

// Mode returns the active authentication mode.
func (a *Authenticator) Mode() AuthMode {
	if a == nil {
		return ModeLegacy
	}
	return a.mode
}

// Tenants returns the set of tenants known to the authenticator (only
// meaningful in tenant-keys mode).
func (a *Authenticator) Tenants() []string {
	if a == nil {
		return nil
	}
	out := make([]string, 0, len(a.tenants))
	for t := range a.tenants {
		out = append(out, t)
	}
	return out
}

func (a *Authenticator) Authenticate(r *http.Request) (domain.Principal, error) {
	if a == nil {
		return domain.Principal{}, domain.ErrAuthFailed
	}
	presented := r.Header.Get(HeaderName)
	if presented == "" {
		return domain.Principal{}, domain.ErrAuthFailed
	}

	switch a.mode {
	case ModeTenantKeys:
		tenantID, ok := a.keyToTenant[presented]
		if !ok {
			return domain.Principal{}, domain.ErrAuthFailed
		}
		// If the client also asserts a tenant, it must match the bound one.
		// Mismatch is an authorization error (403), not authentication (401).
		if claimed := strings.TrimSpace(r.Header.Get(TenantHeaderName)); claimed != "" && claimed != tenantID {
			return domain.Principal{}, domain.ErrTenantMismatch
		}
		if claimed := strings.TrimSpace(r.URL.Query().Get(TenantQueryName)); claimed != "" && claimed != tenantID {
			return domain.Principal{}, domain.ErrTenantMismatch
		}
		return domain.Principal{
			ID:       "tenant-key:" + Fingerprint(presented),
			Type:     "tenant_key",
			TenantID: tenantID,
		}, nil

	default: // ModeLegacy
		if a.legacyKey == "" || presented != a.legacyKey {
			return domain.Principal{}, domain.ErrAuthFailed
		}
		tenantID, ok := ResolveTenantID(r)
		if !ok {
			return domain.Principal{}, domain.ErrAuthFailed
		}
		return domain.Principal{ID: "configured-key", Type: "header_key", TenantID: tenantID}, nil
	}
}

// Fingerprint returns the first 8 hex chars of sha256(key). It is suitable
// for log correlation only; do NOT use it for authentication or reverse
// lookup.
func Fingerprint(key string) string {
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:8]
}

func TenantIDFromRequest(r *http.Request) string {
	tenantID, _ := ResolveTenantID(r)
	return tenantID
}

// ResolveTenantID extracts a client-asserted tenant id. In tenant-keys mode
// the result is informational only; the authoritative tenant comes from the
// authenticated principal.
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
