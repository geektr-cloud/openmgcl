package cmd

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
		{
			name:         "ipv6 loopback",
			host:         "[::1]:1217",
			listenerAddr: "[::1]:1217",
			want:         "http://[::1]:1217",
		},
		{
			name:         "ipv6 zone escaped",
			host:         "[fe80::1%eth0]:1217",
			listenerAddr: "[fe80::1%eth0]:1217",
			want:         "http://[fe80::1%25eth0]:1217",
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

func TestHostFromServerURL(t *testing.T) {
	tests := []struct {
		name      string
		serverURL string
		want      string
	}{
		{"http URL", "http://127.0.0.1:1217", "127.0.0.1:1217"},
		{"https URL", "https://example.com", "example.com"},
		{"trailing slash", "http://example.com/", "example.com"},
		{"ipv6", "http://[::1]:1217", "[::1]:1217"},
		{"bare host and port", "127.0.0.1:1217", "127.0.0.1:1217"},
		{"bare hostname", "example.com", "example.com"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hostFromServerURL(tt.serverURL); got != tt.want {
				t.Errorf("hostFromServerURL(%q) = %q, want %q", tt.serverURL, got, tt.want)
			}
		})
	}
}

func TestWithEnv(t *testing.T) {
	tests := []struct {
		name string
		env  []string
		kv   []string
		want []string
	}{
		{
			name: "no existing vars",
			env:  []string{"PATH=/usr/bin", "HOME=/root"},
			kv:   []string{"TF_CLOUD_HOSTNAME=127.0.0.1:1217"},
			want: []string{"PATH=/usr/bin", "HOME=/root", "TF_CLOUD_HOSTNAME=127.0.0.1:1217"},
		},
		{
			name: "overwrites existing var",
			env:  []string{"PATH=/usr/bin", "TF_CLOUD_HOSTNAME=bad", "HOME=/root"},
			kv:   []string{"TF_CLOUD_HOSTNAME=127.0.0.1:1217"},
			want: []string{"PATH=/usr/bin", "HOME=/root", "TF_CLOUD_HOSTNAME=127.0.0.1:1217"},
		},
		{
			name: "removes multiple existing vars",
			env:  []string{"TF_CLOUD_HOSTNAME=bad1", "PATH=/usr/bin", "TF_CLOUD_HOSTNAME=bad2"},
			kv:   []string{"TF_CLOUD_HOSTNAME=127.0.0.1:1217"},
			want: []string{"PATH=/usr/bin", "TF_CLOUD_HOSTNAME=127.0.0.1:1217"},
		},
		{
			name: "multiple keys",
			env:  []string{"A=1", "B=2", "C=3"},
			kv:   []string{"B=20", "D=4"},
			want: []string{"A=1", "C=3", "B=20", "D=4"},
		},
		{
			name: "empty env",
			env:  nil,
			kv:   []string{"TF_CLOUD_HOSTNAME=127.0.0.1:1217"},
			want: []string{"TF_CLOUD_HOSTNAME=127.0.0.1:1217"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := withEnv(tt.env, tt.kv...)
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

func TestDropEnv(t *testing.T) {
	env := []string{"PATH=/usr/bin", passphraseEnv + "=secret", "HOME=/root"}
	got := dropEnv(env, passphraseEnv)
	want := []string{"PATH=/usr/bin", "HOME=/root"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestLoadEncryptionIdentity(t *testing.T) {
	t.Run("unencrypted", func(t *testing.T) {
		path := writeTestIdentity(t, nil)
		if _, err := loadEncryptionIdentity(path); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("passphrase from environment", func(t *testing.T) {
		path := writeTestIdentity(t, []byte("secret"))
		t.Setenv(passphraseEnv, "secret")
		if _, err := loadEncryptionIdentity(path); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("wrong passphrase from environment does not prompt", func(t *testing.T) {
		path := writeTestIdentity(t, []byte("secret"))
		t.Setenv(passphraseEnv, "wrong")
		if _, err := loadEncryptionIdentity(path); err == nil {
			t.Fatal("wrong passphrase succeeded")
		}
	})
}

func TestBuildCLIConfig(t *testing.T) {
	const (
		serverURL = "http://127.0.0.1:1217"
		hostname  = "127.0.0.1:1217"
	)

	t.Run("adds host block and credentials", func(t *testing.T) {
		cfg := buildCLIConfig("", serverURL, hostname, "secret")
		for _, want := range []string{
			`host "127.0.0.1:1217" {`,
			`"state.v2" = "http://127.0.0.1:1217/api/v2/"`,
			`"tfe.v2"   = "http://127.0.0.1:1217/api/v2/"`,
			`"tfe.v2.1" = "http://127.0.0.1:1217/api/v2/"`,
			`credentials "127.0.0.1:1217" {`,
			`token = "secret"`,
		} {
			if !strings.Contains(cfg, want) {
				t.Errorf("config missing %q:\n%s", want, cfg)
			}
		}
	})

	t.Run("omits credentials without token", func(t *testing.T) {
		cfg := buildCLIConfig("", serverURL, hostname, "")
		if strings.Contains(cfg, "credentials") {
			t.Errorf("config should not contain credentials:\n%s", cfg)
		}
	})

	t.Run("preserves existing content", func(t *testing.T) {
		existing := "# my config\n"
		cfg := buildCLIConfig(existing, serverURL, hostname, "")
		if !strings.HasPrefix(cfg, existing) {
			t.Errorf("config does not start with existing content:\n%s", cfg)
		}
	})

	t.Run("does not duplicate host block", func(t *testing.T) {
		existing := "host \"127.0.0.1:1217\" {\n}\n"
		cfg := buildCLIConfig(existing, serverURL, hostname, "secret")
		if cfg != existing {
			t.Errorf("existing host block should win, got:\n%s", cfg)
		}
	})

	t.Run("escapes token", func(t *testing.T) {
		cfg := buildCLIConfig("", serverURL, hostname, "a\"b\\c")
		if !strings.Contains(cfg, `token = "a\"b\\c"`) {
			t.Errorf("token not escaped:\n%s", cfg)
		}
	})
}

func TestPrepareCLIConfigMergesExisting(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "existing.tfrc")
	if err := os.WriteFile(existing, []byte("# existing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TF_CLI_CONFIG_FILE", existing)

	path, cleanup, err := prepareCLIConfig("http://127.0.0.1:1217", "127.0.0.1:1217", "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("temporary config mode = %o, want 600", got)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "# existing\n") {
		t.Errorf("temporary config does not preserve existing content:\n%s", data)
	}
	if !strings.Contains(string(data), `host "127.0.0.1:1217" {`) {
		t.Errorf("temporary config missing host block:\n%s", data)
	}
	if !strings.Contains(string(data), `token = "secret"`) {
		t.Errorf("temporary config missing current token:\n%s", data)
	}

	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("temporary config was not removed: %v", err)
	}
}

func TestPrepareCLIConfigInheritsDefaultUserConfig(t *testing.T) {
	home := t.TempDir()
	userConfig := `provider_installation {
  dev_overrides {
    "geektr-cloud/mgcl" = "/opt/mgcl-provider/bin"
  }
  direct {}
}
`
	if err := os.WriteFile(filepath.Join(home, ".tofurc"), []byte(userConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("TF_CLI_CONFIG_FILE", "")

	path, cleanup, err := prepareCLIConfig("http://127.0.0.1:1217", "127.0.0.1:1217", "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, `"geektr-cloud/mgcl" = "/opt/mgcl-provider/bin"`) {
		t.Errorf("temporary config lost the default ~/.tofurc dev_overrides:\n%s", got)
	}
	if !strings.Contains(got, `host "127.0.0.1:1217" {`) {
		t.Errorf("temporary config missing host block:\n%s", got)
	}
	if !strings.Contains(got, `token = "secret"`) {
		t.Errorf("temporary config missing current token:\n%s", got)
	}
}

func TestPrepareCLIConfigEnvOverridesDefaultUserConfig(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".tofurc"), []byte("# from home\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	envConfig := filepath.Join(t.TempDir(), "env.tfrc")
	if err := os.WriteFile(envConfig, []byte("# from env\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("TF_CLI_CONFIG_FILE", envConfig)

	path, cleanup, err := prepareCLIConfig("http://127.0.0.1:1217", "127.0.0.1:1217", "")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.HasPrefix(got, "# from env\n") {
		t.Errorf("TF_CLI_CONFIG_FILE was not read first:\n%s", got)
	}
	if strings.Contains(got, "# from home") {
		t.Errorf("default ~/.tofurc leaked into config while TF_CLI_CONFIG_FILE was set:\n%s", got)
	}
}

func TestPrepareCLIConfigMissingDefaultUserConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TF_CLI_CONFIG_FILE", "")

	path, cleanup, err := prepareCLIConfig("http://127.0.0.1:1217", "127.0.0.1:1217", "")
	if err != nil {
		t.Fatalf("missing ~/.tofurc should not fail: %v", err)
	}
	defer cleanup()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), `host "127.0.0.1:1217" {`) {
		t.Errorf("temporary config should contain only the generated host block:\n%s", data)
	}
}

func TestPrepareCLIConfigUnreadableDefaultUserConfigFails(t *testing.T) {
	home := t.TempDir()
	userConfig := filepath.Join(home, ".tofurc")
	if err := os.WriteFile(userConfig, []byte("# config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(userConfig, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(userConfig, 0o600) })
	if _, err := os.ReadFile(userConfig); err == nil {
		t.Skip("cannot make ~/.tofurc unreadable (running as root)")
	}
	t.Setenv("HOME", home)
	t.Setenv("TF_CLI_CONFIG_FILE", "")

	if _, cleanup, err := prepareCLIConfig("http://127.0.0.1:1217", "127.0.0.1:1217", ""); err == nil {
		cleanup()
		t.Fatal("unreadable ~/.tofurc should fail")
	} else if !strings.Contains(err.Error(), "read default OpenTofu CLI config") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestPrepareCLIConfigMissingExplicitFileFails(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".tofurc"), []byte("# from home\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("TF_CLI_CONFIG_FILE", filepath.Join(t.TempDir(), "missing.tfrc"))

	if _, cleanup, err := prepareCLIConfig("http://127.0.0.1:1217", "127.0.0.1:1217", ""); err == nil {
		cleanup()
		t.Fatal("missing explicit TF_CLI_CONFIG_FILE should fail")
	} else if !strings.Contains(err.Error(), "read TF_CLI_CONFIG_FILE") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestTacosDiscovery(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "mgcl-test-*.db")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())
	tmpFile.Close()

	store, err := tacos.Open(tmpFile.Name(), testTacosIdentity(t))
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
