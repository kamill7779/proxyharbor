package proxyharbor

import "time"

// Proxy is the structured proxy returned by GetProxy.
//
// URL is the gateway URL the caller should plug into their HTTP client's
// proxy setting (with username/password embedded). LeaseID/ProxyID are
// returned for observability and advanced use cases.
type Proxy struct {
	URL       string
	Key       string
	LeaseID   string
	ProxyID   string
	ExpiresAt time.Time
}

// LeasePolicy controls how the SDK manages keyed leases.
type LeasePolicy struct {
	AutoRenew           bool
	AutoReacquire       bool
	BackgroundKeepAlive bool
	RenewSkew           time.Duration
}

// RetryConfig controls SDK-level transport retries.
type RetryConfig struct {
	MaxAttempts int
	MinBackoff  time.Duration
	MaxBackoff  time.Duration
}

// --- DTOs that mirror the ProxyHarbor HTTP contract ---

type subjectDTO struct {
	Type   string            `json:"subject_type"`
	ID     string            `json:"subject_id"`
	Labels map[string]string `json:"labels,omitempty"`
}

type resourceRefDTO struct {
	Kind   string            `json:"kind"`
	ID     string            `json:"id"`
	Scope  string            `json:"scope,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
}

type createLeaseRequest struct {
	Subject     subjectDTO     `json:"subject"`
	ResourceRef resourceRefDTO `json:"resource_ref"`
	PolicyID    string         `json:"policy_id,omitempty"`
	TTLSeconds  int64          `json:"ttl_seconds,omitempty"`
}

type leaseDTO struct {
	LeaseID        string    `json:"lease_id"`
	TenantID       string    `json:"tenant_id"`
	Generation     int64     `json:"lease_generation"`
	GatewayURL     string    `json:"gateway_url"`
	Username       string    `json:"username"`
	Password       string    `json:"password,omitempty"`
	ProxyID        string    `json:"proxy_id"`
	ExpiresAt      time.Time `json:"expires_at"`
	RenewBefore    time.Time `json:"renew_before"`
	CatalogVersion string    `json:"catalog_version"`
}

type ProxyDTO struct {
	ID         string            `json:"id,omitempty"`
	ProviderID string            `json:"provider_id,omitempty"`
	Endpoint   string            `json:"endpoint"`
	Healthy    bool              `json:"healthy"`
	Weight     int               `json:"weight,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
}

type ProviderDTO struct {
	ID      string            `json:"id"`
	Type    string            `json:"type,omitempty"`
	Name    string            `json:"name,omitempty"`
	Enabled bool              `json:"enabled,omitempty"`
	Labels  map[string]string `json:"labels,omitempty"`
}

type errorDTO struct {
	Error   string `json:"error,omitempty"`
	Message string `json:"message,omitempty"`
}
