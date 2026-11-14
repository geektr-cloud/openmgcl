package sqlitebackup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTestDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	dsn := sqliteDSN(path, url.Values{
		"_pragma": {"busy_timeout(5000)", "journal_mode(WAL)"},
		"_txlock": {"immediate"},
	})
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func createSource(t *testing.T, path string) *sql.DB {
	t.Helper()
	db := openTestDB(t, path)
	if _, err := db.Exec(`CREATE TABLE items (id INTEGER PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO items (value) VALUES ('initial')`); err != nil {
		t.Fatal(err)
	}
	return db
}

func insertItem(t *testing.T, db *sql.DB, value string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO items (value) VALUES (?)`, value); err != nil {
		t.Fatal(err)
	}
}

func tryQuerySingle(path, query string) (string, error) {
	dsn := sqliteDSN(path, url.Values{
		"mode":    {"ro"},
		"_pragma": {"busy_timeout(1000)"},
	})
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return "", err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	var value string
	if err := db.QueryRow(query).Scan(&value); err != nil {
		return "", err
	}
	return value, nil
}

func waitForValue(t *testing.T, path, query, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		value, err := tryQuerySingle(path, query)
		if err == nil {
			last = value
			if value == want {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("value at %s = %q, want %q", path, last, want)
}

func waitForFatal(t *testing.T, m *Manager) {
	t.Helper()
	select {
	case <-m.Fatal():
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for backup fatal")
	}
}

func mustManager(t *testing.T, source string, specs []string) *Manager {
	t.Helper()
	dests, err := ParseDestinations(specs, "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(source, dests)
	if err != nil {
		t.Fatal(err)
	}
	if m == nil {
		t.Fatal("expected manager")
	}
	return m
}

func TestParseDestinationsRelativeAndAbsolute(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	abs := filepath.Join(dir, "abs.db")
	dests, err := ParseDestinations([]string{
		"sqlite:relative.db",
		"path:./nested/../other.db",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(dests) != 2 {
		t.Fatalf("got %d destinations", len(dests))
	}
	if dests[0].Path != filepath.Join(dir, "relative.db") {
		t.Errorf("flag relative path = %q", dests[0].Path)
	}
	if !strings.HasPrefix(dests[1].Path, dir) {
		t.Errorf("normalized path = %q, want under %q", dests[1].Path, dir)
	}

	configBase := t.TempDir()
	dests, err = ParseDestinations([]string{"sqlite:state.db"}, configBase)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(configBase, "state.db"); dests[0].Path != want {
		t.Errorf("config relative path = %q, want %q", dests[0].Path, want)
	}

	dests, err = ParseDestinations([]string{"sqlite:" + abs}, configBase)
	if err != nil {
		t.Fatal(err)
	}
	if dests[0].Path != abs {
		t.Errorf("absolute path = %q, want %q", dests[0].Path, abs)
	}

	colon := filepath.Join(dir, "with:colon.db")
	dests, err = ParseDestinations([]string{"sqlite:" + colon}, "")
	if err != nil {
		t.Fatalf("path with colon was rejected: %v", err)
	}
	if dests[0].Path != colon {
		t.Errorf("colon path = %q, want %q", dests[0].Path, colon)
	}
}

func TestParseDestinationsErrors(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	cases := [][]string{
		{"missing-scheme.db"},
		{":no-scheme"},
		{"sqlite:"},
		{"http:/tmp/db"},
		{"SQLITE:/tmp/db"},
		{"sqlite:/tmp/with\x00nul"},
	}
	for _, specs := range cases {
		if _, err := ParseDestinations(specs, ""); err == nil {
			t.Errorf("%v did not fail", specs)
		}
	}

	dup := [][]string{
		{"sqlite:a.db", "path:a.db"},
		{"sqlite:sub/../a.db", "path:a.db"},
		{"sqlite:a.db", "path:a.db-wal"},
	}
	for _, specs := range dup {
		if _, err := ParseDestinations(specs, ""); err == nil {
			t.Errorf("duplicate %v did not fail", specs)
		}
	}
}

func TestNewRejectsConflictsAndBadTargets(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "mgcl.db")
	db := createSource(t, source)
	defer db.Close()

	// Source sidecars are part of the source identity even before they exist.
	for _, spec := range []string{
		"sqlite:" + source,
		"path:" + source + "-wal",
		"sqlite:" + source + "-shm",
	} {
		dests, err := ParseDestinations([]string{spec}, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := New(source, dests); err == nil {
			t.Errorf("%s was accepted", spec)
		}
	}

	dirTarget := filepath.Join(dir, "target-dir")
	if err := os.Mkdir(dirTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := New(source, mustDests(t, "path:"+dirTarget)); err == nil {
		t.Error("directory target was accepted")
	}

	missingParent := filepath.Join(dir, "missing", "nested", "backup.db")
	if _, err := ParseDestinations([]string{"sqlite:" + missingParent}, ""); err == nil {
		t.Error("target with a missing parent directory was accepted")
	}
	fileAsParent := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(fileAsParent, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseDestinations([]string{"sqlite:" + filepath.Join(fileAsParent, "backup.db")}, ""); err == nil {
		t.Error("target under a non-directory parent was accepted")
	}

	corruptSQLite := filepath.Join(dir, "corrupt.db")
	corrupt := append([]byte(sqliteHeader), make([]byte, 128)...)
	if err := os.WriteFile(corruptSQLite, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(source, mustDests(t, "sqlite:"+corruptSQLite)); err == nil {
		t.Error("corrupt SQLite target was accepted")
	}

	symlinkTarget := filepath.Join(dir, "symlink.db")
	if err := os.Symlink(filepath.Join(dir, "does-not-matter.db"), symlinkTarget); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseDestinations([]string{"sqlite:" + symlinkTarget}, ""); err == nil {
		t.Error("symlink target was accepted")
	}

	nonSQLite := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(nonSQLite, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(source, mustDests(t, "sqlite:"+nonSQLite)); err == nil {
		t.Error("non-empty non-SQLite target was accepted")
	}

	walTarget := filepath.Join(dir, "wal.db")
	if err := os.WriteFile(walTarget, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(walTarget+"-wal", []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(source, mustDests(t, "sqlite:"+walTarget)); err == nil {
		t.Error("target with non-empty WAL was accepted")
	}

	original := filepath.Join(dir, "original.db")
	if err := os.WriteFile(original, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	hardlink := filepath.Join(dir, "hardlink.db")
	if err := os.Link(original, hardlink); err != nil {
		t.Fatal(err)
	}
	if _, err := New(source, mustDests(t, "path:"+hardlink)); err == nil {
		t.Error("hard-linked target was accepted")
	}

	emptyTarget := filepath.Join(dir, "empty.db")
	if err := os.WriteFile(emptyTarget, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := New(source, mustDests(t, "path:"+emptyTarget))
	if err != nil {
		t.Fatalf("empty target was rejected: %v", err)
	}
	if m == nil {
		t.Fatal("expected manager for empty target")
	}
}

func mustDests(t *testing.T, specs ...string) []Destination {
	t.Helper()
	dests, err := ParseDestinations(specs, "")
	if err != nil {
		t.Fatal(err)
	}
	return dests
}

func TestPathProbeDoesNotTouchTarget(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "mgcl.db")
	db := createSource(t, source)
	defer db.Close()

	target := filepath.Join(dir, "backup.db")
	m := mustManager(t, source, []string{"path:" + target})
	if m == nil {
		t.Fatal("nil manager")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("probe touched target: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), "mgcl-probe") {
			t.Fatalf("probe file left behind: %s", entry.Name())
		}
	}
}

func TestPathProbeFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; directory permissions cannot block probe")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "mgcl.db")
	db := createSource(t, source)
	defer db.Close()

	readonly := filepath.Join(dir, "readonly")
	if err := os.Mkdir(readonly, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(readonly, 0o700) })

	if _, err := New(source, mustDests(t, "path:"+filepath.Join(readonly, "backup.db"))); err == nil {
		t.Fatal("probe succeeded in a read-only directory")
	}
}

func TestSQLiteInitialBackgroundAndDrain(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "mgcl.db")
	db := createSource(t, source)

	target := filepath.Join(dir, "backup.db")
	m := mustManager(t, source, []string{"sqlite:" + target})
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}

	waitForValue(t, target, `SELECT value FROM items WHERE id = 1`, "initial")

	insertItem(t, db, "second")
	m.MarkDirty()
	waitForValue(t, target, `SELECT value FROM items ORDER BY id DESC LIMIT 1`, "second")

	insertItem(t, db, "final")
	m.MarkDirty()
	if err := m.Shutdown(db); err != nil {
		t.Fatal(err)
	}

	waitForValue(t, target, `SELECT value FROM items ORDER BY id DESC LIMIT 1`, "final")
	waitForValue(t, target, `SELECT COUNT(*) FROM items`, "3")

	fi, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("target mode = %o, want 600", fi.Mode().Perm())
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), "mgcl-tmp") || strings.Contains(entry.Name(), "mgcl-probe") {
			t.Fatalf("temporary file left behind: %s", entry.Name())
		}
	}
}

func TestSQLiteTargetRecreatedAfterDelete(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "mgcl.db")
	db := createSource(t, source)

	target := filepath.Join(dir, "backup.db")
	m := mustManager(t, source, []string{"sqlite:" + target})
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}

	insertItem(t, db, "recreated")
	m.MarkDirty()
	waitForValue(t, target, `SELECT value FROM items ORDER BY id DESC LIMIT 1`, "recreated")
	if err := m.Shutdown(db); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteFatalContinuesOtherTargets(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "mgcl.db")
	db := createSource(t, source)

	broken := filepath.Join(dir, "broken.db")
	healthy := filepath.Join(dir, "healthy.db")
	m := mustManager(t, source, []string{"sqlite:" + broken, "sqlite:" + healthy})
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(broken); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(broken, 0o700); err != nil {
		t.Fatal(err)
	}

	insertItem(t, db, "synced-after-fatal")
	m.MarkDirty()
	waitForFatal(t, m)
	waitForValue(t, healthy, `SELECT value FROM items ORDER BY id DESC LIMIT 1`, "synced-after-fatal")

	if err := m.Shutdown(db); err == nil {
		t.Fatal("shutdown after fatal did not fail")
	}
}

func TestSQLiteFatalStopsFurtherSync(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "mgcl.db")
	db := createSource(t, source)

	target := filepath.Join(dir, "backup.db")
	m := mustManager(t, source, []string{"sqlite:" + target})
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}

	insertItem(t, db, "unbackupable")
	m.MarkDirty()
	waitForFatal(t, m)
	if m.Err() == nil {
		t.Fatal("fatal error was not recorded")
	}

	// Shutdown still reports the first fatal error and closes the source.
	if err := m.Shutdown(db); err == nil {
		t.Fatal("shutdown after fatal did not fail")
	}
	waitForValue(t, source, `SELECT COUNT(*) FROM items`, "2")
}

func TestPathCopyOnlyOnShutdown(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "mgcl.db")
	db := createSource(t, source)

	target := filepath.Join(dir, "backup.db")
	m := mustManager(t, source, []string{"path:" + target})
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("path target was written during startup: %v", err)
	}

	insertItem(t, db, "shutdown-only")
	if err := m.Shutdown(db); err != nil {
		t.Fatal(err)
	}

	waitForValue(t, target, `SELECT COUNT(*) FROM items`, "2")
	waitForValue(t, target, `SELECT value FROM items ORDER BY id DESC LIMIT 1`, "shutdown-only")

	if _, err := os.Stat(source); err != nil {
		t.Fatalf("source disappeared: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), "mgcl-tmp") || strings.Contains(entry.Name(), "mgcl-probe") {
			t.Fatalf("temporary file left behind: %s", entry.Name())
		}
	}
}

func TestPathBackupPermissionsFollowUmask(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "mgcl.db")
	db := createSource(t, source)

	// A 0666 reference in the target's directory shows the mode this process
	// gets from the umask and any default ACL. The path temp file and final
	// target must follow the same rules instead of being forced to 0600.
	reference := filepath.Join(dir, "reference")
	refFile, err := os.OpenFile(reference, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o666)
	if err != nil {
		t.Fatal(err)
	}
	if err := refFile.Close(); err != nil {
		t.Fatal(err)
	}
	refInfo, err := os.Stat(reference)
	if err != nil {
		t.Fatal(err)
	}
	want := refInfo.Mode().Perm()

	target := filepath.Join(dir, "backup.db")
	m := mustManager(t, source, []string{"path:" + target})
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	if err := m.Shutdown(db); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != want {
		t.Fatalf("path target mode = %04o, want %04o from a 0666 reference (umask/default ACL decide, not a fixed 0600)", got, want)
	}
}

func TestAbortDoesNotCopyPath(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "mgcl.db")
	db := createSource(t, source)

	target := filepath.Join(dir, "backup.db")
	m := mustManager(t, source, []string{"path:" + target})
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	insertItem(t, db, "aborted")
	if err := m.Abort(db); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("abort copied the path target: %v", err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("source disappeared on abort: %v", err)
	}
}

func TestPathCopyOverExistingSQLiteTarget(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "mgcl.db")
	db := createSource(t, source)

	target := filepath.Join(dir, "backup.db")
	old := openTestDB(t, target)
	if _, err := old.Exec(`CREATE TABLE old (id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	m := mustManager(t, source, []string{"path:" + target})
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	if err := m.Shutdown(db); err != nil {
		t.Fatal(err)
	}

	waitForValue(t, target, `SELECT value FROM items WHERE id = 1`, "initial")
	waitForValue(t, target, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'old'`, "0")
}

func TestPathBusyCheckpointSkipsCopy(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "mgcl.db")
	db := createSource(t, source)

	target := filepath.Join(dir, "backup.db")
	m := mustManager(t, source, []string{"path:" + target})
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}

	reader := openTestDB(t, source)
	conn, err := reader.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(t.Context(), "BEGIN"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := conn.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM items`).Scan(&count); err != nil {
		t.Fatal(err)
	}

	err = m.Shutdown(db)
	if err == nil {
		t.Fatal("checkpoint with an active reader did not fail")
	}
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Fatalf("target was copied despite busy checkpoint: %v", statErr)
	}

	_, _ = conn.ExecContext(t.Context(), "ROLLBACK")
	_ = conn.Close()
}

