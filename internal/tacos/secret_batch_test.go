package tacos

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/geektr-cloud/openmgcl/internal/sqlitebackup"
)

func newBatchStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "mgcl.db"), testIdentity(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func secretBool(value bool) *bool { return &value }

func secretRowCount(t *testing.T, store *Store, name string) int {
	t.Helper()
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM secrets WHERE name = ?`, name).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestSecretHierarchicalStoreCRUD(t *testing.T) {
	store := openTestSecretStore(t, filepath.Join(t.TempDir(), "mgcl.db"), testIdentity(t))
	name := "aws-605592676484-cn/scarborough"

	if _, err := store.CreateSecret(name); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSecretValueWithPublic(name, "region", []byte("us-east-1"), true); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSecretValue(name, "token", []byte("value")); err != nil {
		t.Fatal(err)
	}
	if got, err := store.GetSecretValue(name, "region"); err != nil || string(got) != "us-east-1" {
		t.Fatalf("region = %q, %v", got, err)
	}
	values, err := store.ReadSecretValues(name)
	if err != nil {
		t.Fatal(err)
	}
	if values["region"] != "us-east-1" || values["token"] != "value" {
		t.Fatalf("values = %#v", values)
	}

	list, err := store.ListSecrets()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != name || list[0].KeyCount != 2 {
		t.Fatalf("list = %+v", list)
	}
	details, err := store.ShowSecret(name)
	if err != nil {
		t.Fatal(err)
	}
	if details.Name != name || len(details.Keys) != 2 {
		t.Fatalf("details = %+v", details)
	}
	marks := map[string]bool{}
	for _, key := range details.Keys {
		marks[key.Key] = key.IsEphemeral
	}
	if marks["region"] || !marks["token"] {
		t.Fatalf("marks = %#v", marks)
	}

	// A nested name remains a distinct Secret from a name that is its prefix.
	if _, err := store.CreateSecret("aws-605592676484-cn"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSecretValue("aws-605592676484-cn", "region", []byte("parent")); err != nil {
		t.Fatal(err)
	}
	if got, err := store.GetSecretValue(name, "region"); err != nil || string(got) != "us-east-1" {
		t.Fatalf("nested region changed: %q, %v", got, err)
	}

	// Complete references split at the last slash.
	parsedName, parsedKey, err := ParseSecretReference("aws-605592676484-cn/scarborough/zone")
	if err != nil || parsedName != name || parsedKey != "zone" {
		t.Fatalf("parse = %q/%q, %v", parsedName, parsedKey, err)
	}
	if err := store.SetSecretValue(name, "zone", []byte("a")); err != nil {
		t.Fatal(err)
	}

	if err := store.UnsetSecretValue(name, "token"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteSecret(name, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ShowSecret(name); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("show after delete = %v", err)
	}
}

func TestSecretBatchAutoCreateAndPreserve(t *testing.T) {
	store := newBatchStore(t)
	ctx := context.Background()
	name := "apps/prod/database"

	// A non-empty batch creates the Secret, DEK and all KVs atomically even
	// when ensure is false. A nil mark defaults to ephemeral.
	results, err := store.WriteSecretKVsContext(ctx, name, []SecretKVWrite{
		{Key: "host", Value: []byte("db.internal")},
		{Key: "password", Value: []byte("s3cr3t"), IsEphemeral: secretBool(false)},
	}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].Key != "host" || !results[0].IsEphemeral || results[1].Key != "password" || results[1].IsEphemeral {
		t.Fatalf("results = %+v", results)
	}
	var secrets, deks int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM secrets`).Scan(&secrets); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM secret_deks`).Scan(&deks); err != nil {
		t.Fatal(err)
	}
	if secrets != 1 || deks != 1 {
		t.Fatalf("secrets/deks = %d/%d", secrets, deks)
	}

	// A later batch deletes one KV, updates another and preserves everything
	// else that it does not mention.
	if _, _, err := store.WriteSecretKVContext(ctx, name+"/unmentioned", []byte("keep"), true); err != nil {
		t.Fatal(err)
	}
	results, err = store.WriteSecretKVsContext(ctx, name, []SecretKVWrite{
		{Key: "host", Value: []byte("new.internal"), IsEphemeral: secretBool(false)},
		{Key: "added", Value: []byte("added")},
	}, []string{"password", "missing"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].Key != "added" || results[1].Key != "host" || results[1].IsEphemeral {
		t.Fatalf("results = %+v", results)
	}
	values, marks, err := store.ReadSecretValuesContextAccess(ctx, name, true)
	if err != nil {
		t.Fatal(err)
	}
	if values["host"] != "new.internal" || values["unmentioned"] != "keep" || values["added"] != "added" {
		t.Fatalf("values = %#v", values)
	}
	if _, ok := values["password"]; ok {
		t.Fatal("deleted KV is still present")
	}
	if marks["host"] || !marks["added"] {
		t.Fatalf("marks = %#v", marks)
	}

	// ensure=true creates an empty Secret with a DEK.
	emptyName := "apps/prod/empty"
	results, err = store.WriteSecretKVsContext(ctx, emptyName, nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("empty ensure results = %+v", results)
	}
	if secretRowCount(t, store, emptyName) != 1 {
		t.Fatal("ensure did not create the empty Secret")
	}
	var emptyKeys int
	if err := store.db.QueryRow(
		`SELECT COUNT(*) FROM secret_kvs WHERE secret_id = (SELECT id FROM secrets WHERE name = ?)`, emptyName,
	).Scan(&emptyKeys); err != nil {
		t.Fatal(err)
	}
	if emptyKeys != 0 {
		t.Fatalf("empty ensure keys = %d", emptyKeys)
	}

	// A pure delete against a missing Secret succeeds without creating it.
	missing := "apps/prod/missing"
	if results, err := store.WriteSecretKVsContext(ctx, missing, nil, []string{"old"}, false); err != nil {
		t.Fatalf("pure delete: %v", err)
	} else if len(results) != 0 {
		t.Fatalf("pure delete results = %+v", results)
	}
	// A no-op batch also succeeds without creating it.
	if _, err := store.WriteSecretKVsContext(ctx, missing, nil, nil, false); err != nil {
		t.Fatalf("no-op: %v", err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM secrets WHERE name = ?`, missing).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("a pure delete or no-op created a missing Secret")
	}

	// ensure=true on an existing empty Secret is a no-op, not a duplicate.
	if _, err := store.WriteSecretKVsContext(ctx, emptyName, nil, nil, true); err != nil {
		t.Fatal(err)
	}
}

