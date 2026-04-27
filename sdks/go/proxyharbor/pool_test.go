package proxyharbor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestPoolRunStoresMinedProxyThroughDefaultRoute(t *testing.T) {
	var calls int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/proxies" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("ProxyHarbor-Key"); got != "admin-key" {
			t.Errorf("admin auth header = %q", got)
		}
		var req ProxyDTO
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Endpoint != "http://1.2.3.4:8080" {
			t.Errorf("endpoint = %q", req.Endpoint)
		}
		if req.ProviderID != "default" {
			t.Errorf("provider route = %q", req.ProviderID)
		}
		if !req.Healthy || req.Weight != 1 {
			t.Errorf("expected healthy weight=1, got healthy=%v weight=%d", req.Healthy, req.Weight)
		}
		if req.Labels["source"] != "unit" {
			t.Errorf("labels = %#v", req.Labels)
		}
		_ = json.NewEncoder(w).Encode(ProxyDTO{ID: "proxy-1", ProviderID: req.ProviderID, Endpoint: req.Endpoint, Healthy: req.Healthy, Weight: req.Weight, Labels: req.Labels})
	})

	pool, err := NewPool(c, StaticMiner{Candidates: []Candidate{{Endpoint: "1.2.3.4:8080", Labels: map[string]string{"source": "unit"}}}})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	if err := pool.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("expected 1 stored proxy, got %d", calls)
	}
}

func TestPoolRunUsesConfiguredProviderRoute(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var req ProxyDTO
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.ProviderID != "mined" {
			t.Errorf("provider route = %q", req.ProviderID)
		}
		_ = json.NewEncoder(w).Encode(req)
	}, WithDefaultProviderID("mined"))

	pool, err := NewPool(c, StaticMiner{Candidates: []Candidate{{Endpoint: "http://5.6.7.8:3128"}}})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	if err := pool.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestNormalizeProxyEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		want     string
		wantErr  bool
	}{
		{name: "bare host port", endpoint: "1.2.3.4:8080", want: "http://1.2.3.4:8080"},
		{name: "keeps socks5", endpoint: "socks5://127.0.0.1:1080", want: "socks5://127.0.0.1:1080"},
		{name: "strips query", endpoint: "HTTPS://example.com:443/path?q=1#x", want: "https://example.com:443"},
		{name: "rejects missing port", endpoint: "http://example.com", wantErr: true},
		{name: "rejects unsupported scheme", endpoint: "ftp://example.com:21", wantErr: true},
		{name: "rejects empty", endpoint: " ", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeProxyEndpoint(tt.endpoint)
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidProxyEndpoint) {
					t.Fatalf("expected ErrInvalidProxyEndpoint, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeProxyEndpoint: %v", err)
			}
			if got != tt.want {
				t.Errorf("endpoint = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDeduperDropsDuplicateEndpoints(t *testing.T) {
	var calls int32
	sink := SinkFunc(func(_ context.Context, candidate Candidate) error {
		atomic.AddInt32(&calls, 1)
		if candidate.Endpoint != "http://1.2.3.4:8080" {
			t.Errorf("endpoint = %q", candidate.Endpoint)
		}
		return nil
	})
	d := &Deduper{Next: sink}
	for _, endpoint := range []string{"1.2.3.4:8080", "http://1.2.3.4:8080"} {
		if err := d.Submit(context.Background(), Candidate{Endpoint: endpoint}); err != nil {
			t.Fatalf("Submit: %v", err)
		}
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("expected one forwarded candidate, got %d", calls)
	}
}

func TestChannelMinerStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	candidates := make(chan Candidate)
	cancel()
	err := ChannelMiner{Candidates: candidates}.Mine(ctx, SinkFunc(func(context.Context, Candidate) error {
		t.Fatal("sink should not be called")
		return nil
	}))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestIntervalMinerRunsOnceWithoutInterval(t *testing.T) {
	var calls int32
	err := IntervalMiner{Miner: MinerFunc(func(_ context.Context, _ Sink) error {
		atomic.AddInt32(&calls, 1)
		return nil
	})}.Mine(context.Background(), SinkFunc(func(context.Context, Candidate) error { return nil }))
	if err != nil {
		t.Fatalf("Mine: %v", err)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("expected one run, got %d", calls)
	}
}

func TestIntervalMinerReturnsCancelBetweenRuns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls int32
	err := IntervalMiner{
		Interval: time.Hour,
		Miner: MinerFunc(func(_ context.Context, _ Sink) error {
			atomic.AddInt32(&calls, 1)
			cancel()
			return nil
		}),
	}.Mine(ctx, SinkFunc(func(context.Context, Candidate) error { return nil }))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("expected one run, got %d", calls)
	}
}

type SinkFunc func(context.Context, Candidate) error

func (f SinkFunc) Submit(ctx context.Context, candidate Candidate) error {
	return f(ctx, candidate)
}
