package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/geektr-cloud/openmgcl/internal/tacos"
)

// ──────────────────────────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────────────────────────

// buildMgcl compiles the mgcl binary into t.TempDir() and returns its path.
func buildMgcl(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mgcl")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = findModuleRoot(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build mgcl: %v\n%s", err, out)
	}
	return bin
}

// findModuleRoot walks up from cwd to find go.mod.
func findModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("cannot find module root (go.mod)")
		}
		dir = parent
	}
}

// startMgcl launches "mgcl run" with the given args, parses the listen address
// from stderr, and returns the cmd + server URL (e.g. "http://127.0.0.1:PORT").
func startMgcl(t *testing.T, bin string, args ...string) (*exec.Cmd, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	fullArgs := append([]string{"run"}, args...)
	cmd := exec.CommandContext(ctx, bin, fullArgs...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start mgcl: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	re := regexp.MustCompile(`MgCl server listening on (.+)$`)
	scanner := bufio.NewScanner(stderr)
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for mgcl to start")
		default:
		}
		if !scanner.Scan() {
			t.Fatal("mgcl exited before printing listen address")
		}
		line := scanner.Text()
		if m := re.FindStringSubmatch(line); m != nil {
			addr := m[1]
			serverURL := "http://" + addr
			t.Logf("mgcl started at %s", serverURL)
			return cmd, serverURL
		}
	}
}

// newRouterServer creates an httptest.Server backed by tacos.NewRouter whose
// serverURL matches its own listener address. Returns the base URL + cleanup.
func newRouterServer(t *testing.T, store *tacos.Store, token string) (string, func()) {
	t.Helper()
	// First pass: placeholder URL to get the handler registered.
	ts := httptest.NewServer(tacos.NewRouter(store, "http://placeholder", token))
	// Rebuild handler with the actual URL so discovery/upload/download URLs work.
	ts.Config.Handler = tacos.NewRouter(store, ts.URL, token)
	return ts.URL, ts.Close
}

// writeCLIConfig writes a tofu CLI config file that registers a host block
// with service URLs and credentials.
func writeCLIConfig(t *testing.T, dir, hostPort, token string) string {
	t.Helper()
	cfg := fmt.Sprintf(`host "%s" {
  services = {
    "state.v2" = "http://%s/api/v2/"
    "tfe.v2"   = "http://%s/api/v2/"
    "tfe.v2.1" = "http://%s/api/v2/"
  }
}

credentials "%s" {
  token = %q
}
`, hostPort, hostPort, hostPort, hostPort, hostPort, token)
	path := filepath.Join(dir, "tofu.rc")
	if err := os.WriteFile(path, []byte(cfg), 0644); err != nil {
		t.Fatalf("write CLI config: %v", err)
	}
	return path
}

// runTofu runs a tofu command and returns combined output.
// If expectFail is true, a non-zero exit is expected.
func runTofu(t *testing.T, dir string, env []string, expectFail bool, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	cmd := exec.CommandContext(ctx, "tofu", args...)
	cmd.Dir = dir
	cmd.Env = env

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	err := cmd.Run()
	out := buf.String()
	t.Logf("tofu %s (exit %v):\n%s", strings.Join(args, " "), err, out)

	if err != nil && !expectFail {
		t.Fatalf("tofu %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	if err == nil && expectFail {
		t.Fatalf("tofu %s succeeded but failure expected\n%s", strings.Join(args, " "), out)
	}
	return out
}

// requireTofu skips the test if tofu is not on PATH or -short is set.
func requireTofu(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}
	if _, err := exec.LookPath("tofu"); err != nil {
		t.Skip("tofu not found on PATH, skipping e2e test")
	}
}

// baseEnv returns a clean environment that avoids leaking host settings.
func baseEnv(extra ...string) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"CHECKPOINT_DISABLE=1",
	}
	env = append(env, extra...)
	return env
}

