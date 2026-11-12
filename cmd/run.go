package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/geektr-cloud/openmgcl/internal/tacos"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

const (
	defaultHost             = "127.0.0.1:1217"
	defaultDBPath           = "mgcl.db"
	childGracePeriod        = 10 * time.Second
	serverShutdownTimeout   = 10 * time.Second
	serverReadHeaderTimeout = 10 * time.Second
	serverIdleTimeout       = 2 * time.Minute
)

var runCmd = &cobra.Command{
	Use:   "run [flags] [-- command [args...]]",
	Short: "Start local MgCl server and optionally run a command",
	Long: `Start a local HTTP server that serves /.well-known/terraform.json
and a TACOS state.v2 backend backed by SQLite.

If a command is specified after --, it will be executed with TF_CLOUD_HOSTNAME
and TF_CLI_CONFIG_FILE set up so OpenTofu can reach the local server. An existing
TF_CLI_CONFIG_FILE is preserved and extended. The server will be shut down when
the command exits. If no command is specified, the server runs until
SIGINT/SIGTERM.

Flags:
  -h, --host                  Listen address (default "127.0.0.1:1217")
  -s, --server                Base URL for Terraform (default derived from host)
  -d, --db                    SQLite database path (default "mgcl.db")
  -t, --token                 Bearer token for API auth (empty = no auth)
      --encryption-key-file   SSH Ed25519 private key (default "~/.ssh/id_ed25519")`,
	DisableFlagParsing: true,
	RunE:               runRun,
}

// deriveServerURL computes the server URL for discovery and upload/download links.
// If explicitServer is non-empty, it is used as-is (with trailing slashes trimmed).
// Otherwise, the URL is derived from host, using listenerAddr for the actual port
// when the host port is 0.
func deriveServerURL(host string, listenerAddr net.Addr, explicitServer string) (string, error) {
	if explicitServer != "" {
		return strings.TrimRight(explicitServer, "/"), nil
	}

	// Get the actual address from listener (handles port 0)
	addr := listenerAddr.String()

	// Extract IP and port
	h, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("split listener addr: %w", err)
	}

	// Normalize wildcard/unspecified addresses to loopback
	if h == "0.0.0.0" || h == "::" || h == "" {
		h = "127.0.0.1"
	}

	// Handle IPv6
	if strings.Contains(h, ":") {
		h = "[" + h + "]"
	}

	return fmt.Sprintf("http://%s:%s", h, port), nil
}

// hostFromServerURL returns the host[:port] portion of serverURL, which is the
// format TF_CLOUD_HOSTNAME expects. If serverURL has no scheme it is treated as
// a bare host.
func hostFromServerURL(serverURL string) string {
	if strings.Contains(serverURL, "://") {
		if u, err := url.Parse(serverURL); err == nil && u.Host != "" {
			return u.Host
		}
	}
	return strings.TrimRight(serverURL, "/")
}

// withEnv returns a copy of env with the given KEY=VALUE pairs set. Any
// existing entries with the same keys are removed first.
func withEnv(env []string, kv ...string) []string {
	replace := make(map[string]struct{}, len(kv))
	for _, pair := range kv {
		if key, _, ok := strings.Cut(pair, "="); ok {
			replace[key] = struct{}{}
		}
	}

	out := make([]string, 0, len(env)+len(kv))
	for _, e := range env {
		if key, _, ok := strings.Cut(e, "="); ok {
			if _, replaced := replace[key]; replaced {
				continue
			}
		}
		out = append(out, e)
	}
	return append(out, kv...)
}

// dropEnv returns a copy of env without the given keys.
func dropEnv(env []string, keys ...string) []string {
	drop := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		drop[key] = struct{}{}
	}
	out := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if ok {
			if _, dropped := drop[key]; dropped {
				continue
			}
		}
		out = append(out, entry)
	}
	return out
}

// hclQuote returns s as an HCL quoted string literal.
func hclQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// hasHostBlock reports whether cfg already declares a host block for hostname.
// The check is textual because adding a duplicate host block would make
// OpenTofu reject the generated CLI config.
func hasHostBlock(cfg, hostname string) bool {
	re := regexp.MustCompile(`(?m)^\s*host\s+"` + regexp.QuoteMeta(hostname) + `"\s*\{`)
	return re.MatchString(cfg)
}

