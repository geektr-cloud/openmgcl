package cmd

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/geektr-cloud/openmgcl/internal/tacos"
)

func TestDeriveServerURL(t *testing.T) {
	tests := []struct {
		name           string
		host           string
		listenerAddr   string
		explicitServer string
		want           string
	}{
		{
			name:         "normal host",
			host:         "127.0.0.1:1217",
			listenerAddr: "127.0.0.1:1217",
			want:         "http://127.0.0.1:1217",
		},
		{
			name:         "port zero gets actual port",
			host:         "127.0.0.1:0",
			listenerAddr: "127.0.0.1:54321",
			want:         "http://127.0.0.1:54321",
		},
		{
			name:         "colon-only host",
			host:         ":1217",
			listenerAddr: "0.0.0.0:1217",
			want:         "http://127.0.0.1:1217",
		},
		{
			name:         "wildcard 0.0.0.0",
			host:         "0.0.0.0:8080",
			listenerAddr: "0.0.0.0:8080",
			want:         "http://127.0.0.1:8080",
		},
		{
			name:           "explicit server preserved",
			host:           "127.0.0.1:1217",
			listenerAddr:   "127.0.0.1:1217",
			explicitServer: "http://example.com",
			want:           "http://example.com",
		},
		{
			name:           "explicit server trailing slash trimmed",
			host:           "127.0.0.1:1217",
			listenerAddr:   "127.0.0.1:1217",
			explicitServer: "http://example.com/",
			want:           "http://example.com",
		},
		{
			name:           "explicit server double trailing slash trimmed",
			host:           "127.0.0.1:1217",
			listenerAddr:   "127.0.0.1:1217",
			explicitServer: "http://example.com//",
			want:           "http://example.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr := &net.TCPAddr{}
			if tt.listenerAddr != "" {
				a, err := net.ResolveTCPAddr("tcp", tt.listenerAddr)
				if err != nil {
					t.Fatal(err)
				}
				addr = a
			}
			got, err := deriveServerURL(tt.host, addr, tt.explicitServer)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWithTFCloudHostname(t *testing.T) {
	tests := []struct {
		name   string
		env    []string
		server string
		want   []string
	}{
		{
			name:   "no existing var",
			env:    []string{"PATH=/usr/bin", "HOME=/root"},
			server: "http://127.0.0.1:1217",
			want:   []string{"PATH=/usr/bin", "HOME=/root", "TF_CLOUD_HOSTNAME=http://127.0.0.1:1217"},
		},
		{
			name:   "overwrites existing var",
			env:    []string{"PATH=/usr/bin", "TF_CLOUD_HOSTNAME=http://bad", "HOME=/root"},
			server: "http://127.0.0.1:1217",
			want:   []string{"PATH=/usr/bin", "HOME=/root", "TF_CLOUD_HOSTNAME=http://127.0.0.1:1217"},
		},
		{
			name:   "removes multiple existing vars",
			env:    []string{"TF_CLOUD_HOSTNAME=bad1", "PATH=/usr/bin", "TF_CLOUD_HOSTNAME=bad2"},
			server: "http://127.0.0.1:1217",
			want:   []string{"PATH=/usr/bin", "TF_CLOUD_HOSTNAME=http://127.0.0.1:1217"},
		},
		{
			name:   "empty env",
			env:    nil,
			server: "http://127.0.0.1:1217",
			want:   []string{"TF_CLOUD_HOSTNAME=http://127.0.0.1:1217"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := withTFCloudHostname(tt.env, tt.server)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d entries, want %d: %v", len(got), len(tt.want), got)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("entry %d: got %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestTacosDiscovery(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "mgcl-test-*.db")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())
	tmpFile.Close()

	store, err := tacos.Open(tmpFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	serverURL := "http://127.0.0.1:1217"
	handler := tacos.NewRouter(store, serverURL, "")

	ts := httptest.NewServer(handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/.well-known/terraform.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("got status %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("got content-type %q, want %q", ct, "application/json")
	}

	var doc map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("decode discovery doc: %v", err)
	}

	expectedSuffix := "/api/v2/"
	for _, key := range []string{"state.v2", "tfe.v2", "tfe.v2.1"} {
		val, ok := doc[key]
		if !ok {
			t.Errorf("discovery doc missing key %q", key)
			continue
		}
		if val != serverURL+expectedSuffix {
			t.Errorf("discovery[%q] = %q, want %q", key, val, serverURL+expectedSuffix)
		}
	}
}
