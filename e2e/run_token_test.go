//go:build linux

package e2e

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// outputCollector captures a running process's combined stdout/stderr so tests
// can wait for readiness and access-token lines without blocking the child.
type outputCollector struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *outputCollector) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *outputCollector) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func startRunWithOutput(t *testing.T, bin, dir string, env []string, args ...string) (*exec.Cmd, *outputCollector) {
	t.Helper()

	fullArgs := append([]string{"run"}, args...)
	cmd := exec.Command(bin, fullArgs...)
	cmd.Dir = dir
	cmd.Env = env
	out := &outputCollector{}
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start mgcl: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	})
	return cmd, out
}

// waitForLine waits for a line with the given prefix and returns the rest.
func waitForLine(t *testing.T, out *outputCollector, prefix string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		for _, line := range strings.Split(out.String(), "\n") {
			if strings.HasPrefix(line, prefix) {
				return strings.TrimPrefix(line, prefix)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %q in:\n%s", prefix, redactAccessToken(out.String()))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

var accessTokenLineRE = regexp.MustCompile(`(?m)^(MgCl access token: ).+$`)

// redactAccessToken keeps test failure output useful without writing a
// generated token into the test log.
func redactAccessToken(output string) string {
	return accessTokenLineRE.ReplaceAllString(output, "${1}<redacted>")
}

func stopRun(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal mgcl: %v", err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	select {
	case <-waitCh:
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		<-waitCh
		t.Fatal("timeout waiting for mgcl to exit")
	}
}

func expectStatus(t *testing.T, method, url, token string, want int) {
	t.Helper()
	resp := apiDo(t, method, url, token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("%s %s token_present=%t: status = %d, want %d", method, url, token != "", resp.StatusCode, want)
	}
}

var cliConfigTokenRE = regexp.MustCompile(`(?m)^\s*token\s*=\s*"([^"]+)"`)

func tokenFromCLIConfig(t *testing.T, content string) string {
	t.Helper()
	m := cliConfigTokenRE.FindStringSubmatch(content)
	if m == nil {
		t.Fatalf("no token in temporary CLI config")
	}
	return m[1]
}

func assertGeneratedToken(t *testing.T, token string) {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatalf("auto token is not base64url without padding: %v", err)
	}
	if len(raw) < 32 {
		t.Fatalf("auto token entropy = %d bytes, want at least 32", len(raw))
	}
}

func TestCLICommandlessRunGeneratesAndPrintsToken(t *testing.T) {
	requireCLI(t)

	bin := buildMgcl(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mgcl.db")
	keyPath := writeTestIdentity(t)

	cmd, out := startRunWithOutput(t, bin, dir, baseEnv(),
		"--host", "127.0.0.1:0", "--db", dbPath,
		"--encryption-key-file", keyPath,
	)

	addr := waitForLine(t, out, "MgCl server listening on ", 10*time.Second)
	token := waitForLine(t, out, "MgCl access token: ", 10*time.Second)
	if token == "" {
		t.Fatal("printed access token is empty")
	}
	assertGeneratedToken(t, token)

	serverURL := "http://" + addr
	expectStatus(t, http.MethodGet, serverURL+"/api/v2/organizations/dev/entitlement-set", token, http.StatusOK)
	expectStatus(t, http.MethodGet, serverURL+"/api/v2/organizations/dev/entitlement-set", "", http.StatusUnauthorized)
	expectStatus(t, http.MethodGet, serverURL+"/api/v2/organizations/dev/entitlement-set", "wrong-token", http.StatusUnauthorized)

	// The token line must come after readiness so existing listeners can keep
	// parsing the listening line first.
	output := out.String()
	listenIdx := strings.Index(output, "MgCl server listening on ")
	tokenIdx := strings.Index(output, "MgCl access token: ")
	if listenIdx < 0 || tokenIdx < listenIdx {
		t.Fatalf("access token was not printed after the listening line:\n%s", redactAccessToken(output))
	}

	stopRun(t, cmd)
	if got := strings.Count(out.String(), "MgCl access token: "); got != 1 {
		t.Fatalf("access token printed %d times, want 1", got)
	}
}

func TestCLICommandlessRunExplicitTokenIsNotPrinted(t *testing.T) {
	requireCLI(t)

	bin := buildMgcl(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mgcl.db")
	keyPath := writeTestIdentity(t)

	cmd, out := startRunWithOutput(t, bin, dir, baseEnv(),
		"--host", "127.0.0.1:0", "--db", dbPath,
		"--encryption-key-file", keyPath, "--token", "fixed-token",
	)

	addr := waitForLine(t, out, "MgCl server listening on ", 10*time.Second)
	// Give the process a chance to emit an accidental token line.
	time.Sleep(200 * time.Millisecond)
	if strings.Contains(out.String(), "MgCl access token:") {
		t.Fatalf("explicit token was printed:\n%s", out.String())
	}

	serverURL := "http://" + addr
	expectStatus(t, http.MethodGet, serverURL+"/api/v2/organizations/dev/entitlement-set", "fixed-token", http.StatusOK)
	expectStatus(t, http.MethodGet, serverURL+"/api/v2/organizations/dev/entitlement-set", "", http.StatusUnauthorized)

	stopRun(t, cmd)
	if strings.Contains(out.String(), "MgCl access token:") {
		t.Fatalf("explicit token was printed:\n%s", out.String())
	}
}

func TestCLIRunWithChildGeneratesFreshUnprintedToken(t *testing.T) {
	requireTofu(t)

	bin := buildMgcl(t)
	workdir := t.TempDir()
	dbPath := filepath.Join(workdir, "mgcl.db")
	keyPath := writeTestIdentity(t)

	org := "e2e-token-org"
	wsName := "e2e-token-" + time.Now().Format("20060102150405")
	tfContent := fmt.Sprintf(`
terraform {
  cloud {
    organization = %q
    workspaces {
      name = %q
    }
  }
}

resource "terraform_data" "x" {
  input = "v1"
}
`, org, wsName)
	if err := os.WriteFile(filepath.Join(workdir, "main.tf"), []byte(tfContent), 0o644); err != nil {
		t.Fatal(err)
	}

	run := func(name string) (string, string) {
		t.Helper()
		capture := filepath.Join(workdir, "capture-"+name+".tfrc")
		modeFile := filepath.Join(workdir, "mode-"+name)
		script := `cp "$TF_CLI_CONFIG_FILE" "$CAPTURE" && ` +
			`stat -c '%a' "$TF_CLI_CONFIG_FILE" > "$MODE" && ` +
			`tofu init -input=false && tofu apply -auto-approve -input=false`
		code, out := runMgclSync(t, bin, workdir, baseEnv("CAPTURE="+capture, "MODE="+modeFile),
			"run", "--host", "127.0.0.1:0", "--db", dbPath,
			"--encryption-key-file", keyPath,
			"--", "sh", "-c", script)
		if code != 0 {
			t.Fatalf("mgcl run %s exit code = %d\n%s", name, code, out)
		}
		data, err := os.ReadFile(capture)
		if err != nil {
			t.Fatal(err)
		}
		token := tokenFromCLIConfig(t, string(data))
		assertGeneratedToken(t, token)
		mode, err := os.ReadFile(modeFile)
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(mode)) != "600" {
			t.Fatalf("temporary CLI config mode on run %s = %q, want 600", name, strings.TrimSpace(string(mode)))
		}
		if strings.Contains(out, token) {
			t.Fatalf("auto token leaked to mgcl output on run %s", name)
		}
		if strings.Contains(out, "MgCl access token:") {
			t.Fatalf("child run printed its auto token on run %s", name)
		}
		return token, out
	}

	first, _ := run("one")
	second, _ := run("two")
	if first == second {
		t.Fatal("two child runs used the same TACOS token")
	}

	// A token from a finished run cannot access a new server.
	cmd, out := startRunWithOutput(t, bin, workdir, baseEnv(),
		"--host", "127.0.0.1:0", "--db", dbPath,
		"--encryption-key-file", keyPath, "--token", "fixed-token",
	)
	addr := waitForLine(t, out, "MgCl server listening on ", 10*time.Second)
	serverURL := "http://" + addr
	expectStatus(t, http.MethodGet, serverURL+"/api/v2/organizations/dev/entitlement-set", first, http.StatusUnauthorized)
	expectStatus(t, http.MethodGet, serverURL+"/api/v2/organizations/dev/entitlement-set", "fixed-token", http.StatusOK)
	stopRun(t, cmd)
}
