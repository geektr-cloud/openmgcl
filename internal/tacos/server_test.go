package tacos

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// helper to create a test server with a temp SQLite DB.
func newTestServer(t *testing.T, token string) (*httptest.Server, *Store) {
	t.Helper()
	f, err := os.CreateTemp("", "tacos-test-*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })

	store, err := Open(f.Name(), testIdentity(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	// Create server first with placeholder URL, then update after we know the real port.
	s := &Server{store: store, token: token}
	r := buildRouter(s)
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)

	// Now set the real server URL so absolute URLs are correct.
	s.serverURL = ts.URL
	return ts, store
}

func get(t *testing.T, url string, token string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func postJSON(t *testing.T, url string, body any, token string) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	json.NewEncoder(&buf).Encode(body)
	req, _ := http.NewRequest("POST", url, &buf)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func putBytes(t *testing.T, url string, data []byte) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("PUT", url, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decodeJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	return m
}

// ──────────────────────────────────────────────────────────────────────────────
// Tests
// ──────────────────────────────────────────────────────────────────────────────

func TestPingAndAPIVersion(t *testing.T) {
	ts, _ := newTestServer(t, "")
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v2/ping")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("ping status = %d, want 200", resp.StatusCode)
	}
	if v := resp.Header.Get("TFP-API-Version"); v != "2.5" {
		t.Errorf("TFP-API-Version = %q, want %q", v, "2.5")
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "{}" {
		t.Errorf("ping body = %q, want %q", string(body), "{}")
	}
}

func TestEntitlementSet(t *testing.T) {
	ts, _ := newTestServer(t, "")
	defer ts.Close()

	resp := get(t, ts.URL+"/api/v2/organizations/dev/entitlement-set", "")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	m := decodeJSON(t, resp)
	data := m["data"].(map[string]any)
	attrs := data["attributes"].(map[string]any)
	if attrs["operations"] != false {
		t.Errorf("operations = %v, want false", attrs["operations"])
	}
}

func TestWorkspaceCRUD(t *testing.T) {
	ts, _ := newTestServer(t, "")
	defer ts.Close()

	// Read non-existent → 404
	resp := get(t, ts.URL+"/api/v2/organizations/dev/workspaces/demo", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("get non-existent ws: status = %d, want 404", resp.StatusCode)
	}

	// Create
	resp = postJSON(t, ts.URL+"/api/v2/organizations/dev/workspaces", map[string]any{
		"data": map[string]any{
			"attributes": map[string]any{
				"name":              "demo",
				"terraform-version": "1.11.11",
			},
		},
	}, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("create ws: status = %d, want 201: %s", resp.StatusCode, body)
	}
	ws := decodeJSON(t, resp)
	attrs := ws["data"].(map[string]any)["attributes"].(map[string]any)
	if attrs["terraform-version"] != "1.11.11" {
		t.Errorf("terraform-version = %q, want %q", attrs["terraform-version"], "1.11.11")
	}
	if attrs["execution-mode"] != "local" {
		t.Errorf("execution-mode = %q, want %q", attrs["execution-mode"], "local")
	}

	// Read back
	resp = get(t, ts.URL+"/api/v2/organizations/dev/workspaces/demo", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get ws: status = %d, want 200", resp.StatusCode)
	}
	ws = decodeJSON(t, resp)
	attrs = ws["data"].(map[string]any)["attributes"].(map[string]any)
	if attrs["terraform-version"] != "1.11.11" {
		t.Errorf("terraform-version = %q, want %q", attrs["terraform-version"], "1.11.11")
	}

	// Create another workspace for list test
	postJSON(t, ts.URL+"/api/v2/organizations/dev/workspaces", map[string]any{
		"data": map[string]any{
			"attributes": map[string]any{"name": "demo-2"},
		},
	}, "").Body.Close()
	postJSON(t, ts.URL+"/api/v2/organizations/dev/workspaces", map[string]any{
		"data": map[string]any{
			"attributes": map[string]any{"name": "other"},
		},
	}, "").Body.Close()

	// List with prefix filter
	resp = get(t, ts.URL+"/api/v2/organizations/dev/workspaces?search[name]=demo", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list ws: status = %d, want 200", resp.StatusCode)
	}
	list := decodeJSON(t, resp)
	items := list["data"].([]any)
	if len(items) != 2 {
		t.Errorf("list count = %d, want 2", len(items))
	}
	meta := list["meta"].(map[string]any)
	pag := meta["pagination"].(map[string]any)
	if int(pag["total-count"].(float64)) != 2 {
		t.Errorf("total-count = %v, want 2", pag["total-count"])
	}
}

func TestFullStateWorkflow(t *testing.T) {
	ts, _ := newTestServer(t, "")
	defer ts.Close()

	// Create workspace
	postJSON(t, ts.URL+"/api/v2/organizations/dev/workspaces", map[string]any{
		"data": map[string]any{
			"attributes": map[string]any{"name": "ws1", "terraform-version": "1.11.11"},
		},
	}, "").Body.Close()

	ws := get(t, ts.URL+"/api/v2/organizations/dev/workspaces/ws1", "")
	wsData := decodeJSON(t, ws)
	ws.Body.Close()
	wsID := wsData["data"].(map[string]any)["id"].(string)

	// current-state-version 404
	resp := get(t, fmt.Sprintf("%s/api/v2/workspaces/%s/current-state-version", ts.URL, wsID), "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("current SV on empty: status = %d, want 404", resp.StatusCode)
	}

	// Lock
	resp = postJSON(t, fmt.Sprintf("%s/api/v2/workspaces/%s/actions/lock", ts.URL, wsID), map[string]any{
		"reason": "apply",
	}, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("lock: status = %d, want 200", resp.StatusCode)
	}

	// POST state-version without state (modern upload path) → 201 pending
	stateBytes := []byte(`{"version":4,"terraform_version":"1.11.11","serial":0,"lineage":"abc"}`)
	resp = postJSON(t, fmt.Sprintf("%s/api/v2/workspaces/%s/state-versions", ts.URL, wsID), map[string]any{
		"data": map[string]any{
			"attributes": map[string]any{
				"serial":  0,
				"lineage": "abc",
				"md5":     "test",
			},
		},
	}, "")
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create SV: status = %d, want 201: %s", resp.StatusCode, body)
	}
	svResp := decodeJSON(t, resp)
	resp.Body.Close()
	svData := svResp["data"].(map[string]any)
	svAttrs := svData["attributes"].(map[string]any)
	svID := svData["id"].(string)

	if svAttrs["status"] != "pending" {
		t.Errorf("SV status = %q, want %q", svAttrs["status"], "pending")
	}
	uploadURL, ok := svAttrs["hosted-state-upload-url"]
	if !ok || uploadURL == nil {
		t.Fatal("missing hosted-state-upload-url")
	}
	jsonUploadURL, ok := svAttrs["hosted-json-state-upload-url"]
	if !ok || jsonUploadURL == nil {
		t.Fatal("missing hosted-json-state-upload-url")
	}

	// PUT state-upload (no auth, foreign request)
	resp = putBytes(t, uploadURL.(string), stateBytes)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("state-upload: status = %d, want 200", resp.StatusCode)
	}

	// GET state-version should now be finalized
	resp = get(t, fmt.Sprintf("%s/api/v2/state-versions/%s", ts.URL, svID), "")
	svResp = decodeJSON(t, resp)
	resp.Body.Close()
	svAttrs = svResp["data"].(map[string]any)["attributes"].(map[string]any)
	if svAttrs["status"] != "finalized" {
		t.Errorf("SV status after upload = %q, want %q", svAttrs["status"], "finalized")
	}

	// Unlock
	resp = postJSON(t, fmt.Sprintf("%s/api/v2/workspaces/%s/actions/unlock", ts.URL, wsID), nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("unlock: status = %d, want 200", resp.StatusCode)
	}

	// current-state-version → 200
	resp = get(t, fmt.Sprintf("%s/api/v2/workspaces/%s/current-state-version", ts.URL, wsID), "")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("current SV: status = %d, want 200", resp.StatusCode)
	}
	curResp := decodeJSON(t, resp)
	resp.Body.Close()
	curAttrs := curResp["data"].(map[string]any)["attributes"].(map[string]any)
	downloadURL, ok := curAttrs["hosted-state-download-url"]
	if !ok || downloadURL == nil {
		t.Fatal("missing hosted-state-download-url")
	}

	// Download (no auth, foreign request)
	resp, err := http.Get(downloadURL.(string))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("download: status = %d, want 200", resp.StatusCode)
	}
	downloaded, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(downloaded, stateBytes) {
		t.Errorf("downloaded bytes don't match uploaded")
	}
}

