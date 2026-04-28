package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestOfflineSQLiteBackupRequiresExplicitOffline(t *testing.T) {
	source := filepath.Join(t.TempDir(), "proxyharbor.db")
	if err := os.WriteFile(source, []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := offlineSQLiteBackup(source, filepath.Join(t.TempDir(), "backup.db"), false)
	if err == nil || !strings.Contains(err.Error(), "--offline") {
		t.Fatalf("err = %v, want --offline guidance", err)
	}
}

func TestOfflineSQLiteBackupRejectsSamePath(t *testing.T) {
	source := filepath.Join(t.TempDir(), "proxyharbor.db")
	if err := os.WriteFile(source, []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := offlineSQLiteBackup(source, source, true)
	if err == nil || !strings.Contains(err.Error(), "must differ") {
		t.Fatalf("err = %v, want same-path rejection", err)
	}
}

func TestOfflineSQLiteBackupCopiesFileWhenOffline(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "proxyharbor.db")
	dest := filepath.Join(dir, "backup.db")
	if err := os.WriteFile(source, []byte("db contents"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := offlineSQLiteBackup(source, dest, true); err != nil {
		t.Fatalf("offlineSQLiteBackup returned error: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "db contents" {
		t.Fatalf("backup contents = %q", got)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestOfflineSQLiteBackupRejectsSidecarFiles(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "proxyharbor.db")
	if err := os.WriteFile(source, []byte("db contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source+"-wal", []byte("wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := offlineSQLiteBackup(source, filepath.Join(dir, "backup.db"), true)
	if err == nil || !strings.Contains(err.Error(), "checkpointed") {
		t.Fatalf("err = %v, want sidecar checkpoint guidance", err)
	}
}

func TestOfflineSQLiteRestoreRequiresForce(t *testing.T) {
	backup := filepath.Join(t.TempDir(), "backup.db")
	if err := os.WriteFile(backup, []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := offlineSQLiteRestore(backup, filepath.Join(t.TempDir(), "proxyharbor.db"), false)
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("err = %v, want --force guidance", err)
	}
}
