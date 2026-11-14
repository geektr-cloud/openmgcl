package cmd

import (
	"context"
	"crypto/rand"
	"encoding/base64"
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

	"github.com/geektr-cloud/openmgcl/internal/sqlitebackup"
	"github.com/geektr-cloud/openmgcl/internal/tacos"
	"github.com/geektr-cloud/openmgcl/pkg/providerapi"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

const (
	defaultHost             = "127.0.0.1:1217"
	childGracePeriod        = 10 * time.Second
	serverShutdownTimeout   = 10 * time.Second
	serverReadHeaderTimeout = 10 * time.Second
	serverIdleTimeout       = 2 * time.Minute
)

func newRunCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run [flags] [-- command [args...]]",
		Short: "Start local MgCl server and optionally run a command",
		Long: `Start a local HTTP server that serves /.well-known/terraform.json
and a TACOS state.v2 backend backed by SQLite.

If a command is specified after --, it will be executed with TF_CLOUD_HOSTNAME
and TF_CLI_CONFIG_FILE set up so OpenTofu can reach the local server, plus
MGCL_PROVIDER_ENDPOINT and MGCL_PROVIDER_TOKEN for the ephemeral mgcl Secret
provider. The user's CLI config is preserved and extended: TF_CLI_CONFIG_FILE
when set, otherwise the default ~/.tofurc. The server will be shut down when
the command exits. If no command is specified, the server runs until
SIGINT/SIGTERM; a command-less run never issues a provider token.

When no TACOS user token is configured, mgcl generates a fresh random token for
each run. A run with a child writes it only into the temporary CLI config and
never prints it. A command-less run prints it once to stderr after the listening
line so external OpenTofu clients can be configured. The token is valid only for
the lifetime of this run.

Listening on anything other than the literal host 127.0.0.1 publishes state
and secrets. It requires a TTY confirmation or the exact flag
--yes-i-know-it-will-publish-state-and-secrets.

Configuration:
  Inside a Git working tree, <git-root>/.mgcl.yaml supplies project config.
  A database path is required: use --db, MGCL_DB, or db in .mgcl.yaml.

Flags:
  -h, --host                  Listen address (default "127.0.0.1:1217")
  -s, --server                Base URL for Terraform (default derived from host)
  -d, --db                    SQLite database path (required)
  -t, --token                 Bearer token for API auth (default: generated per run)
      --encryption-key-file   SSH Ed25519 private key (default "~/.ssh/id_ed25519")
      --backup-to             Backup destination; repeatable (sqlite:<path> or path:<path>)
      --yes-i-know-it-will-publish-state-and-secrets
                              Skip the non-loopback confirmation prompt`,
		DisableFlagParsing: true,
		RunE:               runRun,
	}
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

	return (&url.URL{Scheme: "http", Host: net.JoinHostPort(h, port)}).String(), nil
}

// deriveProviderEndpoint builds the provider base URL from the actual listener,
// never from --server. Wildcard listeners are normalized to 127.0.0.1 because
// that is the address a local child can use.
func deriveProviderEndpoint(listenerAddr net.Addr) (string, error) {
	if listenerAddr == nil {
		return "", errors.New("listener address is not available")
	}
	h, port, err := net.SplitHostPort(listenerAddr.String())
	if err != nil {
		return "", err
	}
	if h == "0.0.0.0" || h == "::" || h == "" {
		h = "127.0.0.1"
	}
	return (&url.URL{Scheme: "http", Host: net.JoinHostPort(h, port)}).String(), nil
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

// readExistingCLIConfig returns the OpenTofu CLI config that mgcl extends.
// TF_CLI_CONFIG_FILE wins when it is set. Otherwise mgcl inherits the default
// user config (~/.tofurc) so settings such as provider_installation
// dev_overrides keep working in the child process. A missing default config is
// equivalent to an empty one; an unreadable one is an error.
func readExistingCLIConfig() (string, error) {
	if p := os.Getenv("TF_CLI_CONFIG_FILE"); p != "" {
		data, err := os.ReadFile(p)
		if err != nil {
			return "", fmt.Errorf("read TF_CLI_CONFIG_FILE %q: %w", p, err)
		}
		return string(data), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		// Without a home directory there is no default CLI config to inherit.
		return "", nil
	}
	p := filepath.Join(home, ".tofurc")
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read default OpenTofu CLI config %q: %w", p, err)
	}
	return string(data), nil
}

