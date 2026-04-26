// Package config 加载 proxyharbor 运行配置。
//
// 加载顺序（后者覆盖前者）：
//  1. 内置默认值
//  2. 环境变量
//  3. 命令行 flag
//
// 环境变量统一前缀 PROXYHARBOR_*。
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

// StorageDriver 决定底层 Store 实现。
type StorageDriver string

const (
	DriverMemory StorageDriver = "memory" // 仅供本地开发与测试
	DriverMySQL  StorageDriver = "mysql"  // 生产推荐
)

// Config 是 proxyharbor 进程启动所需的全部配置。
type Config struct {
	Role                       string        // all | controller | gateway
	Addr                       string        // HTTP 监听地址
	GatewayURL                 string        // Lease 中回填的网关地址
	AuthKey                    string        // ProxyHarbor-Key 头校验值
	StorageDriver              StorageDriver // memory | mysql
	MySQLDSN                   string        // 形如 user:pass@tcp(host:3306)/db?parseTime=true
	MySQLMaxOpen               int
	MySQLMaxIdle               int
	MySQLConnMaxAge            time.Duration
	RedisAddr                  string // 例如 redis:6379；为空时禁用缓存
	RedisPassword              string
	RedisDB                    int
	CacheTTL                   time.Duration // Catalog/Lease 默认缓存时长
	ShutdownTimeout            time.Duration
	AllowInternalProxyEndpoint bool // 仅 dev/本地：允许上游代理指向 loopback/私网
}

// Load 解析环境变量与命令行 flag 后返回配置。
func Load(args []string) (Config, error) {
	cfg := Config{
		Role:                       envStr("PROXYHARBOR_ROLE", "all"),
		Addr:                       envStr("PROXYHARBOR_ADDR", ":8080"),
		GatewayURL:                 envStr("PROXYHARBOR_GATEWAY_URL", "http://localhost:8080"),
		AuthKey:                    os.Getenv("PROXYHARBOR_AUTH_KEY"),
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
	}

	fs := flag.NewFlagSet("proxyharbor", flag.ContinueOnError)
	fs.StringVar(&cfg.Role, "role", cfg.Role, "进程角色：all | controller | gateway")
	fs.StringVar(&cfg.Addr, "addr", cfg.Addr, "HTTP 监听地址")
	fs.StringVar(&cfg.GatewayURL, "gateway-url", cfg.GatewayURL, "lease 中回填的 gateway URL")
	fs.StringVar(&cfg.AuthKey, "auth-key", cfg.AuthKey, "ProxyHarbor-Key 头校验值")
	storageStr := fs.String("storage", string(cfg.StorageDriver), "存储驱动：memory | mysql")
	fs.StringVar(&cfg.MySQLDSN, "mysql-dsn", cfg.MySQLDSN, "MySQL DSN")
	fs.StringVar(&cfg.RedisAddr, "redis-addr", cfg.RedisAddr, "Redis 地址，为空禁用缓存")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	cfg.StorageDriver = StorageDriver(*storageStr)
	return cfg, cfg.validate()
}

func (c Config) validate() error {
	if c.AuthKey == "" {
		return errors.New("auth key 必须提供：设置 -auth-key 或环境变量 PROXYHARBOR_AUTH_KEY")
	}
	switch c.Role {
	case "all", "controller", "gateway":
	default:
		return fmt.Errorf("非法 role: %q", c.Role)
	}
	switch c.StorageDriver {
	case DriverMemory:
	case DriverMySQL:
		if strings.TrimSpace(c.MySQLDSN) == "" {
			return errors.New("storage=mysql 需要 -mysql-dsn 或 PROXYHARBOR_MYSQL_DSN")
		}
	default:
		return fmt.Errorf("不支持的 storage 驱动: %q", c.StorageDriver)
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
