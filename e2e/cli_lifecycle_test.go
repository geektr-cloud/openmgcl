//go:build linux

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// requireCLI skips CLI lifecycle tests in short mode. These tests use the
// compiled binary but do not need OpenTofu.
func requireCLI(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping CLI lifecycle e2e test in short mode")
	}
}

// runMgclSync runs mgcl to completion and returns its exit code and combined
// output. A non-zero exit is returned as a code, not a test failure.
func runMgclSync(t *testing.T, bin, dir string, env []string, args ...string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	err := cmd.Run()
	out := buf.String()
	if err == nil {
		return 0, out
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), out
	}
	t.Fatalf("run mgcl: %v\n%s", err, out)
	return 0, out
}

// startMgclProcess starts mgcl and waits until it reports the listening address.
func startMgclProcess(t *testing.T, bin, dir string, env []string, args ...string) *exec.Cmd {
	t.Helper()

	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = io.Discard
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start mgcl: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	ready := make(chan struct{})
	var once sync.Once
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), "MgCl server listening on") {
				once.Do(func() { close(ready) })
			}
		}
	}()

	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for mgcl to start")
	}
	return cmd
}

func waitMgclExit(t *testing.T, cmd *exec.Cmd, timeout time.Duration) int {
	t.Helper()

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	select {
	case err := <-waitCh:
		if err == nil {
			return 0
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		t.Fatalf("wait mgcl: %v", err)
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		<-waitCh
		t.Fatal("timeout waiting for mgcl to exit")
	}
	return 0
}

func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", path)
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse pid %q: %v", data, err)
	}
	return pid
}

// killProcessGroupOnCleanup force-kills a child process group if a test fails
// before mgcl has cleaned it up.
func killProcessGroupOnCleanup(t *testing.T, pidFile string) {
	t.Cleanup(func() {
		data, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			return
		}
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	})
}

// shellQuote returns s as a single-quoted POSIX shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func shellQuotedCommand(program string, args ...string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, shellQuote(program))
	for _, arg := range args {
		parts = append(parts, shellQuote(arg))
	}
	return strings.Join(parts, " ")
}

// startScriptPTY runs command under a real PTY using util-linux script. The
// caller drives stdin and waits for the process. Tests skip when script is not
// available.
func startScriptPTY(t *testing.T, dir, command string, env []string) (*exec.Cmd, io.WriteCloser, *bytes.Buffer) {
	t.Helper()
	scriptBin, err := exec.LookPath("script")
	if err != nil {
		t.Skip("script command not found, skipping PTY test")
	}

	cmd := exec.Command(scriptBin, "-q", "-e", "-c", command, "/dev/null")
	cmd.Dir = dir
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd, stdin, &out
}

func TestCLIChildExitCodeAndTempConfigCleanup(t *testing.T) {
	requireCLI(t)

	bin := buildMgcl(t)
	dir := t.TempDir()
	tmpDir := filepath.Join(dir, "tmp")
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "mgcl.db")
	keyPath := writeTestIdentity(t)

	code, out := runMgclSync(t, bin, dir, baseEnv("TMPDIR="+tmpDir),
		"run", "--host", "127.0.0.1:0", "--db", dbPath,
		"--token", "secret-token", "--encryption-key-file", keyPath,
		"--", "sh", "-c", "exit 7")

	if code != 7 {
		t.Fatalf("exit code = %d, want 7\n%s", code, out)
	}
	if strings.Contains(out, "Error:") || strings.Contains(out, "Usage:") {
		t.Fatalf("expected child exit produced CLI error output:\n%s", out)
	}
	matches, err := filepath.Glob(filepath.Join(tmpDir, "mgcl-tofu-*.tfrc"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary CLI config was not cleaned up: %v", matches)
	}
}

