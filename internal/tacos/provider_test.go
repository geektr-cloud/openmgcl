package tacos

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/geektr-cloud/openmgcl/pkg/providerapi"
)

func newProviderTestServer(t *testing.T, userToken string) (*httptest.Server, *Store, *ProviderSessions, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mgcl.db")
	store, err := Open(path, testIdentity(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	providers := NewProviderSessions()
	ts := httptest.NewServer(NewRouter(store, "http://placeholder", userToken, providers))
	t.Cleanup(ts.Close)
	return ts, store, providers, path
}

func seedSecret(t *testing.T, path string, fn func(*SecretStore)) {
	t.Helper()
	store, err := OpenSecretStore(path, testIdentity(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	fn(store)
}

func providerRequest(t *testing.T, method, url, token string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestProviderSessionsIssueAndRevoke(t *testing.T) {
	providers := NewProviderSessions()
	token, err := providers.Issue()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < 32 {
		t.Fatalf("token entropy = %d bytes", len(raw))
	}
	if !providers.Authorize(token) {
		t.Fatal("issued token was not authorized")
	}
	if providers.Authorize(token + "x") {
		t.Fatal("modified token was authorized")
	}
	providers.RevokeAll()
	if providers.Authorize(token) {
		t.Fatal("revoked token was authorized")
	}
	if _, err := providers.Issue(); err == nil {
		t.Fatal("issue after revoke succeeded")
	}

	var nilProviders *ProviderSessions
	if nilProviders.Authorize(token) {
		t.Fatal("nil sessions authorized a token")
	}
}

func TestSecretsReadAPI(t *testing.T) {
	var logs bytes.Buffer
	oldLog := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(oldLog)

	ts, _, providers, path := newProviderTestServer(t, "user-token")
	token, err := providers.Issue()
	if err != nil {
		t.Fatal(err)
	}
	seedSecret(t, path, func(secrets *SecretStore) {
		if _, err := secrets.CreateSecret("bootstrap"); err != nil {
			t.Fatal(err)
		}
		if err := secrets.SetSecretValue("bootstrap", "access_key_id", []byte("AKIA")); err != nil {
			t.Fatal(err)
		}
		if err := secrets.SetSecretValue("bootstrap", "secret_access_key", []byte("s3cr3t\n")); err != nil {
			t.Fatal(err)
		}
		if _, err := secrets.CreateSecret("empty"); err != nil {
			t.Fatal(err)
		}
	})

	read := func(name string) (*http.Response, providerapi.ReadSecretResponse) {
		t.Helper()
		body, _ := json.Marshal(providerapi.ReadSecretRequest{Name: name})
		resp := providerRequest(t, http.MethodPost, ts.URL+providerapi.SecretsReadPath, token, bytes.NewReader(body))
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		var out providerapi.ReadSecretResponse
		if resp.StatusCode == http.StatusOK {
			if err := json.Unmarshal(data, &out); err != nil {
				t.Fatalf("decode response: %v", err)
			}
		}
		return resp, out
	}

	resp, out := read("bootstrap")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", resp.Header.Get("Cache-Control"))
	}
	if out.Name != "bootstrap" || out.Values["access_key_id"] != "AKIA" || out.Values["secret_access_key"] != "s3cr3t\n" {
		t.Fatalf("response = %+v", out)
	}

	resp, out = read("empty")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("empty status = %d", resp.StatusCode)
	}
	if out.Values == nil || len(out.Values) != 0 {
		t.Fatalf("empty values = %#v", out.Values)
	}

	// Latest values are read on every request.
	seedSecret(t, path, func(secrets *SecretStore) {
		if err := secrets.SetSecretValue("bootstrap", "access_key_id", []byte("AKIA2")); err != nil {
			t.Fatal(err)
		}
	})
	_, out = read("bootstrap")
	if out.Values["access_key_id"] != "AKIA2" {
		t.Fatalf("stale value = %q", out.Values["access_key_id"])
	}

	// Missing, invalid and oversized requests.
	cases := []struct {
		name   string
		method string
		body   string
		token  string
		want   int
	}{
		{"missing token", http.MethodPost, `{"name":"bootstrap"}`, "", http.StatusUnauthorized},
		{"wrong token", http.MethodPost, `{"name":"bootstrap"}`, "not-a-token", http.StatusUnauthorized},
		{"user token", http.MethodPost, `{"name":"bootstrap"}`, "user-token", http.StatusUnauthorized},
		{"get", http.MethodGet, "", "live", http.StatusMethodNotAllowed},
		{"malformed", http.MethodPost, `{"name":`, "live", http.StatusBadRequest},
		{"missing name", http.MethodPost, `{}`, "live", http.StatusBadRequest},
		{"invalid name", http.MethodPost, `{"name":"Bad//Name"}`, "live", http.StatusBadRequest},
		{"unknown", http.MethodPost, `{"name":"unknown"}`, "live", http.StatusNotFound},
		{"oversized", http.MethodPost, `{"name":"` + strings.Repeat("a", 9000) + `"}`, "live", http.StatusRequestEntityTooLarge},
		{"trailing", http.MethodPost, `{"name":"bootstrap"} {}`, "live", http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tok := tc.token
			if tok == "live" {
				tok = token
			}
			resp := providerRequest(t, tc.method, ts.URL+providerapi.SecretsReadPath, tok, strings.NewReader(tc.body))
			defer resp.Body.Close()
			data, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d (body %q)", resp.StatusCode, tc.want, data)
			}
			if resp.Header.Get("Cache-Control") != "no-store" {
				t.Fatalf("error response Cache-Control = %q", resp.Header.Get("Cache-Control"))
			}
			if bytes.Contains(data, []byte(token)) || bytes.Contains(data, []byte("s3cr3t")) {
				t.Fatalf("error response leaked token or value: %s", data)
			}
		})
	}

	// Provider tokens are confined to the fixed secrets:read path, even when
	// the request targets an otherwise unknown route.
	resp = providerRequest(t, http.MethodPost, ts.URL+"/api/mgcl/v1/other", token, strings.NewReader(`{"name":"bootstrap"}`))
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unknown route status = %d, want 401", resp.StatusCode)
	}

	if strings.Contains(logs.String(), token) || strings.Contains(logs.String(), "s3cr3t") || strings.Contains(logs.String(), "AKIA") {
		t.Fatalf("logs leaked token or values: %s", logs.String())
	}
}