func TestSecretBatchValidation(t *testing.T) {
	store := newBatchStore(t)
	ctx := context.Background()
	full := bytes.Repeat([]byte("x"), MaxSecretValueBytes+1)
	tooManyDeletes := make([]string, 0, MaxSecretKVs+1)
	for i := 0; i < MaxSecretKVs+1; i++ {
		tooManyDeletes = append(tooManyDeletes, fmt.Sprintf("d%02d", i))
	}

	cases := []struct {
		name       string
		secretName string
		kvs        []SecretKVWrite
		deleteKeys []string
		ensure     bool
		want       error
	}{
		{"invalid name", "a//b", nil, nil, true, ErrSecretInvalidBatch},
		{"name too long", strings.Repeat("a", 65), nil, nil, true, ErrSecretInvalidBatch},
		{"key with slash", "valid", []SecretKVWrite{{Key: "a/b", Value: []byte("v")}}, nil, true, ErrSecretInvalidBatch},
		{"duplicate upsert", "valid", []SecretKVWrite{{Key: "a", Value: []byte("1")}, {Key: "a", Value: []byte("2")}}, nil, true, ErrSecretInvalidBatch},
		{"duplicate delete", "valid", nil, []string{"a", "a"}, true, ErrSecretInvalidBatch},
		{"overlap", "valid", []SecretKVWrite{{Key: "a", Value: []byte("1")}}, []string{"a"}, true, ErrSecretInvalidBatch},
		{"too many kvs", "valid", make([]SecretKVWrite, MaxSecretKVs+1), nil, true, ErrSecretLimitExceeded},
		{"too many delete keys", "valid", nil, tooManyDeletes, true, ErrSecretLimitExceeded},
		{"invalid utf8", "valid", []SecretKVWrite{{Key: "a", Value: []byte{0xff}}}, nil, true, ErrSecretInvalidValue},
		{"oversized value", "valid", []SecretKVWrite{{Key: "a", Value: full}}, nil, true, ErrSecretInvalidValue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "too many kvs" {
				for i := range tc.kvs {
					tc.kvs[i] = SecretKVWrite{Key: fmt.Sprintf("k%02d", i), Value: []byte("v")}
				}
			}
			_, err := store.WriteSecretKVsContext(ctx, tc.secretName, tc.kvs, tc.deleteKeys, tc.ensure)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}

	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM secrets`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("validation failure created %d Secrets", count)
	}
}

func TestSecretBatchQuotaUsesFinalState(t *testing.T) {
	ctx := context.Background()

	t.Run("count", func(t *testing.T) {
		store := newBatchStore(t)
		full := make([]SecretKVWrite, 0, MaxSecretKVs)
		for i := 0; i < MaxSecretKVs; i++ {
			full = append(full, SecretKVWrite{Key: fmt.Sprintf("k%02d", i), Value: []byte("v")})
		}
		if _, err := store.WriteSecretKVsContext(ctx, "count", full, nil, true); err != nil {
			t.Fatal(err)
		}
		// Delete one and add one keeps exactly 64 KVs.
		if _, err := store.WriteSecretKVsContext(ctx, "count",
			[]SecretKVWrite{{Key: "new", Value: []byte("v")}}, []string{"k00"}, false); err != nil {
			t.Fatalf("swap at count limit: %v", err)
		}
		// Net growth is rejected.
		if _, err := store.WriteSecretKVsContext(ctx, "count",
			[]SecretKVWrite{{Key: "extra", Value: []byte("v")}}, nil, false); !errors.Is(err, ErrSecretLimitExceeded) {
			t.Fatalf("growth error = %v", err)
		}
		var count int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM secret_kvs WHERE secret_id = (SELECT id FROM secrets WHERE name='count')`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != MaxSecretKVs {
			t.Fatalf("kv count = %d", count)
		}
	})

	t.Run("total", func(t *testing.T) {
		store := newBatchStore(t)
		value := bytes.Repeat([]byte("y"), MaxSecretValueBytes)
		full := make([]SecretKVWrite, 0, 4)
		for i := 0; i < 4; i++ {
			full = append(full, SecretKVWrite{Key: fmt.Sprintf("k%d", i), Value: value})
		}
		if _, err := store.WriteSecretKVsContext(ctx, "total", full, nil, true); err != nil {
			t.Fatal(err)
		}
		// Replace a deleted KV with the same-size value while flipping its
		// mark: the final total stays exactly at the limit.
		if _, err := store.WriteSecretKVsContext(ctx, "total",
			[]SecretKVWrite{{Key: "replacement", Value: value, IsEphemeral: secretBool(false)}}, []string{"k0"}, false); err != nil {
			t.Fatalf("swap at total limit: %v", err)
		}
		if _, err := store.WriteSecretKVsContext(ctx, "total",
			[]SecretKVWrite{{Key: "tiny", Value: []byte("z")}}, nil, false); !errors.Is(err, ErrSecretLimitExceeded) {
			t.Fatalf("growth error = %v", err)
		}
		values, marks, err := store.ReadSecretValuesContextAccess(ctx, "total", true)
		if err != nil {
			t.Fatal(err)
		}
		if len(values) != 4 || values["replacement"] != string(value) || marks["replacement"] {
			t.Fatalf("values/marks = %d/%#v", len(values), marks)
		}
		if _, ok := values["tiny"]; ok {
			t.Fatal("rejected batch partially wrote")
		}
	})
}

