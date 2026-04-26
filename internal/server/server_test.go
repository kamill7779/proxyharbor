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

// --- v0.1.4 tenant-key auth coverage --------------------------------------

func tenantKeyHandler(t *testing.T, keys map[string]string) http.Handler {
	t.Helper()
	store := storage.NewMemoryStore()
	svc := control.NewService(store, "http://gateway.local")
	return server.New(svc, auth.NewWithTenantKeys(keys))
}

// raw issues an HTTP request with explicit headers, used for cases where the
// existing `request` helper cannot vary the key / tenant header.
func raw(t *testing.T, handler http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func TestTenantKeyAuth_IsolationAndOverrides(t *testing.T) {
	const (
		keyA = "tenantAkeyAAAAAAAA"
		keyB = "tenantBkeyBBBBBBBB"
	)
	handler := tenantKeyHandler(t, map[string]string{keyA: "tenanta", keyB: "tenantb"})

	policyBody := `{"id":"p1","name":"p1","enabled":true}`

	// A1: tenantA creates a policy; readable by tenantA.
	create := raw(t, handler, http.MethodPost, "/v1/policies", policyBody, map[string]string{auth.HeaderName: keyA})
	if create.Code != http.StatusCreated {
		t.Fatalf("A1 create status=%d body=%s", create.Code, create.Body.String())
	}
	listA := raw(t, handler, http.MethodGet, "/v1/policies", "", map[string]string{auth.HeaderName: keyA})
	if listA.Code != http.StatusOK || !strings.Contains(listA.Body.String(), `"p1"`) {
		t.Fatalf("A1 list status=%d body=%s", listA.Code, listA.Body.String())
	}

	// A2: tenantB cannot see tenantA's policy.
	listB := raw(t, handler, http.MethodGet, "/v1/policies", "", map[string]string{auth.HeaderName: keyB})
	if listB.Code != http.StatusOK {
		t.Fatalf("A2 list status=%d body=%s", listB.Code, listB.Body.String())
	}
	if strings.Contains(listB.Body.String(), `"p1"`) {
		t.Fatalf("A2 cross-tenant leak: %s", listB.Body.String())
	}

	// A3: tenantA key + tenantB header asserts mismatch -> 403.
	mismatch := raw(t, handler, http.MethodPost, "/v1/policies", policyBody, map[string]string{
		auth.HeaderName:       keyA,
		auth.TenantHeaderName: "tenantb",
	})
	if mismatch.Code != http.StatusForbidden {
		t.Fatalf("A3 mismatch status=%d body=%s", mismatch.Code, mismatch.Body.String())
	}
	if !strings.Contains(mismatch.Body.String(), "tenant_mismatch") {
		t.Fatalf("A3 expected tenant_mismatch error, got %s", mismatch.Body.String())
	}

	// A4: tenantA key + matching tenant header is accepted.
	match := raw(t, handler, http.MethodPost, "/v1/policies", `{"id":"p2","name":"p2","enabled":true}`, map[string]string{
		auth.HeaderName:       keyA,
		auth.TenantHeaderName: "tenanta",
	})
	if match.Code != http.StatusCreated {
		t.Fatalf("A4 match status=%d body=%s", match.Code, match.Body.String())
	}

	// A5: unknown key -> 401.
	unknown := raw(t, handler, http.MethodGet, "/v1/policies", "", map[string]string{auth.HeaderName: "doesnotexistAAAAAA"})
	if unknown.Code != http.StatusUnauthorized {
		t.Fatalf("A5 unknown key status=%d", unknown.Code)
	}

	// A6: missing key header -> 401.
	missing := raw(t, handler, http.MethodGet, "/v1/policies", "", map[string]string{})
	if missing.Code != http.StatusUnauthorized {
		t.Fatalf("A6 missing key status=%d", missing.Code)
	}

	// A7: body-asserted tenant_id is ignored; principal.TenantID wins.
	bodyWithCrossTenant := `{"id":"p3","name":"p3","enabled":true,"tenant_id":"tenantb"}`
	create7 := raw(t, handler, http.MethodPost, "/v1/policies", bodyWithCrossTenant, map[string]string{auth.HeaderName: keyA})
	if create7.Code != http.StatusCreated {
		t.Fatalf("A7 create status=%d body=%s", create7.Code, create7.Body.String())
	}
	listAAfter := raw(t, handler, http.MethodGet, "/v1/policies", "", map[string]string{auth.HeaderName: keyA})
	if !strings.Contains(listAAfter.Body.String(), `"p3"`) {
		t.Fatalf("A7 expected p3 under tenantA, got %s", listAAfter.Body.String())
	}
	listBAfter := raw(t, handler, http.MethodGet, "/v1/policies", "", map[string]string{auth.HeaderName: keyB})
	if strings.Contains(listBAfter.Body.String(), `"p3"`) {
		t.Fatalf("A7 leaked into tenantB: %s", listBAfter.Body.String())
	}

	// A3b: query param tenant_id mismatch -> 403.
	queryMismatch := raw(t, handler, http.MethodGet, "/v1/catalog/latest?tenant_id=tenantb", "", map[string]string{auth.HeaderName: keyA})
	if queryMismatch.Code != http.StatusForbidden {
		t.Fatalf("A3b query mismatch status=%d body=%s", queryMismatch.Code, queryMismatch.Body.String())
	}
}

func TestLegacySingleKeyAuth_BackwardCompat(t *testing.T) {
	// L1 / L2: legacy mode preserves v0.1.3 semantics: client-asserted tenant
	// is honoured; absence defaults to "default".
	store := storage.NewMemoryStore()
	svc := control.NewService(store, "http://gateway.local")
	handler := server.New(svc, auth.New("legacy-key"))

	createDefault := raw(t, handler, http.MethodPost, "/v1/policies", `{"id":"plegacy","name":"plegacy","enabled":true}`, map[string]string{
		auth.HeaderName: "legacy-key",
	})
	if createDefault.Code != http.StatusCreated {
		t.Fatalf("L1 create status=%d body=%s", createDefault.Code, createDefault.Body.String())
	}

	// L2: explicit tenant header still drives tenancy in legacy mode.
	createTeam := raw(t, handler, http.MethodPost, "/v1/policies", `{"id":"pteam","name":"pteam","enabled":true}`, map[string]string{
		auth.HeaderName:       "legacy-key",
		auth.TenantHeaderName: "team-a",
	})
	if createTeam.Code != http.StatusCreated {
		t.Fatalf("L2 create status=%d body=%s", createTeam.Code, createTeam.Body.String())
	}

	listDefault := raw(t, handler, http.MethodGet, "/v1/policies", "", map[string]string{
		auth.HeaderName: "legacy-key",
	})
	if !strings.Contains(listDefault.Body.String(), `"plegacy"`) || strings.Contains(listDefault.Body.String(), `"pteam"`) {
		t.Fatalf("L1/L2 default tenant view wrong: %s", listDefault.Body.String())
	}

	listTeam := raw(t, handler, http.MethodGet, "/v1/policies", "", map[string]string{
		auth.HeaderName:       "legacy-key",
		auth.TenantHeaderName: "team-a",
	})
	if !strings.Contains(listTeam.Body.String(), `"pteam"`) || strings.Contains(listTeam.Body.String(), `"plegacy"`) {
		t.Fatalf("L2 team-a view wrong: %s", listTeam.Body.String())
	}

	// Bad key still rejected.
	bad := raw(t, handler, http.MethodGet, "/v1/policies", "", map[string]string{auth.HeaderName: "nope"})
	if bad.Code != http.StatusUnauthorized {
		t.Fatalf("legacy bad key status=%d", bad.Code)
	}
}

func TestAuth_FingerprintIsStableAndShort(t *testing.T) {
	if got := auth.Fingerprint(""); got != "" {
		t.Fatalf("empty key fingerprint should be empty, got %q", got)
	}
	a := auth.Fingerprint("tenantAkeyAAAAAAAA")
	b := auth.Fingerprint("tenantAkeyAAAAAAAA")
	if a != b || len(a) != 8 {
		t.Fatalf("fingerprint not stable/short: a=%q b=%q", a, b)
	}
	if auth.Fingerprint("other") == a {
		t.Fatalf("fingerprint collided unexpectedly")
	}
}