func TestSecretsReadAccessModes(t *testing.T) {
	ts, _, providers, path := newProviderTestServer(t, "user-token")
	token, err := providers.Issue()
	if err != nil {
		t.Fatal(err)
	}
	seedSecret(t, path, func(secrets *SecretStore) {
		if _, err := secrets.CreateSecret("mixed"); err != nil {
			t.Fatal(err)
		}
		if err := secrets.SetSecretValueWithPublic("mixed", "public-key", []byte("public-value"), true); err != nil {
			t.Fatal(err)
		}
		if err := secrets.SetSecretValue("mixed", "ephemeral-key", []byte("ephemeral-value")); err != nil {
			t.Fatal(err)
		}
		if _, err := secrets.CreateSecret("ephemeral-secret"); err != nil {
			t.Fatal(err)
		}
		if err := secrets.SetSecretValue("ephemeral-secret", "key", []byte("ephemeral-value")); err != nil {
			t.Fatal(err)
		}
	})

	read := func(name, access string) (int, providerapi.ReadSecretResponse, string, string) {
		t.Helper()
		request := map[string]string{"name": name}
		if access != "" {
			request["access"] = access
		}
		body, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		resp := providerRequest(t, http.MethodPost, ts.URL+providerapi.SecretsReadPath, token, bytes.NewReader(body))
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		var out providerapi.ReadSecretResponse
		if resp.StatusCode == http.StatusOK {
			if err := json.Unmarshal(data, &out); err != nil {
				t.Fatalf("decode response: %v", err)
			}
		}
		var errBody map[string]string
		_ = json.Unmarshal(data, &errBody)
		return resp.StatusCode, out, errBody["error"], resp.Header.Get("Cache-Control")
	}

	cases := []struct {
		name       string
		secret     string
		access     string
		wantStatus int
		wantValues map[string]string
		wantMarks  map[string]bool
		wantError  string
	}{
		{
			name:       "empty access returns all KVs",
			secret:     "mixed",
			wantStatus: http.StatusOK,
			wantValues: map[string]string{"public-key": "public-value", "ephemeral-key": "ephemeral-value"},
			wantMarks:  map[string]bool{"public-key": false, "ephemeral-key": true},
		},
		{
			name:       "explicit ephemeral returns all KVs",
			secret:     "mixed",
			access:     providerapi.SecretAccessEphemeral,
			wantStatus: http.StatusOK,
			wantValues: map[string]string{"public-key": "public-value", "ephemeral-key": "ephemeral-value"},
			wantMarks:  map[string]bool{"public-key": false, "ephemeral-key": true},
		},
		{
			name:       "public returns only public KVs",
			secret:     "mixed",
			access:     providerapi.SecretAccessPublic,
			wantStatus: http.StatusOK,
			wantValues: map[string]string{"public-key": "public-value"},
			wantMarks:  map[string]bool{"public-key": false},
		},
		{
			name:       "public read of an ephemeral-only secret returns empty maps",
			secret:     "ephemeral-secret",
			access:     providerapi.SecretAccessPublic,
			wantStatus: http.StatusOK,
			wantValues: map[string]string{},
			wantMarks:  map[string]bool{},
		},
		{"invalid access", "mixed", "private", http.StatusBadRequest, nil, nil, "invalid access"},
		{"not found", "missing", providerapi.SecretAccessPublic, http.StatusNotFound, nil, nil, "secret not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, out, message, cacheControl := read(tc.secret, tc.access)
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d (error %q)", status, tc.wantStatus, message)
			}
			if cacheControl != "no-store" {
				t.Fatalf("Cache-Control = %q", cacheControl)
			}
			if tc.wantError != "" && message != tc.wantError {
				t.Fatalf("error = %q, want %q", message, tc.wantError)
			}
			if tc.wantValues != nil {
				if out.Name != tc.secret || len(out.Values) != len(tc.wantValues) {
					t.Fatalf("response = %+v", out)
				}
				for key, want := range tc.wantValues {
					if out.Values[key] != want {
						t.Fatalf("value %q = %q, want %q", key, out.Values[key], want)
					}
				}
				if len(out.EphemeralMarks) != len(tc.wantMarks) {
					t.Fatalf("marks = %#v", out.EphemeralMarks)
				}
				for key, want := range tc.wantMarks {
					if got, ok := out.EphemeralMarks[key]; !ok || got != want {
						t.Fatalf("mark %q = %t (present %t), want %t", key, got, ok, want)
					}
				}
			}
			if strings.Contains(message, "ephemeral-value") || strings.Contains(message, "public-value") {
				t.Fatalf("error leaked a value: %q", message)
			}
		})
	}
}

