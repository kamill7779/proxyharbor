package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kamill7779/proxyharbor/internal/storage"
	_ "modernc.org/sqlite"
)

type sqliteBackupMetadata struct {
	SourcePath     string `json:"source_path"`
	SchemaVersion  int    `json:"schema_version"`
	CreatedAt      string `json:"created_at"`
	ChecksumSHA256 string `json:"checksum_sha256"`
	BackupPath     string `json:"backup_path"`
}

func runOpsCommand(args []string, stdout, stderr io.Writer) (bool, int) {
	if len(args) == 0 {
		return false, 0
	}
	switch args[0] {
	case "backup":
		return true, runBackupCommand(args[1:], stdout, stderr)
	case "restore":
		return true, runRestoreCommand(args[1:], stdout, stderr)
	case "retention":
		return true, runRetentionCommand(args[1:], stdout, stderr)
	default:
		return false, 0
	}
}

func runBackupCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	input := fs.String("sqlite-path", os.Getenv("PROXYHARBOR_SQLITE_PATH"), "SQLite DB path")
	output := fs.String("output", "", "backup output path")
	offline := fs.Bool("offline", false, "confirm the process is stopped before file-level backup")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := sqliteBackup(*input, *output, *offline); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "backup written to %s\nmetadata written to %s\n", *output, *output+".metadata.json")
	return 0
}

func runRestoreCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(stderr)
	input := fs.String("input", "", "backup input path")
	output := fs.String("sqlite-path", os.Getenv("PROXYHARBOR_SQLITE_PATH"), "SQLite DB path to restore")
	force := fs.Bool("force", false, "confirm the process is stopped and replace target DB")
	doctor := fs.Bool("doctor", false, "validate restored SQLite DB before returning")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := offlineSQLiteRestore(*input, *output, *force); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "restored %s from %s\n", *output, *input)
	if *doctor {
		if err := sqliteDoctorChecks(*output); err != nil {
			fmt.Fprintf(stderr, "restore doctor failed: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, "doctor OK")
	}
	return 0
}

func runRetentionCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("retention", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sqlitePath := fs.String("sqlite-path", os.Getenv("PROXYHARBOR_SQLITE_PATH"), "SQLite DB path")
	auditDays := fs.Int("audit-days", 0, "audit retention in days; 0 disables cleanup")
	usageDays := fs.Int("usage-days", 0, "usage retention in days; 0 disables cleanup")
	execute := fs.Bool("execute", false, "delete rows; default is dry-run")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	statements := storage.BuildRetentionStatements(storage.RetentionPolicy{AuditRetentionDays: *auditDays, UsageRetentionDays: *usageDays})
	if len(statements) == 0 {
		fmt.Fprintln(stdout, "retention disabled; set --audit-days or --usage-days")
		return 0
	}
	if strings.TrimSpace(*sqlitePath) == "" {
		for _, statement := range statements {
			fmt.Fprintf(stdout, "%s: %s; cutoff = now - %d days\n", statement.Kind, statement.SQL, statement.RetentionDays)
		}
		return 0
	}
	if err := runSQLiteRetention(*sqlitePath, statements, *execute, stdout); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func runSQLiteRetention(path string, statements []storage.RetentionStatement, execute bool, stdout io.Writer) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	now := time.Now().UTC()
	if !execute {
		fmt.Fprintln(stdout, "retention mode=dry-run")
		for _, statement := range statements {
			cutoff := now.AddDate(0, 0, -statement.RetentionDays)
			var count int64
			if err := db.QueryRow(`SELECT COUNT(*) FROM `+statement.Table+` WHERE occurred_at < ?`, cutoff).Scan(&count); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "%s would_delete=%d cutoff=%s\n", statement.Kind, count, cutoff.Format(time.RFC3339))
		}
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	fmt.Fprintln(stdout, "retention mode=execute")
	for _, statement := range statements {
		cutoff := now.AddDate(0, 0, -statement.RetentionDays)
		result, err := tx.Exec(statement.SQL, cutoff)
		if err != nil {
			return err
		}
		rows, _ := result.RowsAffected()
		fmt.Fprintf(stdout, "%s deleted=%d cutoff=%s\n", statement.Kind, rows, cutoff.Format(time.RFC3339))
	}
	return tx.Commit()
}

func sqliteBackup(input, output string, offline bool) error {
	input = strings.TrimSpace(input)
	output = strings.TrimSpace(output)
	if input == "" {
		return errors.New("backup requires --sqlite-path or PROXYHARBOR_SQLITE_PATH")
	}
	if output == "" {
		return errors.New("backup requires --output")
	}
	if offline && hasSQLiteSidecarFiles(input) {
		return errors.New("offline SQLite backup requires checkpointed database; stop ProxyHarbor and remove or checkpoint -wal/-shm files first")
	}
	if err := vacuumIntoSQLite(input, output); err != nil {
		return err
	}
	return writeSQLiteBackupMetadata(input, output)
}

func vacuumIntoSQLite(input, output string) error {
	source, err := filepath.Abs(input)
	if err != nil {
		return err
	}
	dest, err := filepath.Abs(output)
	if err != nil {
		return err
	}
	if source == dest {
		return errors.New("source and destination paths must differ")
	}
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("destination already exists: %s", dest)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", source)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec(`VACUUM INTO ?`, dest); err != nil {
		return err
	}
	return os.Chmod(dest, 0o600)
}

