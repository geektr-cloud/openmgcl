package tacos

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/geektr-cloud/openmgcl/internal/sqlitebackup"

	_ "modernc.org/sqlite"
)

// Store provides TACOS state.v2 persistence backed by SQLite.
type Store struct {
	db       *sql.DB
	identity *Identity
	backup   *sqlitebackup.Manager

	keyMu    sync.Mutex
	keyCache map[string][]byte

	writeMu      sync.RWMutex
	writesClosed bool

	closeOnce sync.Once
	closeErr  error
}

// Open creates or opens the SQLite database at path, runs migrations, and
// returns a ready-to-use Store. identity is mandatory: there is no plaintext
// database mode.
func Open(path string, identity *Identity) (*Store, error) {
	return OpenWithBackup(path, identity, nil)
}

// OpenWithBackup is Open plus a reusable backup manager. The manager starts
// after migrations and before the caller accepts traffic, so its initial
// sqlite backup sees the complete schema.
func OpenWithBackup(path string, identity *Identity, backup *sqlitebackup.Manager) (*Store, error) {
	if identity == nil {
		return nil, errors.New("encryption identity is required")
	}

	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_txlock=immediate", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	// Single connection simplifies WAL mode; remote-backend tofu is single-process.
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	store := &Store{
		db:       db,
		identity: identity,
		backup:   backup,
		keyCache: make(map[string][]byte),
	}
	if backup != nil {
		if err := backup.Start(); err != nil {
			db.Close()
			return nil, fmt.Errorf("start backup: %w", err)
		}
	}
	return store, nil
}

// Close closes the underlying database connection. It first closes the write
// gate and waits for in-flight writes, then runs the ordered backup shutdown
// sequence: drain the sqlite worker, checkpoint and close the main database,
// then copy path destinations.
func (s *Store) Close() error {
	s.StopWrites()
	s.closeOnce.Do(func() {
		if s.backup != nil {
			s.closeErr = s.backup.Shutdown(s.db)
			return
		}
		s.closeErr = s.db.Close()
	})
	return s.closeErr
}

// Abort closes the underlying database without running the backup shutdown
// sequence. It is used when startup fails before traffic is accepted, so path
// destinations keep their previous content. It also stops and waits for
// in-flight writes.
func (s *Store) Abort() error {
	s.StopWrites()
	s.closeOnce.Do(func() {
		if s.backup != nil {
			s.closeErr = s.backup.Abort(s.db)
			return
		}
		s.closeErr = s.db.Close()
	})
	return s.closeErr
}

// StopWrites closes the write gate and waits until every in-flight write has
// committed or rolled back. Later writes are rejected. It is idempotent.
func (s *Store) StopWrites() {
	s.writeMu.Lock()
	s.writesClosed = true
	s.writeMu.Unlock()
}

// beginWrite holds the write gate for the duration of one public write method,
// including its commit and dirty mark. The returned release must be deferred.
func (s *Store) beginWrite() (release func(), err error) {
	s.writeMu.RLock()
	if s.writesClosed {
		s.writeMu.RUnlock()
		return nil, errors.New("store is shutting down")
	}
	if s.backup != nil {
		if err := s.backup.Err(); err != nil {
			s.writeMu.RUnlock()
			return nil, fmt.Errorf("backup unavailable: %w", err)
		}
	}
	return s.writeMu.RUnlock, nil
}

// markDirty records a successful commit for the backup worker.
func (s *Store) markDirty() error {
	if s.backup == nil {
		return nil
	}
	return s.backup.MarkDirty()
}

