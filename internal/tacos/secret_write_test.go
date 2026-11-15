package tacos

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/geektr-cloud/openmgcl/internal/sqlitebackup"
)

func TestSecretReadKVAccessModes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mgcl.db")
	store, err := Open(path, testIdentity(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	if _, _, err := store.WriteSecretKVContext(ctx, "mixed/public-key", []byte("public-value"), false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.WriteSecretKVContext(ctx, "mixed/ephemeral-key", []byte("ephemeral-value"), true); err != nil {
		t.Fatal(err)
	}

	values, marks, err := store.ReadSecretValuesContextAccess(ctx, "mixed", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || values["public-key"] != "public-value" || values["ephemeral-key"] != "ephemeral-value" {
		t.Fatalf("all values = %#v", values)
	}
	if marks["public-key"] || !marks["ephemeral-key"] {
		t.Fatalf("all marks = %#v", marks)
	}

	values, marks, err = store.ReadSecretValuesContextAccess(ctx, "mixed", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values["public-key"] != "public-value" {
		t.Fatalf("public values = %#v", values)
	}
	if len(marks) != 1 || marks["public-key"] {
		t.Fatalf("public marks = %#v", marks)
	}

	// A Secret with no public KV returns empty maps, not an error.
	if _, _, err := store.WriteSecretKVContext(ctx, "no-public/key", []byte("value"), true); err != nil {
		t.Fatal(err)
	}
	values, marks, err = store.ReadSecretValuesContextAccess(ctx, "no-public", false)
	if err != nil {
		t.Fatal(err)
	}
	if values == nil || marks == nil || len(values) != 0 || len(marks) != 0 {
		t.Fatalf("empty public read = %#v / %#v", values, marks)
	}

	if _, _, err := store.ReadSecretValuesContextAccess(ctx, "missing", true); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("missing read error = %v", err)
	}
}

func TestSecretPublicReadSkipsCorruptEphemeralKV(t *testing.T) {
	newStore := func(t *testing.T) (*Store, string) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "mgcl.db")
		store, err := Open(path, testIdentity(t))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.Close() })
		if _, _, err := store.WriteSecretKVContext(context.Background(), "mixed/public-key", []byte("public-value"), false); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.WriteSecretKVContext(context.Background(), "mixed/ephemeral-key", []byte("ephemeral-value"), true); err != nil {
			t.Fatal(err)
		}
		return store, path
	}

	t.Run("corrupt ephemeral KV", func(t *testing.T) {
		store, _ := newStore(t)
		if _, err := store.db.Exec(
			`UPDATE secret_kvs SET ciphertext = substr(ciphertext, 1, 10) WHERE key = 'ephemeral-key'`,
		); err != nil {
			t.Fatal(err)
		}
		values, marks, err := store.ReadSecretValuesContextAccess(context.Background(), "mixed", false)
		if err != nil {
			t.Fatalf("public read failed on corrupt ephemeral KV: %v", err)
		}
		if values["public-key"] != "public-value" || len(values) != 1 || marks["public-key"] {
			t.Fatalf("public read = %#v / %#v", values, marks)
		}
		if _, _, err := store.ReadSecretValuesContextAccess(context.Background(), "mixed", true); !errors.Is(err, ErrSecretCorrupt) {
			t.Fatalf("all read error = %v, want corrupt", err)
		}
	})

	t.Run("corrupt public KV", func(t *testing.T) {
		store, _ := newStore(t)
		if _, err := store.db.Exec(
			`UPDATE secret_kvs SET ciphertext = substr(ciphertext, 1, 10) WHERE key = 'public-key'`,
		); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.ReadSecretValuesContextAccess(context.Background(), "mixed", false); !errors.Is(err, ErrSecretCorrupt) {
			t.Fatalf("public read error = %v, want corrupt", err)
		}
	})
}

func TestSecretWriteCreatesSecretAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mgcl.db")
	store, err := Open(path, testIdentity(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	name, key, err := store.WriteSecretKVContext(ctx, "fresh/key", []byte("value"), true)
	if err != nil {
		t.Fatal(err)
	}
	if name != "fresh" || key != "key" {
		t.Fatalf("write returned %q/%q", name, key)
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
	if secrets != 1 || deks != 1 || kvs != 1 {
		t.Fatalf("secrets/deks/kvs = %d/%d/%d", secrets, deks, kvs)
	}

	readOnly, err := OpenSecretStoreReadOnly(path, testIdentity(t))
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	details, err := readOnly.ShowSecret("fresh")
	if err != nil {
		t.Fatal(err)
	}
	if len(details.Keys) != 1 || !details.Keys[0].IsEphemeral {
		t.Fatalf("fresh metadata = %+v", details.Keys)
	}

	// Overwriting updates the value and the mark without changing the Secret.
	if _, _, err := store.WriteSecretKVContext(ctx, "fresh/key", []byte("updated"), false); err != nil {
		t.Fatal(err)
	}
	details, err = readOnly.ShowSecret("fresh")
	if err != nil {
		t.Fatal(err)
	}
	if len(details.Keys) != 1 || details.Keys[0].IsEphemeral {
		t.Fatalf("updated mark = %+v", details.Keys)
	}
	values, marks, err := store.ReadSecretValuesContextAccess(ctx, "fresh", true)
	if err != nil {
		t.Fatal(err)
	}
	if values["key"] != "updated" || marks["key"] {
		t.Fatalf("updated read = %#v / %#v", values, marks)
	}

	// Empty values are valid.
	if _, _, err := store.WriteSecretKVContext(ctx, "fresh/empty", []byte{}, true); err != nil {
		t.Fatal(err)
	}
	values, _, err = store.ReadSecretValuesContextAccess(ctx, "fresh", true)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := values["empty"]; !ok || got != "" {
		t.Fatalf("empty value = %q, present = %t", got, ok)
	}

	// Invalid references and values are rejected without creating a Secret.
	if _, _, err := store.WriteSecretKVContext(ctx, "bad", []byte("x"), true); err == nil {
		t.Fatal("invalid reference was accepted")
	}
	if _, _, err := store.WriteSecretKVContext(ctx, "bad/key", []byte{0xff}, true); !errors.Is(err, ErrSecretInvalidValue) {
		t.Fatalf("invalid UTF-8 error = %v", err)
	}
	if _, _, err := store.WriteSecretKVContext(ctx, "bad/key", bytes.Repeat([]byte("x"), MaxSecretValueBytes+1), true); !errors.Is(err, ErrSecretInvalidValue) {
		t.Fatalf("oversized value error = %v", err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM secrets WHERE name = 'bad'`).Scan(&secrets); err != nil {
		t.Fatal(err)
	}
	if secrets != 0 {
		t.Fatal("failed write left a Secret behind")
	}
}

func TestSecretWriteLimits(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "mgcl.db"), testIdentity(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	for i := 0; i < MaxSecretKVs; i++ {
		if _, _, err := store.WriteSecretKVContext(ctx, fmt.Sprintf("limits/k%02d", i), []byte("v"), true); err != nil {
			t.Fatalf("kv %d: %v", i, err)
		}
	}
	if _, _, err := store.WriteSecretKVContext(ctx, "limits/extra", []byte("v"), true); !errors.Is(err, ErrSecretLimitExceeded) {
		t.Fatalf("KV count limit error = %v", err)
	}
	// Replacing an existing KV at the limit succeeds.
	if _, _, err := store.WriteSecretKVContext(ctx, "limits/k00", []byte("v2"), true); err != nil {
		t.Fatalf("replacement at count limit: %v", err)
	}
}

func TestSecretWriteWrongRecipient(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mgcl.db")
	owner := testIdentity(t)
	store, err := Open(path, owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.WriteSecretKVContext(context.Background(), "owner/key", []byte("original"), true); err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	other, err := Open(path, testIdentityNamed(t, "other"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()

	if _, _, err := other.WriteSecretKVContext(context.Background(), "owner/key", []byte("changed"), true); !errors.Is(err, ErrSecretRecipientMismatch) {
		t.Fatalf("wrong recipient write error = %v", err)
	}
	readOnly, err := OpenSecretStoreReadOnly(path, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	values, marks, err := readSecretValues(context.Background(), readOnly.db, owner, "owner", true)
	if err != nil {
		t.Fatal(err)
	}
	if values["key"] != "original" || !marks["key"] {
		t.Fatalf("wrong recipient changed the KV: %#v / %#v", values, marks)
	}
}

func TestSecretDeleteKVContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mgcl.db")
	store, err := Open(path, testIdentity(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	if _, _, err := store.WriteSecretKVContext(ctx, "delete/key", []byte("value"), true); err != nil {
		t.Fatal(err)
	}
	if name, key, err := store.DeleteSecretKVContext(ctx, "delete/key"); err != nil {
		t.Fatal(err)
	} else if name != "delete" || key != "key" {
		t.Fatalf("delete returned %q/%q", name, key)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM secret_kvs`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("kv count after delete = %d", count)
	}
	// The empty Secret is kept.
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM secrets WHERE name = 'delete'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("delete removed the empty Secret")
	}

	// Deleting the same KV again and deleting an unknown Secret/KV both succeed.
	if _, _, err := store.DeleteSecretKVContext(ctx, "delete/key"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.DeleteSecretKVContext(ctx, "missing/key"); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM secrets WHERE name = 'missing'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("delete created a missing Secret")
	}
	if _, _, err := store.DeleteSecretKVContext(ctx, "bad"); err == nil {
		t.Fatal("invalid reference was accepted")
	}
}

func TestSecretWriteConcurrentNewSecret(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "mgcl.db"), testIdentity(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const n = 24
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := store.WriteSecretKVContext(context.Background(), fmt.Sprintf("shared/key%02d", i), []byte(fmt.Sprintf("value-%d", i)), true)
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent write: %v", err)
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
	values, _, err := store.ReadSecretValuesContextAccess(context.Background(), "shared", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != n {
		t.Fatalf("read %d values, want %d", len(values), n)
	}
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("key%02d", i)
		if values[key] != fmt.Sprintf("value-%d", i) {
			t.Fatalf("%s = %q", key, values[key])
		}
	}
}

func TestSecretWriteConcurrentIndependentHandles(t *testing.T) {
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

	const perHandle = 12
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, perHandle*2)
	for handleIndex, store := range []*Store{first, second} {
		for i := 0; i < perHandle; i++ {
			wg.Add(1)
			go func(handleIndex, i int, store *Store) {
				defer wg.Done()
				<-start
				ref := fmt.Sprintf("shared/h%d-key%02d", handleIndex, i)
				value := fmt.Sprintf("h%d-value-%d", handleIndex, i)
				_, _, err := store.WriteSecretKVContext(context.Background(), ref, []byte(value), true)
				errs <- err
			}(handleIndex, i, store)
		}
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent independent-handle write: %v", err)
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
	values, _, err := first.ReadSecretValuesContextAccess(context.Background(), "shared", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != perHandle*2 {
		t.Fatalf("read %d values, want %d", len(values), perHandle*2)
	}
	for handleIndex := 0; handleIndex < 2; handleIndex++ {
		for i := 0; i < perHandle; i++ {
			key := fmt.Sprintf("h%d-key%02d", handleIndex, i)
			want := fmt.Sprintf("h%d-value-%d", handleIndex, i)
			if values[key] != want {
				t.Fatalf("%s = %q, want %q", key, values[key], want)
			}
		}
	}
}

func TestSecretWriteBackupIntegration(t *testing.T) {
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
	if _, _, err := store.WriteSecretKVContext(context.Background(), "backed/key", []byte("value"), false); err != nil {
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
	details, err := backup.ShowSecret("backed")
	if err != nil {
		t.Fatal(err)
	}
	if len(details.Keys) != 1 || details.Keys[0].IsEphemeral {
		t.Fatalf("backup metadata = %+v", details.Keys)
	}
	got, err := backup.GetSecretValue("backed", "key")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "value" {
		t.Fatalf("backup value = %q", got)
	}
}
