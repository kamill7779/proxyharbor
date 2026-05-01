package auth

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kamill7779/proxyharbor/internal/metrics"
	"github.com/redis/go-redis/v9"
)

const (
	DefaultInvalidationChannel = "proxyharbor:cache:invalidate"
	InvalidationSchemaVersion  = 1
)

const (
	CacheAuth    = "auth"
	CacheCatalog = "catalog"
	CacheLease   = "lease"

	ActionRefresh    = "refresh"
	ActionInvalidate = "invalidate"
)

var (
	ErrInvalidationDecode     = errors.New("cache invalidation decode")
	ErrInvalidationBadVersion = errors.New("cache invalidation bad version")
	ErrInvalidationBadCache   = errors.New("cache invalidation bad cache")
	ErrInvalidationBadAction  = errors.New("cache invalidation bad action")
)

type InvalidationMessage struct {
	SchemaVersion int       `json:"schema_version"`
	Cache         string    `json:"cache"`
	Action        string    `json:"action"`
	Version       int64     `json:"version,omitempty"`
	Reason        string    `json:"reason,omitempty"`
	Instance      string    `json:"instance,omitempty"`
	EmittedAt     time.Time `json:"emitted_at,omitempty"`
}

type InvalidationEvent = InvalidationMessage

type Status struct {
	Transport     string `json:"transport"`
	State         string `json:"state"`
	Required      bool   `json:"required,omitempty"`
	LastErrorKind string `json:"last_error_kind,omitempty"`
}

type StatusReporter interface {
	InvalidationStatus() Status
}

type staticStatusReporter struct {
	transport string
	state     string
	required  bool
}

type StatusTracker struct {
	transport     string
	required      bool
	state         atomic.Value
	lastErrorKind atomic.Value
}

func NewStatusTracker(transport, state string) *StatusTracker {
	return NewStatusTrackerWithRequired(transport, state, false)
}

func NewStatusTrackerWithRequired(transport, state string, required bool) *StatusTracker {
	t := &StatusTracker{transport: transport, required: required}
	t.state.Store(state)
	t.lastErrorKind.Store("")
	return t
}

func (t *StatusTracker) Set(state, errorKind string) {
	if state != "" {
		t.state.Store(state)
	}
	t.lastErrorKind.Store(errorKind)
}

func (t *StatusTracker) InvalidationStatus() Status {
	if t == nil {
		return Status{Transport: "none", State: "disabled"}
	}
	status := Status{Transport: t.transport, Required: t.required}
	if state, ok := t.state.Load().(string); ok {
		status.State = state
	}
	if errKind, ok := t.lastErrorKind.Load().(string); ok {
		status.LastErrorKind = errKind
	}
	return status
}

func NewStatusReporter(transport, state string) StatusReporter {
	return staticStatusReporter{transport: transport, state: state}
}

func (s staticStatusReporter) InvalidationStatus() Status {
	return Status{Transport: s.transport, State: s.state, Required: s.required}
}

type HotCacheInvalidator interface {
	InvalidateCatalogLocal(ctx context.Context) error
	InvalidateLeaseLocal(ctx context.Context, tenantID, leaseID string) error
	InvalidateAllLeases(ctx context.Context) error
}

type Invalidator interface {
	Publish(ctx context.Context, ev InvalidationEvent) error
	Close() error
}

type NoopInvalidator struct{}

func (NoopInvalidator) Publish(context.Context, InvalidationEvent) error { return nil }
func (NoopInvalidator) Close() error                                     { return nil }

type RedisInvalidator struct {
	client  *redis.Client
	channel string
	logger  *slog.Logger
}

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
	if ev.SchemaVersion == 0 {
		ev.SchemaVersion = InvalidationSchemaVersion
	}
	if ev.Cache == "" {
		ev.Cache = CacheAuth
	}
	if ev.Action == "" {
		ev.Action = ActionRefresh
	}
	if ev.EmittedAt.IsZero() {
		ev.EmittedAt = time.Now().UTC()
	}
	payload, err := EncodeInvalidationMessage(ev)
	if err != nil {
		recordInvalidation(ev.Cache, ev.Action, "redis", "error", ClassifyInvalidationError(err))
		return err
	}
	pubCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := r.client.Publish(pubCtx, r.channel, payload).Err(); err != nil {
		r.logger.Warn("cache.invalidation.publish_failed", "channel", r.channel, "cache", ev.Cache, "action", ev.Action, "error_kind", ClassifyInvalidationError(err))
		recordInvalidation(ev.Cache, ev.Action, "redis", "error", ClassifyInvalidationError(err))
		return err
	}
	recordInvalidation(ev.Cache, ev.Action, "redis", "published", "none")
	return nil
}

