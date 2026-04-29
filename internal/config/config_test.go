package config

import "testing"

func TestDefaultSelectorIsLocal(t *testing.T) {
	t.Setenv("PROXYHARBOR_ADMIN_KEY", "admin-key-with-at-least-thirty-two-bytes")
	t.Setenv("PROXYHARBOR_KEY_PEPPER", "pepper-with-at-least-thirty-two-bytes")

	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Selector != "local" {
		t.Fatalf("Selector = %q, want local", cfg.Selector)
	}
}

func TestRedisRequiredDefaultSelectorIsZFair(t *testing.T) {
	t.Setenv("PROXYHARBOR_ADMIN_KEY", "admin-key-with-at-least-thirty-two-bytes")
	t.Setenv("PROXYHARBOR_KEY_PEPPER", "pepper-with-at-least-thirty-two-bytes")
	t.Setenv("PROXYHARBOR_REDIS_ADDR", "redis:6379")
	t.Setenv("PROXYHARBOR_SELECTOR_REDIS_REQUIRED", "true")

	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Selector != "zfair" {
		t.Fatalf("Selector = %q, want zfair", cfg.Selector)
	}
}
