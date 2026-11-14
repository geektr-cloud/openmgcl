package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/geektr-cloud/openmgcl/internal/sqlitebackup"
	"github.com/geektr-cloud/openmgcl/internal/tacos"
	"github.com/spf13/cobra"
)

type secretFlags struct {
	db       string
	keyFile  string
	backupTo []string
	useStdin bool
	yes      bool
	output   string
}

type secretOptions struct {
	dbPath      string
	keyFile     string
	backupSpecs []string
	backupBase  string
}

func newSecretCmd() *cobra.Command {
	flags := &secretFlags{}
	secretCmd := &cobra.Command{
		Use:   "secret",
		Short: "Manage bootstrap secrets in the local database",
		Long: `Manage bootstrap secrets offline in the same mgcl.db used by mgcl run.

Secret names and KV keys match ^[a-z0-9][a-z0-9_-]{0,63}$. A KV reference is
<secret-name>/<key>. Values are always encrypted at rest with a per-Secret DEK,
which is wrapped by the configured SSH Ed25519 recipient.

Configuration follows mgcl run: --db (or MGCL_DB, or db in .mgcl.yaml) is
required. --encryption-key-file (or MGCL_ENCRYPTION_KEY_FILE, or
encryption_key_file) defaults to ~/.ssh/id_ed25519. create/set/get/unset/delete
load the key; list/show do not.

Write commands are write sessions and honor --backup-to / backup_to. list,
show and get are read-only and never trigger backups.`,
	}
	secretCmd.PersistentFlags().StringVarP(&flags.db, "db", "d", "", "SQLite database path (required)")
	secretCmd.PersistentFlags().StringVar(&flags.keyFile, "encryption-key-file", "", "SSH Ed25519 private key (default \"~/.ssh/id_ed25519\")")
	secretCmd.PersistentFlags().StringArrayVar(&flags.backupTo, "backup-to", nil, "Backup destination for write commands; repeatable (sqlite:<path> or path:<path>)")

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List secret metadata",
		Long:  "List plaintext secret metadata sorted by name. This command does not load the encryption key or create a missing database.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSecretReadStore(cmd, flags, false, func(store *tacos.SecretStore) error {
				secrets, err := store.ListSecrets()
				if err != nil {
					return err
				}
				return writeSecretList(cmd, secrets)
			})
		},
	}

	showCmd := &cobra.Command{
		Use:   "show <secret-name>",
		Short: "Show secret metadata and KV keys",
		Long:  "Show one secret's metadata and KV key names. Values, DEKs, wrapped keys and nonces are never printed. This command does not load the encryption key or create a missing database.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch flags.output {
			case "table", "yaml", "json":
			default:
				return fmt.Errorf("unsupported output format %q (want table, yaml, or json)", flags.output)
			}
			return withSecretReadStore(cmd, flags, false, func(store *tacos.SecretStore) error {
				details, err := store.ShowSecret(args[0])
				if err != nil {
					return err
				}
				return writeSecretDetails(cmd, details, flags.output)
			})
		},
	}
	showCmd.Flags().StringVarP(&flags.output, "output", "o", "table", "Output format: table, yaml, or json")

	createCmd := &cobra.Command{
		Use:   "create <secret-name>",
		Short: "Create an empty secret",
		Long:  "Create an empty secret and generate its independent 32-byte Secret DEK. The database is initialized if it does not exist. The encryption key is loaded and validated before the database is opened.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !tacos.ValidSecretName(args[0]) {
				return fmt.Errorf("invalid secret name %q", args[0])
			}
			return withSecretWriteStore(cmd, flags, true, true, func(store *tacos.SecretStore) (string, error) {
				if _, err := store.CreateSecret(args[0]); err != nil {
					return "", err
				}
				return fmt.Sprintf("Created secret %q.", args[0]), nil
			})
		},
	}

	setCmd := &cobra.Command{
		Use:   "set <secret-name>/<key> [value]",
		Short: "Set or overwrite one KV value",
		Long: `Set or overwrite one KV value. The secret must already exist.

The value can come from three mutually exclusive sources:
  1. The positional [value] argument. WARNING: it appears in shell history,
     process argv and the local process list. Prefer the other two forms.
  2. --stdin reads all bytes until EOF, preserving newlines and trailing bytes.
  3. With no value and a TTY stdin, one value is read without echo; paste or
     type it and press Enter. The prompt is written to stderr.

A new key sets created_at and updated_at; overwriting preserves created_at and
updates updated_at. Only the latest value is stored.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, key, err := tacos.ParseSecretReference(args[0])
			if err != nil {
				return err
			}
			hasPositional := len(args) == 2
			if hasPositional && flags.useStdin {
				return errors.New("--stdin cannot be combined with a positional value")
			}
			value, err := readSecretSetValue(cmd, args[0], hasPositional, args, flags.useStdin)
			if err != nil {
				return err
			}
			return withSecretWriteStore(cmd, flags, false, !flags.useStdin, func(store *tacos.SecretStore) (string, error) {
				if err := store.SetSecretValue(name, key, value); err != nil {
					return "", err
				}
				return fmt.Sprintf("Set %s/%s.", name, key), nil
			})
		},
	}
	setCmd.Flags().BoolVar(&flags.useStdin, "stdin", false, "Read the value from stdin until EOF")

	getCmd := &cobra.Command{
		Use:   "get <secret-name>/<key>",
		Short: "Print one decrypted KV value",
		Long:  "Print the raw UTF-8 value bytes to stdout without a trailing newline or prefix. No partial plaintext is written if decryption or authentication fails. This command does not create a missing database.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, key, err := tacos.ParseSecretReference(args[0])
			if err != nil {
				return err
			}
			return withSecretReadStore(cmd, flags, true, func(store *tacos.SecretStore) error {
				value, err := store.GetSecretValue(name, key)
				if err != nil {
					return err
				}
				_, err = cmd.OutOrStdout().Write(value)
				return err
			})
		},
	}

	unsetCmd := &cobra.Command{
		Use:   "unset <secret-name>/<key>",
		Short: "Delete one KV from a secret",
		Long:  "Delete one KV record. The Secret DEK must unwrap with the configured recipient before the delete transaction starts. Missing secrets and keys are errors.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, key, err := tacos.ParseSecretReference(args[0])
			if err != nil {
				return err
			}
			return withSecretWriteStore(cmd, flags, false, true, func(store *tacos.SecretStore) (string, error) {
				if err := store.UnsetSecretValue(name, key); err != nil {
					return "", err
				}
				return fmt.Sprintf("Unset %s/%s.", name, key), nil
			})
		},
	}

	deleteCmd := &cobra.Command{
		Use:   "delete <secret-name>",
		Short: "Delete a secret and all of its KVs",
		Long:  "Delete a secret, its wrapped Secret DEK and all KV rows. The Secret DEK must unwrap with the configured recipient before deletion. On a TTY, confirmation requires y or yes; a non-TTY requires --yes.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !tacos.ValidSecretName(args[0]) {
				return fmt.Errorf("invalid secret name %q", args[0])
			}
			return withSecretWriteStore(cmd, flags, false, true, func(store *tacos.SecretStore) (string, error) {
				var confirm func(int) (bool, error)
				if !flags.yes {
					if !stdinIsTerminal() {
						return "", errors.New("deleting a secret on non-TTY stdin requires --yes")
					}
					confirm = func(keyCount int) (bool, error) {
						fmt.Fprintf(cmd.ErrOrStderr(), "Delete secret %q with %d key(s)? [y/N] ", args[0], keyCount)
						answer, err := readTerminalLine()
						if err != nil {
							return false, err
						}
						switch strings.ToLower(strings.TrimSpace(answer)) {
						case "y", "yes":
							return true, nil
						default:
							return false, nil
						}
					}
				}
				if err := store.DeleteSecret(args[0], confirm); err != nil {
					return "", err
				}
				return fmt.Sprintf("Deleted secret %q.", args[0]), nil
			})
		},
	}
	deleteCmd.Flags().BoolVar(&flags.yes, "yes", false, "Skip interactive confirmation")

	secretCmd.AddCommand(listCmd, showCmd, createCmd, setCmd, getCmd, unsetCmd, deleteCmd)
	return secretCmd
}

func readSecretSetValue(cmd *cobra.Command, ref string, hasPositional bool, args []string, useStdin bool) ([]byte, error) {
	if hasPositional {
		return []byte(args[1]), nil
	}
	if useStdin {
		data, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), tacos.MaxSecretValueBytes+1))
		if err != nil {
			return nil, err
		}
		if len(data) > tacos.MaxSecretValueBytes {
			return nil, fmt.Errorf("%w: value exceeds %d bytes", tacos.ErrSecretInvalidValue, tacos.MaxSecretValueBytes)
		}
		return data, nil
	}
	if !stdinIsTerminal() {
		return nil, errors.New("no value provided: pass a value, use --stdin, or run with a TTY")
	}
	value, err := readTerminalPassword(fmt.Sprintf("Enter value for %s: ", ref), cmd.ErrOrStderr())
	if err != nil {
		return nil, fmt.Errorf("read value: %w", err)
	}
	return value, nil
}

func withSecretReadStore(cmd *cobra.Command, flags *secretFlags, withIdentity bool, fn func(*tacos.SecretStore) error) error {
	opts, err := resolveSecretOptions(cmd, flags, false, withIdentity)
	if err != nil {
		return err
	}
	if opts.dbPath == "" {
		return errors.New("database path is required: set --db, MGCL_DB, or db in .mgcl.yaml")
	}
	var identity *tacos.Identity
	if withIdentity {
		identity, err = loadEncryptionIdentity(opts.keyFile)
		if err != nil {
			return fmt.Errorf("load encryption key: %w", err)
		}
	}
	store, err := tacos.OpenSecretStoreReadOnly(opts.dbPath, identity)
	if err != nil {
		if errors.Is(err, tacos.ErrSecretDatabaseNotFound) {
			return fmt.Errorf("%w: %s", err, opts.dbPath)
		}
		return err
	}
	fnErr := fn(store)
	closeErr := store.Close()
	if closeErr != nil {
		closeErr = fmt.Errorf("close database: %w", closeErr)
	}
	if fnErr != nil {
		if closeErr != nil {
			return errors.Join(fnErr, closeErr)
		}
		return fnErr
	}
	if closeErr != nil {
		return closeErr
	}
	return nil
}

func withSecretWriteStore(cmd *cobra.Command, flags *secretFlags, allowCreate, allowPassphraseTTY bool, fn func(*tacos.SecretStore) (string, error)) error {
	opts, err := resolveSecretOptions(cmd, flags, true, true)
	if err != nil {
		return err
	}
	if opts.dbPath == "" {
		return errors.New("database path is required: set --db, MGCL_DB, or db in .mgcl.yaml")
	}

	var manager *sqlitebackup.Manager
	if len(opts.backupSpecs) > 0 {
		destinations, err := sqlitebackup.ParseDestinations(opts.backupSpecs, opts.backupBase)
		if err != nil {
			return fmt.Errorf("backup destination: %w", err)
		}
		manager, err = sqlitebackup.New(opts.dbPath, destinations)
		if err != nil {
			return fmt.Errorf("backup destination: %w", err)
		}
	}

	identity, err := loadEncryptionIdentityTTY(opts.keyFile, allowPassphraseTTY)
	if err != nil {
		abortManager(manager)
		return fmt.Errorf("load encryption key: %w", err)
	}
	if !allowCreate {
		if _, statErr := os.Stat(opts.dbPath); statErr != nil {
			abortManager(manager)
			if os.IsNotExist(statErr) {
				return fmt.Errorf("%w: %s", tacos.ErrSecretDatabaseNotFound, opts.dbPath)
			}
			return fmt.Errorf("stat database: %w", statErr)
		}
	}
	store, err := tacos.OpenSecretStore(opts.dbPath, identity, manager)
	if err != nil {
		abortManager(manager)
		return fmt.Errorf("open database: %w", err)
	}

	message, opErr := fn(store)
	closeErr := store.Close()
	if closeErr != nil {
		if opErr != nil {
			return errors.Join(opErr, fmt.Errorf("backup failed: %w", closeErr))
		}
		return fmt.Errorf("backup failed: %w", closeErr)
	}
	if opErr != nil {
		return opErr
	}
	if message != "" {
		fmt.Fprintln(cmd.ErrOrStderr(), message)
	}
	return nil
}

func abortManager(manager *sqlitebackup.Manager) {
	if manager == nil {
		return
	}
	_ = manager.Abort(nil)
}

func resolveSecretOptions(cmd *cobra.Command, flags *secretFlags, withBackup, needIdentity bool) (*secretOptions, error) {
	config, projectRoot, err := newConfig()
	if err != nil {
		return nil, err
	}
	opts := &secretOptions{}

	if cmd.Flags().Changed("db") {
		opts.dbPath = flags.db
	} else if value := os.Getenv("MGCL_DB"); value != "" {
		opts.dbPath = value
	} else if value := config.GetString("db"); value != "" {
		opts.dbPath = resolveConfigDBPath(value, projectRoot)
	}

	if cmd.Flags().Changed("encryption-key-file") {
		opts.keyFile = flags.keyFile
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
			opts.backupSpecs = flags.backupTo
			opts.backupBase = ""
		} else {
			opts.backupSpecs = specs
			opts.backupBase = projectRoot
		}
	}
	return opts, nil
}
