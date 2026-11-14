package tacos

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func mustCreateWorkspaceHTTP(t *testing.T, ts *httptest.Server, name string) string {
	t.Helper()
	resp := postJSON(t, ts.URL+"/api/v2/organizations/dev/workspaces", map[string]any{
		"data": map[string]any{"attributes": map[string]any{"name": name}},
	}, "")
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create workspace: status %d: %s", resp.StatusCode, body)
	}
	body := decodeJSON(t, resp)
	resp.Body.Close()
	return body["data"].(map[string]any)["id"].(string)
}

func createStateVersionHTTP(t *testing.T, ts *httptest.Server, wsID string, attrs map[string]any) (int, map[string]any) {
	t.Helper()
	resp := postJSON(t, fmt.Sprintf("%s/api/v2/workspaces/%s/state-versions", ts.URL, wsID), map[string]any{
		"data": map[string]any{"attributes": attrs},
	}, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, decodeJSON(t, resp)
}

func stateVersionID(t *testing.T, body map[string]any) string {
	t.Helper()
	return body["data"].(map[string]any)["id"].(string)
}

func stateVersionAttrs(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	return body["data"].(map[string]any)["attributes"].(map[string]any)
}

func getCurrentVersion(t *testing.T, ts *httptest.Server, wsID string) map[string]any {
	t.Helper()
	resp := get(t, fmt.Sprintf("%s/api/v2/workspaces/%s/current-state-version", ts.URL, wsID), "")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("current state version: status %d: %s", resp.StatusCode, body)
	}
	body := decodeJSON(t, resp)
	resp.Body.Close()
	return body
}

func downloadState(t *testing.T, ts *httptest.Server, svID string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("%s/api/v2/state-versions/%s/download", ts.URL, svID))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func TestNoWildcardCORS(t *testing.T) {
	ts, _ := newTestServer(t, "")
	defer ts.Close()

	for _, path := range []string{
		"/api/v2/ping",
		"/api/v2/organizations/dev/entitlement-set",
	} {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		req.Header.Set("Origin", "https://evil.example")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("%s: Access-Control-Allow-Origin = %q, want empty", path, got)
		}
		if got := resp.Header.Get("Access-Control-Allow-Methods"); got != "" {
			t.Errorf("%s: Access-Control-Allow-Methods = %q, want empty", path, got)
		}
	}

	req, _ := http.NewRequest(http.MethodOptions, ts.URL+"/api/v2/ping", nil)
	req.Header.Set("Origin", "https://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		t.Error("OPTIONS preflight was accepted")
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("OPTIONS: Access-Control-Allow-Origin = %q, want empty", got)
	}
}

