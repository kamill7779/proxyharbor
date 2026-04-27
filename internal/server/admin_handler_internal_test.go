package server

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/kamill7779/proxyharbor/internal/auth"
	"github.com/kamill7779/proxyharbor/internal/shared/domain"
)

type errReader struct{}

func (errReader) Read([]byte) (int, error) {
	return 0, errors.New("entropy unavailable")
}

func TestRandomHexFailsClosedWhenEntropyUnavailable(t *testing.T) {
	oldReader := cryptoRandReader
	cryptoRandReader = errReader{}
	t.Cleanup(func() { cryptoRandReader = oldReader })

	if got, err := randomHex(8); err == nil || got != "" {
		t.Fatalf("randomHex should fail closed, got %q err=%v", got, err)
	}
	if got, err := generateKey(); err == nil || got != "" {
		t.Fatalf("generateKey should fail closed, got %q err=%v", got, err)
	}
}

type recordingInvalidator struct {
	mu     sync.Mutex
	events []auth.InvalidationEvent
}

func (r *recordingInvalidator) Publish(_ context.Context, ev auth.InvalidationEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return nil
}

func (r *recordingInvalidator) Close() error { return nil }

func (r *recordingInvalidator) snapshot() []auth.InvalidationEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]auth.InvalidationEvent, len(r.events))
	copy(out, r.events)
	return out
}

type blockingInvalidator struct{}

func (blockingInvalidator) Publish(ctx context.Context, _ auth.InvalidationEvent) error {
	<-ctx.Done()
	return ctx.Err()
}

func (blockingInvalidator) Close() error { return nil }

type tenantStatusStore struct {
	tenant domain.Tenant
	keys   []auth.TenantKey
}

func (s *tenantStatusStore) GetTenant(context.Context, string) (domain.Tenant, error) {
	return s.tenant, nil
}
func (s *tenantStatusStore) ListTenants(context.Context) ([]domain.Tenant, error) { return nil, nil }
func (s *tenantStatusStore) CreateTenant(context.Context, domain.Tenant) error    { return nil }
func (s *tenantStatusStore) UpdateTenant(_ context.Context, _ string, _ *string, status *string) error {
	if status != nil {
		s.tenant.Enabled = *status == "active"
		for i := range s.keys {
			now := s.keys[i].CreatedAt
			s.keys[i].RevokedAt = &now
		}
	}
	return nil
}
func (s *tenantStatusStore) SoftDeleteTenant(context.Context, string) error { return nil }
func (s *tenantStatusStore) ListTenantKeys(context.Context, string) ([]auth.TenantKey, error) {
	return s.keys, nil
}
func (s *tenantStatusStore) CreateTenantKey(context.Context, auth.TenantKey) error { return nil }
func (s *tenantStatusStore) RevokeTenantKey(context.Context, string, string) error { return nil }
func (s *tenantStatusStore) IncrementTenantKeysVersion(context.Context) error      { return nil }
func (s *tenantStatusStore) AppendAuditEvents(context.Context, []domain.AuditEvent) error {
	return nil
}

func TestTenantStatusChangeEmitsInvalidationEvenWhenStoreRevokedKeys(t *testing.T) {
	store := &tenantStatusStore{
		tenant: domain.Tenant{ID: "t1", Enabled: true},
		keys:   []auth.TenantKey{{ID: "k1", TenantID: "t1"}},
	}
	invalidator := &recordingInvalidator{}
	handler := newAdminHandler(store, "pepper", invalidator, "inst-test")
	req := httptest.NewRequest(http.MethodPatch, "/admin/tenants/t1", bytes.NewBufferString(`{"status":"disabled"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.tenantByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	var events []auth.InvalidationEvent
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		events = invalidator.snapshot()
		if len(events) == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if len(events) != 1 {
		t.Fatalf("expected invalidation event, got %#v", events)
	}
	if events[0].Reason != "tenant_status_change" || events[0].TenantID != "t1" {
		t.Fatalf("unexpected event: %#v", events[0])
	}
}

func TestTenantStatusChangeDoesNotWaitForSlowInvalidator(t *testing.T) {
	store := &tenantStatusStore{
		tenant: domain.Tenant{ID: "t1", Enabled: true},
		keys:   []auth.TenantKey{{ID: "k1", TenantID: "t1"}},
	}
	handler := newAdminHandler(store, "pepper", blockingInvalidator{}, "inst-test")
	req := httptest.NewRequest(http.MethodPatch, "/admin/tenants/t1", bytes.NewBufferString(`{"status":"disabled"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	start := time.Now()
	handler.tenantByID(rr, req)
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("admin response waited for invalidator: %s", elapsed)
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", rr.Code, rr.Body.String())
	}
}
