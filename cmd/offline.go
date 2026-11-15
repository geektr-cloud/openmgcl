package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// offlineOptions is the resolved configuration shared by commands that open a
// local database without starting the HTTP server.
type offlineOptions struct {
	dbPath      string
	keyFile     string
	backupSpecs []string
	backupBase  string
}

// offlineFlagValues carries the raw persistent flag values into
// resolveOfflineOptions. Flags are read through cmd.Flags() so Cobra's
// Changed/empty-value semantics stay identical across commands.
type offlineFlagValues struct {
	db       string
	keyFile  string
	backupTo []string
}

// resolveOfflineOptions applies the same precedence as mgcl run:
// explicit flag > environment > <git-root>/.mgcl.yaml, with YAML paths relative
// to the Git root. An explicit --backup-to list replaces the YAML list.
func resolveOfflineOptions(cmd *cobra.Command, values offlineFlagValues, withBackup, needIdentity bool) (*offlineOptions, error) {
	config, projectRoot, err := newConfig()
	if err != nil {
		return nil, err
	}
	opts := &offlineOptions{}

	if cmd.Flags().Changed("db") {
		opts.dbPath = values.db
	} else if value := os.Getenv("MGCL_DB"); value != "" {
		opts.dbPath = value
	} else if value := config.GetString("db"); value != "" {
		opts.dbPath = resolveConfigDBPath(value, projectRoot)
	}

	if cmd.Flags().Changed("encryption-key-file") {
		opts.keyFile = values.keyFile
	} else if value := os.Getenv("MGCL_ENCRYPTION_KEY_FILE"); value != "" {
		opts.keyFile = value
	} else if value := config.GetString("encryption_key_file"); value != "" {
		opts.keyFile = value
	}
	if needIdentity && opts.keyFile == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve home directory for encryption key: %w", err)
		}
		opts.keyFile = filepath.Join(home, ".ssh", "id_ed25519")
	}

	if withBackup {
		specs, err := backupListFromConfig(config.Get("backup_to"))
		if err != nil {
			return nil, err
		}
		if cmd.Flags().Changed("backup-to") {
			opts.backupSpecs = values.backupTo
			opts.backupBase = ""
		} else {
			opts.backupSpecs = specs
			opts.backupBase = projectRoot
		}
	}
	return opts, nil
}
