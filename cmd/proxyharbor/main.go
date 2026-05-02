// Command proxyharbor is a single binary that can run controller, gateway, or both.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kamill7779/proxyharbor/internal/auth"
	"github.com/kamill7779/proxyharbor/internal/cache"
	"github.com/kamill7779/proxyharbor/internal/cluster"
	"github.com/kamill7779/proxyharbor/internal/config"
	"github.com/kamill7779/proxyharbor/internal/control"
	"github.com/kamill7779/proxyharbor/internal/control/health"
	"github.com/kamill7779/proxyharbor/internal/control/selector"
	"github.com/kamill7779/proxyharbor/internal/metrics"
	"github.com/kamill7779/proxyharbor/internal/server"
	"github.com/kamill7779/proxyharbor/internal/shared/domain"
	"github.com/kamill7779/proxyharbor/internal/storage"
	"github.com/redis/go-redis/v9"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "doctor":
			os.Exit(runDoctor(os.Args[2:], os.Stdout, os.Stderr))
		case "init":
			os.Exit(runInit(os.Args[2:], os.Stdout, os.Stderr))
		}
	}
	if handled, code := runOpsCommand(os.Args[1:], os.Stdout, os.Stderr); handled {
		os.Exit(code)
	}

	cfg, err := config.Load(os.Args[1:])
	logger := newLogger(cfg)
	slog.SetDefault(logger)
	if err != nil {
		kind := configErrorKind(err)
		metrics.RecordRuntimeConfigValidationResult("error", kind)
		metrics.RecordRuntimeStartupResult("error", "config")
		logger.Error("load config", "error_kind", kind, "err", err)
		os.Exit(2)
	}
	metrics.RecordRuntimeConfigValidationResult("ok", "none")
	if cfg.SecretsFile != "" {
		logger.Info("local secrets ready", "path", cfg.SecretsFile, "admin_key_fp", auth.Fingerprint(cfg.AdminKey))
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	cleanups := &cleanupStack{}

	store, closeStore, err := openStore(ctx, cfg, logger)
	if err != nil {
		metrics.RecordRuntimeStartupResult("error", "store")
		logger.Error("open store", "error_kind", runtimeErrorKind(string(cfg.StorageDriver), err), "err", safeRuntimeError(err, cfg))
		fatalWithCleanup(1, cleanups)
	}
	cleanups.add(closeStore)

	cacheImpl, closeCache, err := openCache(ctx, cfg, logger)
	if err != nil {
		metrics.RecordRuntimeStartupResult("error", "cache")
		logger.Error("open cache", "error_kind", runtimeErrorKind("cache", err), "err", safeRuntimeError(err, cfg))
		fatalWithCleanup(1, cleanups)
	}
	cleanups.add(closeCache)

	selectorImpl, closeSelector, err := openSelector(ctx, cfg, logger)
	if err != nil {
		metrics.RecordRuntimeStartupResult("error", "selector")
		logger.Error("open selector", "error_kind", runtimeErrorKind("selector", err), "err", safeRuntimeError(err, cfg))
		fatalWithCleanup(1, cleanups)
	}
	cleanups.add(closeSelector)

	healthRecorder := health.NewCoalescingRecorder(store, health.RecorderOptions{
		BufferSize:    cfg.HealthBufferMax,
		FlushInterval: cfg.HealthFlushInterval,
		Policy:        health.ScoringPolicyForProfile(cfg.ScoringProfile),
	})
	cleanups.add(func() {
		ctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		healthRecorder.Close(ctx)
	})

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
	authInvalidationMode := strings.ToLower(strings.TrimSpace(cfg.AuthInvalidation))
	if authInvalidationMode == "" {
		authInvalidationMode = "auto"
	}
	if authInvalidationMode == "redis" && strings.TrimSpace(cfg.RedisAddr) == "" {
		logger.Error("auth invalidation redis mode requires redis address")
		fatalWithCleanup(1, cleanups)
	}
	if cfg.RedisAddr != "" {
		invalidationClient = redis.NewClient(&redis.Options{
			Addr:         cfg.RedisAddr,
			Password:     cfg.RedisPassword,
			DB:           cfg.RedisDB,
			DialTimeout:  3 * time.Second,
			ReadTimeout:  0, // pub/sub uses blocking reads
			WriteTimeout: 2 * time.Second,
		})
	}
	cleanups.add(func() {
		if invalidationClient != nil {
			_ = invalidationClient.Close()
		}
	})
	backgroundCtx, cancelBackground := context.WithCancel(ctx)
	cleanups.add(cancelBackground)
	background := &backgroundGroup{logger: logger}

	// Verify MySQL schema before starting any traffic-serving goroutine.
	if mysqlStore, ok := store.(*storage.MySQLStore); ok {
		if err := storage.EnsureDynamicAuthSchema(ctx, mysqlStore.DB()); err != nil {
			metrics.RecordRuntimeStartupResult("error", "schema")
			logger.Error("schema check failed", "error_kind", runtimeErrorKind("mysql", err), "err", safeRuntimeError(err, cfg))
			fatalWithCleanup(1, cleanups)
		}
	}
	var clusterRunner *cluster.Runner
	if cfg.ClusterEnabled {
		clusterStore, ok := store.(storage.ClusterStore)
		if !ok {
			metrics.RecordRuntimeStartupResult("error", "cluster")
			logger.Error("cluster mode requires cluster-capable storage", "error_kind", "cluster")
			fatalWithCleanup(1, cleanups)
		}
		clusterRunner = &cluster.Runner{
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
		}
	}

	authn, dynamicStore, err := buildAuthenticator(ctx, cfg, store)
	if err != nil {
		metrics.RecordRuntimeStartupResult("error", "auth")
		logger.Error("open authenticator", "error_kind", runtimeErrorKind("auth", err), "err", safeRuntimeError(err, cfg))
		fatalWithCleanup(1, cleanups)
	}
	if dynamicStore != nil {
		background.Go("auth_refresh", func() { dynamicStore.Run(backgroundCtx) })
	}

	var invalidator auth.Invalidator = auth.NoopInvalidator{}
	var invalidationStatus auth.StatusReporter = auth.NewStatusReporter("polling", "fallback")
	hotCache := cacheHotInvalidator(cacheImpl)
	if invalidationClient != nil {
		invalidator = auth.NewRedisInvalidator(invalidationClient, auth.DefaultInvalidationChannel, logger)
		if dynamicStore != nil || hotCache != nil {
			statusTracker := auth.NewStatusTrackerWithRequired("redis", "configured", authInvalidationMode == "redis" || hotCache != nil)
			invalidationStatus = statusTracker
			subDynamicStore := dynamicStore
			if authInvalidationMode == "polling" {
				subDynamicStore = nil
			}
			background.Go("cache_invalidation", func() {
				auth.SubscribeCacheInvalidationsWithStatus(backgroundCtx, invalidationClient, auth.DefaultInvalidationChannel, subDynamicStore, hotCache, statusTracker, logger)
			})
		}
	}

	adminStore := buildAdminStore(store)
	if cfg.AutoSecrets && cfg.StorageDriver == config.DriverSQLite && !cfg.ClusterEnabled {
		if err := ensureDefaultTenant(ctx, adminStore); err != nil {
			metrics.RecordRuntimeStartupResult("error", "tenant")
			logger.Error("ensure default tenant", "error_kind", runtimeErrorKind("tenant", err), "err", safeRuntimeError(err, cfg))
			fatalWithCleanup(1, cleanups)
		}
	}
	gatewayShutdownCtx, cancelGatewayShutdown := context.WithCancel(context.Background())
	cleanups.add(cancelGatewayShutdown)
	opts := server.Options{
		Role:               role,
		HealthRecorder:     healthRecorder,
		Dependency:         dependencyChecks{store: store, cache: cacheImpl, selector: selectorImpl},
		AdminStore:         adminStore,
		Pepper:             cfg.KeyPepper,
		Invalidator:        invalidator,
		InvalidationStatus: invalidationStatus,
		InstanceID:         instanceID,
		ClusterStore:       clusterStoreForOptions(store),
		ClusterSummary:     clusterSummary(cfg),
		ShutdownContext:    gatewayShutdownCtx,
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

	listener, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		metrics.RecordRuntimeStartupResult("error", "listen")
		logger.Error("listen", "error_kind", runtimeErrorKind("listen", err), "err", safeRuntimeError(err, cfg))
		fatalWithCleanup(1, cleanups)
	}
	if clusterRunner != nil {
		background.Go("cluster", func() { clusterRunner.Run(backgroundCtx) })
	}
	serverDone := make(chan error, 1)
	go func() {
		logger.Info("proxyharbor listening",
			"role", cfg.Role, "addr", cfg.Addr, "storage", cfg.StorageDriver,
			"redis", cfg.RedisAddr != "", "selector", cfg.Selector,
			"auth_cache_entries", authn.CacheEntries(),
			"auth_invalidation", authInvalidationLabel(cfg, invalidationClient != nil),
			"instance_id", instanceID)
		metrics.RecordRuntimeStartupResult("started", "none")
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			metrics.RecordRuntimeStartupResult("error", "listen")
			logger.Error("listen", "error_kind", runtimeErrorKind("listen", err), "err", safeRuntimeError(err, cfg))
			serverDone <- err
			stop()
			return
		}
		serverDone <- nil
	}()

	<-ctx.Done()
	stop()
	logger.Info("shutdown signal received")
	handler.StartDraining()
	metrics.RecordRuntimeShutdownResult("started", "none")
	drainDelay := shutdownDrainDelay(cfg.ShutdownTimeout)
	waitShutdownDrain(drainDelay)
	phaseBudgets := splitShutdownBudget(cfg.ShutdownTimeout-drainDelay, 4)
	cancelGatewayShutdown()

	shutdownResult := "graceful"
	shutdownKind := "none"
	var serverErr error
	httpShutdownCtx, cancelHTTP := context.WithTimeout(context.Background(), phaseBudgets[0])
	if err := srv.Shutdown(httpShutdownCtx); err != nil {
		shutdownResult = "error"
		shutdownKind = "http"
		logger.Error("graceful shutdown", "error_kind", runtimeErrorKind("http", err), "err", safeRuntimeError(err, cfg))
	}
	select {
	case err := <-serverDone:
		if err != nil {
			serverErr = err
			if shutdownKind == "none" {
				shutdownResult = "error"
				shutdownKind = "server_wait"
			}
		}
	case <-httpShutdownCtx.Done():
		shutdownResult = "error"
		shutdownKind = "server_wait"
		logger.Error("http server shutdown wait timed out", "error_kind", shutdownKind)
	}
	cancelHTTP()
	tunnelShutdownCtx, cancelTunnels := context.WithTimeout(context.Background(), phaseBudgets[1])
	if err := handler.WaitForGatewayTunnels(tunnelShutdownCtx); err != nil {
		shutdownResult = "error"
		shutdownKind = "gateway_tunnel"
		logger.Error("gateway tunnel shutdown wait timed out", "error_kind", shutdownKind)
	}
	cancelTunnels()
	cancelBackground()
	backgroundShutdownCtx, cancelBackgroundWait := context.WithTimeout(context.Background(), phaseBudgets[2])
	if err := background.Wait(backgroundShutdownCtx); err != nil {
		shutdownResult = "error"
		shutdownKind = "background"
		logger.Error("background shutdown wait timed out", "error_kind", shutdownKind)
	}
	cancelBackgroundWait()
	healthShutdownCtx, cancelHealth := context.WithTimeout(context.Background(), phaseBudgets[3])
	healthRecorder.Close(healthShutdownCtx)
	if healthShutdownCtx.Err() != nil && shutdownKind == "none" {
		shutdownResult = "error"
		shutdownKind = "health_drain"
	}
	cancelHealth()
	metrics.RecordRuntimeShutdownResult(shutdownResult, shutdownKind)
	cleanups.run()
	logger.Info("shutdown complete", "result", shutdownResult, "error_kind", shutdownKind)
	if serverErr != nil {
		os.Exit(1)
	}
}

