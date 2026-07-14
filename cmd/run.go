package cmd

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var runCmd = &cobra.Command{
	Use:   "run [flags] [-- command [args...]]",
	Short: "Start local MGCL server and optionally run a command",
	Long: `Start a local HTTP server that serves /.well-known/terraform.json.

If a command is specified after --, it will be executed with TF_CLOUD_HOSTNAME
set to the server URL. The server will be shut down when the command exits.
If no command is specified, the server runs until SIGINT/SIGTERM.

Flags:
  -h, --host    Listen address (default "127.0.0.1:1217")
  -s, --server  Base URL for Terraform (default derived from host)`,
	DisableFlagParsing: true,
	RunE:               runRun,
}

func init() {
	viper.BindPFlag("host", runCmd.Flags().Lookup("host"))
	viper.BindPFlag("server", runCmd.Flags().Lookup("server"))
}

// terraformHandler returns an http.Handler that serves /.well-known/terraform.json.
func terraformHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/terraform.json", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/terraform.json" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, "{}")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	return mux
}

// deriveServerURL computes the server URL to inject into TF_CLOUD_HOSTNAME.
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

// withTFCloudHostname returns a copy of env with TF_CLOUD_HOSTNAME set to server.
// Any existing TF_CLOUD_HOSTNAME entries are removed first.
func withTFCloudHostname(env []string, server string) []string {
	filtered := make([]string, 0, len(env)+1)
	for _, e := range env {
		if !strings.HasPrefix(e, "TF_CLOUD_HOSTNAME=") {
			filtered = append(filtered, e)
		}
	}
	return append(filtered, "TF_CLOUD_HOSTNAME="+server)
}

func runRun(cmd *cobra.Command, args []string) error {
	// Manual flag parsing to handle -h for host (conflicts with help)
	host := "127.0.0.1:1217"
	serverURL := ""
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

	server := &http.Server{Handler: terraformHandler()}

	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		}
	}()

	// Server is ready since listener is already bound
	fmt.Fprintf(os.Stderr, "MGCL server listening on %s\n", listener.Addr())

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

	// Set up environment with TF_CLOUD_HOSTNAME (deduped)
	execCmd.Env = withTFCloudHostname(os.Environ(), serverURL)

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