func migrate(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	defer tx.Rollback()

	// Never add the KV access mark to a pre-existing secret schema. The
	// external mgcl-migrate tool owns that one-time conversion. A brand-new
	// database has neither table and is created below with the current schema.
	var existingSecretTables int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name IN ('secrets', 'secret_kvs')`,
	).Scan(&existingSecretTables); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if existingSecretTables > 0 {
		if err := requireSecretSchema(tx); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}

	stmts := []string{
		`CREATE TABLE IF NOT EXISTS workspaces (
			id TEXT PRIMARY KEY,
			organization TEXT NOT NULL,
			name TEXT NOT NULL,
			execution_mode TEXT NOT NULL DEFAULT 'local',
			terraform_version TEXT,
			locked INTEGER NOT NULL DEFAULT 0,
			lock_info TEXT,
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			UNIQUE(organization, name)
		)`,
		`CREATE TABLE IF NOT EXISTS state_versions (
			id TEXT PRIMARY KEY,
			workspace_id TEXT NOT NULL REFERENCES workspaces(id),
			serial INTEGER NOT NULL,
			lineage TEXT,
			md5 TEXT,
			state BLOB,
			json_state BLOB,
			json_state_outputs BLOB,
			status TEXT NOT NULL DEFAULT 'pending',
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_state_versions_finalized_workspace_serial
			ON state_versions(workspace_id, serial) WHERE status = 'finalized'`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_state_versions_pending_workspace_serial
			ON state_versions(workspace_id, serial) WHERE status = 'pending'`,
		`CREATE TABLE IF NOT EXISTS lineage_keys (
			lineage TEXT PRIMARY KEY,
			key_fingerprint TEXT NOT NULL UNIQUE,
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
		)`,
		`CREATE TABLE IF NOT EXISTS wrapped_lineage_keys (
			key_fingerprint TEXT PRIMARY KEY REFERENCES lineage_keys(key_fingerprint),
			recipient_fingerprint TEXT NOT NULL,
			suite INTEGER NOT NULL,
			ephemeral_public_key BLOB NOT NULL,
			hkdf_salt BLOB NOT NULL,
			nonce BLOB NOT NULL,
			wrapped_key BLOB NOT NULL,
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
		)`,
		`CREATE INDEX IF NOT EXISTS idx_wrapped_lineage_keys_recipient
			ON wrapped_lineage_keys(recipient_fingerprint)`,
		`CREATE TABLE IF NOT EXISTS secrets (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS secret_deks (
			secret_id TEXT PRIMARY KEY REFERENCES secrets(id) ON DELETE CASCADE,
			dek_fingerprint TEXT NOT NULL UNIQUE,
			recipient_fingerprint TEXT NOT NULL,
			suite INTEGER NOT NULL,
			ephemeral_public_key BLOB NOT NULL,
			hkdf_salt BLOB NOT NULL,
			nonce BLOB NOT NULL,
			wrapped_dek BLOB NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS secret_kvs (
			secret_id TEXT NOT NULL REFERENCES secrets(id) ON DELETE CASCADE,
			key TEXT NOT NULL,
			suite INTEGER NOT NULL,
			nonce BLOB NOT NULL,
			ciphertext BLOB NOT NULL,
			is_ephemeral INTEGER NOT NULL DEFAULT 1 CHECK (is_ephemeral IN (0, 1)),
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY (secret_id, key)
		)`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	if err := requireSecretSchema(tx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// randomID returns "prefix-" + 16 hex chars from crypto/rand.
func randomID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}

// nowISO returns the current UTC time in RFC3339Millisecond format.
func nowISO() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

// ──────────────────────────────────────────────────────────────────────────────
// Workspace
// ──────────────────────────────────────────────────────────────────────────────

// Workspace is the row representation used internally.
type Workspace struct {
	ID               string
	Organization     string
	Name             string
	ExecutionMode    string
	TerraformVersion string
	Locked           bool
	LockInfo         string
	CreatedAt        string
}

// CreateWorkspace inserts a new workspace. Returns the generated ID.
func (s *Store) CreateWorkspace(org, name, tfVersion string) (*Workspace, error) {
	release, err := s.beginWrite()
	if err != nil {
		return nil, err
	}
	defer release()

	id := randomID("ws")
	if tfVersion == "" {
		tfVersion = "latest"
	}
	_, err = s.db.Exec(
		`INSERT INTO workspaces (id, organization, name, terraform_version) VALUES (?, ?, ?, ?)`,
		id, org, name, tfVersion,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, ErrConflict("workspace already exists")
		}
		return nil, err
	}
	if err := s.markDirty(); err != nil {
		return nil, err
	}
	return s.GetWorkspaceByID(id)
}

// UpdateWorkspaceTerraformVersion updates one workspace's terraform-version.
// A nil version leaves the stored value unchanged. An empty version follows
// CreateWorkspace's default and becomes "latest". Returns ErrNotFound for an
// unknown workspace.
func (s *Store) UpdateWorkspaceTerraformVersion(wsID string, tfVersion *string) error {
	release, err := s.beginWrite()
	if err != nil {
		return err
	}
	defer release()

	if tfVersion == nil {
		_, err := s.GetWorkspaceByID(wsID)
		return err
	}
	version := *tfVersion
	if version == "" {
		version = "latest"
	}
	res, err := s.db.Exec(`UPDATE workspaces SET terraform_version = ? WHERE id = ?`, version, wsID)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNotFound("workspace not found")
	}
	return s.markDirty()
}