func (r *RedisInvalidator) Close() error { return nil }

func (r *RedisInvalidator) InvalidationStatus() Status {
	state := "configured"
	if r == nil || r.client == nil {
		state = "disabled"
	}
	return Status{Transport: "redis", State: state}
}

func EncodeInvalidationMessage(msg InvalidationMessage) ([]byte, error) {
	if msg.SchemaVersion == 0 {
		msg.SchemaVersion = InvalidationSchemaVersion
	}
	if msg.SchemaVersion != InvalidationSchemaVersion {
		return nil, ErrInvalidationBadVersion
	}
	if msg.Cache == "" {
		msg.Cache = CacheAuth
	}
	if msg.Action == "" {
		msg.Action = ActionRefresh
	}
	if !validCache(msg.Cache) {
		return nil, ErrInvalidationBadCache
	}
	if !validAction(msg.Cache, msg.Action) {
		return nil, ErrInvalidationBadAction
	}
	return json.Marshal(msg)
}

func DecodeInvalidationMessage(payload []byte) (InvalidationMessage, error) {
	var msg InvalidationMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		return InvalidationMessage{}, errors.Join(ErrInvalidationDecode, err)
	}
	if msg.SchemaVersion == 0 {
		msg.SchemaVersion = InvalidationSchemaVersion
	}
	if msg.SchemaVersion != InvalidationSchemaVersion {
		return InvalidationMessage{}, ErrInvalidationBadVersion
	}
	if msg.Cache == "" {
		msg.Cache = CacheAuth
	}
	if msg.Action == "" {
		msg.Action = ActionRefresh
	}
	if !validCache(msg.Cache) {
		return InvalidationMessage{}, ErrInvalidationBadCache
	}
	if !validAction(msg.Cache, msg.Action) {
		return InvalidationMessage{}, ErrInvalidationBadAction
	}
	return msg, nil
}

func ClassifyInvalidationError(err error) string {
	if err == nil {
		return "none"
	}
	if errors.Is(err, ErrInvalidationDecode) {
		return "decode"
	}
	if errors.Is(err, ErrInvalidationBadVersion) {
		return "bad_version"
	}
	if errors.Is(err, ErrInvalidationBadCache) {
		return "bad_cache"
	}
	if errors.Is(err, ErrInvalidationBadAction) {
		return "bad_action"
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "timeout"
	}
	var netErr net.Error
	if errors.Is(err, redis.Nil) || errors.As(err, &netErr) || strings.Contains(strings.ToLower(err.Error()), "redis:") {
		return "redis"
	}
	return "backend"
}

func SubscribeInvalidations(ctx context.Context, client *redis.Client, channel string, dynamic *DynamicStore, logger *slog.Logger) {
	SubscribeCacheInvalidations(ctx, client, channel, dynamic, nil, logger)
}

func SubscribeCacheInvalidations(ctx context.Context, client *redis.Client, channel string, dynamic *DynamicStore, hotCache HotCacheInvalidator, logger *slog.Logger) {
	SubscribeCacheInvalidationsWithStatus(ctx, client, channel, dynamic, hotCache, nil, logger)
}