func TestForceClearedLowSerialMakesNewVersionCurrent(t *testing.T) {
	ts, _ := newTestServer(t, "")
	defer ts.Close()
	wsID := mustCreateWorkspaceHTTP(t, ts, "force-current")

	_, body5 := createStateVersionHTTP(t, ts, wsID, map[string]any{
		"serial": 5, "lineage": "force-lineage", "state": base64.StdEncoding.EncodeToString([]byte("v5")), "md5": "md5-v5",
	})
	id5 := stateVersionID(t, body5)

	// OpenTofu's cloud backend clears force on state push. A lower serial with
	// different content must still restore.
	status, body3 := createStateVersionHTTP(t, ts, wsID, map[string]any{
		"serial": 3, "lineage": "force-lineage", "state": base64.StdEncoding.EncodeToString([]byte("v3")), "md5": "md5-v3",
	})
	if status != http.StatusCreated {
		t.Fatalf("force low serial: status = %d, want 201", status)
	}
	id3 := stateVersionID(t, body3)

	current := getCurrentVersion(t, ts, wsID)
	curID := stateVersionID(t, current)
	if curID != id3 {
		t.Fatalf("current id = %q, want forced %q (old %q)", curID, id3, id5)
	}
	if serial := stateVersionAttrs(t, current)["serial"]; serial != float64(3) {
		t.Fatalf("current serial = %v, want 3", serial)
	}
	if code, data := downloadState(t, ts, curID); code != http.StatusOK || string(data) != "v3" {
		t.Fatalf("current download = %d %q, want 200 v3", code, data)
	}

	// Pushing a state whose computed serial equals current has the same
	// force-cleared shape; different content replaces the current row.
	status, body3b := createStateVersionHTTP(t, ts, wsID, map[string]any{
		"serial": 3, "lineage": "force-lineage", "state": base64.StdEncoding.EncodeToString([]byte("v3b")), "md5": "md5-v3b",
	})
	if status != http.StatusCreated {
		t.Fatalf("force-cleared equal serial: status = %d, want 201", status)
	}
	if got := stateVersionID(t, getCurrentVersion(t, ts, wsID)); got != stateVersionID(t, body3b) {
		t.Fatal("force-cleared equal serial did not become current")
	}
	if code, _ := downloadState(t, ts, id3); code != http.StatusNotFound {
		t.Fatalf("replaced equal-serial download = %d, want 404", code)
	}

	// A normal client continues from the restored serial, even though older
	// finalized versions still have higher serials.
	status, body4 := createStateVersionHTTP(t, ts, wsID, map[string]any{
		"serial": 4, "lineage": "force-lineage", "state": base64.StdEncoding.EncodeToString([]byte("v4")),
	})
	if status != http.StatusCreated {
		t.Fatalf("post-restore normal create: status = %d, want 201", status)
	}
	if got := stateVersionID(t, getCurrentVersion(t, ts, wsID)); got != stateVersionID(t, body4) {
		t.Fatal("normal create after restore did not become current")
	}

	// Reusing a serial that exists only in history replaces that historical
	// row instead of leaving a UNIQUE conflict.
	status, body5new := createStateVersionHTTP(t, ts, wsID, map[string]any{
		"serial": 5, "lineage": "force-lineage", "state": base64.StdEncoding.EncodeToString([]byte("v5-new")),
	})
	if status != http.StatusCreated {
		t.Fatalf("historical same serial create: status = %d, want 201", status)
	}
	if got := stateVersionID(t, getCurrentVersion(t, ts, wsID)); got != stateVersionID(t, body5new) {
		t.Fatal("historical same-serial replacement did not become current")
	}
	if code, _ := downloadState(t, ts, id5); code != http.StatusNotFound {
		t.Fatalf("replaced historical download = %d, want 404", code)
	}
	if code, data := downloadState(t, ts, stateVersionID(t, body5new)); code != http.StatusOK || string(data) != "v5-new" {
		t.Fatalf("replacement download = %d %q, want 200 v5-new", code, data)
	}
}

func TestForceSameSerialDifferentContentReplacesFinalized(t *testing.T) {
	ts, _ := newTestServer(t, "")
	defer ts.Close()
	wsID := mustCreateWorkspaceHTTP(t, ts, "force-same-serial")

	_, first := createStateVersionHTTP(t, ts, wsID, map[string]any{
		"serial": 2, "lineage": "same-serial", "md5": "md5-old",
		"state": base64.StdEncoding.EncodeToString([]byte("old")),
	})
	oldID := stateVersionID(t, first)

	status, second := createStateVersionHTTP(t, ts, wsID, map[string]any{
		"serial": 2, "lineage": "same-serial", "md5": "md5-new",
		"state": base64.StdEncoding.EncodeToString([]byte("new")), "force": true,
	})
	if status != http.StatusCreated {
		t.Fatalf("force same serial: status = %d, want 201", status)
	}
	newID := stateVersionID(t, second)
	if newID == oldID {
		t.Fatal("force same-serial replacement reused the old state version ID")
	}
	if code, _ := downloadState(t, ts, oldID); code != http.StatusNotFound {
		t.Fatalf("old download = %d, want 404", code)
	}
	if code, data := downloadState(t, ts, newID); code != http.StatusOK || string(data) != "new" {
		t.Fatalf("new download = %d %q, want 200 new", code, data)
	}
	if got := stateVersionID(t, getCurrentVersion(t, ts, wsID)); got != newID {
		t.Fatalf("current id = %q, want %q", got, newID)
	}
}

