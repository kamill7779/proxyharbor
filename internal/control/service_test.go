package control

import (
	"context"
	"testing"
	"time"

	"github.com/kamill7779/proxyharbor/internal/shared/domain"
	"github.com/kamill7779/proxyharbor/internal/storage"
)

func TestRenewLeaseUsesSingleGenerationBump(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemoryStore()
	_, err := store.UpsertProxy(ctx, domain.Proxy{ID: "proxy-a", Endpoint: "http://proxy.local:8080", Healthy: true, Weight: 1})
	if err != nil {
		t.Fatalf("UpsertProxy() error = %v", err)
	}
	now := time.Now().UTC()
	svc := NewService(store, "http://gateway.local")
	svc.now = func() time.Time { return now }

	principal := domain.Principal{TenantID: "tenant-a"}
	created, err := svc.CreateLease(ctx, principal, "idem-1", CreateLeaseRequest{
		Subject:     domain.Subject{Type: "user", ID: "user-a"},
		ResourceRef: domain.ResourceRef{Kind: "url", ID: "https://example.com/resource"},
	})
	if err != nil {
		t.Fatalf("CreateLease() error = %v", err)
	}
	if created.Generation != 1 {
		t.Fatalf("created generation = %d, want 1", created.Generation)
	}

	svc.now = func() time.Time { return created.CreatedAt.Add(time.Minute) }
	renewed, err := svc.RenewLease(ctx, principal, created.ID)
	if err != nil {
		t.Fatalf("RenewLease() error = %v", err)
	}
	if renewed.Generation != 2 {
		t.Fatalf("renewed generation = %d, want 2", renewed.Generation)
	}
}

func TestCreateLeaseReturnsTenantScopedGatewayUsername(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemoryStore()
	_, err := store.UpsertProxy(ctx, domain.Proxy{ID: "proxy-a", Endpoint: "http://proxy.local:8080", Healthy: true, Weight: 1})
	if err != nil {
		t.Fatalf("UpsertProxy() error = %v", err)
	}
	svc := NewService(store, "http://gateway.local")

	created, err := svc.CreateLease(ctx, domain.Principal{TenantID: "tenant-a"}, "idem-username", CreateLeaseRequest{
		Subject:     domain.Subject{Type: "user", ID: "user-a"},
		ResourceRef: domain.ResourceRef{Kind: "url", ID: "https://example.com/resource"},
	})
	if err != nil {
		t.Fatalf("CreateLease() error = %v", err)
	}
	if created.Username != "tenant-a|"+created.ID {
		t.Fatalf("created username = %q, want %q", created.Username, "tenant-a|"+created.ID)
	}
}