func shutdownDrainDelay(total time.Duration) time.Duration {
	delay := time.Second
	if total < 2*time.Second {
		delay = total / 2
	}
	if delay <= 0 {
		return 0
	}
	return delay
}

func waitShutdownDrain(delay time.Duration) {
	if delay <= 0 {
		return
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	<-timer.C
}

func splitShutdownBudget(total time.Duration, phases int) []time.Duration {
	if phases <= 0 {
		return nil
	}
	budgets := make([]time.Duration, phases)
	if total <= 0 {
		return budgets
	}
	base := total / time.Duration(phases)
	remainder := total % time.Duration(phases)
	for i := 0; i < phases; i++ {
		budgets[i] = base
		if remainder > 0 {
			budgets[i]++
			remainder--
		}
	}
	return budgets
}

type cleanupStack struct {
	fns []func()
}

func (c *cleanupStack) add(fn func()) {
	if c == nil || fn == nil {
		return
	}
	c.fns = append(c.fns, fn)
}

func (c *cleanupStack) run() {
	if c == nil {
		return
	}
	for i := len(c.fns) - 1; i >= 0; i-- {
		c.fns[i]()
	}
	c.fns = nil
}

func fatalWithCleanup(code int, cleanups *cleanupStack) {
	cleanups.run()
	os.Exit(code)
}

func cacheHotInvalidator(cacheImpl cache.Cache) auth.HotCacheInvalidator {
	hot, ok := cacheImpl.(auth.HotCacheInvalidator)
	if !ok {
		return nil
	}
	return hot
}

func buildAdminStore(store storage.Store) server.AdminStore {
	if mysqlStore, ok := store.(*storage.MySQLStore); ok {
		return server.NewMySQLAdminStore(mysqlStore.DB())
	}
	if sqliteStore, ok := store.(*storage.SQLiteStore); ok {
		return sqliteStore.AdminStore()
	}
	return server.NewMemoryAdminStore()
}

func ensureDefaultTenant(ctx context.Context, adminStore server.AdminStore) error {
	if adminStore == nil {
		return nil
	}
	if _, err := adminStore.GetTenant(ctx, "default"); err == nil {
		return nil
	}
	return adminStore.CreateTenant(ctx, domain.Tenant{ID: "default", Name: "Default Tenant", Enabled: true, CreatedAt: time.Now().UTC()})
}

func buildAuthenticator(ctx context.Context, cfg config.Config, store storage.Store) (*auth.Authenticator, *auth.DynamicStore, error) {
	var keyStore auth.KeyStore
	if mysqlStore, ok := store.(*storage.MySQLStore); ok {
		keyStore = auth.NewMySQLKeyStore(mysqlStore.DB())
	} else if sqliteStore, ok := store.(*storage.SQLiteStore); ok {
		keyStore = sqliteStore
	} else {
		return nil, nil, errors.New("dynamic auth requires mysql or sqlite storage")
	}
	dynamicStore, err := auth.NewDynamicStoreWithContext(ctx, keyStore, []byte(cfg.KeyPepper), cfg.AuthRefreshInterval)
	if err != nil {
		return nil, nil, err
	}
	return auth.NewDynamicKeys(dynamicStore).WithAdminKey(cfg.AdminKey), dynamicStore, nil
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

func configErrorKind(err error) string {
	if err == nil {
		return "none"
	}
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "admin_key is required") || strings.Contains(text, "key_pepper is required"):
		return "missing_secret"
	case strings.Contains(text, "admin_key must") || strings.Contains(text, "key_pepper must") || strings.Contains(text, "auto_secrets"):
		return "security"
	case strings.Contains(text, "ha mode") || strings.Contains(text, "cluster mode"):
		return "ha"
	default:
		return "invalid"
	}
}