func TestPathRenameFailureKeepsOldTargetAndCleansTemp(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "mgcl.db")
	db := createSource(t, source)

	target := filepath.Join(dir, "backup.db")
	old := openTestDB(t, target)
	if _, err := old.Exec(`CREATE TABLE old (id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	old.Close()
	before, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}

	m := mustManager(t, source, []string{"path:" + target})
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}

	originalRename := renameFile
	renameFile = func(string, string) error { return errors.New("injected rename failure") }
	t.Cleanup(func() { renameFile = originalRename })

	if err := m.Shutdown(db); err == nil {
		t.Fatal("shutdown succeeded despite rename failure")
	}

	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("old target was modified by a failed rename")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), "mgcl-tmp") {
			t.Fatalf("temporary file left after rename failure: %s", entry.Name())
		}
	}
}

func TestSQLiteTargetExistingWALDatabase(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "mgcl.db")
	db := createSource(t, source)

	target := filepath.Join(dir, "backup.db")
	old := openTestDB(t, target)
	if _, err := old.Exec(`CREATE TABLE old (id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	m := mustManager(t, source, []string{"sqlite:" + target})
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	waitForValue(t, target, `SELECT value FROM items WHERE id = 1`, "initial")
	if nonEmptyFile(target+"-wal") || nonEmptyFile(target+"-journal") {
		t.Fatalf("target depends on sidecars after initial backup")
	}
	if err := m.Shutdown(db); err != nil {
		t.Fatal(err)
	}
}

func TestPathDirSyncFailureReportsErrorAfterRename(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "mgcl.db")
	db := createSource(t, source)

	target := filepath.Join(dir, "backup.db")
	m := mustManager(t, source, []string{"path:" + target})
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}

	originalSync := syncDir
	syncDir = func(string) error { return errors.New("injected directory fsync failure") }
	t.Cleanup(func() { syncDir = originalSync })

	if err := m.Shutdown(db); err == nil {
		t.Fatal("shutdown succeeded despite directory fsync failure")
	}
	// The rename already happened, so the target has the new content even
	// though the command reports failure.
	waitForValue(t, target, `SELECT value FROM items WHERE id = 1`, "initial")
}

