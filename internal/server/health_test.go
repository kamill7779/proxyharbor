package server

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
	"github.com/kamill7779/proxyharbor/internal/metrics"
	"github.com/kamill7779/proxyharbor/internal/storage"
)

type fakeDependencyChecker struct {
	checks map[string]error
}

func (f fakeDependencyChecker) CheckDependencies(context.Context) map[string]error {
	return f.checks
}

type failingAuthReady struct{}

func (failingAuthReady) CheckAuthReady(context.Context) error {
	return errors.New("auth cache not initialized")
}

func TestHealthzIgnoresDependencyFailures(t *testing.T) {
	handler := NewWithOptions(
		control.NewService(storage.NewMemoryStore(), "http://localhost:8080"),
		auth.NewDynamicKeys(nil).WithAdminKey("admin-key-with-at-least-thirty-two-bytes"),
		Options{Dependency: fakeDependencyChecker{checks: map[string]error{"mysql": errors.New("down")}}},
	)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
}

func TestReadyzReportsLowCardinalityErrorKindsWithoutSecrets(t *testing.T) {
	status := auth.NewStatusTrackerWithRequired("redis", "degraded", true)
	status.Set("degraded", "redis")
	handler := NewWithOptions(
		control.NewService(storage.NewMemoryStore(), "http://localhost:8080"),
		auth.NewDynamicKeys(nil).WithAdminKey("admin-key-with-at-least-thirty-two-bytes"),
		Options{
			Dependency: fakeDependencyChecker{checks: map[string]error{
				"mysql":       errors.New("mysql dial failed with super-secret-password"),
				"redis_cache": errors.New("redis unavailable with super-secret-password"),
			}},
			InvalidationStatus: status,
			AuthReady:          failingAuthReady{},
		},
	)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz status = %d, want 503; body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "super-secret-password") {
		t.Fatalf("readyz leaked dependency error detail: %s", rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode readyz: %v", err)
	}
	errorKinds, ok := body["error_kinds"].(map[string]any)
	if !ok {
		t.Fatalf("readyz missing error_kinds: %#v", body)
	}
	for dependency, want := range map[string]string{
		"mysql":              "mysql",
		"redis_cache":        "redis",
		"cache_invalidation": "redis",
		"auth_cache":         "not_initialized",
	} {
		if got := errorKinds[dependency]; got != want {
			t.Fatalf("error_kinds[%s] = %v, want %s; body=%s", dependency, got, want, rr.Body.String())
		}
	}
}

func TestReadyzReportsDrainingWhenShutdownStarts(t *testing.T) {
	shutdownCtx, cancel := context.WithCancel(context.Background())
	handler := NewWithOptions(
		control.NewService(storage.NewMemoryStore(), "http://localhost:8080"),
		auth.NewDynamicKeys(nil).WithAdminKey("admin-key-with-at-least-thirty-two-bytes"),
		Options{
			Dependency:      fakeDependencyChecker{checks: map[string]error{"memory": nil}},
			ShutdownContext: shutdownCtx,
		},
	)
	handler.StartDraining()
	cancel()

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz status = %d, want 503; body=%s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode readyz: %v", err)
	}
	if body["status"] != "draining" {
		t.Fatalf("readyz status body = %v, want draining; body=%s", body["status"], rr.Body.String())
	}
	reasons, ok := body["reasons"].(map[string]any)
	if !ok || reasons["shutdown"] != "draining" {
		t.Fatalf("readyz reasons = %#v, want shutdown draining; body=%s", body["reasons"], rr.Body.String())
	}
}

func TestMetricsRefreshesRuntimeDependencyStatus(t *testing.T) {
	const adminKey = "admin-key-with-at-least-thirty-two-bytes"
	metrics.RecordRuntimeDependencyStatus("mysql", "degraded", "mysql")
	handler := NewWithOptions(
		control.NewService(storage.NewMemoryStore(), "http://localhost:8080"),
		auth.NewDynamicKeys(nil).WithAdminKey(adminKey),
		Options{Dependency: fakeDependencyChecker{checks: map[string]error{"mysql": nil}}},
	)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set(auth.HeaderName, adminKey)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `proxyharbor_runtime_dependency_status{dependency="mysql",result="ready",error_kind="none"} 1`) {
		t.Fatalf("metrics did not refresh mysql ready gauge: %s", rr.Body.String())
	}
}