func runtimeErrorKind(component string, err error) string {
	if err == nil {
		return "none"
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "timeout"
	}
	text := strings.ToLower(err.Error())
	component = strings.ToLower(component)
	switch {
	case strings.Contains(component, "redis") || strings.Contains(component, "cache") || strings.Contains(component, "selector") || strings.Contains(text, "redis"):
		return "redis"
	case strings.Contains(component, "mysql") || strings.Contains(text, "mysql"):
		return "mysql"
	default:
		return "backend"
	}
}

func safeRuntimeError(err error, cfg config.Config) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	for _, secret := range []string{
		cfg.AdminKey,
		cfg.KeyPepper,
		cfg.MySQLDSN,
		mysqlPasswordFromDSN(cfg.MySQLDSN),
		cfg.RedisPassword,
	} {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[redacted]")
		}
	}
	return text
}

func mysqlPasswordFromDSN(dsn string) string {
	at := strings.Index(dsn, "@")
	if at < 0 {
		return ""
	}
	colon := strings.LastIndex(dsn[:at], ":")
	if colon < 0 || colon+1 >= at {
		return ""
	}
	return dsn[colon+1 : at]
}

func runDoctor(args []string, stdout io.Writer, stderr io.Writer) int {
	cfg, err := config.LoadUnchecked(args)
	if err != nil {
		fmt.Fprintf(stdout, "FAIL config: %v\n", err)
		return 2
	}

	failed := false
	check := func(ok bool, name, detail string) {
		status := "OK"
		if !ok {
			status = "FAIL"
			failed = true
		}
		if detail == "" {
			fmt.Fprintf(stdout, "%s %s\n", status, name)
			return
		}
		fmt.Fprintf(stdout, "%s %s: %s\n", status, name, detail)
	}

	switch cfg.StorageDriver {
	case config.DriverMemory:
		check(true, "storage driver memory", "dev/demo/CI only; not for production")
	case config.DriverMySQL:
		check(true, "storage driver mysql", "persistent HA profile")
		check(strings.TrimSpace(cfg.MySQLDSN) != "", "mysql dsn configured", "set PROXYHARBOR_MYSQL_DSN or -mysql-dsn")
	case config.DriverSQLite:
		check(true, "storage driver sqlite", "single-instance profile")
		check(strings.TrimSpace(cfg.SQLitePath) != "", "sqlite path configured", "set PROXYHARBOR_SQLITE_PATH or -sqlite-path")
		if strings.TrimSpace(cfg.SQLitePath) != "" {
			check(sqliteParentWritable(cfg.SQLitePath) == nil, "sqlite path parent writable", "parent directory must be writable")
			runSQLiteDoctorChecks(cfg.SQLitePath, check)
		}
	default:
		check(false, "storage driver supported", "use sqlite, mysql, or memory")
	}

	if cfg.SelectorRedisRequired {
		check(strings.TrimSpace(cfg.RedisAddr) != "", "redis required configured", "set PROXYHARBOR_REDIS_ADDR or disable selector redis requirement")
	} else {
		check(true, "redis requirement", "optional")
	}
	check(strings.TrimSpace(cfg.AdminKey) != "", "admin key configured", "set PROXYHARBOR_ADMIN_KEY or -admin-key")
	check(len(cfg.AdminKey) >= 32, "admin key length", "must be at least 32 bytes")
	check(strings.TrimSpace(cfg.KeyPepper) != "", "key pepper configured", "set PROXYHARBOR_KEY_PEPPER or -key-pepper")
	check(len(cfg.KeyPepper) >= 32, "key pepper length", "must be at least 32 bytes")

	if failed {
		return 1
	}
	return 0
}

