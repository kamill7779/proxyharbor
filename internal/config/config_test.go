package config

import (
	"strings"
	"testing"
	"time"

	"github.com/kamill7779/proxyharbor/internal/auth"
)

func TestResolveAuthModeExplicit(t *testing.T) {
	cases := []struct {
		mode       auth.AuthMode
		wantMode   auth.AuthMode
		wantErr    bool
		errContain string
	}{
		{auth.ModeDynamicKeys, auth.ModeDynamicKeys, true, "storage=mysql"},
		{auth.ModeTenantKeys, auth.ModeTenantKeys, true, "tenant-keys requires PROXYHARBOR_TENANT_KEYS"},
		{auth.ModeLegacy, auth.ModeLegacy, true, "legacy-single-key requires"},
	}
	for _, tc := range cases {
		cfg := validTestConfig(Config{AuthMode: tc.mode, Role: "all", Selector: "zfair", StickyPolicy: "none", HealthBufferMax: 1, ZFairQuantum: 1, ZFairDefaultLatencyMS: 1, ZFairMaxPromote: 1, StorageDriver: DriverMemory})
		err := cfg.validate()
		if tc.wantErr {
			if err == nil {
				t.Fatalf("mode=%s: expected error", tc.mode)
			}
			if tc.errContain != "" {
				if !contains(err.Error(), tc.errContain) {
					t.Fatalf("mode=%s: error %q does not contain %q", tc.mode, err.Error(), tc.errContain)
				}
			}
		} else {
			if err != nil {
				t.Fatalf("mode=%s: unexpected error: %v", tc.mode, err)
			}
		}
	}
}

func TestResolveAuthModeDynamicOK(t *testing.T) {
	cfg := validTestConfig(Config{
		AuthMode:              auth.ModeDynamicKeys,
		AdminKey:              "admin-key-1234567890123456789012345678",
		KeyPepper:             "pepper-1234567890123456789012345678",
		Role:                  "all",
		Selector:              "zfair",
		StickyPolicy:          "none",
		HealthBufferMax:       1,
		ZFairQuantum:          1,
		ZFairDefaultLatencyMS: 1,
		ZFairMaxPromote:       1,
		StorageDriver:         DriverMySQL,
		MySQLDSN:              "user:pass@tcp(localhost:3306)/proxyharbor",
	})
	if err := cfg.validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDynamicModeRequiresMySQL(t *testing.T) {
	cfg := validTestConfig(Config{
		AuthMode:              auth.ModeDynamicKeys,
		AdminKey:              "admin-key-1234567890123456789012345678",
		KeyPepper:             "pepper-1234567890123456789012345678",
		Role:                  "all",
		Selector:              "zfair",
		StickyPolicy:          "none",
		HealthBufferMax:       1,
		ZFairQuantum:          1,
		ZFairDefaultLatencyMS: 1,
		ZFairMaxPromote:       1,
		StorageDriver:         DriverMemory,
	})
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "storage=mysql") {
		t.Fatalf("expected storage=mysql validation error, got %v", err)
	}
}

func TestLegacyAliasNormalizes(t *testing.T) {
	cfg := validTestConfig(Config{
		AuthMode:              "legacy",
		AuthKey:               "legacy-key",
		Role:                  "all",
		Selector:              "zfair",
		StickyPolicy:          "none",
		HealthBufferMax:       1,
		ZFairQuantum:          1,
		ZFairDefaultLatencyMS: 1,
		ZFairMaxPromote:       1,
		StorageDriver:         DriverMemory,
	})
	if err := cfg.validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resolveAuthMode(cfg) != auth.ModeLegacy {
		t.Fatalf("expected legacy-single-key, got %s", resolveAuthMode(cfg))
	}
}

