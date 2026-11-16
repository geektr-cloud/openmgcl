package providerapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientReadSecret(t *testing.T) {
	var gotPath, gotAuth, gotContentType, gotCacheControl string
	var gotBody ReadSecretRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		gotCacheControl = r.Header.Get("Cache-Control")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"name":"bootstrap","values":{"a":"1","b":"two\nlines"},"ephemeral_marks":{"a":false,"b":true}}`)
	}))
	defer ts.Close()

	client := &Client{Endpoint: ts.URL + "/", Token: "provider-token"}
	out, err := client.ReadSecret(context.Background(), "bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != SecretsReadPath {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer provider-token" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotContentType != "application/json" {
		t.Fatalf("Content-Type = %q", gotContentType)
	}
	if gotCacheControl != "no-store" {
		t.Fatalf("Cache-Control = %q", gotCacheControl)
	}
	if gotBody.Name != "bootstrap" || gotBody.Access != SecretAccessEphemeral {
		t.Fatalf("request = %+v", gotBody)
	}
	if out.Name != "bootstrap" || out.Values["a"] != "1" || out.Values["b"] != "two\nlines" {
		t.Fatalf("response = %+v", out)
	}
	if out.EphemeralMarks["a"] || !out.EphemeralMarks["b"] {
		t.Fatalf("marks = %#v", out.EphemeralMarks)
	}
}

func TestClientWriteSecretKV(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody WriteSecretKVRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"name":"foo","key":"bar","is_ephemeral":true}`)
	}))
	defer ts.Close()

	client := &Client{Endpoint: ts.URL, Token: "provider-token"}
	out, err := client.WriteSecretKV(context.Background(), "foo/bar", "value", true)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != SecretKVsWritePath {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer provider-token" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotBody.Key != "foo/bar" || gotBody.Value != "value" || gotBody.IsEphemeral == nil || !*gotBody.IsEphemeral {
		t.Fatalf("request = %+v", gotBody)
	}
	if out.Name != "foo" || out.Key != "bar" || !out.IsEphemeral {
		t.Fatalf("response = %+v", out)
	}

	if _, err := client.WriteSecretKV(context.Background(), "foo/bar", "value", false); err != nil {
		t.Fatal(err)
	}
	if gotBody.IsEphemeral == nil || *gotBody.IsEphemeral {
		t.Fatalf("explicit false was not sent: %+v", gotBody)
	}
}

func TestClientDeleteSecretKV(t *testing.T) {
	var gotPath string
	var gotBody DeleteSecretKVRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"name":"foo","key":"bar"}`)
	}))
	defer ts.Close()

	client := &Client{Endpoint: ts.URL, Token: "provider-token"}
	out, err := client.DeleteSecretKV(context.Background(), "foo/bar")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != SecretKVsDeletePath {
		t.Fatalf("path = %q", gotPath)
	}
	if gotBody.Key != "foo/bar" {
		t.Fatalf("request = %+v", gotBody)
	}
	if out.Name != "foo" || out.Key != "bar" {
		t.Fatalf("response = %+v", out)
	}
}

func TestClientWriteSecretKVs(t *testing.T) {
	var gotPath, gotAuth, gotCacheControl string
	var gotBody WriteSecretKVsRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotCacheControl = r.Header.Get("Cache-Control")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"name":"aws-1/cn/scarborough","kvs":[{"key":"region","is_ephemeral":false},{"key":"token","is_ephemeral":true}]}`)
	}))
	defer ts.Close()

	public := false
	client := &Client{Endpoint: ts.URL, Token: "provider-token"}
	out, err := client.WriteSecretKVs(context.Background(), "aws-1/cn/scarborough", []SecretKVWriteItem{
		{Key: "region", Value: "us-east-1", IsEphemeral: &public},
		{Key: "token", Value: "secret-value"},
	}, []string{"old"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != SecretKVsWriteBatchPath {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer provider-token" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotCacheControl != "no-store" {
		t.Fatalf("Cache-Control = %q", gotCacheControl)
	}
	if gotBody.Name != "aws-1/cn/scarborough" || !gotBody.Ensure || len(gotBody.KVs) != 2 || len(gotBody.DeleteKeys) != 1 || gotBody.DeleteKeys[0] != "old" {
		t.Fatalf("request = %+v", gotBody)
	}
	if gotBody.KVs[0].IsEphemeral == nil || *gotBody.KVs[0].IsEphemeral {
		t.Fatalf("explicit false was not sent: %+v", gotBody.KVs[0])
	}
	if gotBody.KVs[1].IsEphemeral != nil {
		t.Fatalf("omitted mark was not omitted: %+v", gotBody.KVs[1])
	}
	if out.Name != "aws-1/cn/scarborough" || len(out.KVs) != 2 || out.KVs[0].Key != "region" || out.KVs[0].IsEphemeral {
		t.Fatalf("response = %+v", out)
	}
	if strings.Contains(fmt.Sprintf("%+v", out), "secret-value") {
		t.Fatalf("client response contains a value: %+v", out)
	}

	// A success response without kvs normalizes to an empty slice.
	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"name":"empty"}`)
	}))
	defer ts2.Close()
	emptyClient := &Client{Endpoint: ts2.URL, Token: "provider-token"}
	empty, err := emptyClient.WriteSecretKVs(context.Background(), "empty", nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if empty.KVs == nil || len(empty.KVs) != 0 {
		t.Fatalf("empty response kvs = %#v", empty.KVs)
	}
}

func TestClientReadPublicSecret(t *testing.T) {
	var gotBody ReadSecretRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"name":"bootstrap","values":{"a":"1"}}`)
	}))
	defer ts.Close()

	client := &Client{Endpoint: ts.URL, Token: "provider-token"}
	out, err := client.ReadPublicSecret(context.Background(), "bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	if gotBody.Name != "bootstrap" || gotBody.Access != SecretAccessPublic {
		t.Fatalf("request = %+v", gotBody)
	}
	if out.Name != "bootstrap" || out.Values["a"] != "1" {
		t.Fatalf("response = %+v", out)
	}
}

