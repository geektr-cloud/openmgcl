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
		io.WriteString(w, `{"name":"bootstrap","values":{"a":"1","b":"two\nlines"}}`)
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
	if gotBody.Name != "bootstrap" {
		t.Fatalf("request = %+v", gotBody)
	}
	if out.Name != "bootstrap" || out.Values["a"] != "1" || out.Values["b"] != "two\nlines" {
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
