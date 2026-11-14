package sqlitebackup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	sqlite "modernc.org/sqlite"
)

const (
	sourceOpenTimeout = 10 * time.Second
	sourceConnTimeout = 30 * time.Second
)

// Manager owns destination validation, the sqlite dirty-merge worker, and the
// ordered shutdown sequence for one write session.
type Manager struct {
	sourcePath  string
	sqliteDests []Destination
	pathDests   []Destination

	sourceDB *sql.DB

	mu       sync.Mutex
	dirty    bool
	firstErr error
	stopped  bool

	startCalled bool
	closing     bool
	lateDirty   bool

	fatalOnce sync.Once
	fatalCh   chan struct{}
	wake      chan struct{}
	stop      chan struct{}
	done      chan struct{}

	shutdownOnce sync.Once
	shutdownErr  error
}

// New validates all destinations, probes path destinations, and returns a
// manager. It does not open the source database or touch sqlite targets.
func New(sourcePath string, destinations []Destination) (*Manager, error) {
	if len(destinations) == 0 {
		return nil, nil
	}
	if sourcePath == "" {
		return nil, errors.New("source database path is required")
	}
	absSource, err := filepath.Abs(sourcePath)
	if err != nil {
		return nil, fmt.Errorf("resolve source database path: %w", err)
	}
	src := newSourceInfo(absSource)

	for _, d := range destinations {
		if err := validateDestination(d, src); err != nil {
			return nil, fmt.Errorf("%s: %w", d, err)
		}
	}
	for i := range destinations {
		for j := i + 1; j < len(destinations); j++ {
			if destinationsSameFile(destinations[i], destinations[j]) {
				return nil, fmt.Errorf("backup destinations refer to the same file: %s and %s", destinations[i], destinations[j])
			}
		}
	}
	for _, d := range destinations {
		if d.Scheme == SchemePath {
			if err := probePath(d); err != nil {
				return nil, fmt.Errorf("%s: %w", d, err)
			}
		}
	}

	m := &Manager{
		sourcePath: absSource,
		fatalCh:    make(chan struct{}),
		wake:       make(chan struct{}, 1),
		stop:       make(chan struct{}),
	}
	for _, d := range destinations {
		switch d.Scheme {
		case SchemeSQLite:
			m.sqliteDests = append(m.sqliteDests, d)
		case SchemePath:
			m.pathDests = append(m.pathDests, d)
		}
	}
	return m, nil
}

// Start opens a dedicated read-only source handle, performs the initial full
// backup for every sqlite destination, and starts the worker. It must be called
// after the source database schema is ready and before traffic is accepted.
func (m *Manager) Start() error {
	m.mu.Lock()
	if m.startCalled {
		m.mu.Unlock()
		return errors.New("backup manager already started")
	}
	m.startCalled = true
	m.mu.Unlock()

	if len(m.sqliteDests) == 0 {
		return nil
	}

	dsn := sqliteDSN(m.sourcePath, map[string][]string{
		"mode":    {"ro"},
		"_pragma": {"busy_timeout(5000)"},
	})
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("open backup source: %w", err)
	}
	db.SetMaxOpenConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), sourceOpenTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return fmt.Errorf("open backup source: %w", err)
	}
	m.sourceDB = db

	var errs []error
	for _, d := range m.sqliteDests {
		if err := m.backupSQLite(d, true); err != nil {
			errs = append(errs, fmt.Errorf("initial backup %s: %w", d, err))
		}
	}
	if len(errs) > 0 {
		m.sourceDB.Close()
		m.sourceDB = nil
		return errors.Join(errs...)
	}

	m.done = make(chan struct{})
	go m.worker()
	return nil
}

// MarkDirty records that a write transaction committed. It is idempotent and
// never blocks the write path. It returns an error after shutdown has begun so
// a late mark is not silently accepted; Shutdown also reports the late mark.
func (m *Manager) MarkDirty() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	if m.closing {
		m.lateDirty = true
		m.mu.Unlock()
		return errors.New("backup manager is shutting down")
	}
	m.dirty = true
	m.mu.Unlock()
	select {
	case m.wake <- struct{}{}:
	default:
	}
	return nil
}

// BeginShutdown closes the dirty-marking gate. Writes that commit after this
// point are recorded as late dirty marks and cause Shutdown to fail.
func (m *Manager) BeginShutdown() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.closing = true
	m.mu.Unlock()
}

// Fatal is closed when the first sqlite backup error is recorded.
func (m *Manager) Fatal() <-chan struct{} {
	if m == nil {
		return nil
	}
	return m.fatalCh
}

// Err returns the first fatal backup error, if any.
func (m *Manager) Err() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.firstErr
}

func (m *Manager) recordFatal(err error) {
	m.fatalOnce.Do(func() {
		m.mu.Lock()
		m.firstErr = err
		m.mu.Unlock()
		close(m.fatalCh)
	})
}

func (m *Manager) worker() {
	defer close(m.done)
	for {
		select {
		case <-m.wake:
		case <-m.stop:
			m.mu.Lock()
			dirty := m.dirty
			m.dirty = false
			m.mu.Unlock()
			// Drain even after a fatal error so other targets still get a final
			// attempt; the first fatal error is preserved.
			if dirty {
				m.syncSQLiteTargets()
			}
			return
		}

		m.mu.Lock()
		dirty := m.dirty
		m.dirty = false
		m.mu.Unlock()
		if !dirty {
			continue
		}
		m.syncSQLiteTargets()
	}
}