func TestSecretBatchAtomicRollback(t *testing.T) {
	ctx := context.Background()
	store := newBatchStore(t)
	if _, err := store.db.Exec(`CREATE TRIGGER reject_boom BEFORE INSERT ON secret_kvs
		WHEN NEW.key = 'boom' BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}

	name := "atomic/new"
	_, err := store.WriteSecretKVsContext(ctx, name, []SecretKVWrite{
		{Key: "good", Value: []byte("value")},
		{Key: "boom", Value: []byte("value")},
	}, nil, true)
	if err == nil {
		t.Fatal("failing batch succeeded")
	}
	var secrets, deks, kvs int
	for _, query := range []struct {
		sql string
		dst *int
	}{
		{`SELECT COUNT(*) FROM secrets`, &secrets},
		{`SELECT COUNT(*) FROM secret_deks`, &deks},
		{`SELECT COUNT(*) FROM secret_kvs`, &kvs},
	} {
		if err := store.db.QueryRow(query.sql).Scan(query.dst); err != nil {
			t.Fatal(err)
		}
	}
	if secrets != 0 || deks != 0 || kvs != 0 {
		t.Fatalf("rollback left secrets/deks/kvs = %d/%d/%d", secrets, deks, kvs)
	}

	// The same failure rolls back updates to an existing Secret.
	if _, err := store.WriteSecretKVsContext(ctx, "existing", []SecretKVWrite{
		{Key: "keep", Value: []byte("original")},
	}, nil, true); err != nil {
		t.Fatal(err)
	}
	_, err = store.WriteSecretKVsContext(ctx, "existing", []SecretKVWrite{
		{Key: "keep", Value: []byte("changed")},
		{Key: "boom", Value: []byte("value")},
	}, nil, false)
	if err == nil {
		t.Fatal("failing update batch succeeded")
	}
	values, _, err := store.ReadSecretValuesContextAccess(ctx, "existing", true)
	if err != nil {
		t.Fatal(err)
	}
	if values["keep"] != "original" {
		t.Fatalf("value = %q, want original", values["keep"])
	}
}

func TestSecretBatchWrongRecipient(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mgcl.db")
	owner := testIdentity(t)
	ownerStore, err := Open(path, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer ownerStore.Close()
	ctx := context.Background()
	if _, err := ownerStore.WriteSecretKVsContext(ctx, "owned/secret", []SecretKVWrite{
		{Key: "key", Value: []byte("original")},
	}, nil, true); err != nil {
		t.Fatal(err)
	}

	other, err := Open(path, testIdentityNamed(t, "other"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.WriteSecretKVsContext(ctx, "owned/secret", []SecretKVWrite{
		{Key: "key", Value: []byte("changed")},
	}, nil, false); !errors.Is(err, ErrSecretRecipientMismatch) {
		t.Fatalf("upsert error = %v", err)
	}
	if _, err := other.WriteSecretKVsContext(ctx, "owned/secret", nil, []string{"key"}, false); !errors.Is(err, ErrSecretRecipientMismatch) {
		t.Fatalf("delete error = %v", err)
	}
	values, _, err := ownerStore.ReadSecretValuesContextAccess(ctx, "owned/secret", true)
	if err != nil {
		t.Fatal(err)
	}
	if values["key"] != "original" {
		t.Fatalf("wrong recipient changed the value: %q", values["key"])
	}
}

func TestSecretBatchBackupIntegration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mgcl.db")
	target := filepath.Join(dir, "backup.db")
	manager, err := sqlitebackup.New(path, []sqlitebackup.Destination{{Scheme: sqlitebackup.SchemePath, Path: target}})
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenWithBackup(path, testIdentity(t), manager)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.WriteSecretKVsContext(context.Background(), "backed/batch", []SecretKVWrite{
		{Key: "key", Value: []byte("value"), IsEphemeral: secretBool(false)},
	}, nil, true); err != nil {
		store.Abort()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	backup, err := OpenSecretStoreReadOnly(target, testIdentity(t))
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	details, err := backup.ShowSecret("backed/batch")
	if err != nil {
		t.Fatal(err)
	}
	if len(details.Keys) != 1 || details.Keys[0].IsEphemeral {
		t.Fatalf("backup metadata = %+v", details.Keys)
	}
	got, err := backup.GetSecretValue("backed/batch", "key")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "value" {
		t.Fatalf("backup value = %q", got)
	}
}

func TestSecretBatchConcurrentSameSecret(t *testing.T) {
	store := newBatchStore(t)
	ctx := context.Background()
	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := store.WriteSecretKVsContext(ctx, "concurrent/batch", []SecretKVWrite{
				{Key: fmt.Sprintf("key%02d", i), Value: []byte(fmt.Sprintf("value-%d", i))},
			}, nil, false)
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent batch: %v", err)
		}
	}

	var secrets, deks, kvs int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM secrets`).Scan(&secrets); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM secret_deks`).Scan(&deks); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM secret_kvs`).Scan(&kvs); err != nil {
		t.Fatal(err)
	}
	if secrets != 1 || deks != 1 || kvs != n {
		t.Fatalf("secrets/deks/kvs = %d/%d/%d, want 1/1/%d", secrets, deks, kvs, n)
	}
	values, _, err := store.ReadSecretValuesContextAccess(ctx, "concurrent/batch", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != n {
		t.Fatalf("read %d values, want %d", len(values), n)
	}
}

