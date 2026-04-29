package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestSQLiteBackupRejectsSamePath(t *testing.T) {
	source := filepath.Join(t.TempDir(), "proxyharbor.db")
	initTestOpsDB(t, source)

	err := sqliteBackup(source, source, false)
	if err == nil || !strings.Contains(err.Error(), "must differ") {
		t.Fatalf("err = %v, want same-path rejection", err)
	}
}

func TestSQLiteBackupCreatesRestrictedFile(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "proxyharbor.db")
	dest := filepath.Join(dir, "backup.db")
	initTestOpsDB(t, source)

	if err := sqliteBackup(source, dest, false); err != nil {
		t.Fatalf("sqliteBackup returned error: %v", err)
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
	err := sqliteBackup(source, filepath.Join(dir, "backup.db"), true)
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

func TestOfflineSQLiteRestoreRejectsDestinationSidecarFiles(t *testing.T) {
	dir := t.TempDir()
	backup := filepath.Join(dir, "backup.db")
	target := filepath.Join(dir, "proxyharbor.db")
	if err := os.WriteFile(backup, []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target+"-wal", []byte("wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := offlineSQLiteRestore(backup, target, true)
	if err == nil || !strings.Contains(err.Error(), "clean destination") {
		t.Fatalf("err = %v, want clean destination guidance", err)
	}
}

func TestOfflineSQLiteRestoreReplacesTargetAtomically(t *testing.T) {
	dir := t.TempDir()
	backup := filepath.Join(dir, "backup.db")
	target := filepath.Join(dir, "proxyharbor.db")
	if err := os.WriteFile(backup, []byte("new database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("old database"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := offlineSQLiteRestore(backup, target, true); err != nil {
		t.Fatalf("offlineSQLiteRestore() error = %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new database" {
		t.Fatalf("target = %q, want restored backup", string(got))
	}
	matches, err := filepath.Glob(filepath.Join(dir, ".restore-*.db"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary restore files left behind: %v", matches)
	}
}

func TestRunOpsCommandDispatchesRetention(t *testing.T) {
	var out strings.Builder
	handled, code := runOpsCommand([]string{"retention", "--audit-days", "30"}, &out, &strings.Builder{})
	if !handled || code != 0 {
		t.Fatalf("handled=%v code=%d, want handled true code 0", handled, code)
	}
	if !strings.Contains(out.String(), "audit:") {
		t.Fatalf("retention output = %q, want audit preview", out.String())
	}
}

func TestRetentionExecuteRequiresSQLitePath(t *testing.T) {
	var stdout strings.Builder
	var stderr strings.Builder
	code := runRetentionCommand([]string{"--audit-days", "30", "--execute"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "requires --sqlite-path") {
		t.Fatalf("stderr = %q, want sqlite path requirement", stderr.String())
	}
}

func TestBackupCommandWritesMetadataAndUsableCopy(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "proxyharbor.db")
	initTestOpsDB(t, source)
	dest := filepath.Join(dir, "backup.db")

	var out strings.Builder
	code := runBackupCommand([]string{"--sqlite-path", source, "--output", dest}, &out, &strings.Builder{})
	if code != 0 {
		t.Fatalf("backup code = %d output=%s", code, out.String())
	}

	db, err := sql.Open("sqlite", dest)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		t.Fatalf("query backup schema version: %v", err)
	}
	if version != 1 {
		t.Fatalf("backup schema version = %d, want 1", version)
	}

	metadataBytes, err := os.ReadFile(dest + ".metadata.json")
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	var metadata sqliteBackupMetadata
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if metadata.SourcePath == "" || metadata.SchemaVersion != 1 || metadata.CreatedAt == "" || metadata.ChecksumSHA256 == "" {
		t.Fatalf("metadata missing fields: %+v", metadata)
	}
}

func TestSQLiteBackupMetadataChecksumMatchesBackup(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "proxyharbor.db")
	dest := filepath.Join(dir, "backup.db")
	initTestOpsDB(t, source)
	if err := sqliteBackup(source, dest, false); err != nil {
		t.Fatalf("sqliteBackup() error = %v", err)
	}
	metadataBytes, err := os.ReadFile(dest + ".metadata.json")
	if err != nil {
		t.Fatal(err)
	}
	var metadata sqliteBackupMetadata
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
		t.Fatal(err)
	}
	checksum, err := fileSHA256(dest)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.ChecksumSHA256 != checksum {
		t.Fatalf("metadata checksum = %s, want %s", metadata.ChecksumSHA256, checksum)
	}
}

func TestSQLiteDoctorRejectsMetadataChecksumMismatch(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "proxyharbor.db")
	dest := filepath.Join(dir, "backup.db")
	initTestOpsDB(t, source)
	if err := sqliteBackup(source, dest, false); err != nil {
		t.Fatalf("sqliteBackup() error = %v", err)
	}
	metadataBytes, err := os.ReadFile(dest + ".metadata.json")
	if err != nil {
		t.Fatal(err)
	}
	var metadata sqliteBackupMetadata
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
		t.Fatal(err)
	}
	metadata.ChecksumSHA256 = strings.Repeat("0", 64)
	damaged, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest+".metadata.json", damaged, 0o600); err != nil {
		t.Fatal(err)
	}
	err = sqliteDoctorChecks(dest)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v, want checksum mismatch", err)
	}
}