// buildCLIConfig appends a host block pointing at the local server to existing
// CLI config content. If the user already configured this host, their config is
// returned unchanged and wins.
func buildCLIConfig(existing, serverURL, hostname, token string) string {
	if hasHostBlock(existing, hostname) {
		return existing
	}

	var b strings.Builder
	b.WriteString(existing)
	if existing != "" {
		if !strings.HasSuffix(existing, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	b.WriteString("host ")
	b.WriteString(hclQuote(hostname))
	b.WriteString(" {\n  services = {\n")
	for _, svc := range []struct {
		id  string
		pad string
	}{
		{"state.v2", " "},
		{"tfe.v2", "   "},
		{"tfe.v2.1", " "},
	} {
		b.WriteString("    ")
		b.WriteString(hclQuote(svc.id))
		b.WriteString(svc.pad)
		b.WriteString("= ")
		b.WriteString(hclQuote(strings.TrimRight(serverURL, "/") + "/api/v2/"))
		b.WriteString("\n")
	}
	b.WriteString("  }\n}\n")

	if token != "" {
		b.WriteString("\ncredentials ")
		b.WriteString(hclQuote(hostname))
		b.WriteString(" {\n  token = ")
		b.WriteString(hclQuote(token))
		b.WriteString("\n}\n")
	}
	return b.String()
}

// prepareCLIConfig writes a temporary OpenTofu CLI config that points hostname
// at serverURL, preserving any existing TF_CLI_CONFIG_FILE content. The caller
// must invoke cleanup after the child process exits.
func prepareCLIConfig(serverURL, hostname, token string) (path string, cleanup func(), err error) {
	existing := ""
	if p := os.Getenv("TF_CLI_CONFIG_FILE"); p != "" {
		data, err := os.ReadFile(p)
		if err != nil {
			return "", nil, fmt.Errorf("read TF_CLI_CONFIG_FILE %q: %w", p, err)
		}
		existing = string(data)
	}

	content := buildCLIConfig(existing, serverURL, hostname, token)

	f, err := os.CreateTemp("", "mgcl-tofu-*.tfrc")
	if err != nil {
		return "", nil, fmt.Errorf("create temporary tofu CLI config: %w", err)
	}
	// os.CreateTemp creates the file with mode 0600, which matters because the
	// config may contain a Bearer token.
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", nil, fmt.Errorf("write temporary tofu CLI config: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", nil, fmt.Errorf("close temporary tofu CLI config: %w", err)
	}

	return f.Name(), func() { os.Remove(f.Name()) }, nil
}

// runOptions is the effective configuration for the run command after parsing
// flags and merging config/env values.
type runOptions struct {
	host              string
	serverURL         string
	dbPath            string
	token             string
	encryptionKeyFile string
	command           []string
	explicit          map[string]bool
	showVersion       bool
	showHelp          bool
}

// parseRunArgs manually parses flags so -h can mean --host instead of help. It
// records which flags were supplied explicitly; config/env values must not
// override an explicit flag, including one explicitly set to its default or to
// an empty string.
func parseRunArgs(args []string) (*runOptions, error) {
	opts := &runOptions{
		host:     defaultHost,
		dbPath:   defaultDBPath,
		explicit: make(map[string]bool, 5),
	}

	for i := 0; i < len(args); {
		arg := args[i]
		switch arg {
		case "--":
			opts.command = args[i+1:]
			return opts, nil
		case "--host", "-h":
			value, next, err := nextFlagValue(args, i, arg)
			if err != nil {
				return nil, err
			}
			opts.host = value
			opts.explicit["host"] = true
			i = next
		case "--server", "-s":
			value, next, err := nextFlagValue(args, i, arg)
			if err != nil {
				return nil, err
			}
			opts.serverURL = value
			opts.explicit["server"] = true
			i = next
		case "--db", "-d":
			value, next, err := nextFlagValue(args, i, arg)
			if err != nil {
				return nil, err
			}
			opts.dbPath = value
			opts.explicit["db"] = true
			i = next
		case "--token", "-t":
			value, next, err := nextFlagValue(args, i, arg)
			if err != nil {
				return nil, err
			}
			opts.token = value
			opts.explicit["token"] = true
			i = next
		case "--encryption-key-file":
			value, next, err := nextFlagValue(args, i, arg)
			if err != nil {
				return nil, err
			}
			opts.encryptionKeyFile = value
			opts.explicit["encryption_key_file"] = true
			i = next
		case "--version":
			opts.showVersion = true
			return opts, nil
		case "--help":
			opts.showHelp = true
			return opts, nil
		default:
			return nil, fmt.Errorf("unknown flag: %s", arg)
		}
	}
	return opts, nil
}

func nextFlagValue(args []string, i int, flag string) (string, int, error) {
	if i+1 >= len(args) {
		return "", 0, fmt.Errorf("flag %s requires a value", flag)
	}
	return args[i+1], i + 2, nil
}

// applyConfig fills values that were not supplied on the command line. An
// explicit flag always wins, even when it equals the default value or is empty.
func (opts *runOptions) applyConfig(config *viper.Viper) {
	if !opts.explicit["host"] {
		if value := config.GetString("host"); value != "" {
			opts.host = value
		}
	}
	if !opts.explicit["server"] {
		if value := config.GetString("server"); value != "" {
			opts.serverURL = value
		}
	}
	if !opts.explicit["db"] {
		if value := config.GetString("db"); value != "" {
			opts.dbPath = value
		}
	}
	if !opts.explicit["token"] {
		if value := config.GetString("token"); value != "" {
			opts.token = value
		}
	}
	if !opts.explicit["encryption_key_file"] {
		if value := config.GetString("encryption_key_file"); value != "" {
			opts.encryptionKeyFile = value
		}
	}
}

// exitCodeError carries a process exit code through Cobra without being printed
// as an error. main uses ExitCode after all defers have run.
type exitCodeError struct {
	code int
}

func (e *exitCodeError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func (e *exitCodeError) ExitCode() int { return e.code }

// shutdownServer gracefully stops the HTTP server within a bounded window,
// falling back to Close when handlers do not finish in time.
func shutdownServer(server *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), serverShutdownTimeout)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		_ = server.Close()
	}
}

