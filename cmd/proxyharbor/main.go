// Command proxyharbor is a single binary that can run controller, gateway, or both.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/kamill7779/proxyharbor/internal/auth"
	"github.com/kamill7779/proxyharbor/internal/cache"
	"github.com/kamill7779/proxyharbor/internal/cluster"
	"github.com/kamill7779/proxyharbor/internal/config"
	"github.com/kamill7779/proxyharbor/internal/control"
	"github.com/kamill7779/proxyharbor/internal/control/health"
	"github.com/kamill7779/proxyharbor/internal/control/selector"
	"github.com/kamill7779/proxyharbor/internal/server"
	"github.com/kamill7779/proxyharbor/internal/storage"
	"github.com/redis/go-redis/v9"
)

func main() {
	cfg, err := config.Load(os.Args[1:])
	logger := newLogger(cfg)
	slog.SetDefault(logger)
	if err != nil {
		logger.Error("load config", "err", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store, closeStore, err := openStore(ctx, cfg, logger)
	if err != nil {
		logger.Error("open store", "err", err)
		os.Exit(1)
	}
	defer closeStore()

	cacheImpl, closeCache := openCache(ctx, cfg, logger)
	defer closeCache()

	selectorImpl, closeSelector, err := openSelector(ctx, cfg, logger)
	if err != nil {
		logger.Error("open selector", "err", err)
		os.Exit(1)
	}
	defer closeSelector()

	healthRecorder := health.NewCoalescingRecorder(store, health.RecorderOptions{
		BufferSize:    cfg.HealthBufferMax,
		FlushInterval: cfg.HealthFlushInterval,
		Policy:        health.ScoringPolicyForProfile(cfg.ScoringProfile),
	})
	defer healthRecorder.Close(context.Background())

	svc := control.NewService(store, cfg.GatewayURL)
	svc.SetLogger(logger)
	svc.SetCache(cacheImpl, cfg.CacheTTL)
	svc.SetAllowInternalProxyEndpoint(cfg.AllowInternalProxyEndpoint)
	svc.SetSelector(selectorImpl)

	role := server.Role(cfg.Role)
	instanceID := resolveInstanceID(cfg)

	// Optional Redis client reused for cache/selector pub/sub invalidation.
	// Cache/selector keep their own clients to avoid coupling timeouts;
	// this client is dedicated to pub/sub so subscribe blocking does not
	// stall normal traffic.
	var invalidationClient *redis.Client
	if cfg.RedisAddr != "" && cfg.AuthInvalidation != "polling" {
		invalidationClient = redis.NewClient(&redis.Options{
			Addr:         cfg.RedisAddr,
			Password:     cfg.RedisPassword,
			DB:           cfg.RedisDB,
			DialTimeout:  3 * time.Second,
			ReadTimeout:  0, // pub/sub uses blocking reads
			WriteTimeout: 2 * time.Second,
		})
	}
	defer func() {
		if invalidationClient != nil {
			_ = invalidationClient.Close()
		}
	}()

	// Verify MySQL schema before starting any traffic-serving goroutine.
	if mysqlStore, ok := store.(*storage.MySQLStore); ok {
		if err := storage.EnsureDynamicAuthSchema(ctx, mysqlStore.DB()); err != nil {
			logger.Error("schema check failed", "err", err)
			os.Exit(1)
		}
	}
	if cfg.ClusterEnabled {
		clusterStore, ok := store.(storage.ClusterStore)
		if !ok {
			logger.Error("cluster mode requires cluster-capable storage")
			os.Exit(1)
		}
		clusterCtx, clusterCancel := context.WithCancel(ctx)
		defer clusterCancel()
		go cluster.Runner{
			Store:             clusterStore,
			InstanceID:        instanceID,
			Role:              cfg.Role,
			Version:           server.Version,
			ConfigFingerprint: configFingerprint(cfg),
			StartedAt:         time.Now().UTC(),
			HeartbeatInterval: cfg.InstanceHeartbeatInterval,
			LeaderLeaseTTL:    cfg.LeaderLeaseTTL,
			MaintenanceEvery:  cfg.MaintenanceInterval,
			MaintenanceLimit:  cfg.MaintenanceBatchSize,
			Logger:            logger,
		}.Run(clusterCtx)
	}

	authn, dynamicStore, authClose, err := buildAuthenticator(ctx, cfg, store)
	if err != nil {
		logger.Error("open authenticator", "err", err)
		os.Exit(1)
	}
	defer authClose()

	var invalidator auth.Invalidator = auth.NoopInvalidator{}
	if invalidationClient != nil {
		invalidator = auth.NewRedisInvalidator(invalidationClient, auth.DefaultInvalidationChannel, logger)
		if dynamicStore != nil {
			subCtx, subCancel := context.WithCancel(ctx)
			go auth.SubscribeInvalidations(subCtx, invalidationClient, auth.DefaultInvalidationChannel, dynamicStore, logger)
			defer subCancel()
		}
	}

	adminStore := buildAdminStore(store)
	opts := server.Options{
		Role:           role,
		HealthRecorder: healthRecorder,
		Dependency:     dependencyChecks{store: store, cache: cacheImpl, selector: selectorImpl},
		AdminStore:     adminStore,
		Pepper:         cfg.KeyPepper,
		Invalidator:    invalidator,
		InstanceID:     instanceID,
		ClusterStore:   clusterStoreForOptions(store),
		ClusterSummary: clusterSummary(cfg),
	}
	if dynamicStore != nil {
		readyChecker := dynamicAuthReady{store: dynamicStore}
		opts.AuthReady = readyChecker
		opts.AuthSnapshot = readyChecker
	}
	handler := server.NewWithOptions(svc, authn, opts)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logger.Info("proxyharbor listening",
			"role", cfg.Role, "addr", cfg.Addr, "storage", cfg.StorageDriver,
			"redis", cfg.RedisAddr != "", "selector", cfg.Selector,
			"auth_cache_entries", authn.CacheEntries(),
			"auth_invalidation", authInvalidationLabel(cfg, invalidationClient != nil),
			"instance_id", instanceID)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("listen", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("shutdown signal received")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown", "err", err)
	}
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer drainCancel()
	healthRecorder.Close(drainCtx)
}

func buildAdminStore(store storage.Store) server.AdminStore {
	if mysqlStore, ok := store.(*storage.MySQLStore); ok {
		return server.NewMySQLAdminStore(mysqlStore.DB())
	}
	return server.NewMemoryAdminStore()
}

func buildAuthenticator(ctx context.Context, cfg config.Config, store storage.Store) (*auth.Authenticator, *auth.DynamicStore, func(), error) {
	mysqlStore, ok := store.(*storage.MySQLStore)
	if !ok {
		return nil, nil, func() {}, errors.New("v0.2.0 auth requires mysql storage")
	}
	dynamicStore, err := auth.NewDynamicStore(auth.NewMySQLKeyStore(mysqlStore.DB()), []byte(cfg.KeyPepper), cfg.AuthRefreshInterval)
	if err != nil {
		return nil, nil, func() {}, err
	}
	refreshCtx, cancel := context.WithCancel(ctx)
	go dynamicStore.Run(refreshCtx)
	return auth.NewDynamicKeys(dynamicStore).WithAdminKey(cfg.AdminKey), dynamicStore, cancel, nil
}

// dynamicAuthReady satisfies server.AuthReadyChecker / AuthSnapshotProvider
// without leaking secrets.
type dynamicAuthReady struct {
	store *auth.DynamicStore
}

func (d dynamicAuthReady) CheckAuthReady(context.Context) error {
	if d.store == nil {
		return nil
	}
	if !d.store.Initialized() {
		return errors.New("dynamic auth cache not initialized")
	}
	return nil
}

func (d dynamicAuthReady) AuthSnapshot() auth.Snapshot {
	if d.store == nil {
		return auth.Snapshot{}
	}
	return d.store.Snapshot()
}

func resolveInstanceID(cfg config.Config) string {
	if cfg.InstanceID != "" {
		return cfg.InstanceID
	}
	if v := os.Getenv("HOSTNAME"); v != "" {
		return v + "-" + shortRand()
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "proxyharbor"
	}
	return host + "-" + shortRand()
}

func configFingerprint(cfg config.Config) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("role=%s;storage=%s;selector=%s;auth_refresh=%s;cluster=%t",
		cfg.Role, cfg.StorageDriver, cfg.Selector, cfg.AuthRefreshInterval, cfg.ClusterEnabled)))
	return hex.EncodeToString(sum[:8])
}