func TestFallbackInlineState(t *testing.T) {
	ts, _ := newTestServer(t, "")
	defer ts.Close()

	// Create workspace
	postJSON(t, ts.URL+"/api/v2/organizations/dev/workspaces", map[string]any{
		"data": map[string]any{"attributes": map[string]any{"name": "fb"}},
	}, "").Body.Close()
	ws := get(t, ts.URL+"/api/v2/organizations/dev/workspaces/fb", "")
	wsData := decodeJSON(t, ws)
	ws.Body.Close()
	wsID := wsData["data"].(map[string]any)["id"].(string)

	stateBytes := []byte(`{"version":4,"serial":1}`)
	b64State := base64.StdEncoding.EncodeToString(stateBytes)

	// POST with inline state → 201 finalized
	resp := postJSON(t, fmt.Sprintf("%s/api/v2/workspaces/%s/state-versions", ts.URL, wsID), map[string]any{
		"data": map[string]any{
			"attributes": map[string]any{
				"serial":  1,
				"lineage": "fallback-lineage",
				"state":   b64State,
			},
		},
	}, "")
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create SV inline: status = %d, want 201: %s", resp.StatusCode, body)
	}
	svResp := decodeJSON(t, resp)
	resp.Body.Close()
	svAttrs := svResp["data"].(map[string]any)["attributes"].(map[string]any)
	if svAttrs["status"] != "finalized" {
		t.Errorf("SV status = %q, want %q", svAttrs["status"], "finalized")
	}

	// Download
	svID := svResp["data"].(map[string]any)["id"].(string)
	resp, err := http.Get(fmt.Sprintf("%s/api/v2/state-versions/%s/download", ts.URL, svID))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	downloaded, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(downloaded, stateBytes) {
		t.Errorf("downloaded bytes don't match inline state")
	}
}