func TestResolveAuthModeTenantKeysOK(t *testing.T) {
	cfg := validTestConfig(Config{
		AuthMode:              auth.ModeTenantKeys,
		TenantKeys:            "t1:k1",
		Role:                  "all",
		Selector:              "zfair",
		StickyPolicy:          "none",
		HealthBufferMax:       1,
		ZFairQuantum:          1,
		ZFairDefaultLatencyMS: 1,
		ZFairMaxPromote:       1,
		StorageDriver:         DriverMySQL,
		MySQLDSN:              "user:pass@tcp(localhost:3306)/proxyharbor",
	})
	if err := cfg.validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveAuthModeLegacyOK(t *testing.T) {
	cfg := validTestConfig(Config{
		AuthMode:              auth.ModeLegacy,
		AuthKey:               "legacy-key",
		Role:                  "all",
		Selector:              "zfair",
		StickyPolicy:          "none",
		HealthBufferMax:       1,
		ZFairQuantum:          1,
		ZFairDefaultLatencyMS: 1,
		ZFairMaxPromote:       1,
		StorageDriver:         DriverMySQL,
		MySQLDSN:              "user:pass@tcp(localhost:3306)/proxyharbor",
	})
	if err := cfg.validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestTenantKeysModeRejectsMalformedConfig(t *testing.T) {
	cfg := validTestConfig(Config{
		AuthMode:              auth.ModeTenantKeys,
		TenantKeys:            "broken",
		Role:                  "all",
		Selector:              "zfair",
		StickyPolicy:          "none",
		HealthBufferMax:       1,
		ZFairQuantum:          1,
		ZFairDefaultLatencyMS: 1,
		ZFairMaxPromote:       1,
		StorageDriver:         DriverMemory,
	})
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "invalid PROXYHARBOR_TENANT_KEYS") {
		t.Fatalf("expected invalid tenant keys error, got %v", err)
	}
}

func TestResolveAuthModeDefault(t *testing.T) {
	// AdminKey only -> dynamic
	cfg := validTestConfig(Config{
		AdminKey:              "admin-key-1234567890123456780123456789",
		KeyPepper:             "pepper-123456789012345678901234567890",
		MySQLDSN:              "user:pass@tcp(localhost:3306)/proxyharbor",
		Role:                  "all",
		Selector:              "zfair",
		StickyPolicy:          "none",
		HealthBufferMax:       1,
		ZFairQuantum:          1,
		ZFairDefaultLatencyMS: 1,
		ZFairMaxPromote:       1,
		StorageDriver:         DriverMySQL,
	})
	if err := cfg.validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resolveAuthMode(cfg) != auth.ModeDynamicKeys {
		t.Fatalf("expected dynamic-keys default, got %s", resolveAuthMode(cfg))
	}

	// TenantKeys only -> tenant-keys
	cfg = validTestConfig(Config{
		TenantKeys:            "t1:k1",
		Role:                  "all",
		Selector:              "zfair",
		StickyPolicy:          "none",
		HealthBufferMax:       1,
		ZFairQuantum:          1,
		ZFairDefaultLatencyMS: 1,
		ZFairMaxPromote:       1,
		StorageDriver:         DriverMemory,
	})
	if resolveAuthMode(cfg) != auth.ModeTenantKeys {
		t.Fatalf("expected tenant-keys default, got %s", resolveAuthMode(cfg))
	}

	// AuthKey only -> legacy
	cfg = validTestConfig(Config{
		AuthKey:               "legacy",
		Role:                  "all",
		Selector:              "zfair",
		StickyPolicy:          "none",
		HealthBufferMax:       1,
		ZFairQuantum:          1,
		ZFairDefaultLatencyMS: 1,
		ZFairMaxPromote:       1,
		StorageDriver:         DriverMemory,
	})
	if resolveAuthMode(cfg) != auth.ModeLegacy {
		t.Fatalf("expected legacy default, got %s", resolveAuthMode(cfg))
	}
}

func TestPepperTooShort(t *testing.T) {
	cfg := validTestConfig(Config{
		AuthMode:              auth.ModeDynamicKeys,
		AdminKey:              "admin-key-1234567890123456789012345678",
		KeyPepper:             "short",
		Role:                  "all",
		Selector:              "zfair",
		StickyPolicy:          "none",
		HealthBufferMax:       1,
		ZFairQuantum:          1,
		ZFairDefaultLatencyMS: 1,
		ZFairMaxPromote:       1,
		StorageDriver:         DriverMySQL,
		MySQLDSN:              "user:pass@tcp(localhost:3306)/proxyharbor",
	})
	err := cfg.validate()
	if err == nil {
		t.Fatal("expected error for short pepper")
	}
	if !contains(err.Error(), "at least 32 bytes") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDynamicRefreshIntervalMustSatisfyRevocationSLO(t *testing.T) {
	cfg := validTestConfig(Config{
		AuthMode:              auth.ModeDynamicKeys,
		AdminKey:              "admin-key-1234567890123456789012345678",
		KeyPepper:             "pepper-1234567890123456789012345678",
		AuthRefreshInterval:   10 * time.Second,
		Role:                  "all",
		Selector:              "zfair",
		StickyPolicy:          "none",
		HealthBufferMax:       1,
		ZFairQuantum:          1,
		ZFairDefaultLatencyMS: 1,
		ZFairMaxPromote:       1,
		StorageDriver:         DriverMySQL,
		MySQLDSN:              "user:pass@tcp(localhost:3306)/proxyharbor",
	})
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "<= 5s") {
		t.Fatalf("expected refresh interval validation error, got %v", err)
	}
}

func validTestConfig(cfg Config) Config {
	cfg.LogFormat = "json"
	cfg.LogLevel = "info"
	if cfg.AuthRefreshInterval == 0 {
		cfg.AuthRefreshInterval = 5 * time.Second
	}
	return cfg
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsAt(s, substr))
}

func containsAt(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
