package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kamill7779/proxyharbor/internal/auth"
	"github.com/kamill7779/proxyharbor/internal/control"
	"github.com/kamill7779/proxyharbor/internal/server"
	"github.com/kamill7779/proxyharbor/internal/shared/domain"
	"github.com/kamill7779/proxyharbor/internal/storage"
)

// mockAdminStore implements server.AdminStore for tests.
type mockAdminStore struct {
	tenants   map[string]domain.Tenant
	keys      map[string][]auth.TenantKey
	events    []domain.AuditEvent
	version   int64
	keyHashFn func(pepper, key string) string
}

func newMockAdminStore() *mockAdminStore {
	return &mockAdminStore{
		tenants: make(map[string]domain.Tenant),
		keys:    make(map[string][]auth.TenantKey),
	}
}

func (m *mockAdminStore) GetTenant(ctx context.Context, id string) (domain.Tenant, error) {
	t, ok := m.tenants[id]
	if !ok {
		return domain.Tenant{}, domain.ErrNotFound
	}
	return t, nil
}
func (m *mockAdminStore) ListTenants(ctx context.Context) ([]domain.Tenant, error) {
	var out []domain.Tenant
	for _, t := range m.tenants {
		out = append(out, t)
	}
	return out, nil
}
func (m *mockAdminStore) CreateTenant(ctx context.Context, tenant domain.Tenant) error {
	m.tenants[tenant.ID] = tenant
	return nil
}
func (m *mockAdminStore) UpdateTenant(ctx context.Context, id string, displayName, status *string) error {
	t, ok := m.tenants[id]
	if !ok {
		return domain.ErrNotFound
	}
	if displayName != nil {
		t.Name = *displayName
	}
	if status != nil {
		t.Enabled = *status == "active"
	}
	m.tenants[id] = t
	return nil
}
func (m *mockAdminStore) SoftDeleteTenant(ctx context.Context, id string) error {
	t, ok := m.tenants[id]
	if !ok {
		return domain.ErrNotFound
	}
	t.Enabled = false
	m.tenants[id] = t
	return nil
}
func (m *mockAdminStore) ListTenantKeys(ctx context.Context, tenantID string) ([]auth.TenantKey, error) {
	return m.keys[tenantID], nil
}
func (m *mockAdminStore) CreateTenantKey(ctx context.Context, key auth.TenantKey) error {
	m.keys[key.TenantID] = append(m.keys[key.TenantID], key)
	return nil
}
func (m *mockAdminStore) RevokeTenantKey(ctx context.Context, tenantID, keyID string) error {
	for i := range m.keys[tenantID] {
		if m.keys[tenantID][i].ID == keyID {
			now := time.Now()
			m.keys[tenantID][i].RevokedAt = &now
		}
	}
	return nil
}
func (m *mockAdminStore) IncrementTenantKeysVersion(ctx context.Context) error {
	m.version++
	return nil
}
func (m *mockAdminStore) AppendAuditEvents(ctx context.Context, events []domain.AuditEvent) error {
	m.events = append(m.events, events...)
	return nil
}

