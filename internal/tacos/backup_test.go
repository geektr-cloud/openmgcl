package tacos

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/geektr-cloud/openmgcl/internal/sqlitebackup"
)

func openSQLiteBackupTarget(t *testing.T, path string) *sql.DB {
	t.Helper()
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&_pragma=busy_timeout(1000)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func waitBackupCount(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		db := openSQLiteBackupTarget(t, path)
		err := db.QueryRow(`SELECT COUNT(*) FROM workspaces`).Scan(&got)
		db.Close()
		if err == nil && got == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("workspace count at %s = %q, want %q", path, got, want)
}

func TestStoreSQLiteBackupFailClosed(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mgcl.db")
	target := filepath.Join(dir, "backup.db")

	dests, err := sqlitebackup.ParseDestinations([]string{"sqlite:" + target}, "")
	if err != nil {
		t.Fatal(err)
	}
	manager, err := sqlitebackup.New(dbPath, dests)
	if err != nil {
		t.Fatal(err)
	}

	store, err := OpenWithBackup(dbPath, testIdentity(t), manager)
	if err != nil {
		t.Fatal(err)
	}
	createTestWorkspace(t, store, "backed-up")
	waitBackupCount(t, target, "1")

	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}

	// This commit succeeds in the source and marks dirty; the background sync
	// then fails against the directory.
	if _, err := store.CreateWorkspace("test-org", "after-target-break", ""); err != nil {
		t.Fatalf("write before fatal was rejected: %v", err)
	}

	select {
	case <-manager.Fatal():
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for backup fatal")
	}

	if _, err := store.CreateWorkspace("test-org", "must-be-rejected", ""); err == nil {
		t.Fatal("write after fatal backup was accepted")
	}

	if err := store.Close(); err == nil {
		t.Fatal("store close did not report the backup failure")
	}

	// The committed source transaction cannot be rolled back.
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_pragma=busy_timeout(1000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workspaces`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("source workspace count = %d, want 2", count)
	}
}

func countSQLiteRows(t *testing.T, path string) int {
	t.Helper()
	db := openSQLiteBackupTarget(t, path)
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workspaces`).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", path, err)
	}
	return count
}

func TestStoreStopWritesWaitsForInFlightGate(t *testing.T) {
	store, _ := openTestStore(t, testIdentity(t))

	release, err := store.beginWrite()
	if err != nil {
		t.Fatal(err)
	}

	stopped := make(chan struct{})
	go func() {
		store.StopWrites()
		close(stopped)
	}()

	select {
	case <-stopped:
		t.Fatal("StopWrites returned while a write gate was still held")
	case <-time.After(100 * time.Millisecond):
	}

	release()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("StopWrites did not return after the in-flight write completed")
	}

	if release, err := store.beginWrite(); err == nil {
		release()
		t.Fatal("beginWrite succeeded after StopWrites")
	}
}

func TestStoreRejectsWritesAfterStopWrites(t *testing.T) {
	store, _ := openTestStore(t, testIdentity(t))
	ws := createTestWorkspace(t, store, "before-stop")

	store.StopWrites()

	if _, err := store.CreateWorkspace("test-org", "after-stop", ""); err == nil {
		t.Fatal("CreateWorkspace succeeded after StopWrites")
	}
	if err := store.LockWorkspace(ws.ID, "after-stop"); err == nil {
		t.Fatal("LockWorkspace succeeded after StopWrites")
	}
}

func TestStoreShutdownDrainsConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mgcl.db")
	sqliteTarget := filepath.Join(dir, "sqlite-backup.db")
	pathTarget := filepath.Join(dir, "path-backup.db")

	dests, err := sqlitebackup.ParseDestinations([]string{
		"sqlite:" + sqliteTarget,
		"path:" + pathTarget,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	manager, err := sqlitebackup.New(dbPath, dests)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenWithBackup(dbPath, testIdentity(t), manager)
	if err != nil {
		t.Fatal(err)
	}

	var successes atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				_, err := store.CreateWorkspace("load-org", fmt.Sprintf("ws-%d-%d", worker, i), "")
				if err == nil {
					successes.Add(1)
					continue
				}
				if strings.Contains(err.Error(), "shutting down") {
					return
				}
				t.Errorf("write failed unexpectedly: %v", err)
				return
			}
		}(worker)
	}

	deadline := time.Now().Add(10 * time.Second)
	for successes.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if successes.Load() == 0 {
		t.Fatal("writer did not complete any write before shutdown")
	}

	store.StopWrites()
	close(stop)
	wg.Wait()

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	want := int(successes.Load())
	if got := countSQLiteRows(t, dbPath); got != want {
		t.Fatalf("source rows = %d, want %d", got, want)
	}
	if got := countSQLiteRows(t, sqliteTarget); got != want {
		t.Fatalf("sqlite backup rows = %d, want %d", got, want)
	}
	if got := countSQLiteRows(t, pathTarget); got != want {
		t.Fatalf("path backup rows = %d, want %d", got, want)
	}
}

func TestStorePathBackupOnCloseAfterCheckpoint(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mgcl.db")
	target := filepath.Join(dir, "backup.db")

	dests, err := sqlitebackup.ParseDestinations([]string{"path:" + target}, "")
	if err != nil {
		t.Fatal(err)
	}
	manager, err := sqlitebackup.New(dbPath, dests)
	if err != nil {
		t.Fatal(err)
	}

	store, err := OpenWithBackup(dbPath, testIdentity(t), manager)
	if err != nil {
		t.Fatal(err)
	}
	createTestWorkspace(t, store, "path-backed")

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	waitBackupCount(t, target, "1")
}