func clusterSummary(cfg config.Config) map[string]any {
	return map[string]any{
		"cluster_enabled":       cfg.ClusterEnabled,
		"storage":               string(cfg.StorageDriver),
		"selector":              cfg.Selector,
		"auth_refresh_interval": cfg.AuthRefreshInterval.String(),
		"config_fingerprint":    configFingerprint(cfg),
		"version":               server.Version,
	}
}

func clusterStoreForOptions(store storage.Store) storage.ClusterStore {
	clusterStore, _ := store.(storage.ClusterStore)
	return clusterStore
}

func shortRand() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "0000"
	}
	return hex.EncodeToString(b[:])
}

func authInvalidationLabel(cfg config.Config, redisActive bool) string {
	mode := strings.ToLower(strings.TrimSpace(cfg.AuthInvalidation))
	switch mode {
	case "polling":
		return "polling"
	case "redis":
		if redisActive {
			return "redis"
		}
		return "redis-unconfigured"
	default:
		if redisActive {
			return "auto-redis"
		}
		return "polling"
	}
}

func newLogger(cfg config.Config) *slog.Logger {
	level := slog.LevelInfo
	if cfg.LogLevel == "debug" {
		level = slog.LevelDebug
	}
	options := &slog.HandlerOptions{Level: level}
	if cfg.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(os.Stdout, options))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, options))
}

