package proxyharbor

import (
	"context"
	"net/url"
	"sync"
	"time"
)

// GetProxyOption customises a single GetProxy / GetProxyURL call.
type GetProxyOption func(*getProxyOptions)

type getProxyOptions struct {
	key       string
	target    string
	policyID  string
	ttl       time.Duration
	subjectID string
	labels    map[string]string
	force     bool
}

// WithKey sets the sticky lease key.
func WithKey(k string) GetProxyOption { return func(o *getProxyOptions) { o.key = k } }

// WithTarget overrides the resource target sent with the lease request.
//
// ProxyHarbor rejects unsafe targets (private IPs, localhost, ...). When
// omitted the SDK falls back to Config.DefaultTarget.
func WithTarget(t string) GetProxyOption { return func(o *getProxyOptions) { o.target = t } }

// WithPolicyID requests a specific policy on the lease.
func WithPolicyID(id string) GetProxyOption { return func(o *getProxyOptions) { o.policyID = id } }

// WithTTL requests a specific lease TTL.
func WithTTL(d time.Duration) GetProxyOption { return func(o *getProxyOptions) { o.ttl = d } }

// WithSubjectID overrides the subject ID used for lease creation.
func WithSubjectID(id string) GetProxyOption { return func(o *getProxyOptions) { o.subjectID = id } }

// WithSubjectLabels attaches labels to the subject of the lease request.
func WithSubjectLabels(l map[string]string) GetProxyOption {
	return func(o *getProxyOptions) { o.labels = l }
}

// WithForceNew bypasses the keyed lease cache and always creates a new lease.
func WithForceNew() GetProxyOption { return func(o *getProxyOptions) { o.force = true } }

// ReleaseOption configures a Release call.
type ReleaseOption func(*releaseOptions)

type releaseOptions struct {
	key     string
	leaseID string
}

// WithReleaseKey selects the key whose lease should be released.
func WithReleaseKey(k string) ReleaseOption { return func(o *releaseOptions) { o.key = k } }

// WithReleaseLeaseID releases a specific lease ID directly.
func WithReleaseLeaseID(id string) ReleaseOption {
	return func(o *releaseOptions) { o.leaseID = id }
}

// leaseEntry holds the cached lease state for a single key. Its mutex is
// only used to publish lease updates; serialisation of create/renew is done
// by the per-key mutex on the client.
type leaseEntry struct {
	mu    sync.Mutex
	lease leaseDTO
}

// leasesAPI exposes the lower-level lease HTTP API for advanced users.
type leasesAPI struct{ c *Client }

// GetProxy returns a Proxy bound to the requested key, creating, reusing,
// renewing, or recreating the underlying lease as needed.
func (c *Client) GetProxy(ctx context.Context, opts ...GetProxyOption) (Proxy, error) {
	o := c.resolveGetOptions(opts)
	lease, err := c.acquireLease(ctx, o)
	if err != nil {
		return Proxy{}, err
	}
	return c.toProxy(o.key, lease), nil
}

// GetProxyURL is a convenience wrapper around GetProxy that returns just the
// gateway URL with embedded credentials.
func (c *Client) GetProxyURL(ctx context.Context, opts ...GetProxyOption) (string, error) {
	p, err := c.GetProxy(ctx, opts...)
	if err != nil {
		return "", err
	}
	return p.URL, nil
}

// Release explicitly revokes the lease for a given key (default key when
// none is provided) or for an explicit lease ID.
func (c *Client) Release(ctx context.Context, opts ...ReleaseOption) error {
	o := releaseOptions{}
	for _, opt := range opts {
		opt(&o)
	}
	if o.leaseID != "" {
		return c.Leases.Revoke(ctx, o.leaseID)
	}
	if o.key == "" {
		o.key = c.cfg.DefaultKey
	}
	if v, ok := c.leases.LoadAndDelete(o.key); ok {
		entry := v.(*leaseEntry)
		if entry.lease.LeaseID != "" {
			return c.Leases.Revoke(ctx, entry.lease.LeaseID)
		}
	}
	return nil
}

// --- internals ---

func (c *Client) resolveGetOptions(opts []GetProxyOption) getProxyOptions {
	o := getProxyOptions{}
	for _, opt := range opts {
		opt(&o)
	}
	if o.key == "" {
		o.key = c.cfg.DefaultKey
	}
	if o.target == "" {
		o.target = c.cfg.DefaultTarget
	}
	if o.subjectID == "" {
		o.subjectID = o.key
	}
	return o
}

// acquireLease implements the sec-5 decision tree:
// reuse > renew > recreate, with a per-key mutex for write paths.
func (c *Client) acquireLease(ctx context.Context, o getProxyOptions) (leaseDTO, error) {
	if !o.force {
		if lease, ok := c.cachedLease(o.key); ok && c.leaseUsable(lease) {
			return lease, nil
		}
	}

	mu := c.keyMutex(o.key)
	mu.Lock()
	defer mu.Unlock()

	if !o.force {
		if lease, ok := c.cachedLease(o.key); ok {
			if c.leaseUsable(lease) {
				return lease, nil
			}
			if c.leaseShouldRenew(lease) && c.cfg.LeasePolicy.AutoRenew {
				if renewed, err := c.tryRenew(ctx, o.key); err == nil {
					return renewed, nil
				}
			}
		}
	}

	if !c.cfg.LeasePolicy.AutoReacquire {
		if lease, ok := c.cachedLease(o.key); ok && lease.LeaseID != "" && !c.leaseUsable(lease) {
			return leaseDTO{}, ErrLeaseExpired
		}
	}

	lease, err := c.Leases.Create(ctx, c.buildCreateRequest(o))
	if err != nil {
		return leaseDTO{}, err
	}
	c.storeLease(o.key, lease)
	return lease, nil
}

