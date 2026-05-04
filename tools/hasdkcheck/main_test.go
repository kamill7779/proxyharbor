package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
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
	if server.addedProxyID() != server.happyPathProxyID() {
		t.Fatalf("happy path leased proxy %q, want added proxy %q", server.happyPathProxyID(), server.addedProxyID())
	}
	out := stdout.String()
	if !strings.Contains(out, "ok sdk ha happy path") {
		t.Fatalf("stdout missing happy path success line:\n%s", out)
	}
}

func TestWaitTenantKeyVisibleRetriesUntilAuthenticated(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/catalog/latest" {
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		if r.Header.Get("ProxyHarbor-Key") != "tenant-key" {
			http.Error(w, "missing tenant key", http.StatusUnauthorized)
			return
		}
		mu.Lock()
		attempts++
		current := attempts
		mu.Unlock()
		if current < 3 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"auth_failed"}`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"forbidden"}`))
	}))
	t.Cleanup(srv.Close)

	if err := waitTenantKeyVisible(context.Background(), "http://unused.invalid", []string{srv.URL}, "tenant-key"); err != nil {
		t.Fatalf("waitTenantKeyVisible: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts != 3 {
		t.Fatalf("auth probe attempts = %d, want 3", attempts)
	}
}

func TestRunSDKHAHappyPathLabelsFailingStep(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		failStage string
		want      string
	}{
		{name: "add proxy", failStage: "add", want: "sdk ha happy path add proxy"},
		{name: "get proxy", failStage: "get", want: "sdk ha happy path get proxy"},
		{name: "renew lease", failStage: "renew", want: "sdk ha happy path renew lease"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var addedProxyID string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if shouldFailHappyPathRequest(tt.failStage, r) {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.Write([]byte(`{"error":"unavailable","message":"boom"}`))
					return
				}
				switch {
				case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/v1/proxies/"):
					var proxy proxyPayload
					if err := json.NewDecoder(r.Body).Decode(&proxy); err != nil {
						http.Error(w, err.Error(), http.StatusBadRequest)
						return
					}
					if proxy.ID == "" {
						proxy.ID = strings.TrimPrefix(r.URL.Path, "/v1/proxies/")
					}
					if proxy.Weight == 0 {
						proxy.Weight = 1
					}
					_ = json.NewEncoder(w).Encode(proxy)
				case r.Method == http.MethodPost && r.URL.Path == "/v1/proxies":
					var proxy proxyPayload
					if err := json.NewDecoder(r.Body).Decode(&proxy); err != nil {
						http.Error(w, err.Error(), http.StatusBadRequest)
						return
					}
					if proxy.ID == "" {
						proxy.ID = "proxy-added"
					}
					if proxy.Weight == 0 {
						proxy.Weight = 1
					}
					addedProxyID = proxy.ID
					_ = json.NewEncoder(w).Encode(proxy)
				case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/proxies/"):
					w.WriteHeader(http.StatusOK)
				case r.Method == http.MethodPost && r.URL.Path == "/v1/leases":
					if addedProxyID == "" {
						addedProxyID = "sdk-ha-flow-proxy-test"
					}
					_ = json.NewEncoder(w).Encode(leaseResponse{
						LeaseID:     "sdk-ha-flow-lease",
						ProxyID:     addedProxyID,
						GatewayURL:  "http://gw.local:1080",
						Username:    "tenant|sdk-ha-flow-lease",
						Password:    "secret",
						ExpiresAt:   time.Now().Add(2 * time.Minute),
						RenewBefore: time.Now().Add(time.Minute),
					})
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, ":renew"):
					if addedProxyID == "" {
						addedProxyID = "sdk-ha-flow-proxy-test"
					}
					_ = json.NewEncoder(w).Encode(leaseResponse{
						LeaseID:     "sdk-ha-flow-lease",
						ProxyID:     addedProxyID,
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
				proxyharbor.WithDefaultTarget("https://1.1.1.1"),
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

func TestRunSDKHAHappyPathRequiresAddedProxyLease(t *testing.T) {
	t.Parallel()

	var addedProxyID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/v1/proxies/"):
			var proxy proxyPayload
			if err := json.NewDecoder(r.Body).Decode(&proxy); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if proxy.ID == "" {
				proxy.ID = strings.TrimPrefix(r.URL.Path, "/v1/proxies/")
			}
			if proxy.Weight == 0 {
				proxy.Weight = 1
			}
			_ = json.NewEncoder(w).Encode(proxy)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/proxies":
			var proxy proxyPayload
			if err := json.NewDecoder(r.Body).Decode(&proxy); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if proxy.ID == "" {
				proxy.ID = "proxy-added"
			}
			if proxy.Weight == 0 {
				proxy.Weight = 1
			}
			addedProxyID = proxy.ID
			_ = json.NewEncoder(w).Encode(proxy)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/proxies/"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/leases":
			if addedProxyID == "" {
				addedProxyID = "sdk-ha-flow-proxy-test"
			}
			_ = json.NewEncoder(w).Encode(leaseResponse{
				LeaseID:     "lease-from-other-proxy",
				ProxyID:     "sdk-ha-proxy-00",
				GatewayURL:  "http://gw.local:1080",
				Username:    "tenant|lease-from-other-proxy",
				Password:    "secret",
				ExpiresAt:   time.Now().Add(2 * time.Minute),
				RenewBefore: time.Now().Add(time.Minute),
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/leases/lease-from-other-proxy:renew":
			_ = json.NewEncoder(w).Encode(leaseResponse{
				LeaseID:     "lease-from-other-proxy",
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
		proxyharbor.WithDefaultTarget("https://1.1.1.1"),
		proxyharbor.WithTimeout(5*time.Second),
		proxyharbor.WithRetry(proxyharbor.RetryConfig{MaxAttempts: 1, MinBackoff: time.Millisecond, MaxBackoff: time.Millisecond}),
		proxyharbor.WithLeasePolicy(proxyharbor.LeasePolicy{AutoRenew: false, AutoReacquire: false}),
	)
	if err != nil {
		t.Fatalf("new tenant client: %v", err)
	}

	err = runSDKHAHappyPath(context.Background(), admin, tenant, io.Discard)
	if err == nil {
		t.Fatal("expected added proxy lease mismatch error")
	}
	if !strings.Contains(err.Error(), "want added proxy") {
		t.Fatalf("error %q does not mention added proxy mismatch", err)
	}
}

func shouldFailHappyPathRequest(stage string, r *http.Request) bool {
	switch stage {
	case "add":
		return r.Method == http.MethodPost && r.URL.Path == "/v1/proxies"
	case "get":
		return r.Method == http.MethodPost && r.URL.Path == "/v1/leases"
	case "renew":
		return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, ":renew")
	default:
		return false
	}
}

type fakeHAServer struct {
	mu               sync.Mutex
	proxyAdds        int
	happyGets        int
	renews           int
	leaseCreates     int
	proxies          map[string]proxyPayload
	leases           map[string]leaseResponse
	lastAddedProxyID string
	lastHappyProxyID string
}

func newFakeHAServer() *fakeHAServer {
	return &fakeHAServer{
		proxies: map[string]proxyPayload{},
		leases:  map[string]leaseResponse{},
	}
}

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

func (s *fakeHAServer) addedProxyID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastAddedProxyID
}

func (s *fakeHAServer) happyPathProxyID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastHappyProxyID
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
		if proxy.ID == "" {
			proxy.ID = strings.TrimPrefix(r.URL.Path, "/v1/proxies/")
		}
		if proxy.Weight == 0 {
			proxy.Weight = 1
		}
		s.mu.Lock()
		s.proxies[proxy.ID] = proxy
		s.mu.Unlock()
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
		if proxy.ID == "" {
			proxy.ID = fmt.Sprintf("proxy-added-%02d", s.proxyAdds)
		}
		if proxy.Weight == 0 {
			proxy.Weight = 1
		}
		s.proxies[proxy.ID] = proxy
		s.lastAddedProxyID = proxy.ID
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(proxy)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/proxies/"):
		s.requireKey(w, r, "admin-key")
		s.mu.Lock()
		delete(s.proxies, strings.TrimPrefix(r.URL.Path, "/v1/proxies/"))
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/leases":
		s.requireKey(w, r, "tenant-key")
		var req leaseCreateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		lease, err := s.nextLease(req.Subject.ID)
		if err != nil {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "no_healthy_proxy"})
			return
		}
		_ = json.NewEncoder(w).Encode(lease)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, ":renew"):
		s.requireKey(w, r, "tenant-key")
		s.mu.Lock()
		s.renews++
		leaseID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/leases/"), ":renew")
		lease, ok := s.leases[leaseID]
		if ok {
			lease.ExpiresAt = time.Now().Add(4 * time.Minute)
			lease.RenewBefore = time.Now().Add(2 * time.Minute)
			lease.Username = ""
			lease.Password = ""
			s.leases[leaseID] = lease
		}
		s.mu.Unlock()
		if !ok {
			http.Error(w, "unknown lease", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(lease)
	default:
		http.Error(w, fmt.Sprintf("unexpected request: %s %s", r.Method, r.URL.Path), http.StatusNotFound)
	}
}

func (s *fakeHAServer) requireKey(w http.ResponseWriter, r *http.Request, want string) {
	if got := r.Header.Get("ProxyHarbor-Key"); got != want {
		http.Error(w, fmt.Sprintf("proxyharbor-key=%q want %q", got, want), http.StatusUnauthorized)
	}
}

func (s *fakeHAServer) nextLease(subjectID string) (leaseResponse, error) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leaseCreates++
	proxyID, err := weightedProxyID(s.leaseCreates, s.proxies)
	if err != nil {
		return leaseResponse{}, err
	}
	lease := leaseResponse{
		LeaseID:     fmt.Sprintf("lease-%d", s.leaseCreates),
		ProxyID:     proxyID,
		GatewayURL:  "http://gw.local:1080",
		Username:    fmt.Sprintf("tenant|lease-%d", s.leaseCreates),
		Password:    "secret",
		ExpiresAt:   now.Add(2 * time.Minute),
		RenewBefore: now.Add(time.Minute),
	}
	s.leases[lease.LeaseID] = lease
	if strings.HasPrefix(subjectID, "sdk-ha-flow") {
		s.happyGets++
		s.lastHappyProxyID = proxyID
	}
	return lease, nil
}

func weightedProxyID(call int, proxies map[string]proxyPayload) (string, error) {
	ids := make([]string, 0, len(proxies))
	totalWeight := 0
	for id, proxy := range proxies {
		if !proxy.Healthy || proxy.Weight <= 0 {
			continue
		}
		ids = append(ids, id)
		totalWeight += proxy.Weight
	}
	if totalWeight == 0 {
		return "", fmt.Errorf("no healthy proxies")
	}
	sort.Strings(ids)
	position := (call - 1) % totalWeight
	weight := 0
	for _, id := range ids {
		weight += proxies[id].Weight
		if position < weight {
			return id, nil
		}
	}
	return "", fmt.Errorf("weighted selection fell through")
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
