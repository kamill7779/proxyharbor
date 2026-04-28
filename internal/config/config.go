// Package config loads ProxyHarbor runtime configuration.
//
// Load order, later values override earlier values:
//  1. built-in defaults
//  2. environment variables
//  3. command-line flags
//
// Environment variables use the PROXYHARBOR_* prefix.
package config

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type StorageDriver string

const (
	DriverMemory StorageDriver = "memory"
	DriverMySQL  StorageDriver = "mysql"
)

type Config struct {
	Role                       string
	Addr                       string
	GatewayURL                 string
	AdminKey                   string
	KeyPepper                  string
	AuthRefreshInterval        time.Duration
	LogFormat                  string
	LogLevel                   string
	StorageDriver              StorageDriver
	MySQLDSN                   string
	MySQLMaxOpen               int
	MySQLMaxIdle               int
	MySQLConnMaxAge            time.Duration
	RedisAddr                  string
	RedisPassword              string
	RedisDB                    int
	CacheTTL                   time.Duration
	ShutdownTimeout            time.Duration
	AllowInternalProxyEndpoint bool
	Selector                   string
	SelectorRedisRequired      bool
	ScoringProfile             string
	HealthFlushInterval        time.Duration
	HealthBufferMax            int
	ZFairQuantum               int
	ZFairDefaultLatencyMS      int
	ZFairMaxPromote            int
	// AuthInvalidation selects the auth-cache invalidation transport in
	// dynamic-keys mode. ""/"auto" enables Redis pub/sub when Redis is
	// configured and falls back to polling otherwise; "redis" forces Redis
	// (still polling-backed); "polling" disables pub/sub.
	AuthInvalidation string
	// InstanceID is a non-secret identifier surfaced in /readyz and the
	// debug auth cache endpoint to help correlate logs across replicas.
	InstanceID                string
	ClusterEnabled            bool
	InstanceHeartbeatInterval time.Duration
	LeaderLeaseTTL            time.Duration
	MaintenanceInterval       time.Duration
	MaintenanceBatchSize      int
	AuditRetentionDays        int
	UsageRetentionDays        int
	RetentionInterval         time.Duration
}