func TestInvalidInlineJSONBase64(t *testing.T) {
	ts, _ := newTestServer(t, "")
	defer ts.Close()

	postJSON(t, ts.URL+"/api/v2/organizations/dev/workspaces", map[string]any{
		"data": map[string]any{"attributes": map[string]any{"name": "bad-json"}},
	}, "").Body.Close()
	ws := get(t, ts.URL+"/api/v2/organizations/dev/workspaces/bad-json", "")
	wsData := decodeJSON(t, ws)
	ws.Body.Close()
	wsID := wsData["data"].(map[string]any)["id"].(string)
	url := fmt.Sprintf("%s/api/v2/workspaces/%s/state-versions", ts.URL, wsID)

	for name, attrs := range map[string]map[string]any{
		"json-state":         {"serial": 1, "lineage": "invalid-b64", "json-state": "%%%"},
		"json-state-outputs": {"serial": 2, "lineage": "invalid-b64", "json-state-outputs": "%%%"},
	} {
		t.Run(name, func(t *testing.T) {
			resp := postJSON(t, url, map[string]any{
				"data": map[string]any{"attributes": attrs},
			}, "")
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnprocessableEntity {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want 422: %s", resp.StatusCode, body)
			}
		})
	}
}

func TestSerialConflict(t *testing.T) {
	ts, _ := newTestServer(t, "")
	defer ts.Close()

	postJSON(t, ts.URL+"/api/v2/organizations/dev/workspaces", map[string]any{
		"data": map[string]any{"attributes": map[string]any{"name": "sc"}},
	}, "").Body.Close()
	ws := get(t, ts.URL+"/api/v2/organizations/dev/workspaces/sc", "")
	wsData := decodeJSON(t, ws)
	ws.Body.Close()
	wsID := wsData["data"].(map[string]any)["id"].(string)

	stateBytes := []byte(`{"version":4,"serial":5}`)
	b64State := base64.StdEncoding.EncodeToString(stateBytes)

	// Create sv with serial=5
	resp := postJSON(t, fmt.Sprintf("%s/api/v2/workspaces/%s/state-versions", ts.URL, wsID), map[string]any{
		"data": map[string]any{
			"attributes": map[string]any{"serial": 5, "lineage": "serial-lineage", "state": b64State},
		},
	}, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first SV: status = %d, want 201", resp.StatusCode)
	}

	// serial=5 again → 409 conflict
	resp = postJSON(t, fmt.Sprintf("%s/api/v2/workspaces/%s/state-versions", ts.URL, wsID), map[string]any{
		"data": map[string]any{
			"attributes": map[string]any{"serial": 5, "lineage": "serial-lineage", "state": b64State},
		},
	}, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("duplicate serial: status = %d, want 409", resp.StatusCode)
	}

	// serial=3 (less than finalized max of 5) → 409
	resp = postJSON(t, fmt.Sprintf("%s/api/v2/workspaces/%s/state-versions", ts.URL, wsID), map[string]any{
		"data": map[string]any{
			"attributes": map[string]any{"serial": 3, "lineage": "serial-lineage", "state": b64State},
		},
	}, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("lower serial: status = %d, want 409", resp.StatusCode)
	}

	// force:true bypasses serial check
	resp = postJSON(t, fmt.Sprintf("%s/api/v2/workspaces/%s/state-versions", ts.URL, wsID), map[string]any{
		"data": map[string]any{
			"attributes": map[string]any{"serial": 3, "lineage": "serial-lineage", "state": b64State, "force": true},
		},
	}, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("force serial: status = %d, want 201", resp.StatusCode)
	}
}

