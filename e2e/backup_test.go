//go:build linux

package e2e

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func backupWorkspaceCount(t *testing.T, path string) int {
	t.Helper()
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&immutable=1"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workspaces`).Scan(&count); err != nil {
		t.Fatalf("query backup %s: %v", path, err)
	}
	return count
}

func TestCLIBackupExitCodes(t *testing.T) {
	requireCLI(t)
	bin := buildMgcl(t)
	dir := t.TempDir()
	keyPath := writeTestIdentity(t)
	dbPath := filepath.Join(dir, "mgcl.db")

	backupDir := filepath.Join(dir, "backup")
	if err := os.Mkdir(backupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sqliteTarget := filepath.Join(backupDir, "mgcl.db")

	args := []string{
		"run", "--host", "127.0.0.1:0", "--db", dbPath,
		"--encryption-key-file", keyPath,
		"--backup-to", "sqlite:" + sqliteTarget,
		"--", "sh", "-c", "exit 0",
	}
	code, out := runMgclSync(t, bin, dir, baseEnv(), args...)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out)
	}
	if strings.Contains(out, "backup failed") {
		t.Fatalf("unexpected backup failure:\n%s", out)
	}
	if _, err := os.Stat(sqliteTarget); err != nil {
		t.Fatalf("sqlite target missing: %v", err)
	}
	if count := backupWorkspaceCount(t, sqliteTarget); count != 0 {
		t.Fatalf("initial backup workspace count = %d", count)
	}

	// A child's own non-zero code is preserved when backups succeed.
	code, out = runMgclSync(t, bin, dir, baseEnv(),
		"run", "--host", "127.0.0.1:0", "--db", dbPath,
		"--encryption-key-file", keyPath,
		"--backup-to", "sqlite:"+sqliteTarget,
		"--", "sh", "-c", "exit 7")
	if code != 7 {
		t.Fatalf("exit code = %d, want 7\n%s", code, out)
	}
	if strings.Contains(out, "backup failed") {
		t.Fatalf("unexpected backup failure:\n%s", out)
	}

	// A path copy failure with a successful child becomes exit 1.
	failDir := filepath.Join(dir, "path-failure")
	if err := os.Mkdir(failDir, 0o700); err != nil {
		t.Fatal(err)
	}
	pathTarget := filepath.Join(failDir, "mgcl.db")
	code, out = runMgclSync(t, bin, dir, baseEnv(),
		"run", "--host", "127.0.0.1:0", "--db", dbPath,
		"--encryption-key-file", keyPath,
		"--backup-to", "path:"+pathTarget,
		"--", "sh", "-c", "rmdir "+shellQuote(failDir)+" && exit 0")
	if code != 1 {
		t.Fatalf("path failure exit code = %d, want 1\n%s", code, out)
	}
	if !strings.Contains(out, "backup failed") {
		t.Fatalf("missing backup failure report:\n%s", out)
	}

	// A child's non-zero code is preserved even when shutdown backup fails.
	failDir2 := filepath.Join(dir, "path-failure-2")
	if err := os.Mkdir(failDir2, 0o700); err != nil {
		t.Fatal(err)
	}
	pathTarget2 := filepath.Join(failDir2, "mgcl.db")
	code, out = runMgclSync(t, bin, dir, baseEnv(),
		"run", "--host", "127.0.0.1:0", "--db", dbPath,
		"--encryption-key-file", keyPath,
		"--backup-to", "path:"+pathTarget2,
		"--", "sh", "-c", "rmdir "+shellQuote(failDir2)+" && exit 7")
	if code != 7 {
		t.Fatalf("exit code = %d, want 7\n%s", code, out)
	}
	if !strings.Contains(out, "backup failed") {
		t.Fatalf("missing backup failure report:\n%s", out)
	}
}

func TestCLIBackupInvalidDestinationFailsBeforeStartup(t *testing.T) {
	requireCLI(t)
	bin := buildMgcl(t)
	dir := t.TempDir()
	keyPath := writeTestIdentity(t)
	dbPath := filepath.Join(dir, "mgcl.db")

	code, out := runMgclSync(t, bin, dir, baseEnv(),
		"run", "--host", "127.0.0.1:0", "--db", dbPath,
		"--encryption-key-file", keyPath,
		"--backup-to", "http:/tmp/backup.db",
		"--", "sh", "-c", "true")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1\n%s", code, out)
	}
	if strings.Contains(out, "MgCl server listening on") {
		t.Fatalf("server started with invalid backup destination:\n%s", out)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("database was created before backup validation: %v", err)
	}
	if !strings.Contains(out, "unsupported scheme") {
		t.Fatalf("unexpected error:\n%s", out)
	}
}

func TestCLIBackupEnvironmentVariableIgnored(t *testing.T) {
	requireCLI(t)
	bin := buildMgcl(t)
	dir := t.TempDir()
	keyPath := writeTestIdentity(t)
	dbPath := filepath.Join(dir, "mgcl.db")
	target := filepath.Join(dir, "env-backup.db")

	code, out := runMgclSync(t, bin, dir,
		baseEnv("MGCL_BACKUP_TO=sqlite:"+target),
		"run", "--host", "127.0.0.1:0", "--db", dbPath,
		"--encryption-key-file", keyPath,
		"--", "sh", "-c", "true")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("MGCL_BACKUP_TO was honored: %v", err)
	}
}

func TestCLIBackupYAMLRelativeToGitRoot(t *testing.T) {
	requireCLI(t)
	bin := buildMgcl(t)
	root := t.TempDir()
	initTestGitRepo(t, root)
	subdir := filepath.Join(root, "nested", "workdir")
	if err := os.MkdirAll(subdir, 0o700); err != nil {
		t.Fatal(err)
	}
	keyPath := writeTestIdentity(t)
	dbPath := filepath.Join(root, "project.db")
	config := fmt.Sprintf(`host: "127.0.0.1:0"
db: project.db
encryption_key_file: %q
backup_to:
  - sqlite:backups/from-yaml.db
  - path:backups/from-yaml-copy.db
`, keyPath)
	if err := os.WriteFile(filepath.Join(root, ".mgcl.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "backups"), 0o700); err != nil {
		t.Fatal(err)
	}

	code, out := runMgclSync(t, bin, subdir, baseEnv(),
		"run", "--", "sh", "-c", "true")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(root, "backups", "from-yaml.db")); err != nil {
		t.Fatalf("YAML sqlite target was not relative to Git root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "backups", "from-yaml-copy.db")); err != nil {
		t.Fatalf("YAML path target was not relative to Git root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(subdir, "backups")); !os.IsNotExist(err) {
		t.Fatalf("YAML target was resolved relative to cwd: %v", err)
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("database was not created relative to Git root: %v", err)
	}
}