func runInit(args []string, stdout io.Writer, stderr io.Writer) int {
	cfg, err := config.LoadUnchecked(args)
	if err != nil {
		fmt.Fprintf(stdout, "FAIL config: %v\n", err)
		return 2
	}
	switch cfg.StorageDriver {
	case config.DriverSQLite:
		store, err := storage.NewSQLiteStore(context.Background(), cfg.SQLitePath)
		if err != nil {
			fmt.Fprintf(stderr, "sqlite init failed: %v\n", err)
			return 1
		}
		if err := store.Close(); err != nil {
			fmt.Fprintf(stderr, "sqlite close failed: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "sqlite initialized at %s\n", cfg.SQLitePath)
		return 0
	case config.DriverMySQL:
		fmt.Fprintln(stdout, "mysql initialization is not performed by proxyharbor init; apply migrations/mysql/init.sql explicitly")
		return 1
	case config.DriverMemory:
		fmt.Fprintln(stdout, "memory storage has no schema to initialize; use only for dev/demo/CI")
		return 0
	default:
		fmt.Fprintf(stdout, "unsupported storage driver %q\n", cfg.StorageDriver)
		return 2
	}
}

func sqliteParentWritable(path string) error {
	parent := filepath.Dir(path)
	if parent == "." || parent == "" {
		parent = "."
	}
	info, err := os.Stat(parent)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("sqlite parent is not a directory")
	}
	probe, err := os.CreateTemp(parent, ".proxyharbor-doctor-*")
	if err != nil {
		return err
	}
	name := probe.Name()
	closeErr := probe.Close()
	removeErr := os.Remove(name)
	if closeErr != nil {
		return closeErr
	}
	return removeErr
}

func runSQLiteDoctorChecks(path string, check func(bool, string, string)) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		check(true, "sqlite file", "not created yet")
	} else if err != nil {
		check(false, "sqlite file", err.Error())
	} else {
		check(info.Mode().IsRegular(), "sqlite file", fmt.Sprintf("size=%d bytes", info.Size()))
		check(info.Size() < 1<<40, "sqlite file size", fmt.Sprintf("size=%d bytes", info.Size()))
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); err == nil {
			check(false, "sqlite sidecar "+suffix, "sidecar exists; stop process or checkpoint WAL before backup/restore")
		} else if errors.Is(err, os.ErrNotExist) {
			check(true, "sqlite sidecar "+suffix, "absent")
		} else {
			check(false, "sqlite sidecar "+suffix, err.Error())
		}
	}
	if err == nil && info.Mode().IsRegular() {
		version, err := sqliteSchemaVersionOf(path)
		if err != nil {
			check(false, "sqlite schema version", err.Error())
		} else {
			check(version == 1, "sqlite schema version", fmt.Sprintf("version=%d expected=1", version))
		}
	}
	check(sqliteParentWritable(path) == nil, "sqlite parent writable", "parent accepts writes")
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

