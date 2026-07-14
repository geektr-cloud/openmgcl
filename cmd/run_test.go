package cmd

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTerraformJSONHandler(t *testing.T) {
	handler := terraformHandler()

	tests := []struct {
		name     string
		path     string
		wantCode int
		wantBody string
		wantCT   string
	}{
		{
			name:     "terraform.json",
			path:     "/.well-known/terraform.json",
			wantCode: http.StatusOK,
			wantBody: "{}",
			wantCT:   "application/json",
		},
		{
			name:     "other path",
			path:     "/other",
			wantCode: http.StatusNotFound,
		},
		{
			name:     "terraform.json with extra path",
			path:     "/.well-known/terraform.json/extra",
			wantCode: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.path, nil)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code != tt.wantCode {
				t.Errorf("got status %d, want %d", w.Code, tt.wantCode)
			}
			if tt.wantBody != "" && w.Body.String() != tt.wantBody {
				t.Errorf("got body %q, want %q", w.Body.String(), tt.wantBody)
			}
			if tt.wantCT != "" && w.Header().Get("Content-Type") != tt.wantCT {
				t.Errorf("got content-type %q, want %q", w.Header().Get("Content-Type"), tt.wantCT)
			}
		})
	}
}

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
