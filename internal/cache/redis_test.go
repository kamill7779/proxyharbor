package cache

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/kamill7779/proxyharbor/internal/shared/domain"
	"github.com/redis/go-redis/v9"
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

func TestRedisInvalidatesCatalogAndPublishes(t *testing.T) {
	server := miniredis.RunT(t)
	cache, err := NewRedis(context.Background(), server.Addr(), "", 0)
	if err != nil {
		t.Fatalf("NewRedis() error = %v", err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	sub := cache.client.Subscribe(context.Background(), "proxyharbor:cache:invalidate")
	t.Cleanup(func() { _ = sub.Close() })
	if _, err := sub.ReceiveTimeout(context.Background(), time.Second); err != nil {
		t.Fatalf("subscribe confirm error = %v", err)
	}

	catalog := domain.Catalog{Proxies: []domain.Proxy{{ID: "proxy-1", Healthy: true, Weight: 1, HealthScore: 100}}}
	if err := cache.PutCatalog(context.Background(), catalog, time.Minute); err != nil {
		t.Fatalf("PutCatalog() error = %v", err)
	}
	if _, hit, err := cache.GetCatalog(context.Background()); err != nil || !hit {
		t.Fatalf("GetCatalog() hit=%v err=%v", hit, err)
	}
	if err := cache.InvalidateCatalog(context.Background()); err != nil {
		t.Fatalf("InvalidateCatalog() error = %v", err)
	}
	if _, hit, err := cache.GetCatalog(context.Background()); err != nil || hit {
		t.Fatalf("GetCatalog() after invalidate hit=%v err=%v", hit, err)
	}
	received, err := sub.ReceiveTimeout(context.Background(), time.Second)
	if err != nil {
		t.Fatalf("ReceiveMessage() error = %v", err)
	}
	msg := received.(*redis.Message)
	if !stringContains(msg.Payload, `"cache":"catalog"`) || !stringContains(msg.Payload, `"action":"invalidate"`) {
		t.Fatalf("published payload = %s", msg.Payload)
	}
}

func TestRedisInvalidatesLeaseAndPublishes(t *testing.T) {
	server := miniredis.RunT(t)
	cache, err := NewRedis(context.Background(), server.Addr(), "", 0)
	if err != nil {
		t.Fatalf("NewRedis() error = %v", err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	sub := cache.client.Subscribe(context.Background(), "proxyharbor:cache:invalidate")
	t.Cleanup(func() { _ = sub.Close() })
	if _, err := sub.ReceiveTimeout(context.Background(), time.Second); err != nil {
		t.Fatalf("subscribe confirm error = %v", err)
	}

	lease := domain.Lease{ID: "lease-1", TenantID: "tenant-a", PasswordHash: "hash-1", ExpiresAt: time.Now().Add(time.Minute)}
	if err := cache.PutLease(context.Background(), lease, time.Minute); err != nil {
		t.Fatalf("PutLease() error = %v", err)
	}
	if _, hit, err := cache.GetLease(context.Background(), lease.TenantID, lease.ID); err != nil || !hit {
		t.Fatalf("GetLease() hit=%v err=%v", hit, err)
	}
	if err := cache.InvalidateLease(context.Background(), lease.TenantID, lease.ID); err != nil {
		t.Fatalf("InvalidateLease() error = %v", err)
	}
	if _, hit, err := cache.GetLease(context.Background(), lease.TenantID, lease.ID); err != nil || hit {
		t.Fatalf("GetLease() after invalidate hit=%v err=%v", hit, err)
	}
	received, err := sub.ReceiveTimeout(context.Background(), time.Second)
	if err != nil {
		t.Fatalf("ReceiveMessage() error = %v", err)
	}
	msg := received.(*redis.Message)
	if !stringContains(msg.Payload, `"cache":"lease"`) || !stringContains(msg.Payload, `"action":"invalidate"`) {
		t.Fatalf("published payload = %s", msg.Payload)
	}
}

func TestRedisLocalInvalidationHelpersDoNotPublish(t *testing.T) {
	server := miniredis.RunT(t)
	cache, err := NewRedis(context.Background(), server.Addr(), "", 0)
	if err != nil {
		t.Fatalf("NewRedis() error = %v", err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	sub := cache.client.Subscribe(context.Background(), "proxyharbor:cache:invalidate")
	t.Cleanup(func() { _ = sub.Close() })
	if _, err := sub.ReceiveTimeout(context.Background(), time.Second); err != nil {
		t.Fatalf("subscribe confirm error = %v", err)
	}

	lease := domain.Lease{ID: "lease-local", TenantID: "tenant-a", PasswordHash: "hash-1", ExpiresAt: time.Now().Add(time.Minute)}
	if err := cache.PutLease(context.Background(), lease, time.Minute); err != nil {
		t.Fatalf("PutLease() error = %v", err)
	}
	if err := cache.PutCatalog(context.Background(), domain.Catalog{Proxies: []domain.Proxy{{ID: "proxy-local"}}}, time.Minute); err != nil {
		t.Fatalf("PutCatalog() error = %v", err)
	}
	if err := cache.InvalidateCatalogLocal(context.Background()); err != nil {
		t.Fatalf("InvalidateCatalogLocal() error = %v", err)
	}
	if _, hit, err := cache.GetCatalog(context.Background()); err != nil || hit {
		t.Fatalf("GetCatalog() after local invalidate hit=%v err=%v", hit, err)
	}
	if err := cache.InvalidateAllLeases(context.Background()); err != nil {
		t.Fatalf("InvalidateAllLeases() error = %v", err)
	}
	if _, hit, err := cache.GetLease(context.Background(), lease.TenantID, lease.ID); err != nil || hit {
		t.Fatalf("GetLease() after local invalidate hit=%v err=%v", hit, err)
	}
	if msg, err := sub.ReceiveTimeout(context.Background(), 100*time.Millisecond); err == nil {
		t.Fatalf("local helper published unexpected payload = %v", msg)
	}
}

func stringContains(s, needle string) bool {
	for i := 0; i+len(needle) <= len(s); i++ {
		if s[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
