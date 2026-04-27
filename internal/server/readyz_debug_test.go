package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kamill7779/proxyharbor/internal/auth"
	"github.com/kamill7779/proxyharbor/internal/control"
	"github.com/kamill7779/proxyharbor/internal/server"
	"github.com/kamill7779/proxyharbor/internal/storage"
)

type fakeAuthReady struct{ ready bool }

func (f fakeAuthReady) CheckAuthReady(context.Context) error {
	if f.ready {
		return nil
	}
	return errors.New("not ready")
}

type fakeAuthSnapshot struct{ snap auth.Snapshot }

func (f fakeAuthSnapshot) AuthSnapshot() auth.Snapshot { return f.snap }

func TestReadyz_AuthCacheGate(t *testing.T) {
	svc := control.NewService(storage.NewMemoryStore(), "http://gateway.local")
	authn := auth.NewLegacy("legacy")
	handler := server.NewWithOptions(svc, authn, server.Options{
		AuthReady:  fakeAuthReady{ready: false},
		InstanceID: "inst-test",
	})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when auth not ready, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "auth_cache") || !strings.Contains(rr.Body.String(), "not_initialized") {
		t.Fatalf("readyz body should include auth_cache reason: %s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "inst-test") {
		t.Fatalf("readyz body should include instance_id: %s", rr.Body.String())
	}

	handler = server.NewWithOptions(svc, authn, server.Options{
		AuthReady: fakeAuthReady{ready: true},
	})
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 when auth ready, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestReadyz_NoAuthCheck_LegacyMode(t *testing.T) {
	svc := control.NewService(storage.NewMemoryStore(), "http://gateway.local")
	authn := auth.NewLegacy("legacy")
	handler := server.NewWithOptions(svc, authn, server.Options{})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("legacy readyz should be 200 without auth check, got %d", rr.Code)
	}
}

func TestDebugAuthCache_AdminOnly_NoSecrets(t *testing.T) {
	svc := control.NewService(storage.NewMemoryStore(), "http://gateway.local")
	authn := auth.NewLegacy("legacy").WithAdminKey("admin-secret-key-of-32+ chars-min")
	snap := auth.Snapshot{Initialized: true, Version: 42, Entries: 7, StaleSeconds: 1}
	handler := server.NewWithOptions(svc, authn, server.Options{
		AdminStore:   server.NewMemoryAdminStore(),
		AuthSnapshot: fakeAuthSnapshot{snap: snap},
		InstanceID:   "inst-debug",
	})

	// Unauthenticated -> auth failure (401).
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/debug/auth-cache", nil))
	if rr.Code == http.StatusOK {
		t.Fatalf("debug endpoint must require auth, got 200")
	}

	// Tenant key (non-admin) -> 403.
	authnTenant := auth.NewTenantKeys(map[string]string{"tk": "t1"})
	tenantHandler := server.NewWithOptions(svc, authnTenant, server.Options{
		AdminStore:   server.NewMemoryAdminStore(),
		AuthSnapshot: fakeAuthSnapshot{snap: snap},
	})
	rr = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/debug/auth-cache", nil)
	req.Header.Set(auth.HeaderName, "tk")
	tenantHandler.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("tenant key should get 403, got %d body=%s", rr.Code, rr.Body.String())
	}

	// Admin -> 200 with snapshot, no secrets.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/debug/auth-cache", nil)
	req.Header.Set(auth.HeaderName, "admin-secret-key-of-32+ chars-min")
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("admin debug status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, forbidden := range []string{"phk_", "pepper", "admin-secret", "key_hash"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("debug body must not contain %q: %s", forbidden, body)
		}
	}
	var resp struct {
		InstanceID string        `json:"instance_id"`
		AuthMode   string        `json:"auth_mode"`
		Cache      auth.Snapshot `json:"cache"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.InstanceID != "inst-debug" || resp.Cache.Version != 42 || resp.Cache.Entries != 7 {
		t.Fatalf("unexpected debug payload: %+v", resp)
	}
}

func TestDebugAuthCacheMetrics_AdminOnly_NoSecrets(t *testing.T) {
	svc := control.NewService(storage.NewMemoryStore(), "http://gateway.local")
	authn := auth.NewLegacy("legacy").WithAdminKey("admin-secret-key-of-32+ chars-min")
	snap := auth.Snapshot{Initialized: true, Version: 42, Entries: 7, StaleSeconds: 3, RefreshFailures: 1, RefreshSuccess: 2}
	handler := server.NewWithOptions(svc, authn, server.Options{
		AdminStore:   server.NewMemoryAdminStore(),
		AuthSnapshot: fakeAuthSnapshot{snap: snap},
		InstanceID:   "inst-metrics",
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/debug/auth-cache/metrics", nil)
	req.Header.Set(auth.HeaderName, "admin-secret-key-of-32+ chars-min")
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("metrics status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, forbidden := range []string{"phk_", "pepper", "admin-secret", "key_hash"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("metrics body must not contain %q: %s", forbidden, body)
		}
	}
	var resp struct {
		InstanceID       string `json:"instance_id"`
		Initialized      bool   `json:"auth_cache_initialized"`
		Version          int64  `json:"auth_cache_version"`
		Entries          int    `json:"auth_cache_entries"`
		StaleSeconds     int64  `json:"auth_cache_stale_seconds"`
		RefreshFailures  int64  `json:"auth_cache_refresh_failures"`
		RefreshSuccesses int64  `json:"auth_cache_refresh_success"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.InstanceID != "inst-metrics" || !resp.Initialized || resp.Version != 42 || resp.Entries != 7 || resp.StaleSeconds != 3 || resp.RefreshFailures != 1 || resp.RefreshSuccesses != 2 {
		t.Fatalf("unexpected metrics payload: %+v", resp)
	}
}

func TestHealthzStaysSimple(t *testing.T) {
	svc := control.NewService(storage.NewMemoryStore(), "http://gateway.local")
	authn := auth.NewLegacy("legacy")
	handler := server.NewWithOptions(svc, authn, server.Options{InstanceID: "inst-health"})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("health status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, forbidden := range []string{"instance_id", "auth_cache", "stale_seconds"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("healthz should stay simple, found %q in %s", forbidden, body)
		}
	}
}