func TestClientDoesNotFollowRedirects(t *testing.T) {
	var followed atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followed.Store(true)
		io.WriteString(w, `{"name":"bootstrap","values":{}}`)
	}))
	defer target.Close()

	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+SecretsReadPath, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()

	client := &Client{Endpoint: redirect.URL, Token: "token"}
	_, err := client.ReadSecret(context.Background(), "bootstrap")
	if err == nil {
		t.Fatal("redirect response was treated as success")
	}
	if followed.Load() {
		t.Fatal("client followed the redirect and could leak its token")
	}
}

func TestClientResponseLimit(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"name":"bootstrap","values":{"a":"`)
		w.Write(make([]byte, maxResponseBytes))
		io.WriteString(w, `"}}`)
	}))
	defer ts.Close()

	client := &Client{Endpoint: ts.URL, Token: "token"}
	if _, err := client.ReadSecret(context.Background(), "bootstrap"); err == nil {
		t.Fatal("oversized response was accepted")
	}
}

func TestClientErrorsDoNotLeakTokenOrValue(t *testing.T) {
	const token = "dont-leak-this-token"
	const value = "dont-leak-this-value"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"error":"`+token+` `+value+`"}`)
	}))
	defer ts.Close()

	client := &Client{Endpoint: ts.URL, Token: token}
	_, err := client.ReadSecret(context.Background(), "bootstrap")
	if err == nil {
		t.Fatal("non-2xx response was accepted")
	}
	if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), value) {
		t.Fatalf("error leaked secret data: %v", err)
	}
}

func TestClientNon2xxReasonPhraseNotLeaked(t *testing.T) {
	const (
		token        = "reason-token-marker"
		value        = "reason-value-marker"
		reasonMarker = "server-controlled-reason-marker"
	)
	body := `{"error":"` + value + `"}`

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("test server does not support hijacking")
			return
		}
		conn, buf, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		fmt.Fprintf(buf, "HTTP/1.1 500 %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
			reasonMarker, len(body), body)
		if err := buf.Flush(); err != nil {
			t.Errorf("flush raw response: %v", err)
		}
	}))
	defer ts.Close()

	client := &Client{Endpoint: ts.URL, Token: token}
	_, err := client.ReadSecret(context.Background(), "bootstrap")
	if err == nil {
		t.Fatal("non-2xx response was accepted")
	}
	if got, want := err.Error(), "provider API returned status 500"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
	for _, marker := range []string{token, value, reasonMarker} {
		if strings.Contains(err.Error(), marker) {
			t.Fatalf("error leaked %q: %v", marker, err)
		}
	}
}

type closeTrackingBody struct {
	io.Reader
	closed *atomic.Bool
}

func (b *closeTrackingBody) Close() error {
	b.closed.Store(true)
	return nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestClientClosesBody(t *testing.T) {
	var closed atomic.Bool
	client := &Client{
		Endpoint: "http://example.test",
		Token:    "token",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Body:       &closeTrackingBody{Reader: strings.NewReader(`{"name":"bootstrap","values":{}}`), closed: &closed},
				Header:     make(http.Header),
			}, nil
		})},
	}
	if _, err := client.ReadSecret(context.Background(), "bootstrap"); err != nil {
		t.Fatal(err)
	}
	if !closed.Load() {
		t.Fatal("response body was not closed")
	}
}

func TestClientConfigurationErrors(t *testing.T) {
	cases := []*Client{
		nil,
		{},
		{Endpoint: "http://example.test"},
	}
	for i, client := range cases {
		if _, err := client.ReadSecret(context.Background(), "bootstrap"); err == nil {
			t.Errorf("case %d: configuration error was not reported", i)
		}
	}
}

func TestClientDefaultTimeout(t *testing.T) {
	c := &Client{}
	client := c.httpClient()
	if client.Timeout != 10*time.Second {
		t.Fatalf("timeout = %v", client.Timeout)
	}
	if client.CheckRedirect == nil {
		t.Fatal("redirect policy is not set")
	}
	if err := client.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("redirect policy = %v", err)
	}

	provided := &http.Client{Timeout: time.Second}
	c.HTTPClient = provided
	client = c.httpClient()
	if client.Timeout != time.Second {
		t.Fatalf("provided timeout = %v", client.Timeout)
	}
	// The caller's client must not be mutated.
	if provided.CheckRedirect != nil {
		t.Fatal("provided HTTPClient was mutated")
	}
}
