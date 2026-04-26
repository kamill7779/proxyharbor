package server_test

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/kamill7779/proxyharbor/internal/auth"
	"github.com/kamill7779/proxyharbor/internal/control"
	"github.com/kamill7779/proxyharbor/internal/server"
	"github.com/kamill7779/proxyharbor/internal/shared/domain"
	"github.com/kamill7779/proxyharbor/internal/storage"
)

func TestCoreLeasePolicyCatalogAndGatewayValidation(t *testing.T) {
	store := storage.NewMemoryStore()
	svc := control.NewService(store, "http://gateway.local")
	handler := server.New(svc, auth.New("test-key"))

	badMethod := request(t, handler, http.MethodPost, "/v1/catalog/latest", "{}", "")
	if badMethod.Code != http.StatusMethodNotAllowed {
		t.Fatalf("catalog POST status = %d", badMethod.Code)
	}
	catalog := request(t, handler, http.MethodGet, "/v1/catalog/latest", "", "")
	if catalog.Code != http.StatusOK {
		t.Fatalf("catalog status = %d body=%s", catalog.Code, catalog.Body.String())
	}

	body := `{"subject":{"subject_type":"workload","subject_id":"alpha"},"resource_ref":{"kind":"url","id":"https://example.com"}}`
	first := request(t, handler, http.MethodPost, "/v1/leases", body, "idem-1")
	if first.Code != http.StatusCreated {
		t.Fatalf("create lease status = %d body=%s", first.Code, first.Body.String())
	}
	var lease struct {
		ID         string `json:"lease_id"`
		Password   string `json:"password"`
		Generation int64  `json:"lease_generation"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &lease); err != nil {
		t.Fatal(err)
	}
	if lease.ID == "" || lease.Password == "" || lease.Generation != 1 {
		t.Fatalf("unexpected lease: %+v", lease)
	}

	second := request(t, handler, http.MethodPost, "/v1/leases", body, "idem-1")
	if second.Code != http.StatusCreated {
		t.Fatalf("idempotent status = %d", second.Code)
	}
	var lease2 struct {
		ID string `json:"lease_id"`
	}
	_ = json.Unmarshal(second.Body.Bytes(), &lease2)
	if lease2.ID != lease.ID {
		t.Fatalf("idempotency returned %q, want %q", lease2.ID, lease.ID)
	}

	validated := request(t, handler, http.MethodGet, "/v1/gateway/validate?tenant_id=default&lease_id="+lease.ID+"&password="+lease.Password+"&target=example.com", "", "")
	if validated.Code != http.StatusOK {
		t.Fatalf("validate status = %d body=%s", validated.Code, validated.Body.String())
	}

	feedback := request(t, handler, http.MethodPost, "/v1/internal/gateway-feedback:batch", `{"events":[{"event_id":"feedback-1","resource":"gateway"}]}`, "")
	if feedback.Code != http.StatusAccepted {
		t.Fatalf("feedback status = %d body=%s", feedback.Code, feedback.Body.String())
	}
	revoked := request(t, handler, http.MethodDelete, "/v1/leases/"+lease.ID, "", "")
	if revoked.Code != http.StatusOK {
		t.Fatalf("revoke status = %d", revoked.Code)
	}
	denied := request(t, handler, http.MethodGet, "/v1/gateway/validate?tenant_id=default&lease_id="+lease.ID+"&password="+lease.Password+"&target=example.com", "", "")
	if denied.Code != http.StatusBadRequest {
		t.Fatalf("revoked validate status = %d", denied.Code)
	}
}

func TestStaticProviderHealthyProxyLeaseAndGatewayForward(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = w.Write([]byte("forwarded:" + r.URL.String()))
	}))
	t.Cleanup(upstream.Close)

	store := storage.NewMemoryStore()
	svc := control.NewService(store, "http://gateway.local")
	svc.SetAllowInternalProxyEndpoint(true)
	handler := server.New(svc, auth.New("test-key"))

	provider := request(t, handler, http.MethodPost, "/v1/providers", `{"id":"static-test","type":"static","name":"test","enabled":true}`, "")
	if provider.Code != http.StatusCreated {
		t.Fatalf("create provider status = %d body=%s", provider.Code, provider.Body.String())
	}
	proxy := request(t, handler, http.MethodPost, "/v1/proxies", `{"id":"proxy-test","provider_id":"static-test","endpoint":"`+upstream.URL+`","healthy":true,"weight":100}`, "")
	if proxy.Code != http.StatusCreated {
		t.Fatalf("create proxy status = %d body=%s", proxy.Code, proxy.Body.String())
	}
	health := request(t, handler, http.MethodPost, "/v1/proxies/proxy-test:health", `{"healthy":true}`, "")
	if health.Code != http.StatusOK {
		t.Fatalf("health status = %d body=%s", health.Code, health.Body.String())
	}

	body := `{"subject":{"subject_type":"workload","subject_id":"beta"},"resource_ref":{"kind":"url","id":"https://example.com"}}`
	first := request(t, handler, http.MethodPost, "/v1/leases", body, "idem-forward")
	if first.Code != http.StatusCreated {
		t.Fatalf("create lease status = %d body=%s", first.Code, first.Body.String())
	}
	second := request(t, handler, http.MethodPost, "/v1/leases", body, "idem-forward")
	// 幂等重试不会重新返回明文密码（后端不持久化明文），所以仅比较 lease ID。
	var firstLease, secondLease struct {
		ID string `json:"lease_id"`
	}
	_ = json.Unmarshal(first.Body.Bytes(), &firstLease)
	_ = json.Unmarshal(second.Body.Bytes(), &secondLease)
	if firstLease.ID == "" || firstLease.ID != secondLease.ID {
		t.Fatalf("idempotency mismatch first=%s second=%s", first.Body.String(), second.Body.String())
	}
	var lease struct {
		ID       string `json:"lease_id"`
		Password string `json:"password"`
		ProxyID  string `json:"proxy_id"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &lease); err != nil {
		t.Fatal(err)
	}
	if lease.ProxyID != "proxy-test" {
		t.Fatalf("lease proxy = %q", lease.ProxyID)
	}

	target, _ := url.Parse("http://example.com/resource")
	req := httptest.NewRequest(http.MethodGet, target.String(), nil)
	req.URL = target
	req.Host = target.Host
	req.SetBasicAuth(lease.ID, lease.Password)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("gateway forward status = %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "forwarded:http://example.com/resource") {
		t.Fatalf("gateway body = %s", rr.Body.String())
	}

	dangerous := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/private", nil)
	dangerous.SetBasicAuth(lease.ID, lease.Password)
	dangerousRR := httptest.NewRecorder()
	handler.ServeHTTP(dangerousRR, dangerous)
	if dangerousRR.Code != http.StatusBadRequest || !strings.Contains(dangerousRR.Body.String(), "unsafe_destination") {
		t.Fatalf("dangerous target response = %d %s", dangerousRR.Code, dangerousRR.Body.String())
	}
	missingPassword := httptest.NewRequest(http.MethodGet, target.String(), nil)
	missingPassword.URL = target
	missingPassword.Host = target.Host
	missingPassword.Header.Set("ProxyHarbor-Lease", lease.ID)
	missingPasswordRR := httptest.NewRecorder()
	handler.ServeHTTP(missingPasswordRR, missingPassword)
	if missingPasswordRR.Code != http.StatusUnauthorized || !strings.Contains(missingPasswordRR.Body.String(), "auth_failed") {
		t.Fatalf("missing password response = %d %s", missingPasswordRR.Code, missingPasswordRR.Body.String())
	}

	revoked := request(t, handler, http.MethodDelete, "/v1/leases/"+lease.ID, "", "")
	if revoked.Code != http.StatusOK {
		t.Fatalf("revoke status = %d", revoked.Code)
	}
	revokedReq := httptest.NewRequest(http.MethodGet, target.String(), nil)
	revokedReq.URL = target
	revokedReq.Host = target.Host
	revokedReq.SetBasicAuth(lease.ID, lease.Password)
	revokedRR := httptest.NewRecorder()
	handler.ServeHTTP(revokedRR, revokedReq)
	if revokedRR.Code != http.StatusBadRequest || !strings.Contains(revokedRR.Body.String(), "lease_revoked") {
		t.Fatalf("revoked response = %d %s", revokedRR.Code, revokedRR.Body.String())
	}
}

