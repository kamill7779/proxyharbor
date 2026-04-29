package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kamill7779/proxyharbor/internal/auth"
	"github.com/kamill7779/proxyharbor/internal/control"
	"github.com/kamill7779/proxyharbor/internal/shared/domain"
	"github.com/kamill7779/proxyharbor/internal/storage"
)

func TestAdminOnlyRoutesReturnContractErrors(t *testing.T) {
	store := storage.NewMemoryStore()
	adminStore := NewMemoryAdminStore()
	if err := adminStore.CreateTenant(context.Background(), domain.Tenant{ID: "tenant-a", Name: "Tenant A", Enabled: true}); err != nil {
		t.Fatalf("CreateTenant() error = %v", err)
	}
	handler := NewWithOptions(
		control.NewService(store, "http://localhost:8080"),
		auth.NewDynamicKeys(nil).WithAdminKey("admin-key-with-at-least-thirty-two-bytes"),
		Options{AdminStore: adminStore, Pepper: "pepper-with-at-least-thirty-two-bytes"},
	)

	cases := []struct {
		name       string
		method     string
		path       string
		key        string
		body       string
		wantStatus int
	}{
		{name: "missing key", method: http.MethodGet, path: "/v1/providers", wantStatus: http.StatusUnauthorized},
		{name: "invalid key", method: http.MethodGet, path: "/v1/providers", key: "not-admin", wantStatus: http.StatusUnauthorized},
		{name: "bad request", method: http.MethodPost, path: "/v1/providers", key: "admin-key-with-at-least-thirty-two-bytes", body: "{", wantStatus: http.StatusBadRequest},
		{name: "not found", method: http.MethodGet, path: "/v1/providers/missing", key: "admin-key-with-at-least-thirty-two-bytes", wantStatus: http.StatusNotFound},
		{name: "missing tenant keys", method: http.MethodGet, path: "/admin/tenants/missing/keys", key: "admin-key-with-at-least-thirty-two-bytes", wantStatus: http.StatusNotFound},
		{name: "existing tenant keys", method: http.MethodGet, path: "/admin/tenants/tenant-a/keys", key: "admin-key-with-at-least-thirty-two-bytes", wantStatus: http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			if tc.key != "" {
				req.Header.Set(auth.HeaderName, tc.key)
			}
			if tc.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rr.Code, tc.wantStatus, rr.Body.String())
			}
		})
	}
}

func TestGatewayValidateResponseOmitsLeasePassword(t *testing.T) {
	store := storage.NewMemoryStore()
	svc := control.NewService(store, "http://localhost:8080")
	if _, err := store.UpsertProxy(context.Background(), domain.Proxy{ID: "proxy-a", Endpoint: "http://proxy.example.com:8080", Healthy: true, Weight: 1, HealthScore: 100}); err != nil {
		t.Fatalf("UpsertProxy() error = %v", err)
	}
	lease, err := svc.CreateLease(context.Background(), domain.Principal{TenantID: "default"}, "idem-gateway-validate", control.CreateLeaseRequest{
		Subject:     domain.Subject{Type: "user", ID: "subj"},
		ResourceRef: domain.ResourceRef{Kind: "host", ID: "example.com"},
		TTLSeconds:  60,
	})
	if err != nil {
		t.Fatalf("CreateLease() error = %v", err)
	}
	handler := NewWithOptions(
		svc,
		auth.NewDynamicKeys(nil).WithAdminKey("admin-key-with-at-least-thirty-two-bytes"),
		Options{AdminStore: NewMemoryAdminStore(), Pepper: "pepper-with-at-least-thirty-two-bytes"},
	)

	req := httptest.NewRequest(http.MethodGet, "/v1/gateway/validate?tenant_id=default&lease_id="+lease.ID+"&password="+lease.Password+"&target=example.com", nil)
	req.Header.Set(auth.HeaderName, "admin-key-with-at-least-thirty-two-bytes")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if _, ok := body["password"]; ok {
		t.Fatalf("gateway validate leaked password: %s", rr.Body.String())
	}
	if body["lease_id"] != lease.ID || body["proxy_id"] == "" {
		t.Fatalf("gateway validate body = %#v", body)
	}
}
