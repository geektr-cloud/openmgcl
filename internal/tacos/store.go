package tacos

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store provides TACOS state.v2 persistence backed by SQLite.
type Store struct {
	db *sql.DB
}

// Open creates or opens the SQLite database at path, runs migrations, and
// returns a ready-to-use Store.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", path)
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
	return &Store{db: db}, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error { return s.db.Close() }

func migrate(db *sql.DB) error {
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
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			UNIQUE(workspace_id, serial)
		)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
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
	id := randomID("ws")
	if tfVersion == "" {
		tfVersion = "latest"
	}
	_, err := s.db.Exec(
		`INSERT INTO workspaces (id, organization, name, terraform_version) VALUES (?, ?, ?, ?)`,
		id, org, name, tfVersion,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, ErrConflict("workspace already exists")
		}
		return nil, err
	}
	return s.GetWorkspaceByID(id)
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
		pageSize = 100
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
	return tx.Commit()
}

// UnlockWorkspace releases the workspace lock. Returns ErrConflict if not locked.
func (s *Store) UnlockWorkspace(wsID string) error {
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
	return tx.Commit()
}

// ──────────────────────────────────────────────────────────────────────────────
// State Version
// ──────────────────────────────────────────────────────────────────────────────

// StateVersion is the row representation.
type StateVersion struct {
	ID                 string
	WorkspaceID        string
	Serial             int
	Lineage            string
	MD5                string
	State              []byte
	JSONState          []byte
	JSONStateOutputs   []byte
	Status             string
	CreatedAt          string
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

// CreateStateVersion inserts a new state version row. If force is false it
// validates serial > max finalized serial. Returns the generated ID.
// state may be nil (modern upload path) or populated (fallback inline path).
func (s *Store) CreateStateVersion(wsID string, serial int, lineage, md5 string, state, jsonState, jsonStateOutputs []byte, force bool) (*StateVersion, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if !force {
		maxSer, err := s.getMaxFinalizedSerialTx(tx, wsID)
		if err != nil {
			return nil, err
		}
		if serial <= maxSer {
			return nil, ErrConflict(fmt.Sprintf("serial %d <= current max finalized %d", serial, maxSer))
		}
	}

	id := randomID("sv")
	status := "pending"
	if len(state) > 0 {
		status = "finalized"
	}

	_, err = tx.Exec(
		`INSERT INTO state_versions (id, workspace_id, serial, lineage, md5, state, json_state, json_state_outputs, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, wsID, serial, lineage, md5, state, jsonState, jsonStateOutputs, status,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, ErrConflict(fmt.Sprintf("serial %d already exists for workspace", serial))
		}
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetStateVersion(id)
}

func (s *Store) getMaxFinalizedSerialTx(tx *sql.Tx, wsID string) (int, error) {
	var v sql.NullInt64
	err := tx.QueryRow(
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

// GetStateVersion returns a state version by ID.
func (s *Store) GetStateVersion(id string) (*StateVersion, error) {
	row := s.db.QueryRow(
		`SELECT id, workspace_id, serial, COALESCE(lineage,''), COALESCE(md5,''), state, json_state, json_state_outputs, status, created_at
		FROM state_versions WHERE id = ?`, id)
	return scanStateVersion(row)
}

// GetCurrentStateVersion returns the latest finalized state version for a workspace.
func (s *Store) GetCurrentStateVersion(wsID string) (*StateVersion, error) {
	row := s.db.QueryRow(
		`SELECT id, workspace_id, serial, COALESCE(lineage,''), COALESCE(md5,''), state, json_state, json_state_outputs, status, created_at
		FROM state_versions WHERE workspace_id = ? AND status = 'finalized'
		ORDER BY serial DESC LIMIT 1`, wsID)
	sv, err := scanStateVersion(row)
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
	res, err := s.db.Exec(
		`UPDATE state_versions SET state = ?, status = 'finalized' WHERE id = ?`, data, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound("state version not found")
	}
	return nil
}

// UploadJSONState writes the JSON state bytes (idempotent).
func (s *Store) UploadJSONState(id string, data []byte) error {
	res, err := s.db.Exec(
		`UPDATE state_versions SET json_state = ? WHERE id = ?`, data, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound("state version not found")
	}
	return nil
}

func scanStateVersion(row *sql.Row) (*StateVersion, error) {
	var sv StateVersion
	if err := row.Scan(&sv.ID, &sv.WorkspaceID, &sv.Serial, &sv.Lineage, &sv.MD5, &sv.State, &sv.JSONState, &sv.JSONStateOutputs, &sv.Status, &sv.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound("state version not found")
		}
		return nil, err
	}
	return &sv, nil
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
func ErrNotFound(detail string) *APIError { return &APIError{Status: 404, Title: "Not Found", Detail: detail} }

// ErrConflict creates a 409 APIError.
func ErrConflict(detail string) *APIError { return &APIError{Status: 409, Title: "Conflict", Detail: detail} }

// ErrUnprocessable creates a 422 APIError.
func ErrUnprocessable(detail string) *APIError { return &APIError{Status: 422, Title: "Unprocessable Entity", Detail: detail} }

// IsAPIError checks if an error is an APIError with the given status.
func IsAPIError(err error, status int) bool {
	if e, ok := err.(*APIError); ok {
		return e.Status == status
	}
	return false
}
