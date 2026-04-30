package proxyharbor

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Client is a long-lived ProxyHarbor SDK instance. It holds a keyed cache of
// leases so concurrent calls for the same key reuse the same lease.
type Client struct {
	cfg  Config
	http *http.Client

	leases sync.Map // key -> *leaseEntry

	muMu  sync.Mutex
	muMap map[string]*sync.Mutex

	// Lower-level API namespaces.
	Leases    *leasesAPI
	Providers *providersAPI
	Proxies   *proxiesAPI
	Health    *healthAPI
}

// Option configures a Client at construction time.
type Option func(*Config)

// WithBaseURL sets the ProxyHarbor base URL.
func WithBaseURL(u string) Option { return func(c *Config) { c.BaseURL = u } }

// WithTenantKey sets the tenant API key used for lease APIs.
func WithTenantKey(k string) Option { return func(c *Config) { c.TenantKey = k } }

// WithAdminKey sets the admin API key used for inventory APIs.
func WithAdminKey(k string) Option { return func(c *Config) { c.AdminKey = k } }

// WithSecretsFile loads local single-instance secrets from an env-style file.
// Explicit options and environment variables still take precedence.
func WithSecretsFile(path string) Option { return func(c *Config) { c.SecretsFile = path } }

// WithLocalDefaults enables the shortest local development path. It defaults
// the base URL to http://localhost:18080 and discovers data/secrets.env when
// explicit keys or PROXYHARBOR_SECRETS_FILE are not set.
func WithLocalDefaults() Option {
	return func(c *Config) {
		if c.BaseURL == "" {
			c.BaseURL = "http://localhost:18080"
		}
		if c.SecretsFile == "" && os.Getenv("PROXYHARBOR_SECRETS_FILE") == "" {
			for _, candidate := range localSecretsCandidates() {
				if _, err := readSecretsFile(candidate); err == nil {
					c.SecretsFile = candidate
					break
				}
			}
		}
	}
}

// WithDefaultKey overrides the sticky-lease key used when callers omit one.
func WithDefaultKey(k string) Option { return func(c *Config) { c.DefaultKey = k } }

// WithDefaultProviderID sets the provider used by default for admin calls.
func WithDefaultProviderID(id string) Option {
	return func(c *Config) { c.DefaultProviderID = id }
}

// WithTimeout sets the per-request HTTP timeout.
func WithTimeout(d time.Duration) Option { return func(c *Config) { c.Timeout = d } }

// WithRetry sets the retry behaviour for transport-level failures.
func WithRetry(r RetryConfig) Option { return func(c *Config) { c.Retry = r } }

// WithUserAgent customises the User-Agent header.
func WithUserAgent(ua string) Option { return func(c *Config) { c.UserAgent = ua } }

// WithHTTPClient lets advanced users plug in a pre-configured *http.Client.
func WithHTTPClient(h *http.Client) Option { return func(c *Config) { c.HTTPClient = h } }

// WithLeasePolicy sets the lease management policy.
func WithLeasePolicy(p LeasePolicy) Option { return func(c *Config) { c.LeasePolicy = p } }

// WithDefaultTarget sets the resource target used by GetProxy when no
// per-call WithTarget is specified.
func WithDefaultTarget(t string) Option { return func(c *Config) { c.DefaultTarget = t } }

// WithConfig replaces the entire Config (subsequent Options still apply).
func WithConfig(cfg Config) Option { return func(c *Config) { *c = cfg } }

// New constructs a Client from environment variables and the given Options.
//
// The default lease policy is {AutoRenew: true, AutoReacquire: true,
// BackgroundKeepAlive: false}; callers can override it with WithLeasePolicy.
func New(opts ...Option) (*Client, error) {
	cfg := Config{
		LeasePolicy: LeasePolicy{
			AutoRenew:           true,
			AutoReacquire:       true,
			BackgroundKeepAlive: false,
		},
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	cfg = cfg.withDefaults()
	if cfg.BaseURL == "" {
		return nil, ErrNoBaseURL
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: cfg.Timeout}
	}
	c := &Client{
		cfg:   cfg,
		http:  httpClient,
		muMap: make(map[string]*sync.Mutex),
	}
	c.Leases = &leasesAPI{c: c}
	c.Providers = &providersAPI{c: c}
	c.Proxies = &proxiesAPI{c: c}
	c.Health = &healthAPI{c: c}
	return c, nil
}

// Close releases SDK-owned resources. The current implementation does not
// hold background goroutines, so Close is a no-op for symmetry.
func (c *Client) Close(_ context.Context) error { return nil }

// Config returns a copy of the resolved client configuration.
func (c *Client) Config() Config { return c.cfg }

// keyMutex returns a per-key mutex used to serialise lease creation/renewal
// for a single key.
func (c *Client) keyMutex(key string) *sync.Mutex {
	c.muMu.Lock()
	defer c.muMu.Unlock()
	if m, ok := c.muMap[key]; ok {
		return m
	}
	m := &sync.Mutex{}
	c.muMap[key] = m
	return m
}

// --- HTTP plumbing ---

type requestSpec struct {
	method      string
	path        string
	body        any
	auth        authMode
	idempotency string
	out         any
}

func (c *Client) do(ctx context.Context, spec requestSpec) error {
	var bodyBytes []byte
	if spec.body != nil {
		b, err := json.Marshal(spec.body)
		if err != nil {
			return fmt.Errorf("proxyharbor: marshal request: %w", err)
		}
		bodyBytes = b
	}
	cfg := c.cfg.Retry
	var lastErr error
	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		err := c.doOnce(ctx, spec, bodyBytes)
		if err == nil {
			return nil
		}
		lastErr = err
		if !IsRetryable(err) || attempt == cfg.MaxAttempts {
			return err
		}
		if sleepErr := sleepCtx(ctx, backoff(attempt, cfg)); sleepErr != nil {
			return sleepErr
		}
	}
	return lastErr
}

func (c *Client) doOnce(ctx context.Context, spec requestSpec, bodyBytes []byte) error {
	var body io.Reader
	if bodyBytes != nil {
		body = bytes.NewReader(bodyBytes)
	}
	req, err := http.NewRequestWithContext(ctx, spec.method, c.cfg.BaseURL+spec.path, body)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	req.Header.Set("Accept", "application/json")
	if bodyBytes != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if spec.idempotency != "" {
		req.Header.Set("Idempotency-Key", spec.idempotency)
	}
	if err := c.applyAuth(req, spec.auth); err != nil {
		return err
	}
	if spec.auth == authTenant && c.cfg.TenantKey == "" && c.cfg.AdminKey != "" {
		req.Header.Set("X-On-Behalf-Of", "default")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if spec.out == nil || len(respBody) == 0 {
			return nil
		}
		if err := json.Unmarshal(respBody, spec.out); err != nil {
			return fmt.Errorf("proxyharbor: decode response: %w", err)
		}
		return nil
	}
	apiErr := &APIError{
		StatusCode: resp.StatusCode,
		RequestID:  resp.Header.Get("X-Request-Id"),
	}
	if len(respBody) > 0 {
		var e errorDTO
		if jerr := json.Unmarshal(respBody, &e); jerr == nil {
			apiErr.Code = e.Error
			apiErr.Message = e.Message
		} else {
			apiErr.Message = strings.TrimSpace(string(respBody))
		}
	}
	return apiErr
}

func newIdempotencyKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("ts-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
