package tacos

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/geektr-cloud/openmgcl/internal/sqlitebackup"
)

const (
	// MaxSecretNameBytes is the maximum length of a full Secret name, including
	// its separating slashes.
	MaxSecretNameBytes = 64
	// MaxSecretValueBytes is the maximum UTF-8 size of one KV value.
	MaxSecretValueBytes = 64 << 10
	// MaxSecretKVs is the maximum number of KV entries in one Secret.
	MaxSecretKVs = 64
	// MaxSecretTotalBytes is the maximum combined UTF-8 size of all values in
	// one Secret.
	MaxSecretTotalBytes = 256 << 10
)

var (
	// ErrSecretDatabaseNotFound reports a missing secret database. Read-only
	// commands never create it.
	ErrSecretDatabaseNotFound = errors.New("secret database not found")
	// ErrSecretNotFound reports an unknown Secret name.
	ErrSecretNotFound = errors.New("secret not found")
	// ErrSecretKeyNotFound reports an unknown KV key in an existing Secret.
	ErrSecretKeyNotFound = errors.New("secret key not found")
	// ErrSecretAlreadyExists reports a duplicate Secret name.
	ErrSecretAlreadyExists = errors.New("secret already exists")
	// ErrSecretRecipientMismatch reports that the loaded SSH identity did not
	// wrap the Secret DEK.
	ErrSecretRecipientMismatch = errors.New("secret key access denied")
	// ErrSecretCorrupt reports malformed or unauthenticated encrypted data.
	ErrSecretCorrupt = errors.New("secret data is corrupt")
	// ErrSecretInvalidValue reports non-UTF-8 or oversized input.
	ErrSecretInvalidValue = errors.New("invalid secret value")
	// ErrSecretInvalidBatch reports duplicate, overlapping, or structurally
	// invalid bulk KV input.
	ErrSecretInvalidBatch = errors.New("invalid secret batch")
	// ErrSecretLimitExceeded reports a KV count or total-size limit.
	ErrSecretLimitExceeded = errors.New("secret limit exceeded")
	// ErrSecretSchemaOutdated reports a database created before
	// secret_kvs.is_ephemeral existed. mgcl never migrates such a database
	// automatically.
	ErrSecretSchemaOutdated = errors.New("secret database schema is outdated: secret_kvs.is_ephemeral is missing; run mgcl-migrate to migrate this database")
)

var secretNameSegmentPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// SecretSummary is the plaintext metadata returned by ListSecrets.
type SecretSummary struct {
	Name      string
	CreatedAt string
	UpdatedAt string
	KeyCount  int
}

// SecretKeyMetadata is the plaintext metadata of one KV entry.
type SecretKeyMetadata struct {
	Key         string
	CreatedAt   string
	UpdatedAt   string
	IsEphemeral bool
}

// SecretDetails is the plaintext metadata of one Secret. It never contains a
// value, DEK, wrapped key, nonce or ciphertext.
type SecretDetails struct {
	ID        string
	Name      string
	CreatedAt string
	UpdatedAt string
	Keys      []SecretKeyMetadata
}

// SecretKVWrite is one upsert in a bulk write. IsEphemeral nil means true
// (ephemeral), matching the fail-closed provider API default.
type SecretKVWrite struct {
	Key         string
	Value       []byte
	IsEphemeral *bool
}

// SecretKVWriteResult is the applied metadata of one bulk upsert. It never
// contains a value.
type SecretKVWriteResult struct {
	Key         string
	IsEphemeral bool
}

// SecretStore is an offline SQLite handle for the secret tables. A read-only
// handle never migrates or creates the database and may omit the identity when
// only metadata is read.
type SecretStore struct {
	db       *sql.DB
	identity *Identity
	backup   *sqlitebackup.Manager

	writeMu      sync.RWMutex
	writesClosed bool

	// afterUnsetPreflight is a deterministic test hook between DEK preflight
	// and the delete transaction.
	afterUnsetPreflight func()

	closeOnce sync.Once
	closeErr  error
}

// ValidSecretName reports whether name is a hierarchical Secret name. A name
// is one or more slash-separated segments, each matching the v1 segment syntax,
// with a total length of at most MaxSecretNameBytes.
func ValidSecretName(name string) bool {
	if name == "" || len(name) > MaxSecretNameBytes {
		return false
	}
	for _, segment := range strings.Split(name, "/") {
		if !secretNameSegmentPattern.MatchString(segment) {
			return false
		}
	}
	return true
}

// ValidSecretKey reports whether key is a single KV key segment. Keys never
// contain a slash, even when the owning Secret name does.
func ValidSecretKey(key string) bool {
	return secretNameSegmentPattern.MatchString(key)
}