func TestSecretsReadAccessScopesDecryption(t *testing.T) {
	mutations := []struct {
		name string
		key  string
	}{
		{"corrupt ephemeral KV", "ephemeral-key"},
		{"corrupt public KV", "public-key"},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			ts, store, providers, path := newProviderTestServer(t, "")
			seedSecret(t, path, func(secrets *SecretStore) {
				if _, err := secrets.CreateSecret("mixed"); err != nil {
					t.Fatal(err)
				}
				if err := secrets.SetSecretValueWithPublic("mixed", "public-key", []byte("public-value"), true); err != nil {
					t.Fatal(err)
				}
				if err := secrets.SetSecretValue("mixed", "ephemeral-key", []byte("ephemeral-value")); err != nil {
					t.Fatal(err)
				}
			})
			if _, err := store.db.Exec(
				`UPDATE secret_kvs SET ciphertext = substr(ciphertext, 1, 10) WHERE key = ?`, mutation.key,
			); err != nil {
				t.Fatal(err)
			}
			token, err := providers.Issue()
			if err != nil {
				t.Fatal(err)
			}

			read := func(access string) (int, string) {
				t.Helper()
				body := fmt.Sprintf(`{"name":"mixed","access":%q}`, access)
				resp := providerRequest(t, http.MethodPost, ts.URL+providerapi.SecretsReadPath, token, strings.NewReader(body))
				defer resp.Body.Close()
				data, _ := io.ReadAll(resp.Body)
				return resp.StatusCode, string(data)
			}

			if mutation.key == "ephemeral-key" {
				status, data := read(providerapi.SecretAccessPublic)
				if status != http.StatusOK {
					t.Fatalf("public read status = %d, want 200 (body %q)", status, data)
				}
				if !strings.Contains(data, "public-value") || strings.Contains(data, "ephemeral-value") {
					t.Fatalf("public read body = %s", data)
				}
				if status, data := read(providerapi.SecretAccessEphemeral); status != http.StatusInternalServerError {
					t.Fatalf("all read status = %d, want 500 (body %q)", status, data)
				}
			} else {
				if status, data := read(providerapi.SecretAccessPublic); status != http.StatusInternalServerError {
					t.Fatalf("public read status = %d, want 500 (body %q)", status, data)
				}
			}
		})
	}
}

