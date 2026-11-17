package cmd

import (
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/dustin/go-humanize"
	"github.com/geektr-cloud/openmgcl/internal/tacos"

	_ "modernc.org/sqlite"
)

type stateCLI struct {
	dir string
	db  string
	key string
}

func newStateCLI(t *testing.T) *stateCLI {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("MGCL_DB", "")
	t.Setenv("MGCL_ENCRYPTION_KEY_FILE", "")
	t.Setenv("MGCL_BACKUP_TO", "")
	return &stateCLI{
		dir: dir,
		db:  filepath.Join(dir, "mgcl.db"),
		key: writeTestIdentity(t, nil),
	}
}

func (h *stateCLI) args(rest ...string) []string {
	base := []string{"state"}
	base = append(base, rest...)
	base = append(base, "--db", h.db, "--encryption-key-file", h.key)
	return base
}

func (h *stateCLI) withStore(t *testing.T, fn func(*tacos.Store)) {
	t.Helper()
	identity, err := tacos.LoadIdentity(h.key, nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := tacos.Open(h.db, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	fn(store)
}

type seededWorkspace struct {
	workspace *tacos.Workspace
	finalized []*tacos.StateVersion
	pending   []*tacos.StateVersion
}

func seedStateWorkspace(t *testing.T, store *tacos.Store, org, name string, finalized, pending int) *seededWorkspace {
	t.Helper()
	ws, err := store.CreateWorkspace(org, name, "latest")
	if err != nil {
		t.Fatal(err)
	}
	seeded := &seededWorkspace{workspace: ws}
	lineage := "lineage-" + org + "-" + name
	for i := 1; i <= finalized; i++ {
		sv, err := store.CreateStateVersion(
			ws.ID, i, lineage, "",
			[]byte("raw-state"),
			stateJSONState(i),
			stateJSONOutputs(i),
			false,
		)
		if err != nil {
			t.Fatalf("create finalized version %d: %v", i, err)
		}
		seeded.finalized = append(seeded.finalized, sv)
	}
	for i := 0; i < pending; i++ {
		sv, err := store.CreateStateVersion(
			ws.ID, finalized+i+1, lineage, "",
			nil, nil, nil, false,
		)
		if err != nil {
			t.Fatalf("create pending version %d: %v", finalized+i+1, err)
		}
		seeded.pending = append(seeded.pending, sv)
	}
	return seeded
}

// createLegacyStateDB builds a database whose secret schema predates
// secret_kvs.is_ephemeral. Every state command must fail before confirmation.
func createLegacyStateDB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		`CREATE TABLE secrets (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE secret_kvs (
			secret_id TEXT NOT NULL REFERENCES secrets(id) ON DELETE CASCADE,
			key TEXT NOT NULL,
			suite INTEGER NOT NULL,
			nonce BLOB NOT NULL,
			ciphertext BLOB NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY (secret_id, key)
		)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
}

func stateJSONState(serial int) []byte {
	return []byte(fmt.Sprintf("{\"version\":4,\"serial\":%d}\n", serial))
}

func stateJSONOutputs(serial int) []byte {
	return []byte(fmt.Sprintf("{\"outputs\":{\"serial\":%d}}\n", serial))
}

func stateVersionCount(t *testing.T, h *stateCLI, org, workspace string) int {
	t.Helper()
	store, err := tacos.OpenStateStoreReadOnly(h.db, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	versions, err := store.ListStateVersionInfo(org, workspace)
	if err != nil {
		t.Fatal(err)
	}
	return len(versions)
}

func TestStateCLIList(t *testing.T) {
	h := newStateCLI(t)
	h.withStore(t, func(store *tacos.Store) {
		seedStateWorkspace(t, store, "acme", "dev", 2, 1)
		seedStateWorkspace(t, store, "acme", "empty", 0, 0)
	})

	stdout, _, err := runCLI(t, "", h.args("list")...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "STATE") || !strings.Contains(stdout, "VERSIONS") || !strings.Contains(stdout, "PENDING") {
		t.Fatalf("missing list header: %q", stdout)
	}
	if !strings.Contains(stdout, "acme/empty") {
		t.Fatalf("workspace without versions missing: %q", stdout)
	}
	rows := stateRows(stdout)
	dev, ok := rows["acme/dev"]
	if !ok {
		t.Fatalf("acme/dev missing: %q", stdout)
	}
	if len(dev) != 3 || dev[1] != "2" || dev[2] != "1" {
		t.Fatalf("acme/dev columns = %v, want [STATE VERSIONS PENDING] counts 2/1", dev)
	}
	empty := rows["acme/empty"]
	if len(empty) != 3 || empty[1] != "0" || empty[2] != "0" {
		t.Fatalf("acme/empty columns = %v, want 0/0", empty)
	}
}

func TestStateCLIListDoesNotLoadKey(t *testing.T) {
	h := newStateCLI(t)
	h.withStore(t, func(store *tacos.Store) {
		seedStateWorkspace(t, store, "acme", "dev", 1, 0)
	})

	stdout, _, err := runCLI(t, "", "state", "list", "--db", h.db, "--encryption-key-file", filepath.Join(h.dir, "missing-key"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "acme/dev") {
		t.Fatalf("list failed to use metadata without a key: %q", stdout)
	}
}

func TestStateCLIStats(t *testing.T) {
	h := newStateCLI(t)
	h.withStore(t, func(store *tacos.Store) {
		seedStateWorkspace(t, store, "acme", "dev", 2, 1)
		seedStateWorkspace(t, store, "acme", "empty", 0, 0)
	})

	stdout, _, err := runCLI(t, "", h.args("stats")...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "SIZE") {
		t.Fatalf("missing SIZE header: %q", stdout)
	}

	store, err := tacos.OpenStateStoreReadOnly(h.db, nil)
	if err != nil {
		t.Fatal(err)
	}
	summaries, err := store.ListStateSummaries()
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	rows := stateRows(stdout)
	for _, summary := range summaries {
		ref := summary.Organization + "/" + summary.Name
		row, ok := rows[ref]
		if !ok {
			t.Fatalf("%s missing from stats: %q", ref, stdout)
		}
		if len(row) != 4 {
			t.Fatalf("%s row = %v, want 4 columns", ref, row)
		}
		want := humanize.IBytes(summary.StoredBytes)
		if row[3] != want {
			t.Fatalf("%s SIZE = %q, want %q", ref, row[3], want)
		}
	}
}

func TestStateCLIInfo(t *testing.T) {
	h := newStateCLI(t)
	var seeded *seededWorkspace
	h.withStore(t, func(store *tacos.Store) {
		seeded = seedStateWorkspace(t, store, "acme", "dev", 2, 1)
	})

	stdout, _, err := runCLI(t, "", h.args("info", "acme/dev")...)
	if err != nil {
		t.Fatal(err)
	}
	for _, header := range []string{"ID", "SERIAL", "STATUS", "CURRENT", "CREATED"} {
		if !strings.Contains(stdout, header) {
			t.Fatalf("info missing header %q: %q", header, stdout)
		}
	}

	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 4 {
		t.Fatalf("info rows = %d, want 3:\n%s", len(lines)-1, stdout)
	}
	// Newest rowid first: pending, then finalized serial 2 (current), then serial 1.
	first := strings.Fields(lines[1])
	second := strings.Fields(lines[2])
	third := strings.Fields(lines[3])
	if first[0] != seeded.pending[0].ID || first[2] != "pending" {
		t.Fatalf("first row = %v, want newest pending %s", first, seeded.pending[0].ID)
	}
	if second[0] != seeded.finalized[1].ID || second[2] != "finalized" || second[3] != "true" {
		t.Fatalf("second row = %v, want current finalized %s", second, seeded.finalized[1].ID)
	}
	if third[0] != seeded.finalized[0].ID || third[3] != "false" {
		t.Fatalf("third row = %v, want older finalized %s", third, seeded.finalized[0].ID)
	}
}

func TestStateCLIInfoEmptyWorkspaceOnlyHeader(t *testing.T) {
	h := newStateCLI(t)
	h.withStore(t, func(store *tacos.Store) {
		seedStateWorkspace(t, store, "acme", "empty", 0, 0)
	})

	stdout, _, err := runCLI(t, "", h.args("info", "acme/empty")...)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 1 {
		t.Fatalf("empty workspace info printed %d lines:\n%s", len(lines), stdout)
	}
	if !strings.Contains(lines[0], "ID") {
		t.Fatalf("empty workspace header = %q", lines[0])
	}
}

func TestStateCLIInvalidReference(t *testing.T) {
	h := newStateCLI(t)
	h.withStore(t, func(store *tacos.Store) {
		seedStateWorkspace(t, store, "acme", "dev", 1, 0)
	})
	for _, ref := range []string{"dev", "acme/", "/dev", "acme/dev/extra", ""} {
		if _, _, err := runCLI(t, "", h.args("info", ref)...); err == nil {
			t.Fatalf("info accepted invalid reference %q", ref)
		}
		if _, _, err := runCLI(t, "", h.args("export", ref)...); err == nil {
			t.Fatalf("export accepted invalid reference %q", ref)
		}
	}
}

func TestStateCLIExportStdoutAndOutputs(t *testing.T) {
	h := newStateCLI(t)
	h.withStore(t, func(store *tacos.Store) {
		seedStateWorkspace(t, store, "acme", "dev", 2, 1)
	})

	stdout, stderr, err := runCLI(t, "", h.args("export", "acme/dev")...)
	if err != nil {
		t.Fatal(err)
	}
	if stdout != string(stateJSONState(2)) {
		t.Fatalf("export stdout = %q, want %q", stdout, stateJSONState(2))
	}
	if stderr != "" {
		t.Fatalf("export wrote diagnostics: %q", stderr)
	}

	stdout, _, err = runCLI(t, "", h.args("export", "acme/dev", "--outputs")...)
	if err != nil {
		t.Fatal(err)
	}
	if stdout != string(stateJSONOutputs(2)) {
		t.Fatalf("export --outputs stdout = %q, want %q", stdout, stateJSONOutputs(2))
	}
}

func TestStateCLIExportFile(t *testing.T) {
	h := newStateCLI(t)
	h.withStore(t, func(store *tacos.Store) {
		seedStateWorkspace(t, store, "acme", "dev", 1, 0)
	})

	path := filepath.Join(h.dir, "state.json")
	stdout, stderr, err := runCLI(t, "", h.args("export", "acme/dev", "-f", path)...)
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "" {
		t.Fatalf("export -f wrote to stdout: %q", stdout)
	}
	if stderr != "" {
		t.Fatalf("export -f wrote diagnostics: %q", stderr)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(stateJSONState(1)) {
		t.Fatalf("export file = %q, want %q", data, stateJSONState(1))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("export file mode = %o, want 600", info.Mode().Perm())
	}
}

func TestStateCLIExportFileRefusesExisting(t *testing.T) {
	h := newStateCLI(t)
	h.withStore(t, func(store *tacos.Store) {
		seedStateWorkspace(t, store, "acme", "dev", 1, 0)
	})

	path := filepath.Join(h.dir, "existing.json")
	if err := os.WriteFile(path, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := runCLI(t, "", h.args("export", "acme/dev", "-f", path)...)
	if err == nil {
		t.Fatal("export overwrote an existing file")
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != "keep" {
		t.Fatalf("existing file changed: %q", data)
	}
	info, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("existing file mode changed to %o", info.Mode().Perm())
	}
}

func TestStateCLIExportExplicitEmptyFile(t *testing.T) {
	h := newStateCLI(t)
	h.withStore(t, func(store *tacos.Store) {
		seedStateWorkspace(t, store, "acme", "dev", 1, 0)
	})

	stdout, _, err := runCLI(t, "", h.args("export", "acme/dev", "-f", "")...)
	if err == nil {
		t.Fatal("export with an explicitly empty --file succeeded")
	}
	if !strings.Contains(err.Error(), "--file") {
		t.Fatalf("empty --file error = %v", err)
	}
	if stdout != "" {
		t.Fatalf("export with an empty --file leaked payload to stdout: %q", stdout)
	}

	stdout, _, err = runCLI(t, "", h.args("export", "acme/dev", "--file", "")...)
	if err == nil {
		t.Fatal("export with an explicitly empty --file succeeded")
	}
	if stdout != "" {
		t.Fatalf("export with an empty --file leaked payload to stdout: %q", stdout)
	}
}

func TestStateCLIExportMissing(t *testing.T) {
	h := newStateCLI(t)
	h.withStore(t, func(store *tacos.Store) {
		seedStateWorkspace(t, store, "acme", "dev", 0, 0)
	})
	if _, _, err := runCLI(t, "", h.args("export", "acme/dev")...); err == nil {
		t.Fatal("export from a workspace without a finalized version succeeded")
	}
	if _, _, err := runCLI(t, "", h.args("export", "acme/missing")...); err == nil {
		t.Fatal("export of a missing workspace succeeded")
	}
}

func TestStateCLIPruneConfirmationAndPlan(t *testing.T) {
	h := newStateCLI(t)
	h.withStore(t, func(store *tacos.Store) {
		seedStateWorkspace(t, store, "acme", "dev", 4, 1)
	})

	stubTTY(t, "acme/dev", nil)
	stdout, stderr, err := runCLI(t, "", h.args("prune", "acme/dev", "--keeps", "2")...)
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "" {
		t.Fatalf("prune wrote plan to stdout: %q", stdout)
	}
	for _, want := range []string{"Prune plan for acme/dev", "4 total, 2 delete, 2 keep", "pending: 1", "Pruned acme/dev"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("prune stderr missing %q:\n%s", want, stderr)
		}
	}
	if got := stateVersionCount(t, h, "acme", "dev"); got != 3 {
		t.Fatalf("versions after prune = %d, want 3", got)
	}
}

func TestStateCLIPruneRequiresKeeps(t *testing.T) {
	h := newStateCLI(t)
	h.withStore(t, func(store *tacos.Store) {
		seedStateWorkspace(t, store, "acme", "dev", 2, 0)
	})
	stubNonTTY(t)

	if _, _, err := runCLI(t, "", h.args("prune", "acme/dev")...); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("missing --keeps error = %v", err)
	}
	if _, _, err := runCLI(t, "", h.args("prune", "acme/dev", "--keeps", "0")...); err == nil || !strings.Contains(err.Error(), "at least 1") {
		t.Fatalf("--keeps 0 error = %v", err)
	}
}

func TestStateCLIPruneNonTTYRequiresFlag(t *testing.T) {
	h := newStateCLI(t)
	h.withStore(t, func(store *tacos.Store) {
		seedStateWorkspace(t, store, "acme", "dev", 3, 0)
	})
	stubNonTTY(t)

	_, _, err := runCLI(t, "", h.args("prune", "acme/dev", "--keeps", "1")...)
	if err == nil || !strings.Contains(err.Error(), stateDeleteBypassFlag) {
		t.Fatalf("non-TTY prune error = %v", err)
	}
	if got := stateVersionCount(t, h, "acme", "dev"); got != 3 {
		t.Fatalf("versions after rejected prune = %d, want 3", got)
	}
}

func TestStateCLIPruneWrongConfirmationDoesNotWriteOrBackup(t *testing.T) {
	h := newStateCLI(t)
	h.withStore(t, func(store *tacos.Store) {
		seedStateWorkspace(t, store, "acme", "dev", 3, 0)
	})
	backup := filepath.Join(h.dir, "backup.db")
	stubTTY(t, "yes", nil)

	_, stderr, err := runCLI(t, "", h.args("prune", "acme/dev", "--keeps", "1", "--backup-to", "sqlite:"+backup)...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "Aborted.") {
		t.Fatalf("cancellation message missing:\n%s", stderr)
	}
	if got := stateVersionCount(t, h, "acme", "dev"); got != 3 {
		t.Fatalf("versions after cancelled prune = %d, want 3", got)
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatalf("cancelled prune created a backup: %v", err)
	}
}

func TestStateCLIPruneLongFlagAndBackup(t *testing.T) {
	h := newStateCLI(t)
	h.withStore(t, func(store *tacos.Store) {
		seedStateWorkspace(t, store, "acme", "dev", 3, 0)
	})
	backup := filepath.Join(h.dir, "backup.db")
	stubNonTTY(t)

	_, stderr, err := runCLI(t, "", h.args("prune", "acme/dev", "--keeps", "1", "--backup-to", "sqlite:"+backup, "--"+stateDeleteBypassFlag)...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "Pruned acme/dev") {
		t.Fatalf("prune result missing:\n%s", stderr)
	}
	if got := stateVersionCount(t, h, "acme", "dev"); got != 1 {
		t.Fatalf("versions after prune = %d, want 1", got)
	}
	if _, err := os.Stat(backup); err != nil {
		t.Fatalf("prune backup missing: %v", err)
	}
}

func TestStateCLIPruneBackupFailureDoesNotWrite(t *testing.T) {
	h := newStateCLI(t)
	h.withStore(t, func(store *tacos.Store) {
		seedStateWorkspace(t, store, "acme", "dev", 3, 0)
	})
	stubNonTTY(t)

	_, _, err := runCLI(t, "", h.args("prune", "acme/dev", "--keeps", "1", "--backup-to", "sqlite:"+h.db, "--"+stateDeleteBypassFlag)...)
	if err == nil {
		t.Fatal("prune with an invalid backup destination succeeded")
	}
	if got := stateVersionCount(t, h, "acme", "dev"); got != 3 {
		t.Fatalf("versions after failed backup = %d, want 3", got)
	}
}

func TestStateCLIWriteSessionParsesBackupBeforeIdentity(t *testing.T) {
	h := newStateCLI(t)
	h.withStore(t, func(store *tacos.Store) {
		seedStateWorkspace(t, store, "acme", "dev", 3, 0)
	})
	stubNonTTY(t)
	missingKey := filepath.Join(h.dir, "missing-key")

	// The invalid destination must be reported before the missing key is read.
	_, _, err := runCLI(t, "",
		"state", "prune", "acme/dev", "--keeps", "1", "--"+stateDeleteBypassFlag,
		"--db", h.db, "--encryption-key-file", missingKey,
		"--backup-to", "sqlite:"+h.db,
	)
	if err == nil || !strings.Contains(err.Error(), "backup destination") {
		t.Fatalf("invalid backup destination error = %v, want backup destination before key load", err)
	}

	// A valid destination is prepared, then a key failure aborts the manager
	// without starting a backup or writing.
	backup := filepath.Join(h.dir, "backup.db")
	_, _, err = runCLI(t, "",
		"state", "prune", "acme/dev", "--keeps", "1", "--"+stateDeleteBypassFlag,
		"--db", h.db, "--encryption-key-file", missingKey,
		"--backup-to", "sqlite:"+backup,
	)
	if err == nil || !strings.Contains(err.Error(), "load encryption key") {
		t.Fatalf("missing key error = %v", err)
	}
	if _, statErr := os.Stat(backup); !os.IsNotExist(statErr) {
		t.Fatalf("failed identity load created a backup: %v", statErr)
	}
	if got := stateVersionCount(t, h, "acme", "dev"); got != 3 {
		t.Fatalf("versions after failed identity load = %d, want 3", got)
	}
}

func TestStateCLIPreflightOutdatedSecretSchema(t *testing.T) {
	h := newStateCLI(t)
	createLegacyStateDB(t, h.db)
	stubNonTTY(t)
	backup := filepath.Join(h.dir, "backup.db")

	cases := [][]string{
		{"prune", "acme/dev", "--keeps", "1", "--" + stateDeleteBypassFlag},
		{"delete", "acme/dev", "--" + stateDeleteBypassFlag},
		{"unlock", "acme/dev"},
	}
	for _, caseArgs := range cases {
		args := append([]string{"state"}, caseArgs...)
		args = append(args, "--db", h.db, "--encryption-key-file", h.key, "--backup-to", "sqlite:"+backup)
		_, _, err := runCLI(t, "", args...)
		if err == nil {
			t.Fatalf("state %s accepted a legacy secret schema", caseArgs[0])
		}
		if !strings.Contains(err.Error(), "is_ephemeral") || !strings.Contains(err.Error(), "mgcl-migrate") {
			t.Fatalf("state %s legacy schema error = %v", caseArgs[0], err)
		}
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatalf("legacy-schema preflight created a backup: %v", err)
	}
}

func TestStateCLIPruneLocked(t *testing.T) {
	h := newStateCLI(t)
	h.withStore(t, func(store *tacos.Store) {
		seeded := seedStateWorkspace(t, store, "acme", "dev", 3, 0)
		if err := store.LockWorkspace(seeded.workspace.ID, `{"id":"lock-1"}`); err != nil {
			t.Fatal(err)
		}
	})
	backup := filepath.Join(h.dir, "backup.db")
	stubNonTTY(t)

	_, _, err := runCLI(t, "", h.args("prune", "acme/dev", "--keeps", "1", "--backup-to", "sqlite:"+backup, "--"+stateDeleteBypassFlag)...)
	if err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("locked prune error = %v", err)
	}
	if got := stateVersionCount(t, h, "acme", "dev"); got != 3 {
		t.Fatalf("versions after locked prune = %d, want 3", got)
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatalf("locked prune created a backup: %v", err)
	}
}

func TestStateCLIPruneUnchangedWhenKeepsExceedsVersions(t *testing.T) {
	h := newStateCLI(t)
	h.withStore(t, func(store *tacos.Store) {
		seedStateWorkspace(t, store, "acme", "dev", 2, 1)
	})
	stubNonTTY(t)

	_, stderr, err := runCLI(t, "", h.args("prune", "acme/dev", "--keeps", "9", "--"+stateDeleteBypassFlag)...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "0 delete, 2 keep") {
		t.Fatalf("unexpected plan:\n%s", stderr)
	}
	if got := stateVersionCount(t, h, "acme", "dev"); got != 3 {
		t.Fatalf("versions after no-op prune = %d, want 3", got)
	}
}

func TestStateCLIDeleteConfirmation(t *testing.T) {
	h := newStateCLI(t)
	h.withStore(t, func(store *tacos.Store) {
		seedStateWorkspace(t, store, "acme", "dev", 1, 1)
		seedStateWorkspace(t, store, "acme", "keep", 1, 0)
	})
	stubTTY(t, "acme/dev", nil)

	stdout, stderr, err := runCLI(t, "", h.args("delete", "acme/dev")...)
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "" {
		t.Fatalf("delete wrote to stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "permanently delete workspace acme/dev") || !strings.Contains(stderr, "Deleted state for acme/dev") {
		t.Fatalf("delete diagnostics missing:\n%s", stderr)
	}
	if _, _, err := runCLI(t, "", h.args("info", "acme/dev")...); err == nil {
		t.Fatal("deleted workspace still exists")
	}
	if _, _, err := runCLI(t, "", h.args("info", "acme/keep")...); err != nil {
		t.Fatalf("delete removed another workspace: %v", err)
	}
}

func TestStateCLIDeleteNonTTYRequiresFlag(t *testing.T) {
	h := newStateCLI(t)
	h.withStore(t, func(store *tacos.Store) {
		seedStateWorkspace(t, store, "acme", "dev", 1, 0)
	})
	stubNonTTY(t)

	_, _, err := runCLI(t, "", h.args("delete", "acme/dev")...)
	if err == nil || !strings.Contains(err.Error(), stateDeleteBypassFlag) {
		t.Fatalf("non-TTY delete error = %v", err)
	}
	if _, _, err := runCLI(t, "", h.args("info", "acme/dev")...); err != nil {
		t.Fatalf("workspace disappeared after rejected delete: %v", err)
	}
}

func TestStateCLIDeleteWrongConfirmationDoesNotWrite(t *testing.T) {
	h := newStateCLI(t)
	h.withStore(t, func(store *tacos.Store) {
		seedStateWorkspace(t, store, "acme", "dev", 1, 0)
	})
	backup := filepath.Join(h.dir, "backup.db")
	stubTTY(t, "acme", nil)

	_, stderr, err := runCLI(t, "", h.args("delete", "acme/dev", "--backup-to", "sqlite:"+backup)...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "Aborted.") {
		t.Fatalf("cancellation message missing:\n%s", stderr)
	}
	if _, _, err := runCLI(t, "", h.args("info", "acme/dev")...); err != nil {
		t.Fatalf("workspace disappeared after cancelled delete: %v", err)
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatalf("cancelled delete created a backup: %v", err)
	}
}

func TestStateCLIDeleteLongFlagAndBackup(t *testing.T) {
	h := newStateCLI(t)
	h.withStore(t, func(store *tacos.Store) {
		seedStateWorkspace(t, store, "acme", "dev", 2, 1)
	})
	backup := filepath.Join(h.dir, "backup.db")
	stubNonTTY(t)

	_, stderr, err := runCLI(t, "", h.args("delete", "acme/dev", "--backup-to", "sqlite:"+backup, "--"+stateDeleteBypassFlag)...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "Deleted state for acme/dev") {
		t.Fatalf("delete result missing:\n%s", stderr)
	}
	if _, _, err := runCLI(t, "", h.args("info", "acme/dev")...); err == nil {
		t.Fatal("deleted workspace still exists")
	}
	if _, err := os.Stat(backup); err != nil {
		t.Fatalf("delete backup missing: %v", err)
	}
}

func TestStateCLIDeleteLocked(t *testing.T) {
	h := newStateCLI(t)
	h.withStore(t, func(store *tacos.Store) {
		seeded := seedStateWorkspace(t, store, "acme", "dev", 1, 0)
		if err := store.LockWorkspace(seeded.workspace.ID, `{"id":"lock-1"}`); err != nil {
			t.Fatal(err)
		}
	})
	stubNonTTY(t)

	_, _, err := runCLI(t, "", h.args("delete", "acme/dev", "--"+stateDeleteBypassFlag)...)
	if err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("locked delete error = %v", err)
	}
	if _, _, err := runCLI(t, "", h.args("info", "acme/dev")...); err != nil {
		t.Fatalf("locked workspace disappeared: %v", err)
	}
}

func TestStateCLIDeleteMissing(t *testing.T) {
	h := newStateCLI(t)
	h.withStore(t, func(store *tacos.Store) {
		seedStateWorkspace(t, store, "acme", "dev", 1, 0)
	})
	stubNonTTY(t)

	if _, _, err := runCLI(t, "", h.args("delete", "acme/missing", "--"+stateDeleteBypassFlag)...); err == nil {
		t.Fatal("deleting a missing workspace succeeded")
	}
}

func TestStateCLIUnlockNoopDoesNotWriteOrBackup(t *testing.T) {
	h := newStateCLI(t)
	h.withStore(t, func(store *tacos.Store) {
		seedStateWorkspace(t, store, "acme", "dev", 1, 0)
	})
	stubNonTTY(t)

	// A syntactically valid but self-referential backup destination would fail
	// if a write session were opened. Success proves no backup was started.
	_, stderr, err := runCLI(t, "", h.args("unlock", "acme/dev", "--backup-to", "sqlite:"+h.db)...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "not locked") {
		t.Fatalf("unlock no-op message missing:\n%s", stderr)
	}
}

func TestStateCLIUnlockNonTTYLockedFails(t *testing.T) {
	h := newStateCLI(t)
	h.withStore(t, func(store *tacos.Store) {
		seeded := seedStateWorkspace(t, store, "acme", "dev", 1, 0)
		if err := store.LockWorkspace(seeded.workspace.ID, `{"id":"lock-1"}`); err != nil {
			t.Fatal(err)
		}
	})
	backup := filepath.Join(h.dir, "backup.db")
	stubNonTTY(t)

	_, stderr, err := runCLI(t, "", h.args("unlock", "acme/dev", "--backup-to", "sqlite:"+backup)...)
	if err == nil || !strings.Contains(err.Error(), "interactive terminal") {
		t.Fatalf("non-TTY unlock error = %v", err)
	}
	if !strings.Contains(stderr, "lock-1") {
		t.Fatalf("lock info missing from stderr:\n%s", stderr)
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatalf("failed unlock created a backup: %v", err)
	}
}

func TestStateCLIUnlockWrongConfirmation(t *testing.T) {
	h := newStateCLI(t)
	h.withStore(t, func(store *tacos.Store) {
		seeded := seedStateWorkspace(t, store, "acme", "dev", 1, 0)
		if err := store.LockWorkspace(seeded.workspace.ID, `{"id":"lock-1"}`); err != nil {
			t.Fatal(err)
		}
	})
	stubTTY(t, "y", nil)

	_, stderr, err := runCLI(t, "", h.args("unlock", "acme/dev")...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "Aborted.") {
		t.Fatalf("cancellation message missing:\n%s", stderr)
	}
	h.withStore(t, func(store *tacos.Store) {
		ws, err := store.GetWorkspace("acme", "dev")
		if err != nil {
			t.Fatal(err)
		}
		if !ws.Locked {
			t.Fatal("cancelled unlock cleared the lock")
		}
	})
}

func TestStateCLIUnlockConfirmationAndBackup(t *testing.T) {
	h := newStateCLI(t)
	h.withStore(t, func(store *tacos.Store) {
		seeded := seedStateWorkspace(t, store, "acme", "dev", 1, 0)
		if err := store.LockWorkspace(seeded.workspace.ID, `{"id":"lock-1","who":"alice"}`); err != nil {
			t.Fatal(err)
		}
	})
	backup := filepath.Join(h.dir, "backup.db")
	stubTTY(t, "acme/dev", nil)

	_, stderr, err := runCLI(t, "", h.args("unlock", "acme/dev", "--backup-to", "sqlite:"+backup)...)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Workspace acme/dev is locked", "lock-1", "Unlocked workspace acme/dev"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("unlock stderr missing %q:\n%s", want, stderr)
		}
	}
	h.withStore(t, func(store *tacos.Store) {
		ws, err := store.GetWorkspace("acme", "dev")
		if err != nil {
			t.Fatal(err)
		}
		if ws.Locked {
			t.Fatal("unlock left the workspace locked")
		}
	})
	if _, err := os.Stat(backup); err != nil {
		t.Fatalf("unlock backup missing: %v", err)
	}
}

func TestStateCLIRename(t *testing.T) {
	h := newStateCLI(t)
	var seeded *seededWorkspace
	h.withStore(t, func(store *tacos.Store) {
		seeded = seedStateWorkspace(t, store, "acme", "dev", 2, 1)
	})
	stubNonTTY(t)

	stdout, stderr, err := runCLI(t, "", h.args("rename", "acme/dev", "acme-prod/dev-renamed")...)
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "" {
		t.Fatalf("rename wrote to stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "Renamed state from acme/dev to acme-prod/dev-renamed.") {
		t.Fatalf("rename result missing:\n%s", stderr)
	}

	if _, _, err := runCLI(t, "", h.args("info", "acme/dev")...); err == nil {
		t.Fatal("old reference still exists after rename")
	}
	stdout, _, err = runCLI(t, "", h.args("info", "acme-prod/dev-renamed")...)
	if err != nil {
		t.Fatalf("info on new reference: %v", err)
	}
	for _, id := range []string{seeded.finalized[0].ID, seeded.finalized[1].ID, seeded.pending[0].ID} {
		if !strings.Contains(stdout, id) {
			t.Fatalf("version %s missing after rename:\n%s", id, stdout)
		}
	}

	h.withStore(t, func(store *tacos.Store) {
		got, err := store.GetWorkspace("acme-prod", "dev-renamed")
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != seeded.workspace.ID {
			t.Fatalf("workspace ID = %q, want preserved %q", got.ID, seeded.workspace.ID)
		}
		if got.CreatedAt != seeded.workspace.CreatedAt {
			t.Fatalf("created_at = %q, want preserved %q", got.CreatedAt, seeded.workspace.CreatedAt)
		}
	})

	stdout, _, err = runCLI(t, "", h.args("export", "acme-prod/dev-renamed")...)
	if err != nil {
		t.Fatal(err)
	}
	if stdout != string(stateJSONState(2)) {
		t.Fatalf("export after rename = %q, want %q", stdout, stateJSONState(2))
	}

	stdout, _, err = runCLI(t, "", h.args("list")...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "acme-prod/dev-renamed") || strings.Contains(stdout, "acme/dev") {
		t.Fatalf("list after rename:\n%s", stdout)
	}
}

func TestStateCLIRenameValidatesReferencesBeforeDatabase(t *testing.T) {
	h := newStateCLI(t)
	stubNonTTY(t)

	cases := [][2]string{
		{"acme", "acme/dev"},
		{"acme/dev", "acme"},
		{"acme/a b", "acme/dev"},
		{"acme/dev", "acme/a b"},
		{"acme/a%b", "acme/dev"},
		{"acme/dev", "acme/a/b"},
		{"acme/dev", "acme/a%2Fb"},
		{"", "acme/dev"},
	}
	for _, tc := range cases {
		_, _, err := runCLI(t, "", h.args("rename", tc[0], tc[1])...)
		if err == nil || !strings.Contains(err.Error(), "state reference") {
			t.Fatalf("rename %q -> %q error = %v, want a state reference error", tc[0], tc[1], err)
		}
	}
	if _, err := os.Stat(h.db); !os.IsNotExist(err) {
		t.Fatalf("invalid rename opened or created the database: %v", err)
	}
}

func TestStateCLIRenameRejectsWithoutChanges(t *testing.T) {
	h := newStateCLI(t)
	var dev, keep, locked *seededWorkspace
	h.withStore(t, func(store *tacos.Store) {
		dev = seedStateWorkspace(t, store, "acme", "dev", 1, 0)
		keep = seedStateWorkspace(t, store, "acme", "keep", 1, 0)
		locked = seedStateWorkspace(t, store, "acme", "locked", 1, 0)
		if err := store.LockWorkspace(locked.workspace.ID, `{"id":"lock-1"}`); err != nil {
			t.Fatal(err)
		}
	})
	stubNonTTY(t)

	if _, _, err := runCLI(t, "", h.args("rename", "acme/dev", "acme/dev")...); err == nil || !strings.Contains(err.Error(), "same workspace") {
		t.Fatalf("same-reference rename error = %v", err)
	}
	if _, _, err := runCLI(t, "", h.args("rename", "acme/dev", "acme/keep")...); err == nil {
		t.Fatal("rename onto an existing workspace succeeded")
	}
	if _, _, err := runCLI(t, "", h.args("rename", "acme/missing", "acme/new")...); err == nil {
		t.Fatal("rename from a missing workspace succeeded")
	}
	if _, _, err := runCLI(t, "", h.args("rename", "acme/locked", "acme/moved")...); err == nil ||
		!strings.Contains(err.Error(), "locked") || !strings.Contains(err.Error(), "mgcl state unlock acme/locked") {
		t.Fatalf("locked rename error = %v", err)
	}

	h.withStore(t, func(store *tacos.Store) {
		for _, tc := range []struct {
			ref string
			id  string
		}{
			{"acme/dev", dev.workspace.ID},
			{"acme/keep", keep.workspace.ID},
			{"acme/locked", locked.workspace.ID},
		} {
			org, name, err := tacos.ParseStateReference(tc.ref)
			if err != nil {
				t.Fatal(err)
			}
			got, err := store.GetWorkspace(org, name)
			if err != nil {
				t.Fatalf("%s disappeared after rejected rename: %v", tc.ref, err)
			}
			if got.ID != tc.id {
				t.Fatalf("%s ID = %q, want %q", tc.ref, got.ID, tc.id)
			}
		}
		for _, ref := range []string{"acme/new", "acme/moved"} {
			if _, err := store.GetWorkspace("acme", strings.TrimPrefix(ref, "acme/")); !tacos.IsAPIError(err, http.StatusNotFound) {
				t.Fatalf("%s was created by a rejected rename: %v", ref, err)
			}
		}
	})
}

func TestStateCLIRenameBackupFailureDoesNotRename(t *testing.T) {
	h := newStateCLI(t)
	h.withStore(t, func(store *tacos.Store) {
		seedStateWorkspace(t, store, "acme", "dev", 1, 0)
	})
	stubNonTTY(t)

	_, _, err := runCLI(t, "", h.args("rename", "acme/dev", "acme/moved", "--backup-to", "sqlite:"+h.db)...)
	if err == nil {
		t.Fatal("rename with an invalid backup destination succeeded")
	}
	if _, _, err := runCLI(t, "", h.args("info", "acme/dev")...); err != nil {
		t.Fatalf("source disappeared after failed backup: %v", err)
	}
	if _, _, err := runCLI(t, "", h.args("info", "acme/moved")...); err == nil {
		t.Fatal("destination exists after failed backup")
	}
}

func TestStateCLIListMissingDatabase(t *testing.T) {
	h := newStateCLI(t)
	stdout, _, err := runCLI(t, "", h.args("list")...)
	if err == nil {
		t.Fatal("list on a missing database succeeded")
	}
	if stdout != "" {
		t.Fatalf("list on a missing database wrote to stdout: %q", stdout)
	}
	if _, statErr := os.Stat(h.db); !os.IsNotExist(statErr) {
		t.Fatalf("read-only list created the database: %v", statErr)
	}
}

func TestStateCLIHelp(t *testing.T) {
	stdout, _, err := runCLI(t, "", "state", "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"list", "stats", "info", "export", "prune", "delete", "unlock", "rename", "--db", "--encryption-key-file", "--backup-to"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("state --help missing %q", want)
		}
	}
}

var stateColumnSplit = regexp.MustCompile(`\s{2,}`)

func stateRows(out string) map[string][]string {
	rows := make(map[string][]string)
	for i, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if i == 0 {
			continue
		}
		fields := stateColumnSplit.Split(strings.TrimSpace(line), -1)
		if len(fields) == 0 || fields[0] == "" {
			continue
		}
		rows[fields[0]] = fields
	}
	return rows
}
