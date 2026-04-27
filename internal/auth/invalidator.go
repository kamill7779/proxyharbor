package auth

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// DefaultInvalidationChannel is the Redis pub/sub channel used to broadcast
// tenant_keys cache invalidation events between ProxyHarbor instances.
//
// Event payload is JSON of InvalidationEvent. Receivers MUST treat the
// payload as a hint only and re-read tenant_keys_version + tenant_keys
// from the authoritative store before mutating their cache. They MUST NOT
// trust the event to encode key material.
const DefaultInvalidationChannel = "proxyharbor:auth:tenant_keys:invalidate"

// InvalidationEvent is the lightweight pub/sub payload. It must NEVER carry
// plaintext keys, key hashes, the pepper, or admin keys.
type InvalidationEvent struct {
	Version    int64  `json:"version"`
	Reason     string `json:"reason"`
	TenantID   string `json:"tenant_id,omitempty"`
	KeyID      string `json:"key_id,omitempty"`
	InstanceID string `json:"instance_id,omitempty"`
	EmittedAt  int64  `json:"emitted_at"`
}

// Invalidator publishes auth cache invalidation events. Implementations
// must never block admin operations on transient failures; failures are
// to be logged/counted and returned for observability only.
type Invalidator interface {
	Publish(ctx context.Context, ev InvalidationEvent) error
	Close() error
}

// NoopInvalidator is used when no broadcast transport is configured.
type NoopInvalidator struct{}

func (NoopInvalidator) Publish(context.Context, InvalidationEvent) error { return nil }
func (NoopInvalidator) Close() error                                     { return nil }

// RedisInvalidator publishes invalidation events on a Redis pub/sub channel.
// Pub/sub is best-effort; receivers must still rely on polling as the
// authoritative convergence path.
type RedisInvalidator struct {
	client  *redis.Client
	channel string
	logger  *slog.Logger
}

// NewRedisInvalidator builds a publisher backed by the supplied Redis client.
// The caller owns the client lifecycle (Close on RedisInvalidator does not
// close the underlying client because it may be shared).
func NewRedisInvalidator(client *redis.Client, channel string, logger *slog.Logger) *RedisInvalidator {
	if channel == "" {
		channel = DefaultInvalidationChannel
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &RedisInvalidator{client: client, channel: channel, logger: logger}
}

func (r *RedisInvalidator) Publish(ctx context.Context, ev InvalidationEvent) error {
	if r == nil || r.client == nil {
		return nil
	}
	if ev.EmittedAt == 0 {
		ev.EmittedAt = time.Now().Unix()
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	pubCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := r.client.Publish(pubCtx, r.channel, payload).Err(); err != nil {
		r.logger.Warn("auth.invalidation.publish_failed",
			"channel", r.channel, "reason", ev.Reason, "err", err)
		return err
	}
	return nil
}

func (r *RedisInvalidator) Close() error { return nil }

// SubscribeInvalidations subscribes to the given channel and triggers
// dynamic.RefreshNow on every received message until ctx is done.
//
// Subscription failures are logged and retried with exponential backoff.
// Polling remains the authoritative convergence path; this only accelerates
// it. The function blocks; callers should run it in its own goroutine.
func SubscribeInvalidations(ctx context.Context, client *redis.Client, channel string, dynamic *DynamicStore, logger *slog.Logger) {
	if client == nil || dynamic == nil {
		return
	}
	if channel == "" {
		channel = DefaultInvalidationChannel
	}
	if logger == nil {
		logger = slog.Default()
	}
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		sub := client.Subscribe(ctx, channel)
		// Wait for subscription confirmation but do not block forever.
		confirmCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err := sub.Receive(confirmCtx)
		cancel()
		if err != nil {
			_ = sub.Close()
			if errors.Is(err, context.Canceled) {
				return
			}
			logger.Warn("auth.invalidation.subscribe_failed", "channel", channel, "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		logger.Info("auth.invalidation.subscribed", "channel", channel)
		ch := sub.Channel()
		consumeInvalidations(ctx, ch, dynamic, logger)
		_ = sub.Close()
		if ctx.Err() != nil {
			return
		}
		// Channel closed unexpectedly; reconnect.
	}
}

func consumeInvalidations(ctx context.Context, ch <-chan *redis.Message, dynamic *DynamicStore, logger *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			refreshCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if err := dynamic.RefreshNow(refreshCtx); err != nil {
				logger.Warn("auth.invalidation.refresh_failed", "err", err)
			}
			cancel()
			_ = msg
		}
	}
}
