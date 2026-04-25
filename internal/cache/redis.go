package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kamill7779/proxyharbor/internal/shared/domain"
	"github.com/redis/go-redis/v9"
)

// Redis 用 Redis 作为热路径缓存。所有 key 形如 "ph:{kind}:{tenant}:{id}"。
type Redis struct {
	client *redis.Client
}

// NewRedis 建立一个 ping 通过的 Redis 缓存。如果 addr 为空，调用方应使用 Noop。
func NewRedis(ctx context.Context, addr, password string, db int) (*Redis, error) {
	if addr == "" {
		return nil, errors.New("redis addr 为空")
	}
	c := redis.NewClient(&redis.Options{
		Addr:         addr,
		Password:     password,
		DB:           db,
		DialTimeout:  3 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
	})
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := c.Ping(pingCtx).Err(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("redis ping: %w", err)
	}
	return &Redis{client: c}, nil
}

func (r *Redis) Close() error { return r.client.Close() }

func leaseKey(tenantID, leaseID string) string {
	return "ph:lease:" + tenantID + ":" + leaseID
}

func catalogKey(tenantID string) string {
	return "ph:catalog:" + tenantID
}

func (r *Redis) GetLease(ctx context.Context, tenantID, leaseID string) (domain.Lease, bool, error) {
	raw, err := r.client.Get(ctx, leaseKey(tenantID, leaseID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return domain.Lease{}, false, nil
	}
	if err != nil {
		return domain.Lease{}, false, err
	}
	var lease domain.Lease
	if err := json.Unmarshal(raw, &lease); err != nil {
		return domain.Lease{}, false, err
	}
	return lease, true, nil
}

func (r *Redis) PutLease(ctx context.Context, lease domain.Lease, ttl time.Duration) error {
	if ttl <= 0 {
		return nil
	}
	raw, err := json.Marshal(lease)
	if err != nil {
		return err
	}
	return r.client.Set(ctx, leaseKey(lease.TenantID, lease.ID), raw, ttl).Err()
}

func (r *Redis) InvalidateLease(ctx context.Context, tenantID, leaseID string) error {
	return r.client.Del(ctx, leaseKey(tenantID, leaseID)).Err()
}

func (r *Redis) GetCatalog(ctx context.Context, tenantID string) (domain.Catalog, bool, error) {
	raw, err := r.client.Get(ctx, catalogKey(tenantID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return domain.Catalog{}, false, nil
	}
	if err != nil {
		return domain.Catalog{}, false, err
	}
	var cat domain.Catalog
	if err := json.Unmarshal(raw, &cat); err != nil {
		return domain.Catalog{}, false, err
	}
	return cat, true, nil
}

func (r *Redis) PutCatalog(ctx context.Context, catalog domain.Catalog, ttl time.Duration) error {
	if ttl <= 0 {
		return nil
	}
	raw, err := json.Marshal(catalog)
	if err != nil {
		return err
	}
	return r.client.Set(ctx, catalogKey(catalog.TenantID), raw, ttl).Err()
}

func (r *Redis) InvalidateCatalog(ctx context.Context, tenantID string) error {
	return r.client.Del(ctx, catalogKey(tenantID)).Err()
}
