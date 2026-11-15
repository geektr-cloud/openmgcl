package tacos

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func openTestStateStore(t *testing.T, path string, identity *Identity) *Store {
	t.Helper()
	store, err := Open(path, identity)
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

func createStateTestWorkspace(t *testing.T, store *Store, org, name string) *Workspace {
	t.Helper()
	ws, err := store.CreateWorkspace(org, name, "")
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func createTestStateVersion(t *testing.T, store *Store, wsID string, serial int, lineage, md5 string, state, jsonState, outputs []byte) *StateVersion {
	t.Helper()
	sv, err := store.CreateStateVersion(wsID, serial, lineage, md5, state, jsonState, outputs, false)
	if err != nil {
		t.Fatal(err)
	}
	return sv
}

func TestParseStateReference(t *testing.T) {
	valid := []struct {
		ref  string
		org  string
		name string
	}{
		{"org/ws", "org", "ws"},
		{"org-name/ws_name", "org-name", "ws_name"},
		{"a/b", "a", "b"},
		{"a.b/c.d", "a.b", "c.d"},
	}
	for _, tc := range valid {
		org, name, err := ParseStateReference(tc.ref)
		if err != nil {
			t.Errorf("ParseStateReference(%q) error = %v", tc.ref, err)
			continue
		}
		if org != tc.org || name != tc.name {
			t.Errorf("ParseStateReference(%q) = %q/%q, want %q/%q", tc.ref, org, name, tc.org, tc.name)
		}
	}

	invalid := []string{
		"",
		"org",
		"org/",
		"/ws",
		"org/ws/extra",
		"org//ws",
		"a/b/c",
		"/",
	}
	for _, ref := range invalid {
		if _, _, err := ParseStateReference(ref); err == nil {
			t.Errorf("ParseStateReference(%q) accepted an invalid reference", ref)
		}
	}
}

func TestStateGetWorkspace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mgcl.db")
	store := openTestStateStore(t, path, testIdentity(t))
	ws := createStateTestWorkspace(t, store, "org", "main")

	got, err := store.GetStateWorkspace("org", "main")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != ws.ID {
		t.Fatalf("GetStateWorkspace ID = %q, want %q", got.ID, ws.ID)
	}
	if _, err := store.GetStateWorkspace("org", "missing"); !IsAPIError(err, http.StatusNotFound) {
		t.Fatalf("missing workspace error = %v, want 404", err)
	}
	for _, parts := range [][2]string{{"", "ws"}, {"org", ""}, {"org/extra", "ws"}, {"org", "ws/extra"}} {
		if _, err := store.GetStateWorkspace(parts[0], parts[1]); err == nil {
			t.Fatalf("GetStateWorkspace(%q, %q) accepted an invalid reference", parts[0], parts[1])
		}
	}
}

func TestStateListSummaries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mgcl.db")
	store := openTestStateStore(t, path, testIdentity(t))

	summaries, err := store.ListStateSummaries()
	if err != nil {
		t.Fatal(err)
	}
	if summaries == nil || len(summaries) != 0 {
		t.Fatalf("empty summaries = %#v, want non-nil empty slice", summaries)
	}

	wsEmpty := createStateTestWorkspace(t, store, "org-a", "empty")
	wsAlpha := createStateTestWorkspace(t, store, "org-a", "alpha")
	wsBeta := createStateTestWorkspace(t, store, "org-b", "beta")

	alphaState := []byte(`{"alpha":"state"}`)
	alphaJSON := []byte(`{"alpha":true}`)
	alphaOutputs := []byte(`{"out":1}`)
	createTestStateVersion(t, store, wsAlpha.ID, 1, "alpha-lineage", "alpha-md5", alphaState, alphaJSON, alphaOutputs)
	createTestStateVersion(t, store, wsBeta.ID, 1, "beta-lineage", "beta-md5", []byte("raw-beta"), []byte(`{"beta":true}`), nil)
	pending := createTestStateVersion(t, store, wsBeta.ID, 2, "beta-lineage", "beta-pending", nil, []byte(`{"pending":true}`), nil)

	summaries, err = store.ListStateSummaries()
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 3 {
		t.Fatalf("summaries = %+v, want 3", summaries)
	}
	if summaries[0].ID != wsAlpha.ID || summaries[0].Organization != "org-a" || summaries[0].Name != "alpha" {
		t.Fatalf("summaries[0] = %+v, want org-a/alpha", summaries[0])
	}
	if summaries[0].Versions != 1 || summaries[0].Pending != 0 {
		t.Fatalf("alpha counts = %d/%d, want 1/0", summaries[0].Versions, summaries[0].Pending)
	}
	if summaries[1].ID != wsEmpty.ID || summaries[1].Versions != 0 || summaries[1].Pending != 0 || summaries[1].StoredBytes != 0 {
		t.Fatalf("empty summary = %+v, want zero counts and bytes", summaries[1])
	}
	if summaries[2].ID != wsBeta.ID || summaries[2].Versions != 1 || summaries[2].Pending != 1 {
		t.Fatalf("beta summary = %+v, want 1 finalized and 1 pending", summaries[2])
	}
	if summaries[2].StoredBytes == 0 {
		t.Fatal("beta stored bytes = 0, want pending blob bytes counted")
	}

	var alphaStored int64
	if err := store.db.QueryRow(
		`SELECT COALESCE(SUM(
			COALESCE(length(state), 0) + COALESCE(length(json_state), 0) + COALESCE(length(json_state_outputs), 0)
		), 0) FROM state_versions WHERE workspace_id = ?`, wsAlpha.ID,
	).Scan(&alphaStored); err != nil {
		t.Fatal(err)
	}
	if uint64(alphaStored) != summaries[0].StoredBytes {
		t.Fatalf("alpha StoredBytes = %d, want encrypted blob sum %d", summaries[0].StoredBytes, alphaStored)
	}
	plainAlpha := len(alphaState) + len(alphaJSON) + len(alphaOutputs)
	if summaries[0].StoredBytes <= uint64(plainAlpha) {
		t.Fatalf("alpha StoredBytes = %d, want encrypted size greater than plaintext %d", summaries[0].StoredBytes, plainAlpha)
	}

	var betaStored int64
	if err := store.db.QueryRow(
		`SELECT COALESCE(SUM(
			COALESCE(length(state), 0) + COALESCE(length(json_state), 0) + COALESCE(length(json_state_outputs), 0)
		), 0) FROM state_versions WHERE workspace_id = ?`, wsBeta.ID,
	).Scan(&betaStored); err != nil {
		t.Fatal(err)
	}
	if uint64(betaStored) != summaries[2].StoredBytes {
		t.Fatalf("beta StoredBytes = %d, want encrypted blob sum %d", summaries[2].StoredBytes, betaStored)
	}
	var pendingBlob int64
	if err := store.db.QueryRow(`SELECT COALESCE(length(json_state), 0) FROM state_versions WHERE id = ?`, pending.ID).Scan(&pendingBlob); err != nil {
		t.Fatal(err)
	}
	if pendingBlob == 0 {
		t.Fatal("pending version json_state blob is empty")
	}
}

func TestStateListVersionInfoCurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mgcl.db")
	store := openTestStateStore(t, path, testIdentity(t))
	ws := createStateTestWorkspace(t, store, "org", "ws")

	v10 := createTestStateVersion(t, store, ws.ID, 10, "lineage", "md5-10", []byte("raw-10"), []byte(`{"serial":10}`), nil)
	v5 := createTestStateVersion(t, store, ws.ID, 5, "lineage", "md5-5", []byte("raw-5"), []byte(`{"serial":5}`), nil)
	pending := createTestStateVersion(t, store, ws.ID, 6, "lineage", "md5-6", nil, []byte(`{"pending":true}`), nil)

	info, err := store.ListStateVersionInfo("org", "ws")
	if err != nil {
		t.Fatal(err)
	}
	if len(info) != 3 {
		t.Fatalf("info = %+v, want 3 versions", info)
	}
	if info[0].ID != pending.ID || info[0].Current {
		t.Fatalf("info[0] = %+v, want newest pending and not current", info[0])
	}
	if info[1].ID != v5.ID || !info[1].Current {
		t.Fatalf("info[1] = %+v, want restored serial 5 current", info[1])
	}
	if info[2].ID != v10.ID || info[2].Current {
		t.Fatalf("info[2] = %+v, want older serial 10 not current", info[2])
	}

	replaced := createTestStateVersion(t, store, ws.ID, 10, "lineage", "md5-10b", []byte("raw-10b"), []byte(`{"serial":10,"v":2}`), nil)
	info, err = store.ListStateVersionInfo("org", "ws")
	if err != nil {
		t.Fatal(err)
	}
	if len(info) != 3 {
		t.Fatalf("info after same-serial replacement = %+v, want 3 versions", info)
	}
	if info[0].ID != replaced.ID || !info[0].Current {
		t.Fatalf("info[0] after replacement = %+v, want replacement current", info[0])
	}
	for _, item := range info {
		if item.ID == v10.ID {
			t.Fatalf("replaced finalized version %s still listed", v10.ID)
		}
	}
	if info[1].ID != pending.ID || info[1].Current {
		t.Fatalf("info[1] after replacement = %+v, want pending not current", info[1])
	}
	if info[2].ID != v5.ID || info[2].Current {
		t.Fatalf("info[2] after replacement = %+v, want restored serial 5 not current", info[2])
	}

	emptyWS := createStateTestWorkspace(t, store, "org", "empty")
	emptyInfo, err := store.ListStateVersionInfo("org", "empty")
	if err != nil {
		t.Fatal(err)
	}
	if emptyInfo == nil || len(emptyInfo) != 0 {
		t.Fatalf("empty info = %#v, want non-nil empty slice", emptyInfo)
	}
	_ = emptyWS

	if _, err := store.ListStateVersionInfo("org", "missing"); !IsAPIError(err, http.StatusNotFound) {
		t.Fatalf("missing workspace error = %v, want 404", err)
	}
	for _, parts := range [][2]string{{"", "ws"}, {"org", ""}, {"org/extra", "ws"}, {"org", "ws/extra"}} {
		if _, err := store.ListStateVersionInfo(parts[0], parts[1]); err == nil {
			t.Fatalf("ListStateVersionInfo(%q, %q) accepted an invalid reference", parts[0], parts[1])
		}
	}
}

func TestStateExportCurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mgcl.db")
	identity := testIdentity(t)
	store, err := Open(path, identity)
	if err != nil {
		t.Fatal(err)
	}
	ws := createStateTestWorkspace(t, store, "org", "main")
	jsonState := []byte(`{"version":4,"serial":1}`)
	jsonOutputs := []byte(`{"outputs":{"value":{"value":"ok"}}}`)
	createTestStateVersion(t, store, ws.ID, 1, "lineage", "md5", []byte("raw-state"), jsonState, jsonOutputs)

	rawOnly := createStateTestWorkspace(t, store, "org", "raw-only")
	rawState := []byte("raw-only-state")
	createTestStateVersion(t, store, rawOnly.ID, 1, "raw-lineage", "raw-md5", rawState, nil, nil)

	missingOutputs := createStateTestWorkspace(t, store, "org", "no-outputs")
	createTestStateVersion(t, store, missingOutputs.ID, 1, "output-lineage", "output-md5", []byte("raw"), []byte(`{"json":true}`), nil)

	createStateTestWorkspace(t, store, "org", "no-current")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	readonly, err := OpenStateStoreReadOnly(path, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer readonly.Close()

	got, err := readonly.ExportCurrentState("org", "main", false)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, jsonState) {
		t.Fatalf("JSON state = %s, want %s", got, jsonState)
	}
	got, err = readonly.ExportCurrentState("org", "main", true)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, jsonOutputs) {
		t.Fatalf("JSON outputs = %s, want %s", got, jsonOutputs)
	}

	got, err = readonly.ExportCurrentState("org", "raw-only", false)
	if err == nil {
		t.Fatal("export of raw-only version succeeded, want missing JSON state error")
	}
	if got != nil {
		t.Fatalf("raw-only export returned %q, want no fallback bytes", got)
	}
	got, err = readonly.ExportCurrentState("org", "no-outputs", true)
	if err == nil {
		t.Fatal("export of version without outputs succeeded")
	}
	if got != nil {
		t.Fatalf("missing-outputs export returned %q, want nil", got)
	}
	if _, err := readonly.ExportCurrentState("org", "no-current", false); !IsAPIError(err, http.StatusNotFound) {
		t.Fatalf("no-current export error = %v, want 404", err)
	}
	if _, err := readonly.ExportCurrentState("org", "missing", false); !IsAPIError(err, http.StatusNotFound) {
		t.Fatalf("missing workspace export error = %v, want 404", err)
	}

	other, err := OpenStateStoreReadOnly(path, testIdentityNamed(t, "other"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.ExportCurrentState("org", "main", false); !IsAPIError(err, http.StatusForbidden) {
		t.Fatalf("wrong recipient export error = %v, want 403", err)
	}
}

func TestStateExportIgnoresCorruptNonTargetBlob(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mgcl.db")
	identity := testIdentity(t)
	store, err := Open(path, identity)
	if err != nil {
		t.Fatal(err)
	}
	ws := createStateTestWorkspace(t, store, "org", "main")
	jsonState := []byte(`{"version":4,"serial":1}`)
	jsonOutputs := []byte(`{"outputs":{"value":"ok"}}`)
	sv := createTestStateVersion(t, store, ws.ID, 1, "lineage", "md5", []byte("raw-state"), jsonState, jsonOutputs)

	// Raw state is not the default export target.
	if _, err := store.db.Exec(
		`UPDATE state_versions SET state = ? WHERE id = ?`,
		[]byte{0xde, 0xad}, sv.ID,
	); err != nil {
		t.Fatal(err)
	}
	got, err := store.ExportCurrentState("org", "main", false)
	if err != nil {
		t.Fatalf("default export failed because a non-target blob is corrupt: %v", err)
	}
	if !bytes.Equal(got, jsonState) {
		t.Fatalf("default export = %s, want %s", got, jsonState)
	}

	// Corrupt json_state, which is a non-target for --outputs.
	if _, err := store.db.Exec(
		`UPDATE state_versions SET json_state = ? WHERE id = ?`,
		[]byte{0xba, 0xad}, sv.ID,
	); err != nil {
		t.Fatal(err)
	}
	got, err = store.ExportCurrentState("org", "main", true)
	if err != nil {
		t.Fatalf("outputs export failed because a non-target blob is corrupt: %v", err)
	}
	if !bytes.Equal(got, jsonOutputs) {
		t.Fatalf("outputs export = %s, want %s", got, jsonOutputs)
	}
}

func TestStatePrunePlanAndNoOp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mgcl.db")
	store := openTestStateStore(t, path, testIdentity(t))
	ws := createStateTestWorkspace(t, store, "org", "ws")

	for serial := 1; serial <= 5; serial++ {
		createTestStateVersion(t, store, ws.ID, serial, "lineage", fmt.Sprintf("md5-%d", serial), []byte(fmt.Sprintf("raw-%d", serial)), []byte(fmt.Sprintf(`{"serial":%d}`, serial)), nil)
	}
	pending := createTestStateVersion(t, store, ws.ID, 9, "lineage", "md5-pending", nil, []byte(`{"pending":true}`), nil)

	plan, err := store.PlanStatePrune("org", "ws", 2)
	if err != nil {
		t.Fatal(err)
	}
	if plan.TotalFinalized != 5 || plan.DeleteFinalized != 3 || plan.KeepFinalized != 2 || plan.Pending != 1 {
		t.Fatalf("plan = %+v, want total=5 delete=3 keep=2 pending=1", plan)
	}

	plan, err = store.PlanStatePrune("org", "ws", 10)
	if err != nil {
		t.Fatal(err)
	}
	if plan.TotalFinalized != 5 || plan.DeleteFinalized != 0 || plan.KeepFinalized != 5 || plan.Pending != 1 {
		t.Fatalf("over-keep plan = %+v, want no deletion", plan)
	}

	plan, err = store.PruneState("org", "ws", 10)
	if err != nil {
		t.Fatal(err)
	}
	if plan.DeleteFinalized != 0 || plan.KeepFinalized != 5 || plan.Pending != 1 {
		t.Fatalf("no-op prune = %+v, want no deletion", plan)
	}
	info, err := store.ListStateVersionInfo("org", "ws")
	if err != nil {
		t.Fatal(err)
	}
	if len(info) != 6 {
		t.Fatalf("versions after no-op prune = %d, want 6", len(info))
	}

	for _, keeps := range []int{0, -1} {
		if _, err := store.PlanStatePrune("org", "ws", keeps); err == nil {
			t.Fatalf("PlanStatePrune keeps=%d succeeded", keeps)
		}
		if _, err := store.PruneState("org", "ws", keeps); err == nil {
			t.Fatalf("PruneState keeps=%d succeeded", keeps)
		}
	}

	if err := store.LockWorkspace(ws.ID, `{"owner":"test"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PlanStatePrune("org", "ws", 2); !IsAPIError(err, http.StatusConflict) {
		t.Fatalf("locked plan error = %v, want 409", err)
	}
	if _, err := store.PruneState("org", "ws", 2); !IsAPIError(err, http.StatusConflict) {
		t.Fatalf("locked prune error = %v, want 409", err)
	}
	if err := store.UnlockWorkspace(ws.ID); err != nil {
		t.Fatal(err)
	}

	plan, err = store.PruneState("org", "ws", 2)
	if err != nil {
		t.Fatal(err)
	}
	if plan.TotalFinalized != 5 || plan.DeleteFinalized != 3 || plan.KeepFinalized != 2 || plan.Pending != 1 {
		t.Fatalf("prune plan = %+v, want total=5 delete=3 keep=2 pending=1", plan)
	}

	info, err = store.ListStateVersionInfo("org", "ws")
	if err != nil {
		t.Fatal(err)
	}
	if len(info) != 3 {
		t.Fatalf("versions after prune = %+v, want 2 finalized + 1 pending", info)
	}
	foundPending := false
	var currentSerial int
	var currentJSON []byte
	for _, item := range info {
		if item.ID == pending.ID {
			foundPending = true
			if item.Current {
				t.Fatal("pending version marked current")
			}
		}
		if item.Current {
			currentSerial = item.Serial
			currentJSON, err = store.ExportCurrentState("org", "ws", false)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if !foundPending {
		t.Fatal("pending version was pruned")
	}
	if currentSerial != 5 {
		t.Fatalf("current serial = %d, want 5", currentSerial)
	}
	if string(currentJSON) != `{"serial":5}` {
		t.Fatalf("current JSON = %s, want serial 5 payload", currentJSON)
	}

	if _, err := store.PlanStatePrune("org", "missing", 1); !IsAPIError(err, http.StatusNotFound) {
		t.Fatalf("missing plan error = %v, want 404", err)
	}
	if _, err := store.PruneState("org", "missing", 1); !IsAPIError(err, http.StatusNotFound) {
		t.Fatalf("missing prune error = %v, want 404", err)
	}
}

func TestStatePruneRestoreOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mgcl.db")
	store := openTestStateStore(t, path, testIdentity(t))
	ws := createStateTestWorkspace(t, store, "org", "ws")

	createTestStateVersion(t, store, ws.ID, 10, "lineage", "md5-10", []byte("raw-10"), []byte(`{"serial":10}`), nil)
	createTestStateVersion(t, store, ws.ID, 5, "lineage", "md5-5", []byte("raw-5"), []byte(`{"serial":5}`), nil)

	plan, err := store.PruneState("org", "ws", 1)
	if err != nil {
		t.Fatal(err)
	}
	if plan.TotalFinalized != 2 || plan.DeleteFinalized != 1 || plan.KeepFinalized != 1 {
		t.Fatalf("restore prune plan = %+v, want oldest rowid deleted", plan)
	}

	info, err := store.ListStateVersionInfo("org", "ws")
	if err != nil {
		t.Fatal(err)
	}
	if len(info) != 1 || info[0].Serial != 5 || !info[0].Current {
		t.Fatalf("info after restore prune = %+v, want current serial 5", info)
	}
	got, err := store.ExportCurrentState("org", "ws", false)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"serial":5}` {
		t.Fatalf("exported current = %s, want restored serial 5", got)
	}
}

func TestStateDelete(t *testing.T) {
	t.Run("locked", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "mgcl.db")
		store := openTestStateStore(t, path, testIdentity(t))
		ws := createStateTestWorkspace(t, store, "org", "ws")
		createTestStateVersion(t, store, ws.ID, 1, "lineage", "md5", []byte("raw"), []byte(`{"v":1}`), nil)
		if err := store.LockWorkspace(ws.ID, `{"owner":"test"}`); err != nil {
			t.Fatal(err)
		}
		if err := store.DeleteState("org", "ws"); !IsAPIError(err, http.StatusConflict) {
			t.Fatalf("locked delete error = %v, want 409", err)
		}
		if _, err := store.GetWorkspace("org", "ws"); err != nil {
			t.Fatalf("locked workspace was removed: %v", err)
		}
	})

	t.Run("missing", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "mgcl.db")
		store := openTestStateStore(t, path, testIdentity(t))
		if err := store.DeleteState("org", "missing"); !IsAPIError(err, http.StatusNotFound) {
			t.Fatalf("missing delete error = %v, want 404", err)
		}
	})

	t.Run("preserves shared lineage key", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "mgcl.db")
		store := openTestStateStore(t, path, testIdentity(t))
		ws1 := createStateTestWorkspace(t, store, "org", "one")
		ws2 := createStateTestWorkspace(t, store, "org", "two")
		createTestStateVersion(t, store, ws1.ID, 1, "shared-lineage", "md5-one", []byte("raw-one"), []byte(`{"workspace":"one"}`), nil)
		createTestStateVersion(t, store, ws2.ID, 1, "shared-lineage", "md5-two", []byte("raw-two"), []byte(`{"workspace":"two"}`), nil)

		var lineageCount, wrappedCount int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM lineage_keys`).Scan(&lineageCount); err != nil {
			t.Fatal(err)
		}
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM wrapped_lineage_keys`).Scan(&wrappedCount); err != nil {
			t.Fatal(err)
		}
		if lineageCount != 1 || wrappedCount != 1 {
			t.Fatalf("lineage rows = %d/%d, want 1/1", lineageCount, wrappedCount)
		}

		if err := store.DeleteState("org", "one"); err != nil {
			t.Fatal(err)
		}
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM lineage_keys`).Scan(&lineageCount); err != nil {
			t.Fatal(err)
		}
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM wrapped_lineage_keys`).Scan(&wrappedCount); err != nil {
			t.Fatal(err)
		}
		if lineageCount != 1 || wrappedCount != 1 {
			t.Fatalf("lineage rows after delete = %d/%d, want preserved 1/1", lineageCount, wrappedCount)
		}

		if _, err := store.ListStateVersionInfo("org", "one"); !IsAPIError(err, http.StatusNotFound) {
			t.Fatalf("deleted workspace info error = %v, want 404", err)
		}
		info, err := store.ListStateVersionInfo("org", "two")
		if err != nil {
			t.Fatal(err)
		}
		if len(info) != 1 || !info[0].Current {
			t.Fatalf("surviving workspace info = %+v, want one current version", info)
		}
		got, err := store.ExportCurrentState("org", "two", false)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != `{"workspace":"two"}` {
			t.Fatalf("surviving workspace export = %s", got)
		}
		summaries, err := store.ListStateSummaries()
		if err != nil {
			t.Fatal(err)
		}
		if len(summaries) != 1 || summaries[0].Name != "two" {
			t.Fatalf("summaries after delete = %+v, want only workspace two", summaries)
		}
		var versionCount int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM state_versions`).Scan(&versionCount); err != nil {
			t.Fatal(err)
		}
		if versionCount != 1 {
			t.Fatalf("state_versions rows = %d, want 1", versionCount)
		}
	})
}

func TestStateReadOnlyDoesNotCreateDatabase(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.db")
	if _, err := OpenStateStoreReadOnly(missing, testIdentity(t)); !errors.Is(err, ErrStateDatabaseNotFound) {
		t.Fatalf("missing database error = %v, want ErrStateDatabaseNotFound", err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("read-only open created %s", missing)
	}
}

func TestStateReadOnlyEmptyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStateStoreReadOnly(path, testIdentity(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	summaries, err := store.ListStateSummaries()
	if err != nil {
		t.Fatal(err)
	}
	if summaries == nil || len(summaries) != 0 {
		t.Fatalf("empty summaries = %#v, want non-nil empty slice", summaries)
	}
	if _, err := store.GetStateWorkspace("org", "ws"); !IsAPIError(err, http.StatusNotFound) {
		t.Fatalf("empty workspace error = %v, want 404", err)
	}
	if _, err := store.ListStateVersionInfo("org", "ws"); !IsAPIError(err, http.StatusNotFound) {
		t.Fatalf("empty info error = %v, want 404", err)
	}
	if _, err := store.PlanStatePrune("org", "ws", 1); !IsAPIError(err, http.StatusNotFound) {
		t.Fatalf("empty plan error = %v, want 404", err)
	}
	if _, err := store.ExportCurrentState("org", "ws", false); !IsAPIError(err, http.StatusNotFound) {
		t.Fatalf("empty export error = %v, want 404", err)
	}

	stat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if stat.Size() != 0 {
		t.Fatalf("empty database grew to %d bytes after read-only operations", stat.Size())
	}
}

func TestStateReadOnlyOutdatedSecretSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	createLegacySecretDB(t, path, false)

	store, err := OpenStateStoreReadOnly(path, testIdentity(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := store.GetStateWorkspace("org", "ws"); !errors.Is(err, ErrSecretSchemaOutdated) {
		t.Fatalf("GetStateWorkspace error = %v, want ErrSecretSchemaOutdated", err)
	}
	if _, err := store.ListStateSummaries(); !errors.Is(err, ErrSecretSchemaOutdated) {
		t.Fatalf("ListStateSummaries error = %v, want ErrSecretSchemaOutdated", err)
	}
	if _, err := store.ListStateVersionInfo("org", "ws"); !errors.Is(err, ErrSecretSchemaOutdated) {
		t.Fatalf("ListStateVersionInfo error = %v, want ErrSecretSchemaOutdated", err)
	}
	if _, err := store.ExportCurrentState("org", "ws", false); !errors.Is(err, ErrSecretSchemaOutdated) {
		t.Fatalf("ExportCurrentState error = %v, want ErrSecretSchemaOutdated", err)
	}
	if _, err := store.PlanStatePrune("org", "ws", 1); !errors.Is(err, ErrSecretSchemaOutdated) {
		t.Fatalf("PlanStatePrune error = %v, want ErrSecretSchemaOutdated", err)
	}

	var stateTables int
	if err := store.db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name IN ('workspaces', 'state_versions')`,
	).Scan(&stateTables); err != nil {
		t.Fatal(err)
	}
	if stateTables != 0 {
		t.Fatalf("read-only state operations created %d state tables", stateTables)
	}
	var ephemeralColumn int
	if err := store.db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('secret_kvs') WHERE name = 'is_ephemeral'`,
	).Scan(&ephemeralColumn); err != nil {
		t.Fatal(err)
	}
	if ephemeralColumn != 0 {
		t.Fatal("read-only state operations altered the legacy secret schema")
	}
}