type checker interface {
	Check(context.Context) error
}

type dependencyChecks struct {
	store    storage.Store
	cache    cache.Cache
	selector selector.ProxySelector
}

func (d dependencyChecks) CheckDependencies(ctx context.Context) map[string]error {
	checks := map[string]error{}
	if store, ok := d.store.(storage.DependencyChecker); ok {
		for name, err := range store.CheckDependencies(ctx) {
			checks[name] = err
		}
	}
	if redisCache, ok := d.cache.(checker); ok {
		checks["redis_cache"] = redisCache.Check(ctx)
	}
	if redisSelector, ok := d.selector.(checker); ok {
		checks["redis_selector"] = redisSelector.Check(ctx)
	}
	return checks
}

func openSelector(ctx context.Context, cfg config.Config, logger *slog.Logger) (selector.ProxySelector, func(), error) {
	if cfg.Selector != selector.NameZFair {
		return nil, func() {}, errors.New("unsupported selector")
	}
	if cfg.RedisAddr == "" {
		if cfg.SelectorRedisRequired {
			return nil, func() {}, errors.New("selector=zfair requires redis addr")
		}
		return nil, func() {}, nil
	}
	sel, err := retry(ctx, 30*time.Second, logger, "redis selector", func(attemptCtx context.Context) (*selector.RedisZFair, error) {
		return selector.NewRedisZFair(attemptCtx, selector.RedisZFairConfig{
			Addr:             cfg.RedisAddr,
			Password:         cfg.RedisPassword,
			DB:               cfg.RedisDB,
			Quantum:          float64(cfg.ZFairQuantum),
			DefaultLatencyMS: int64(cfg.ZFairDefaultLatencyMS),
			MaxPromote:       int64(cfg.ZFairMaxPromote),
			MaxScan:          int64(cfg.ZFairMaxPromote),
		})
	})
	if err != nil {
		return nil, func() {}, err
	}
	return sel, func() { _ = sel.Close() }, nil
}

func openStore(ctx context.Context, cfg config.Config, logger *slog.Logger) (storage.Store, func(), error) {
	switch cfg.StorageDriver {
	case config.DriverMySQL:
		s, err := retry(ctx, 30*time.Second, logger, "mysql", func(attemptCtx context.Context) (*storage.MySQLStore, error) {
			return storage.NewMySQLStore(attemptCtx, cfg.MySQLDSN, cfg.MySQLMaxOpen, cfg.MySQLMaxIdle, cfg.MySQLConnMaxAge)
		})
		if err != nil {
			return nil, func() {}, err
		}
		return s, func() { _ = s.Close() }, nil
	case config.DriverMemory:
		logger.Warn("using memory storage; only for development and tests")
		return storage.NewMemoryStore(), func() {}, nil
	default:
		return nil, func() {}, errors.New("unknown storage driver")
	}
}

func openCache(ctx context.Context, cfg config.Config, logger *slog.Logger) (cache.Cache, func()) {
	if cfg.RedisAddr == "" {
		logger.Warn("redis is not configured; cache falls back to noop")
		return cache.Noop{}, func() {}
	}
	r, err := retry(ctx, 30*time.Second, logger, "redis cache", func(attemptCtx context.Context) (*cache.Redis, error) {
		return cache.NewRedis(attemptCtx, cfg.RedisAddr, cfg.RedisPassword, cfg.RedisDB)
	})
	if err != nil {
		logger.Error("redis cache init failed; cache falls back to noop", "err", err)
		return cache.Noop{}, func() {}
	}
	return r, func() { _ = r.Close() }
}

func retry[T any](ctx context.Context, maxElapsed time.Duration, logger *slog.Logger, name string, fn func(context.Context) (T, error)) (T, error) {
	deadline := time.Now().Add(maxElapsed)
	attempt := 0
	delay := 500 * time.Millisecond
	var zero T
	var lastErr error
	for attempt < 10 {
		attempt++
		attemptCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		value, err := fn(attemptCtx)
		cancel()
		if err == nil {
			if attempt > 1 {
				logger.Info("startup dependency ready", "dependency", name, "attempt", attempt)
			}
			return value, nil
		}
		lastErr = err
		if time.Now().Add(delay).After(deadline) || ctx.Err() != nil {
			logger.Error("startup dependency failed", "dependency", name, "attempts", attempt, "elapsed", time.Since(deadline.Add(-maxElapsed)), "err", lastErr)
			return zero, lastErr
		}
		logger.Warn("startup dependency retry", "dependency", name, "attempt", attempt, "err", err)
		select {
		case <-time.After(delay + time.Duration(attempt%3)*100*time.Millisecond):
		case <-ctx.Done():
			return zero, ctx.Err()
		}
		delay *= 2
		if delay > 5*time.Second {
			delay = 5 * time.Second
		}
	}
	logger.Error("startup dependency failed", "dependency", name, "attempts", attempt, "elapsed", time.Since(deadline.Add(-maxElapsed)), "err", lastErr)
	return zero, lastErr
}
