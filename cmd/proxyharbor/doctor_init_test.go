package main

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
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

func TestDoctorSQLiteChecksFileSchemaAndSidecars(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "proxyharbor.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_version (version INTEGER NOT NULL PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_version (version, applied_at) VALUES (1, ?)`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath+"-wal", []byte("wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROXYHARBOR_ADMIN_KEY", "admin-key-with-at-least-thirty-two-bytes")
	t.Setenv("PROXYHARBOR_KEY_PEPPER", "pepper-with-at-least-thirty-two-bytes")

	var out bytes.Buffer
	code := runDoctor([]string{
		"-storage=sqlite",
		"-sqlite-path=" + dbPath,
		"-selector-redis-required=false",
	}, &out, nil)
	if code != 1 {
		t.Fatalf("doctor exit code = %d, want 1; output: %s", code, out.String())
	}
	for _, want := range []string{"sqlite file", "sqlite schema version", "sqlite sidecar -wal", "sqlite disk space"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("doctor output missing %q: %s", want, out.String())
		}
	}
}

func TestInitSQLiteCreatesSchema(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "proxyharbor.db")
	var out bytes.Buffer
	var stderr bytes.Buffer
	code := runInit([]string{"-storage=sqlite", "-sqlite-path=" + dbPath}, &out, &stderr)
	if code != 0 {
		t.Fatalf("init exit code = %d, want 0; stdout=%s stderr=%s", code, out.String(), stderr.String())
	}
	if !strings.Contains(out.String(), "sqlite initialized") {
		t.Fatalf("init output missing success message: %s", out.String())
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatalf("query schema_version: %v", err)
	}
	if version != 1 {
		t.Fatalf("schema version = %d, want 1", version)
	}

	out.Reset()
	stderr.Reset()
	code = runInit([]string{"-storage=sqlite", "-sqlite-path=" + dbPath}, &out, &stderr)
	if code != 0 {
		t.Fatalf("second init exit code = %d, want 0; stdout=%s stderr=%s", code, out.String(), stderr.String())
	}
}