// GetWorkspace returns a workspace by (organization, name).
func (s *Store) GetWorkspace(org, name string) (*Workspace, error) {
	row := s.db.QueryRow(
		`SELECT id, organization, name, execution_mode, COALESCE(terraform_version,'latest'), locked, COALESCE(lock_info,''), created_at
		FROM workspaces WHERE organization = ? AND name = ?`, org, name)
	return scanWorkspace(row)
}

// GetWorkspaceByID returns a workspace by its ID.
func (s *Store) GetWorkspaceByID(id string) (*Workspace, error) {
	row := s.db.QueryRow(
		`SELECT id, organization, name, execution_mode, COALESCE(terraform_version,'latest'), locked, COALESCE(lock_info,''), created_at
		FROM workspaces WHERE id = ?`, id)
	return scanWorkspace(row)
}

// ListWorkspaces returns a page of workspaces. namePrefix is an optional LIKE
// prefix filter. Page is 1-based.
func (s *Store) ListWorkspaces(org, namePrefix string, page, pageSize int) ([]Workspace, int, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = maxPageSize
	}
	if pageSize > maxPageSize {
		pageSize = maxPageSize
	}

	var args []any
	where := "WHERE organization = ?"
	args = append(args, org)
	if namePrefix != "" {
		where += " AND name LIKE ?"
		args = append(args, namePrefix+"%")
	}

	// Count
	var total int
	err := s.db.QueryRow("SELECT COUNT(*) FROM workspaces "+where, args...).Scan(&total)
	if err != nil {
		return nil, 0, err
	}

	// Page
	offset := (page - 1) * pageSize
	rows, err := s.db.Query(
		"SELECT id, organization, name, execution_mode, COALESCE(terraform_version,'latest'), locked, COALESCE(lock_info,''), created_at FROM workspaces "+where+
			" ORDER BY created_at LIMIT ? OFFSET ?",
		append(args, pageSize, offset)...,
	)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []Workspace
	for rows.Next() {
		w, err := scanWorkspaceRows(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *w)
	}
	return out, total, rows.Err()
}

func scanWorkspace(row *sql.Row) (*Workspace, error) {
	var w Workspace
	if err := row.Scan(&w.ID, &w.Organization, &w.Name, &w.ExecutionMode, &w.TerraformVersion, &w.Locked, &w.LockInfo, &w.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound("workspace not found")
		}
		return nil, err
	}
	return &w, nil
}

