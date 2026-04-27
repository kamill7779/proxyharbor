package cache

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/kamill7779/proxyharbor/internal/shared/domain"
)

func TestRedisLeaseCachePreservesPasswordHash(t *testing.T) {
	server := miniredis.RunT(t)
	cache, err := NewRedis(context.Background(), server.Addr(), "", 0)
	if err != nil {
		t.Fatalf("NewRedis() error = %v", err)
	}
	t.Cleanup(func() { _ = cache.Close() })

	lease := domain.Lease{
		ID:           "lease-1",
		TenantID:     "tenant-a",
		Username:     "tenant-a|lease-1",
		PasswordHash: "hash-1",
		ProxyID:      "proxy-1",
		ExpiresAt:    time.Now().Add(time.Minute),
	}
	if err := cache.PutLease(context.Background(), lease, time.Minute); err != nil {
		t.Fatalf("PutLease() error = %v", err)
	}
	got, hit, err := cache.GetLease(context.Background(), lease.TenantID, lease.ID)
	if err != nil {
		t.Fatalf("GetLease() error = %v", err)
	}
	if !hit {
		t.Fatal("GetLease() hit = false")
	}
	if got.PasswordHash != lease.PasswordHash {
		t.Fatalf("PasswordHash = %q, want %q", got.PasswordHash, lease.PasswordHash)
	}
	if got.Password != "" {
		t.Fatalf("Password = %q, want empty", got.Password)
	}
}
