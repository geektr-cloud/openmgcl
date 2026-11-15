package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/geektheripper/go-gutils/git/git_utils"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var version = "dev"

// newRootCmd builds a fresh command tree. Execute uses a new tree each time so
// tests and repeated in-process invocations do not share Cobra flag state.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:     "mgcl",
		Short:   "OpenMgCl CLI",
		Long:    "OpenMgCl - a local TACOS state backend for OpenTofu",
		Version: version,
		// Errors are handled in main so expected child-process exit codes can
		// be returned without Cobra printing an error or usage message.
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	root.AddCommand(newRunCmd())
	root.AddCommand(newSecretCmd())
	root.AddCommand(newStateCmd())
	return root
}

// newConfig builds a Viper instance for project config and environment lookup.
// Inside a Git working tree it reads <git-root>/.mgcl.yaml, so commands started
// in a subdirectory still use the project configuration. Outside Git no config
// file is read. A missing config file is not an error; unreadable or malformed
// config files are.
func newConfig() (*viper.Viper, string, error) {
	v := viper.New()
	v.SetEnvPrefix("MGCL")
	// Bind only declared keys. AutomaticEnv would also expose undeclared keys
	// such as MGCL_BACKUP_TO, which the backup design deliberately does not
	// support. BindEnv keeps the existing empty-value semantics: an empty
	// environment variable does not override the config file.
	for _, key := range []string{"host", "server", "db", "token", "encryption_key_file"} {
		if err := v.BindEnv(key); err != nil {
			return nil, "", fmt.Errorf("bind environment variable for %s: %w", key, err)
		}
	}

	root, err := findProjectRoot()
	if err != nil {
		return nil, "", err
	}
	if root == "" {
		return v, "", nil
	}

	v.SetConfigFile(filepath.Join(root, ".mgcl.yaml"))
	if err := v.ReadInConfig(); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return v, root, nil
		}
		return nil, "", fmt.Errorf("read config: %w", err)
	}
	return v, root, nil
}

// findProjectRoot returns the nearest Git working tree root, or an empty string
// when the current directory is not inside a Git repository.
func findProjectRoot() (string, error) {
	repo, err := git_utils.NewWorkingRepo()
	if err != nil {
		if isNotGitRepoError(err) {
			return "", nil
		}
		return "", fmt.Errorf("locate git root: %w", err)
	}
	return repo.Root, nil
}

// isNotGitRepoError isolates git_utils' lack of a sentinel error. Keep the
// string check in this single compatibility point.
func isNotGitRepoError(err error) bool {
	return err != nil && err.Error() == "not in a git repo"
}

// Execute runs the CLI once.
func Execute() error {
	return newRootCmd().Execute()
}

// Main runs the CLI and maps errors and child-process exit codes to the process
// exit status. Both main packages use it so their behavior cannot drift.
func Main() {
	err := Execute()
	if err == nil {
		return
	}

	var exitErr interface{ ExitCode() int }
	if errors.As(err, &exitErr) {
		code := exitErr.ExitCode()
		if code <= 0 {
			code = 1
		}
		os.Exit(code)
	}

	fmt.Fprintln(os.Stderr, "Error:", err)
	os.Exit(1)
}