// childExitCode converts a command wait error into an exit code. Non-ExitError
// failures are returned as errors; signal termination becomes 128+signal.
func childExitCode(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return processExitCode(exitErr.ProcessState), nil
	}
	return 0, err
}

// runChildProcess starts child in its own process group and waits for it. On
// SIGINT/SIGTERM the signal is forwarded to the whole group, a bounded grace
// period is allowed, and the group is killed if it does not exit. A second
// signal forces the group down immediately.
func runChildProcess(child *exec.Cmd, sigCh <-chan os.Signal) (int, error) {
	configureChildProcess(child)

	if err := child.Start(); err != nil {
		return 0, err
	}
	// Hand the controlling terminal to the child's process group so interactive
	// commands (tofu prompts, read, ...) can read stdin instead of receiving
	// SIGTTIN. The parent restores the terminal when the child finishes.
	restoreTerminal := handoffTerminalToChild(child)
	defer restoreTerminal()

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- child.Wait()
	}()

	select {
	case err := <-waitCh:
		// Prefer an already-pending shutdown signal over the child's own exit
		// status so a signal delivered around the same time always maps to
		// 128+signal.
		select {
		case sig := <-sigCh:
			fmt.Fprintf(os.Stderr, "\nReceived %v, shutting down...\n", sig)
			return signalExitCode(sig), nil
		default:
		}
		return childExitCode(err)
	case sig := <-sigCh:
		fmt.Fprintf(os.Stderr, "\nReceived %v, shutting down...\n", sig)
		forwardSignal(child, sig)
		return finishChildAfterSignal(child, waitCh, sigCh, sig), nil
	}
}