func TestProviderSessionsConcurrentAuthorizeAndRevoke(t *testing.T) {
	providers := NewProviderSessions()
	token, err := providers.Issue()
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				providers.Authorize(token)
				_, _ = providers.Issue()
			}
			providers.RevokeAll()
		}()
	}
	wg.Wait()
	if providers.Authorize(token) {
		t.Fatal("token was authorized after concurrent revocation")
	}
}

func TestSecretsReadRevokedAfterDecrypt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mgcl.db")
	store, err := Open(path, testIdentity(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	seedSecret(t, path, func(secrets *SecretStore) {
		if _, err := secrets.CreateSecret("bootstrap"); err != nil {
			t.Fatal(err)
		}
		if err := secrets.SetSecretValue("bootstrap", "key", []byte("must-not-leak")); err != nil {
			t.Fatal(err)
		}
	})

	providers := NewProviderSessions()
	token, err := providers.Issue()
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{store: store, providers: providers}
	s.afterProviderDecrypt = func() { providers.RevokeAll() }
	ts := httptest.NewServer(buildRouter(s))
	defer ts.Close()

	resp := providerRequest(t, http.MethodPost, ts.URL+providerapi.SecretsReadPath, token,
		strings.NewReader(`{"name":"bootstrap"}`))
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if bytes.Contains(data, []byte("must-not-leak")) {
		t.Fatalf("revoked session received plaintext: %s", data)
	}
}

func TestSecretsReadPublicRevokedAfterDecrypt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mgcl.db")
	store, err := Open(path, testIdentity(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	seedSecret(t, path, func(secrets *SecretStore) {
		if _, err := secrets.CreateSecret("public-secret"); err != nil {
			t.Fatal(err)
		}
		if err := secrets.SetSecretValueWithPublic("public-secret", "key", []byte("must-not-leak"), true); err != nil {
			t.Fatal(err)
		}
	})

	providers := NewProviderSessions()
	token, err := providers.Issue()
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{store: store, providers: providers}
	s.afterProviderDecrypt = func() { providers.RevokeAll() }
	ts := httptest.NewServer(buildRouter(s))
	defer ts.Close()

	resp := providerRequest(t, http.MethodPost, ts.URL+providerapi.SecretsReadPath, token,
		strings.NewReader(`{"name":"public-secret","access":"public"}`))
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if bytes.Contains(data, []byte("must-not-leak")) {
		t.Fatalf("revoked session received plaintext: %s", data)
	}
}

func TestSecretsWriteAndDeleteAPI(t *testing.T) {
	ts, store, providers, _ := newProviderTestServer(t, "user-token")
	token, err := providers.Issue()
	if err != nil {
		t.Fatal(err)
	}

	write := func(tok, body string) (*http.Response, string) {
		t.Helper()
		resp := providerRequest(t, http.MethodPost, ts.URL+providerapi.SecretKVsWritePath, tok, strings.NewReader(body))
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return resp, string(data)
	}
	del := func(tok, body string) (*http.Response, string) {
		t.Helper()
		resp := providerRequest(t, http.MethodPost, ts.URL+providerapi.SecretKVsDeletePath, tok, strings.NewReader(body))
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return resp, string(data)
	}

	resp, data := write(token, `{"key":"managed/key","value":"v1","is_ephemeral":true}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("write status = %d (body %q)", resp.StatusCode, data)
	}
	if strings.Contains(data, "v1") {
		t.Fatalf("write response leaked the value: %s", data)
	}
	var written providerapi.WriteSecretKVResponse
	if err := json.Unmarshal([]byte(data), &written); err != nil {
		t.Fatal(err)
	}
	if written.Name != "managed" || written.Key != "key" || !written.IsEphemeral {
		t.Fatalf("write response = %+v", written)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("write Cache-Control = %q", resp.Header.Get("Cache-Control"))
	}

	read := func(access string) providerapi.ReadSecretResponse {
		t.Helper()
		body := fmt.Sprintf(`{"name":"managed","access":%q}`, access)
		resp := providerRequest(t, http.MethodPost, ts.URL+providerapi.SecretsReadPath, token, strings.NewReader(body))
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("read status = %d (body %q)", resp.StatusCode, raw)
		}
		var out providerapi.ReadSecretResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	out := read(providerapi.SecretAccessEphemeral)
	if out.Values["key"] != "v1" || !out.EphemeralMarks["key"] {
		t.Fatalf("read after write = %+v", out)
	}

	// An omitted is_ephemeral field means ephemeral, never public.
	resp, data = write(token, `{"key":"managed/defaulted","value":"defaulted"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("defaulted write status = %d (body %q)", resp.StatusCode, data)
	}
	var defaulted providerapi.WriteSecretKVResponse
	if err := json.Unmarshal([]byte(data), &defaulted); err != nil {
		t.Fatal(err)
	}
	if defaulted.Name != "managed" || defaulted.Key != "defaulted" || !defaulted.IsEphemeral {
		t.Fatalf("defaulted write response = %+v", defaulted)
	}
	out = read(providerapi.SecretAccessEphemeral)
	if out.Values["defaulted"] != "defaulted" || !out.EphemeralMarks["defaulted"] {
		t.Fatalf("defaulted read = %+v", out)
	}
	out = read(providerapi.SecretAccessPublic)
	if _, ok := out.Values["defaulted"]; ok {
		t.Fatalf("defaulted KV appeared in a public read: %+v", out)
	}

	// Updating changes the value and mark.
	resp, data = write(token, `{"key":"managed/key","value":"v2","is_ephemeral":false}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update status = %d (body %q)", resp.StatusCode, data)
	}
	out = read(providerapi.SecretAccessEphemeral)
	if out.Values["key"] != "v2" || out.EphemeralMarks["key"] {
		t.Fatalf("read after update = %+v", out)
	}
	out = read(providerapi.SecretAccessPublic)
	if out.Values["key"] != "v2" || len(out.Values) != 1 {
		t.Fatalf("public read after update = %+v", out)
	}

	// Delete is idempotent and keeps the empty Secret.
	resp, data = del(token, `{"key":"managed/key"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d (body %q)", resp.StatusCode, data)
	}
	var deleted providerapi.DeleteSecretKVResponse
	if err := json.Unmarshal([]byte(data), &deleted); err != nil {
		t.Fatal(err)
	}
	if deleted.Name != "managed" || deleted.Key != "key" {
		t.Fatalf("delete response = %+v", deleted)
	}
	for _, body := range []string{`{"key":"managed/key"}`, `{"key":"missing/key"}`, `{"key":"managed/defaulted"}`} {
		resp, data = del(token, body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("idempotent delete %s status = %d (body %q)", body, resp.StatusCode, data)
		}
	}
	out = read(providerapi.SecretAccessEphemeral)
	if len(out.Values) != 0 || len(out.EphemeralMarks) != 0 {
		t.Fatalf("read after delete = %+v", out)
	}
	var secrets, deks int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM secrets`).Scan(&secrets); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM secret_deks`).Scan(&deks); err != nil {
		t.Fatal(err)
	}
	if secrets != 1 || deks != 1 {
		t.Fatalf("delete removed the empty Secret: secrets/deks = %d/%d", secrets, deks)
	}

	// Invalid inputs.
	if resp, _ = write(token, `{"key":"bad"}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid reference status = %d, want 400", resp.StatusCode)
	}
	if resp, _ = write(token, `{"key":`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid JSON status = %d, want 400", resp.StatusCode)
	}
	if resp, _ = write(token, `{"key":"managed/key","value":"`+strings.Repeat("x", MaxSecretValueBytes+1)+`","is_ephemeral":true}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized value status = %d, want 400", resp.StatusCode)
	}
	if resp, _ = write(token, `{"key":"managed/key","value":"`+strings.Repeat("x", 600<<10)+`","is_ephemeral":true}`); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body status = %d, want 413", resp.StatusCode)
	}
	if resp, _ = write(token, `{"key":"managed/key","value":"v","is_ephemeral":true} trailing`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("trailing JSON status = %d, want 400", resp.StatusCode)
	}

	// Method, user token and revoked token.
	getResp := providerRequest(t, http.MethodGet, ts.URL+providerapi.SecretKVsWritePath, token, nil)
	getResp.Body.Close()
	if getResp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", getResp.StatusCode)
	}
	if resp, _ = write("user-token", `{"key":"x/y","value":"v","is_ephemeral":true}`); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("user token write status = %d, want 401", resp.StatusCode)
	}
	providers.RevokeAll()
	if resp, _ = write(token, `{"key":"x/y","value":"v","is_ephemeral":true}`); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked token write status = %d, want 401", resp.StatusCode)
	}
	if resp, _ = del(token, `{"key":"x/y"}`); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked token delete status = %d, want 401", resp.StatusCode)
	}
}