func TestSQLiteDoctorRejectsCorruptDatabase(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "proxyharbor.db")
	if err := os.WriteFile(dbPath, []byte("not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := sqliteDoctorChecks(dbPath)
	if err == nil {
		t.Fatal("sqliteDoctorChecks() error = nil, want corrupt database error")
	}
}

func TestRestoreCommandCanValidateWithDoctor(t *testing.T) {
	dir := t.TempDir()
	backup := filepath.Join(dir, "backup.db")
	target := filepath.Join(dir, "proxyharbor.db")
	initTestOpsDB(t, backup)

	var out strings.Builder
	stderr := strings.Builder{}
	code := runRestoreCommand([]string{"--input", backup, "--sqlite-path", target, "--force", "--doctor"}, &out, &stderr)
	if code != 0 {
		t.Fatalf("restore code = %d stdout=%s stderr=%s", code, out.String(), stderr.String())
	}
	if !strings.Contains(out.String(), "doctor OK") {
		t.Fatalf("restore output missing doctor OK: %s", out.String())
	}
}

func TestRetentionDryRunAndExecute(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "proxyharbor.db")
	initTestOpsDB(t, dbPath)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	oldTime := time.Now().UTC().AddDate(0, 0, -40).Format(time.RFC3339Nano)
	newTime := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT INTO proxy_audit_events (event_id, tenant_id, actor, action, resource, occurred_at) VALUES ('old-audit','t','a','x','r',?), ('new-audit','t','a','x','r',?)`, oldTime, newTime); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO proxy_usage_events (event_id, tenant_id, lease_id, bytes_sent, bytes_received, occurred_at) VALUES ('old-usage','t','l',1,1,?), ('new-usage','t','l',1,1,?)`, oldTime, newTime); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	code := runRetentionCommand([]string{"--sqlite-path", dbPath, "--audit-days", "30", "--usage-days", "30"}, &out, &strings.Builder{})
	if code != 0 || !strings.Contains(out.String(), "dry-run") || !strings.Contains(out.String(), "would_delete=1") {
		t.Fatalf("dry-run code=%d output=%s", code, out.String())
	}
	assertTableCount(t, db, "proxy_audit_events", 2)

	out.Reset()
	code = runRetentionCommand([]string{"--sqlite-path", dbPath, "--audit-days", "30", "--usage-days", "30", "--execute"}, &out, &strings.Builder{})
	if code != 0 || !strings.Contains(out.String(), "deleted=1") {
		t.Fatalf("execute code=%d output=%s", code, out.String())
	}
	assertTableCount(t, db, "proxy_audit_events", 1)
	assertTableCount(t, db, "proxy_usage_events", 1)
}

func initTestOpsDB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE schema_version (version INTEGER NOT NULL PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_version (version, applied_at) VALUES (1, ?)`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE proxy_audit_events (event_id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, actor TEXT NOT NULL, action TEXT NOT NULL, resource TEXT NOT NULL, occurred_at DATETIME NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE proxy_usage_events (event_id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, lease_id TEXT NOT NULL, bytes_sent INTEGER NOT NULL, bytes_received INTEGER NOT NULL, occurred_at DATETIME NOT NULL)`); err != nil {
		t.Fatal(err)
	}
}

func assertTableCount(t *testing.T, db *sql.DB, table string, want int) {
	t.Helper()
	var got int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s count = %d, want %d", table, got, want)
	}
}