func SubscribeCacheInvalidationsWithStatus(ctx context.Context, client *redis.Client, channel string, dynamic *DynamicStore, hotCache HotCacheInvalidator, status *StatusTracker, logger *slog.Logger) {
	if client == nil || (dynamic == nil && hotCache == nil) {
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
		confirmCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err := sub.Receive(confirmCtx)
		cancel()
		if err != nil {
			_ = sub.Close()
			if errors.Is(err, context.Canceled) {
				return
			}
			errorKind := ClassifyInvalidationError(err)
			recordInvalidation(CacheAuth, ActionRefresh, "redis", "error", errorKind)
			if status != nil {
				status.Set("degraded", errorKind)
			}
			logger.Warn("cache.invalidation.subscribe_failed", "channel", channel, "cache", CacheAuth, "transport", "redis", "error_kind", errorKind)
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
		if status != nil {
			status.Set("subscribed", "")
		}
		logger.Info("cache.invalidation.subscribed", "channel", channel)
		consumeInvalidations(ctx, client, sub, dynamic, hotCache, status, logger)
		_ = sub.Close()
		if ctx.Err() != nil {
			return
		}
	}
}

func consumeInvalidations(ctx context.Context, client *redis.Client, sub *redis.PubSub, dynamic *DynamicStore, hotCache HotCacheInvalidator, status *StatusTracker, logger *slog.Logger) {
	for {
		receiveCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		redisMsg, err := sub.ReceiveMessage(receiveCtx)
		cancel()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				return
			}
			if errors.Is(err, context.DeadlineExceeded) {
				pingCtx, pingCancel := context.WithTimeout(ctx, 2*time.Second)
				pingErr := client.Ping(pingCtx).Err()
				pingCancel()
				if pingErr == nil {
					continue
				}
				errorKind := ClassifyInvalidationError(pingErr)
				recordInvalidation(CacheAuth, ActionRefresh, "redis", "error", errorKind)
				if status != nil {
					status.Set("degraded", errorKind)
				}
				logger.Warn("cache.invalidation.redis_unhealthy", "transport", "redis", "error_kind", errorKind)
				return
			}
			errorKind := ClassifyInvalidationError(err)
			recordInvalidation(CacheAuth, ActionRefresh, "redis", "error", errorKind)
			if status != nil {
				status.Set("degraded", errorKind)
			}
			logger.Warn("cache.invalidation.receive_failed", "transport", "redis", "error_kind", errorKind)
			return
		}

		msg, err := DecodeInvalidationMessage([]byte(redisMsg.Payload))
		if err != nil {
			errorKind := ClassifyInvalidationError(err)
			recordInvalidation(CacheAuth, ActionRefresh, "redis", "dropped", errorKind)
			logger.Warn("cache.invalidation.decode_failed", "transport", "redis", "error_kind", ClassifyInvalidationError(err))
			continue
		}
		switch msg.Cache {
		case CacheAuth:
			recordInvalidation(CacheAuth, ActionRefresh, "redis", "received", "none")
			if dynamic == nil {
				recordInvalidation(CacheAuth, ActionRefresh, "redis", "dropped", "unsupported_receiver")
				continue
			}
			refreshCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if err := dynamic.RefreshNow(refreshCtx); err != nil {
				errorKind := "backend"
				recordInvalidation(CacheAuth, ActionRefresh, "redis", "error", errorKind)
				if status != nil {
					status.Set("degraded", errorKind)
				}
				logger.Warn("cache.invalidation.refresh_failed", "cache", CacheAuth, "transport", "redis", "error_kind", errorKind)
			} else {
				recordInvalidation(CacheAuth, ActionRefresh, "redis", "applied", "none")
				if status != nil {
					status.Set("subscribed", "")
				}
			}
			cancel()
		case CacheCatalog:
			recordInvalidation(CacheCatalog, ActionInvalidate, "redis", "received", "none")
			if hotCache == nil {
				recordInvalidation(CacheCatalog, ActionInvalidate, "redis", "dropped", "unsupported_receiver")
				continue
			}
			if err := hotCache.InvalidateCatalogLocal(ctx); err != nil {
				errorKind := "backend"
				recordInvalidation(CacheCatalog, ActionInvalidate, "redis", "error", errorKind)
				if status != nil {
					status.Set("degraded", errorKind)
				}
				logger.Warn("cache.invalidation.catalog_apply_failed", "transport", "redis", "error_kind", errorKind)
				continue
			}
			recordInvalidation(CacheCatalog, ActionInvalidate, "redis", "applied", "none")
			if status != nil {
				status.Set("subscribed", "")
			}
		case CacheLease:
			recordInvalidation(CacheLease, ActionInvalidate, "redis", "received", "none")
			if hotCache == nil {
				recordInvalidation(CacheLease, ActionInvalidate, "redis", "dropped", "unsupported_receiver")
				continue
			}
			if err := hotCache.InvalidateAllLeases(ctx); err != nil {
				errorKind := "backend"
				recordInvalidation(CacheLease, ActionInvalidate, "redis", "error", errorKind)
				if status != nil {
					status.Set("degraded", errorKind)
				}
				logger.Warn("cache.invalidation.lease_apply_failed", "transport", "redis", "error_kind", errorKind)
				continue
			}
			recordInvalidation(CacheLease, ActionInvalidate, "redis", "applied", "none")
			if status != nil {
				status.Set("subscribed", "")
			}
		}
	}
}

func validCache(cache string) bool {
	switch cache {
	case CacheAuth, CacheCatalog, CacheLease:
		return true
	default:
		return false
	}
}

func validAction(cache, action string) bool {
	switch cache {
	case CacheAuth:
		return action == ActionRefresh
	case CacheCatalog, CacheLease:
		return action == ActionInvalidate
	default:
		return false
	}
}