func scanWorkspaceRows(rows *sql.Rows) (*Workspace, error) {
	var w Workspace
	if err := rows.Scan(&w.ID, &w.Organization, &w.Name, &w.ExecutionMode, &w.TerraformVersion, &w.Locked, &w.LockInfo, &w.CreatedAt); err != nil {
		return nil, err
	}
	return &w, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Lock / Unlock
// ──────────────────────────────────────────────────────────────────────────────

// LockWorkspace acquires the workspace lock. Returns ErrConflict if already locked.
func (s *Store) LockWorkspace(wsID, lockInfo string) error {
	release, err := s.beginWrite()
	if err != nil {
		return err
	}
	defer release()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var locked int
	if err := tx.QueryRow(`SELECT locked FROM workspaces WHERE id = ?`, wsID).Scan(&locked); err != nil {
		if err == sql.ErrNoRows {
			return ErrNotFound("workspace not found")
		}
		return err
	}
	if locked != 0 {
		return ErrConflict("workspace already locked")
	}
	if _, err := tx.Exec(`UPDATE workspaces SET locked = 1, lock_info = ? WHERE id = ?`, lockInfo, wsID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.markDirty()
}

// UnlockWorkspace releases the workspace lock. Returns ErrConflict if not locked.
func (s *Store) UnlockWorkspace(wsID string) error {
	release, err := s.beginWrite()
	if err != nil {
		return err
	}
	defer release()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var locked int
	if err := tx.QueryRow(`SELECT locked FROM workspaces WHERE id = ?`, wsID).Scan(&locked); err != nil {
		if err == sql.ErrNoRows {
			return ErrNotFound("workspace not found")
		}
		return err
	}
	if locked == 0 {
		return ErrConflict("workspace not locked")
	}
	if _, err := tx.Exec(`UPDATE workspaces SET locked = 0, lock_info = NULL WHERE id = ?`, wsID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.markDirty()
}

// ──────────────────────────────────────────────────────────────────────────────
// State Version
// ──────────────────────────────────────────────────────────────────────────────

// StateVersion is the row representation.
type StateVersion struct {
	ID               string
	WorkspaceID      string
	Serial           int
	Lineage          string
	MD5              string
	State            []byte
	JSONState        []byte
	JSONStateOutputs []byte
	Status           string
	CreatedAt        string
	StatePresent     bool
	JSONStatePresent bool
	OutputsPresent   bool
}

// GetMaxFinalizedSerial returns the highest finalized serial for the workspace, or -1 if none.
func (s *Store) GetMaxFinalizedSerial(wsID string) (int, error) {
	var v sql.NullInt64
	err := s.db.QueryRow(
		`SELECT MAX(serial) FROM state_versions WHERE workspace_id = ? AND status = 'finalized'`, wsID,
	).Scan(&v)
	if err != nil {
		return -1, err
	}
	if !v.Valid {
		return -1, nil
	}
	return int(v.Int64), nil
}

// CreateStateVersion inserts a new state version row. serial is client
// generated: normal updates advance it, but lower or equal serials are accepted
// for restore compatibility. An identical finalized retransmit returns a
// conflict regardless of the client's force flag. state may be nil (modern
// upload path) or populated (fallback inline path).
//
// The final bool parameter is retained for call-site compatibility. The
// server does not trust force and it does not bypass any validation.
func (s *Store) CreateStateVersion(wsID string, serial int, lineage, md5 string, state, jsonState, jsonStateOutputs []byte, _ bool) (*StateVersion, error) {
	release, err := s.beginWrite()
	if err != nil {
		return nil, err
	}
	defer release()

	if lineage == "" {
		return nil, ErrUnprocessable("lineage is required")
	}
	if serial < 0 {
		return nil, ErrUnprocessable("serial must be non-negative")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var exists int
	if err := tx.QueryRow(`SELECT 1 FROM workspaces WHERE id = ?`, wsID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound("workspace not found")
		}
		return nil, err
	}

	status := "pending"
	if len(state) > 0 {
		status = "finalized"
	}

	var pendingID string
	err = tx.QueryRow(
		`SELECT id FROM state_versions WHERE workspace_id = ? AND serial = ? AND status = 'pending'`,
		wsID, serial,
	).Scan(&pendingID)
	hasPending := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	var finalizedID, finalizedLineage, finalizedMD5 string
	err = tx.QueryRow(
		`SELECT id, COALESCE(lineage,''), COALESCE(md5,'')
		FROM state_versions WHERE workspace_id = ? AND serial = ? AND status = 'finalized'`,
		wsID, serial,
	).Scan(&finalizedID, &finalizedLineage, &finalizedMD5)
	hasFinalized := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	switch {
	case hasFinalized && finalizedLineage == lineage && finalizedMD5 == md5:
		// Identical finalized retransmit, including historical serials after
		// a restore and even when a same-serial pending row exists. force is
		// not trusted, so this applies to force requests too.
		return nil, ErrConflict(fmt.Sprintf("serial %d already exists for workspace", serial))
	case status == "pending" && hasPending:
		// Safe retry: replace the pending row, never the finalized version
		// that is still serving current/download.
		if _, err := tx.Exec(`DELETE FROM state_versions WHERE id = ?`, pendingID); err != nil {
			return nil, err
		}
	case status == "finalized":
		// Inline state carries the payload, so the replacement is atomic.
		if hasPending {
			if _, err := tx.Exec(`DELETE FROM state_versions WHERE id = ?`, pendingID); err != nil {
				return nil, err
			}
		}
		if hasFinalized {
			if _, err := tx.Exec(`DELETE FROM state_versions WHERE id = ?`, finalizedID); err != nil {
				return nil, err
			}
		}
	}

	id := randomID("sv")

	dek, fingerprint, err := s.lineageKeyTx(tx, lineage, true)
	if err != nil {
		return nil, err
	}
	encryptedState, err := sealOptional(dek, fingerprint, wsID, id, lineage, "state", state)
	if err != nil {
		return nil, err
	}
	encryptedJSONState, err := sealOptional(dek, fingerprint, wsID, id, lineage, "json_state", jsonState)
	if err != nil {
		return nil, err
	}
	encryptedJSONOutputs, err := sealOptional(dek, fingerprint, wsID, id, lineage, "json_state_outputs", jsonStateOutputs)
	if err != nil {
		return nil, err
	}

	_, err = tx.Exec(
		`INSERT INTO state_versions (id, workspace_id, serial, lineage, md5, state, json_state, json_state_outputs, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, wsID, serial, lineage, md5, encryptedState, encryptedJSONState, encryptedJSONOutputs, status,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, ErrConflict(fmt.Sprintf("serial %d already exists for workspace", serial))
		}
		return nil, err
	}
	var createdAt string
	if err := tx.QueryRow(`SELECT created_at FROM state_versions WHERE id = ?`, id).Scan(&createdAt); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.cacheKey(fingerprint, dek)
	if err := s.markDirty(); err != nil {
		return nil, err
	}

	// Return the row from this transaction rather than re-reading it: another
	// concurrent same-serial retry may replace this pending row immediately
	// after commit.
	return &StateVersion{
		ID:               id,
		WorkspaceID:      wsID,
		Serial:           serial,
		Lineage:          lineage,
		MD5:              md5,
		State:            state,
		JSONState:        jsonState,
		JSONStateOutputs: jsonStateOutputs,
		Status:           status,
		CreatedAt:        createdAt,
		StatePresent:     state != nil,
		JSONStatePresent: jsonState != nil,
		OutputsPresent:   jsonStateOutputs != nil,
	}, nil
}

func sealOptional(dek []byte, fingerprint, wsID, svID, lineage, fieldName string, data []byte) ([]byte, error) {
	if data == nil {
		return nil, nil
	}
	return sealPayload(dek, fingerprint, wsID, svID, lineage, fieldName, data)
}

// lineageKeyTx returns the existing DEK or creates one inside the transaction.
func (s *Store) lineageKeyTx(tx *sql.Tx, lineage string, create bool) ([]byte, string, error) {
	var fingerprint string
	err := tx.QueryRow(`SELECT key_fingerprint FROM lineage_keys WHERE lineage = ?`, lineage).Scan(&fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		if !create {
			return nil, "", errors.New("lineage key metadata is missing")
		}
		dek, record, err := newWrappedLineageKey(s.identity, lineage)
		if err != nil {
			return nil, "", err
		}
		if _, err := tx.Exec(
			`INSERT INTO lineage_keys (lineage, key_fingerprint) VALUES (?, ?)`,
			lineage, record.KeyFingerprint,
		); err != nil {
			return nil, "", fmt.Errorf("insert lineage key: %w", err)
		}
		if _, err := tx.Exec(
			`INSERT INTO wrapped_lineage_keys
			(key_fingerprint, recipient_fingerprint, suite, ephemeral_public_key, hkdf_salt, nonce, wrapped_key)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			record.KeyFingerprint, record.RecipientFingerprint, record.Suite,
			record.EphemeralPublicKey, record.HKDFSalt, record.Nonce, record.WrappedKey,
		); err != nil {
			return nil, "", fmt.Errorf("insert wrapped lineage key: %w", err)
		}
		return dek, record.KeyFingerprint, nil
	}
	if err != nil {
		return nil, "", err
	}

	record, err := getWrappedKeyTx(tx, fingerprint)
	if err != nil {
		return nil, "", err
	}
	if _, err := validateWrappedKeyRecord(record); err != nil {
		return nil, "", err
	}
	if record.RecipientFingerprint != s.identity.recipient {
		return nil, "", ErrForbidden("lineage state is not accessible with the configured recipient")
	}
	if dek, ok := s.cachedKey(fingerprint); ok {
		return dek, fingerprint, nil
	}
	dek, err := unwrapLineageKey(s.identity, lineage, record)
	if err != nil {
		return nil, "", err
	}
	return dek, fingerprint, nil
}

func getWrappedKeyTx(tx *sql.Tx, fingerprint string) (*wrappedKeyRecord, error) {
	record := &wrappedKeyRecord{KeyFingerprint: fingerprint}
	err := tx.QueryRow(
		`SELECT recipient_fingerprint, suite, ephemeral_public_key, hkdf_salt, nonce, wrapped_key
		FROM wrapped_lineage_keys WHERE key_fingerprint = ?`, fingerprint,
	).Scan(
		&record.RecipientFingerprint, &record.Suite, &record.EphemeralPublicKey,
		&record.HKDFSalt, &record.Nonce, &record.WrappedKey,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("lineage key metadata is missing")
	}
	if err != nil {
		return nil, err
	}
	return record, nil
}

func (s *Store) cacheKey(fingerprint string, dek []byte) {
	copyOfDEK := append([]byte(nil), dek...)
	s.keyMu.Lock()
	s.keyCache[fingerprint] = copyOfDEK
	s.keyMu.Unlock()
}

func (s *Store) cachedKey(fingerprint string) ([]byte, bool) {
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	dek, ok := s.keyCache[fingerprint]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), dek...), true
}