// prepareCLIConfig writes a temporary OpenTofu CLI config that points hostname
// at serverURL, preserving the user's CLI config (TF_CLI_CONFIG_FILE when set,
// else the default ~/.tofurc). The caller must invoke cleanup after the child
// process exits.
func prepareCLIConfig(serverURL, hostname, token string) (path string, cleanup func(), err error) {
	existing, err := readExistingCLIConfig()
	if err != nil {
		return "", nil, err
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

// generateRunToken returns a fresh TACOS user token. The token is random per
// run, URL-safe, and never derived from the provider session or persisted.
func generateRunToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", errors.New("generate access token")
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// resolveRunToken keeps an explicitly configured token and generates a new one
// otherwise. generated reports whether the caller must treat the token as this
// run's ephemeral secret.
func resolveRunToken(configured string, generate func() (string, error)) (token string, generated bool, err error) {
	if configured != "" {
		return configured, false, nil
	}
	token, err = generate()
	if err != nil {
		return "", false, err
	}
	return token, true, nil
}

// runOptions is the effective configuration for the run command after parsing
// flags and merging config/env values.
type runOptions struct {
	host              string
	serverURL         string
	dbPath            string
	token             string
	encryptionKeyFile string
	backupTo          []string
	command           []string
	explicit          map[string]bool
	showVersion       bool
	showHelp          bool
	yesPublish        bool
}

// parseRunArgs manually parses flags so -h can mean --host instead of help. It
// records which flags were supplied explicitly; config/env values must not
// override an explicit flag, including one explicitly set to its default or to
// an empty string.
func parseRunArgs(args []string) (*runOptions, error) {
	opts := &runOptions{
		host:     defaultHost,
		explicit: make(map[string]bool, 6),
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
		case "--backup-to":
			value, next, err := nextFlagValue(args, i, arg)
			if err != nil {
				return nil, err
			}
			opts.backupTo = append(opts.backupTo, value)
			opts.explicit["backup_to"] = true
			i = next
		case "--version":
			opts.showVersion = true
			return opts, nil
		case "--yes-i-know-it-will-publish-state-and-secrets":
			opts.yesPublish = true
			i++
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
// Environment values take precedence over the project config file. The db value
// is handled explicitly so its origin decides how a relative path is resolved:
// flag/env stay relative to the process working directory, while a config-file
// path is relative to the Git root.
func (opts *runOptions) applyConfig(config *viper.Viper, projectRoot string) {
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
		if value := os.Getenv("MGCL_DB"); value != "" {
			opts.dbPath = value
		} else if value := config.GetString("db"); value != "" {
			opts.dbPath = resolveConfigDBPath(value, projectRoot)
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

// resolveConfigDBPath anchors a relative config-file database path at the Git
// root. Absolute paths are returned unchanged.
func resolveConfigDBPath(value, projectRoot string) string {
	if filepath.IsAbs(value) || projectRoot == "" {
		return value
	}
	return filepath.Join(projectRoot, value)
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
// signal forces the group down immediately. A closed backupFatal channel means
// the backup layer has failed fatally: the child group is terminated and
// backupTriggered is true so the caller does not report a signal exit.
func runChildProcess(child *exec.Cmd, sigCh <-chan os.Signal, backupFatal <-chan struct{}) (exitCode int, backupTriggered bool, err error) {
	configureChildProcess(child)

	if err := child.Start(); err != nil {
		return 0, false, err
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
			return signalExitCode(sig), false, nil
		default:
		}
		code, err := childExitCode(err)
		if err != nil {
			return 0, false, err
		}
		// The child exited on its own; preserve its code even if a backup fatal
		// arrived concurrently. The deferred backup shutdown still reports the
		// backup failure and adjusts a successful child's exit code to 1.
		return code, false, nil
	case sig := <-sigCh:
		fmt.Fprintf(os.Stderr, "\nReceived %v, shutting down...\n", sig)
		forwardSignal(child, sig)
		return finishChildAfterSignal(child, waitCh, sigCh, sig), false, nil
	case <-backupFatal:
		// If the child already exited, let its own status win. Only terminate a
		// still-running child and report the backup-triggered exit otherwise.
		select {
		case err := <-waitCh:
			code, codeErr := childExitCode(err)
			return code, false, codeErr
		default:
		}
		killChildProcessGroup(child)
		waitErr := <-waitCh
		if waitErr == nil {
			return 0, false, nil
		}
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) && !processSignaled(exitErr.ProcessState) {
			code, codeErr := childExitCode(waitErr)
			return code, false, codeErr
		}
		return 0, true, nil
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

func runRun(cmd *cobra.Command, args []string) (retErr error) {
	opts, err := parseRunArgs(args)
	if err != nil {
		return err
	}
	if opts.showVersion {
		fmt.Fprintf(cmd.OutOrStdout(), "mgcl %s\n", version)
		return nil
	}
	if opts.showHelp {
		fmt.Fprint(cmd.OutOrStdout(), cmd.Long)
		return nil
	}

	// Load config before touching the key, database, or listener so malformed
	// config fails the command instead of silently running with defaults.
	config, projectRoot, err := newConfig()
	if err != nil {
		return err
	}
	opts.applyConfig(config, projectRoot)

	if opts.dbPath == "" {
		return errors.New("database path is required: set --db, MGCL_DB, or db in .mgcl.yaml")
	}

	// Enforce the non-loopback exposure gate before any key, database,
	// backup probe or listener work.
	if err := checkPublishSafety(opts.host, opts.serverURL, opts.yesPublish, os.Stderr); err != nil {
		return err
	}

	// A configured token keeps its fixed value. Otherwise each run gets a fresh
	// random token: a run with a child passes it only through the temporary CLI
	// config, while a command-less run prints it after listening so external
	// clients can use it.
	tokenGenerated := false
	opts.token, tokenGenerated, err = resolveRunToken(opts.token, generateRunToken)
	if err != nil {
		return err
	}

	// Resolve and validate backup destinations before loading the key or
	// opening the database. Path destinations are probed here as well.
	backupSpecs, backupBase, err := opts.backupDestinations(config, projectRoot)
	if err != nil {
		return err
	}
	var backupManager *sqlitebackup.Manager
	if len(backupSpecs) > 0 {
		destinations, err := sqlitebackup.ParseDestinations(backupSpecs, backupBase)
		if err != nil {
			return fmt.Errorf("backup destination: %w", err)
		}
		backupManager, err = sqlitebackup.New(opts.dbPath, destinations)
		if err != nil {
			return fmt.Errorf("backup destination: %w", err)
		}
	}

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

	// Open the TACOS store. With backups enabled this also performs the initial
	// sqlite backup before any listener is created.
	store, err := tacos.OpenWithBackup(opts.dbPath, identity, backupManager)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}

	var server *http.Server
	sessionStarted := false
	defer func() {
		// Close the write gate first and wait for in-flight writes before HTTP
		// shutdown and the backup drain. Store.Close repeats this idempotently.
		store.StopWrites()
		if server != nil {
			shutdownServer(server)
		}
		var closeErr error
		if sessionStarted {
			closeErr = store.Close()
		} else {
			closeErr = store.Abort()
		}
		if closeErr != nil {
			fmt.Fprintf(os.Stderr, "backup failed: %v\n", closeErr)
			if retErr == nil {
				retErr = &exitCodeError{code: 1}
			}
		}
	}()

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
	providerEndpoint, err := deriveProviderEndpoint(listener.Addr())
	if err != nil {
		listener.Close()
		return fmt.Errorf("derive provider endpoint: %w", err)
	}

	var providers *tacos.ProviderSessions
	if len(opts.command) > 0 {
		// Only a run with a child gets a provider session. A long-running
		// server never issues or prints a token.
		providers = tacos.NewProviderSessions()
		defer providers.RevokeAll()
	}

	handler := tacos.NewRouter(store, serverURL, opts.token, providers)
	server = &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: serverReadHeaderTimeout,
		IdleTimeout:       serverIdleTimeout,
	}

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
	if tokenGenerated && len(opts.command) == 0 {
		// Only the command-less run needs the operator to copy the generated
		// token into an external OpenTofu CLI config. A run with a child
		// already receives it through the private temporary config.
		fmt.Fprintf(os.Stderr, "MgCl access token: %s\n", opts.token)
	}
	sessionStarted = true

	var providerToken string
	if providers != nil {
		providerToken, err = providers.Issue()
		if err != nil {
			return fmt.Errorf("create provider session: %w", err)
		}
	}

	var backupFatal <-chan struct{}
	if backupManager != nil {
		backupFatal = backupManager.Fatal()
	}

	if len(opts.command) == 0 {
		// No command - wait for a signal or a fatal backup failure.
		select {
		case sig := <-sigCh:
			fmt.Fprintf(os.Stderr, "\nReceived %v, shutting down...\n", sig)
			return &exitCodeError{code: signalExitCode(sig)}
		case <-backupFatal:
			return &exitCodeError{code: 1}
		}
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
	child.Env = withEnv(dropEnv(os.Environ(), passphraseEnv, providerapi.EnvEndpoint, providerapi.EnvToken),
		"TF_CLOUD_HOSTNAME="+hostname,
		"TF_CLI_CONFIG_FILE="+cliConfigPath,
		providerapi.EnvEndpoint+"="+providerEndpoint,
		providerapi.EnvToken+"="+providerToken,
	)

	exitCode, backupTriggered, err := runChildProcess(child, sigCh, backupFatal)
	// Revoke immediately when the child returns, before CLI-config cleanup and
	// deferred server/backup shutdown can delay token invalidation. RevokeAll is
	// idempotent; the defer remains as a fallback for early returns.
	if providers != nil {
		providers.RevokeAll()
	}
	if err != nil {
		return err
	}
	if backupTriggered {
		return &exitCodeError{code: 1}
	}
	if exitCode != 0 {
		return &exitCodeError{code: exitCode}
	}
	return nil
}