func TestSecretsWriteWrongRecipient(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mgcl.db")
	owner := testIdentityNamed(t, "owner")
	ownerStore, err := OpenSecretStore(path, owner, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ownerStore.CreateSecret("owned"); err != nil {
		t.Fatal(err)
	}
	if err := ownerStore.SetSecretValue("owned", "key", []byte("value")); err != nil {
		t.Fatal(err)
	}
	if err := ownerStore.Close(); err != nil {
		t.Fatal(err)
	}

	wrongStore, err := Open(path, testIdentityNamed(t, "wrong"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { wrongStore.Close() })
	providers := NewProviderSessions()
	ts := httptest.NewServer(NewRouter(wrongStore, "http://placeholder", "", providers))
	defer ts.Close()
	token, err := providers.Issue()
	if err != nil {
		t.Fatal(err)
	}

	writeResp := providerRequest(t, http.MethodPost, ts.URL+providerapi.SecretKVsWritePath, token,
		strings.NewReader(`{"key":"owned/key","value":"changed","is_ephemeral":true}`))
	writeBody, _ := io.ReadAll(writeResp.Body)
	writeResp.Body.Close()
	if writeResp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong recipient write status = %d, want 403 (body %q)", writeResp.StatusCode, writeBody)
	}
	if strings.TrimSpace(string(writeBody)) != `{"error":"secret key access denied"}` {
		t.Fatalf("wrong recipient write body = %s", writeBody)
	}

	readResp := providerRequest(t, http.MethodPost, ts.URL+providerapi.SecretsReadPath, token,
		strings.NewReader(`{"name":"owned","access":"ephemeral"}`))
	readBody, _ := io.ReadAll(readResp.Body)
	readResp.Body.Close()
	if readResp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong recipient read status = %d, want 403 (body %q)", readResp.StatusCode, readBody)
	}
	if strings.TrimSpace(string(readBody)) != `{"error":"secret key access denied"}` {
		t.Fatalf("wrong recipient read body = %s", readBody)
	}
	if strings.Contains(string(readBody), "value") {
		t.Fatalf("wrong recipient read leaked a value: %s", readBody)
	}
}