func offlineSQLiteRestore(input, output string, force bool) error {
	input = strings.TrimSpace(input)
	output = strings.TrimSpace(output)
	if input == "" {
		return errors.New("restore requires --input")
	}
	if output == "" {
		return errors.New("sqlite restore unsupported: set --sqlite-path or PROXYHARBOR_SQLITE_PATH after SQLite storage is integrated")
	}
	if !force {
		return errors.New("restore requires --force to confirm ProxyHarbor is stopped and target DB may be replaced")
	}
	if hasSQLiteSidecarFiles(output) {
		return errors.New("offline SQLite restore requires a clean destination; remove or checkpoint existing -wal/-shm files first")
	}
	return replaceSQLiteFile(input, output)
}

func replaceSQLiteFile(input, output string) error {
	dest, err := filepath.Abs(output)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".restore-*.db")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := copySQLiteFile(input, tmpPath, true); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, dest); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

func copySQLiteFile(input, output string, replace bool) error {
	source, err := filepath.Abs(input)
	if err != nil {
		return err
	}
	dest, err := filepath.Abs(output)
	if err != nil {
		return err
	}
	if source == dest {
		return errors.New("source and destination paths must differ")
	}
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", source)
	}
	if !replace {
		if _, err := os.Stat(dest); err == nil {
			return fmt.Errorf("destination already exists: %s", dest)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}

	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()

	flags := os.O_WRONLY | os.O_CREATE
	if replace {
		flags |= os.O_TRUNC
	} else {
		flags |= os.O_EXCL
	}
	out, err := os.OpenFile(dest, flags, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Chmod(0o600); err != nil {
		return err
	}
	return out.Sync()
}

func writeSQLiteBackupMetadata(input, output string) error {
	source, err := filepath.Abs(input)
	if err != nil {
		return err
	}
	dest, err := filepath.Abs(output)
	if err != nil {
		return err
	}
	checksum, err := fileSHA256(dest)
	if err != nil {
		return err
	}
	version, err := sqliteSchemaVersionOf(dest)
	if err != nil {
		return err
	}
	metadata := sqliteBackupMetadata{
		SourcePath:     source,
		SchemaVersion:  version,
		CreatedAt:      time.Now().UTC().Format(time.RFC3339Nano),
		ChecksumSHA256: checksum,
		BackupPath:     dest,
	}
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(dest+".metadata.json", append(data, '\n'), 0o600)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func sqliteSchemaVersionOf(path string) (int, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return storage.SQLMigrationDB{DB: db}.SchemaVersion(ctx)
}

func sqliteDoctorChecks(path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("sqlite path is empty")
	}
	if _, err := os.Stat(path); err != nil {
		return err
	}
	if hasSQLiteSidecarFiles(path) {
		return errors.New("sqlite sidecar files exist; stop ProxyHarbor or checkpoint WAL first")
	}
	version, err := sqliteSchemaVersionOf(path)
	if err != nil {
		return err
	}
	if version != 1 {
		return fmt.Errorf("sqlite schema version %d does not match expected 1", version)
	}
	return nil
}

func hasSQLiteSidecarFiles(path string) bool {
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); err == nil {
			return true
		}
	}
	return false
}