func TestCLIBadConfigFailsBeforeStartup(t *testing.T) {
	requireCLI(t)

	bin := buildMgcl(t)
	dir := t.TempDir()
	initTestGitRepo(t, dir)
	dbPath := filepath.Join(dir, "mgcl.db")
	keyPath := writeTestIdentity(t)

	if err := os.WriteFile(filepath.Join(dir, ".mgcl.yaml"), []byte("host: [unterminated\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out := runMgclSync(t, bin, dir, baseEnv(),
		"run", "--host", "127.0.0.1:0", "--db", dbPath,
		"--encryption-key-file", keyPath,
		"--", "sh", "-c", "true")

	if code == 0 {
		t.Fatalf("mgcl started despite invalid config:\n%s", out)
	}
	if strings.Contains(out, "MgCl server listening on") {
		t.Fatalf("server started despite invalid config:\n%s", out)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("database was created before config validation: %v", err)
	}
	if !strings.Contains(out, "read config") {
		t.Fatalf("expected config read error, got:\n%s", out)
	}
}

func TestCLIMissingDBFailsBeforeStartup(t *testing.T) {
	requireCLI(t)

	bin := buildMgcl(t)
	dir := t.TempDir()
	missingKey := filepath.Join(dir, "missing-key")

	code, out := runMgclSync(t, bin, dir, baseEnv(),
		"run", "--host", "127.0.0.1:0", "--encryption-key-file", missingKey,
		"--", "sh", "-c", "true")

	if code != 1 {
		t.Fatalf("exit code = %d, want 1\n%s", code, out)
	}
	if !strings.Contains(out, "database path is required") {
		t.Fatalf("expected missing database error, got:\n%s", out)
	}
	if strings.Contains(out, "MgCl server listening on") {
		t.Fatalf("server started without a database path:\n%s", out)
	}
}

func TestCLIExplicitEmptyDBDoesNotFallBackToEnvironment(t *testing.T) {
	requireCLI(t)

	bin := buildMgcl(t)
	dir := t.TempDir()
	keyPath := writeTestIdentity(t)
	envDB := filepath.Join(dir, "env.db")

	code, out := runMgclSync(t, bin, dir, baseEnv("MGCL_DB="+envDB),
		"run", "--host", "127.0.0.1:0", "--db", "", "--encryption-key-file", keyPath,
		"--", "sh", "-c", "true")

	if code != 1 {
		t.Fatalf("exit code = %d, want 1\n%s", code, out)
	}
	if !strings.Contains(out, "database path is required") {
		t.Fatalf("expected missing database error, got:\n%s", out)
	}
	if _, err := os.Stat(envDB); !os.IsNotExist(err) {
		t.Fatalf("environment database was used despite explicit empty --db: %v", err)
	}
}

func TestCLIConfigDBRelativeToGitRootFromSubdirectory(t *testing.T) {
	requireCLI(t)

	bin := buildMgcl(t)
	root := t.TempDir()
	initTestGitRepo(t, root)
	subdir := filepath.Join(root, "nested", "workdir")
	if err := os.MkdirAll(subdir, 0o700); err != nil {
		t.Fatal(err)
	}
	keyPath := writeTestIdentity(t)
	config := fmt.Sprintf("host: \"127.0.0.1:0\"\ndb: project.db\nencryption_key_file: %q\n", keyPath)
	if err := os.WriteFile(filepath.Join(root, ".mgcl.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out := runMgclSync(t, bin, subdir, baseEnv(),
		"run", "--", "sh", "-c", "true")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(root, "project.db")); err != nil {
		t.Fatalf("database was not created relative to Git root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(subdir, "project.db")); !os.IsNotExist(err) {
		t.Fatalf("database was created relative to the working directory: %v", err)
	}
}

func TestCLINonGitUsesFlagDBWithoutReadingMgclYAML(t *testing.T) {
	requireCLI(t)

	bin := buildMgcl(t)
	dir := t.TempDir()
	keyPath := writeTestIdentity(t)
	dbPath := filepath.Join(dir, "flag.db")
	if err := os.WriteFile(filepath.Join(dir, ".mgcl.yaml"), []byte("host: [unterminated\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out := runMgclSync(t, bin, dir, baseEnv(),
		"run", "--host", "127.0.0.1:0", "--db", dbPath,
		"--encryption-key-file", keyPath, "--", "sh", "-c", "true")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out)
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("database was not created: %v", err)
	}
}

func TestCLINonGitUsesEnvDBRelativeToCwd(t *testing.T) {
	requireCLI(t)

	bin := buildMgcl(t)
	dir := t.TempDir()
	keyPath := writeTestIdentity(t)
	if err := os.WriteFile(filepath.Join(dir, ".mgcl.yaml"), []byte("host: [unterminated\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out := runMgclSync(t, bin, dir, baseEnv("MGCL_DB=env.db"),
		"run", "--host", "127.0.0.1:0", "--encryption-key-file", keyPath,
		"--", "sh", "-c", "true")

	dbPath := filepath.Join(dir, "env.db")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out)
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("database was not created relative to cwd: %v", err)
	}
}