func TestLockConflict(t *testing.T) {
	ts, _ := newTestServer(t, "")
	defer ts.Close()

	postJSON(t, ts.URL+"/api/v2/organizations/dev/workspaces", map[string]any{
		"data": map[string]any{"attributes": map[string]any{"name": "lc"}},
	}, "").Body.Close()
	ws := get(t, ts.URL+"/api/v2/organizations/dev/workspaces/lc", "")
	wsData := decodeJSON(t, ws)
	ws.Body.Close()
	wsID := wsData["data"].(map[string]any)["id"].(string)

	// Lock
	resp := postJSON(t, fmt.Sprintf("%s/api/v2/workspaces/%s/actions/lock", ts.URL, wsID), nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("lock: status = %d, want 200", resp.StatusCode)
	}

	// Lock again → 409
	resp = postJSON(t, fmt.Sprintf("%s/api/v2/workspaces/%s/actions/lock", ts.URL, wsID), nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("lock conflict: status = %d, want 409", resp.StatusCode)
	}

	// Unlock
	resp = postJSON(t, fmt.Sprintf("%s/api/v2/workspaces/%s/actions/unlock", ts.URL, wsID), nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unlock: status = %d, want 200", resp.StatusCode)
	}

	// Unlock again → 409
	resp = postJSON(t, fmt.Sprintf("%s/api/v2/workspaces/%s/actions/unlock", ts.URL, wsID), nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("unlock when not locked: status = %d, want 409", resp.StatusCode)
	}
}

func TestAuthentication(t *testing.T) {
	ts, _ := newTestServer(t, "secret-token")
	defer ts.Close()

	// No auth header → 401
	resp := get(t, ts.URL+"/api/v2/organizations/dev/entitlement-set", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no auth: status = %d, want 401", resp.StatusCode)
	}

	// Wrong token → 401
	resp = get(t, ts.URL+"/api/v2/organizations/dev/entitlement-set", "wrong-token")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong token: status = %d, want 401", resp.StatusCode)
	}

	// Correct token → 200
	resp = get(t, ts.URL+"/api/v2/organizations/dev/entitlement-set", "secret-token")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("correct token: status = %d, want 200", resp.StatusCode)
	}

	// Upload is auth-exempt: first create a pending SV
	postJSON(t, ts.URL+"/api/v2/organizations/dev/workspaces", map[string]any{
		"data": map[string]any{"attributes": map[string]any{"name": "auth-test"}},
	}, "secret-token").Body.Close()
	ws := get(t, ts.URL+"/api/v2/organizations/dev/workspaces/auth-test", "secret-token")
	wsData := decodeJSON(t, ws)
	ws.Body.Close()
	wsID := wsData["data"].(map[string]any)["id"].(string)

	resp = postJSON(t, fmt.Sprintf("%s/api/v2/workspaces/%s/state-versions", ts.URL, wsID), map[string]any{
		"data": map[string]any{"attributes": map[string]any{"serial": 0, "lineage": "auth-lineage"}},
	}, "secret-token")
	svResp := decodeJSON(t, resp)
	resp.Body.Close()
	uploadURL := svResp["data"].(map[string]any)["attributes"].(map[string]any)["hosted-state-upload-url"].(string)

	// PUT without auth → should succeed (auth-exempt)
	resp = putBytes(t, uploadURL, []byte(`{"version":4}`))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("upload without auth: status = %d, want 200", resp.StatusCode)
	}

	// Download without auth → should succeed
	svID := svResp["data"].(map[string]any)["id"].(string)
	resp, err := http.Get(fmt.Sprintf("%s/api/v2/state-versions/%s/download", ts.URL, svID))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("download without auth: status = %d, want 200", resp.StatusCode)
	}
}

func TestAPIVersionHeaderOnAllAPIRoutes(t *testing.T) {
	ts, _ := newTestServer(t, "")
	defer ts.Close()

	routes := []string{
		"/api/v2/ping",
		"/api/v2/organizations/x/entitlement-set",
		"/api/v2/organizations/x/workspaces",
	}
	for _, route := range routes {
		resp, err := http.Get(ts.URL + route)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if v := resp.Header.Get("TFP-API-Version"); v != "2.5" {
			t.Errorf("%s: TFP-API-Version = %q, want %q", route, v, "2.5")
		}
	}
}
