package config

import (
	"testing"

	"github.com/kamill7779/proxyharbor/internal/auth"
)

func TestResolveAuthModeExplicit(t *testing.T) {
	cases := []struct {
		mode       auth.AuthMode
		wantMode   auth.AuthMode
		wantErr    bool
		errContain string
	}{
		{auth.ModeDynamicKeys, auth.ModeDynamicKeys, true, "dynamic-keys requires PROXYHARBOR_ADMIN_KEY"},
		{auth.ModeTenantKeys, auth.ModeTenantKeys, true, "tenant-keys requires PROXYHARBOR_TENANT_KEYS"},
		{auth.ModeLegacy, auth.ModeLegacy, true, "legacy-single-key requires"},
	}
	for _, tc := range cases {
		cfg := validBaseConfig()
		cfg.AuthMode = tc.mode
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
	cfg := validBaseConfig()
	cfg.AuthMode = auth.ModeDynamicKeys
	cfg.AdminKey = "admin-key-1234567890123456789012345678"
	cfg.KeyPepper = "pepper-1234567890123456789012345678"
	if err := cfg.validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveAuthModeTenantKeysOK(t *testing.T) {
	cfg := validBaseConfig()
	cfg.AuthMode = auth.ModeTenantKeys
	cfg.TenantKeys = "t1:k1"
	if err := cfg.validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveAuthModeLegacyOK(t *testing.T) {
	cfg := validBaseConfig()
	cfg.AuthMode = auth.ModeLegacy
	cfg.AuthKey = "legacy-key"
	if err := cfg.validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveAuthModeDefault(t *testing.T) {
	// AdminKey only -> dynamic
	cfg := validBaseConfig()
	cfg.AdminKey = "admin-key-1234567890123456780123456789"
	cfg.KeyPepper = "pepper-123456789012345678901234567890"
	if err := cfg.validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resolveAuthMode(cfg) != auth.ModeDynamicKeys {
		t.Fatalf("expected dynamic-keys default, got %s", resolveAuthMode(cfg))
	}

	// TenantKeys only -> tenant-keys
	cfg = validBaseConfig()
	cfg.TenantKeys = "t1:k1"
	if resolveAuthMode(cfg) != auth.ModeTenantKeys {
		t.Fatalf("expected tenant-keys default, got %s", resolveAuthMode(cfg))
	}

	// AuthKey only -> legacy
	cfg = validBaseConfig()
	cfg.AuthKey = "legacy"
	if resolveAuthMode(cfg) != auth.ModeLegacy {
		t.Fatalf("expected legacy default, got %s", resolveAuthMode(cfg))
	}
}

func TestPepperTooShort(t *testing.T) {
	cfg := validBaseConfig()
	cfg.AuthMode = auth.ModeDynamicKeys
	cfg.AdminKey = "admin-key-1234567890123456789012345678"
	cfg.KeyPepper = "short"
	err := cfg.validate()
	if err == nil {
		t.Fatal("expected error for short pepper")
	}
	if !contains(err.Error(), "at least 32 bytes") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func validBaseConfig() Config {
	return Config{
		Role:                  "all",
		LogFormat:             "json",
		LogLevel:              "info",
		Selector:              "zfair",
		StickyPolicy:          "none",
		HealthBufferMax:       1,
		ZFairQuantum:          1,
		ZFairDefaultLatencyMS: 1,
		ZFairMaxPromote:       1,
		StorageDriver:         DriverMemory,
	}
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