func (m *Manager) syncSQLiteTargets() {
	for _, d := range m.sqliteDests {
		if err := m.backupSQLite(d, false); err != nil {
			m.recordFatal(fmt.Errorf("sync %s: %w", d, err))
		}
	}
}

func (m *Manager) backupSQLite(d Destination, initial bool) error {
	if err := validateTargetFile(d.Path, newSourceInfo(m.sourcePath), false); err != nil {
		return fmt.Errorf("recheck target: %w", err)
	}
	if err := precreateSQLiteTarget(d.Path); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), sourceConnTimeout)
	defer cancel()
	conn, err := m.sourceDB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire source connection: %w", err)
	}
	defer conn.Close()

	dstURI := sqliteDSN(d.Path, map[string][]string{
		"mode":    {"rwc"},
		"_pragma": {"busy_timeout(5000)"},
	})

	return conn.Raw(func(driverConn any) error {
		creator, ok := driverConn.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return errors.New("sqlite driver does not support online backup")
		}

		backup, err := creator.NewBackup(dstURI)
		if err != nil {
			return fmt.Errorf("start online backup: %w", err)
		}

		var stepErr error
		for {
			more, err := backup.Step(-1)
			if err != nil {
				stepErr = fmt.Errorf("backup step: %w", err)
				break
			}
			if !more {
				break
			}
		}
		finishErr := backup.Finish()
		if stepErr != nil || finishErr != nil {
			return errors.Join(stepErr, finishErr)
		}

		return settleSQLiteTarget(d.Path, initial)
	})
}

// Abort closes the worker and source handle without running the path copy
// sequence. It is used when startup fails before the write session accepts
// traffic, so path targets keep their previous content.
func (m *Manager) Abort(mainDB *sql.DB) error {
	if m == nil {
		if mainDB == nil {
			return nil
		}
		return mainDB.Close()
	}
	m.shutdownOnce.Do(func() {
		m.BeginShutdown()
		var errs []error
		if err := m.stopWorker(); err != nil {
			errs = append(errs, fmt.Errorf("stop backup worker: %w", err))
		}
		if err := m.lateDirtyError(); err != nil {
			errs = append(errs, err)
		}
		if mainDB != nil {
			if err := mainDB.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close main database: %w", err))
			}
		}
		if err := m.Err(); err != nil {
			errs = append(errs, err)
		}
		if err := m.lateDirtyError(); err != nil {
			errs = append(errs, err)
		}
		m.shutdownErr = errors.Join(errs...)
	})
	return m.shutdownErr
}

// Shutdown drains the worker, optionally checkpoints and closes the main
// database, verifies it is self-contained, and copies every path destination.
// The main database is closed before Shutdown returns.
func (m *Manager) Shutdown(mainDB *sql.DB) error {
	if m == nil {
		if mainDB == nil {
			return nil
		}
		return mainDB.Close()
	}
	m.shutdownOnce.Do(func() {
		m.BeginShutdown()
		m.shutdownErr = m.shutdown(mainDB)
	})
	return m.shutdownErr
}

func (m *Manager) lateDirtyError() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lateDirty {
		m.lateDirty = false
		return errors.New("backup marked dirty after shutdown began")
	}
	return nil
}

func (m *Manager) shutdown(mainDB *sql.DB) error {
	var errs []error

	if err := m.stopWorker(); err != nil {
		errs = append(errs, fmt.Errorf("stop backup worker: %w", err))
	}
	if err := m.lateDirtyError(); err != nil {
		errs = append(errs, err)
	}

	if len(m.pathDests) > 0 {
		if mainDB == nil {
			errs = append(errs, errors.New("main database is not open"))
		} else {
			checkpointErr := checkpointMainDB(mainDB)
			closeErr := mainDB.Close()
			if checkpointErr != nil {
				errs = append(errs, checkpointErr)
			} else if closeErr != nil {
				errs = append(errs, fmt.Errorf("close main database: %w", closeErr))
			} else if err := verifySourceSelfContained(m.sourcePath); err != nil {
				errs = append(errs, err)
			} else {
				for _, d := range m.pathDests {
					if err := m.copyPathToDestination(d); err != nil {
						errs = append(errs, fmt.Errorf("copy %s: %w", d, err))
					}
				}
			}
		}
	} else if mainDB != nil {
		if err := mainDB.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close main database: %w", err))
		}
	}

	if err := m.Err(); err != nil {
		errs = append(errs, err)
	}
	if err := m.lateDirtyError(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (m *Manager) stopWorker() error {
	if m.done == nil {
		return nil
	}
	m.mu.Lock()
	if !m.stopped {
		m.stopped = true
		close(m.stop)
	}
	m.mu.Unlock()

	<-m.done
	if m.sourceDB != nil {
		err := m.sourceDB.Close()
		m.sourceDB = nil
		return err
	}
	return nil
}

func checkpointMainDB(db *sql.DB) error {
	if db == nil {
		return errors.New("main database is not open")
	}
	busy, err := checkpointDB(db)
	if err != nil {
		return fmt.Errorf("checkpoint main database: %w", err)
	}
	if busy != 0 {
		return fmt.Errorf("checkpoint main database: busy counter is %d", busy)
	}
	return nil
}
