package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/geektr-cloud/openmgcl/internal/sqlitebackup"
	"github.com/geektr-cloud/openmgcl/internal/tacos"
	"github.com/spf13/cobra"
)

// stateDeleteBypassFlag is intentionally long and explicit: pruning or
// deleting state is irreversible, and non-interactive callers must opt in by
// name.
const stateDeleteBypassFlag = "yes-i-know-this-will-permanently-delete-this-state"

type stateFlags struct {
	db         string
	keyFile    string
	backupTo   []string
	outputs    bool
	outputFile string
	keeps      int
	yes        bool
}

func newStateCmd() *cobra.Command {
	flags := &stateFlags{}
	stateCmd := &cobra.Command{
		Use:   "state",
		Short: "Inspect and maintain workspace state in the local database",
		Long: `Inspect and maintain workspace state offline in the same mgcl.db used by mgcl run.

Configuration follows mgcl run: --db (or MGCL_DB, or db in .mgcl.yaml) is
required. --encryption-key-file (or MGCL_ENCRYPTION_KEY_FILE, or
encryption_key_file) defaults to ~/.ssh/id_ed25519. list, stats and info are
metadata-only and do not load the key. export and every write command load it.

export, prune, delete, unlock and rename are read-only until they have finished
their preflight. Write commands then open a write session and honor --backup-to /
backup_to. Stop other mgcl processes that write the same database before a
write command; the backup shutdown is fail-closed.

prune, delete and rename never unlock a workspace. Run "state unlock" first.`,
	}
	stateCmd.PersistentFlags().StringVarP(&flags.db, "db", "d", "", "SQLite database path (required)")
	stateCmd.PersistentFlags().StringVar(&flags.keyFile, "encryption-key-file", "", "SSH Ed25519 private key (default \"~/.ssh/id_ed25519\")")
	stateCmd.PersistentFlags().StringArrayVar(&flags.backupTo, "backup-to", nil, "Backup destination for write commands; repeatable (sqlite:<path> or path:<path>)")

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List workspaces with version counts",
		Long:  "List every workspace, including workspaces without any state version. This command does not load the encryption key or create a missing database.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStateReadStore(cmd, flags, false, func(store *tacos.Store) error {
				summaries, err := store.ListStateSummaries()
				if err != nil {
					return err
				}
				return writeStateSummaries(cmd.OutOrStdout(), summaries, false)
			})
		},
	}

	statsCmd := &cobra.Command{
		Use:   "stats",
		Short: "List workspaces with version counts and encrypted size",
		Long:  "List every workspace with its encrypted state blob size. This command does not load the encryption key or create a missing database.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStateReadStore(cmd, flags, false, func(store *tacos.Store) error {
				summaries, err := store.ListStateSummaries()
				if err != nil {
					return err
				}
				return writeStateSummaries(cmd.OutOrStdout(), summaries, true)
			})
		},
	}

	infoCmd := &cobra.Command{
		Use:   "info <organization>/<workspace>",
		Short: "List state versions for one workspace",
		Long:  "List state versions newest first. An existing workspace without versions prints only the table header. This command does not load the encryption key or create a missing database.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			org, workspace, err := tacos.ParseStateReference(args[0])
			if err != nil {
				return err
			}
			return withStateReadStore(cmd, flags, false, func(store *tacos.Store) error {
				versions, err := store.ListStateVersionInfo(org, workspace)
				if err != nil {
					return err
				}
				return writeStateVersions(cmd.OutOrStdout(), versions)
			})
		},
	}

	exportCmd := &cobra.Command{
		Use:   "export <organization>/<workspace>",
		Short: "Export the current decrypted JSON state",
		Long: `Export the current decrypted JSON state payload to stdout.

By default the payload is json_state; --outputs selects json_state_outputs.
Only the raw payload is written to stdout, without diagnostics or a trailing
newline. This command loads the encryption key and never creates a missing
database.

With -f/--file the payload is written to a new file with mode 0600. An existing
file is refused. The file is written to a temporary file in the same directory,
synced and closed, then published without overwriting. Failures remove the
temporary file and never change an existing file's mode or ACL.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			org, workspace, err := tacos.ParseStateReference(args[0])
			if err != nil {
				return err
			}
			if cmd.Flags().Changed("file") && flags.outputFile == "" {
				return errors.New("--file path must not be empty")
			}
			return withStateReadStore(cmd, flags, true, func(store *tacos.Store) error {
				data, err := store.ExportCurrentState(org, workspace, flags.outputs)
				if err != nil {
					return err
				}
				if !cmd.Flags().Changed("file") {
					_, err = cmd.OutOrStdout().Write(data)
					return err
				}
				return writeStateExportFile(cmd.ErrOrStderr(), flags.outputFile, data)
			})
		},
	}
	exportCmd.Flags().BoolVar(&flags.outputs, "outputs", false, "Export json_state_outputs instead of json_state")
	exportCmd.Flags().StringVarP(&flags.outputFile, "file", "f", "", "Write the payload to a new file instead of stdout")

	pruneCmd := &cobra.Command{
		Use:   "prune <organization>/<workspace>",
		Short: "Delete old finalized state versions",
		Long: `Delete old finalized state versions while keeping the newest --keeps versions.

The command first plans from a read-only snapshot and fails if the workspace is
locked. On a TTY, confirmation requires typing the full <organization>/<workspace>
reference; y/yes is not accepted. Non-interactive stdin requires
--yes-i-know-this-will-permanently-delete-this-state.

Only after confirmation does it open a write session and any backup. The result
is written to stderr. Pending versions are reported but never pruned. This
command never unlocks a workspace.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStatePrune(cmd, flags, args[0])
		},
	}
	pruneCmd.Flags().IntVar(&flags.keeps, "keeps", 0, "Number of newest finalized versions to keep (required, at least 1)")
	pruneCmd.Flags().BoolVar(&flags.yes, stateDeleteBypassFlag, false, "Skip interactive confirmation (permanently deletes state)")

	deleteCmd := &cobra.Command{
		Use:   "delete <organization>/<workspace>",
		Short: "Permanently delete one workspace and its state versions",
		Long: `Permanently delete one workspace and all of its state versions.

Lineage keys are shared and are deliberately preserved. The command first
performs a read-only preflight and fails if the workspace is missing or locked.
On a TTY, confirmation requires typing the full <organization>/<workspace>
reference; y/yes is not accepted. Non-interactive stdin requires
--yes-i-know-this-will-permanently-delete-this-state.

Only after confirmation does it open a write session and any backup. The result
is written to stderr. This command never unlocks a workspace.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStateDelete(cmd, flags, args[0])
		},
	}
	deleteCmd.Flags().BoolVar(&flags.yes, stateDeleteBypassFlag, false, "Skip interactive confirmation (permanently deletes state)")

	unlockCmd := &cobra.Command{
		Use:   "unlock <organization>/<workspace>",
		Short: "Clear a workspace lock after confirmation",
		Long: `Clear a workspace lock after a read-only preflight.

An unlocked workspace is an idempotent success: no write session and no backup
are opened. A locked workspace prints its lock information to stderr and
requires an interactive terminal. Confirmation requires typing the full
<organization>/<workspace> reference; y/yes is not accepted and there is no
non-interactive bypass. Only after confirmation does it open a write session and
any backup. The result is written to stderr.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStateUnlock(cmd, flags, args[0])
		},
	}

	renameCmd := &cobra.Command{
		Use:   "rename <source> <destination>",
		Short: "Rename a workspace and keep its state",
		Long: `Rename one workspace to a new State reference without touching its state.

Both arguments are full <organization>/<workspace> references; the organization
and workspace name may both change. The workspace ID, every finalized and
pending version, version IDs, created_at, and the encrypted state blobs are
preserved. The source must exist, must not be locked, and the destination must
not already exist; the destination is never overwritten. Renaming to the same
reference is a conflict.

This is a non-destructive write session: it needs no confirmation and works on
non-TTY stdin. It reuses --backup-to / backup_to, and the result is written to
stderr. A locked source must be unlocked first with "state unlock".`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStateRename(cmd, flags, args[0], args[1])
		},
	}

	stateCmd.AddCommand(listCmd, statsCmd, infoCmd, exportCmd, pruneCmd, deleteCmd, unlockCmd, renameCmd)
	return stateCmd
}