func TestCLIHelpAndVersionDoNotRequireDB(t *testing.T) {
	requireCLI(t)

	bin := buildMgcl(t)
	dir := t.TempDir()

	for _, args := range [][]string{
		{"run", "--help"},
		{"run", "--version"},
	} {
		code, out := runMgclSync(t, bin, dir, baseEnv(), args...)
		if code != 0 {
			t.Fatalf("%v exit code = %d, want 0\n%s", args, code, out)
		}
		if strings.Contains(out, "Error:") {
			t.Fatalf("%v failed without a database:\n%s", args, out)
		}
	}
}

func TestCLISignalForwardedToProcessGroup(t *testing.T) {
	requireCLI(t)

	bin := buildMgcl(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mgcl.db")
	keyPath := writeTestIdentity(t)
	pidFile := filepath.Join(dir, "child.pid")
	marker := filepath.Join(dir, "grandchild.term")

	// The background shell must receive SIGTERM through the process group. If
	// mgcl only kills the direct child, the marker file is never written.
	script := `echo $$ > "$CHILD_PID_FILE"; ` +
		`sh -c 'trap "touch \"$GRANDCHILD_MARKER\"; exit 0" TERM; while :; do sleep 0.1; done' & wait`

	env := baseEnv(
		"CHILD_PID_FILE="+pidFile,
		"GRANDCHILD_MARKER="+marker,
	)
	cmd := startMgclProcess(t, bin, dir, env,
		"run", "--host", "127.0.0.1:0", "--db", dbPath,
		"--encryption-key-file", keyPath,
		"--", "sh", "-c", script)

	waitForFile(t, pidFile, 10*time.Second)
	killProcessGroupOnCleanup(t, pidFile)
	childPID := readPID(t, pidFile)

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	code := waitMgclExit(t, cmd, 10*time.Second)
	if code != 128+int(syscall.SIGTERM) {
		t.Fatalf("exit code = %d, want %d", code, 128+int(syscall.SIGTERM))
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("grandchild did not receive forwarded signal: %v", err)
	}
	if err := syscall.Kill(childPID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("child process %d still exists: %v", childPID, err)
	}
}

func TestCLISecondSignalForcesShutdown(t *testing.T) {
	requireCLI(t)

	bin := buildMgcl(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mgcl.db")
	keyPath := writeTestIdentity(t)
	pidFile := filepath.Join(dir, "child.pid")

	// The child ignores TERM and INT, so only a second signal (which force-kills
	// the process group) can end it before the grace period expires.
	script := `echo $$ > "$CHILD_PID_FILE"; trap "" TERM INT; while :; do sleep 0.2; done`

	env := baseEnv("CHILD_PID_FILE=" + pidFile)
	cmd := startMgclProcess(t, bin, dir, env,
		"run", "--host", "127.0.0.1:0", "--db", dbPath,
		"--encryption-key-file", keyPath,
		"--", "sh", "-c", script)

	waitForFile(t, pidFile, 10*time.Second)
	killProcessGroupOnCleanup(t, pidFile)
	childPID := readPID(t, pidFile)

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}

	// The normal grace period is 10s; a forced exit must be much faster.
	code := waitMgclExit(t, cmd, 5*time.Second)
	if code != 128+int(syscall.SIGTERM) {
		t.Fatalf("exit code = %d, want %d", code, 128+int(syscall.SIGTERM))
	}
	if err := syscall.Kill(childPID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("child process %d still exists: %v", childPID, err)
	}
}

func TestCLISignalsHupQuitForwarded(t *testing.T) {
	requireCLI(t)

	bin := buildMgcl(t)
	for _, sig := range []syscall.Signal{syscall.SIGHUP, syscall.SIGQUIT} {
		sig := sig
		t.Run(sig.String(), func(t *testing.T) {
			dir := t.TempDir()
			dbPath := filepath.Join(dir, "mgcl.db")
			keyPath := writeTestIdentity(t)
			pidFile := filepath.Join(dir, "child.pid")

			cmd := startMgclProcess(t, bin, dir, baseEnv("CHILD_PID_FILE="+pidFile),
				"run", "--host", "127.0.0.1:0", "--db", dbPath,
				"--encryption-key-file", keyPath,
				"--", "sh", "-c", `echo $$ > "$CHILD_PID_FILE"; exec sleep 30`)

			waitForFile(t, pidFile, 10*time.Second)
			killProcessGroupOnCleanup(t, pidFile)
			childPID := readPID(t, pidFile)

			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			code := waitMgclExit(t, cmd, 5*time.Second)
			if code != 128+int(sig) {
				t.Fatalf("exit code = %d, want %d", code, 128+int(sig))
			}
			if err := syscall.Kill(childPID, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("child process %d still exists: %v", childPID, err)
			}
		})
	}
}

func TestCLIPTYInteractiveRead(t *testing.T) {
	requireCLI(t)

	bin := buildMgcl(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mgcl.db")
	keyPath := writeTestIdentity(t)
	pidFile := filepath.Join(dir, "child.pid")
	readyFile := filepath.Join(dir, "ready")

	childScript := `echo $$ > "$CHILD_PID_FILE"; touch "$READY_FILE"; read x; echo "GOT:$x"`
	command := shellQuotedCommand(bin,
		"run", "--host", "127.0.0.1:0", "--db", dbPath,
		"--encryption-key-file", keyPath,
		"--", "sh", "-c", childScript)
	env := baseEnv(
		"CHILD_PID_FILE="+pidFile,
		"READY_FILE="+readyFile,
	)

	cmd, stdin, out := startScriptPTY(t, dir, command, env)
	waitForFile(t, readyFile, 10*time.Second)
	killProcessGroupOnCleanup(t, pidFile)

	if _, err := io.WriteString(stdin, "hello\n"); err != nil {
		t.Fatal(err)
	}
	_ = stdin.Close()

	code := waitMgclExit(t, cmd, 10*time.Second)
	output := out.String()
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, output)
	}
	if !strings.Contains(output, "GOT:hello") {
		t.Fatalf("stdin was not delivered through the PTY:\n%s", output)
	}
}

