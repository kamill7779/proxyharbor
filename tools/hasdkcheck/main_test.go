package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	proxyharbor "github.com/kamill7779/proxyharbor/sdks/go/proxyharbor"
)

func TestRunIncludesSDKHAHappyPath(t *testing.T) {
	t.Parallel()

	server := newFakeHAServer()
	srv := httptest.NewServer(http.HandlerFunc(server.handle))
	t.Cleanup(srv.Close)

	cfg := config{
		BaseURL:        srv.URL,
		AdminKey:       "admin-key",
		TenantKey:      "tenant-key",
		Concurrency:    16,
		Samples:        500,
		DisableSamples: 100,
		Timeout:        time.Minute,
	}

	var stdout bytes.Buffer
	if err := run(context.Background(), cfg, &stdout); err != nil {
		t.Fatalf("run: %v", err)
	}

	if server.happyPathGets() != 1 {
		t.Fatalf("happy path get calls = %d, want 1", server.happyPathGets())
	}
	if server.renewCalls() != 1 {
		t.Fatalf("renew calls = %d, want 1", server.renewCalls())
	}
	if server.proxyAddCalls() != 1 {
		t.Fatalf("admin add proxy calls = %d, want 1", server.proxyAddCalls())
	}
	out := stdout.String()
	if !strings.Contains(out, "ok sdk ha happy path") {
		t.Fatalf("stdout missing happy path success line:\n%s", out)
	}
}

func TestRunSDKHAHappyPathLabelsFailingStep(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		failPath string
		want     string
	}{
		{name: "add proxy", failPath: "/v1/proxies", want: "sdk ha happy path add proxy"},
		{name: "get proxy", failPath: "/v1/leases", want: "sdk ha happy path get proxy"},
		{name: "renew lease", failPath: "/v1/leases/sdk-ha-flow-lease:renew", want: "sdk ha happy path renew lease"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == tt.failPath:
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.Write([]byte(`{"error":"unavailable","message":"boom"}`))
				case r.Method == http.MethodPost && r.URL.Path == "/v1/proxies":
					_ = json.NewEncoder(w).Encode(proxyPayload{ID: "sdk-ha-flow-added", Endpoint: "http://198.51.100.10:28080"})
				case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/proxies/"):
					w.WriteHeader(http.StatusOK)
				case r.Method == http.MethodPost && r.URL.Path == "/v1/leases":
					_ = json.NewEncoder(w).Encode(leaseResponse{
						LeaseID:     "sdk-ha-flow-lease",
						ProxyID:     "sdk-ha-proxy-00",
						GatewayURL:  "http://gw.local:1080",
						Username:    "tenant|sdk-ha-flow-lease",
						Password:    "secret",
						ExpiresAt:   time.Now().Add(2 * time.Minute),
						RenewBefore: time.Now().Add(time.Minute),
					})
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, ":renew"):
					_ = json.NewEncoder(w).Encode(leaseResponse{
						LeaseID:     "sdk-ha-flow-lease",
						ProxyID:     "sdk-ha-proxy-00",
						GatewayURL:  "http://gw.local:1080",
						ExpiresAt:   time.Now().Add(4 * time.Minute),
						RenewBefore: time.Now().Add(2 * time.Minute),
					})
				default:
					http.Error(w, "unexpected request", http.StatusNotFound)
				}
			}))
			defer srv.Close()

			admin, err := proxyharbor.New(
				proxyharbor.WithBaseURL(srv.URL),
				proxyharbor.WithAdminKey("admin-key"),
				proxyharbor.WithTimeout(5*time.Second),
				proxyharbor.WithRetry(proxyharbor.RetryConfig{MaxAttempts: 1, MinBackoff: time.Millisecond, MaxBackoff: time.Millisecond}),
			)
			if err != nil {
				t.Fatalf("new admin client: %v", err)
			}
			tenant, err := proxyharbor.New(
				proxyharbor.WithBaseURL(srv.URL),
				proxyharbor.WithTenantKey("tenant-key"),
				proxyharbor.WithDefaultTarget("https://example.com"),
				proxyharbor.WithTimeout(5*time.Second),
				proxyharbor.WithRetry(proxyharbor.RetryConfig{MaxAttempts: 1, MinBackoff: time.Millisecond, MaxBackoff: time.Millisecond}),
				proxyharbor.WithLeasePolicy(proxyharbor.LeasePolicy{AutoRenew: false, AutoReacquire: false}),
			)
			if err != nil {
				t.Fatalf("new tenant client: %v", err)
			}

			err = runSDKHAHappyPath(context.Background(), admin, tenant, io.Discard)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not contain %q", err, tt.want)
			}
		})
	}
}

type fakeHAServer struct {
	mu            sync.Mutex
	disabledProxy bool
	proxyAdds     int
	happyGets     int
	renews        int
	leaseCreates  int
}

func newFakeHAServer() *fakeHAServer { return &fakeHAServer{} }

func (s *fakeHAServer) proxyAddCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.proxyAdds
}