func TestSecretBatchConcurrentIndependentHandles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mgcl.db")
	identity := testIdentity(t)
	first, err := Open(path, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Open(path, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	const perHandle = 10
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, perHandle*2)
	for handleIndex, store := range []*Store{first, second} {
		for i := 0; i < perHandle; i++ {
			wg.Add(1)
			go func(handleIndex, i int, store *Store) {
				defer wg.Done()
				<-start
				key := fmt.Sprintf("h%d-key%02d", handleIndex, i)
				_, err := store.WriteSecretKVsContext(context.Background(), "shared/batch", []SecretKVWrite{
					{Key: key, Value: []byte(key)},
				}, nil, false)
				errs <- err
			}(handleIndex, i, store)
		}
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent independent-handle batch: %v", err)
		}
	}

	var secrets, deks, kvs int
	for _, query := range []struct {
		sql string
		dst *int
	}{
		{`SELECT COUNT(*) FROM secrets`, &secrets},
		{`SELECT COUNT(*) FROM secret_deks`, &deks},
		{`SELECT COUNT(*) FROM secret_kvs`, &kvs},
	} {
		if err := first.db.QueryRow(query.sql).Scan(query.dst); err != nil {
			t.Fatal(err)
		}
	}
	if secrets != 1 || deks != 1 || kvs != perHandle*2 {
		t.Fatalf("secrets/deks/kvs = %d/%d/%d, want 1/1/%d", secrets, deks, kvs, perHandle*2)
	}
	values, _, err := first.ReadSecretValuesContextAccess(context.Background(), "shared/batch", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != perHandle*2 {
		t.Fatalf("read %d values, want %d", len(values), perHandle*2)
	}
}

