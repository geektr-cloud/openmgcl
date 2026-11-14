package cmd

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/geektr-cloud/openmgcl/pkg/providerapi"
)

func TestCheckPublishSafety(t *testing.T) {
	tests := []struct {
		name      string
		host      string
		serverURL string
		skip      bool
		tty       bool
		answer    string
		wantErr   bool
	}{
		{name: "default loopback", host: "127.0.0.1:1217"},
		{name: "loopback port zero", host: "127.0.0.1:0"},
		{name: "loopback with loopback server", host: "127.0.0.1:1217", serverURL: "http://127.0.0.1:9999"},
		{name: "localhost", host: "localhost:1217", wantErr: true},
		{name: "ipv6 loopback", host: "[::1]:1217", wantErr: true},
		{name: "wildcard v4", host: "0.0.0.0:1217", wantErr: true},
		{name: "wildcard v6", host: "[::]:1217", wantErr: true},
		{name: "empty host", host: ":1217", wantErr: true},
		{name: "other loopback", host: "127.0.0.2:1217", wantErr: true},
		{name: "lan ip", host: "192.0.2.10:1217", wantErr: true},
		{name: "hostname", host: "example.test:1217", wantErr: true},
		{name: "server URL is LAN", host: "127.0.0.1:1217", serverURL: "http://192.0.2.10:1217", wantErr: true},
		{name: "server URL localhost", host: "127.0.0.1:1217", serverURL: "http://localhost:1217", wantErr: true},
		{name: "skip flag", host: "0.0.0.0:1217", skip: true},
		{name: "tty yes", host: "0.0.0.0:1217", tty: true, answer: "yes"},
		{name: "tty YES", host: "0.0.0.0:1217", tty: true, answer: "YES"},
		{name: "tty no", host: "0.0.0.0:1217", tty: true, answer: "no", wantErr: true},
		{name: "tty empty", host: "0.0.0.0:1217", tty: true, answer: "", wantErr: true},
		{name: "bad listen host", host: "127.0.0.1", wantErr: true},
		{name: "bad server URL", host: "127.0.0.1:1217", serverURL: "http://", wantErr: true},
		{name: "malformed server URL", host: "127.0.0.1:1217", serverURL: "http://%zz", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldTerm := stdinIsTerminal
			oldLine := readTerminalLine
			stdinIsTerminal = func() bool { return tt.tty }
			readTerminalLine = func() (string, error) { return tt.answer, nil }
			t.Cleanup(func() {
				stdinIsTerminal = oldTerm
				readTerminalLine = oldLine
			})

			var stderr bytes.Buffer
			err := checkPublishSafety(tt.host, tt.serverURL, tt.skip, &stderr)
			if tt.wantErr && err == nil {
				t.Fatalf("no error, stderr = %q", stderr.String())
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantErr && !strings.Contains(err.Error(), "non-loopback") && !strings.Contains(err.Error(), "invalid") {
				t.Fatalf("unexpected error text: %v", err)
			}
			if tt.wantErr && strings.Contains(stderr.String(), "secret-value") {
				t.Fatalf("warning leaked a value: %q", stderr.String())
			}
			if !strings.HasPrefix(tt.host, "127.0.0.1") && err == nil && !strings.Contains(stderr.String(), "WARNING") {
				t.Fatalf("missing warning for non-loopback host: %q", stderr.String())
			}
		})
	}
}

func TestPublishSafetyInvalidServerURLErrorIsSafe(t *testing.T) {
	malicious := []string{
		"http://alice:hunter2@127.0.0.1:1217/%zz?q=querysecret",
		"http://alice:hunter2@[::1/?q=querysecret",
		"example.com?q=querysecret",
		"alice:hunter2@example.com?q=querysecret",
		"http://:1217?q=querysecret",
	}
	for _, raw := range malicious {
		var stderr bytes.Buffer
		err := checkPublishSafety("127.0.0.1:1217", raw, false, &stderr)
		if err == nil {
			t.Fatalf("%q was accepted", raw)
		}
		message := err.Error() + stderr.String()
		for _, secret := range []string{"alice", "hunter2", "querysecret"} {
			if strings.Contains(message, secret) {
				t.Errorf("error/warning leaked %q: %s", secret, message)
			}
		}
	}
}

