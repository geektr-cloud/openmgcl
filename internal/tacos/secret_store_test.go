package tacos

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTestSecretStore(t *testing.T, path string, identity *Identity) *SecretStore {
	t.Helper()
	store, err := OpenSecretStore(path, identity, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return store
}

func TestSecretCreateListShow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mgcl.db")
	store := openTestSecretStore(t, path, testIdentity(t))

	created, err := store.CreateSecret("bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	if created.Name != "bootstrap" || created.ID == "" || len(created.Keys) != 0 {
		t.Fatalf("created = %+v", created)
	}
	if _, err := store.CreateSecret("bootstrap"); !errors.Is(err, ErrSecretAlreadyExists) {
		t.Fatalf("duplicate create error = %v", err)
	}
	if err := store.SetSecretValue("bootstrap", "access_key_id", []byte("AKIA")); err != nil {
		t.Fatal(err)
	}

	list, err := store.ListSecrets()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "bootstrap" || list[0].KeyCount != 1 {
		t.Fatalf("list = %+v", list)
	}
	details, err := store.ShowSecret("bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	if len(details.Keys) != 1 || details.Keys[0].Key != "access_key_id" {
		t.Fatalf("details = %+v", details)
	}
	if _, err := store.ShowSecret("missing"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("missing show error = %v", err)
	}
	for _, name := range []string{"", "Bootstrap", "a/b", "a b", strings.Repeat("a", 65), "-bad", "_bad"} {
		if _, err := store.CreateSecret(name); err == nil {
			t.Errorf("invalid name %q was accepted", name)
		}
	}
}

func TestSecretSetGetOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mgcl.db")
	store := openTestSecretStore(t, path, testIdentity(t))
	if _, err := store.CreateSecret("bootstrap"); err != nil {
		t.Fatal(err)
	}

	first := []byte("hello\nworld")
	if err := store.SetSecretValue("bootstrap", "key", first); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetSecretValue("bootstrap", "key")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, first) {
		t.Fatalf("value = %q", got)
	}
	details, err := store.ShowSecret("bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	createdAt := details.Keys[0].CreatedAt
	if !strings.HasSuffix(createdAt, "Z") || len(createdAt) != len("2026-11-13T12:00:00.000Z") {
		t.Fatalf("timestamp = %q", createdAt)
	}
	time.Sleep(2 * time.Millisecond)

	if err := store.SetSecretValue("bootstrap", "key", []byte("second")); err != nil {
		t.Fatal(err)
	}
	got, err = store.GetSecretValue("bootstrap", "key")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "second" {
		t.Fatalf("overwritten value = %q", got)
	}
	details, err = store.ShowSecret("bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	if len(details.Keys) != 1 {
		t.Fatalf("keys = %d after overwrite", len(details.Keys))
	}
	if details.Keys[0].CreatedAt != createdAt {
		t.Fatal("created_at changed on overwrite")
	}
	if details.Keys[0].UpdatedAt == createdAt {
		t.Fatal("updated_at did not change on overwrite")
	}

	if err := store.SetSecretValue("missing", "key", []byte("x")); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("set on missing secret error = %v", err)
	}
	if _, err := store.GetSecretValue("bootstrap", "missing"); !errors.Is(err, ErrSecretKeyNotFound) {
		t.Fatalf("get missing key error = %v", err)
	}
}

func TestSecretEmptyValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mgcl.db")
	store := openTestSecretStore(t, path, testIdentity(t))
	if _, err := store.CreateSecret("empty"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSecretValue("empty", "key", []byte{}); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetSecretValue("empty", "key")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("empty value = %#v", got)
	}
	values, err := store.ReadSecretValues("empty")
	if err != nil {
		t.Fatal(err)
	}
	if values["key"] != "" {
		t.Fatalf("provider value = %q", values["key"])
	}
}