func TestPendingUploadRetryReplacesOldURL(t *testing.T) {
	ts, _ := newTestServer(t, "")
	defer ts.Close()
	wsID := mustCreateWorkspaceHTTP(t, ts, "pending-retry")

	_, first := createStateVersionHTTP(t, ts, wsID, map[string]any{"serial": 1, "lineage": "retry-lineage"})
	firstID := stateVersionID(t, first)
	firstUpload := stateVersionAttrs(t, first)["hosted-state-upload-url"].(string)

	status, second := createStateVersionHTTP(t, ts, wsID, map[string]any{"serial": 1, "lineage": "retry-lineage"})
	if status != http.StatusCreated {
		t.Fatalf("retry pending: status = %d, want 201", status)
	}
	secondID := stateVersionID(t, second)
	secondUpload := stateVersionAttrs(t, second)["hosted-state-upload-url"].(string)
	if secondID == firstID {
		t.Fatal("pending retry reused the old state version ID")
	}

	if resp := putBytes(t, firstUpload, []byte("stale")); resp.StatusCode != http.StatusNotFound {
		resp.Body.Close()
		t.Fatalf("old upload URL: status = %d, want 404", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := putBytes(t, secondUpload, []byte("fresh")); resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("new upload URL: status = %d, want 200", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if code, data := downloadState(t, ts, secondID); code != http.StatusOK || string(data) != "fresh" {
		t.Fatalf("retried download = %d %q, want 200 fresh", code, data)
	}
}

func TestExpiredPendingUploadRejected(t *testing.T) {
	ts, _ := newTestServer(t, "")
	defer ts.Close()
	wsID := mustCreateWorkspaceHTTP(t, ts, "expired-pending")

	_, pending := createStateVersionHTTP(t, ts, wsID, map[string]any{"serial": 1, "lineage": "expired-lineage"})
	pendingID := stateVersionID(t, pending)
	pendingUpload := stateVersionAttrs(t, pending)["hosted-state-upload-url"].(string)

	_, finalized := createStateVersionHTTP(t, ts, wsID, map[string]any{
		"serial": 2, "lineage": "expired-lineage", "state": base64.StdEncoding.EncodeToString([]byte("v2")),
	})
	finalizedID := stateVersionID(t, finalized)

	if resp := putBytes(t, pendingUpload, []byte("late")); resp.StatusCode != http.StatusConflict {
		resp.Body.Close()
		t.Fatalf("stale pending upload: status = %d, want 409", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if code, data := downloadState(t, ts, finalizedID); code != http.StatusOK || string(data) != "v2" {
		t.Fatalf("current download after rejected upload = %d %q, want 200 v2", code, data)
	}
	if code, _ := downloadState(t, ts, pendingID); code != http.StatusNotFound {
		t.Fatalf("pending download = %d, want 404", code)
	}

	// A real force restore must not be confused with a stale pending upload.
	status, restored := createStateVersionHTTP(t, ts, wsID, map[string]any{
		"serial": 1, "lineage": "expired-lineage", "state": base64.StdEncoding.EncodeToString([]byte("restored")), "force": true,
	})
	if status != http.StatusCreated {
		t.Fatalf("force restore: status = %d, want 201", status)
	}
	if code, data := downloadState(t, ts, stateVersionID(t, restored)); code != http.StatusOK || string(data) != "restored" {
		t.Fatalf("restored download = %d %q, want 200 restored", code, data)
	}
}

func TestCreateStateVersionUnknownWorkspace(t *testing.T) {
	ts, store := newTestServer(t, "")
	defer ts.Close()

	resp := postJSON(t, ts.URL+"/api/v2/workspaces/ws-missing/state-versions", map[string]any{
		"data": map[string]any{"attributes": map[string]any{
			"serial": 0, "lineage": "missing-workspace",
			"state": base64.StdEncoding.EncodeToString([]byte("{}")),
		}},
	}, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown workspace: status = %d, want 404", resp.StatusCode)
	}

	var stateRows, keyRows int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM state_versions`).Scan(&stateRows); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM lineage_keys`).Scan(&keyRows); err != nil {
		t.Fatal(err)
	}
	if stateRows != 0 || keyRows != 0 {
		t.Fatalf("failed create left rows: state=%d keys=%d", stateRows, keyRows)
	}
}

func TestNegativeSerialRejected(t *testing.T) {
	ts, store := newTestServer(t, "")
	defer ts.Close()
	wsID := mustCreateWorkspaceHTTP(t, ts, "negative-serial")

	resp := postJSON(t, fmt.Sprintf("%s/api/v2/workspaces/%s/state-versions", ts.URL, wsID), map[string]any{
		"data": map[string]any{"attributes": map[string]any{
			"serial": -1, "lineage": "negative",
			"state": base64.StdEncoding.EncodeToString([]byte("{}")),
		}},
	}, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("negative serial: status = %d, want 422", resp.StatusCode)
	}

	if _, err := store.CreateStateVersion(wsID, -5, "negative", "", []byte("{}"), nil, nil, true); !IsAPIError(err, http.StatusUnprocessableEntity) {
		t.Fatalf("store negative serial error = %v, want 422", err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM state_versions`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("state version rows = %d, want 0", count)
	}
}

func TestLockRejectsMalformedJSON(t *testing.T) {
	ts, store := newTestServer(t, "")
	defer ts.Close()
	wsID := mustCreateWorkspaceHTTP(t, ts, "lock-json")

	resp := postRaw(t, fmt.Sprintf("%s/api/v2/workspaces/%s/actions/lock", ts.URL, wsID), `{"reason":`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed lock body: status = %d, want 400", resp.StatusCode)
	}
	ws, err := store.GetWorkspaceByID(wsID)
	if err != nil {
		t.Fatal(err)
	}
	if ws.Locked {
		t.Fatal("malformed JSON still locked the workspace")
	}

	// Empty body and explicit null remain valid.
	resp = postRaw(t, fmt.Sprintf("%s/api/v2/workspaces/%s/actions/lock", ts.URL, wsID), "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("empty lock body: status = %d, want 200", resp.StatusCode)
	}
	unlockWorkspace(t, ts, wsID)
	resp = postRaw(t, fmt.Sprintf("%s/api/v2/workspaces/%s/actions/lock", ts.URL, wsID), "null")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("null lock body: status = %d, want 200", resp.StatusCode)
	}
}

func postRaw(t *testing.T, url, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testUserToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func unlockWorkspace(t *testing.T, ts *httptest.Server, wsID string) {
	t.Helper()
	resp := postJSON(t, fmt.Sprintf("%s/api/v2/workspaces/%s/actions/unlock", ts.URL, wsID), nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unlock: status = %d, want 200", resp.StatusCode)
	}
}

func TestEmptyStateUploadIsDownloadable(t *testing.T) {
	ts, _ := newTestServer(t, "")
	defer ts.Close()
	wsID := mustCreateWorkspaceHTTP(t, ts, "empty-upload")

	_, pending := createStateVersionHTTP(t, ts, wsID, map[string]any{"serial": 1, "lineage": "empty-upload"})
	uploadURL := stateVersionAttrs(t, pending)["hosted-state-upload-url"].(string)
	resp := putBytes(t, uploadURL, []byte{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("empty upload: status = %d, want 200", resp.StatusCode)
	}

	svID := stateVersionID(t, pending)
	code, data := downloadState(t, ts, svID)
	if code != http.StatusOK {
		t.Fatalf("empty download: status = %d, want 200", code)
	}
	if len(data) != 0 {
		t.Fatalf("empty download returned %d bytes", len(data))
	}
}

func TestBodyLimitConstants(t *testing.T) {
	if maxStateUploadBytes != 100<<20 {
		t.Fatalf("maxStateUploadBytes = %d", maxStateUploadBytes)
	}
	if minCreate := int64(base64.StdEncoding.EncodedLen(int(maxStateUploadBytes))); maxStateVersionCreateBodySize < minCreate {
		t.Fatalf("create body limit %d does not leave base64 headroom for %d bytes", maxStateVersionCreateBodySize, minCreate)
	}
	if maxPageSize != 100 {
		t.Fatalf("maxPageSize = %d", maxPageSize)
	}
}

func TestReadBodyRejectsOversize(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader("12345"))
	if _, ok := readBody(rec, req, 4); ok {
		t.Fatal("oversize body was accepted")
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

func TestHTTPBodyLimits(t *testing.T) {
	ts, _ := newTestServer(t, "")
	defer ts.Close()

	oversize := `{"data":{"attributes":{"name":"` + strings.Repeat("a", int(maxWorkspaceBodySize)+1) + `"}}}`
	resp := postRaw(t, ts.URL+"/api/v2/organizations/dev/workspaces", oversize)
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize workspace body: status = %d, want 413", resp.StatusCode)
	}

	wsID := mustCreateWorkspaceHTTP(t, ts, "body-limit")
	oversize = `{"reason":"` + strings.Repeat("a", int(maxLockBodySize)+1) + `"}`
	resp = postRaw(t, fmt.Sprintf("%s/api/v2/workspaces/%s/actions/lock", ts.URL, wsID), oversize)
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize lock body: status = %d, want 413", resp.StatusCode)
	}
}

func TestListPageSizeCapped(t *testing.T) {
	store, _ := openTestStore(t, testIdentity(t))
	for i := 0; i < maxPageSize+1; i++ {
		if _, err := store.CreateWorkspace("page-org", fmt.Sprintf("ws-%03d", i), ""); err != nil {
			t.Fatal(err)
		}
	}
	wss, total, err := store.ListWorkspaces("page-org", "", 1, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if total != maxPageSize+1 {
		t.Fatalf("total = %d, want %d", total, maxPageSize+1)
	}
	if len(wss) != maxPageSize {
		t.Fatalf("page length = %d, want %d", len(wss), maxPageSize)
	}
}

func TestConcurrentStoresNoBusySnapshot(t *testing.T) {
	identity := testIdentity(t)
	store1, path := openTestStore(t, identity)
	store2, err := Open(path, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	ws := createTestWorkspace(t, store1, "concurrent-writes")

	const writers = 12
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
			_, err := store.CreateStateVersion(ws.ID, i, "concurrent-lineage", "",
				[]byte(fmt.Sprintf(`{"serial":%d}`, i)), nil, nil, true)
			if err != nil {
				errCh <- err
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent create: %v", err)
	}

	const lockers = 8
	errCh = make(chan error, lockers)
	var successes int
	var mu sync.Mutex
	for i := 0; i < lockers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			store := store1
			if i%2 == 1 {
				store = store2
			}
			err := store.LockWorkspace(ws.ID, "concurrent-lock")
			if err == nil {
				mu.Lock()
				successes++
				mu.Unlock()
				return
			}
			if !IsAPIError(err, http.StatusConflict) {
				errCh <- err
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent lock: %v", err)
	}
	if successes != 1 {
		t.Fatalf("lock successes = %d, want 1", successes)
	}
}

func TestCorruptRecipientFingerprintIsDataError(t *testing.T) {
	store, _ := openTestStore(t, testIdentity(t))
	ws := createTestWorkspace(t, store, "recipient-corruption")
	sv, err := store.CreateStateVersion(ws.ID, 1, "recipient-lineage", "", []byte(`{}`), nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.db.Exec(`UPDATE wrapped_lineage_keys SET recipient_fingerprint = 'not-a-fingerprint'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetStateVersion(sv.ID); err == nil || IsAPIError(err, http.StatusForbidden) {
		t.Fatalf("malformed recipient fingerprint error = %v, want non-403 data error", err)
	}

	other := fingerprintString(bytes.Repeat([]byte{0x7f}, 32))
	if _, err := store.db.Exec(`UPDATE wrapped_lineage_keys SET recipient_fingerprint = ?`, other); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetStateVersion(sv.ID); !IsAPIError(err, http.StatusForbidden) {
		t.Fatalf("valid mismatched recipient error = %v, want 403", err)
	}
}

func TestPageSizeQueryCapped(t *testing.T) {
	ts, store := newTestServer(t, "")
	defer ts.Close()
	for i := 0; i < maxPageSize+1; i++ {
		if _, err := store.CreateWorkspace("dev", fmt.Sprintf("http-ws-%03d", i), ""); err != nil {
			t.Fatal(err)
		}
	}
	resp := get(t, ts.URL+"/api/v2/organizations/dev/workspaces?page[size]=1000", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	items := body["data"].([]any)
	if len(items) != maxPageSize {
		t.Fatalf("items = %d, want %d", len(items), maxPageSize)
	}
}
