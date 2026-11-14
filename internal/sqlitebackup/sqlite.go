package sqlitebackup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"time"

	_ "modernc.org/sqlite"
)

const (
	sqliteHeader      = "SQLite format 3\x00"
	checkpointTimeout = 30 * time.Second
)

func sqliteDSN(path string, query url.Values) string {
	u := url.URL{Scheme: "file", Path: path, RawQuery: query.Encode()}
	return u.String()
}

func checkSQLiteHeader(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open target header: %w", err)
	}
	defer f.Close()

	buf := make([]byte, len(sqliteHeader))
	if _, err := io.ReadFull(f, buf); err != nil {
		return fmt.Errorf("read target header: %w", err)
	}
	if string(buf) != sqliteHeader {
		return errors.New("target is not a SQLite database")
	}
	return nil
}

// quickCheckSQLite opens path read-only and immutable so validation does not
// create sidecars or depend on SQLite locking support.
func quickCheckSQLite(path string) error {
	dsn := sqliteDSN(path, url.Values{
		"mode":      {"ro"},
		"immutable": {"1"},
	})
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	var result string
	if err := db.QueryRow("PRAGMA quick_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("quick_check returned %q", result)
	}
	return nil
}

func nonEmptyFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Size() > 0
}

// precreateSQLiteTarget reserves a new target with mode 0600 before the
// Online Backup API opens it. Without this, a failure between creation and the
// final chmod can leave a 0644 file. Existing targets are left untouched.
func precreateSQLiteTarget(path string) error {
	if _, err := os.Lstat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect target: %w", err)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("precreate target: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close precreated target: %w", err)
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// settleSQLiteTarget confirms that a completed backup target has no
// non-empty WAL or rollback journal, tightens permissions to 0600, and
// optionally re-runs a read-only integrity check.
func settleSQLiteTarget(path string, verify bool) error {
	if nonEmptyFile(path + "-journal") {
		return errors.New("target has a non-empty rollback journal")
	}
	if nonEmptyFile(path + "-wal") {
		if err := checkpointSQLiteFile(path); err != nil {
			return fmt.Errorf("checkpoint target WAL: %w", err)
		}
	}
	if nonEmptyFile(path+"-wal") || nonEmptyFile(path+"-journal") {
		return errors.New("target still depends on a non-empty sidecar")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("tighten target permissions: %w", err)
	}
	if verify {
		if err := quickCheckSQLite(path); err != nil {
			return fmt.Errorf("verify target: %w", err)
		}
	}
	return nil
}

func checkpointSQLiteFile(path string) error {
	dsn := sqliteDSN(path, url.Values{
		"_pragma": {"busy_timeout(5000)"},
	})
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	busy, err := checkpointDB(db)
	if err != nil {
		return err
	}
	if busy != 0 {
		return errors.New("target database is busy")
	}
	return nil
}

// checkpointDB runs wal_checkpoint(TRUNCATE) with a bounded wait and returns
// the busy counter.
func checkpointDB(db *sql.DB) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), checkpointTimeout)
	defer cancel()
	return checkpointDBContext(ctx, db)
}

func checkpointDBContext(ctx context.Context, db *sql.DB) (int, error) {
	var busy, logFrames, checkpointed int
	if err := db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed); err != nil {
		return 0, err
	}
	return busy, nil
}