func (s *fakeHAServer) happyPathGets() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.happyGets
}

func (s *fakeHAServer) renewCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.renews
}

func (s *fakeHAServer) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/readyz":
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPost && r.URL.Path == "/admin/tenants":
		s.requireKey(w, r, "admin-key")
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/admin/tenants/") && strings.HasSuffix(r.URL.Path, "/keys"):
		s.requireKey(w, r, "admin-key")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(tenantKeyResponse{Key: "tenant-key"})
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/v1/proxies/"):
		s.requireKey(w, r, "admin-key")
		var proxy proxyPayload
		if err := json.NewDecoder(r.Body).Decode(&proxy); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		if strings.HasSuffix(r.URL.Path, "/sdk-ha-proxy-09") {
			s.disabledProxy = !proxy.Healthy
		}
		s.mu.Unlock()
		if proxy.ID == "" {
			proxy.ID = strings.TrimPrefix(r.URL.Path, "/v1/proxies/")
		}
		_ = json.NewEncoder(w).Encode(proxy)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/proxies":
		s.requireKey(w, r, "admin-key")
		var proxy proxyPayload
		if err := json.NewDecoder(r.Body).Decode(&proxy); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.proxyAdds++
		s.mu.Unlock()
		proxy.ID = "sdk-ha-flow-added"
		_ = json.NewEncoder(w).Encode(proxy)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/proxies/"):
		s.requireKey(w, r, "admin-key")
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/leases":
		s.requireKey(w, r, "tenant-key")
		var req leaseCreateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		lease := s.nextLease(req.Subject.ID)
		_ = json.NewEncoder(w).Encode(lease)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, ":renew"):
		s.requireKey(w, r, "tenant-key")
		s.mu.Lock()
		s.renews++
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(leaseResponse{
			LeaseID:     "sdk-ha-flow-lease",
			ProxyID:     "sdk-ha-flow-added",
			GatewayURL:  "http://gw.local:1080",
			ExpiresAt:   time.Now().Add(4 * time.Minute),
			RenewBefore: time.Now().Add(2 * time.Minute),
		})
	default:
		http.Error(w, fmt.Sprintf("unexpected request: %s %s", r.Method, r.URL.Path), http.StatusNotFound)
	}
}

func (s *fakeHAServer) requireKey(w http.ResponseWriter, r *http.Request, want string) {
	if got := r.Header.Get("ProxyHarbor-Key"); got != want {
		http.Error(w, fmt.Sprintf("proxyharbor-key=%q want %q", got, want), http.StatusUnauthorized)
	}
}

func (s *fakeHAServer) nextLease(subjectID string) leaseResponse {
	now := time.Now()
	if strings.HasPrefix(subjectID, "sdk-ha-flow") {
		s.mu.Lock()
		s.happyGets++
		s.mu.Unlock()
		return leaseResponse{
			LeaseID:     "sdk-ha-flow-lease",
			ProxyID:     "sdk-ha-flow-added",
			GatewayURL:  "http://gw.local:1080",
			Username:    "tenant|sdk-ha-flow-lease",
			Password:    "secret",
			ExpiresAt:   now.Add(2 * time.Minute),
			RenewBefore: now.Add(time.Minute),
		}
	}

	s.mu.Lock()
	s.leaseCreates++
	createIndex := s.leaseCreates
	disabled := s.disabledProxy
	s.mu.Unlock()

	proxyID := weightedProxyID(createIndex, disabled)
	return leaseResponse{
		LeaseID:     fmt.Sprintf("lease-%d", createIndex),
		ProxyID:     proxyID,
		GatewayURL:  "http://gw.local:1080",
		Username:    "tenant|lease",
		Password:    "secret",
		ExpiresAt:   now.Add(2 * time.Minute),
		RenewBefore: now.Add(time.Minute),
	}
}

func weightedProxyID(call int, disabled bool) string {
	totalWeight := 55
	if disabled {
		totalWeight = 45
	}
	position := (call - 1) % totalWeight
	weight := 0
	for i := 0; i < 10; i++ {
		if disabled && i == 9 {
			continue
		}
		weight += i + 1
		if position < weight {
			return fmt.Sprintf("sdk-ha-proxy-%02d", i)
		}
	}
	return "sdk-ha-proxy-00"
}

type proxyPayload struct {
	ID       string `json:"id,omitempty"`
	Endpoint string `json:"endpoint"`
	Healthy  bool   `json:"healthy"`
	Weight   int    `json:"weight,omitempty"`
}

type leaseCreateRequest struct {
	Subject leaseSubject `json:"subject"`
}

type leaseSubject struct {
	ID string `json:"subject_id"`
}

type leaseResponse struct {
	LeaseID     string    `json:"lease_id"`
	ProxyID     string    `json:"proxy_id"`
	GatewayURL  string    `json:"gateway_url"`
	Username    string    `json:"username,omitempty"`
	Password    string    `json:"password,omitempty"`
	ExpiresAt   time.Time `json:"expires_at"`
	RenewBefore time.Time `json:"renew_before"`
}