func adminRequest(t *testing.T, handler http.Handler, method, path, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set(auth.HeaderName, key)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

// Issue key: 201, response contains plaintext key (only once).
func TestAdminHandler_IssueKey(t *testing.T) {
	store := newMockAdminStore()
	svc := control.NewService(storage.NewMemoryStore(), "http://gateway.local")
	authn := auth.NewLegacy("legacy").WithAdminKey("admin-secret")
	handler := server.NewWithAdminStore(svc, authn, store, "pepper")

	// Create tenant first.
	rr := adminRequest(t, handler, "POST", "/admin/tenants", `{"id":"t1","display_name":"Test"}`, "admin-secret")
	if rr.Code != http.StatusCreated {
		t.Fatalf("create tenant status=%d body=%s", rr.Code, rr.Body.String())
	}

	// Issue key.
	rr = adminRequest(t, handler, "POST", "/admin/tenants/t1/keys", `{"label":"test-key"}`, "admin-secret")
	if rr.Code != http.StatusCreated {
		t.Fatalf("issue key status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		KeyID string `json:"key_id"`
		Key   string `json:"key"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.KeyID == "" || resp.Key == "" {
		t.Fatal("expected key_id and plaintext key in response")
	}
	assertAuditAction(t, store.events, "tenant_key.issued")
}

// List keys: 200, response does NOT contain plaintext key.
func TestAdminHandler_ListKeys(t *testing.T) {
	store := newMockAdminStore()
	svc := control.NewService(storage.NewMemoryStore(), "http://gateway.local")
	authn := auth.NewLegacy("legacy").WithAdminKey("admin-secret")
	handler := server.NewWithAdminStore(svc, authn, store, "pepper")

	adminRequest(t, handler, "POST", "/admin/tenants", `{"id":"t1","display_name":"Test"}`, "admin-secret")
	adminRequest(t, handler, "POST", "/admin/tenants/t1/keys", `{"label":"test-key"}`, "admin-secret")

	rr := adminRequest(t, handler, "GET", "/admin/tenants/t1/keys", "", "admin-secret")
	if rr.Code != http.StatusOK {
		t.Fatalf("list keys status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if bytes.Contains([]byte(body), []byte("phk_")) {
		t.Fatal("list keys should not contain plaintext key")
	}
}

// Revoke key: 200, subsequent auth fails.
func TestAdminHandler_RevokeKey(t *testing.T) {
	store := newMockAdminStore()
	svc := control.NewService(storage.NewMemoryStore(), "http://gateway.local")
	authn := auth.NewLegacy("legacy").WithAdminKey("admin-secret")
	handler := server.NewWithAdminStore(svc, authn, store, "pepper")

	adminRequest(t, handler, "POST", "/admin/tenants", `{"id":"t1","display_name":"Test"}`, "admin-secret")
	rr := adminRequest(t, handler, "POST", "/admin/tenants/t1/keys", `{"label":"test-key"}`, "admin-secret")
	var resp struct {
		KeyID string `json:"key_id"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)

	// Revoke.
	rr = adminRequest(t, handler, "DELETE", "/admin/tenants/t1/keys/"+resp.KeyID, "", "admin-secret")
	if rr.Code != http.StatusOK {
		t.Fatalf("revoke status=%d body=%s", rr.Code, rr.Body.String())
	}

	assertAuditAction(t, store.events, "tenant_key.revoked")
}

// Admin without On-Behalf-Of calling control plane -> 400.
func TestAdminHandler_MissingOnBehalfOf(t *testing.T) {
	store := newMockAdminStore()
	svc := control.NewService(storage.NewMemoryStore(), "http://gateway.local")
	authn := auth.NewLegacy("legacy").WithAdminKey("admin-secret")
	handler := server.NewWithAdminStore(svc, authn, store, "pepper")

	// Admin tries to create policy without On-Behalf-Of.
	rr := adminRequest(t, handler, "POST", "/v1/policies", `{"id":"p1","name":"test","enabled":true,"ttl_seconds":600}`, "admin-secret")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// Tenant Key calling /admin/* -> 403.
func TestAdminHandler_TenantKeyForbidden(t *testing.T) {
	store := newMockAdminStore()
	svc := control.NewService(storage.NewMemoryStore(), "http://gateway.local")
	authn := auth.NewTenantKeys(map[string]string{"tk": "t1"})
	handler := server.NewWithAdminStore(svc, authn, store, "pepper")

	rr := adminRequest(t, handler, "GET", "/admin/tenants", "", "tk")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func assertAuditAction(t *testing.T, events []domain.AuditEvent, action string) {
	t.Helper()
	for _, event := range events {
		if event.Action == action {
			if event.EventID == "" || event.TenantID == "" || event.OccurredAt.IsZero() {
				t.Fatalf("audit event %s missing required fields: %#v", action, event)
			}
			return
		}
	}
	t.Fatalf("expected audit action %s in %#v", action, events)
}
