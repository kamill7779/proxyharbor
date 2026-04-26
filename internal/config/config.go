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
	AuthKey                    string
	TenantKeys                 map[string]string // key -> tenantID; non-empty enables strict tenant-key auth mode
	TenantKeyMinLen            int
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
	HealthcheckURL             string
	HealthcheckInterval        time.Duration
	HealthcheckTimeout         time.Duration
	StickyEnabled              bool
	StickyPolicy               string
}

func Load(args []string) (Config, error) {
	cfg := Config{
		Role:                       envStr("PROXYHARBOR_ROLE", "all"),
		Addr:                       envStr("PROXYHARBOR_ADDR", ":8080"),
		GatewayURL:                 envStr("PROXYHARBOR_GATEWAY_URL", "http://localhost:8080"),
		AuthKey:                    os.Getenv("PROXYHARBOR_AUTH_KEY"),
		TenantKeyMinLen:            envInt("PROXYHARBOR_TENANT_KEY_MIN_LEN", 16),
		LogFormat:                  envStr("PROXYHARBOR_LOG_FORMAT", "json"),
		LogLevel:                   envStr("PROXYHARBOR_LOG_LEVEL", "info"),
		StorageDriver:              StorageDriver(envStr("PROXYHARBOR_STORAGE", "memory")),
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
		HealthcheckURL:             os.Getenv("PROXYHARBOR_HEALTHCHECK_URL"),
		HealthcheckInterval:        envDur("PROXYHARBOR_HEALTHCHECK_INTERVAL", 30*time.Second),
		HealthcheckTimeout:         envDur("PROXYHARBOR_HEALTHCHECK_TIMEOUT", 5*time.Second),
		StickyEnabled:              envBool("PROXYHARBOR_STICKY_ENABLED", false),
		StickyPolicy:               envStr("PROXYHARBOR_STICKY_POLICY", "none"),
	}

	fs := flag.NewFlagSet("proxyharbor", flag.ContinueOnError)
	fs.StringVar(&cfg.Role, "role", cfg.Role, "process role: all | controller | gateway")
	fs.StringVar(&cfg.Addr, "addr", cfg.Addr, "HTTP listen address")
	fs.StringVar(&cfg.GatewayURL, "gateway-url", cfg.GatewayURL, "gateway URL returned in leases")
	fs.StringVar(&cfg.AuthKey, "auth-key", cfg.AuthKey, "ProxyHarbor-Key header value")
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
	fs.StringVar(&cfg.HealthcheckURL, "healthcheck-url", cfg.HealthcheckURL, "active healthcheck URL")
	fs.DurationVar(&cfg.HealthcheckInterval, "healthcheck-interval", cfg.HealthcheckInterval, "active healthcheck interval")
	fs.DurationVar(&cfg.HealthcheckTimeout, "healthcheck-timeout", cfg.HealthcheckTimeout, "active healthcheck timeout")
	fs.BoolVar(&cfg.StickyEnabled, "sticky-enabled", cfg.StickyEnabled, "enable sticky affinity placeholder")
	fs.StringVar(&cfg.StickyPolicy, "sticky-policy", cfg.StickyPolicy, "sticky affinity policy")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	cfg.StorageDriver = StorageDriver(*storageStr)
	tenantKeys, err := parseTenantKeys(os.Getenv("PROXYHARBOR_TENANT_KEYS"), cfg.TenantKeyMinLen)
	if err != nil {
		return Config{}, err
	}
	cfg.TenantKeys = tenantKeys
	return cfg, cfg.validate()
}

// parseTenantKeys parses entries of the form `tenantID:key,tenantID:key`.
// The first ':' separates tenant from key; key may itself contain ':'.
// Returns a non-nil map only when at least one entry was provided.
func parseTenantKeys(raw string, minKeyLen int) (map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if minKeyLen <= 0 {
		minKeyLen = 16
	}
	keyToTenant := make(map[string]string)
	seenTenant := make(map[string]struct{})
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		idx := strings.Index(item, ":")
		if idx <= 0 || idx == len(item)-1 {
			return nil, fmt.Errorf("invalid PROXYHARBOR_TENANT_KEYS entry %q: expected tenant:key", item)
		}
		tenant := strings.TrimSpace(item[:idx])
		key := strings.TrimSpace(item[idx+1:])
		if !validTenantID(tenant) {
			return nil, fmt.Errorf("invalid tenant id %q in PROXYHARBOR_TENANT_KEYS", tenant)
		}
		if len(key) < minKeyLen {
			return nil, fmt.Errorf("tenant key for %q is shorter than minimum length %d", tenant, minKeyLen)
		}
		if _, dup := seenTenant[tenant]; dup {
			return nil, fmt.Errorf("duplicate tenant %q in PROXYHARBOR_TENANT_KEYS", tenant)
		}
		if existing, dup := keyToTenant[key]; dup {
			return nil, fmt.Errorf("duplicate tenant key shared by %q and %q", existing, tenant)
		}
		keyToTenant[key] = tenant
		seenTenant[tenant] = struct{}{}
	}
	if len(keyToTenant) == 0 {
		return nil, nil
	}
	return keyToTenant, nil
}

// validTenantID mirrors auth.ValidTenantID without creating an import cycle.
func validTenantID(tenantID string) bool {
	if len(tenantID) == 0 || len(tenantID) > 64 {
		return false
	}
	for _, r := range tenantID {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func (c Config) validate() error {
	hasLegacy := strings.TrimSpace(c.AuthKey) != ""
	hasTenantKeys := len(c.TenantKeys) > 0
	switch {
	case hasLegacy && hasTenantKeys:
		return errors.New("ambiguous auth config: set either PROXYHARBOR_AUTH_KEY (legacy) or PROXYHARBOR_TENANT_KEYS, not both")
	case !hasLegacy && !hasTenantKeys:
		return errors.New("auth key is required: set PROXYHARBOR_TENANT_KEYS (recommended) or PROXYHARBOR_AUTH_KEY (legacy)")
	}
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
	if c.StickyPolicy == "" {
		return errors.New("sticky policy must not be empty")
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
