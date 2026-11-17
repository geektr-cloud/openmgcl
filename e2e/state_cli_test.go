//go:build linux

package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/geektr-cloud/openmgcl/internal/tacos"
)

func runMgclSplit(t *testing.T, bin, dir string, env []string, args ...string) (int, string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("run mgcl: %v\nstderr: %s", err, stderr.String())
		}
	}
	return code, stdout.String(), stderr.String()
}

const e2eStateDeleteBypassFlag = "yes-i-know-this-will-permanently-delete-this-state"

func e2eStateJSON(serial int) []byte {
	return []byte(fmt.Sprintf("{\"version\":4,\"serial\":%d}\n", serial))
}

func TestE2EStateCLI(t *testing.T) {
	requireCLI(t)

	bin := buildMgcl(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mgcl.db")
	keyPath := writeTestIdentity(t)
	identity := loadTestIdentity(t, keyPath)

	store, err := tacos.Open(dbPath, identity)
	if err != nil {
		t.Fatal(err)
	}

	dev, err := store.CreateWorkspace("acme", "dev", "latest")
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	var devVersions []*tacos.StateVersion
	for serial := 1; serial <= 3; serial++ {
		sv, err := store.CreateStateVersion(
			dev.ID, serial, "lineage-e2e-dev", "",
			[]byte("raw-state"),
			e2eStateJSON(serial),
			[]byte(fmt.Sprintf("{\"outputs\":{\"serial\":%d}}\n", serial)),
			false,
		)
		if err != nil {
			store.Close()
			t.Fatal(err)
		}
		devVersions = append(devVersions, sv)
	}
	if _, err := store.CreateWorkspace("acme", "empty", "latest"); err != nil {
		store.Close()
		t.Fatal(err)
	}
	locked, err := store.CreateWorkspace("acme", "locked", "latest")
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.LockWorkspace(locked.ID, `{"id":"lock-e2e","who":"e2e"}`); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	args := func(rest ...string) []string {
		full := []string{"state"}
		full = append(full, rest...)
		return append(full, "--db", dbPath, "--encryption-key-file", keyPath)
	}
	run := func(rest ...string) (int, string) {
		t.Helper()
		return runMgclSync(t, bin, dir, baseEnv(), args(rest...)...)
	}

	code, out := run("list")
	if code != 0 {
		t.Fatalf("state list exit %d:\n%s", code, out)
	}
	for _, want := range []string{"STATE", "VERSIONS", "PENDING", "acme/dev", "acme/empty"} {
		if !strings.Contains(out, want) {
			t.Fatalf("state list missing %q:\n%s", want, out)
		}
	}

	code, out = run("stats")
	if code != 0 {
		t.Fatalf("state stats exit %d:\n%s", code, out)
	}
	for _, want := range []string{"SIZE", "acme/dev", "acme/empty"} {
		if !strings.Contains(out, want) {
			t.Fatalf("state stats missing %q:\n%s", want, out)
		}
	}

	code, out = run("info", "acme/dev")
	if code != 0 {
		t.Fatalf("state info exit %d:\n%s", code, out)
	}
	for _, want := range []string{"ID", "SERIAL", "STATUS", "CURRENT", "CREATED", devVersions[2].ID, devVersions[0].ID} {
		if !strings.Contains(out, want) {
			t.Fatalf("state info missing %q:\n%s", want, out)
		}
	}
	code, out = run("info", "acme/empty")
	if code != 0 {
		t.Fatalf("state info empty exit %d:\n%s", code, out)
	}
	if lines := strings.Split(strings.TrimSpace(out), "\n"); len(lines) != 1 {
		t.Fatalf("empty workspace info rows = %d:\n%s", len(lines)-1, out)
	}

	code, stdout, stderr := runMgclSplit(t, bin, dir, baseEnv(), args("export", "acme/dev")...)
	if code != 0 {
		t.Fatalf("state export exit %d:\n%s", code, stderr)
	}
	if stdout != string(e2eStateJSON(3)) {
		t.Fatalf("state export stdout = %q, want %q", stdout, e2eStateJSON(3))
	}

	exportPath := filepath.Join(dir, "export.json")
	code, out = run("export", "acme/dev", "-f", exportPath)
	if code != 0 {
		t.Fatalf("state export -f exit %d:\n%s", code, out)
	}
	data, err := os.ReadFile(exportPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(e2eStateJSON(3)) {
		t.Fatalf("export file = %q, want %q", data, e2eStateJSON(3))
	}
	info, err := os.Stat(exportPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("export file mode = %o, want 600", info.Mode().Perm())
	}

	backupPath := filepath.Join(dir, "backup.db")
	code, out = run("prune", "acme/dev", "--keeps", "1", "--backup-to", "sqlite:"+backupPath, "--"+e2eStateDeleteBypassFlag)
	if code != 0 {
		t.Fatalf("state prune exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "Pruned acme/dev") {
		t.Fatalf("state prune result missing:\n%s", out)
	}
	if _, err := os.Stat(backupPath); err != nil {
		t.Fatalf("prune backup missing: %v", err)
	}
	code, out = run("info", "acme/dev")
	if code != 0 {
		t.Fatalf("state info after prune exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, devVersions[2].ID) || strings.Contains(out, devVersions[0].ID) {
		t.Fatalf("prune kept the wrong versions:\n%s", out)
	}

	// Rename across organizations keeps the workspace and its current version.
	code, out = run("rename", "acme/dev", "acme-prod/dev")
	if code != 0 {
		t.Fatalf("state rename exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "Renamed state from acme/dev to acme-prod/dev.") {
		t.Fatalf("state rename result missing:\n%s", out)
	}
	code, out = run("info", "acme/dev")
	if code == 0 {
		t.Fatalf("old reference still exists after rename:\n%s", out)
	}
	code, out = run("info", "acme-prod/dev")
	if code != 0 || !strings.Contains(out, devVersions[2].ID) {
		t.Fatalf("state info on renamed reference exit %d:\n%s", code, out)
	}
	code, stdout, stderr = runMgclSplit(t, bin, dir, baseEnv(), args("export", "acme-prod/dev")...)
	if code != 0 {
		t.Fatalf("state export after rename exit %d:\n%s", code, stderr)
	}
	if stdout != string(e2eStateJSON(3)) {
		t.Fatalf("state export after rename = %q, want %q", stdout, e2eStateJSON(3))
	}
	code, out = run("list")
	if code != 0 || !strings.Contains(out, "acme-prod/dev") || strings.Contains(out, "acme/dev") {
		t.Fatalf("state list after rename exit %d:\n%s", code, out)
	}

	// A locked workspace refuses state-changing commands and reports its lock.
	code, out = run("delete", "acme/locked", "--"+e2eStateDeleteBypassFlag)
	if code == 0 {
		t.Fatalf("locked delete succeeded:\n%s", out)
	}
	if !strings.Contains(out, "locked") {
		t.Fatalf("locked delete did not report the lock:\n%s", out)
	}

	// Unlock needs a TTY and the full reference. Run it through script(1).
	unlockCommand := shellQuotedCommand(bin, args("unlock", "acme/locked")...)
	ptyCmd, stdin, ptyOut := startScriptPTY(t, dir, unlockCommand, baseEnv())
	if _, err := io.WriteString(stdin, "acme/locked\n"); err != nil {
		t.Fatal(err)
	}
	_ = stdin.Close()
	if code := waitMgclExit(t, ptyCmd, 10*time.Second); code != 0 {
		t.Fatalf("state unlock exit %d:\n%s", code, ptyOut.String())
	}
	if !strings.Contains(ptyOut.String(), "Unlocked workspace acme/locked") {
		t.Fatalf("state unlock result missing:\n%s", ptyOut.String())
	}
	// The no-op path is idempotent and needs no write session.
	code, out = run("unlock", "acme/locked")
	if code != 0 || !strings.Contains(out, "not locked") {
		t.Fatalf("state unlock no-op exit %d:\n%s", code, out)
	}

	code, out = run("delete", "acme-prod/dev", "--"+e2eStateDeleteBypassFlag)
	if code != 0 {
		t.Fatalf("state delete exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "Deleted state for acme-prod/dev") {
		t.Fatalf("state delete result missing:\n%s", out)
	}
	code, out = run("list")
	if code != 0 {
		t.Fatalf("state list after delete exit %d:\n%s", code, out)
	}
	if strings.Contains(out, "acme-prod/dev") || strings.Contains(out, "acme/dev") {
		t.Fatalf("deleted workspace still listed:\n%s", out)
	}
}