func TestGatewayConnectUsesLeaseProxy(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "example.com:443" {
			t.Fatalf("unexpected proxy request method=%s host=%s", r.Method, r.Host)
		}
		if r.Header.Get("Proxy-Authorization") != "Basic "+basicAuth("upstream", "secret") {
			t.Fatalf("missing upstream proxy auth: %q", r.Header.Get("Proxy-Authorization"))
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(proxy.Close)

	store := storage.NewMemoryStore()
	svc := control.NewService(store, "http://gateway.local")
	svc.SetAllowInternalProxyEndpoint(true)
	handler := server.New(svc, auth.New("test-key"))
	proxyURL, _ := url.Parse(proxy.URL)
	proxyURL.User = url.UserPassword("upstream", "secret")
	_ = request(t, handler, http.MethodPost, "/v1/proxies", `{"id":"connect-proxy","endpoint":"`+proxyURL.String()+`","healthy":true,"weight":100}`, "")
	created := request(t, handler, http.MethodPost, "/v1/leases", `{"subject":{"subject_type":"workload","subject_id":"gamma"},"resource_ref":{"kind":"url","id":"https://example.com"}}`, "idem-connect")
	var lease struct {
		ID       string `json:"lease_id"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &lease); err != nil {
		t.Fatal(err)
	}

	client := httptest.NewServer(handler)
	t.Cleanup(client.Close)
	conn, err := netDial(client.URL)
	if err != nil {
		t.Fatal(err)
	}
	encoded := basicAuth(lease.ID, lease.Password)
	_, _ = io.WriteString(conn, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\nProxy-Authorization: Basic "+encoded+"\r\n\r\n")
	resp, err := http.ReadResponse(bufioNewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = conn.Close()
		t.Fatalf("connect status = %d body=%s", resp.StatusCode, string(body))
	}
	_ = conn.Close()
}

func request(t *testing.T, handler http.Handler, method, path, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set(auth.HeaderName, "test-key")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func netDial(rawurl string) (net.Conn, error) {
	u, err := url.Parse(rawurl)
	if err != nil {
		return nil, err
	}
	return net.Dial("tcp", u.Host)
}

func basicAuth(username, password string) string {
	return base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
}

func bufioNewReader(conn net.Conn) *bufio.Reader {
	return bufio.NewReader(conn)
}

// v0.1.4 static PROXYHARBOR_TENANT_KEYS compatibility.
func TestTenantKeysCompatibility(t *testing.T) {
	store := storage.NewMemoryStore()
	svc := control.NewService(store, "http://gateway.local")
	keys := auth.ParseTenantKeys("tenant_a:key_a,tenant_b:key_b")
	authn := auth.NewTenantKeys(keys)
	handler := server.New(svc, authn)

	// tenant_a key works.
	rr := requestWithAuth(t, handler, "GET", "/v1/catalog/latest", "", "key_a")
	if rr.Code != http.StatusOK {
		t.Fatalf("tenant_a catalog status=%d body=%s", rr.Code, rr.Body.String())
	}

	// tenant_b key works.
	rr = requestWithAuth(t, handler, "GET", "/v1/catalog/latest", "", "key_b")
	if rr.Code != http.StatusOK {
		t.Fatalf("tenant_b catalog status=%d body=%s", rr.Code, rr.Body.String())
	}
}

// Admin with On-Behalf-Of creates policy; tenant sees it with own key.
func TestAdminOnBehalfOfCreatesPolicy(t *testing.T) {
	store := storage.NewMemoryStore()
	svc := control.NewService(store, "http://gateway.local")
	adminStore := newMockAdminStore()
	adminStore.tenants["t1"] = domain.Tenant{ID: "t1", Name: "Test", Enabled: true}
	authn := auth.NewLegacy("legacy").WithAdminKey("admin-secret")
	handler := server.NewWithAdminStore(svc, authn, adminStore, "pepper")

	// Admin creates policy on behalf of t1.
	_ = adminRequest(t, handler, "POST", "/v1/policies", `{"id":"p1","name":"test","enabled":true,"ttl_seconds":600}`, "admin-secret")
	// Re-execute with the header set properly.
	req := httptest.NewRequest("POST", "/v1/policies", bytes.NewBufferString(`{"id":"p1","name":"test","enabled":true,"ttl_seconds":600}`))
	req.Header.Set(auth.HeaderName, "admin-secret")
	req.Header.Set("X-On-Behalf-Of", "t1")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create policy status=%d body=%s", rr.Code, rr.Body.String())
	}

	// Tenant uses own key to list policies.
	authn2 := auth.NewTenantKeys(map[string]string{"tk": "t1"})
	handler2 := server.New(svc, authn2)
	rr = requestWithAuth(t, handler2, "GET", "/v1/policies", "", "tk")
	if rr.Code != http.StatusOK {
		t.Fatalf("list policies status=%d body=%s", rr.Code, rr.Body.String())
	}
}

// Tenant A key with X-On-Behalf-Of: B -> 403.
func TestTenantCrossBehalfOf(t *testing.T) {
	store := storage.NewMemoryStore()
	svc := control.NewService(store, "http://gateway.local")
	authn := auth.NewTenantKeys(map[string]string{"tk": "t1"})
	handler := server.New(svc, authn)

	req := httptest.NewRequest("GET", "/v1/policies", nil)
	req.Header.Set(auth.HeaderName, "tk")
	req.Header.Set("X-On-Behalf-Of", "t2")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// Admin deletes tenant -> all keys immediately invalid.
func TestAdminDeleteTenantInvalidatesKeys(t *testing.T) {
	adminStore := newMockAdminStore()
	adminStore.tenants["t1"] = domain.Tenant{ID: "t1", Name: "Test", Enabled: true}
	adminStore.keys["t1"] = []auth.TenantKey{{ID: "k1", TenantID: "t1", KeyHash: "hash"}}
	store := storage.NewMemoryStore()
	svc := control.NewService(store, "http://gateway.local")
	authn := auth.NewLegacy("legacy").WithAdminKey("admin-secret")
	handler := server.NewWithAdminStore(svc, authn, adminStore, "pepper")

	// Delete tenant.
	rr := adminRequest(t, handler, "DELETE", "/admin/tenants/t1", "", "admin-secret")
	if rr.Code != http.StatusOK {
		t.Fatalf("delete tenant status=%d body=%s", rr.Code, rr.Body.String())
	}

	// Verify tenant is disabled.
	if adminStore.tenants["t1"].Enabled {
		t.Fatal("expected tenant to be disabled")
	}
}

func requestWithAuth(t *testing.T, handler http.Handler, method, path, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set(auth.HeaderName, key)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}