func Load(args []string) (Config, error) {
	cfg := Config{
		Role:                       envStr("PROXYHARBOR_ROLE", "all"),
		Addr:                       envStr("PROXYHARBOR_ADDR", ":8080"),
		GatewayURL:                 envStr("PROXYHARBOR_GATEWAY_URL", "http://localhost:8080"),
		AdminKey:                   os.Getenv("PROXYHARBOR_ADMIN_KEY"),
		KeyPepper:                  os.Getenv("PROXYHARBOR_KEY_PEPPER"),
		AuthRefreshInterval:        envDur("PROXYHARBOR_AUTH_REFRESH_INTERVAL", 5*time.Second),
		LogFormat:                  envStr("PROXYHARBOR_LOG_FORMAT", "json"),
		LogLevel:                   envStr("PROXYHARBOR_LOG_LEVEL", "info"),
		StorageDriver:              StorageDriver(envStr("PROXYHARBOR_STORAGE", "mysql")),
		MySQLDSN:                   os.Getenv("PROXYHARBOR_MYSQL_DSN"),
		MySQLMaxOpen:               envInt("PROXYHARBOR_MYSQL_MAX_OPEN", 20),
		MySQLMaxIdle:               envInt("PROXYHARBOR_MYSQL_MAX_IDLE", 5),
		MySQLConnMaxAge:            envDur("PROXYHARBOR_MYSQL_CONN_MAX_AGE", 30*time.Minute),
		RedisAddr:                  os.Getenv("PROXYHARBOR_REDIS_ADDR"),
		RedisPassword:              os.Getenv("PROXYHARBOR_REDIS_PASSWORD"),
		RedisDB:                    envInt("PROXYHARBOR_REDIS_DB", 0),
		CacheTTL:                   envDur("PROXYHARBOR_CACHE_TTL", 60*time.Second),
		ShutdownTimeout:            envDur("PROXYHARBOR_SHUTDOWN_TIMEOUT", 15*time.Second),
		AllowInternalProxyEndpoint: envBool("PROXYHARBOR_ALLOW_INTERNAL_PROXY_ENDPOINT", false),
		Selector:                   envStr("PROXYHARBOR_SELECTOR", "zfair"),
		SelectorRedisRequired:      envBool("PROXYHARBOR_SELECTOR_REDIS_REQUIRED", true),
		ScoringProfile:             envStr("PROXYHARBOR_SCORING_PROFILE", "default"),
		HealthFlushInterval:        envDur("PROXYHARBOR_HEALTH_FLUSH_INTERVAL", 5*time.Second),
		HealthBufferMax:            envInt("PROXYHARBOR_HEALTH_BUFFER_MAX", 10000),
		ZFairQuantum:               envInt("PROXYHARBOR_ZFAIR_QUANTUM", 1000),
		ZFairDefaultLatencyMS:      envInt("PROXYHARBOR_ZFAIR_DEFAULT_LATENCY_MS", 200),
		ZFairMaxPromote:            envInt("PROXYHARBOR_ZFAIR_MAX_PROMOTE", 128),
		AuthInvalidation:           envStr("PROXYHARBOR_AUTH_INVALIDATION", ""),
		InstanceID:                 envStr("PROXYHARBOR_INSTANCE_ID", ""),
		ClusterEnabled:             envBool("PROXYHARBOR_CLUSTER_ENABLED", false),
		InstanceHeartbeatInterval:  envDur("PROXYHARBOR_INSTANCE_HEARTBEAT_INTERVAL", 10*time.Second),
		LeaderLeaseTTL:             envDur("PROXYHARBOR_LEADER_LEASE_TTL", 45*time.Second),
		MaintenanceInterval:        envDur("PROXYHARBOR_MAINTENANCE_INTERVAL", 30*time.Second),
		MaintenanceBatchSize:       envInt("PROXYHARBOR_MAINTENANCE_BATCH_SIZE", 500),
		AuditRetentionDays:         envInt("PROXYHARBOR_AUDIT_RETENTION_DAYS", 0),
		UsageRetentionDays:         envInt("PROXYHARBOR_USAGE_RETENTION_DAYS", 0),
		RetentionInterval:          envDur("PROXYHARBOR_RETENTION_INTERVAL", time.Hour),
	}

	fs := flag.NewFlagSet("proxyharbor", flag.ContinueOnError)
	fs.StringVar(&cfg.Role, "role", cfg.Role, "process role: all | controller | gateway")
	fs.StringVar(&cfg.Addr, "addr", cfg.Addr, "HTTP listen address")
	fs.StringVar(&cfg.GatewayURL, "gateway-url", cfg.GatewayURL, "gateway URL returned in leases")
	fs.StringVar(&cfg.AdminKey, "admin-key", cfg.AdminKey, "bootstrap admin key")
	fs.StringVar(&cfg.KeyPepper, "key-pepper", cfg.KeyPepper, "key hashing pepper for dynamic mode")
	fs.DurationVar(&cfg.AuthRefreshInterval, "auth-refresh-interval", cfg.AuthRefreshInterval, "dynamic auth cache refresh interval")
	fs.StringVar(&cfg.LogFormat, "log-format", cfg.LogFormat, "log format: json | text")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "log level: info | debug")
	storageStr := fs.String("storage", string(cfg.StorageDriver), "storage driver: memory | mysql")
	fs.StringVar(&cfg.MySQLDSN, "mysql-dsn", cfg.MySQLDSN, "MySQL DSN")
	fs.StringVar(&cfg.RedisAddr, "redis-addr", cfg.RedisAddr, "Redis address")
	fs.StringVar(&cfg.Selector, "selector", cfg.Selector, "proxy selector")
	fs.BoolVar(&cfg.SelectorRedisRequired, "selector-redis-required", cfg.SelectorRedisRequired, "require Redis for selector")
	fs.StringVar(&cfg.ScoringProfile, "scoring-profile", cfg.ScoringProfile, "health scoring profile")
	fs.DurationVar(&cfg.HealthFlushInterval, "health-flush-interval", cfg.HealthFlushInterval, "health flush interval")
	fs.IntVar(&cfg.HealthBufferMax, "health-buffer-max", cfg.HealthBufferMax, "health event buffer size")
	fs.IntVar(&cfg.ZFairQuantum, "zfair-quantum", cfg.ZFairQuantum, "zfair scheduler quantum")
	fs.IntVar(&cfg.ZFairDefaultLatencyMS, "zfair-default-latency-ms", cfg.ZFairDefaultLatencyMS, "zfair default latency ms")
	fs.IntVar(&cfg.ZFairMaxPromote, "zfair-max-promote", cfg.ZFairMaxPromote, "zfair max delayed promotions")
	fs.StringVar(&cfg.AuthInvalidation, "auth-invalidation", cfg.AuthInvalidation, "auth invalidation transport: auto | redis | polling")
	fs.StringVar(&cfg.InstanceID, "instance-id", cfg.InstanceID, "non-secret instance identifier exposed in readiness/debug responses")
	fs.BoolVar(&cfg.ClusterEnabled, "cluster-enabled", cfg.ClusterEnabled, "enable MySQL-backed instance heartbeat and maintenance leader election")
	fs.DurationVar(&cfg.InstanceHeartbeatInterval, "instance-heartbeat-interval", cfg.InstanceHeartbeatInterval, "cluster instance heartbeat interval")
	fs.DurationVar(&cfg.LeaderLeaseTTL, "leader-lease-ttl", cfg.LeaderLeaseTTL, "cluster leader lock lease TTL")
	fs.DurationVar(&cfg.MaintenanceInterval, "maintenance-interval", cfg.MaintenanceInterval, "leader maintenance interval")
	fs.IntVar(&cfg.MaintenanceBatchSize, "maintenance-batch-size", cfg.MaintenanceBatchSize, "leader expired lease cleanup batch size")
	fs.IntVar(&cfg.AuditRetentionDays, "audit-retention-days", cfg.AuditRetentionDays, "audit event retention in days; 0 disables cleanup")
	fs.IntVar(&cfg.UsageRetentionDays, "usage-retention-days", cfg.UsageRetentionDays, "usage event retention in days; 0 disables cleanup")
	fs.DurationVar(&cfg.RetentionInterval, "retention-interval", cfg.RetentionInterval, "audit/usage retention worker interval")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	cfg.StorageDriver = StorageDriver(*storageStr)
	return cfg, cfg.validate()
}

