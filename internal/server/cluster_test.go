package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kamill7779/proxyharbor/internal/auth"
	"github.com/kamill7779/proxyharbor/internal/control"
	"github.com/kamill7779/proxyharbor/internal/storage"
)

type failingClusterStore struct{}

func (failingClusterStore) HeartbeatInstance(context.Context, storage.InstanceHeartbeat) error {
	return nil
}

func (failingClusterStore) TryAcquireLock(context.Context, string, string, time.Duration) (bool, error) {
	return false, nil
}

func (failingClusterStore) GetClusterLock(context.Context, string) (storage.ClusterLock, bool, error) {
	return storage.ClusterLock{}, false, errors.New("mysql failed with secret dsn password")
}

func (failingClusterStore) DeleteExpiredLeasesBatch(context.Context, time.Time, int) (int, error) {
	return 0, nil
}

func TestAdminClusterClassifiesLeaderError(t *testing.T) {
	const adminKey = "admin-key-with-at-least-thirty-two-bytes"
	handler := NewWithOptions(
		control.NewService(storage.NewMemoryStore(), "http://localhost:8080"),
		auth.NewDynamicKeys(nil).WithAdminKey(adminKey),
		Options{
			AdminStore:   NewMemoryAdminStore(),
			ClusterStore: failingClusterStore{},
			ClusterSummary: map[string]any{
				"cluster_enabled": true,
			},
		},
	)

	req := httptest.NewRequest(http.MethodGet, "/admin/cluster", nil)
	req.Header.Set(auth.HeaderName, adminKey)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("admin cluster status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if strings.Contains(body, "secret") || strings.Contains(body, "password") || strings.Contains(body, "leader_error\"") {
		t.Fatalf("admin cluster leaked raw leader error: %s", body)
	}
	if !strings.Contains(body, `"leader_error_kind":"mysql"`) {
		t.Fatalf("admin cluster missing leader_error_kind mysql: %s", body)
	}
}
