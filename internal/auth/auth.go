package auth

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/kamill7779/proxyharbor/internal/shared/domain"
)

// TenantKey represents a persisted tenant API key (plaintext never stored).
// Retained for compatibility with admin_handler.go (Milestone B).
type TenantKey struct {
	ID        string
	TenantID  string
	KeyHash   string // sha256(pepper || key) as hex
	KeyFP     string // sha256(key)[:8] as hex
	Label     string
	Purpose   string
	CreatedBy string
	CreatedAt time.Time
	ExpiresAt *time.Time
	RevokedAt *time.Time
}

const (
	HeaderName      = "ProxyHarbor-Key"
	TenantQueryName = "tenant_id"
	DefaultTenantID = "default"
)

// TenantHeaderName is used by admin keys to assert which tenant they act on behalf of.
const TenantHeaderName = "X-On-Behalf-Of"

// Fingerprint returns the first 8 hex chars of SHA-256 of the raw key.
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

// Authenticator supports three auth modes plus an optional admin key.
type Authenticator struct {
	mode     AuthMode
	legacy   string            // ModeLegacy
	static   map[string]string // ModeTenantKeys: key -> tenantID
	dynamic  *DynamicStore     // ModeDynamicKeys
	adminKey string
}

// New creates a legacy authenticator (single key).
// Deprecated: use NewLegacy for explicit mode selection.
func New(key string) *Authenticator {
	return NewLegacy(key)
}

// NewLegacy creates a legacy authenticator (single key).
func NewLegacy(key string) *Authenticator {
	return &Authenticator{mode: ModeLegacy, legacy: key}
}

// NewTenantKeys creates a static tenant-key authenticator.
func NewTenantKeys(keys map[string]string) *Authenticator {
	return &Authenticator{mode: ModeTenantKeys, static: keys}
}

// NewDynamicKeys creates a dynamic-key authenticator backed by a DynamicStore.
func NewDynamicKeys(dynamic *DynamicStore) *Authenticator {
	return &Authenticator{mode: ModeDynamicKeys, dynamic: dynamic}
}

// WithAdminKey sets the admin key and returns the same Authenticator for chaining.
func (a *Authenticator) WithAdminKey(adminKey string) *Authenticator {
	a.adminKey = adminKey
	return a
}

// Authenticate checks the ProxyHarbor-Key header and returns a Principal.
func (a *Authenticator) Authenticate(r *http.Request) (domain.Principal, error) {
	if a == nil {
		return domain.Principal{}, domain.ErrAuthFailed
	}
	presented := r.Header.Get(HeaderName)
	if presented == "" {
		return domain.Principal{}, domain.ErrAuthFailed
	}

	// L0 Admin: constant-time compare, independent of mode.
	if a.adminKey != "" && subtle.ConstantTimeCompare([]byte(presented), []byte(a.adminKey)) == 1 {
		return domain.Principal{
			ID:       "admin:" + Fingerprint(presented),
			Type:     "admin",
			TenantID: "",
		}, nil
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
		if claimed := r.Header.Get(TenantHeaderName); claimed != "" && claimed != e.tenantID {
			return domain.Principal{}, domain.ErrTenantMismatch
		}
		return domain.Principal{
			ID:       "tenant-key:" + Fingerprint(presented),
			Type:     "tenant_key",
			TenantID: e.tenantID,
		}, nil
	case ModeTenantKeys:
		tenantID, ok := a.static[presented]
		if !ok {
			return domain.Principal{}, domain.ErrAuthFailed
		}
		if claimed := r.Header.Get(TenantHeaderName); claimed != "" && claimed != tenantID {
			return domain.Principal{}, domain.ErrTenantMismatch
		}
		return domain.Principal{
			ID:       "tenant-key:" + Fingerprint(presented),
			Type:     "tenant_key",
			TenantID: tenantID,
		}, nil
	case ModeLegacy:
		if a.legacy == "" {
			return domain.Principal{}, domain.ErrAuthFailed
		}
		if subtle.ConstantTimeCompare([]byte(presented), []byte(a.legacy)) != 1 {
			return domain.Principal{}, domain.ErrAuthFailed
		}
		return domain.Principal{
			ID:       "configured-key",
			Type:     "header_key",
			TenantID: "default",
		}, nil
	default:
		return domain.Principal{}, domain.ErrAuthFailed
	}
}

// ParseTenantKeys parses a comma-separated list of "tenant_id:key" pairs.
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