func TestCLIPTYCtrlC(t *testing.T) {
	requireCLI(t)

	bin := buildMgcl(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mgcl.db")
	keyPath := writeTestIdentity(t)
	pidFile := filepath.Join(dir, "child.pid")
	readyFile := filepath.Join(dir, "ready")

	childScript := `echo $$ > "$CHILD_PID_FILE"; touch "$READY_FILE"; read x; echo AFTER`
	command := shellQuotedCommand(bin,
		"run", "--host", "127.0.0.1:0", "--db", dbPath,
		"--encryption-key-file", keyPath,
		"--", "sh", "-c", childScript)
	env := baseEnv(
		"CHILD_PID_FILE="+pidFile,
		"READY_FILE="+readyFile,
	)

	cmd, stdin, out := startScriptPTY(t, dir, command, env)
	waitForFile(t, readyFile, 10*time.Second)
	killProcessGroupOnCleanup(t, pidFile)
	childPID := readPID(t, pidFile)

	// Let the child enter read and finish the foreground process group handoff
	// before sending the terminal interrupt character.
	time.Sleep(200 * time.Millisecond)
	if _, err := io.WriteString(stdin, "\x03"); err != nil {
		t.Fatal(err)
	}
	_ = stdin.Close()

	code := waitMgclExit(t, cmd, 10*time.Second)
	output := out.String()
	if code != 128+int(syscall.SIGINT) {
		t.Fatalf("exit code = %d, want %d\n%s", code, 128+int(syscall.SIGINT), output)
	}
	if strings.Contains(output, "AFTER") {
		t.Fatalf("interrupted read continued:\n%s", output)
	}
	if err := syscall.Kill(childPID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("child process %d still exists: %v", childPID, err)
	}
}

func TestCLIPTYTofuPrompt(t *testing.T) {
	requireCLI(t)

	tofuPath, err := exec.LookPath("tofu")
	if err != nil {
		t.Skip("tofu not found on PATH, skipping PTY tofu prompt test")
	}
	bin := buildMgcl(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mgcl.db")
	keyPath := writeTestIdentity(t)
	readyFile := filepath.Join(dir, "ready")

	tf := `
variable "name" {}
output "name" { value = var.name }
`
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(tf), 0o644); err != nil {
		t.Fatal(err)
	}

	childScript := `touch "$READY_FILE"; exec ` + shellQuote(tofuPath) + ` plan -input=true -no-color`
	command := shellQuotedCommand(bin,
		"run", "--host", "127.0.0.1:0", "--db", dbPath,
		"--encryption-key-file", keyPath,
		"--", "sh", "-c", childScript)
	env := baseEnv("READY_FILE=" + readyFile)

	cmd, stdin, out := startScriptPTY(t, dir, command, env)
	waitForFile(t, readyFile, 10*time.Second)
	time.Sleep(1 * time.Second)

	if _, err := io.WriteString(stdin, "world\n"); err != nil {
		t.Fatal(err)
	}
	_ = stdin.Close()

	code := waitMgclExit(t, cmd, 30*time.Second)
	output := out.String()
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, output)
	}
	if !strings.Contains(output, `name = "world"`) {
		t.Fatalf("tofu prompt did not receive input:\n%s", output)
	}
}