func TestSecretBatchTimestamps(t *testing.T) {
	store := newBatchStore(t)
	ctx := context.Background()
	name := "timestamps/secret"

	if _, err := store.WriteSecretKVsContext(ctx, name, []SecretKVWrite{
		{Key: "key", Value: []byte("v1"), IsEphemeral: secretBool(false)},
	}, nil, true); err != nil {
		t.Fatal(err)
	}
	type timestamps struct {
		secretCreated, secretUpdated, keyCreated, keyUpdated string
	}
	read := func() timestamps {
		t.Helper()
		var got timestamps
		if err := store.db.QueryRow(
			`SELECT s.created_at, s.updated_at, k.created_at, k.updated_at
			FROM secrets s JOIN secret_kvs k ON k.secret_id = s.id
			WHERE s.name = ? AND k.key = 'key'`, name,
		).Scan(&got.secretCreated, &got.secretUpdated, &got.keyCreated, &got.keyUpdated); err != nil {
			t.Fatal(err)
		}
		return got
	}
	initial := read()
	time.Sleep(5 * time.Millisecond)

	if _, err := store.WriteSecretKVsContext(ctx, name, []SecretKVWrite{
		{Key: "key", Value: []byte("v2")},
	}, nil, false); err != nil {
		t.Fatal(err)
	}
	updated := read()
	if updated.secretCreated != initial.secretCreated {
		t.Fatal("Secret created_at changed on batch upsert")
	}
	if updated.keyCreated != initial.keyCreated {
		t.Fatal("KV created_at changed on batch upsert")
	}
	if updated.keyUpdated == initial.keyUpdated {
		t.Fatal("KV updated_at did not advance on batch upsert")
	}
	if updated.secretUpdated == initial.secretUpdated {
		t.Fatal("Secret updated_at did not advance on batch upsert")
	}

	// ensure=true on an existing Secret without changes must not bump the
	// Secret updated_at.
	beforeEnsure := read()
	time.Sleep(5 * time.Millisecond)
	if _, err := store.WriteSecretKVsContext(ctx, name, nil, nil, true); err != nil {
		t.Fatal(err)
	}
	afterEnsure := read()
	if afterEnsure.secretUpdated != beforeEnsure.secretUpdated {
		t.Fatal("no-op ensure bumped Secret updated_at")
	}
}

func backupDirty(manager *sqlitebackup.Manager) bool {
	return reflect.ValueOf(manager).Elem().FieldByName("dirty").Bool()
}

func TestSecretBatchNoChangeDoesNotMarkDirty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mgcl.db")
	store, err := Open(path, testIdentity(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		store.backup = nil
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	}()
	ctx := context.Background()
	name := "nochange/secret"
	if _, err := store.WriteSecretKVsContext(ctx, name, []SecretKVWrite{
		{Key: "key", Value: []byte("value")},
	}, nil, true); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(t.TempDir(), "backup.db")
	manager, err := sqlitebackup.New(path, []sqlitebackup.Destination{{Scheme: sqlitebackup.SchemePath, Path: target}})
	if err != nil {
		t.Fatal(err)
	}
	// Install the manager without Start so dirty marks cannot be consumed by
	// the background worker before this test observes them.
	store.backup = manager
	if backupDirty(manager) {
		t.Fatal("new backup manager is dirty")
	}

	if _, err := store.WriteSecretKVsContext(ctx, name, nil, nil, true); err != nil {
		t.Fatal(err)
	}
	if backupDirty(manager) {
		t.Fatal("no-op ensure marked the backup dirty")
	}

	if _, err := store.WriteSecretKVsContext(ctx, name, []SecretKVWrite{
		{Key: "second", Value: []byte("value")},
	}, nil, false); err != nil {
		t.Fatal(err)
	}
	if !backupDirty(manager) {
		t.Fatal("a real batch change did not mark the backup dirty")
	}
}