func runStatePrune(cmd *cobra.Command, flags *stateFlags, ref string) error {
	org, workspace, err := tacos.ParseStateReference(ref)
	if err != nil {
		return err
	}
	if !cmd.Flags().Changed("keeps") {
		return errors.New("--keeps is required")
	}
	if flags.keeps < 1 {
		return fmt.Errorf("--keeps must be at least 1, got %d", flags.keeps)
	}

	var plan *tacos.StatePrunePlan
	err = withStateReadStore(cmd, flags, false, func(store *tacos.Store) error {
		ws, err := store.GetStateWorkspace(org, workspace)
		if err != nil {
			return err
		}
		if ws.Locked {
			return stateLockedError(ref)
		}
		plan, err = store.PlanStatePrune(org, workspace, flags.keeps)
		return err
	})
	if err != nil {
		return err
	}

	printStatePrunePlan(cmd, ref, plan)
	if !flags.yes {
		if !stdinIsTerminal() {
			return fmt.Errorf("pruning state on non-TTY stdin requires --%s", stateDeleteBypassFlag)
		}
		confirmed, err := confirmTerminalExact(cmd.ErrOrStderr(), fmt.Sprintf("Permanently prune %s.", ref), ref)
		if err != nil {
			return err
		}
		if !confirmed {
			fmt.Fprintln(cmd.ErrOrStderr(), "Aborted.")
			return nil
		}
	}

	return withStateWriteStore(cmd, flags, func(store *tacos.Store) (string, error) {
		result, err := store.PruneState(org, workspace, flags.keeps)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Pruned %s: deleted %d finalized version(s), kept %d, pending %d.",
			ref, result.DeleteFinalized, result.KeepFinalized, result.Pending), nil
	})
}

