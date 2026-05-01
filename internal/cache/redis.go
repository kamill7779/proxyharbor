package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kamill7779/proxyharbor/internal/auth"
	"github.com/kamill7779/proxyharbor/internal/shared/domain"
	"github.com/redis/go-redis/v9"
)

type Redis struct {
	client      *redis.Client
	invalidator auth.Invalidator
}

type cachedLease struct {
	domain.Lease
	PasswordHash string `json:"password_hash"`
}

func NewRedis(ctx context.Context, addr, password string, db int) (*Redis, error) {
	if addr == "" {
		return nil, errors.New("redis addr is empty")
	}
	client := redis.NewClient(&redis.Options{
		Addr:         addr,
		Password:     password,
		DB:           db,
		DialTimeout:  3 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
	})
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("redis ping: %w", err)
	}
	return &Redis{client: client, invalidator: auth.NewRedisInvalidator(client, auth.DefaultInvalidationChannel, nil)}, nil
}

func (r *Redis) Close() error { return r.client.Close() }

func (r *Redis) Check(ctx context.Context) error {
	pingCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	return r.client.Ping(pingCtx).Err()
}

func leaseKey(tenantID, leaseID string) string {
	return "ph:lease:" + tenantID + ":" + leaseID
}

func catalogKey() string {
	return "ph:catalog:global"
}

func (r *Redis) GetLease(ctx context.Context, tenantID, leaseID string) (domain.Lease, bool, error) {
	raw, err := r.client.Get(ctx, leaseKey(tenantID, leaseID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return domain.Lease{}, false, nil
	}
	if err != nil {
		return domain.Lease{}, false, err
	}
	var cached cachedLease
	if err := json.Unmarshal(raw, &cached); err != nil {
		return domain.Lease{}, false, err
	}
	lease := cached.Lease
	lease.PasswordHash = cached.PasswordHash
	return lease, true, nil
}

func (r *Redis) PutLease(ctx context.Context, lease domain.Lease, ttl time.Duration) error {
	if ttl <= 0 {
		return nil
	}
	lease.Password = ""
	raw, err := json.Marshal(cachedLease{Lease: lease, PasswordHash: lease.PasswordHash})
	if err != nil {
		return err
	}
	return r.client.Set(ctx, leaseKey(lease.TenantID, lease.ID), raw, ttl).Err()
}

func (r *Redis) InvalidateLease(ctx context.Context, tenantID, leaseID string) error {
	if err := r.InvalidateLeaseLocal(ctx, tenantID, leaseID); err != nil {
		return err
	}
	_ = r.invalidator.Publish(ctx, auth.InvalidationEvent{Cache: auth.CacheLease, Action: auth.ActionInvalidate, Reason: "lease_change"})
	return nil
}

func (r *Redis) GetCatalog(ctx context.Context) (domain.Catalog, bool, error) {
	raw, err := r.client.Get(ctx, catalogKey()).Bytes()
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
	return r.client.Set(ctx, catalogKey(), raw, ttl).Err()
}

func (r *Redis) InvalidateCatalogLocal(ctx context.Context) error {
	return r.client.Del(ctx, catalogKey()).Err()
}

func (r *Redis) InvalidateLeaseLocal(ctx context.Context, tenantID, leaseID string) error {
	return r.client.Del(ctx, leaseKey(tenantID, leaseID)).Err()
}

func (r *Redis) InvalidateAllLeases(ctx context.Context) error {
	var cursor uint64
	for {
		keys, next, err := r.client.Scan(ctx, cursor, "ph:lease:*", 100).Result()
		if err != nil {
			return err
		}
		if len(keys) > 0 {
			if err := r.client.Del(ctx, keys...).Err(); err != nil {
				return err
			}
		}
		cursor = next
		if cursor == 0 {
			return nil
		}
	}
}

func (r *Redis) InvalidateCatalog(ctx context.Context) error {
	if err := r.InvalidateCatalogLocal(ctx); err != nil {
		return err
	}
	_ = r.invalidator.Publish(ctx, auth.InvalidationEvent{Cache: auth.CacheCatalog, Action: auth.ActionInvalidate, Reason: "catalog_change"})
	return nil
}