func (c Config) validate() error {
	switch c.Role {
	case "all", "controller", "gateway":
	default:
		return fmt.Errorf("invalid role: %q", c.Role)
	}
	switch c.LogFormat {
	case "json", "text":
	default:
		return fmt.Errorf("invalid log format: %q", c.LogFormat)
	}
	switch c.LogLevel {
	case "info", "debug":
	default:
		return fmt.Errorf("invalid log level: %q", c.LogLevel)
	}
	switch c.StorageDriver {
	case DriverMemory:
	case DriverMySQL:
		if strings.TrimSpace(c.MySQLDSN) == "" {
			return errors.New("storage=mysql requires -mysql-dsn or PROXYHARBOR_MYSQL_DSN")
		}
	default:
		return fmt.Errorf("unsupported storage driver: %q", c.StorageDriver)
	}
	if c.Selector != "zfair" {
		return fmt.Errorf("unsupported selector: %q", c.Selector)
	}
	if c.SelectorRedisRequired && strings.TrimSpace(c.RedisAddr) == "" {
		return errors.New("selector=zfair requires PROXYHARBOR_REDIS_ADDR when selector redis is required")
	}
	if c.HealthBufferMax <= 0 {
		return errors.New("health buffer max must be positive")
	}
	if c.ZFairQuantum <= 0 || c.ZFairDefaultLatencyMS <= 0 || c.ZFairMaxPromote <= 0 {
		return errors.New("zfair numeric settings must be positive")
	}
	switch strings.ToLower(strings.TrimSpace(c.AuthInvalidation)) {
	case "", "auto", "redis", "polling":
	default:
		return fmt.Errorf("invalid auth invalidation transport: %q", c.AuthInvalidation)
	}

	if c.StorageDriver != DriverMySQL {
		return errors.New("v0.2.0 requires storage=mysql")
	}
	if c.AdminKey == "" {
		return errors.New("PROXYHARBOR_ADMIN_KEY is required")
	}
	if len(c.AdminKey) < 32 {
		return errors.New("PROXYHARBOR_ADMIN_KEY must be at least 32 bytes")
	}
	if c.KeyPepper == "" {
		return errors.New("PROXYHARBOR_KEY_PEPPER is required")
	}
	if len(c.KeyPepper) < 32 {
		return errors.New("PROXYHARBOR_KEY_PEPPER must be at least 32 bytes")
	}
	if c.AuthRefreshInterval <= 0 || c.AuthRefreshInterval > 5*time.Second {
		return errors.New("PROXYHARBOR_AUTH_REFRESH_INTERVAL must be > 0 and <= 5s")
	}
	if c.ClusterEnabled && c.StorageDriver != DriverMySQL {
		return errors.New("cluster mode requires storage=mysql")
	}
	if c.InstanceHeartbeatInterval <= 0 {
		return errors.New("PROXYHARBOR_INSTANCE_HEARTBEAT_INTERVAL must be positive")
	}
	if c.LeaderLeaseTTL <= 0 {
		return errors.New("PROXYHARBOR_LEADER_LEASE_TTL must be positive")
	}
	if c.MaintenanceInterval <= 0 {
		return errors.New("PROXYHARBOR_MAINTENANCE_INTERVAL must be positive")
	}
	if c.MaintenanceBatchSize <= 0 {
		return errors.New("PROXYHARBOR_MAINTENANCE_BATCH_SIZE must be positive")
	}
	if c.AuditRetentionDays < 0 || c.UsageRetentionDays < 0 {
		return errors.New("retention days must be >= 0")
	}
	if c.RetentionInterval <= 0 {
		return errors.New("PROXYHARBOR_RETENTION_INTERVAL must be positive")
	}
	return nil
}

func envStr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func envDur(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "y", "on":
			return true
		case "0", "false", "no", "n", "off":
			return false
		}
	}
	return fallback
}