func runStateDelete(cmd *cobra.Command, flags *stateFlags, ref string) error {
	org, workspace, err := tacos.ParseStateReference(ref)
	if err != nil {
		return err
	}

	err = withStateReadStore(cmd, flags, false, func(store *tacos.Store) error {
		ws, err := store.GetStateWorkspace(org, workspace)
		if err != nil {
			return err
		}
		if ws.Locked {
			return stateLockedError(ref)
		}
		return nil
	})
	if err != nil {
		return err
	}

	fmt.Fprintf(cmd.ErrOrStderr(), "This will permanently delete workspace %s and all of its state versions.\n", ref)
	if !flags.yes {
		if !stdinIsTerminal() {
			return fmt.Errorf("deleting state on non-TTY stdin requires --%s", stateDeleteBypassFlag)
		}
		confirmed, err := confirmTerminalExact(cmd.ErrOrStderr(), fmt.Sprintf("Permanently delete %s.", ref), ref)
		if err != nil {
			return err
		}
		if !confirmed {
			fmt.Fprintln(cmd.ErrOrStderr(), "Aborted.")
			return nil
		}
	}

	return withStateWriteStore(cmd, flags, func(store *tacos.Store) (string, error) {
		if err := store.DeleteState(org, workspace); err != nil {
			return "", err
		}
		return fmt.Sprintf("Deleted state for %s and all of its versions.", ref), nil
	})
}

func runStateUnlock(cmd *cobra.Command, flags *stateFlags, ref string) error {
	org, workspace, err := tacos.ParseStateReference(ref)
	if err != nil {
		return err
	}

	var (
		wsID     string
		lockInfo string
	)
	err = withStateReadStore(cmd, flags, false, func(store *tacos.Store) error {
		ws, err := store.GetStateWorkspace(org, workspace)
		if err != nil {
			return err
		}
		if !ws.Locked {
			fmt.Fprintf(cmd.ErrOrStderr(), "Workspace %s is not locked.\n", ref)
			return nil
		}
		wsID = ws.ID
		lockInfo = ws.LockInfo
		return nil
	})
	if err != nil {
		return err
	}
	if wsID == "" {
		return nil
	}

	fmt.Fprintf(cmd.ErrOrStderr(), "Workspace %s is locked.\n", ref)
	if lockInfo != "" {
		fmt.Fprintf(cmd.ErrOrStderr(), "Lock info:\n%s\n", lockInfo)
	}
	if !stdinIsTerminal() {
		return errors.New("unlocking a workspace requires an interactive terminal")
	}
	confirmed, err := confirmTerminalExact(cmd.ErrOrStderr(), fmt.Sprintf("Unlock %s.", ref), ref)
	if err != nil {
		return err
	}
	if !confirmed {
		fmt.Fprintln(cmd.ErrOrStderr(), "Aborted.")
		return nil
	}

	return withStateWriteStore(cmd, flags, func(store *tacos.Store) (string, error) {
		if err := store.UnlockWorkspace(wsID); err != nil {
			return "", err
		}
		return fmt.Sprintf("Unlocked workspace %s.", ref), nil
	})
}