type backgroundGroup struct {
	wg     sync.WaitGroup
	logger *slog.Logger
}

func (g *backgroundGroup) Go(component string, fn func()) {
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		if g.logger != nil {
			g.logger.Info("background started", "component", component)
			defer g.logger.Info("background stopped", "component", component)
		}
		fn()
	}()
}

func (g *backgroundGroup) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		g.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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
	if cfg.Selector == selector.NameLocal {
		return selector.NewLocal(), func() {}, nil
	}
	if cfg.Selector != selector.NameZFair {
		return nil, func() {}, errors.New("unsupported selector")
	}
	if cfg.RedisAddr == "" {
		if cfg.SelectorRedisRequired {
			return nil, func() {}, errors.New("selector=zfair requires redis addr")
		}
		logger.Warn("redis selector is not configured; using local selector")
		return selector.NewLocal(), func() {}, nil
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
	case config.DriverSQLite:
		s, err := storage.NewSQLiteStore(ctx, cfg.SQLitePath)
		if err != nil {
			return nil, func() {}, err
		}
		return s, func() { _ = s.Close() }, nil
	case config.DriverMemory:
		logger.Warn("using memory storage; only for development, demos, and CI")
		return storage.NewMemoryStore(), func() {}, nil
	default:
		return nil, func() {}, errors.New("unknown storage driver")
	}
}

