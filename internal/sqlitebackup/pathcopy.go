package sqlitebackup

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Test hooks keep the failure paths observable without making production code
// depend on a mock framework.
var (
	renameFile = os.Rename
	syncDir    = syncDirectory

	// BeforePathCopy is a deterministic failure-injection hook for tests. It
	// must remain nil in production; a non-nil error aborts that path target.
	BeforePathCopy func() error
)

func syncDirectory(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// copyPathToDestination copies the closed, checkpointed main database to a
// path target via a unique temp file in the same directory and an atomic
// rename. The temp file is created with mode 0666 so the process umask and the
// directory's default ACL decide its permissions; the application never
// changes them. The destination format is never opened by SQLite.
func (m *Manager) copyPathToDestination(d Destination) error {
	if BeforePathCopy != nil {
		if err := BeforePathCopy(); err != nil {
			return fmt.Errorf("path copy failure injection: %w", err)
		}
	}
	if err := validateTargetFile(d.Path, newSourceInfo(m.sourcePath), false); err != nil {
		return fmt.Errorf("recheck target: %w", err)
	}

	src, err := os.Open(m.sourcePath)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer src.Close()

	fi, err := src.Stat()
	if err != nil {
		return fmt.Errorf("stat source: %w", err)
	}

	dir := filepath.Dir(d.Path)
	tmp, err := createTempFile(dir, "."+filepath.Base(d.Path)+".mgcl-tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(tmpName) }

	n, err := io.Copy(tmp, src)
	if err != nil {
		cleanup()
		return fmt.Errorf("copy source: %w", err)
	}
	if n != fi.Size() {
		cleanup()
		return fmt.Errorf("short copy: copied %d of %d bytes", n, fi.Size())
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("fsync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close temp file: %w", err)
	}

	if err := renameFile(tmpName, d.Path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("rename temp file: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("fsync target directory: %w", err)
	}
	return nil
}

// verifySourceSelfContained checks the closed main database has no durable
// sidecar state that would have to be copied along with it.
func verifySourceSelfContained(path string) error {
	if nonEmptyFile(path + "-wal") {
		return errors.New("source database still has a non-empty WAL")
	}
	if fileExists(path + "-journal") {
		return errors.New("source database still has a rollback journal")
	}
	return nil
}