func finishChildAfterSignal(child *exec.Cmd, waitCh <-chan error, sigCh <-chan os.Signal, sig os.Signal) int {
	exitCode := signalExitCode(sig)
	grace := time.NewTimer(childGracePeriod)
	defer grace.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	childDone := false
	for {
		select {
		case <-waitCh:
			childDone = true
			waitCh = nil
			// The direct child is gone; wait for any remaining group members
			// until the grace period expires.
			reapAdoptedChildren()
			if !processGroupAlive(child) {
				return exitCode
			}
		case <-ticker.C:
			if childDone {
				reapAdoptedChildren()
				if !processGroupAlive(child) {
					return exitCode
				}
			}
		case second := <-sigCh:
			fmt.Fprintf(os.Stderr, "\nReceived %v, forcing shutdown...\n", second)
			killChildProcessGroup(child)
			if !childDone {
				<-waitCh
			}
			return exitCode
		case <-grace.C:
			killChildProcessGroup(child)
			if !childDone {
				<-waitCh
			}
			return exitCode
		}
	}
}

func runRun(cmd *cobra.Command, args []string) error {
	opts, err := parseRunArgs(args)
	if err != nil {
		return err
	}
	if opts.showVersion {
		fmt.Printf("mgcl %s\n", version)
		return nil
	}
	if opts.showHelp {
		fmt.Print(cmd.Long)
		return nil
	}

	// Load config before touching the key, database, or listener so malformed
	// config fails the command instead of silently running with defaults.
	config, err := newConfig()
	if err != nil {
		return err
	}
	opts.applyConfig(config)

	if opts.encryptionKeyFile == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolve home directory for encryption key: %w", err)
		}
		opts.encryptionKeyFile = filepath.Join(home, ".ssh", "id_ed25519")
	}

	identity, err := loadEncryptionIdentity(opts.encryptionKeyFile)
	if err != nil {
		return fmt.Errorf("load encryption key: %w", err)
	}

	// Open the TACOS store
	store, err := tacos.Open(opts.dbPath, identity)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer store.Close()

	// Start HTTP server
	listener, err := net.Listen("tcp", opts.host)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", opts.host, err)
	}

	// Derive server URL using actual listener address (handles port 0)
	serverURL, err := deriveServerURL(opts.host, listener.Addr(), opts.serverURL)
	if err != nil {
		listener.Close()
		return fmt.Errorf("derive server URL: %w", err)
	}

	handler := tacos.NewRouter(store, serverURL, opts.token)
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: serverReadHeaderTimeout,
		IdleTimeout:       serverIdleTimeout,
	}
	// Every exit path shuts the server down before the deferred store.Close.
	defer shutdownServer(server)

	// Register signal handling before announcing readiness so a signal sent
	// immediately after the listening line cannot bypass cleanup.
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, shutdownSignals...)
	defer signal.Stop(sigCh)

	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		}
	}()

	// Server is ready since listener is already bound
	fmt.Fprintf(os.Stderr, "MgCl server listening on %s\n", listener.Addr())

	if len(opts.command) == 0 {
		// No command - wait for signal
		sig := <-sigCh
		fmt.Fprintf(os.Stderr, "\nReceived %v, shutting down...\n", sig)
		return &exitCodeError{code: signalExitCode(sig)}
	}

	// Point OpenTofu at the local server. TF_CLOUD_HOSTNAME must be host[:port];
	// TF_CLI_CONFIG_FILE carries the http service URLs and credentials.
	hostname := hostFromServerURL(serverURL)
	cliConfigPath, cleanupConfig, err := prepareCLIConfig(serverURL, hostname, opts.token)
	if err != nil {
		return err
	}
	defer cleanupConfig()

	child := exec.Command(opts.command[0], opts.command[1:]...)
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	child.Env = withEnv(dropEnv(os.Environ(), passphraseEnv),
		"TF_CLOUD_HOSTNAME="+hostname,
		"TF_CLI_CONFIG_FILE="+cliConfigPath,
	)

	exitCode, err := runChildProcess(child, sigCh)
	if err != nil {
		return err
	}
	if exitCode != 0 {
		return &exitCodeError{code: exitCode}
	}
	return nil
}