// GetStateVersion returns a state version by ID.
func (s *Store) GetStateVersion(id string) (*StateVersion, error) {
	row := s.db.QueryRow(
		`SELECT id, workspace_id, serial, COALESCE(lineage,''), COALESCE(md5,''), state, json_state, json_state_outputs, status, created_at,
			state IS NOT NULL, json_state IS NOT NULL, json_state_outputs IS NOT NULL
		FROM state_versions WHERE id = ?`, id)
	return s.scanStateVersion(row)
}

// GetCurrentStateVersion returns the most recently created finalized state
// version for a workspace. A restore may lower the serial, so insertion order
// (rowid) is authoritative rather than MAX(serial).
func (s *Store) GetCurrentStateVersion(wsID string) (*StateVersion, error) {
	row := s.db.QueryRow(
		`SELECT id, workspace_id, serial, COALESCE(lineage,''), COALESCE(md5,''), state, json_state, json_state_outputs, status, created_at,
			state IS NOT NULL, json_state IS NOT NULL, json_state_outputs IS NOT NULL
		FROM state_versions WHERE workspace_id = ? AND status = 'finalized'
		ORDER BY rowid DESC LIMIT 1`, wsID)
	sv, err := s.scanStateVersion(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound("no current state version")
		}
		return nil, err
	}
	return sv, nil
}