func (c *Client) cachedLease(key string) (leaseDTO, bool) {
	v, ok := c.leases.Load(key)
	if !ok {
		return leaseDTO{}, false
	}
	entry := v.(*leaseEntry)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return entry.lease, entry.lease.LeaseID != ""
}

func (c *Client) tryRenew(ctx context.Context, key string) (leaseDTO, error) {
	v, ok := c.leases.Load(key)
	if !ok {
		return leaseDTO{}, ErrLeaseExpired
	}
	entry := v.(*leaseEntry)
	entry.mu.Lock()
	id := entry.lease.LeaseID
	prev := entry.lease
	entry.mu.Unlock()
	if id == "" {
		return leaseDTO{}, ErrLeaseExpired
	}
	renewed, err := c.Leases.Renew(ctx, id)
	if err != nil {
		return leaseDTO{}, err
	}
	// Renew responses do not re-issue the password; preserve from previous.
	if renewed.Password == "" {
		renewed.Password = prev.Password
	}
	if renewed.Username == "" {
		renewed.Username = prev.Username
	}
	if renewed.GatewayURL == "" {
		renewed.GatewayURL = prev.GatewayURL
	}
	c.storeLease(key, renewed)
	return renewed, nil
}

func (c *Client) leaseUsable(l leaseDTO) bool {
	if l.LeaseID == "" {
		return false
	}
	if l.ExpiresAt.IsZero() {
		return true
	}
	return time.Until(l.ExpiresAt) > c.cfg.LeasePolicy.RenewSkew
}

func (c *Client) leaseShouldRenew(l leaseDTO) bool {
	if l.LeaseID == "" || l.ExpiresAt.IsZero() {
		return false
	}
	remaining := time.Until(l.ExpiresAt)
	if remaining <= 0 {
		// Already expired: skip renew, fall through to reacquire.
		return false
	}
	if !l.RenewBefore.IsZero() {
		return time.Now().After(l.RenewBefore)
	}
	return remaining <= c.cfg.LeasePolicy.RenewSkew
}

func (c *Client) storeLease(key string, l leaseDTO) {
	v, _ := c.leases.LoadOrStore(key, &leaseEntry{})
	entry := v.(*leaseEntry)
	entry.mu.Lock()
	entry.lease = l
	entry.mu.Unlock()
}

func (c *Client) buildCreateRequest(o getProxyOptions) createLeaseRequest {
	req := createLeaseRequest{
		Subject: subjectDTO{
			Type:   defaultLeaseSubjectType,
			ID:     o.subjectID,
			Labels: o.labels,
		},
		ResourceRef: resourceRefDTO{
			Kind: defaultLeaseResourceKind,
			ID:   o.target,
		},
		PolicyID: o.policyID,
	}
	if o.ttl > 0 {
		req.TTLSeconds = int64(o.ttl / time.Second)
	}
	return req
}

func (c *Client) toProxy(key string, l leaseDTO) Proxy {
	return Proxy{
		URL:       buildGatewayURL(l),
		Key:       key,
		LeaseID:   l.LeaseID,
		ProxyID:   l.ProxyID,
		ExpiresAt: l.ExpiresAt,
	}
}

// buildGatewayURL embeds the lease credentials into the gateway URL.
func buildGatewayURL(l leaseDTO) string {
	if l.GatewayURL == "" {
		return ""
	}
	if l.Username == "" && l.Password == "" {
		return l.GatewayURL
	}
	u, err := url.Parse(l.GatewayURL)
	if err != nil {
		return l.GatewayURL
	}
	u.User = url.UserPassword(l.Username, l.Password)
	return u.String()
}

// --- low-level lease API ---

// Create creates a lease using the supplied request. Idempotency-Key is set
// automatically.
func (a *leasesAPI) Create(ctx context.Context, req createLeaseRequest) (leaseDTO, error) {
	var out leaseDTO
	err := a.c.do(ctx, requestSpec{
		method:      "POST",
		path:        "/v1/leases",
		body:        req,
		auth:        authTenant,
		idempotency: newIdempotencyKey(),
		out:         &out,
	})
	return out, err
}

// Renew extends an existing lease.
func (a *leasesAPI) Renew(ctx context.Context, leaseID string) (leaseDTO, error) {
	var out leaseDTO
	err := a.c.do(ctx, requestSpec{
		method: "POST",
		path:   "/v1/leases/" + leaseID + ":renew",
		auth:   authTenant,
		out:    &out,
	})
	return out, err
}

// Revoke deletes a lease.
func (a *leasesAPI) Revoke(ctx context.Context, leaseID string) error {
	return a.c.do(ctx, requestSpec{
		method: "DELETE",
		path:   "/v1/leases/" + leaseID,
		auth:   authTenant,
	})
}