func recordInvalidation(cache, action, transport, result, errorKind string) {
	switch {
	case cache == CacheAuth && action == ActionRefresh && transport == "redis" && result == "published" && errorKind == "none":
		metrics.CacheInvalidationAuthRedisPublished.Inc()
	case cache == CacheAuth && action == ActionRefresh && transport == "redis" && result == "error" && errorKind == "backend":
		metrics.CacheInvalidationAuthRedisBackendError.Inc()
	case cache == CacheAuth && action == ActionRefresh && transport == "redis" && result == "error":
		incByErrorKind(errorKind, metrics.CacheInvalidationAuthRedisPublishError, metrics.CacheInvalidationAuthRedisTimeoutError, metrics.CacheInvalidationAuthRedisUnknownError)
	case cache == CacheAuth && action == ActionRefresh && transport == "redis" && result == "received" && errorKind == "none":
		metrics.CacheInvalidationAuthRedisReceived.Inc()
	case cache == CacheAuth && action == ActionRefresh && transport == "redis" && result == "applied" && errorKind == "none":
		metrics.CacheInvalidationAuthRedisApplied.Inc()
	case cache == CacheAuth && action == ActionRefresh && transport == "redis" && result == "dropped" && errorKind == "decode":
		metrics.CacheInvalidationAuthRedisDecodeError.Inc()
	case cache == CacheAuth && action == ActionRefresh && transport == "redis" && result == "dropped" && errorKind == "bad_action":
		metrics.CacheInvalidationAuthRedisBadActionDropped.Inc()
	case cache == CacheAuth && action == ActionRefresh && transport == "redis" && result == "dropped" && errorKind == "bad_version":
		metrics.CacheInvalidationAuthRedisBadVersionDropped.Inc()
	case cache == CacheAuth && action == ActionRefresh && transport == "redis" && result == "dropped" && errorKind == "unsupported_receiver":
		metrics.CacheInvalidationAuthRedisUnsupportedReceiver.Inc()
	case cache == CacheAuth && action == ActionRefresh && transport == "redis" && result == "dropped" && errorKind == "bad_cache":
		metrics.CacheInvalidationAuthRedisDropped.Inc()
	case cache == CacheAuth && action == ActionRefresh && transport == "polling" && result == "applied" && errorKind == "none":
		metrics.CacheInvalidationAuthPollingApplied.Inc()
	case cache == CacheCatalog && action == ActionInvalidate && transport == "redis" && result == "published" && errorKind == "none":
		metrics.CacheInvalidationCatalogRedisPublished.Inc()
	case cache == CacheCatalog && action == ActionInvalidate && transport == "redis" && result == "error" && errorKind == "backend":
		metrics.CacheInvalidationCatalogRedisBackendError.Inc()
	case cache == CacheCatalog && action == ActionInvalidate && transport == "redis" && result == "error":
		incByErrorKind(errorKind, metrics.CacheInvalidationCatalogRedisPublishError, metrics.CacheInvalidationCatalogRedisTimeoutError, metrics.CacheInvalidationCatalogRedisUnknownError)
	case cache == CacheCatalog && action == ActionInvalidate && transport == "redis" && result == "received" && errorKind == "none":
		metrics.CacheInvalidationCatalogRedisReceived.Inc()
	case cache == CacheCatalog && action == ActionInvalidate && transport == "redis" && result == "applied" && errorKind == "none":
		metrics.CacheInvalidationCatalogRedisApplied.Inc()
	case cache == CacheCatalog && action == ActionInvalidate && transport == "redis" && result == "dropped" && errorKind == "unsupported_receiver":
		metrics.CacheInvalidationCatalogRedisUnsupportedReceiver.Inc()
	case cache == CacheLease && action == ActionInvalidate && transport == "redis" && result == "published" && errorKind == "none":
		metrics.CacheInvalidationLeaseRedisPublished.Inc()
	case cache == CacheLease && action == ActionInvalidate && transport == "redis" && result == "error" && errorKind == "backend":
		metrics.CacheInvalidationLeaseRedisBackendError.Inc()
	case cache == CacheLease && action == ActionInvalidate && transport == "redis" && result == "error":
		incByErrorKind(errorKind, metrics.CacheInvalidationLeaseRedisPublishError, metrics.CacheInvalidationLeaseRedisTimeoutError, metrics.CacheInvalidationLeaseRedisUnknownError)
	case cache == CacheLease && action == ActionInvalidate && transport == "redis" && result == "received" && errorKind == "none":
		metrics.CacheInvalidationLeaseRedisReceived.Inc()
	case cache == CacheLease && action == ActionInvalidate && transport == "redis" && result == "applied" && errorKind == "none":
		metrics.CacheInvalidationLeaseRedisApplied.Inc()
	case cache == CacheLease && action == ActionInvalidate && transport == "redis" && result == "dropped" && errorKind == "unsupported_receiver":
		metrics.CacheInvalidationLeaseRedisUnsupportedReceiver.Inc()
	}
}

func incByErrorKind(errorKind string, redisCounter, timeoutCounter, unknownCounter *metrics.Counter) {
	switch errorKind {
	case "redis":
		redisCounter.Inc()
	case "timeout":
		timeoutCounter.Inc()
	default:
		unknownCounter.Inc()
	}
}