func TestSecretsWriteLimitError(t *testing.T) {
	ts, _, providers, path := newProviderTestServer(t, "")
	token, err := providers.Issue()
	if err != nil {
		t.Fatal(err)
	}
	seedSecret(t, path, func(secrets *SecretStore) {
		if _, err := secrets.CreateSecret("full"); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < MaxSecretKVs; i++ {
			if err := secrets.SetSecretValue("full", fmt.Sprintf("key%02d", i), []byte("v")); err != nil {
				t.Fatal(err)
			}
		}
	})

	resp := providerRequest(t, http.MethodPost, ts.URL+providerapi.SecretKVsWritePath, token,
		strings.NewReader(`{"key":"full/extra","value":"v","is_ephemeral":true}`))
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("limit write status = %d, want 400 (body %q)", resp.StatusCode, body)
	}
	if strings.TrimSpace(string(body)) != `{"error":"secret limit exceeded"}` {
		t.Fatalf("limit write body = %s", body)
	}
}

func TestSecretsWriteConcurrentHTTP(t *testing.T) {
	ts, store, providers, _ := newProviderTestServer(t, "")
	token, err := providers.Issue()
	if err != nil {
		t.Fatal(err)
	}

	const n = 12
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"key":"shared/key%02d","value":"v%d","is_ephemeral":true}`, i, i)
			resp := providerRequest(t, http.MethodPost, ts.URL+providerapi.SecretKVsWritePath, token, strings.NewReader(body))
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				errs <- fmt.Errorf("write %d status = %d", i, resp.StatusCode)
				return
			}
			errs <- nil
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
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
}

func TestSecretsReadContextCancellation(t *testing.T) {
	_, store, _, path := newProviderTestServer(t, "")
	seedSecret(t, path, func(secrets *SecretStore) {
		if _, err := secrets.CreateSecret("bootstrap"); err != nil {
			t.Fatal(err)
		}
		if err := secrets.SetSecretValue("bootstrap", "key", []byte("value")); err != nil {
			t.Fatal(err)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.ReadSecretValuesContext(ctx, "bootstrap"); err == nil {
		t.Fatal("canceled request context was ignored")
	}
}

type failingReadDeadlineWriter struct {
	*httptest.ResponseRecorder
}

func (w *failingReadDeadlineWriter) SetReadDeadline(time.Time) error {
	return errors.New("read deadline unsupported for this test writer")
}

func TestSecretsReadDeadlineErrorIsNotSwallowed(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "mgcl.db"), testIdentity(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	providers := NewProviderSessions()
	token, err := providers.Issue()
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{store: store, providers: providers}
	req := httptest.NewRequest(http.MethodPost, providerapi.SecretsReadPath, strings.NewReader(`{"name":"bootstrap"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	recorder := &failingReadDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	s.secretsReadHandler(recorder, req)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", recorder.Code)
	}
}

