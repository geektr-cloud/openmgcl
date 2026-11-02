package cmd

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"syscall"

	"github.com/geektr-cloud/openmgcl/internal/tacos"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
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
  -h, --host    Listen address (default "127.0.0.1:1217")
  -s, --server  Base URL for Terraform (default derived from host)
  -d, --db      SQLite database path (default "mgcl.db")
  -t, --token   Bearer token for API auth (empty = no auth)`,
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

func runRun(cmd *cobra.Command, args []string) error {
	// Manual flag parsing to handle -h for host (conflicts with help)
	host := "127.0.0.1:1217"
	serverURL := ""
	dbPath := "mgcl.db"
	token := ""
	var commandArgs []string

	i := 0
	for i < len(args) {
		arg := args[i]
		if arg == "--" {
			commandArgs = args[i+1:]
			break
		} else if arg == "--host" || arg == "-h" {
			if i+1 >= len(args) {
				return fmt.Errorf("flag %s requires a value", arg)
			}
			host = args[i+1]
			i += 2
		} else if arg == "--server" || arg == "-s" {
			if i+1 >= len(args) {
				return fmt.Errorf("flag %s requires a value", arg)
			}
			serverURL = args[i+1]
			i += 2
		} else if arg == "--db" || arg == "-d" {
			if i+1 >= len(args) {
				return fmt.Errorf("flag %s requires a value", arg)
			}
			dbPath = args[i+1]
			i += 2
		} else if arg == "--token" || arg == "-t" {
			if i+1 >= len(args) {
				return fmt.Errorf("flag %s requires a value", arg)
			}
			token = args[i+1]
			i += 2
		} else if arg == "--version" {
			fmt.Printf("mgcl %s\n", version)
			return nil
		} else if arg == "--help" {
			fmt.Print(cmd.Long)
			return nil
		} else {
			return fmt.Errorf("unknown flag: %s", arg)
		}
	}

	// Check config file values
	if host == "127.0.0.1:1217" && viper.GetString("host") != "" {
		host = viper.GetString("host")
	}
	if serverURL == "" && viper.GetString("server") != "" {
		serverURL = viper.GetString("server")
	}
	if dbPath == "mgcl.db" && viper.GetString("db") != "" {
		dbPath = viper.GetString("db")
	}
	if token == "" && viper.GetString("token") != "" {
		token = viper.GetString("token")
	}

	// Open the TACOS store
	store, err := tacos.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer store.Close()

	// Start HTTP server
	listener, err := net.Listen("tcp", host)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", host, err)
	}

	// Derive server URL using actual listener address (handles port 0)
	serverURL, err = deriveServerURL(host, listener.Addr(), serverURL)
	if err != nil {
		listener.Close()
		return fmt.Errorf("derive server URL: %w", err)
	}

	handler := tacos.NewRouter(store, serverURL, token)
	server := &http.Server{Handler: handler}

	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		}
	}()

	// Server is ready since listener is already bound
	fmt.Fprintf(os.Stderr, "MgCl server listening on %s\n", listener.Addr())

	if len(commandArgs) == 0 {
		// No command - wait for signal
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		fmt.Fprintf(os.Stderr, "\nReceived %v, shutting down...\n", sig)
		server.Shutdown(context.Background())
		return nil
	}

	// Run the command
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	execCmd := exec.CommandContext(ctx, commandArgs[0], commandArgs[1:]...)
	execCmd.Stdin = os.Stdin
	execCmd.Stdout = os.Stdout
	execCmd.Stderr = os.Stderr

	// Point OpenTofu at the local server. TF_CLOUD_HOSTNAME must be host[:port];
	// TF_CLI_CONFIG_FILE carries the http service URLs and credentials.
	hostname := hostFromServerURL(serverURL)
	cliConfigPath, cleanupConfig, err := prepareCLIConfig(serverURL, hostname, token)
	if err != nil {
		server.Shutdown(context.Background())
		return err
	}
	defer cleanupConfig()

	execCmd.Env = withEnv(os.Environ(),
		"TF_CLOUD_HOSTNAME="+hostname,
		"TF_CLI_CONFIG_FILE="+cliConfigPath,
	)

	// Handle signals during command execution
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	errCh := make(chan error, 1)
	go func() {
		errCh <- execCmd.Run()
	}()

	select {
	case sig := <-sigCh:
		fmt.Fprintf(os.Stderr, "\nReceived %v, shutting down...\n", sig)
		cancel()
		<-errCh // Wait for command to exit
		server.Shutdown(context.Background())
		return fmt.Errorf("terminated by signal")
	case err := <-errCh:
		server.Shutdown(context.Background())
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				os.Exit(exitErr.ExitCode())
			}
			return err
		}
		return nil
	}
}