func TestDeriveProviderEndpoint(t *testing.T) {
	tests := []struct {
		addr net.Addr
		want string
	}{
		{&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1217}, "http://127.0.0.1:1217"},
		{&net.TCPAddr{IP: net.ParseIP("127.0.0.2"), Port: 1217}, "http://127.0.0.2:1217"},
		{&net.TCPAddr{IP: net.ParseIP("0.0.0.0"), Port: 1217}, "http://127.0.0.1:1217"},
		{&net.TCPAddr{IP: net.ParseIP("::"), Port: 1217}, "http://127.0.0.1:1217"},
		{&net.TCPAddr{IP: net.ParseIP("::1"), Port: 1217}, "http://[::1]:1217"},
		{&net.TCPAddr{IP: net.ParseIP("fe80::1"), Zone: "eth0", Port: 1217}, "http://[fe80::1%25eth0]:1217"},
		{&net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 1217}, "http://192.0.2.10:1217"},
	}
	for _, tt := range tests {
		got, err := deriveProviderEndpoint(tt.addr)
		if err != nil {
			t.Fatal(err)
		}
		if got != tt.want {
			t.Errorf("deriveProviderEndpoint(%s) = %q, want %q", tt.addr, got, tt.want)
		}
	}
	if _, err := deriveProviderEndpoint(nil); err == nil {
		t.Fatal("nil listener address accepted")
	}
}

func TestRunGateFailsBeforeKeyAndDatabase(t *testing.T) {
	stubNonTTY(t)
	dir := t.TempDir()
	t.Chdir(dir)
	db := filepath.Join(dir, "mgcl.db")
	missingKey := filepath.Join(dir, "missing-key")

	_, _, err := runCLI(t, "", "run", "--host", "0.0.0.0:0", "--db", db, "--encryption-key-file", missingKey)
	if err == nil || !strings.Contains(err.Error(), "non-loopback") {
		t.Fatalf("run error = %v", err)
	}
	if _, statErr := os.Stat(db); !os.IsNotExist(statErr) {
		t.Fatal("run opened/created the database before the safety gate")
	}

	_, _, err = runCLI(t, "", "run", "--host", "0.0.0.0:0", "--db", db,
		"--encryption-key-file", missingKey, "--yes-i-know-it-will-publish-state-and-secrets")
	if err == nil || !strings.Contains(err.Error(), "load encryption key") {
		t.Fatalf("skip flag did not proceed to key loading: %v", err)
	}
	if _, statErr := os.Stat(db); !os.IsNotExist(statErr) {
		t.Fatal("run created the database before key validation")
	}
}

func TestRunGateUsesMergedConfigHost(t *testing.T) {
	stubNonTTY(t)
	root := t.TempDir()
	initTestGitRepo(t, root)
	key := writeTestIdentity(t, nil)
	config := "host: \"0.0.0.0:0\"\ndb: \"mgcl.db\"\nencryption_key_file: " + key + "\n"
	if err := os.WriteFile(filepath.Join(root, ".mgcl.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	_, _, err := runCLI(t, "", "run")
	if err == nil || !strings.Contains(err.Error(), "non-loopback") {
		t.Fatalf("config host did not trigger the safety gate: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "mgcl.db")); !os.IsNotExist(statErr) {
		t.Fatal("database was created before the config-host safety gate")
	}
}

func TestRunInjectsProviderEnvironment(t *testing.T) {
	stubNonTTY(t)
	dir := t.TempDir()
	t.Chdir(dir)
	db := filepath.Join(dir, "mgcl.db")
	key := writeTestIdentity(t, nil)
	outFile := filepath.Join(dir, "env.txt")

	t.Setenv(providerapi.EnvEndpoint, "http://inherited.invalid")
	t.Setenv(providerapi.EnvToken, "inherited-token")
	t.Setenv("MGCL_OUT", outFile)

	stdout, stderr, err := runCLI(t, "", "run", "--host", "127.0.0.1:0",
		"--server", "http://127.0.0.1:9999", "--db", db,
		"--encryption-key-file", key, "--",
		"sh", "-c", `printf '%s\n%s\n' "$MGCL_PROVIDER_ENDPOINT" "$MGCL_PROVIDER_TOKEN" > "$MGCL_OUT"`)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("child env = %q", data)
	}
	if !strings.HasPrefix(lines[0], "http://127.0.0.1:") || strings.HasSuffix(lines[0], "/") {
		t.Fatalf("endpoint = %q", lines[0])
	}
	if strings.Contains(lines[0], "9999") {
		t.Fatalf("provider endpoint used --server: %q", lines[0])
	}
	if lines[1] == "" || lines[1] == "inherited-token" {
		t.Fatalf("token was not replaced: %q", lines[1])
	}
	if strings.Contains(stdout, lines[1]) || strings.Contains(stderr, lines[1]) {
		t.Fatal("provider token was printed by mgcl")
	}
}
