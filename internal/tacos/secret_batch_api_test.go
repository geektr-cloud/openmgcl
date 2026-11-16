package tacos

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/geektr-cloud/openmgcl/pkg/providerapi"
)

func TestSecretKVsWriteBatchAPI(t *testing.T) {
	ts, store, providers, path := newProviderTestServer(t, "user-token")
	token, err := providers.Issue()
	if err != nil {
		t.Fatal(err)
	}
	name := "aws-605592676484-cn/scarborough"

	post := func(body []byte) (*http.Response, []byte) {
		t.Helper()
		resp := providerRequest(t, http.MethodPost, ts.URL+providerapi.SecretKVsWriteBatchPath, token, bytes.NewReader(body))
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return resp, data
	}

	public := false
	body, err := json.Marshal(providerapi.WriteSecretKVsRequest{
		Name: name,
		KVs: []providerapi.SecretKVWriteItem{
			{Key: "token", Value: "secret-token-value"},
			{Key: "region", Value: "us-east-1", IsEphemeral: &public},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, data := post(body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create status = %d (body %q)", resp.StatusCode, data)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", resp.Header.Get("Cache-Control"))
	}
	if bytes.Contains(data, []byte("secret-token-value")) || bytes.Contains(data, []byte("us-east-1")) || bytes.Contains(data, []byte("ciphertext")) {
		t.Fatalf("batch response leaked value or ciphertext: %s", data)
	}
	var out providerapi.WriteSecretKVsResponse
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out.Name != name || len(out.KVs) != 2 || out.KVs[0].Key != "region" || out.KVs[0].IsEphemeral || out.KVs[1].Key != "token" || !out.KVs[1].IsEphemeral {
		t.Fatalf("response = %+v", out)
	}

	values, marks, err := store.ReadSecretValuesContextAccess(context.Background(), name, true)
	if err != nil {
		t.Fatal(err)
	}
	if values["region"] != "us-east-1" || values["token"] != "secret-token-value" || marks["region"] || !marks["token"] {
		t.Fatalf("values/marks = %#v/%#v", values, marks)
	}

	// A second batch deletes one KV and preserves every unmentioned KV.
	body, err = json.Marshal(providerapi.WriteSecretKVsRequest{
		Name:       name,
		KVs:        []providerapi.SecretKVWriteItem{{Key: "zone", Value: "a"}},
		DeleteKeys: []string{"token"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, data = post(body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update status = %d (body %q)", resp.StatusCode, data)
	}
	values, marks, err = store.ReadSecretValuesContextAccess(context.Background(), name, true)
	if err != nil {
		t.Fatal(err)
	}
	if values["region"] != "us-east-1" || values["zone"] != "a" || !marks["zone"] {
		t.Fatalf("values/marks after update = %#v/%#v", values, marks)
	}
	if _, ok := values["token"]; ok {
		t.Fatal("deleted KV is still present")
	}

	// ensure=true creates an empty Secret.
	emptyName := "aws-605592676484-cn/empty"
	body, err = json.Marshal(providerapi.WriteSecretKVsRequest{Name: emptyName, Ensure: true})
	if err != nil {
		t.Fatal(err)
	}
	resp, data = post(body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ensure empty status = %d (body %q)", resp.StatusCode, data)
	}
	out = providerapi.WriteSecretKVsResponse{}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out.KVs == nil || len(out.KVs) != 0 {
		t.Fatalf("ensure empty response = %+v", out)
	}
	if secretRowCount(t, store, emptyName) != 1 {
		t.Fatal("ensure did not create the Secret")
	}

	// A pure delete on a missing Secret is idempotent and creates nothing.
	missingName := "aws-605592676484-cn/missing"
	body, err = json.Marshal(providerapi.WriteSecretKVsRequest{Name: missingName, DeleteKeys: []string{"old"}})
	if err != nil {
		t.Fatal(err)
	}
	resp, data = post(body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pure delete status = %d (body %q)", resp.StatusCode, data)
	}
	if secretRowCount(t, store, missingName) != 0 {
		t.Fatal("pure delete created a missing Secret")
	}

	// A batch can create a hierarchical name that nests under an existing one.
	seedSecret(t, path, func(secrets *SecretStore) {
		if _, err := secrets.CreateSecret("aws-605592676484-cn"); err != nil {
			t.Fatal(err)
		}
	})
	body, err = json.Marshal(providerapi.WriteSecretKVsRequest{
		Name: "aws-605592676484-cn/child",
		KVs:  []providerapi.SecretKVWriteItem{{Key: "key", Value: "v"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, data = post(body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("nested create status = %d (body %q)", resp.StatusCode, data)
	}
}

func TestSecretKVsWriteBatchValidationAPI(t *testing.T) {
	ts, _, providers, _ := newProviderTestServer(t, "user-token")
	token, err := providers.Issue()
	if err != nil {
		t.Fatal(err)
	}

	tooMany := providerapi.WriteSecretKVsRequest{Name: "limits"}
	for i := 0; i < MaxSecretKVs+1; i++ {
		tooMany.KVs = append(tooMany.KVs, providerapi.SecretKVWriteItem{Key: fmt.Sprintf("k%02d", i), Value: "v"})
	}
	tooManyBody, err := json.Marshal(tooMany)
	if err != nil {
		t.Fatal(err)
	}
	tooManyDeletes := providerapi.WriteSecretKVsRequest{Name: "limits"}
	for i := 0; i < MaxSecretKVs+1; i++ {
		tooManyDeletes.DeleteKeys = append(tooManyDeletes.DeleteKeys, fmt.Sprintf("d%02d", i))
	}
	tooManyDeletesBody, err := json.Marshal(tooManyDeletes)
	if err != nil {
		t.Fatal(err)
	}
	oversizedBody := []byte(`{"name":"limits","kvs":[{"key":"a","value":"` + strings.Repeat("x", 2<<20) + `"}]}`)

	cases := []struct {
		name   string
		method string
		token  string
		body   []byte
		want   int
	}{
		{"missing name", http.MethodPost, token, []byte(`{"kvs":[]}`), http.StatusBadRequest},
		{"invalid name", http.MethodPost, token, []byte(`{"name":"Bad/Name","kvs":[]}`), http.StatusBadRequest},
		{"invalid key", http.MethodPost, token, []byte(`{"name":"valid","kvs":[{"key":"a/b","value":"v"}]}`), http.StatusBadRequest},
		{"duplicate upsert", http.MethodPost, token, []byte(`{"name":"valid","kvs":[{"key":"a","value":"1"},{"key":"a","value":"2"}]}`), http.StatusBadRequest},
		{"duplicate delete", http.MethodPost, token, []byte(`{"name":"valid","delete_keys":["a","a"]}`), http.StatusBadRequest},
		{"overlap", http.MethodPost, token, []byte(`{"name":"valid","kvs":[{"key":"a","value":"1"}],"delete_keys":["a"]}`), http.StatusBadRequest},
		{"too many kv", http.MethodPost, token, tooManyBody, http.StatusBadRequest},
		{"too many delete keys", http.MethodPost, token, tooManyDeletesBody, http.StatusBadRequest},
		{"invalid json", http.MethodPost, token, []byte(`{"name":`), http.StatusBadRequest},
		{"trailing json", http.MethodPost, token, []byte(`{"name":"valid"} {}`), http.StatusBadRequest},
		{"oversized body", http.MethodPost, token, oversizedBody, http.StatusRequestEntityTooLarge},
		{"user token", http.MethodPost, "user-token", []byte(`{"name":"valid","kvs":[]}`), http.StatusUnauthorized},
		{"missing token", http.MethodPost, "", []byte(`{"name":"valid","kvs":[]}`), http.StatusUnauthorized},
		{"get", http.MethodGet, token, nil, http.StatusMethodNotAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := providerRequest(t, tc.method, ts.URL+providerapi.SecretKVsWriteBatchPath, tc.token, bytes.NewReader(tc.body))
			defer resp.Body.Close()
			data, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d (body %q)", resp.StatusCode, tc.want, data)
			}
			if resp.Header.Get("Cache-Control") != "no-store" {
				t.Fatalf("Cache-Control = %q", resp.Header.Get("Cache-Control"))
			}
			if bytes.Contains(data, []byte(token)) || bytes.Contains(data, []byte("secret-token-value")) {
				t.Fatalf("error response leaked token or value: %s", data)
			}
		})
	}
}

func TestSecretKVsWriteBatchWrongRecipient(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mgcl.db")
	owner := testIdentityNamed(t, "owner")
	ownerStore, err := Open(path, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer ownerStore.Close()
	if _, err := ownerStore.WriteSecretKVsContext(context.Background(), "owned/secret", []SecretKVWrite{
		{Key: "key", Value: []byte("original")},
	}, nil, true); err != nil {
		t.Fatal(err)
	}

	wrongStore, err := Open(path, testIdentityNamed(t, "wrong"))
	if err != nil {
		t.Fatal(err)
	}
	defer wrongStore.Close()
	providers := NewProviderSessions()
	ts := httptest.NewServer(NewRouter(wrongStore, "http://placeholder", "", providers))
	defer ts.Close()
	token, err := providers.Issue()
	if err != nil {
		t.Fatal(err)
	}

	resp := providerRequest(t, http.MethodPost, ts.URL+providerapi.SecretKVsWriteBatchPath, token,
		strings.NewReader(`{"name":"owned/secret","kvs":[{"key":"key","value":"changed"}]}`))
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %q)", resp.StatusCode, data)
	}
	if strings.TrimSpace(string(data)) != `{"error":"secret key access denied"}` {
		t.Fatalf("body = %s", data)
	}

	values, _, err := ownerStore.ReadSecretValuesContextAccess(context.Background(), "owned/secret", true)
	if err != nil {
		t.Fatal(err)
	}
	if values["key"] != "original" {
		t.Fatalf("wrong recipient changed the value: %q", values["key"])
	}
}

func TestSecretKVsWriteBatchConcurrentHTTP(t *testing.T) {
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
			body := fmt.Sprintf(`{"name":"shared/batch","kvs":[{"key":"key%02d","value":"v%d"}]}`, i, i)
			resp := providerRequest(t, http.MethodPost, ts.URL+providerapi.SecretKVsWriteBatchPath, token, strings.NewReader(body))
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				errs <- fmt.Errorf("batch %d status = %d", i, resp.StatusCode)
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
}
