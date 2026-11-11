package tacos

import (
	"bytes"
	"net/http"
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T, identity *Identity) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mgcl.db")
	store, err := Open(path, identity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store, path
}

func rawStateFields(t *testing.T, store *Store, id string) [][]byte {
	t.Helper()
	var state, jsonState, outputs []byte
	if err := store.db.QueryRow(
		`SELECT state, json_state, json_state_outputs FROM state_versions WHERE id = ?`, id,
	).Scan(&state, &jsonState, &outputs); err != nil {
		t.Fatal(err)
	}
	return [][]byte{state, jsonState, outputs}
}

func assertEncrypted(t *testing.T, raw []byte, plaintext []byte) {
	t.Helper()
	if len(raw) < payloadHeaderLen+aeadOverhead {
		t.Fatalf("raw field is not an envelope: %d bytes", len(raw))
	}
	if string(raw[0:4]) != payloadMagic || raw[4] != payloadVersion || raw[5] != payloadSuite {
		t.Fatalf("raw field has invalid header: %x", raw[:6])
	}
	if len(plaintext) > 0 && bytes.Contains(raw, plaintext) {
		t.Fatal("raw encrypted field contains plaintext")
	}
}

func createTestWorkspace(t *testing.T, store *Store, name string) *Workspace {
	t.Helper()
	ws, err := store.CreateWorkspace("test-org", name, "")
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func TestStoreEncryptsAllStateFields(t *testing.T) {
	store, _ := openTestStore(t, testIdentity(t))
	ws := createTestWorkspace(t, store, "encrypted")
	state := []byte(`{"version":4,"serial":1,"lineage":"lineage-a"}`)
	jsonState := []byte(`{"version":4,"serial":1}`)
	outputs := []byte(`{"hello":{"value":"world","type":"string"}}`)

	sv, err := store.CreateStateVersion(ws.ID, 1, "lineage-a", "md5-of-plaintext", state, jsonState, outputs, false)
	if err != nil {
		t.Fatal(err)
	}
	if sv.State == nil || !bytes.Equal(sv.State, state) {
		t.Fatal("Store did not return the original state")
	}
	if !bytes.Equal(sv.JSONState, jsonState) || !bytes.Equal(sv.JSONStateOutputs, outputs) {
		t.Fatal("Store did not return the original JSON fields")
	}
	if sv.MD5 != "md5-of-plaintext" {
		t.Fatalf("MD5 = %q", sv.MD5)
	}

	raw := rawStateFields(t, store, sv.ID)
	plaintexts := [][]byte{state, jsonState, outputs}
	for i := range raw {
		assertEncrypted(t, raw[i], plaintexts[i])
	}

	var lineageCount, wrappedCount int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM lineage_keys WHERE lineage = 'lineage-a'`).Scan(&lineageCount); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM wrapped_lineage_keys`).Scan(&wrappedCount); err != nil {
		t.Fatal(err)
	}
	if lineageCount != 1 || wrappedCount != 1 {
		t.Fatalf("key rows = %d/%d, want 1/1", lineageCount, wrappedCount)
	}
}

func TestStoreReusesLineageKeyAcrossWorkspaces(t *testing.T) {
	store, _ := openTestStore(t, testIdentity(t))
	ws1 := createTestWorkspace(t, store, "one")
	ws2 := createTestWorkspace(t, store, "two")
	state := []byte(`{"version":4,"serial":1}`)

	sv1, err := store.CreateStateVersion(ws1.ID, 1, "shared-lineage", "", state, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	sv2, err := store.CreateStateVersion(ws2.ID, 1, "shared-lineage", "", state, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}

	raw1 := rawStateFields(t, store, sv1.ID)[0]
	raw2 := rawStateFields(t, store, sv2.ID)[0]
	if !bytes.Equal(raw1[6:6+32], raw2[6:6+32]) {
		t.Fatal("same lineage used different key fingerprints")
	}
	if bytes.Equal(raw1, raw2) {
		t.Fatal("same plaintext in different workspaces reused the same envelope")
	}

	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM lineage_keys WHERE lineage = 'shared-lineage'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("lineage key count = %d, want 1", count)
	}
}