func TestMultipleDestinations(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "mgcl.db")
	db := createSource(t, source)

	sqliteTarget := filepath.Join(dir, "sqlite-backup.db")
	pathTarget := filepath.Join(dir, "path-backup.db")
	m := mustManager(t, source, []string{
		"sqlite:" + sqliteTarget,
		"path:" + pathTarget,
	})
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}

	insertItem(t, db, "multi")
	m.MarkDirty()
	waitForValue(t, sqliteTarget, `SELECT value FROM items ORDER BY id DESC LIMIT 1`, "multi")

	if err := m.Shutdown(db); err != nil {
		t.Fatal(err)
	}
	waitForValue(t, pathTarget, `SELECT value FROM items ORDER BY id DESC LIMIT 1`, "multi")
	waitForValue(t, sqliteTarget, `SELECT value FROM items ORDER BY id DESC LIMIT 1`, "multi")
}

func TestSpecialCharacterPaths(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "with space ? # %25")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "mgcl ?#%.db")
	db := createSource(t, source)

	sqliteTarget := filepath.Join(dir, "sqlite ?#%.db")
	pathTarget := filepath.Join(dir, "path ?#%.db")
	m := mustManager(t, source, []string{
		"sqlite:" + sqliteTarget,
		"path:" + pathTarget,
	})
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	insertItem(t, db, "special")
	m.MarkDirty()
	waitForValue(t, sqliteTarget, `SELECT value FROM items ORDER BY id DESC LIMIT 1`, "special")

	if err := m.Shutdown(db); err != nil {
		t.Fatal(err)
	}
	waitForValue(t, pathTarget, `SELECT value FROM items ORDER BY id DESC LIMIT 1`, "special")
}

