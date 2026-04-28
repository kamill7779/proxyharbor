package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kamill7779/proxyharbor/internal/storage"
)

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
	if err := offlineSQLiteBackup(*input, *output, *offline); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "backup written to %s\n", *output)
	return 0
}

func runRestoreCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(stderr)
	input := fs.String("input", "", "backup input path")
	output := fs.String("sqlite-path", os.Getenv("PROXYHARBOR_SQLITE_PATH"), "SQLite DB path to restore")
	force := fs.Bool("force", false, "confirm the process is stopped and replace target DB")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := offlineSQLiteRestore(*input, *output, *force); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "restored %s from %s\n", *output, *input)
	return 0
}

func runRetentionCommand(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("retention", flag.ContinueOnError)
	fs.SetOutput(stderr)
	auditDays := fs.Int("audit-days", 0, "audit retention in days; 0 disables cleanup")
	usageDays := fs.Int("usage-days", 0, "usage retention in days; 0 disables cleanup")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	statements := storage.BuildRetentionStatements(storage.RetentionPolicy{AuditRetentionDays: *auditDays, UsageRetentionDays: *usageDays})
	if len(statements) == 0 {
		fmt.Fprintln(stdout, "retention disabled; set --audit-days or --usage-days")
		return 0
	}
	for _, statement := range statements {
		fmt.Fprintf(stdout, "%s: %s; cutoff = now - %d days\n", statement.Kind, statement.SQL, statement.RetentionDays)
	}
	return 0
}

func offlineSQLiteBackup(input, output string, offline bool) error {
	input = strings.TrimSpace(input)
	output = strings.TrimSpace(output)
	if input == "" {
		return errors.New("sqlite backup unsupported: set --sqlite-path or PROXYHARBOR_SQLITE_PATH after SQLite storage is integrated")
	}
	if output == "" {
		return errors.New("backup requires --output")
	}
	if !offline {
		return errors.New("file-level SQLite backup requires --offline to confirm ProxyHarbor is stopped; online backup API is reserved for SQLite Store integration")
	}
	return copySQLiteFile(input, output, false)
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
	return copySQLiteFile(input, output, true)
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
	out, err := os.OpenFile(dest, flags, info.Mode().Perm())
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