func TestStoreUsesNewKeyForNewLineage(t *testing.T) {
	store, _ := openTestStore(t, testIdentity(t))
	ws := createTestWorkspace(t, store, "lineages")
	state := []byte(`{"version":4}`)

	sv1, err := store.CreateStateVersion(ws.ID, 1, "lineage-1", "", state, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	sv2, err := store.CreateStateVersion(ws.ID, 2, "lineage-2", "", state, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	fp1 := rawStateFields(t, store, sv1.ID)[0][6:38]
	fp2 := rawStateFields(t, store, sv2.ID)[0][6:38]
	if bytes.Equal(fp1, fp2) {
		t.Fatal("new lineage reused the old lineage key")
	}
}

func TestStoreRejectsEmptyLineage(t *testing.T) {
	store, _ := openTestStore(t, testIdentity(t))
	ws := createTestWorkspace(t, store, "empty-lineage")

	_, err := store.CreateStateVersion(ws.ID, 1, "", "", []byte(`{}`), nil, nil, false)
	if !IsAPIError(err, http.StatusUnprocessableEntity) {
		t.Fatalf("empty lineage error = %v, want 422", err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM state_versions`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("state version rows = %d, want 0", count)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM lineage_keys`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("lineage key rows = %d, want 0", count)
	}
}

func TestStoreRejectsOtherRecipient(t *testing.T) {
	identity1 := testIdentityNamed(t, "one")
	store1, path := openTestStore(t, identity1)
	ws1 := createTestWorkspace(t, store1, "owned")
	sv, err := store1.CreateStateVersion(ws1.ID, 1, "owned-lineage", "", []byte(`{"secret":true}`), nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}

	store2, err := Open(path, testIdentityNamed(t, "two"))
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()

	if _, err := store2.GetStateVersion(sv.ID); !IsAPIError(err, http.StatusForbidden) {
		t.Fatalf("other recipient read error = %v, want 403", err)
	}

	if _, err := store2.CreateStateVersion(ws1.ID, 2, "owned-lineage", "", []byte(`{}`), nil, nil, false); !IsAPIError(err, http.StatusForbidden) {
		t.Fatalf("other recipient created a version on an owned lineage: %v", err)
	}
	pending, err := store1.CreateStateVersion(ws1.ID, 2, "owned-lineage", "", nil, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := store2.UploadState(pending.ID, []byte(`{}`)); !IsAPIError(err, http.StatusForbidden) {
		t.Fatalf("other recipient uploaded to an owned lineage: %v", err)
	}

	ws2 := createTestWorkspace(t, store2, "other")
	if _, err := store2.CreateStateVersion(ws2.ID, 1, "other-lineage", "", []byte(`{}`), nil, nil, false); err != nil {
		t.Fatalf("other recipient could not create a new lineage: %v", err)
	}
}

func TestStoreUploadPathEncryptsFields(t *testing.T) {
	store, _ := openTestStore(t, testIdentity(t))
	ws := createTestWorkspace(t, store, "upload")

	sv, err := store.CreateStateVersion(ws.ID, 1, "upload-lineage", "", nil, []byte(`{"outputs":{}}`), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if sv.Status != "pending" {
		t.Fatalf("status = %q, want pending", sv.Status)
	}
	state := []byte(`{"version":4,"serial":1}`)
	jsonState := []byte(`{"version":4}`)
	if err := store.UploadState(sv.ID, state); err != nil {
		t.Fatal(err)
	}
	if err := store.UploadJSONState(sv.ID, jsonState); err != nil {
		t.Fatal(err)
	}

	got, err := store.GetStateVersion(sv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "finalized" || !bytes.Equal(got.State, state) || !bytes.Equal(got.JSONState, jsonState) {
		t.Fatalf("uploaded state mismatch: %+v", got)
	}
	raw := rawStateFields(t, store, sv.ID)
	assertEncrypted(t, raw[0], state)
	assertEncrypted(t, raw[1], jsonState)
	if raw[2] != nil {
		t.Fatalf("untouched json_state_outputs = %v, want NULL", raw[2])
	}
}

func TestStoreUploadEmptyStateUsesEnvelope(t *testing.T) {
	store, _ := openTestStore(t, testIdentity(t))
	ws := createTestWorkspace(t, store, "empty-upload")
	sv, err := store.CreateStateVersion(ws.ID, 1, "empty-upload-lineage", "", nil, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UploadState(sv.ID, []byte{}); err != nil {
		t.Fatal(err)
	}
	raw := rawStateFields(t, store, sv.ID)[0]
	if len(raw) != payloadHeaderLen+aeadOverhead {
		t.Fatalf("empty upload envelope length = %d", len(raw))
	}
}

func TestStoreRejectsPlaintextAndZeroLengthBlobs(t *testing.T) {
	store, _ := openTestStore(t, testIdentity(t))
	ws := createTestWorkspace(t, store, "plaintext")

	sv, err := store.CreateStateVersion(ws.ID, 1, "plaintext-lineage", "", nil, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE state_versions SET state = ? WHERE id = ?`, []byte(`{"version":4}`), sv.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetStateVersion(sv.ID); err == nil {
		t.Fatal("plaintext state was returned")
	}

	if _, err := store.db.Exec(`UPDATE state_versions SET state = ? WHERE id = ?`, []byte{}, sv.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetStateVersion(sv.ID); err == nil {
		t.Fatal("non-NULL zero-length state was returned")
	}
}

func TestStoreFailedStateInsertLeavesNoKey(t *testing.T) {
	store, _ := openTestStore(t, testIdentity(t))
	ws := createTestWorkspace(t, store, "transaction")
	if _, err := store.CreateStateVersion(ws.ID, 1, "first-lineage", "", []byte(`{}`), nil, nil, false); err != nil {
		t.Fatal(err)
	}

	_, err := store.CreateStateVersion(ws.ID, 1, "second-lineage", "", []byte(`{}`), nil, nil, true)
	if !IsAPIError(err, http.StatusConflict) {
		t.Fatalf("serial conflict error = %v, want 409", err)
	}
	var lineageCount, wrappedCount int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM lineage_keys WHERE lineage = 'second-lineage'`).Scan(&lineageCount); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM wrapped_lineage_keys`).Scan(&wrappedCount); err != nil {
		t.Fatal(err)
	}
	if lineageCount != 0 || wrappedCount != 1 {
		t.Fatalf("failed transaction left orphan key rows: lineage=%d wrapped=%d", lineageCount, wrappedCount)
	}
}

func TestStoreCachedKeyStillValidatesMetadata(t *testing.T) {
	store, _ := openTestStore(t, testIdentity(t))
	ws := createTestWorkspace(t, store, "cached")
	sv, err := store.CreateStateVersion(ws.ID, 1, "cached-lineage", "", []byte(`{}`), nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetStateVersion(sv.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE wrapped_lineage_keys SET suite = 2`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetStateVersion(sv.ID); err == nil || IsAPIError(err, http.StatusForbidden) {
		t.Fatalf("cached key hid corrupted metadata: %v", err)
	}
}

func TestStoreRejectsNilIdentity(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "nil.db"), nil); err == nil {
		t.Fatal("Open accepted a nil identity")
	}
}

func TestStoreMissingWrappedKeyIsDataCorruption(t *testing.T) {
	store, _ := openTestStore(t, testIdentity(t))
	ws := createTestWorkspace(t, store, "missing-key")
	sv, err := store.CreateStateVersion(ws.ID, 1, "missing-key-lineage", "", []byte(`{}`), nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DELETE FROM wrapped_lineage_keys`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetStateVersion(sv.ID); err == nil || IsAPIError(err, http.StatusForbidden) {
		t.Fatalf("missing wrapped key error = %v, want non-403 data error", err)
	}
}
