package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoctorDoesNotLeakSecrets(t *testing.T) {
	t.Setenv("PROXYHARBOR_ADMIN_KEY", "super-secret-admin-key-with-enough-bytes")
	t.Setenv("PROXYHARBOR_KEY_PEPPER", "super-secret-pepper-value-with-enough-bytes")
	t.Setenv("PROXYHARBOR_MYSQL_DSN", "ph:secret-pass@tcp(localhost:3306)/proxyharbor?parseTime=true")
	t.Setenv("PROXYHARBOR_REDIS_ADDR", "localhost:6379")
	t.Setenv("PROXYHARBOR_REDIS_PASSWORD", "secret-redis-pass")

	var out bytes.Buffer
	code := runDoctor([]string{"-storage=mysql"}, &out, nil)
	if code != 0 {
		t.Fatalf("doctor exit code = %d, want 0; output: %s", code, out.String())
	}
	output := out.String()
	for _, secret := range []string{"super-secret-admin", "super-secret-pepper", "secret-pass", "secret-redis-pass"} {
		if strings.Contains(output, secret) {
			t.Fatalf("doctor output leaked secret %q: %s", secret, output)
		}
	}
	for _, want := range []string{"storage driver mysql", "mysql dsn configured", "admin key configured", "key pepper configured"} {
		if !strings.Contains(output, want) {
			t.Fatalf("doctor output missing %q: %s", want, output)
		}
	}
}

func TestDoctorSQLiteChecksParentDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PROXYHARBOR_ADMIN_KEY", "admin-key-with-at-least-thirty-two-bytes")
	t.Setenv("PROXYHARBOR_KEY_PEPPER", "pepper-with-at-least-thirty-two-bytes")

	var out bytes.Buffer
	code := runDoctor([]string{
		"-storage=sqlite",
		"-sqlite-path=" + filepath.Join(dir, "proxyharbor.db"),
		"-selector-redis-required=false",
	}, &out, nil)
	if code != 0 {
		t.Fatalf("doctor exit code = %d, want 0; output: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "sqlite path parent writable") {
		t.Fatalf("doctor output missing sqlite parent check: %s", out.String())
	}
}

func TestInitSQLiteComingSoon(t *testing.T) {
	var out bytes.Buffer
	code := runInit([]string{"-storage=sqlite"}, &out, nil)
	if code == 0 {
		t.Fatalf("init exit code = %d, want non-zero while sqlite init is not integrated", code)
	}
	if !strings.Contains(out.String(), "sqlite initialization is not available yet") {
		t.Fatalf("init output missing coming-soon message: %s", out.String())
	}
}