func TestProviderTokenCannotAccessTACOS(t *testing.T) {
	for _, userToken := range []string{"", "user-token"} {
		t.Run("user-token-"+userToken, func(t *testing.T) {
			ts, _, providers, _ := newProviderTestServer(t, userToken)
			token, err := providers.Issue()
			if err != nil {
				t.Fatal(err)
			}
			routes := []struct {
				method string
				path   string
			}{
				{http.MethodGet, "/.well-known/terraform.json"},
				{http.MethodGet, "/api/v2/ping"},
				{http.MethodGet, "/api/v2/organizations/local/workspaces"},
				{http.MethodGet, "/api/v2/state-versions/sv/download"},
				{http.MethodPut, "/api/v2/state-versions/sv/state-upload"},
				{http.MethodPut, "/api/v2/state-versions/sv/json-state-upload"},
				{http.MethodPost, "/api/v2/workspaces/ws/actions/lock"},
			}
			for _, route := range routes {
				resp := providerRequest(t, route.method, ts.URL+route.path, token, nil)
				resp.Body.Close()
				if resp.StatusCode != http.StatusUnauthorized {
					t.Errorf("%s %s: status = %d, want 401", route.method, route.path, resp.StatusCode)
				}
			}
		})
	}
}

func TestSecretsReadAPICorruptData(t *testing.T) {
	ts, store, providers, path := newProviderTestServer(t, "")
	seedSecret(t, path, func(secrets *SecretStore) {
		if _, err := secrets.CreateSecret("bootstrap"); err != nil {
			t.Fatal(err)
		}
		if err := secrets.SetSecretValue("bootstrap", "key", []byte("visible-plaintext")); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := store.db.Exec(`UPDATE secret_kvs SET ciphertext = substr(ciphertext, 1, 10)`); err != nil {
		t.Fatal(err)
	}
	token, err := providers.Issue()
	if err != nil {
		t.Fatal(err)
	}
	resp := providerRequest(t, http.MethodPost, ts.URL+providerapi.SecretsReadPath, token, strings.NewReader(`{"name":"bootstrap"}`))
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", resp.Header.Get("Cache-Control"))
	}
	if bytes.Contains(data, []byte("visible-plaintext")) || bytes.Contains(data, []byte("substr")) {
		t.Fatalf("error response leaked data: %s", data)
	}
}

func TestSecretsReadAPIRequiresLiveSession(t *testing.T) {
	ts, _, providers, _ := newProviderTestServer(t, "")
	token, err := providers.Issue()
	if err != nil {
		t.Fatal(err)
	}
	body := strings.NewReader(`{"name":"anything"}`)
	resp := providerRequest(t, http.MethodPost, ts.URL+providerapi.SecretsReadPath, token, body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for unknown secret", resp.StatusCode)
	}

	providers.RevokeAll()
	body = strings.NewReader(`{"name":"anything"}`)
	resp = providerRequest(t, http.MethodPost, ts.URL+providerapi.SecretsReadPath, token, body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked token status = %d, want 401", resp.StatusCode)
	}
}

func TestSecretsReadAPIWithoutSessions(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "mgcl.db"), testIdentity(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ts := httptest.NewServer(NewRouter(store, "http://placeholder", ""))
	defer ts.Close()

	resp := providerRequest(t, http.MethodPost, ts.URL+providerapi.SecretsReadPath, "anything", strings.NewReader(`{"name":"bootstrap"}`))
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}
