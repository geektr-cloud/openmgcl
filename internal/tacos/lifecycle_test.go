package tacos

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/http"
	"sync"
	"testing"
)

// TestAbandonedSameSerialPendingKeepsCurrent covers the safe replacement
// lifecycle: a modern create for the same serial leaves the old finalized row
// current until the pending upload succeeds, including across a store restart.
func TestAbandonedSameSerialPendingKeepsCurrent(t *testing.T) {
	identity := testIdentity(t)
	store1, path := openTestStore(t, identity)
	ws := createTestWorkspace(t, store1, "safe-replacement")

	oldState := []byte(`{"version":4,"serial":1,"value":"old"}`)
	old, err := store1.CreateStateVersion(ws.ID, 1, "safe-lineage", "md5-old", oldState, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := store1.CreateStateVersion(ws.ID, 1, "safe-lineage", "md5-new", nil, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != "pending" {
		t.Fatalf("replacement status = %q, want pending", pending.Status)
	}
	store1.Close()

	store2, err := Open(path, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()

	current, err := store2.GetCurrentStateVersion(ws.ID)
	if err != nil {
		t.Fatalf("current after abandoned upload: %v", err)
	}
	if current.ID != old.ID {
		t.Fatalf("current id = %q, want old finalized %q", current.ID, old.ID)
	}
	if !bytes.Equal(current.State, oldState) {
		t.Fatalf("current state = %q, want %q", current.State, oldState)
	}

	newState := []byte(`{"version":4,"serial":1,"value":"new"}`)
	if err := store2.UploadState(pending.ID, newState); err != nil {
		t.Fatalf("upload replacement: %v", err)
	}
	current, err = store2.GetCurrentStateVersion(ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != pending.ID || !bytes.Equal(current.State, newState) {
		t.Fatalf("current after upload = %q %q, want %q %q", current.ID, current.State, pending.ID, newState)
	}
	if _, err := store2.GetStateVersion(old.ID); !IsAPIError(err, http.StatusNotFound) {
		t.Fatalf("old finalized error = %v, want 404 after successful replacement", err)
	}
}

func TestModernSameSerialKeepsOldDownloadURLUntilUpload(t *testing.T) {
	ts, _ := newTestServer(t, "")
	defer ts.Close()
	wsID := mustCreateWorkspaceHTTP(t, ts, "download-lifecycle")

	_, oldBody := createStateVersionHTTP(t, ts, wsID, map[string]any{
		"serial": 1, "lineage": "download-lineage", "md5": "md5-old",
		"state": base64.StdEncoding.EncodeToString([]byte("old")),
	})
	oldID := stateVersionID(t, oldBody)

	status, pendingBody := createStateVersionHTTP(t, ts, wsID, map[string]any{
		"serial": 1, "lineage": "download-lineage", "md5": "md5-new",
	})
	if status != http.StatusCreated {
		t.Fatalf("create pending replacement: status = %d, want 201", status)
	}
	uploadURL := stateVersionAttrs(t, pendingBody)["hosted-state-upload-url"].(string)

	if code, data := downloadState(t, ts, oldID); code != http.StatusOK || string(data) != "old" {
		t.Fatalf("old download while pending = %d %q, want 200 old", code, data)
	}
	if got := stateVersionID(t, getCurrentVersion(t, ts, wsID)); got != oldID {
		t.Fatalf("current during pending = %q, want %q", got, oldID)
	}

	resp := putBytes(t, uploadURL, []byte("new"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload replacement: status = %d, want 200", resp.StatusCode)
	}
	if code, _ := downloadState(t, ts, oldID); code != http.StatusNotFound {
		t.Fatalf("old download after upload = %d, want 404", code)
	}
	if code, data := downloadState(t, ts, stateVersionID(t, pendingBody)); code != http.StatusOK || string(data) != "new" {
		t.Fatalf("new download = %d %q, want 200 new", code, data)
	}
}

func TestIdenticalFinalizedRetransmitBlockedByExistingPending(t *testing.T) {
	ts, store := newTestServer(t, "")
	defer ts.Close()
	wsID := mustCreateWorkspaceHTTP(t, ts, "p3-pending")

	_, oldBody := createStateVersionHTTP(t, ts, wsID, map[string]any{
		"serial": 1, "lineage": "p3-pending-lineage", "md5": "md5-historical",
		"state": base64.StdEncoding.EncodeToString([]byte("historical")),
	})
	oldID := stateVersionID(t, oldBody)

	status, pendingBody := createStateVersionHTTP(t, ts, wsID, map[string]any{
		"serial": 1, "lineage": "p3-pending-lineage", "md5": "md5-pending",
	})
	if status != http.StatusCreated {
		t.Fatalf("create pending: status = %d, want 201", status)
	}
	pendingID := stateVersionID(t, pendingBody)
	uploadURL := stateVersionAttrs(t, pendingBody)["hosted-state-upload-url"].(string)

	// A late identical retransmit must not replace the pending row. If it
	// did, uploading would revive the historical finalized state as current.
	status, _ = createStateVersionHTTP(t, ts, wsID, map[string]any{
		"serial": 1, "lineage": "p3-pending-lineage", "md5": "md5-historical",
	})
	if status != http.StatusConflict {
		t.Fatalf("identical retransmit with pending: status = %d, want 409", status)
	}

	current := getCurrentVersion(t, ts, wsID)
	if got := stateVersionID(t, current); got != oldID {
		t.Fatalf("current id = %q, want %q", got, oldID)
	}
	if code, data := downloadState(t, ts, oldID); code != http.StatusOK || string(data) != "historical" {
		t.Fatalf("current download = %d %q, want historical", code, data)
	}
	pending, err := store.GetStateVersion(pendingID)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != "pending" {
		t.Fatalf("pending status = %q, want pending", pending.Status)
	}

	// The rejected retransmit did not damage the existing pending upload.
	resp := putBytes(t, uploadURL, []byte("new"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload original pending: status = %d, want 200", resp.StatusCode)
	}
	if got := stateVersionID(t, getCurrentVersion(t, ts, wsID)); got != pendingID {
		t.Fatalf("current id = %q, want uploaded pending %q", got, pendingID)
	}
}

func TestUploadRejectsLegacyIdenticalPending(t *testing.T) {
	store, _ := openTestStore(t, testIdentity(t))
	ws := createTestWorkspace(t, store, "legacy-identical-pending")

	oldState := []byte(`{"version":4,"serial":1,"value":"old"}`)
	old, err := store.CreateStateVersion(ws.ID, 1, "legacy-identical-lineage", "md5-identical", oldState, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a pending row that predates the create-time P3 check by
	// inserting it directly; new creates of this shape are already rejected.
	pendingID := randomID("sv")
	if _, err := store.db.Exec(
		`INSERT INTO state_versions (id, workspace_id, serial, lineage, md5, status)
		VALUES (?, ?, 1, ?, ?, 'pending')`,
		pendingID, ws.ID, "legacy-identical-lineage", "md5-identical",
	); err != nil {
		t.Fatal(err)
	}

	if err := store.UploadState(pendingID, []byte(`{"version":4,"serial":1,"value":"old"}`)); !IsAPIError(err, http.StatusConflict) {
		t.Fatalf("identical pending upload error = %v, want 409", err)
	}
	current, err := store.GetCurrentStateVersion(ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != old.ID || !bytes.Equal(current.State, oldState) {
		t.Fatalf("current after rejected upload = %q %q, want %q %q", current.ID, current.State, old.ID, oldState)
	}
	got, err := store.GetStateVersion(pendingID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "pending" {
		t.Fatalf("pending after rejected upload status = %q, want pending", got.Status)
	}
}

// TestIdenticalHistoricalFinalizedRetransmitConflicts covers P3: after a
// lower-serial restore, replaying an identical historical serial must not make
// that old state current again.
func TestIdenticalHistoricalFinalizedRetransmitConflicts(t *testing.T) {
	ts, _ := newTestServer(t, "")
	defer ts.Close()
	wsID := mustCreateWorkspaceHTTP(t, ts, "p3-retransmit")

	_, v3 := createStateVersionHTTP(t, ts, wsID, map[string]any{
		"serial": 3, "lineage": "p3-lineage", "md5": "md5-v3",
		"state": base64.StdEncoding.EncodeToString([]byte("v3")),
	})
	id3 := stateVersionID(t, v3)

	status, restored := createStateVersionHTTP(t, ts, wsID, map[string]any{
		"serial": 2, "lineage": "p3-lineage", "md5": "md5-v2",
		"state": base64.StdEncoding.EncodeToString([]byte("restored")), "force": true,
	})
	if status != http.StatusCreated {
		t.Fatalf("restore: status = %d, want 201", status)
	}
	restoredID := stateVersionID(t, restored)

	status, _ = createStateVersionHTTP(t, ts, wsID, map[string]any{
		"serial": 3, "lineage": "p3-lineage", "md5": "md5-v3",
		"state": base64.StdEncoding.EncodeToString([]byte("v3")),
	})
	if status != http.StatusConflict {
		t.Fatalf("identical historical retransmit: status = %d, want 409", status)
	}
	current := getCurrentVersion(t, ts, wsID)
	if got := stateVersionID(t, current); got != restoredID {
		t.Fatalf("current id = %q, want restored %q", got, restoredID)
	}
	if code, _ := downloadState(t, ts, id3); code != http.StatusOK {
		t.Fatalf("historical version download = %d, want 200 (unchanged)", code)
	}

	// The same rule applies to the modern pending create path.
	status, _ = createStateVersionHTTP(t, ts, wsID, map[string]any{
		"serial": 3, "lineage": "p3-lineage", "md5": "md5-v3",
	})
	if status != http.StatusConflict {
		t.Fatalf("identical historical modern retransmit: status = %d, want 409", status)
	}

	// Different content is an explicit restore and may become current.
	status, v3new := createStateVersionHTTP(t, ts, wsID, map[string]any{
		"serial": 3, "lineage": "p3-lineage", "md5": "md5-v3-new",
		"state": base64.StdEncoding.EncodeToString([]byte("v3-new")),
	})
	if status != http.StatusCreated {
		t.Fatalf("different historical restore: status = %d, want 201", status)
	}
	if got := stateVersionID(t, getCurrentVersion(t, ts, wsID)); got != stateVersionID(t, v3new) {
		t.Fatal("different historical restore did not become current")
	}
}

func TestForceIdenticalFinalizedConflict(t *testing.T) {
	ts, _ := newTestServer(t, "")
	defer ts.Close()
	wsID := mustCreateWorkspaceHTTP(t, ts, "force-identical")

	_, first := createStateVersionHTTP(t, ts, wsID, map[string]any{
		"serial": 1, "lineage": "force-identical-lineage", "md5": "md5-first",
		"state": base64.StdEncoding.EncodeToString([]byte("first")),
	})
	firstID := stateVersionID(t, first)

	status, _ := createStateVersionHTTP(t, ts, wsID, map[string]any{
		"serial": 1, "lineage": "force-identical-lineage", "md5": "md5-first",
		"state": base64.StdEncoding.EncodeToString([]byte("second")), "force": true,
	})
	if status != http.StatusConflict {
		t.Fatalf("force identical finalized: status = %d, want 409", status)
	}
	current := getCurrentVersion(t, ts, wsID)
	if got := stateVersionID(t, current); got != firstID {
		t.Fatalf("current id = %q, want unchanged %q", got, firstID)
	}
	if code, data := downloadState(t, ts, firstID); code != http.StatusOK || string(data) != "first" {
		t.Fatalf("current download = %d %q, want first", code, data)
	}

	// Different content is still a valid explicit restore.
	status, restored := createStateVersionHTTP(t, ts, wsID, map[string]any{
		"serial": 1, "lineage": "force-identical-lineage", "md5": "md5-restored",
		"state": base64.StdEncoding.EncodeToString([]byte("restored")), "force": true,
	})
	if status != http.StatusCreated {
		t.Fatalf("force different content restore: status = %d, want 201", status)
	}
	if got := stateVersionID(t, getCurrentVersion(t, ts, wsID)); got != stateVersionID(t, restored) {
		t.Fatal("different-content restore did not become current")
	}
}

func TestConcurrentSameSerialLeavesOneRowPerStatus(t *testing.T) {
	identity := testIdentity(t)
	store1, path := openTestStore(t, identity)
	store2, err := Open(path, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	ws := createTestWorkspace(t, store1, "same-serial-concurrency")

	old, err := store1.CreateStateVersion(ws.ID, 1, "concurrent-same", "md5-old", []byte(`{"old":true}`), nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}

	const writers = 16
	var wg sync.WaitGroup
	errCh := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			store := store1
			if i%2 == 1 {
				store = store2
			}
			_, err := store.CreateStateVersion(ws.ID, 1, "concurrent-same",
				fmt.Sprintf("md5-pending-%d", i), nil, nil, nil, false)
			if err != nil {
				errCh <- err
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent pending create: %v", err)
	}

	var pendingCount, finalizedCount int
	if err := store1.db.QueryRow(
		`SELECT COUNT(*) FROM state_versions WHERE workspace_id = ? AND serial = 1 AND status = 'pending'`, ws.ID,
	).Scan(&pendingCount); err != nil {
		t.Fatal(err)
	}
	if err := store1.db.QueryRow(
		`SELECT COUNT(*) FROM state_versions WHERE workspace_id = ? AND serial = 1 AND status = 'finalized'`, ws.ID,
	).Scan(&finalizedCount); err != nil {
		t.Fatal(err)
	}
	if pendingCount != 1 || finalizedCount != 1 {
		t.Fatalf("rows pending=%d finalized=%d, want 1/1", pendingCount, finalizedCount)
	}

	var pendingID string
	if err := store1.db.QueryRow(
		`SELECT id FROM state_versions WHERE workspace_id = ? AND serial = 1 AND status = 'pending'`, ws.ID,
	).Scan(&pendingID); err != nil {
		t.Fatal(err)
	}
	if err := store1.UploadState(pendingID, []byte(`{"new":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := store1.db.QueryRow(
		`SELECT COUNT(*) FROM state_versions WHERE workspace_id = ? AND serial = 1 AND status = 'finalized'`, ws.ID,
	).Scan(&finalizedCount); err != nil {
		t.Fatal(err)
	}
	if finalizedCount != 1 {
		t.Fatalf("finalized rows after upload = %d, want 1", finalizedCount)
	}
	if _, err := store1.GetStateVersion(old.ID); !IsAPIError(err, http.StatusNotFound) {
		t.Fatalf("old finalized error = %v, want 404", err)
	}
}
