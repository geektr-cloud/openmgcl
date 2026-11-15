package tacos

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// ErrStateDatabaseNotFound reports a missing state database. Read-only commands
// never create it.
var ErrStateDatabaseNotFound = errors.New("state database not found")

// StateSummary is the plaintext state metadata for one workspace.
type StateSummary struct {
	ID           string
	Organization string
	Name         string
	Versions     int
	Pending      int
	StoredBytes  uint64
}

// StateVersionInfo is the plaintext metadata of one state version.
type StateVersionInfo struct {
	ID        string
	Serial    int
	Status    string
	Current   bool
	CreatedAt string
}

// StatePrunePlan describes what PruneState would delete and keep.
type StatePrunePlan struct {
	TotalFinalized  int
	DeleteFinalized int
	KeepFinalized   int
	Pending         int
}

// ParseStateReference splits a strict <organization>/<workspace> reference.
// Exactly one slash is required; both sides must be non-empty.
func ParseStateReference(ref string) (organization, workspace string, err error) {
	if strings.Count(ref, "/") != 1 {
		return "", "", errors.New("state reference must be <organization>/<workspace>")
	}
	organization, workspace, _ = strings.Cut(ref, "/")
	if organization == "" {
		return "", "", errors.New("state reference organization is empty")
	}
	if workspace == "" {
		return "", "", errors.New("state reference workspace is empty")
	}
	return organization, workspace, nil
}

// OpenStateStoreReadOnly opens an existing database without creating or
// migrating it. identity may be nil for metadata-only commands.
func OpenStateStoreReadOnly(path string, identity *Identity) (*Store, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, ErrStateDatabaseNotFound
		}
		return nil, fmt.Errorf("stat state database: %w", err)
	}
	db, err := openStateDBReadOnly(path)
	if err != nil {
		return nil, err
	}
	return &Store{db: db, identity: identity, keyCache: make(map[string][]byte)}, nil
}

// GetStateWorkspace returns a workspace after validating the reference and the
// shared schema. Commands use it for read-only preflight so a legacy secret
// schema or missing state tables fail before any confirmation or write.
func (s *Store) GetStateWorkspace(org, workspace string) (*Workspace, error) {
	organization, name, err := ParseStateReference(org + "/" + workspace)
	if err != nil {
		return nil, err
	}
	ready, err := s.stateReadReady()
	if err != nil {
		return nil, err
	}
	if !ready {
		return nil, ErrNotFound("workspace not found")
	}
	return s.GetWorkspace(organization, name)
}

func openStateDBReadOnly(path string) (*sql.DB, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve state database path: %w", err)
	}
	query := url.Values{
		"_pragma": {"busy_timeout(5000)"},
	}
	query.Add("_pragma", "foreign_keys(1)")
	query.Set("mode", "ro")
	dsn := (&url.URL{Scheme: "file", Path: absPath, RawQuery: query.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open state database: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		if os.IsNotExist(err) {
			return nil, ErrStateDatabaseNotFound
		}
		return nil, fmt.Errorf("open state database: %w", err)
	}
	return db, nil
}

// ListStateSummaries returns one summary per workspace, including workspaces
// with no state versions, sorted by organization and name.
func (s *Store) ListStateSummaries() ([]StateSummary, error) {
	ready, err := s.stateReadReady()
	if err != nil {
		return nil, err
	}
	if !ready {
		return []StateSummary{}, nil
	}

	rows, err := s.db.Query(
		`SELECT w.id, w.organization, w.name,
			COUNT(CASE WHEN sv.status = 'finalized' THEN 1 END),
			COUNT(CASE WHEN sv.status = 'pending' THEN 1 END),
			COALESCE(SUM(
				COALESCE(length(sv.state), 0) +
				COALESCE(length(sv.json_state), 0) +
				COALESCE(length(sv.json_state_outputs), 0)
			), 0)
		FROM workspaces w
		LEFT JOIN state_versions sv ON sv.workspace_id = w.id
		GROUP BY w.id, w.organization, w.name
		ORDER BY w.organization, w.name, w.id`)
	if err != nil {
		return nil, fmt.Errorf("list state summaries: %w", err)
	}
	defer rows.Close()

	out := make([]StateSummary, 0)
	for rows.Next() {
		var item StateSummary
		var stored int64
		if err := rows.Scan(
			&item.ID, &item.Organization, &item.Name,
			&item.Versions, &item.Pending, &stored,
		); err != nil {
			return nil, err
		}
		item.StoredBytes = uint64(stored)
		out = append(out, item)
	}
	return out, rows.Err()
}

