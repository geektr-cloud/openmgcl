package tacos

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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
		{"invalid name", http.MethodPost, `{"name":"Bad/Name"}`, "live", http.StatusBadRequest},
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
