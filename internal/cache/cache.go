// Package cache 为热路径数据提供可选的分布式缓存。
//
// 设计原则：
//   - Cache 接口必须能被任何实现替换（包括 noop），业务层调用方不需要感知是否启用了缓存。
//   - 缓存失败不应阻断主流程：所有 Get* 在 miss 或错误时返回 (zero, false, err) 调用方自行决定是否回源。
//   - 缓存使用 JSON 序列化以便跨语言兼容与 Redis CLI 调试。
package cache

import (
	"context"
	"time"

	"github.com/kamill7779/proxyharbor/internal/shared/domain"
)

// Cache 是热路径缓存的统一接口。
type Cache interface {
	// GetLease 在 gateway 验证路径热点上加速，未命中时返回 false。
	GetLease(ctx context.Context, tenantID, leaseID string) (domain.Lease, bool, error)
	PutLease(ctx context.Context, lease domain.Lease, ttl time.Duration) error
	InvalidateLease(ctx context.Context, tenantID, leaseID string) error

	// GetCatalog 用于 gateway 选择路径上的代理目录加速。
	GetCatalog(ctx context.Context) (domain.Catalog, bool, error)
	PutCatalog(ctx context.Context, catalog domain.Catalog, ttl time.Duration) error
	InvalidateCatalog(ctx context.Context) error

	// Close 释放底层连接。
	Close() error
}

// Noop 是不做任何缓存的实现，用于本地/测试或未配置 Redis 时的降级。
type Noop struct{}

func (Noop) GetLease(context.Context, string, string) (domain.Lease, bool, error) {
	return domain.Lease{}, false, nil
}
func (Noop) PutLease(context.Context, domain.Lease, time.Duration) error { return nil }
func (Noop) InvalidateLease(context.Context, string, string) error       { return nil }
func (Noop) GetCatalog(context.Context) (domain.Catalog, bool, error) {
	return domain.Catalog{}, false, nil
}
func (Noop) PutCatalog(context.Context, domain.Catalog, time.Duration) error { return nil }
func (Noop) InvalidateCatalog(context.Context) error                         { return nil }
func (Noop) Close() error                                                    { return nil }
