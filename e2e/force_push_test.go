package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/geektr-cloud/openmgcl/internal/tacos"
)

// TestE2EStateForcePushRestore verifies the real OpenTofu remote backend path:
// state push -force computes a serial from the local file and can be lower than
// the current remote serial. Current/download must serve the restored content,
// and a normal apply must be able to continue afterwards.
func TestE2EStateForcePushRestore(t *testing.T) {
	requireTofu(t)

	bin := buildMgcl(t)
	workdir := t.TempDir()
	dbPath := filepath.Join(workdir, "mgcl.db")
	identityPath := writeTestIdentity(t)

	_, serverURL := startMgcl(t, bin,
		"--host", "127.0.0.1:0", "--db", dbPath, "--token", "test-token",
		"--encryption-key-file", identityPath,
	)
	hostPort := strings.TrimPrefix(serverURL, "http://")
	cliCfg := writeCLIConfig(t, workdir, hostPort, "test-token")

	org := "e2e-force-org"
	wsName := "e2e-force-" + time.Now().Format("20060102150405")
	env := baseEnv(
		"TF_CLI_CONFIG_FILE="+cliCfg,
		"TF_FORCE_LOCAL_BACKEND=1",
	)

	writeRemoteBackendInput(t, workdir, hostPort, org, wsName, "v1")
	runTofu(t, workdir, env, false, "init", "-input=false")
	runTofu(t, workdir, env, false, "apply", "-auto-approve", "-input=false")
	v1Path := filepath.Join(workdir, "v1.tfstate")
	pullState(t, workdir, env, v1Path)

	writeRemoteBackendInput(t, workdir, hostPort, org, wsName, "v2")
	runTofu(t, workdir, env, false, "apply", "-auto-approve", "-input=false")
	writeRemoteBackendInput(t, workdir, hostPort, org, wsName, "v3")
	runTofu(t, workdir, env, false, "apply", "-auto-approve", "-input=false")

	// Push the old file back. OpenTofu sends serial 2 with force=true, which is
	// lower than the current serial 3 and reuses a historical serial.
	runTofu(t, workdir, env, false, "state", "push", "-force", v1Path)
	serial, input := readStateMeta(t, workdir, env)
	if serial != 2 || input != "v1" {
		t.Fatalf("after force push: serial=%d input=%q, want serial=2 input=v1", serial, input)
	}

	// A normal apply reads the restored serial and advances to 3 with new
	// content. Recreating the identical historical serial-3 state would match
	// the P3 retransmit rule and return 409, so use v2 here.
	writeRemoteBackendInput(t, workdir, hostPort, org, wsName, "v2")
	runTofu(t, workdir, env, false, "apply", "-auto-approve", "-input=false")
	serial, input = readStateMeta(t, workdir, env)
	if serial != 3 || input != "v2" {
		t.Fatalf("after post-restore apply: serial=%d input=%q, want serial=3 input=v2", serial, input)
	}
}

// TestE2ECloudStateForcePushRestore covers the cloud backend, which clears
// force before uploading a state push. The computed serial can equal or be
// lower than the current serial, so the server must still accept the restore.
func TestE2ECloudStateForcePushRestore(t *testing.T) {
	requireTofu(t)

	workdir := t.TempDir()
	dbPath := filepath.Join(workdir, "mgcl.db")
	identityPath := writeTestIdentity(t)

	store, err := tacos.Open(dbPath, loadTestIdentity(t, identityPath))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	serverURL, cleanup := newRouterServer(t, store, "test-token")
	t.Cleanup(cleanup)
	hostPort := strings.TrimPrefix(serverURL, "http://")

	org := "e2e-cloud-force-org"
	wsName := "e2e-cloud-force-" + time.Now().Format("20060102150405")
	createWorkspaceViaAPI(t, serverURL, "test-token", org, wsName)

	cliCfg := writeCLIConfig(t, workdir, hostPort, "test-token")
	env := baseEnv(
		"TF_CLI_CONFIG_FILE="+cliCfg,
		"TF_CLOUD_HOSTNAME="+hostPort,
		"TF_CLOUD_ORGANIZATION="+org,
		"TF_WORKSPACE="+wsName,
	)

	writeCloudInput(t, workdir, org, wsName, "v1")
	runTofu(t, workdir, env, false, "init", "-input=false")
	runTofu(t, workdir, env, false, "apply", "-auto-approve", "-input=false")
	v1Path := filepath.Join(workdir, "v1.tfstate")
	pullState(t, workdir, env, v1Path)

	writeCloudInput(t, workdir, org, wsName, "v2")
	runTofu(t, workdir, env, false, "apply", "-auto-approve", "-input=false")

	// Cloud state push increments the pushed serial and clears force, so this
	// arrives as serial=2, force=false while serial 2 is already current.
	runTofu(t, workdir, env, false, "state", "push", "-force", v1Path)
	serial, input := readStateMeta(t, workdir, env)
	if serial != 2 || input != "v1" {
		t.Fatalf("after cloud force push: serial=%d input=%q, want serial=2 input=v1", serial, input)
	}

	runTofu(t, workdir, env, false, "apply", "-auto-approve", "-input=false")
	serial, input = readStateMeta(t, workdir, env)
	if serial != 3 || input != "v2" {
		t.Fatalf("after cloud post-restore apply: serial=%d input=%q, want serial=3 input=v2", serial, input)
	}
}

func writeCloudInput(t *testing.T, dir, org, workspace, input string) {
	t.Helper()
	tf := fmt.Sprintf(`
terraform {
  cloud {
    organization = %q
    workspaces {
      name = %q
    }
  }
}

resource "terraform_data" "x" {
  input = %q
}
`, org, workspace, input)
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(tf), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeRemoteBackendInput(t *testing.T, dir, hostname, org, workspace, input string) {
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
  input = %q
}
`, hostname, org, workspace, input)
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(tf), 0o644); err != nil {
		t.Fatal(err)
	}
}

func pullState(t *testing.T, dir string, env []string, path string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tofu", "state", "pull")
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if e, ok := err.(*exec.ExitError); ok {
			exitErr = e
		}
		t.Fatalf("tofu state pull: %v\n%s", err, exitErrString(exitErr))
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

func exitErrString(err *exec.ExitError) string {
	if err == nil {
		return ""
	}
	return string(err.Stderr)
}

func readStateMeta(t *testing.T, dir string, env []string) (int, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pulled.tfstate")
	pullState(t, dir, env, path)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Serial    int `json:"serial"`
		Resources []struct {
			Instances []struct {
				Attributes map[string]any `json:"attributes"`
			} `json:"instances"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("decode state: %v\n%s", err, data)
	}
	if len(state.Resources) == 0 || len(state.Resources[0].Instances) == 0 {
		t.Fatalf("state has no resources: %s", data)
	}
	input, _ := state.Resources[0].Instances[0].Attributes["input"].(map[string]any)
	value, _ := input["value"].(string)
	return state.Serial, value
}
