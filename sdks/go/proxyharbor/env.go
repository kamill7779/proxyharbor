package proxyharbor

import (
	"net/http"
	"os"
	"time"
)

// Default values used by the SDK when nothing is configured explicitly.
const (
	defaultDefaultKey        = "default"
	defaultProviderID        = "default"
	defaultUserAgent         = "proxyharbor-go"
	defaultTimeout           = 10 * time.Second
	defaultRetryMax          = 3
	defaultRetryMinBackoff   = 100 * time.Millisecond
	defaultRetryMaxBackoff   = 2 * time.Second
	defaultRenewSkew         = 30 * time.Second
	defaultLeaseSubjectType  = "sdk-go"
	defaultLeaseResourceKind = "http"
	// defaultLeaseTarget is a public placeholder. The ProxyHarbor service
	// rejects unsafe lease targets (private IPs, localhost, ...), so we
	// use an externally-resolvable host by default. Callers can override
	// per-call with WithTarget or globally with WithDefaultTarget.
	defaultLeaseTarget = "https://example.com"
)

// Config carries explicit configuration for a Client.
//
// Empty fields fall back to the corresponding environment variables and
// finally to SDK defaults.
type Config struct {
	BaseURL           string
	AdminKey          string
	TenantKey         string
	DefaultKey        string
	DefaultProviderID string

	Timeout time.Duration
	Retry   RetryConfig

	UserAgent  string
	HTTPClient *http.Client

	LeasePolicy LeasePolicy

	// DefaultTarget is the resource target the SDK uses when GetProxy is
	// called without an explicit WithTarget option.
	DefaultTarget string
}

func envOrDefault(value, env, fallback string) string {
	if value != "" {
		return value
	}
	if v := os.Getenv(env); v != "" {
		return v
	}
	return fallback
}

func (c Config) withDefaults() Config {
	c.BaseURL = envOrDefault(c.BaseURL, "PROXYHARBOR_BASE_URL", "")
	c.AdminKey = envOrDefault(c.AdminKey, "PROXYHARBOR_ADMIN_KEY", "")
	c.TenantKey = envOrDefault(c.TenantKey, "PROXYHARBOR_TENANT_KEY", "")
	if c.DefaultKey == "" {
		c.DefaultKey = defaultDefaultKey
	}
	if c.DefaultProviderID == "" {
		c.DefaultProviderID = defaultProviderID
	}
	if c.Timeout <= 0 {
		c.Timeout = defaultTimeout
	}
	if c.Retry.MaxAttempts <= 0 {
		c.Retry.MaxAttempts = defaultRetryMax
	}
	if c.Retry.MinBackoff <= 0 {
		c.Retry.MinBackoff = defaultRetryMinBackoff
	}
	if c.Retry.MaxBackoff <= 0 {
		c.Retry.MaxBackoff = defaultRetryMaxBackoff
	}
	if c.UserAgent == "" {
		c.UserAgent = defaultUserAgent
	}
	if c.LeasePolicy.RenewSkew <= 0 {
		c.LeasePolicy.RenewSkew = defaultRenewSkew
	}
	if c.DefaultTarget == "" {
		c.DefaultTarget = defaultLeaseTarget
	}
	return c
}
