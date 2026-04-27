package proxyharbor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(t *testing.T, h http.HandlerFunc, opts ...Option) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	base := []Option{
		WithBaseURL(srv.URL),
		WithTenantKey("tenant-key"),
		WithAdminKey("admin-key"),
		WithRetry(RetryConfig{MaxAttempts: 1, MinBackoff: time.Millisecond, MaxBackoff: time.Millisecond}),
	}
	c, err := New(append(base, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestNewRequiresBaseURL(t *testing.T) {
	t.Setenv("PROXYHARBOR_BASE_URL", "")
	if _, err := New(); !errors.Is(err, ErrNoBaseURL) {
		t.Fatalf("expected ErrNoBaseURL, got %v", err)
	}
}

func TestGetProxyURLEmbedsCredentials(t *testing.T) {
	var calls int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.URL.Path != "/v1/leases" || r.Method != http.MethodPost {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("ProxyHarbor-Key"); got != "tenant-key" {
			t.Errorf("auth header = %q", got)
		}
		if r.Header.Get("Idempotency-Key") == "" {
			t.Error("missing idempotency key")
		}
		_ = json.NewEncoder(w).Encode(leaseDTO{
			LeaseID:    "lease-1",
			Username:   "u",
			Password:   "p",
			GatewayURL: "http://gw.local:1080",
			ProxyID:    "proxy-1",
			ExpiresAt:  time.Now().Add(5 * time.Minute),
		})
	})
	url, err := c.GetProxyURL(context.Background())
	if err != nil {
		t.Fatalf("GetProxyURL: %v", err)
	}
	if !strings.Contains(url, "u:p@gw.local") {
		t.Errorf("expected credentials in URL, got %q", url)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("expected exactly 1 lease call, got %d", calls)
	}
}

func TestGetProxyURLEmbedsTenantScopedUsername(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(leaseDTO{
			LeaseID:    "lease-1",
			Username:   "tenant-a|lease-1",
			Password:   "p",
			GatewayURL: "http://gw.local:1080",
			ProxyID:    "proxy-1",
			ExpiresAt:  time.Now().Add(5 * time.Minute),
		})
	})
	proxyURL, err := c.GetProxyURL(context.Background())
	if err != nil {
		t.Fatalf("GetProxyURL: %v", err)
	}
	if !strings.Contains(proxyURL, "tenant-a%7Clease-1:p@gw.local") {
		t.Errorf("expected tenant-scoped credentials in URL, got %q", proxyURL)
	}
}

func TestGetProxyKeyStickyReusesLease(t *testing.T) {
	var calls int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_ = json.NewEncoder(w).Encode(leaseDTO{
			LeaseID:    "lease-A",
			Username:   "u",
			Password:   "p",
			GatewayURL: "http://gw.local:1080",
			ExpiresAt:  time.Now().Add(5 * time.Minute),
		})
	})
	p1, err := c.GetProxy(context.Background(), WithKey("acct-a"))
	if err != nil {
		t.Fatal(err)
	}
	p2, err := c.GetProxy(context.Background(), WithKey("acct-a"))
	if err != nil {
		t.Fatal(err)
	}
	if p1.LeaseID != p2.LeaseID {
		t.Errorf("expected sticky lease, got %q vs %q", p1.LeaseID, p2.LeaseID)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("expected 1 server call, got %d", calls)
	}
}

func TestRetryRetriesOn503(t *testing.T) {
	var calls int32
	c := newTestClient(t,
		func(w http.ResponseWriter, r *http.Request) {
			n := atomic.AddInt32(&calls, 1)
			if n == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"error":"unavailable"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(leaseDTO{
				LeaseID:    "lease-OK",
				GatewayURL: "http://gw.local",
				ExpiresAt:  time.Now().Add(time.Minute),
			})
		},
		WithRetry(RetryConfig{MaxAttempts: 3, MinBackoff: time.Millisecond, MaxBackoff: time.Millisecond}),
	)
	p, err := c.GetProxy(context.Background())
	if err != nil {
		t.Fatalf("GetProxy: %v", err)
	}
	if p.LeaseID != "lease-OK" {
		t.Errorf("expected retry success, got %q", p.LeaseID)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Errorf("expected 2 server calls, got %d", calls)
	}
}

func TestDoesNotRetryOn400(t *testing.T) {
	var calls int32
	c := newTestClient(t,
		func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&calls, 1)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"policy_denied"}`))
		},
		WithRetry(RetryConfig{MaxAttempts: 5, MinBackoff: time.Millisecond, MaxBackoff: time.Millisecond}),
	)
	_, err := c.GetProxy(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("expected single call, got %d", calls)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 APIError, got %v", err)
	}
}

func TestReleaseRevokesCachedLease(t *testing.T) {
	var deleteCalled int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/leases":
			_ = json.NewEncoder(w).Encode(leaseDTO{
				LeaseID:    "lease-X",
				GatewayURL: "http://gw.local",
				ExpiresAt:  time.Now().Add(time.Minute),
			})
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/leases/lease-X":
			atomic.AddInt32(&deleteCalled, 1)
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	if _, err := c.GetProxy(context.Background(), WithKey("k")); err != nil {
		t.Fatal(err)
	}
	if err := c.Release(context.Background(), WithReleaseKey("k")); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&deleteCalled) != 1 {
		t.Errorf("expected DELETE to fire, got %d", deleteCalled)
	}
}

func TestExpiredLeaseAutoReacquire(t *testing.T) {
	var creates int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/leases" || r.Method != http.MethodPost {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		n := atomic.AddInt32(&creates, 1)
		_ = json.NewEncoder(w).Encode(leaseDTO{
			LeaseID:    "lease-" + itoa(n),
			GatewayURL: "http://gw.local",
			// First lease is already past expiry to force a re-acquire.
			ExpiresAt: time.Now().Add(time.Duration(n) * time.Hour),
		})
	})
	// Seed the cache with an expired lease for key=k.
	c.storeLease("k", leaseDTO{
		LeaseID:    "stale",
		GatewayURL: "http://gw.local",
		ExpiresAt:  time.Now().Add(-time.Minute),
	})
	p, err := c.GetProxy(context.Background(), WithKey("k"))
	if err != nil {
		t.Fatal(err)
	}
	if p.LeaseID == "stale" {
		t.Errorf("expected re-acquired lease, got stale")
	}
	if atomic.LoadInt32(&creates) != 1 {
		t.Errorf("expected exactly 1 create call, got %d", creates)
	}
}

func TestExpiredLeaseWithoutAutoReacquireReturnsErr(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("server should not be called: %s %s", r.Method, r.URL.Path)
	}, WithLeasePolicy(LeasePolicy{AutoRenew: true, AutoReacquire: false}))
	c.storeLease("k", leaseDTO{
		LeaseID:    "stale",
		GatewayURL: "http://gw.local",
		ExpiresAt:  time.Now().Add(-time.Minute),
	})
	_, err := c.GetProxy(context.Background(), WithKey("k"))
	if !IsLeaseExpired(err) {
		t.Fatalf("expected ErrLeaseExpired, got %v", err)
	}
}

func itoa(n int32) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	v := int64(n)
	if v < 0 {
		v = -v
	}
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