// ListStateVersionInfo returns every version of one workspace ordered newest
// first by rowid. Only the most recently inserted finalized version is current.
func (s *Store) ListStateVersionInfo(org, workspace string) ([]StateVersionInfo, error) {
	organization, name, err := ParseStateReference(org + "/" + workspace)
	if err != nil {
		return nil, err
	}
	ready, err := s.stateReadReady()
	if err != nil {
		return nil, err
	}
	if !ready {
		return nil, ErrNotFound("workspace not found")
	}

	ws, err := s.GetWorkspace(organization, name)
	if err != nil {
		return nil, err
	}

	rows, err := s.db.Query(
		`SELECT id, serial, status, created_at
		FROM state_versions WHERE workspace_id = ?
		ORDER BY rowid DESC`, ws.ID)
	if err != nil {
		return nil, fmt.Errorf("list state versions: %w", err)
	}
	defer rows.Close()

	out := make([]StateVersionInfo, 0)
	currentSet := false
	for rows.Next() {
		var item StateVersionInfo
		if err := rows.Scan(&item.ID, &item.Serial, &item.Status, &item.CreatedAt); err != nil {
			return nil, err
		}
		if !currentSet && item.Status == "finalized" {
			item.Current = true
			currentSet = true
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// ExportCurrentState returns the current finalized version's JSON state, or its
// JSON state outputs when outputs is true. It never falls back to the raw state
// blob.
func (s *Store) ExportCurrentState(org, workspace string, outputs bool) ([]byte, error) {
	organization, name, err := ParseStateReference(org + "/" + workspace)
	if err != nil {
		return nil, err
	}
	if s.identity == nil {
		return nil, errors.New("encryption identity is required")
	}
	ready, err := s.stateReadReady()
	if err != nil {
		return nil, err
	}
	if !ready {
		return nil, ErrNotFound("workspace not found")
	}

	ws, err := s.GetWorkspace(organization, name)
	if err != nil {
		return nil, err
	}

	var id, workspaceID, lineage string
	var jsonState, jsonStateOutputs []byte
	var jsonStatePresent, outputsPresent bool
	err = s.db.QueryRow(
		`SELECT id, workspace_id, COALESCE(lineage, ''), json_state, json_state_outputs,
			json_state IS NOT NULL, json_state_outputs IS NOT NULL
		FROM state_versions WHERE workspace_id = ? AND status = 'finalized'
		ORDER BY rowid DESC LIMIT 1`, ws.ID,
	).Scan(&id, &workspaceID, &lineage, &jsonState, &jsonStateOutputs, &jsonStatePresent, &outputsPresent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound("no current state version")
	}
	if err != nil {
		return nil, fmt.Errorf("get current state version: %w", err)
	}

	fieldName := "json_state"
	blob := jsonState
	present := jsonStatePresent
	if outputs {
		fieldName = "json_state_outputs"
		blob = jsonStateOutputs
		present = outputsPresent
	}
	if !present || len(blob) == 0 {
		return nil, fmt.Errorf("current state version has no %s", fieldName)
	}
	return s.decryptStateField(&StateVersion{
		ID:          id,
		WorkspaceID: workspaceID,
		Lineage:     lineage,
	}, fieldName, blob)
}

// PlanStatePrune computes which finalized versions PruneState would delete.
// It is read-only.
func (s *Store) PlanStatePrune(org, workspace string, keeps int) (*StatePrunePlan, error) {
	organization, name, err := ParseStateReference(org + "/" + workspace)
	if err != nil {
		return nil, err
	}
	if keeps < 1 {
		return nil, errors.New("keeps must be at least 1")
	}
	ready, err := s.stateReadReady()
	if err != nil {
		return nil, err
	}
	if !ready {
		return nil, ErrNotFound("workspace not found")
	}

	ws, err := s.GetWorkspace(organization, name)
	if err != nil {
		return nil, err
	}
	if ws.Locked {
		return nil, ErrConflict("workspace is locked")
	}

	total, pending, err := stateVersionCounts(s.db, ws.ID)
	if err != nil {
		return nil, err
	}
	return buildStatePrunePlan(total, pending, keeps), nil
}

// PruneState deletes the oldest finalized versions beyond keeps in one
// transaction. Pending versions are never touched. A no-op prune succeeds
// without marking the backup dirty.
func (s *Store) PruneState(org, workspace string, keeps int) (*StatePrunePlan, error) {
	organization, name, err := ParseStateReference(org + "/" + workspace)
	if err != nil {
		return nil, err
	}
	if keeps < 1 {
		return nil, errors.New("keeps must be at least 1")
	}
	ready, err := s.stateReadReady()
	if err != nil {
		return nil, err
	}
	if !ready {
		return nil, ErrNotFound("workspace not found")
	}

	release, err := s.beginWrite()
	if err != nil {
		return nil, err
	}
	defer release()

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var wsID string
	var locked int
	err = tx.QueryRow(
		`SELECT id, locked FROM workspaces WHERE organization = ? AND name = ?`,
		organization, name,
	).Scan(&wsID, &locked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound("workspace not found")
	}
	if err != nil {
		return nil, err
	}
	if locked != 0 {
		return nil, ErrConflict("workspace is locked")
	}

	total, pending, err := stateVersionCounts(tx, wsID)
	if err != nil {
		return nil, err
	}
	plan := buildStatePrunePlan(total, pending, keeps)
	if plan.DeleteFinalized == 0 {
		return plan, nil
	}

	res, err := tx.Exec(
		`DELETE FROM state_versions WHERE id IN (
			SELECT id FROM state_versions
			WHERE workspace_id = ? AND status = 'finalized'
			ORDER BY rowid ASC LIMIT ?
		)`, wsID, plan.DeleteFinalized)
	if err != nil {
		return nil, fmt.Errorf("prune state versions: %w", err)
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if err := s.markDirty(); err != nil {
		return nil, err
	}

	plan.DeleteFinalized = int(deleted)
	plan.KeepFinalized = total - int(deleted)
	return plan, nil
}

// DeleteState removes one workspace and every state version it owns. Lineage
// keys are shared and deliberately preserved.
func (s *Store) DeleteState(org, workspace string) error {
	organization, name, err := ParseStateReference(org + "/" + workspace)
	if err != nil {
		return err
	}
	ready, err := s.stateReadReady()
	if err != nil {
		return err
	}
	if !ready {
		return ErrNotFound("workspace not found")
	}

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

	var wsID string
	var locked int
	err = tx.QueryRow(
		`SELECT id, locked FROM workspaces WHERE organization = ? AND name = ?`,
		organization, name,
	).Scan(&wsID, &locked)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound("workspace not found")
	}
	if err != nil {
		return err
	}
	if locked != 0 {
		return ErrConflict("workspace is locked")
	}

	if _, err := tx.Exec(`DELETE FROM state_versions WHERE workspace_id = ?`, wsID); err != nil {
		return fmt.Errorf("delete state versions: %w", err)
	}
	res, err := tx.Exec(`DELETE FROM workspaces WHERE id = ?`, wsID)
	if err != nil {
		return fmt.Errorf("delete workspace: %w", err)
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if deleted == 0 {
		return ErrNotFound("workspace not found")
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.markDirty()
}

// stateReadReady checks an existing secret schema because state and secret
// tables share one database, then reports whether the state tables exist.
func (s *Store) stateReadReady() (bool, error) {
	if err := requireCurrentSecretSchemaIfPresent(s.db); err != nil {
		return false, err
	}
	return stateTablesExist(s.db)
}

func requireCurrentSecretSchemaIfPresent(db *sql.DB) error {
	exists, err := secretTablesExist(db)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	return requireSecretSchema(db)
}

func stateTablesExist(db *sql.DB) (bool, error) {
	var count int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name IN ('workspaces', 'state_versions')`,
	).Scan(&count); err != nil {
		return false, fmt.Errorf("check state schema: %w", err)
	}
	return count == 2, nil
}

func stateVersionCounts(q rowQuerier, wsID string) (finalized int, pending int, err error) {
	err = q.QueryRow(
		`SELECT
			COUNT(CASE WHEN status = 'finalized' THEN 1 END),
			COUNT(CASE WHEN status = 'pending' THEN 1 END)
		FROM state_versions WHERE workspace_id = ?`, wsID,
	).Scan(&finalized, &pending)
	if err != nil {
		return 0, 0, fmt.Errorf("count state versions: %w", err)
	}
	return finalized, pending, nil
}

func buildStatePrunePlan(total, pending, keeps int) *StatePrunePlan {
	keep := keeps
	if keep > total {
		keep = total
	}
	remove := total - keep
	if remove < 0 {
		remove = 0
	}
	return &StatePrunePlan{
		TotalFinalized:  total,
		DeleteFinalized: remove,
		KeepFinalized:   keep,
		Pending:         pending,
	}
}
