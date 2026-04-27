package proxyharbor

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Candidate is a proxy discovered by a miner or crawler.
type Candidate struct {
	Endpoint string
	Labels   map[string]string
}

// Miner discovers proxy candidates and sends them to the provided sink.
type Miner interface {
	Mine(context.Context, Sink) error
}

// MinerFunc adapts a function into a Miner.
type MinerFunc func(context.Context, Sink) error

// Mine calls f(ctx, sink).
func (f MinerFunc) Mine(ctx context.Context, sink Sink) error { return f(ctx, sink) }

// Sink receives discovered proxy candidates.
type Sink interface {
	Submit(context.Context, Candidate) error
}

// Pool is a minimal mining proxy pool backed by ProxyHarbor.
//
// Submitted candidates are normalized and automatically inserted through the
// default provider route unless DefaultProviderID is overridden in Config.
type Pool struct {
	client *Client
	miner  Miner
}

// NewPool constructs a Pool around an existing Client.
func NewPool(client *Client, miner Miner) (*Pool, error) {
	if client == nil {
		return nil, ErrNilClient
	}
	if miner == nil {
		return nil, ErrNilMiner
	}
	return &Pool{client: client, miner: miner}, nil
}

// Run starts the miner and stores each discovered proxy via the default route.
func (p *Pool) Run(ctx context.Context) error {
	if p == nil || p.client == nil {
		return ErrNilClient
	}
	if p.miner == nil {
		return ErrNilMiner
	}
	return p.miner.Mine(ctx, p)
}

// Submit validates a discovered candidate and inserts it into ProxyHarbor.
func (p *Pool) Submit(ctx context.Context, candidate Candidate) error {
	if p == nil || p.client == nil {
		return ErrNilClient
	}
	endpoint, err := NormalizeProxyEndpoint(candidate.Endpoint)
	if err != nil {
		return err
	}
	proxy := ProxyDTO{
		ProviderID: p.client.cfg.DefaultProviderID,
		Endpoint:   endpoint,
		Healthy:    true,
		Weight:     1,
		Labels:     cloneLabels(candidate.Labels),
	}
	_, err = p.client.Proxies.Upsert(ctx, proxy)
	return err
}

// Harvest runs a miner with the default client created from environment vars.
func Harvest(ctx context.Context, miner Miner) error {
	client, err := Default()
	if err != nil {
		return err
	}
	pool, err := NewPool(client, miner)
	if err != nil {
		return err
	}
	return pool.Run(ctx)
}

// NormalizeProxyEndpoint accepts common mined endpoint formats and returns a
// ProxyHarbor-safe endpoint. Bare host:port values are treated as HTTP proxies.
func NormalizeProxyEndpoint(endpoint string) (string, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "", ErrInvalidProxyEndpoint
	}
	if !strings.Contains(endpoint, "://") {
		endpoint = "http://" + endpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", ErrInvalidProxyEndpoint
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5":
	default:
		return "", ErrInvalidProxyEndpoint
	}
	host := u.Hostname()
	port := u.Port()
	if host == "" || port == "" {
		return "", ErrInvalidProxyEndpoint
	}
	if _, err := net.LookupPort("tcp", port); err != nil {
		return "", ErrInvalidProxyEndpoint
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = net.JoinHostPort(host, port)
	u.Path = ""
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

// StaticMiner is a simple in-memory miner useful for tests and small crawlers.
type StaticMiner struct {
	Candidates []Candidate
}

// Mine submits all configured candidates until the context is cancelled or a
// candidate fails validation/storage.
func (m StaticMiner) Mine(ctx context.Context, sink Sink) error {
	for _, candidate := range m.Candidates {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := sink.Submit(ctx, candidate); err != nil {
			return err
		}
	}
	return nil
}

// ChannelMiner consumes candidates from a channel until it is closed.
type ChannelMiner struct {
	Candidates <-chan Candidate
}

// Mine submits candidates from the channel until close or cancellation.
func (m ChannelMiner) Mine(ctx context.Context, sink Sink) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case candidate, ok := <-m.Candidates:
			if !ok {
				return nil
			}
			if err := sink.Submit(ctx, candidate); err != nil {
				return err
			}
		}
	}
}

// Deduper wraps a Sink and drops duplicate normalized endpoints.
type Deduper struct {
	Next Sink

	mu   sync.Mutex
	seen map[string]struct{}
}

// Submit forwards the candidate once for each normalized endpoint.
func (d *Deduper) Submit(ctx context.Context, candidate Candidate) error {
	if d == nil || d.Next == nil {
		return ErrNilSink
	}
	endpoint, err := NormalizeProxyEndpoint(candidate.Endpoint)
	if err != nil {
		return err
	}
	d.mu.Lock()
	if d.seen == nil {
		d.seen = make(map[string]struct{})
	}
	if _, ok := d.seen[endpoint]; ok {
		d.mu.Unlock()
		return nil
	}
	d.seen[endpoint] = struct{}{}
	d.mu.Unlock()
	candidate.Endpoint = endpoint
	return d.Next.Submit(ctx, candidate)
}

// IntervalMiner repeatedly runs another miner on a fixed interval.
type IntervalMiner struct {
	Miner    Miner
	Interval time.Duration
}

// Mine runs Miner immediately and then after each interval until cancelled.
func (m IntervalMiner) Mine(ctx context.Context, sink Sink) error {
	if m.Miner == nil {
		return ErrNilMiner
	}
	interval := m.Interval
	if interval <= 0 {
		return m.Miner.Mine(ctx, sink)
	}
	for {
		if err := m.Miner.Mine(ctx, sink); err != nil {
			return err
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

var (
	// ErrNilClient means a Pool was created or used without a Client.
	ErrNilClient = errors.New("proxyharbor: nil client")
	// ErrNilMiner means a Pool was created or used without a Miner.
	ErrNilMiner = errors.New("proxyharbor: nil miner")
	// ErrNilSink means a helper miner/sink was used without a destination sink.
	ErrNilSink = errors.New("proxyharbor: nil sink")
	// ErrInvalidProxyEndpoint means a mined endpoint cannot be stored safely.
	ErrInvalidProxyEndpoint = errors.New("proxyharbor: invalid proxy endpoint")
)

func cloneLabels(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	out := make(map[string]string, len(labels))
	for key, value := range labels {
		out[key] = value
	}
	return out
}