func openCache(ctx context.Context, cfg config.Config, logger *slog.Logger) (cache.Cache, func(), error) {
	if cfg.RedisAddr == "" {
		if redisCacheRequired(cfg) {
			return nil, func() {}, errors.New("redis cache requires redis addr")
		}
		logger.Warn("redis is not configured; cache falls back to noop")
		return cache.Noop{}, func() {}, nil
	}
	r, err := retry(ctx, 30*time.Second, logger, "redis cache", func(attemptCtx context.Context) (*cache.Redis, error) {
		return cache.NewRedis(attemptCtx, cfg.RedisAddr, cfg.RedisPassword, cfg.RedisDB)
	})
	if err != nil {
		if redisCacheRequired(cfg) {
			return nil, func() {}, err
		}
		logger.Error("redis cache init failed; cache falls back to noop", "error_kind", runtimeErrorKind("redis cache", err), "err", safeRuntimeError(err, cfg))
		return cache.Noop{}, func() {}, nil
	}
	return r, func() { _ = r.Close() }, nil
}

func redisCacheRequired(cfg config.Config) bool {
	return cfg.ClusterEnabled || cfg.SelectorRedisRequired || strings.EqualFold(strings.TrimSpace(cfg.AuthInvalidation), "redis")
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
			logger.Error("startup dependency failed", "dependency", name, "attempts", attempt, "elapsed", time.Since(deadline.Add(-maxElapsed)), "error_kind", runtimeErrorKind(name, lastErr))
			return zero, lastErr
		}
		logger.Warn("startup dependency retry", "dependency", name, "attempt", attempt, "error_kind", runtimeErrorKind(name, err))
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
	logger.Error("startup dependency failed", "dependency", name, "attempts", attempt, "elapsed", time.Since(deadline.Add(-maxElapsed)), "error_kind", runtimeErrorKind(name, lastErr))
	return zero, lastErr
}