func TestSecretValuesEncryptedAtRest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mgcl.db")
	store := openTestSecretStore(t, path, testIdentity(t))
	if _, err := store.CreateSecret("bootstrap"); err != nil {
		t.Fatal(err)
	}
	const plaintext = "SUPER-SECRET-VALUE"
	if err := store.SetSecretValue("bootstrap", "key", []byte(plaintext)); err != nil {
		t.Fatal(err)
	}

	var nonce, ciphertext []byte
	if err := store.db.QueryRow(
		`SELECT nonce, ciphertext FROM secret_kvs WHERE key = 'key'`,
	).Scan(&nonce, &ciphertext); err != nil {
		t.Fatal(err)
	}
	if len(nonce) != 12 || len(ciphertext) < aeadOverhead {
		t.Fatalf("nonce/ciphertext lengths = %d/%d", len(nonce), len(ciphertext))
	}
	if bytes.Contains(ciphertext, []byte(plaintext)) {
		t.Fatal("plaintext value is present in the ciphertext column")
	}

	var firstNonce, firstCiphertext []byte
	if err := store.db.QueryRow(`SELECT nonce, ciphertext FROM secret_kvs WHERE key = 'key'`).Scan(&firstNonce, &firstCiphertext); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSecretValue("bootstrap", "key", []byte(plaintext)); err != nil {
		t.Fatal(err)
	}
	var secondNonce, secondCiphertext []byte
	if err := store.db.QueryRow(`SELECT nonce, ciphertext FROM secret_kvs WHERE key = 'key'`).Scan(&secondNonce, &secondCiphertext); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(firstNonce, secondNonce) || bytes.Equal(firstCiphertext, secondCiphertext) {
		t.Fatal("rewriting the same plaintext reused nonce/ciphertext")
	}
}