// apiDo sends an HTTP request with optional Bearer token and returns the response.
func apiDo(t *testing.T, method, url, token string, body *strings.Reader) *http.Response {
	t.Helper()
	var reqBody *strings.Reader
	if body != nil {
		reqBody = body
	} else {
		reqBody = strings.NewReader("")
	}
	req, err := http.NewRequest(method, url, reqBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// lockWorkspaceViaAPI locks a workspace by ID via the HTTP API.
func lockWorkspaceViaAPI(t *testing.T, baseURL, token, wsID string) {
	t.Helper()
	resp := apiDo(t, "POST", baseURL+"/api/v2/workspaces/"+wsID+"/actions/lock",
		token, strings.NewReader(`{"reason":"e2e test lock"}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("lock workspace: status %d", resp.StatusCode)
	}
}

// unlockWorkspaceViaAPI unlocks a workspace by ID via the HTTP API.
func unlockWorkspaceViaAPI(t *testing.T, baseURL, token, wsID string) {
	t.Helper()
	resp := apiDo(t, "POST", baseURL+"/api/v2/workspaces/"+wsID+"/actions/unlock",
		token, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unlock workspace: status %d", resp.StatusCode)
	}
}

// createWorkspaceViaAPI creates a workspace via the HTTP API and returns its ID.
func createWorkspaceViaAPI(t *testing.T, baseURL, token, org, name string) string {
	t.Helper()
	payload := fmt.Sprintf(`{"data":{"attributes":{"name":%q,"terraform-version":""}}}`, name)
	resp := apiDo(t, "POST", baseURL+"/api/v2/organizations/"+org+"/workspaces",
		token, strings.NewReader(payload))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create workspace: status %d", resp.StatusCode)
	}
	var result struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode workspace response: %v", err)
	}
	if result.Data.ID == "" {
		t.Fatal("workspace ID is empty")
	}
	t.Logf("created workspace %s/%s → ID %s", org, name, result.Data.ID)
	return result.Data.ID
}

// writeRemoteBackendTF writes a main.tf with a remote backend config.
func writeRemoteBackendTF(t *testing.T, dir, hostname, org, workspace string) {
	t.Helper()
	tf := fmt.Sprintf(`
terraform {
  backend "remote" {
    hostname     = %q
    organization = %q
    workspaces {
      name = %q
    }
  }
}

resource "terraform_data" "x" {
  input = "v1"
}
`, hostname, org, workspace)
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(tf), 0644); err != nil {
		t.Fatal(err)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Tests
// ──────────────────────────────────────────────────────────────────────────────

func TestE2ERemoteBackendBinary(t *testing.T) {
	requireTofu(t)

	bin := buildMgcl(t)
	workdir := t.TempDir()
	dbPath := filepath.Join(workdir, "mgcl.db")

	// Start mgcl with --host 127.0.0.1:0 to get a dynamic port.
	_, serverURL := startMgcl(t, bin, "--host", "127.0.0.1:0", "--db", dbPath, "--token", "test-token")
	hostPort := strings.TrimPrefix(serverURL, "http://")

	// Write CLI config pointing tofu at mgcl.
	cliCfg := writeCLIConfig(t, workdir, hostPort, "test-token")

	// Write main.tf with remote backend.
	org := "e2e-org"
	wsName := "e2e-remote-" + time.Now().Format("20060102150405")
	writeRemoteBackendTF(t, workdir, hostPort, org, wsName)

	env := baseEnv(
		"TF_CLI_CONFIG_FILE="+cliCfg,
		"TF_FORCE_LOCAL_BACKEND=1",
	)

	// init
	runTofu(t, workdir, env, false, "init", "-input=false")

	// apply v1
	out := runTofu(t, workdir, env, false, "apply", "-auto-approve", "-input=false")
	if !strings.Contains(out, "Apply complete") {
		t.Fatalf("expected 'Apply complete' in output:\n%s", out)
	}

	// Update to v2
	tfContent := fmt.Sprintf(`
terraform {
  backend "remote" {
    hostname     = %q
    organization = %q
    workspaces {
      name = %q
    }
  }
}

resource "terraform_data" "x" {
  input = "v2"
}
`, hostPort, org, wsName)
	if err := os.WriteFile(filepath.Join(workdir, "main.tf"), []byte(tfContent), 0644); err != nil {
		t.Fatal(err)
	}

	// apply v2
	out = runTofu(t, workdir, env, false, "apply", "-auto-approve", "-input=false")
	if !strings.Contains(out, "Apply complete") {
		t.Fatalf("expected 'Apply complete' in output:\n%s", out)
	}

	// state list
	out = runTofu(t, workdir, env, false, "state", "list")
	if !strings.Contains(out, "terraform_data.x") {
		t.Fatalf("expected 'terraform_data.x' in state list:\n%s", out)
	}

	// destroy
	out = runTofu(t, workdir, env, false, "destroy", "-auto-approve", "-input=false")
	if !strings.Contains(out, "Destroy complete") {
		t.Fatalf("expected 'Destroy complete' in output:\n%s", out)
	}
}

func TestE2ECloudBlock(t *testing.T) {
	requireTofu(t)

	workdir := t.TempDir()
	dbPath := filepath.Join(workdir, "mgcl.db")

	store, err := tacos.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	serverURL, cleanup := newRouterServer(t, store, "test-token")
	t.Cleanup(cleanup)
	hostPort := strings.TrimPrefix(serverURL, "http://")

	org := "e2e-cloud-org"
	wsName := "e2e-cloud-" + time.Now().Format("20060102150405")

	// Pre-create workspace via API (cloud block requires it to exist).
	createWorkspaceViaAPI(t, serverURL, "test-token", org, wsName)

	// Write CLI config.
	cliCfg := writeCLIConfig(t, workdir, hostPort, "test-token")

	// Write main.tf with cloud {} block.
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
	if err := os.WriteFile(filepath.Join(workdir, "main.tf"), []byte(tfContent), 0644); err != nil {
		t.Fatal(err)
	}

	env := baseEnv(
		"TF_CLI_CONFIG_FILE="+cliCfg,
		"TF_CLOUD_HOSTNAME="+hostPort,
		"TF_CLOUD_ORGANIZATION="+org,
		"TF_WORKSPACE="+wsName,
	)

	// init
	runTofu(t, workdir, env, false, "init", "-input=false")

	// apply
	out := runTofu(t, workdir, env, false, "apply", "-auto-approve", "-input=false")
	if !strings.Contains(out, "Apply complete") {
		t.Fatalf("expected 'Apply complete' in output:\n%s", out)
	}

	// destroy
	out = runTofu(t, workdir, env, false, "destroy", "-auto-approve", "-input=false")
	if !strings.Contains(out, "Destroy complete") {
		t.Fatalf("expected 'Destroy complete' in output:\n%s", out)
	}
}

func TestE2ELockConflict(t *testing.T) {
	requireTofu(t)

	workdir := t.TempDir()
	dbPath := filepath.Join(workdir, "mgcl.db")

	store, err := tacos.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	serverURL, cleanup := newRouterServer(t, store, "test-token")
	t.Cleanup(cleanup)
	hostPort := strings.TrimPrefix(serverURL, "http://")

	org := "e2e-lock-org"
	wsName := "e2e-lock-" + time.Now().Format("20060102150405")

	// Create workspace and get its ID.
	wsID := createWorkspaceViaAPI(t, serverURL, "test-token", org, wsName)

	// Write CLI config.
	cliCfg := writeCLIConfig(t, workdir, hostPort, "test-token")

	// Write main.tf.
	writeRemoteBackendTF(t, workdir, hostPort, org, wsName)

	env := baseEnv(
		"TF_CLI_CONFIG_FILE="+cliCfg,
		"TF_FORCE_LOCAL_BACKEND=1",
	)

	// init first.
	runTofu(t, workdir, env, false, "init", "-input=false")

	// Lock workspace via API.
	lockWorkspaceViaAPI(t, serverURL, "test-token", wsID)

	// apply should fail because workspace is locked.
	out := runTofu(t, workdir, env, true, "apply", "-auto-approve", "-input=false")
	if !strings.Contains(strings.ToLower(out), "lock") {
		t.Fatalf("expected lock-related error in output:\n%s", out)
	}

	// Unlock workspace.
	unlockWorkspaceViaAPI(t, serverURL, "test-token", wsID)
}