// UploadState writes the raw state bytes and finalizes the version (idempotent).
func (s *Store) UploadState(id string, data []byte) error {
	return s.uploadStateField(id, "state", data, true)
}

// UploadJSONState writes the JSON state bytes (idempotent).
func (s *Store) UploadJSONState(id string, data []byte) error {
	return s.uploadStateField(id, "json_state", data, false)
}

func (s *Store) uploadStateField(id, fieldName string, data []byte, finalize bool) error {
	release, err := s.beginWrite()
	if err != nil {
		return err
	}
	defer release()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var rowID int64
	var wsID, lineage, md5, status string
	var serial int
	if err := tx.QueryRow(
		`SELECT rowid, workspace_id, COALESCE(lineage,''), COALESCE(md5,''), status, serial
		FROM state_versions WHERE id = ?`, id,
	).Scan(&rowID, &wsID, &lineage, &md5, &status, &serial); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound("state version not found")
		}
		return err
	}
	if lineage == "" {
		return errors.New("state version has no lineage")
	}
	if status == "pending" {
		var newerFinalized int
		if err := tx.QueryRow(
			`SELECT EXISTS(
				SELECT 1 FROM state_versions
				WHERE workspace_id = ? AND status = 'finalized' AND rowid > ?
			)`, wsID, rowID,
		).Scan(&newerFinalized); err != nil {
			return err
		}
		if newerFinalized != 0 {
			return ErrConflict("state version upload is stale")
		}
	}

	dek, fingerprint, err := s.lineageKeyTx(tx, lineage, false)
	if err != nil {
		return err
	}
	encrypted, err := sealPayload(dek, fingerprint, wsID, id, lineage, fieldName, data)
	if err != nil {
		return err
	}

	var res sql.Result
	if finalize {
		if status == "pending" {
			// Defense for pending rows created before the identical-finalized
			// check existed (or by an older database): never finalize an
			// identical retransmit of the version it would replace.
			var finalizedLineage, finalizedMD5 string
			err := tx.QueryRow(
				`SELECT COALESCE(lineage,''), COALESCE(md5,'')
				FROM state_versions
				WHERE workspace_id = ? AND serial = ? AND status = 'finalized'`,
				wsID, serial,
			).Scan(&finalizedLineage, &finalizedMD5)
			switch {
			case err == nil && finalizedLineage == lineage && finalizedMD5 == md5:
				return ErrConflict("state version upload is an identical finalized retransmit")
			case err == nil, errors.Is(err, sql.ErrNoRows):
			default:
				return err
			}

			// The old finalized version stays current until this upload
			// succeeds. Replace it atomically with the new finalized row.
			if _, err := tx.Exec(
				`DELETE FROM state_versions
				WHERE workspace_id = ? AND serial = ? AND status = 'finalized' AND id != ?`,
				wsID, serial, id,
			); err != nil {
				return err
			}
		}
		res, err = tx.Exec(
			`UPDATE state_versions SET state = ?, status = 'finalized' WHERE id = ?`, encrypted, id)
	} else {
		res, err = tx.Exec(
			`UPDATE state_versions SET json_state = ? WHERE id = ?`, encrypted, id)
	}
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound("state version not found")
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.cacheKey(fingerprint, dek)
	return s.markDirty()
}