func TestSecretWrongRecipient(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mgcl.db")
	owner := testIdentity(t)
	store := openTestSecretStore(t, path, owner)
	if _, err := store.CreateSecret("bootstrap"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSecretValue("bootstrap", "key", []byte("value")); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	other := testIdentityNamed(t, "other")
	otherStore, err := OpenSecretStore(path, other, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer otherStore.Close()

	if _, err := otherStore.GetSecretValue("bootstrap", "key"); !errors.Is(err, ErrSecretRecipientMismatch) {
		t.Fatalf("get error = %v", err)
	}
	if err := otherStore.SetSecretValue("bootstrap", "key", []byte("changed")); !errors.Is(err, ErrSecretRecipientMismatch) {
		t.Fatalf("set error = %v", err)
	}
	if err := otherStore.UnsetSecretValue("bootstrap", "key"); !errors.Is(err, ErrSecretRecipientMismatch) {
		t.Fatalf("unset error = %v", err)
	}
	if err := otherStore.DeleteSecret("bootstrap", nil); !errors.Is(err, ErrSecretRecipientMismatch) {
		t.Fatalf("delete error = %v", err)
	}

	// A different recipient can create and read a fresh secret.
	if _, err := otherStore.CreateSecret("other"); err != nil {
		t.Fatal(err)
	}
	if err := otherStore.SetSecretValue("other", "key", []byte("fresh")); err != nil {
		t.Fatal(err)
	}
	got, err := otherStore.GetSecretValue("other", "key")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "fresh" {
		t.Fatalf("fresh value = %q", got)
	}

	// Owner's original value was not modified by the failed writes.
	ownerStore, err := OpenSecretStoreReadOnly(path, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer ownerStore.Close()
	got, err = ownerStore.GetSecretValue("bootstrap", "key")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "value" {
		t.Fatalf("owner value = %q", got)
	}
}

func TestSecretUnsetUsesPreflightSecretID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mgcl.db")
	store := openTestSecretStore(t, path, testIdentity(t))
	if _, err := store.CreateSecret("recreated"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSecretValue("recreated", "key", []byte("old")); err != nil {
		t.Fatal(err)
	}

	store.afterUnsetPreflight = func() {
		if err := store.DeleteSecret("recreated", nil); err != nil {
			t.Errorf("delete old secret: %v", err)
		}
		if _, err := store.CreateSecret("recreated"); err != nil {
			t.Errorf("create replacement: %v", err)
		}
		if err := store.SetSecretValue("recreated", "key", []byte("new")); err != nil {
			t.Errorf("set replacement value: %v", err)
		}
	}

	if err := store.UnsetSecretValue("recreated", "key"); !errors.Is(err, ErrSecretKeyNotFound) {
		t.Fatalf("unset error = %v, want key not found", err)
	}
	values, err := store.ReadSecretValues("recreated")
	if err != nil {
		t.Fatal(err)
	}
	if values["key"] != "new" {
		t.Fatalf("replacement secret value = %q, want new", values["key"])
	}
}

func TestSecretUnsetAndDeleteCascade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mgcl.db")
	store := openTestSecretStore(t, path, testIdentity(t))
	if _, err := store.CreateSecret("bootstrap"); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"a", "b"} {
		if err := store.SetSecretValue("bootstrap", key, []byte(key)); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.UnsetSecretValue("bootstrap", "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetSecretValue("bootstrap", "a"); !errors.Is(err, ErrSecretKeyNotFound) {
		t.Fatalf("unset key still readable: %v", err)
	}
	if err := store.UnsetSecretValue("bootstrap", "a"); !errors.Is(err, ErrSecretKeyNotFound) {
		t.Fatalf("unset missing key error = %v", err)
	}
	if err := store.UnsetSecretValue("bootstrap", "missing"); !errors.Is(err, ErrSecretKeyNotFound) {
		t.Fatalf("unset missing key error = %v", err)
	}

	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM secret_kvs`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("kv count = %d, want 1", count)
	}

	confirmed := 0
	if err := store.DeleteSecret("bootstrap", func(keyCount int) (bool, error) {
		confirmed = keyCount
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if confirmed != 1 {
		t.Fatalf("confirm key count = %d", confirmed)
	}
	for _, table := range []string{"secrets", "secret_deks", "secret_kvs"} {
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s count = %d after delete", table, count)
		}
	}
	if err := store.DeleteSecret("bootstrap", nil); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("delete missing error = %v", err)
	}

	// Cancellation must not delete anything.
	if _, err := store.CreateSecret("keep"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteSecret("keep", func(int) (bool, error) { return false, nil }); err == nil {
		t.Fatal("cancelled delete succeeded")
	}
	if _, err := store.ShowSecret("keep"); err != nil {
		t.Fatal(err)
	}
}

func TestSecretCorruptionFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mgcl.db")
	store := openTestSecretStore(t, path, testIdentity(t))
	if _, err := store.CreateSecret("bootstrap"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSecretValue("bootstrap", "key", []byte("value")); err != nil {
		t.Fatal(err)
	}

	mutations := []struct {
		name string
		sql  string
	}{
		{"wrapped dek tag", `UPDATE secret_deks SET wrapped_dek = substr(wrapped_dek, 1, 47) || char(0)`},
		{"wrapped dek length", `UPDATE secret_deks SET wrapped_dek = substr(wrapped_dek, 1, 40)`},
		{"dek fingerprint", `UPDATE secret_deks SET dek_fingerprint = 'SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'`},
		{"kv nonce length", `UPDATE secret_kvs SET nonce = substr(nonce, 1, 11)`},
		{"kv ciphertext short", `UPDATE secret_kvs SET ciphertext = substr(ciphertext, 1, 10)`},
		{"kv suite", `UPDATE secret_kvs SET suite = 9`},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			copyPath := filepath.Join(t.TempDir(), "copy.db")
			copySecretDatabase(t, store.db, copyPath)
			db, err := openSecretDB(copyPath, false)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(mutation.sql); err != nil {
				db.Close()
				t.Fatal(err)
			}
			db.Close()

			corrupt, err := OpenSecretStoreReadOnly(copyPath, store.identity)
			if err != nil {
				t.Fatal(err)
			}
			defer corrupt.Close()
			if _, err := corrupt.GetSecretValue("bootstrap", "key"); err == nil {
				t.Fatal("corrupt secret data was accepted")
			}
		})
	}

	// AAD context mismatch: moving a ciphertext row to another key fails.
	copyPath := filepath.Join(t.TempDir(), "copy.db")
	copySecretDatabase(t, store.db, copyPath)
	db, err := openSecretDB(copyPath, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO secret_kvs (secret_id, key, suite, nonce, ciphertext, created_at, updated_at)
		SELECT secret_id, 'other', suite, nonce, ciphertext, created_at, updated_at FROM secret_kvs WHERE key = 'key'`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	moved, err := OpenSecretStoreReadOnly(copyPath, store.identity)
	if err != nil {
		t.Fatal(err)
	}
	defer moved.Close()
	if _, err := moved.GetSecretValue("bootstrap", "other"); !errors.Is(err, ErrSecretCorrupt) {
		t.Fatalf("moved ciphertext error = %v", err)
	}
}

func copySecretDatabase(t *testing.T, db *sql.DB, path string) {
	t.Helper()
	if _, err := db.Exec(`VACUUM INTO '` + strings.ReplaceAll(path, "'", "''") + `'`); err != nil {
		t.Fatal(err)
	}
}

func TestSecretLimits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mgcl.db")
	store := openTestSecretStore(t, path, testIdentity(t))
	if _, err := store.CreateSecret("limits"); err != nil {
		t.Fatal(err)
	}

	if err := store.SetSecretValue("limits", "bad", []byte{0xff}); !errors.Is(err, ErrSecretInvalidValue) {
		t.Fatalf("invalid UTF-8 error = %v", err)
	}
	tooLarge := bytes.Repeat([]byte("x"), MaxSecretValueBytes+1)
	if err := store.SetSecretValue("limits", "bad", tooLarge); !errors.Is(err, ErrSecretInvalidValue) {
		t.Fatalf("oversized value error = %v", err)
	}
	if err := store.SetSecretValue("limits", "max", bytes.Repeat([]byte("x"), MaxSecretValueBytes)); err != nil {
		t.Fatalf("64 KiB value rejected: %v", err)
	}
	if err := store.SetSecretValue("limits", "Bad", []byte("x")); err == nil {
		t.Fatal("invalid key syntax accepted")
	}
	if err := store.SetSecretValue("limits", strings.Repeat("k", 65), []byte("x")); err == nil {
		t.Fatal("oversized key accepted")
	}

	// Total size: four 64 KiB values fit exactly; a fifth must fail.
	store2 := openTestSecretStore(t, filepath.Join(t.TempDir(), "mgcl.db"), testIdentity(t))
	if _, err := store2.CreateSecret("total"); err != nil {
		t.Fatal(err)
	}
	value := bytes.Repeat([]byte("y"), MaxSecretValueBytes)
	for i := 0; i < 4; i++ {
		if err := store2.SetSecretValue("total", fmt.Sprintf("k%d", i), value); err != nil {
			t.Fatalf("value %d: %v", i, err)
		}
	}
	if err := store2.SetSecretValue("total", "k4", []byte("z")); !errors.Is(err, ErrSecretLimitExceeded) {
		t.Fatalf("total-size limit error = %v", err)
	}
	// Replacing an existing value does not count twice.
	if err := store2.SetSecretValue("total", "k0", []byte("z")); err != nil {
		t.Fatalf("replacement after total limit: %v", err)
	}

	// Count limit.
	store3 := openTestSecretStore(t, filepath.Join(t.TempDir(), "mgcl.db"), testIdentity(t))
	if _, err := store3.CreateSecret("count"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxSecretKVs; i++ {
		if err := store3.SetSecretValue("count", fmt.Sprintf("k%02d", i), []byte("v")); err != nil {
			t.Fatalf("kv %d: %v", i, err)
		}
	}
	if err := store3.SetSecretValue("count", "extra", []byte("v")); !errors.Is(err, ErrSecretLimitExceeded) {
		t.Fatalf("KV count limit error = %v", err)
	}
	if err := store3.SetSecretValue("count", "k00", []byte("v2")); err != nil {
		t.Fatalf("replacement at count limit: %v", err)
	}
}

func TestParseSecretReference(t *testing.T) {
	valid := []string{"a/b", "bootstrap/access_key_id", "0/0", "a_b/c-d"}
	for _, ref := range valid {
		name, key, err := ParseSecretReference(ref)
		if err != nil {
			t.Errorf("%q: %v", ref, err)
			continue
		}
		if name+"/"+key != ref {
			t.Errorf("%q parsed as %q/%q", ref, name, key)
		}
	}
	for _, ref := range []string{"", "a", "a/", "/b", "a/b/c", "A/b", "a/B", "a b/c", strings.Repeat("a", 65) + "/b"} {
		if _, _, err := ParseSecretReference(ref); err == nil {
			t.Errorf("invalid reference %q was accepted", ref)
		}
	}
	if !ValidSecretName(strings.Repeat("a", 64)) {
		t.Fatal("64-character name was rejected")
	}
	if ValidSecretName(strings.Repeat("a", 65)) {
		t.Fatal("65-character name was accepted")
	}
}

func TestSecretProviderReadValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mgcl.db")
	identity := testIdentity(t)
	store := openTestSecretStore(t, path, identity)
	if _, err := store.CreateSecret("bootstrap"); err != nil {
		t.Fatal(err)
	}
	values, err := store.ReadSecretValues("bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	if values == nil || len(values) != 0 {
		t.Fatalf("empty secret values = %#v", values)
	}
	if err := store.SetSecretValue("bootstrap", "a", []byte("1")); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSecretValue("bootstrap", "b", []byte("2")); err != nil {
		t.Fatal(err)
	}
	values, err = store.ReadSecretValues("bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || values["a"] != "1" || values["b"] != "2" {
		t.Fatalf("values = %#v", values)
	}
	if err := store.SetSecretValue("bootstrap", "a", []byte("updated")); err != nil {
		t.Fatal(err)
	}
	values, err = store.ReadSecretValues("bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	if values["a"] != "updated" {
		t.Fatalf("latest value = %q", values["a"])
	}
	if _, err := store.ReadSecretValues("missing"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("missing read error = %v", err)
	}
}

func TestSecretDuplicateCreateNoOrphans(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mgcl.db")
	store := openTestSecretStore(t, path, testIdentity(t))
	if _, err := store.CreateSecret("one"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateSecret("one"); !errors.Is(err, ErrSecretAlreadyExists) {
		t.Fatal(err)
	}
	var secrets, deks int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM secrets`).Scan(&secrets); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM secret_deks`).Scan(&deks); err != nil {
		t.Fatal(err)
	}
	if secrets != 1 || deks != 1 {
		t.Fatalf("secrets/deks = %d/%d after duplicate create", secrets, deks)
	}
}