// ParseSecretReference splits a <secret-name>/<key> reference at its last
// slash. The name may contain multiple slash-separated segments; the key must
// be a single segment.
func ParseSecretReference(ref string) (name, key string, err error) {
	index := strings.LastIndex(ref, "/")
	if index < 0 {
		return "", "", errors.New("secret reference must be <secret-name>/<key>")
	}
	name, key = ref[:index], ref[index+1:]
	if !ValidSecretName(name) {
		return "", "", fmt.Errorf("invalid secret name %q", name)
	}
	if !ValidSecretKey(key) {
		return "", "", fmt.Errorf("invalid secret key %q", key)
	}
	return name, key, nil
}

// OpenSecretStore opens or creates the database and starts the backup manager.
// The identity must already be loaded and validated. Missing read-only paths
// must use OpenSecretStoreReadOnly instead.
func OpenSecretStore(path string, identity *Identity, backup *sqlitebackup.Manager) (*SecretStore, error) {
	if identity == nil {
		return nil, errors.New("encryption identity is required")
	}
	db, err := openSecretDB(path, false)
	if err != nil {
		return nil, err
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	store := &SecretStore{db: db, identity: identity, backup: backup}
	if backup != nil {
		if err := backup.Start(); err != nil {
			db.Close()
			return nil, fmt.Errorf("start backup: %w", err)
		}
	}
	return store, nil
}

// OpenSecretStoreReadOnly opens an existing database without creating or
// migrating it. identity may be nil for metadata-only commands.
func OpenSecretStoreReadOnly(path string, identity *Identity) (*SecretStore, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, ErrSecretDatabaseNotFound
		}
		return nil, fmt.Errorf("stat secret database: %w", err)
	}
	db, err := openSecretDB(path, true)
	if err != nil {
		return nil, err
	}
	return &SecretStore{db: db, identity: identity}, nil
}

func openSecretDB(path string, readOnly bool) (*sql.DB, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve secret database path: %w", err)
	}
	query := url.Values{
		"_pragma": {"busy_timeout(5000)"},
	}
	query.Add("_pragma", "foreign_keys(1)")
	if readOnly {
		query.Set("mode", "ro")
	} else {
		query.Set("_txlock", "immediate")
	}
	dsn := (&url.URL{Scheme: "file", Path: absPath, RawQuery: query.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open secret database: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		if readOnly && os.IsNotExist(err) {
			return nil, ErrSecretDatabaseNotFound
		}
		return nil, fmt.Errorf("open secret database: %w", err)
	}
	return db, nil
}