func runStateRename(cmd *cobra.Command, flags *stateFlags, srcRef, destRef string) error {
	srcOrg, srcWorkspace, err := tacos.ParseStateReference(srcRef)
	if err != nil {
		return err
	}
	destOrg, destWorkspace, err := tacos.ParseStateReference(destRef)
	if err != nil {
		return err
	}

	err = withStateReadStore(cmd, flags, false, func(store *tacos.Store) error {
		ws, err := store.GetStateWorkspace(srcOrg, srcWorkspace)
		if err != nil {
			return err
		}
		if ws.Locked {
			return stateLockedError(srcRef)
		}
		return nil
	})
	if err != nil {
		return err
	}

	return withStateWriteStore(cmd, flags, func(store *tacos.Store) (string, error) {
		if err := store.RenameState(srcOrg, srcWorkspace, destOrg, destWorkspace); err != nil {
			return "", err
		}
		return fmt.Sprintf("Renamed state from %s to %s.", srcRef, destRef), nil
	})
}

func printStatePrunePlan(cmd *cobra.Command, ref string, plan *tacos.StatePrunePlan) {
	fmt.Fprintf(cmd.ErrOrStderr(), "Prune plan for %s:\n", ref)
	fmt.Fprintf(cmd.ErrOrStderr(), "  finalized: %d total, %d delete, %d keep\n",
		plan.TotalFinalized, plan.DeleteFinalized, plan.KeepFinalized)
	fmt.Fprintf(cmd.ErrOrStderr(), "  pending: %d (never pruned)\n", plan.Pending)
}

func stateLockedError(ref string) error {
	return fmt.Errorf("workspace %s is locked; run \"mgcl state unlock %s\" first", ref, ref)
}

func withStateReadStore(cmd *cobra.Command, flags *stateFlags, withIdentity bool, fn func(*tacos.Store) error) error {
	opts, err := resolveStateOptions(cmd, flags, false, withIdentity)
	if err != nil {
		return err
	}
	if opts.dbPath == "" {
		return errDatabasePathRequired
	}
	var identity *tacos.Identity
	if withIdentity {
		identity, err = loadEncryptionIdentity(opts.keyFile)
		if err != nil {
			return fmt.Errorf("load encryption key: %w", err)
		}
	}
	store, err := tacos.OpenStateStoreReadOnly(opts.dbPath, identity)
	if err != nil {
		if errors.Is(err, tacos.ErrStateDatabaseNotFound) {
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

func withStateWriteStore(cmd *cobra.Command, flags *stateFlags, fn func(*tacos.Store) (string, error)) error {
	opts, err := resolveStateOptions(cmd, flags, true, true)
	if err != nil {
		return err
	}
	if opts.dbPath == "" {
		return errDatabasePathRequired
	}
	if _, statErr := os.Stat(opts.dbPath); statErr != nil {
		if os.IsNotExist(statErr) {
			return fmt.Errorf("%w: %s", tacos.ErrStateDatabaseNotFound, opts.dbPath)
		}
		return fmt.Errorf("stat database: %w", statErr)
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

	identity, err := loadEncryptionIdentity(opts.keyFile)
	if err != nil {
		abortManager(manager)
		return fmt.Errorf("load encryption key: %w", err)
	}

	store, err := tacos.OpenWithBackup(opts.dbPath, identity, manager)
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

func resolveStateOptions(cmd *cobra.Command, flags *stateFlags, withBackup, needIdentity bool) (*offlineOptions, error) {
	return resolveOfflineOptions(cmd, offlineFlagValues{
		db:       flags.db,
		keyFile:  flags.keyFile,
		backupTo: flags.backupTo,
	}, withBackup, needIdentity)
}

func writeStateExportFile(stderr io.Writer, path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".mgcl-state-export-*")
	if err != nil {
		return fmt.Errorf("create temporary export file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write export file: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync export file: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close export file: %w", err)
	}
	if err = os.Link(tmpName, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("export output already exists: %s", path)
		}
		return fmt.Errorf("publish export output: %w", err)
	}
	if removeErr := os.Remove(tmpName); removeErr != nil {
		// The destination is already published. Reporting failure here would
		// make the user retry an export that actually succeeded, so warn only.
		fmt.Fprintf(stderr, "warning: export published to %s, but temporary file %s could not be removed: %v\n", path, tmpName, removeErr)
	}
	return nil
}

// errDatabasePathRequired is shared by the offline command families.
var errDatabasePathRequired = errors.New("database path is required: set --db, MGCL_DB, or db in .mgcl.yaml")
