package storage

import (
	"context"
	"testing"
	"time"

	"github.com/kamill7779/proxyharbor/internal/shared/domain"
)

func TestMemoryStoreCreateLeaseDoesNotPersistPlaintextPassword(t *testing.T) {
	store := NewMemoryStore()
	lease := domain.Lease{
		ID:           "lease_1",
		TenantID:     "tenant_1",
		Generation:   1,
		Password:     "plaintext",
		PasswordHash: "hash",
		ExpiresAt:    time.Now().Add(time.Hour),
	}
	scope := IdempotencyScope{TenantID: lease.TenantID, StableSubjectID: "subject", ResourceRef: "resource", RequestKind: "create_lease", Key: "idem"}

	created, err := store.CreateLease(context.Background(), scope, lease)
	if err != nil {
		t.Fatalf("CreateLease() error = %v", err)
	}
	if created.Password != "" {
		t.Fatalf("CreateLease() returned persisted plaintext password %q", created.Password)
	}
	if created.PasswordHash != lease.PasswordHash {
		t.Fatalf("CreateLease() PasswordHash = %q, want %q", created.PasswordHash, lease.PasswordHash)
	}

	stored, err := store.GetLease(context.Background(), lease.TenantID, lease.ID)
	if err != nil {
		t.Fatalf("GetLease() error = %v", err)
	}
	if stored.Password != "" {
		t.Fatalf("GetLease() returned plaintext password %q", stored.Password)
	}
	if stored.PasswordHash != lease.PasswordHash {
		t.Fatalf("GetLease() PasswordHash = %q, want %q", stored.PasswordHash, lease.PasswordHash)
	}

	replayed, ok, err := store.GetLeaseByIdempotency(context.Background(), scope)
	if err != nil {
		t.Fatalf("GetLeaseByIdempotency() error = %v", err)
	}
	if !ok {
		t.Fatal("GetLeaseByIdempotency() ok = false")
	}
	if replayed.Password != "" {
		t.Fatalf("GetLeaseByIdempotency() returned plaintext password %q", replayed.Password)
	}
	if replayed.PasswordHash != lease.PasswordHash {
		t.Fatalf("GetLeaseByIdempotency() PasswordHash = %q, want %q", replayed.PasswordHash, lease.PasswordHash)
	}
}
