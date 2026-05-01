package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

func TestSQLiteAutoSecretsGeneratesAndReusesFile(t *testing.T) {
	dir := t.TempDir()
	secretsPath := filepath.Join(dir, "secrets.env")
	dbPath := filepath.Join(dir, "proxyharbor.db")
	t.Setenv("PROXYHARBOR_ADMIN_KEY", "")
	t.Setenv("PROXYHARBOR_KEY_PEPPER", "")

	cfg, err := Load([]string{"-storage=sqlite", "-sqlite-path=" + dbPath, "-secrets-file=" + secretsPath})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.AdminKey) != 64 || len(cfg.KeyPepper) != 64 {
		t.Fatalf("generated secret lengths = admin %d pepper %d, want 64/64", len(cfg.AdminKey), len(cfg.KeyPepper))
	}
	raw, err := os.ReadFile(secretsPath)
	if err != nil {
		t.Fatalf("read generated secrets: %v", err)
	}
	if !strings.Contains(string(raw), "PROXYHARBOR_ADMIN_KEY=") || !strings.Contains(string(raw), "PROXYHARBOR_KEY_PEPPER=") {
		t.Fatalf("generated file missing expected keys: %s", raw)
	}

	again, err := Load([]string{"-storage=sqlite", "-sqlite-path=" + dbPath, "-secrets-file=" + secretsPath})
	if err != nil {
		t.Fatalf("Load() second error = %v", err)
	}
	if again.AdminKey != cfg.AdminKey || again.KeyPepper != cfg.KeyPepper {
		t.Fatal("secrets were not reused across loads")
	}
}

func TestMySQLDoesNotAutoGenerateSecrets(t *testing.T) {
	t.Setenv("PROXYHARBOR_ADMIN_KEY", "")
	t.Setenv("PROXYHARBOR_KEY_PEPPER", "")
	_, err := Load([]string{"-storage=mysql", "-mysql-dsn=user:pass@tcp(localhost:3306)/proxyharbor"})
	if err == nil || !strings.Contains(err.Error(), "PROXYHARBOR_ADMIN_KEY is required") {
		t.Fatalf("Load() error = %v, want explicit admin key requirement", err)
	}
}

func TestEnvOverridesSecretsFile(t *testing.T) {
	dir := t.TempDir()
	secretsPath := filepath.Join(dir, "secrets.env")
	if err := os.WriteFile(secretsPath, []byte("PROXYHARBOR_ADMIN_KEY=file-admin-with-at-least-thirty-two-bytes\nPROXYHARBOR_KEY_PEPPER=file-pepper-with-at-least-thirty-two-bytes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROXYHARBOR_ADMIN_KEY", "env-admin-with-at-least-thirty-two-bytes")
	t.Setenv("PROXYHARBOR_KEY_PEPPER", "env-pepper-with-at-least-thirty-two-bytes")
	cfg, err := Load([]string{"-storage=sqlite", "-sqlite-path=" + filepath.Join(dir, "proxyharbor.db"), "-secrets-file=" + secretsPath})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.AdminKey != "env-admin-with-at-least-thirty-two-bytes" || cfg.KeyPepper != "env-pepper-with-at-least-thirty-two-bytes" {
		t.Fatalf("env did not override secrets file: admin=%q pepper=%q", cfg.AdminKey, cfg.KeyPepper)
	}
}

func TestSecretsFileFlagSurvivesSQLiteDefaultPathRewrite(t *testing.T) {
	dir := t.TempDir()
	secretsPath := filepath.Join(dir, "custom", "secrets.env")
	dbPath := filepath.Join(dir, "proxyharbor.db")
	t.Setenv("PROXYHARBOR_SECRETS_FILE", "")
	t.Setenv("PROXYHARBOR_ADMIN_KEY", "")
	t.Setenv("PROXYHARBOR_KEY_PEPPER", "")

	cfg, err := Load([]string{"-storage=sqlite", "-sqlite-path=" + dbPath, "-secrets-file=" + secretsPath})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.SecretsFile != secretsPath {
		t.Fatalf("SecretsFile = %q, want explicit flag path %q", cfg.SecretsFile, secretsPath)
	}
	if _, err := os.Stat(secretsPath); err != nil {
		t.Fatalf("explicit secrets file was not written: %v", err)
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

func TestHAModeRequiresStrictRedisZFair(t *testing.T) {
	t.Setenv("PROXYHARBOR_ADMIN_KEY", "admin-key-with-at-least-thirty-two-bytes")
	t.Setenv("PROXYHARBOR_KEY_PEPPER", "pepper-with-at-least-thirty-two-bytes")

	_, err := Load([]string{
		"-storage=mysql",
		"-mysql-dsn=user:pass@tcp(localhost:3306)/proxyharbor",
		"-cluster-enabled=true",
		"-selector=local",
		"-selector-redis-required=false",
	})
	if err == nil || !strings.Contains(err.Error(), "ha mode requires storage=mysql with selector=zfair") {
		t.Fatalf("Load() error = %v, want HA strict zfair requirement", err)
	}

	cfg, err := Load([]string{
		"-storage=mysql",
		"-mysql-dsn=user:pass@tcp(localhost:3306)/proxyharbor",
		"-cluster-enabled=true",
		"-selector=zfair",
		"-selector-redis-required=true",
		"-redis-addr=redis:6379",
	})
	if err != nil {
		t.Fatalf("Load() strict zfair error = %v", err)
	}
	if cfg.Selector != "zfair" || !cfg.SelectorRedisRequired || cfg.RedisAddr == "" {
		t.Fatalf("cfg = %+v, want strict zfair redis", cfg)
	}
}