func TestNoDestinationsIsDisabled(t *testing.T) {
	m, err := New("unused.db", nil)
	if err != nil {
		t.Fatal(err)
	}
	if m != nil {
		t.Fatal("empty destination list created a manager")
	}
}

func TestManagerRejectsLateDirty(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "mgcl.db")
	db := createSource(t, source)

	target := filepath.Join(dir, "backup.db")
	m := mustManager(t, source, []string{"path:" + target})
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}

	m.BeginShutdown()
	if err := m.MarkDirty(); err == nil {
		t.Fatal("MarkDirty succeeded after BeginShutdown")
	}
	if err := m.Shutdown(db); err == nil {
		t.Fatal("Shutdown succeeded despite a late dirty mark")
	} else if !strings.Contains(err.Error(), "dirty after shutdown") {
		t.Fatalf("unexpected shutdown error: %v", err)
	}
}

func TestManagerStartTwiceFails(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "mgcl.db")
	db := createSource(t, source)

	target := filepath.Join(dir, "backup.db")
	m := mustManager(t, source, []string{"sqlite:" + target})
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(); err == nil {
		t.Fatal("second Start succeeded")
	} else if !strings.Contains(err.Error(), "already started") {
		t.Fatalf("unexpected second Start error: %v", err)
	}
	if err := m.Shutdown(db); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteTargetExistingPermissionsPreserved(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "mgcl.db")
	db := createSource(t, source)

	target := filepath.Join(dir, "backup.db")
	old := openTestDB(t, target)
	if _, err := old.Exec(`CREATE TABLE old (id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o640); err != nil {
		t.Fatal(err)
	}

	assertMode := func(stage string) {
		t.Helper()
		fi, err := os.Stat(target)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o640 {
			t.Errorf("%s: target mode = %o, want 640", stage, fi.Mode().Perm())
		}
	}

	m := mustManager(t, source, []string{"sqlite:" + target})
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	waitForValue(t, target, `SELECT value FROM items WHERE id = 1`, "initial")
	assertMode("initial backup")

	insertItem(t, db, "synced")
	m.MarkDirty()
	waitForValue(t, target, `SELECT value FROM items ORDER BY id DESC LIMIT 1`, "synced")
	assertMode("background sync")

	if err := m.Shutdown(db); err != nil {
		t.Fatal(err)
	}
	assertMode("shutdown")
}

func TestPrecreateSQLiteTargetPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new.db")
	if err := precreateSQLiteTarget(path); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("precreated mode = %o, want 600", fi.Mode().Perm())
	}
	if fi.Size() != 0 {
		t.Fatalf("precreated size = %d, want 0", fi.Size())
	}

	if err := os.WriteFile(path, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := precreateSQLiteTarget(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "existing" {
		t.Fatalf("existing target was modified: %q", data)
	}
}

func TestCheckpointContextCanceled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.db")
	db := openTestDB(t, path)
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := checkpointDBContext(ctx, db); err == nil {
		t.Fatal("canceled checkpoint context did not fail")
	}
}

func TestCheckSQLiteHeaderDistinguishesErrors(t *testing.T) {
	dir := t.TempDir()

	missing := filepath.Join(dir, "missing.db")
	if err := checkSQLiteHeader(missing); err == nil || !strings.Contains(err.Error(), "open target header") {
		t.Fatalf("missing file error = %v", err)
	}

	short := filepath.Join(dir, "short.db")
	if err := os.WriteFile(short, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkSQLiteHeader(short); err == nil || !strings.Contains(err.Error(), "read target header") {
		t.Fatalf("short file error = %v", err)
	}

	notSQLite := filepath.Join(dir, "not-sqlite.db")
	if err := os.WriteFile(notSQLite, []byte("this is definitely not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkSQLiteHeader(notSQLite); err == nil || !strings.Contains(err.Error(), "not a SQLite database") {
		t.Fatalf("non-SQLite error = %v", err)
	}
}

func TestNewRejectsSameFileDestinations(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "mgcl.db")
	db := createSource(t, source)
	defer db.Close()

	first := filepath.Join(dir, "first.db")
	if err := os.WriteFile(first, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(dir, "second.db")
	if err := os.Link(first, second); err != nil {
		t.Fatal(err)
	}

	dests := []Destination{
		{Scheme: SchemeSQLite, Path: first},
		{Scheme: SchemePath, Path: second},
	}
	if !destinationsSameFile(dests[0], dests[1]) {
		t.Fatal("hard-linked destinations were not detected as the same file")
	}
	if _, err := New(source, dests); err == nil {
		t.Fatal("New accepted hard-linked destinations")
	}
}

func TestConcurrentDirtyMarksAndShutdown(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "mgcl.db")
	db := createSource(t, source)

	target := filepath.Join(dir, "backup.db")
	m := mustManager(t, source, []string{"sqlite:" + target})
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			if _, err := db.Exec(`INSERT INTO items (value) VALUES (?)`, fmt.Sprintf("row-%d", i)); err != nil {
				t.Errorf("insert: %v", err)
				return
			}
			m.MarkDirty()
		}
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("writer timed out")
	}

	if err := m.Shutdown(db); err != nil {
		t.Fatal(err)
	}
	waitForValue(t, target, `SELECT COUNT(*) FROM items`, "51")
}