func (s *Store) scanStateVersion(row *sql.Row) (*StateVersion, error) {
	var sv StateVersion
	var statePresent, jsonStatePresent, outputsPresent bool
	if err := row.Scan(
		&sv.ID, &sv.WorkspaceID, &sv.Serial, &sv.Lineage, &sv.MD5,
		&sv.State, &sv.JSONState, &sv.JSONStateOutputs, &sv.Status, &sv.CreatedAt,
		&statePresent, &jsonStatePresent, &outputsPresent,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound("state version not found")
		}
		return nil, err
	}
	if statePresent && len(sv.State) == 0 ||
		jsonStatePresent && len(sv.JSONState) == 0 ||
		outputsPresent && len(sv.JSONStateOutputs) == 0 {
		return nil, errors.New("invalid encrypted state envelope")
	}
	sv.StatePresent = statePresent
	sv.JSONStatePresent = jsonStatePresent
	sv.OutputsPresent = outputsPresent
	if err := s.decryptStateVersion(&sv); err != nil {
		return nil, err
	}
	return &sv, nil
}

func (s *Store) decryptStateVersion(sv *StateVersion) error {
	fields := []struct {
		name string
		data *[]byte
	}{
		{"state", &sv.State},
		{"json_state", &sv.JSONState},
		{"json_state_outputs", &sv.JSONStateOutputs},
	}
	for _, field := range fields {
		if *field.data == nil {
			continue
		}
		plaintext, err := s.decryptStateField(sv, field.name, *field.data)
		if err != nil {
			return err
		}
		*field.data = plaintext
	}
	return nil
}

