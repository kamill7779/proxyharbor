// Command proxyharbor is a single binary that can run controller, gateway, or both.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kamill7779/proxyharbor/internal/auth"
	"github.com/kamill7779/proxyharbor/internal/cache"
	"github.com/kamill7779/proxyharbor/internal/config"
	"github.com/kamill7779/proxyharbor/internal/control"
	"github.com/kamill7779/proxyharbor/internal/control/health"
	"github.com/kamill7779/proxyharbor/internal/control/selector"
	"github.com/kamill7779/proxyharbor/internal/server"
	"github.com/kamill7779/proxyharbor/internal/storage"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load(os.Args[1:])
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

	selectorImpl, closeSelector, err := openSelector(ctx, cfg)
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
	svc.SetCache(cacheImpl, cfg.CacheTTL)
	svc.SetAllowInternalProxyEndpoint(cfg.AllowInternalProxyEndpoint)
	svc.SetSelector(selectorImpl)

	role := server.Role(cfg.Role)
	handler := server.NewForRoleWithHealthRecorder(svc, auth.New(cfg.AuthKey), role, healthRecorder)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logger.Info("proxyharbor listening",
			"role", cfg.Role, "addr", cfg.Addr, "storage", cfg.StorageDriver,
			"redis", cfg.RedisAddr != "", "selector", cfg.Selector)
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
}

func openSelector(ctx context.Context, cfg config.Config) (selector.ProxySelector, func(), error) {
	if cfg.Selector != selector.NameZFair {
		return nil, func() {}, errors.New("unsupported selector")
	}
	sel, err := selector.NewRedisZFair(ctx, selector.RedisZFairConfig{
		Addr:             cfg.RedisAddr,
		Password:         cfg.RedisPassword,
		DB:               cfg.RedisDB,
		Quantum:          float64(cfg.ZFairQuantum),
		DefaultLatencyMS: int64(cfg.ZFairDefaultLatencyMS),
		MaxPromote:       int64(cfg.ZFairMaxPromote),
		MaxScan:          int64(cfg.ZFairMaxPromote),
	})
	if err != nil {
		return nil, func() {}, err
	}
	return sel, func() { _ = sel.Close() }, nil
}

func openStore(ctx context.Context, cfg config.Config, logger *slog.Logger) (storage.Store, func(), error) {
	switch cfg.StorageDriver {
	case config.DriverMySQL:
		s, err := storage.NewMySQLStore(ctx, cfg.MySQLDSN, cfg.MySQLMaxOpen, cfg.MySQLMaxIdle, cfg.MySQLConnMaxAge)
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
	r, err := cache.NewRedis(ctx, cfg.RedisAddr, cfg.RedisPassword, cfg.RedisDB)
	if err != nil {
		logger.Error("redis cache init failed; cache falls back to noop", "err", err)
		return cache.Noop{}, func() {}
	}
	return r, func() { _ = r.Close() }
}
