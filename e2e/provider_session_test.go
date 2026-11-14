package e2e

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/geektr-cloud/openmgcl/pkg/providerapi"
)

func doWithToken(ctx context.Context, url, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return http.DefaultClient.Do(req)
}

func runSecretCLI(t *testing.T, bin string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("mgcl %v failed: %v\n%s", args, err, out.String())
	}
	return out.String()
}

func TestE2EProviderSessionOnlyWithChild(t *testing.T) {
	bin := buildMgcl(t)
	workdir := t.TempDir()
	db := filepath.Join(workdir, "mgcl.db")
	key := writeTestIdentity(t)

	_, serverURL := startMgcl(t, bin, "--host", "127.0.0.1:0", "--db", db, "--encryption-key-file", key)
	req, err := http.NewRequest(http.MethodPost, serverURL+providerapi.SecretsReadPath, strings.NewReader(`{"name":"bootstrap"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer any-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("command-less run accepted a provider token: status = %d", resp.StatusCode)
	}
}

func TestE2EProviderSessionLifecycle(t *testing.T) {
	bin := buildMgcl(t)
	workdir := t.TempDir()
	db := filepath.Join(workdir, "mgcl.db")
	key := writeTestIdentity(t)
	envFile := filepath.Join(workdir, "provider-env.txt")

	runSecretCLI(t, bin, baseEnv(),
		"secret", "create", "bootstrap", "--db", db, "--encryption-key-file", key)
	runSecretCLI(t, bin, baseEnv(),
		"secret", "set", "bootstrap/access_key_id", "AKIA-E2E", "--db", db, "--encryption-key-file", key)

	cmd := exec.Command(bin, "run", "--host", "127.0.0.1:0", "--db", db,
		"--encryption-key-file", key, "--",
		"sh", "-c", `printf '%s\n%s\n' "$MGCL_PROVIDER_ENDPOINT" "$MGCL_PROVIDER_TOKEN" > "$MGCL_E2E_ENV"; sleep 30`)
	cmd.Env = baseEnv("MGCL_E2E_ENV=" + envFile)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	})

	deadline := time.Now().Add(10 * time.Second)
	var lines []string
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(envFile)
		if err == nil {
			lines = strings.Split(strings.TrimRight(string(data), "\n"), "\n")
			if len(lines) == 2 && lines[0] != "" && lines[1] != "" {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(lines) != 2 {
		t.Fatalf("provider environment was not written: %v", lines)
	}
	endpoint, token := lines[0], lines[1]
	if !strings.HasPrefix(endpoint, "http://127.0.0.1:") {
		t.Fatalf("endpoint = %q", endpoint)
	}
	if strings.Contains(endpoint, " ") || strings.Contains(token, " ") {
		t.Fatalf("malformed provider environment: %q", lines)
	}

	client := &providerapi.Client{Endpoint: endpoint, Token: token}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	secret, err := client.ReadSecret(ctx, "bootstrap")
	if err != nil {
		t.Fatalf("read live secret: %v", err)
	}
	if secret.Values["access_key_id"] != "AKIA-E2E" {
		t.Fatalf("value = %q", secret.Values["access_key_id"])
	}

	// The provider token is not a TACOS user token.
	resp, err := doWithToken(context.Background(), endpoint+"/api/v2/ping", token)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("provider token on TACOS route: status = %d, want 401", resp.StatusCode)
	}

	// Stop the run process: the server goes away and the token cannot be used.
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	select {
	case <-waitCh:
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("mgcl run did not exit after SIGINT")
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	if _, err := client.ReadSecret(ctx2, "bootstrap"); err == nil {
		t.Fatal("provider token still worked after the run process exited")
	}
}