// Close closes the database and, for a write session, runs the backup shutdown
// sequence (drain, checkpoint, close, path copy).
func (s *SecretStore) Close() error {
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

// Abort closes the database without the path-copy shutdown sequence. It is used
// when a write session fails before accepting its first operation.
func (s *SecretStore) Abort() error {
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

// StopWrites closes the write gate and waits for in-flight writes.
func (s *SecretStore) StopWrites() {
	s.writeMu.Lock()
	s.writesClosed = true
	s.writeMu.Unlock()
}

func (s *SecretStore) beginWrite() (release func(), err error) {
	s.writeMu.RLock()
	if s.writesClosed {
		s.writeMu.RUnlock()
		return nil, errors.New("secret store is shutting down")
	}
	if s.backup != nil {
		if err := s.backup.Err(); err != nil {
			s.writeMu.RUnlock()
			return nil, fmt.Errorf("backup unavailable: %w", err)
		}
	}
	return s.writeMu.RUnlock, nil
}

func (s *SecretStore) markDirty() error {
	if s.backup == nil {
		return nil
	}
	return s.backup.MarkDirty()
}

// ListSecrets returns plaintext Secret metadata sorted by name.
func (s *SecretStore) ListSecrets() ([]SecretSummary, error) {
	exists, err := secretTablesExist(s.db)
	if err != nil {
		return nil, err
	}
	if !exists {
		return []SecretSummary{}, nil
	}
	if err := requireSecretSchema(s.db); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(
		`SELECT s.name, s.created_at, s.updated_at, COUNT(k.key)
		FROM secrets s LEFT JOIN secret_kvs k ON k.secret_id = s.id
		GROUP BY s.id ORDER BY s.name`)
	if err != nil {
		return nil, fmt.Errorf("list secrets: %w", err)
	}
	defer rows.Close()

	out := make([]SecretSummary, 0)
	for rows.Next() {
		var item SecretSummary
		if err := rows.Scan(&item.Name, &item.CreatedAt, &item.UpdatedAt, &item.KeyCount); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// ShowSecret returns the plaintext metadata of one Secret, including its KV
// keys but never their values.
func (s *SecretStore) ShowSecret(name string) (*SecretDetails, error) {
	if !ValidSecretName(name) {
		return nil, fmt.Errorf("invalid secret name %q", name)
	}
	exists, err := secretTablesExist(s.db)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrSecretNotFound
	}
	if err := requireSecretSchema(s.db); err != nil {
		return nil, err
	}

	tx, err := beginSecretReadTx(s.db)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var out SecretDetails
	err = tx.QueryRow(
		`SELECT id, name, created_at, updated_at FROM secrets WHERE name = ?`, name,
	).Scan(&out.ID, &out.Name, &out.CreatedAt, &out.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSecretNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get secret: %w", err)
	}

	rows, err := tx.Query(
		`SELECT key, created_at, updated_at, is_ephemeral FROM secret_kvs WHERE secret_id = ? ORDER BY key`,
		out.ID,
	)
	if err != nil {
		return nil, fmt.Errorf("list secret keys: %w", err)
	}
	out.Keys = make([]SecretKeyMetadata, 0)
	for rows.Next() {
		var key SecretKeyMetadata
		var isEphemeral int
		if err := rows.Scan(&key.Key, &key.CreatedAt, &key.UpdatedAt, &isEphemeral); err != nil {
			rows.Close()
			return nil, err
		}
		key.IsEphemeral = isEphemeral != 0
		out.Keys = append(out.Keys, key)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateSecret creates an empty Secret, its independent Secret DEK, and the
// wrapped key record in one transaction.
func (s *SecretStore) CreateSecret(name string) (*SecretDetails, error) {
	if !ValidSecretName(name) {
		return nil, fmt.Errorf("invalid secret name %q", name)
	}
	release, err := s.beginWrite()
	if err != nil {
		return nil, err
	}
	defer release()

	id, err := newSecretID()
	if err != nil {
		return nil, err
	}
	dek, record, err := newWrappedSecretDEK(s.identity, id)
	if err != nil {
		return nil, err
	}
	defer clearBytes(dek)

	now := nowISO()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	_, err = tx.Exec(
		`INSERT INTO secrets (id, name, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		id, name, now, now,
	)
	if err != nil {
		if isUniqueConstraint(err) {
			return nil, ErrSecretAlreadyExists
		}
		return nil, fmt.Errorf("create secret: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO secret_deks
		(secret_id, dek_fingerprint, recipient_fingerprint, suite, ephemeral_public_key, hkdf_salt, nonce, wrapped_dek, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, record.DEKFingerprint, record.RecipientFingerprint, record.Suite,
		record.EphemeralPublicKey, record.HKDFSalt, record.Nonce, record.WrappedDEK, now,
	); err != nil {
		return nil, fmt.Errorf("create secret key: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if err := s.markDirty(); err != nil {
		return nil, err
	}
	return &SecretDetails{
		ID:        id,
		Name:      name,
		CreatedAt: now,
		UpdatedAt: now,
		Keys:      []SecretKeyMetadata{},
	}, nil
}

// SetSecretValue writes or overwrites one KV. The Secret must already exist and
// its DEK must unwrap with the current recipient. The Secret's visibility is
// unchanged.
func (s *SecretStore) SetSecretValue(name, key string, value []byte) error {
	return s.SetSecretValueWithPublic(name, key, value, false)
}

// SetSecretValueWithPublic writes or overwrites one KV. When public is true,
// only that KV is marked public (is_ephemeral = 0) in the same transaction as
// the value write; other KVs keep their marks. When public is false, the KV
// keeps its existing mark, and a new KV defaults to ephemeral. The Secret must
// already exist and its DEK must unwrap with the current recipient before any
// row is modified.
func (s *SecretStore) SetSecretValueWithPublic(name, key string, value []byte, public bool) error {
	if !ValidSecretName(name) {
		return fmt.Errorf("invalid secret name %q", name)
	}
	if !ValidSecretKey(key) {
		return fmt.Errorf("invalid secret key %q", key)
	}
	if !utf8.Valid(value) {
		return fmt.Errorf("%w: value must be valid UTF-8", ErrSecretInvalidValue)
	}
	if len(value) > MaxSecretValueBytes {
		return fmt.Errorf("%w: value exceeds %d bytes", ErrSecretInvalidValue, MaxSecretValueBytes)
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

	secretID, dek, err := unlockSecretDEKTx(tx, s.identity, name)
	if err != nil {
		return err
	}
	defer clearBytes(dek)

	count, total, exists, err := secretKVUsageTx(tx, secretID, key, len(value))
	if err != nil {
		return err
	}
	if !exists {
		count++
	}
	if count > MaxSecretKVs {
		return fmt.Errorf("%w: a secret may contain at most %d keys", ErrSecretLimitExceeded, MaxSecretKVs)
	}
	if total > MaxSecretTotalBytes {
		return fmt.Errorf("%w: values exceed %d bytes", ErrSecretLimitExceeded, MaxSecretTotalBytes)
	}

	nonce, ciphertext, err := sealSecretValue(dek, secretID, key, value)
	if err != nil {
		return err
	}
	now := nowISO()
	ephemeralMark := 1
	if public {
		ephemeralMark = 0
	}
	if exists {
		if public {
			if _, err := tx.Exec(
				`UPDATE secret_kvs SET suite = ?, nonce = ?, ciphertext = ?, updated_at = ?, is_ephemeral = 0
				WHERE secret_id = ? AND key = ?`,
				cryptSuite, nonce, ciphertext, now, secretID, key,
			); err != nil {
				return fmt.Errorf("update secret value: %w", err)
			}
		} else if _, err := tx.Exec(
			`UPDATE secret_kvs SET suite = ?, nonce = ?, ciphertext = ?, updated_at = ?
			WHERE secret_id = ? AND key = ?`,
			cryptSuite, nonce, ciphertext, now, secretID, key,
		); err != nil {
			return fmt.Errorf("update secret value: %w", err)
		}
	} else {
		if _, err := tx.Exec(
			`INSERT INTO secret_kvs (secret_id, key, suite, nonce, ciphertext, is_ephemeral, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			secretID, key, cryptSuite, nonce, ciphertext, ephemeralMark, now, now,
		); err != nil {
			return fmt.Errorf("insert secret value: %w", err)
		}
	}
	if _, err := tx.Exec(`UPDATE secrets SET updated_at = ? WHERE id = ?`, now, secretID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.markDirty()
}

// GetSecretValue decrypts and returns the raw UTF-8 value bytes.
func (s *SecretStore) GetSecretValue(name, key string) ([]byte, error) {
	if !ValidSecretName(name) || !ValidSecretKey(key) {
		return nil, errors.New("invalid secret reference")
	}
	if s.identity == nil {
		return nil, errors.New("encryption identity is required")
	}
	exists, err := secretTablesExist(s.db)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrSecretNotFound
	}
	if err := requireSecretSchema(s.db); err != nil {
		return nil, err
	}

	tx, err := beginSecretReadTx(s.db)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	secretID, dek, err := unlockSecretDEKTx(tx, s.identity, name)
	if err != nil {
		return nil, err
	}
	defer clearBytes(dek)

	var nonce, ciphertext []byte
	var suite int
	err = tx.QueryRow(
		`SELECT suite, nonce, ciphertext FROM secret_kvs WHERE secret_id = ? AND key = ?`,
		secretID, key,
	).Scan(&suite, &nonce, &ciphertext)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSecretKeyNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get secret value: %w", err)
	}
	if suite != cryptSuite {
		return nil, fmt.Errorf("%w: unsupported secret value suite", ErrSecretCorrupt)
	}
	return openSecretValue(dek, secretID, key, nonce, ciphertext)
}

// UnsetSecretValue removes one KV after successfully unwrapping the Secret DEK.
func (s *SecretStore) UnsetSecretValue(name, key string) error {
	if !ValidSecretName(name) || !ValidSecretKey(key) {
		return errors.New("invalid secret reference")
	}
	secretID, _, err := s.preflightSecret(name)
	if err != nil {
		return err
	}
	if s.afterUnsetPreflight != nil {
		s.afterUnsetPreflight()
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

	res, err := tx.Exec(`DELETE FROM secret_kvs WHERE secret_id = ? AND key = ?`, secretID, key)
	if err != nil {
		return fmt.Errorf("delete secret value: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrSecretKeyNotFound
	}
	if _, err := tx.Exec(`UPDATE secrets SET updated_at = ? WHERE id = ?`, nowISO(), secretID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.markDirty()
}

// DeleteSecret removes one Secret and cascades to its DEK and KVs. confirm, if
// non-nil, is called after the DEK unwraps and before the delete transaction;
// returning false cancels without modifying the database.
func (s *SecretStore) DeleteSecret(name string, confirm func(keyCount int) (bool, error)) error {
	if !ValidSecretName(name) {
		return fmt.Errorf("invalid secret name %q", name)
	}
	secretID, keyCount, err := s.preflightSecret(name)
	if err != nil {
		return err
	}
	if confirm != nil {
		ok, err := confirm(keyCount)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("secret deletion cancelled")
		}
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

	res, err := tx.Exec(`DELETE FROM secrets WHERE id = ? AND name = ?`, secretID, name)
	if err != nil {
		return fmt.Errorf("delete secret: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrSecretNotFound
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.markDirty()
}

func (s *SecretStore) preflightSecret(name string) (string, int, error) {
	exists, err := secretTablesExist(s.db)
	if err != nil {
		return "", 0, err
	}
	if !exists {
		return "", 0, ErrSecretNotFound
	}
	if s.identity == nil {
		return "", 0, errors.New("encryption identity is required")
	}
	tx, err := beginSecretReadTx(s.db)
	if err != nil {
		return "", 0, err
	}
	defer tx.Rollback()

	secretID, dek, err := unlockSecretDEKTx(tx, s.identity, name)
	if err != nil {
		return "", 0, err
	}
	clearBytes(dek)
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM secret_kvs WHERE secret_id = ?`, secretID).Scan(&count); err != nil {
		return "", 0, err
	}
	return secretID, count, nil
}

// ReadSecretValues returns the current values of every KV in one Secret.
func (s *SecretStore) ReadSecretValues(name string) (map[string]string, error) {
	values, _, err := readSecretValues(context.Background(), s.db, s.identity, name, true)
	return values, err
}

// ReadSecretValues returns the current values of every KV in one Secret.
func (s *Store) ReadSecretValues(name string) (map[string]string, error) {
	values, _, err := readSecretValues(context.Background(), s.db, s.identity, name, true)
	return values, err
}

// ReadSecretValuesContext reads every KV of one Secret with the caller's
// request context. The provider API uses this so a canceled request also
// cancels its read snapshot.
func (s *Store) ReadSecretValuesContext(ctx context.Context, name string) (map[string]string, error) {
	values, _, err := readSecretValues(ctx, s.db, s.identity, name, true)
	return values, err
}

// ReadSecretValuesContextAccess reads one Secret's KVs and their access marks.
// allKeys true returns every KV; false returns only KVs with is_ephemeral = 0
// and never selects or decrypts ephemeral KVs. It returns empty maps when no
// KV matches.
func (s *Store) ReadSecretValuesContextAccess(ctx context.Context, name string, allKeys bool) (map[string]string, map[string]bool, error) {
	return readSecretValues(ctx, s.db, s.identity, name, allKeys)
}

func readSecretValues(ctx context.Context, db *sql.DB, identity *Identity, name string, allKeys bool) (map[string]string, map[string]bool, error) {
	if !ValidSecretName(name) {
		return nil, nil, errors.New("invalid secret name")
	}
	if identity == nil {
		return nil, nil, errors.New("encryption identity is required")
	}
	exists, err := secretTablesExist(db)
	if err != nil {
		return nil, nil, err
	}
	if !exists {
		return nil, nil, ErrSecretNotFound
	}
	if err := requireSecretSchema(db); err != nil {
		return nil, nil, err
	}

	tx, err := beginSecretReadTxContext(ctx, db)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()

	secretID, err := secretIDTx(tx, name)
	if err != nil {
		return nil, nil, err
	}
	dek, err := unlockSecretDEKByIDTx(tx, identity, secretID)
	if err != nil {
		return nil, nil, err
	}
	defer clearBytes(dek)

	query := `SELECT key, suite, nonce, ciphertext, is_ephemeral FROM secret_kvs WHERE secret_id = ?`
	if !allKeys {
		query += ` AND is_ephemeral = 0`
	}
	query += ` ORDER BY key`
	rows, err := tx.QueryContext(ctx, query, secretID)
	if err != nil {
		return nil, nil, fmt.Errorf("read secret values: %w", err)
	}
	defer rows.Close()

	values := make(map[string]string)
	marks := make(map[string]bool)
	for rows.Next() {
		var key string
		var suite int
		var nonce, ciphertext []byte
		var isEphemeral int
		if err := rows.Scan(&key, &suite, &nonce, &ciphertext, &isEphemeral); err != nil {
			return nil, nil, err
		}
		if suite != cryptSuite {
			return nil, nil, fmt.Errorf("%w: unsupported secret value suite", ErrSecretCorrupt)
		}
		plaintext, err := openSecretValue(dek, secretID, key, nonce, ciphertext)
		if err != nil {
			return nil, nil, err
		}
		values[key] = string(plaintext)
		marks[key] = isEphemeral != 0
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return values, marks, nil
}

func unlockSecretDEKTx(tx *sql.Tx, identity *Identity, name string) (string, []byte, error) {
	secretID, err := secretIDTx(tx, name)
	if err != nil {
		return "", nil, err
	}
	dek, err := unlockSecretDEKByIDTx(tx, identity, secretID)
	if err != nil {
		return "", nil, err
	}
	return secretID, dek, nil
}

func unlockSecretDEKByIDTx(tx *sql.Tx, identity *Identity, secretID string) ([]byte, error) {
	record := &wrappedSecretDEKRecord{}
	err := tx.QueryRow(
		`SELECT dek_fingerprint, recipient_fingerprint, suite, ephemeral_public_key, hkdf_salt, nonce, wrapped_dek
		FROM secret_deks WHERE secret_id = ?`, secretID,
	).Scan(
		&record.DEKFingerprint, &record.RecipientFingerprint, &record.Suite,
		&record.EphemeralPublicKey, &record.HKDFSalt, &record.Nonce, &record.WrappedDEK,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: secret key metadata is missing", ErrSecretCorrupt)
	}
	if err != nil {
		return nil, err
	}
	return unwrapSecretDEK(identity, secretID, record)
}

func secretIDTx(tx *sql.Tx, name string) (string, error) {
	var id string
	err := tx.QueryRow(`SELECT id FROM secrets WHERE name = ?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrSecretNotFound
	}
	if err != nil {
		return "", err
	}
	return id, nil
}

// secretKVUsageTx reports the number of KVs, the total plaintext bytes after
// replacing the target key, and whether the key already exists.
func secretKVUsageTx(tx *sql.Tx, secretID, key string, newSize int) (count int, total int, exists bool, err error) {
	rows, err := tx.Query(`SELECT key, length(ciphertext) FROM secret_kvs WHERE secret_id = ?`, secretID)
	if err != nil {
		return 0, 0, false, err
	}
	defer rows.Close()

	for rows.Next() {
		var rowKey string
		var ciphertextLen int64
		if err := rows.Scan(&rowKey, &ciphertextLen); err != nil {
			return 0, 0, false, err
		}
		if ciphertextLen < aeadOverhead {
			return 0, 0, false, fmt.Errorf("%w: secret value envelope is too short", ErrSecretCorrupt)
		}
		count++
		if rowKey == key {
			exists = true
			total += newSize
			continue
		}
		total += int(ciphertextLen) - aeadOverhead
	}
	if err := rows.Err(); err != nil {
		return 0, 0, false, err
	}
	if !exists {
		total += newSize
	}
	return count, total, exists, nil
}

func beginSecretReadTx(db *sql.DB) (*sql.Tx, error) {
	return beginSecretReadTxContext(context.Background(), db)
}

func beginSecretReadTxContext(ctx context.Context, db *sql.DB) (*sql.Tx, error) {
	return db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
}

func secretTablesExist(db *sql.DB) (bool, error) {
	var name string
	err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'secrets'`).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// rowQuerier is satisfied by *sql.DB and *sql.Tx.
type rowQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

// requireSecretSchema verifies that the existing secret_kvs table has the
// is_ephemeral column. It never alters the database. Callers that accept an
// empty database must check secretTablesExist first.
func requireSecretSchema(q rowQuerier) error {
	var count int
	if err := q.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('secret_kvs') WHERE name = 'is_ephemeral'`,
	).Scan(&count); err != nil {
		return fmt.Errorf("check secret schema: %w", err)
	}
	if count == 0 {
		return ErrSecretSchemaOutdated
	}
	return nil
}

func isUniqueConstraint(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE")
}

// WriteSecretKVContext creates or overwrites one KV and, when the Secret does
// not exist yet, creates the Secret and its independent DEK in the same
// transaction. isEphemeral sets only this KV's access mark. The provider API
// uses this entry point for managed-resource writes.
func (s *Store) WriteSecretKVContext(ctx context.Context, ref string, value []byte, isEphemeral bool) (string, string, error) {
	name, key, err := ParseSecretReference(ref)
	if err != nil {
		return "", "", err
	}
	if err := requireSecretSchema(s.db); err != nil {
		return "", "", err
	}
	if !utf8.Valid(value) {
		return "", "", fmt.Errorf("%w: value must be valid UTF-8", ErrSecretInvalidValue)
	}
	if len(value) > MaxSecretValueBytes {
		return "", "", fmt.Errorf("%w: value exceeds %d bytes", ErrSecretInvalidValue, MaxSecretValueBytes)
	}

	release, err := s.beginWrite()
	if err != nil {
		return "", "", err
	}
	defer release()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()

	now := nowISO()
	var secretID string
	var dek []byte
	err = tx.QueryRowContext(ctx, `SELECT id FROM secrets WHERE name = ?`, name).Scan(&secretID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		secretID, err = newSecretID()
		if err != nil {
			return "", "", err
		}
		var record *wrappedSecretDEKRecord
		dek, record, err = newWrappedSecretDEK(s.identity, secretID)
		if err != nil {
			return "", "", err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO secrets (id, name, created_at, updated_at) VALUES (?, ?, ?, ?)`,
			secretID, name, now, now,
		); err != nil {
			return "", "", fmt.Errorf("create secret: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO secret_deks
			(secret_id, dek_fingerprint, recipient_fingerprint, suite, ephemeral_public_key, hkdf_salt, nonce, wrapped_dek, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			secretID, record.DEKFingerprint, record.RecipientFingerprint, record.Suite,
			record.EphemeralPublicKey, record.HKDFSalt, record.Nonce, record.WrappedDEK, now,
		); err != nil {
			return "", "", fmt.Errorf("create secret key: %w", err)
		}
	case err != nil:
		return "", "", err
	default:
		dek, err = unlockSecretDEKByIDTx(tx, s.identity, secretID)
		if err != nil {
			return "", "", err
		}
	}
	defer clearBytes(dek)

	count, total, exists, err := secretKVUsageTx(tx, secretID, key, len(value))
	if err != nil {
		return "", "", err
	}
	if !exists {
		count++
	}
	if count > MaxSecretKVs {
		return "", "", fmt.Errorf("%w: a secret may contain at most %d keys", ErrSecretLimitExceeded, MaxSecretKVs)
	}
	if total > MaxSecretTotalBytes {
		return "", "", fmt.Errorf("%w: values exceed %d bytes", ErrSecretLimitExceeded, MaxSecretTotalBytes)
	}

	nonce, ciphertext, err := sealSecretValue(dek, secretID, key, value)
	if err != nil {
		return "", "", err
	}
	mark := 1
	if !isEphemeral {
		mark = 0
	}
	if exists {
		if _, err := tx.ExecContext(ctx,
			`UPDATE secret_kvs SET suite = ?, nonce = ?, ciphertext = ?, is_ephemeral = ?, updated_at = ?
			WHERE secret_id = ? AND key = ?`,
			cryptSuite, nonce, ciphertext, mark, now, secretID, key,
		); err != nil {
			return "", "", fmt.Errorf("update secret value: %w", err)
		}
	} else {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO secret_kvs (secret_id, key, suite, nonce, ciphertext, is_ephemeral, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			secretID, key, cryptSuite, nonce, ciphertext, mark, now, now,
		); err != nil {
			return "", "", fmt.Errorf("insert secret value: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE secrets SET updated_at = ? WHERE id = ?`, now, secretID); err != nil {
		return "", "", err
	}
	if err := tx.Commit(); err != nil {
		return "", "", err
	}
	if err := s.markDirty(); err != nil {
		return "", "", err
	}
	return name, key, nil
}

// DeleteSecretKVContext deletes one KV. A missing Secret or KV succeeds
// without creating anything, and the Secret (including an empty Secret) is
// never deleted. No DEK unwrap is needed.
func (s *Store) DeleteSecretKVContext(ctx context.Context, ref string) (string, string, error) {
	name, key, err := ParseSecretReference(ref)
	if err != nil {
		return "", "", err
	}
	if err := requireSecretSchema(s.db); err != nil {
		return "", "", err
	}

	release, err := s.beginWrite()
	if err != nil {
		return "", "", err
	}
	defer release()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()

	var secretID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM secrets WHERE name = ?`, name).Scan(&secretID)
	if errors.Is(err, sql.ErrNoRows) {
		return name, key, nil
	}
	if err != nil {
		return "", "", err
	}

	res, err := tx.ExecContext(ctx, `DELETE FROM secret_kvs WHERE secret_id = ? AND key = ?`, secretID, key)
	if err != nil {
		return "", "", fmt.Errorf("delete secret value: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", "", err
	}
	changed := n > 0
	if changed {
		if _, err := tx.ExecContext(ctx, `UPDATE secrets SET updated_at = ? WHERE id = ?`, nowISO(), secretID); err != nil {
			return "", "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", "", err
	}
	if changed {
		if err := s.markDirty(); err != nil {
			return "", "", err
		}
	}
	return name, key, nil
}

// WriteSecretKVsContext applies one atomic batch of KV upserts and idempotent
// deletes to a hierarchical Secret name. A missing Secret is created with its
// DEK when ensure is true or at least one KV is upserted; a pure delete against
// a missing Secret succeeds without creating anything. KVs that are not named
// in the batch are preserved.
func (s *Store) WriteSecretKVsContext(ctx context.Context, name string, kvs []SecretKVWrite, deleteKeys []string, ensure bool) ([]SecretKVWriteResult, error) {
	if !ValidSecretName(name) {
		return nil, fmt.Errorf("%w: invalid secret name %q", ErrSecretInvalidBatch, name)
	}
	if len(kvs) > MaxSecretKVs {
		return nil, fmt.Errorf("%w: a secret may contain at most %d keys", ErrSecretLimitExceeded, MaxSecretKVs)
	}
	if len(deleteKeys) > MaxSecretKVs {
		return nil, fmt.Errorf("%w: a batch may delete at most %d keys", ErrSecretLimitExceeded, MaxSecretKVs)
	}

	upserted := make(map[string]struct{}, len(kvs))
	upsertBytes := 0
	for _, item := range kvs {
		if !ValidSecretKey(item.Key) {
			return nil, fmt.Errorf("%w: invalid secret key %q", ErrSecretInvalidBatch, item.Key)
		}
		if _, ok := upserted[item.Key]; ok {
			return nil, fmt.Errorf("%w: duplicate secret key %q", ErrSecretInvalidBatch, item.Key)
		}
		upserted[item.Key] = struct{}{}
		if !utf8.Valid(item.Value) {
			return nil, fmt.Errorf("%w: value must be valid UTF-8", ErrSecretInvalidValue)
		}
		if len(item.Value) > MaxSecretValueBytes {
			return nil, fmt.Errorf("%w: value exceeds %d bytes", ErrSecretInvalidValue, MaxSecretValueBytes)
		}
		upsertBytes += len(item.Value)
	}
	if upsertBytes > MaxSecretTotalBytes {
		return nil, fmt.Errorf("%w: values exceed %d bytes", ErrSecretLimitExceeded, MaxSecretTotalBytes)
	}

	deleted := make(map[string]struct{}, len(deleteKeys))
	for _, key := range deleteKeys {
		if !ValidSecretKey(key) {
			return nil, fmt.Errorf("%w: invalid secret key %q", ErrSecretInvalidBatch, key)
		}
		if _, ok := deleted[key]; ok {
			return nil, fmt.Errorf("%w: duplicate delete key %q", ErrSecretInvalidBatch, key)
		}
		if _, ok := upserted[key]; ok {
			return nil, fmt.Errorf("%w: key %q is both upserted and deleted", ErrSecretInvalidBatch, key)
		}
		deleted[key] = struct{}{}
	}

	if err := requireSecretSchema(s.db); err != nil {
		return nil, err
	}

	release, err := s.beginWrite()
	if err != nil {
		return nil, err
	}
	defer release()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	now := nowISO()
	changed := false
	var secretID string
	var dek []byte
	err = tx.QueryRowContext(ctx, `SELECT id FROM secrets WHERE name = ?`, name).Scan(&secretID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if !ensure && len(kvs) == 0 {
			// Pure delete or no-op against a missing Secret: success without
			// creating a Secret or DEK.
			return []SecretKVWriteResult{}, nil
		}
		secretID, err = newSecretID()
		if err != nil {
			return nil, err
		}
		var record *wrappedSecretDEKRecord
		dek, record, err = newWrappedSecretDEK(s.identity, secretID)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO secrets (id, name, created_at, updated_at) VALUES (?, ?, ?, ?)`,
			secretID, name, now, now,
		); err != nil {
			return nil, fmt.Errorf("create secret: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO secret_deks
			(secret_id, dek_fingerprint, recipient_fingerprint, suite, ephemeral_public_key, hkdf_salt, nonce, wrapped_dek, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			secretID, record.DEKFingerprint, record.RecipientFingerprint, record.Suite,
			record.EphemeralPublicKey, record.HKDFSalt, record.Nonce, record.WrappedDEK, now,
		); err != nil {
			return nil, fmt.Errorf("create secret key: %w", err)
		}
		changed = true
	case err != nil:
		return nil, err
	default:
		dek, err = unlockSecretDEKByIDTx(tx, s.identity, secretID)
		if err != nil {
			return nil, err
		}
	}
	defer clearBytes(dek)

	existing := make(map[string]int)
	rows, err := tx.QueryContext(ctx, `SELECT key, length(ciphertext) FROM secret_kvs WHERE secret_id = ?`, secretID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var key string
		var ciphertextLen int64
		if err := rows.Scan(&key, &ciphertextLen); err != nil {
			rows.Close()
			return nil, err
		}
		if ciphertextLen < aeadOverhead {
			rows.Close()
			return nil, fmt.Errorf("%w: secret value envelope is too short", ErrSecretCorrupt)
		}
		existing[key] = int(ciphertextLen) - aeadOverhead
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	final := make(map[string]int, len(existing)+len(kvs))
	for key, size := range existing {
		final[key] = size
	}
	for key := range deleted {
		delete(final, key)
	}
	for _, item := range kvs {
		final[item.Key] = len(item.Value)
	}
	if len(final) > MaxSecretKVs {
		return nil, fmt.Errorf("%w: a secret may contain at most %d keys", ErrSecretLimitExceeded, MaxSecretKVs)
	}
	total := 0
	for _, size := range final {
		total += size
	}
	if total > MaxSecretTotalBytes {
		return nil, fmt.Errorf("%w: values exceed %d bytes", ErrSecretLimitExceeded, MaxSecretTotalBytes)
	}

	for _, key := range deleteKeys {
		res, err := tx.ExecContext(ctx, `DELETE FROM secret_kvs WHERE secret_id = ? AND key = ?`, secretID, key)
		if err != nil {
			return nil, fmt.Errorf("delete secret value: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, err
		}
		if n > 0 {
			changed = true
		}
	}

	results := make([]SecretKVWriteResult, 0, len(kvs))
	for _, item := range kvs {
		mark := 1
		if item.IsEphemeral != nil && !*item.IsEphemeral {
			mark = 0
		}
		nonce, ciphertext, err := sealSecretValue(dek, secretID, item.Key, item.Value)
		if err != nil {
			return nil, err
		}
		if _, ok := existing[item.Key]; ok {
			if _, err := tx.ExecContext(ctx,
				`UPDATE secret_kvs SET suite = ?, nonce = ?, ciphertext = ?, is_ephemeral = ?, updated_at = ?
				WHERE secret_id = ? AND key = ?`,
				cryptSuite, nonce, ciphertext, mark, now, secretID, item.Key,
			); err != nil {
				return nil, fmt.Errorf("update secret value: %w", err)
			}
		} else {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO secret_kvs (secret_id, key, suite, nonce, ciphertext, is_ephemeral, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				secretID, item.Key, cryptSuite, nonce, ciphertext, mark, now, now,
			); err != nil {
				return nil, fmt.Errorf("insert secret value: %w", err)
			}
		}
		results = append(results, SecretKVWriteResult{Key: item.Key, IsEphemeral: mark == 1})
		changed = true
	}

	if changed {
		if _, err := tx.ExecContext(ctx, `UPDATE secrets SET updated_at = ? WHERE id = ?`, now, secretID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if changed {
		if err := s.markDirty(); err != nil {
			return nil, err
		}
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Key < results[j].Key })
	return results, nil
}

// newSecretID returns a random UUID v4.
func newSecretID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate secret id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