func (s *Store) decryptStateField(sv *StateVersion, fieldName string, data []byte) ([]byte, error) {
	fingerprintRaw, err := payloadFingerprint(data)
	if err != nil {
		return nil, err
	}
	fingerprint := fingerprintString(fingerprintRaw[:])
	dek, err := s.lineageKeyForRead(fingerprint, sv.Lineage)
	if err != nil {
		return nil, err
	}
	return openPayload(dek, sv.WorkspaceID, sv.ID, sv.Lineage, fieldName, data)
}

func (s *Store) lineageKeyForRead(fingerprint, lineage string) ([]byte, error) {
	record := &wrappedKeyRecord{KeyFingerprint: fingerprint}
	err := s.db.QueryRow(
		`SELECT w.recipient_fingerprint, w.suite, w.ephemeral_public_key, w.hkdf_salt, w.nonce, w.wrapped_key
		FROM wrapped_lineage_keys w
		JOIN lineage_keys l ON l.key_fingerprint = w.key_fingerprint
		WHERE w.key_fingerprint = ? AND l.lineage = ?`, fingerprint, lineage,
	).Scan(
		&record.RecipientFingerprint, &record.Suite, &record.EphemeralPublicKey,
		&record.HKDFSalt, &record.Nonce, &record.WrappedKey,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("encrypted state key metadata is missing or inconsistent")
	}
	if err != nil {
		return nil, err
	}
	if _, err := validateWrappedKeyRecord(record); err != nil {
		return nil, err
	}
	if record.RecipientFingerprint != s.identity.recipient {
		return nil, ErrForbidden("lineage state is not accessible with the configured recipient")
	}
	if dek, ok := s.cachedKey(fingerprint); ok {
		return dek, nil
	}
	dek, err := unwrapLineageKey(s.identity, lineage, record)
	if err != nil {
		return nil, err
	}
	s.cacheKey(fingerprint, dek)
	return dek, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Errors
// ──────────────────────────────────────────────────────────────────────────────

// APIError represents a JSON:API error response.
type APIError struct {
	Status int    `json:"status"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
}

func (e *APIError) Error() string { return e.Detail }

// ErrNotFound creates a 404 APIError.
func ErrNotFound(detail string) *APIError {
	return &APIError{Status: 404, Title: "Not Found", Detail: detail}
}

// ErrConflict creates a 409 APIError.
func ErrConflict(detail string) *APIError {
	return &APIError{Status: 409, Title: "Conflict", Detail: detail}
}

// ErrForbidden creates a 403 APIError.
func ErrForbidden(detail string) *APIError {
	return &APIError{Status: 403, Title: "Forbidden", Detail: detail}
}

// ErrUnprocessable creates a 422 APIError.
func ErrUnprocessable(detail string) *APIError {
	return &APIError{Status: 422, Title: "Unprocessable Entity", Detail: detail}
}

// IsAPIError checks if an error is an APIError with the given status.
func IsAPIError(err error, status int) bool {
	if e, ok := err.(*APIError); ok {
		return e.Status == status
	}
	return false
}
