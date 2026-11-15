package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/geektr-cloud/openmgcl/internal/sqlitebackup"
	"github.com/geektr-cloud/openmgcl/internal/tacos"
)

type secretCLI struct {
	dir string
	db  string
	key string
}

func newSecretCLI(t *testing.T) *secretCLI {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("MGCL_DB", "")
	t.Setenv("MGCL_ENCRYPTION_KEY_FILE", "")
	t.Setenv("MGCL_BACKUP_TO", "")
	return &secretCLI{
		dir: dir,
		db:  filepath.Join(dir, "mgcl.db"),
		key: writeTestIdentity(t, nil),
	}
}

func stubNonTTY(t *testing.T) {
	t.Helper()
	old := stdinIsTerminal
	stdinIsTerminal = func() bool { return false }
	t.Cleanup(func() { stdinIsTerminal = old })
}

func stubTTY(t *testing.T, line string, password []byte) {
	t.Helper()
	oldTerm := stdinIsTerminal
	oldLine := readTerminalLine
	oldPassword := readTerminalPassword
	stdinIsTerminal = func() bool { return true }
	readTerminalLine = func() (string, error) { return line, nil }
	readTerminalPassword = func(prompt string, stderr io.Writer) ([]byte, error) {
		io.WriteString(stderr, prompt)
		return password, nil
	}
	t.Cleanup(func() {
		stdinIsTerminal = oldTerm
		readTerminalLine = oldLine
		readTerminalPassword = oldPassword
	})
}

func runCLI(t *testing.T, stdin string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := newRootCmd()
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(args)
	err = root.Execute()
	return out.String(), errBuf.String(), err
}

func (h *secretCLI) args(rest ...string) []string {
	base := []string{"secret"}
	base = append(base, rest...)
	base = append(base, "--db", h.db, "--encryption-key-file", h.key)
	return base
}

func TestSecretCLISetStdinLimit(t *testing.T) {
	h := newSecretCLI(t)
	stubNonTTY(t)
	if _, _, err := runCLI(t, "", h.args("create", "limits")...); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, "", h.args("set", "limits/key", "old")...); err != nil {
		t.Fatal(err)
	}
	tooLarge := strings.Repeat("x", tacos.MaxSecretValueBytes+1)
	_, _, err := runCLI(t, tooLarge, h.args("set", "limits/key", "--stdin")...)
	if err == nil {
		t.Fatal("oversized --stdin value was accepted")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("unexpected error: %v", err)
	}
	stdout, _, err := runCLI(t, "", h.args("get", "limits/key")...)
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "old" {
		t.Fatalf("value changed after oversized --stdin: %q", stdout)
	}
}

func TestSecretCLIHelp(t *testing.T) {
	stdout, _, err := runCLI(t, "", "secret", "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"create", "list", "show", "set", "get", "unset", "delete", "--db", "--encryption-key-file", "--backup-to"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("secret --help missing %q", want)
		}
	}
	stdout, _, err = runCLI(t, "", "secret", "set", "--help")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "--stdin") || !strings.Contains(stdout, "shell history") {
		t.Errorf("secret set --help lacks stdin/history warning:\n%s", stdout)
	}
	if !strings.Contains(stdout, "--public") {
		t.Errorf("secret set --help lacks --public:\n%s", stdout)
	}
	stdout, _, err = runCLI(t, "", "secret", "delete", "--help")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "--yes") {
		t.Errorf("secret delete --help lacks --yes:\n%s", stdout)
	}
	stdout, _, err = runCLI(t, "", "run", "--help")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "--yes-i-know-it-will-publish-state-and-secrets") {
		t.Errorf("run --help lacks the publish flag:\n%s", stdout)
	}
}

func TestSecretCLISetPublic(t *testing.T) {
	h := newSecretCLI(t)
	stubNonTTY(t)

	// create has no --public flag.
	if _, _, err := runCLI(t, "", h.args("create", "rejected", "--public")...); err == nil {
		t.Fatal("create accepted --public")
	}
	if _, _, err := runCLI(t, "", h.args("create", "bootstrap")...); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, "", h.args("set", "bootstrap/key", "before")...); err != nil {
		t.Fatal(err)
	}

	showJSON := func(name string) map[string]any {
		t.Helper()
		stdout, _, err := runCLI(t, "", h.args("show", name, "-o", "json")...)
		if err != nil {
			t.Fatal(err)
		}
		var view map[string]any
		if err := json.Unmarshal([]byte(stdout), &view); err != nil {
			t.Fatal(err)
		}
		return view
	}
	keyMark := func(view map[string]any, key string) bool {
		t.Helper()
		keys, _ := view["keys"].([]any)
		for _, item := range keys {
			entry, _ := item.(map[string]any)
			if entry["key"] == key {
				mark, ok := entry["is_ephemeral"].(bool)
				if !ok {
					t.Fatalf("key %q lacks boolean is_ephemeral: %#v", key, entry)
				}
				return mark
			}
		}
		t.Fatalf("key %q not found in %#v", key, view["keys"])
		return false
	}
	if view := showJSON("bootstrap"); !keyMark(view, "key") {
		t.Fatalf("new KV is_ephemeral = %#v", view["keys"])
	}
	if _, exists := showJSON("bootstrap")["is_ephemeral"]; exists {
		t.Fatal("Secret-level is_ephemeral is still present")
	}
	stdout, _, err := runCLI(t, "", h.args("list")...)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout, "ACCESS") {
		t.Fatalf("Secret list still shows a Secret-level access column:\n%s", stdout)
	}

	// A second KV stays ephemeral when only the first is marked public.
	if _, _, err := runCLI(t, "", h.args("set", "bootstrap/other", "other-value")...); err != nil {
		t.Fatal(err)
	}

	sqliteBackup := filepath.Join(h.dir, "public-sqlite.db")
	pathBackup := filepath.Join(h.dir, "public-path.db")
	args := append(h.args("set", "bootstrap/key", "after", "--public"),
		"--backup-to", "sqlite:"+sqliteBackup,
		"--backup-to", "path:"+pathBackup,
	)
	_, stderr, err := runCLI(t, "", args...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "Set bootstrap/key.") {
		t.Fatalf("public set stderr lacks Set message: %q", stderr)
	}
	if !strings.Contains(stderr, "KV bootstrap/key is public.") {
		t.Fatalf("public set stderr lacks KV public message: %q", stderr)
	}

	stdout, _, err = runCLI(t, "", h.args("get", "bootstrap/key")...)
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "after" {
		t.Fatalf("get after --public = %q", stdout)
	}
	view := showJSON("bootstrap")
	if keyMark(view, "key") {
		t.Fatal("public KV is still marked ephemeral")
	}
	if !keyMark(view, "other") {
		t.Fatal("--public changed another KV")
	}
	badKey := filepath.Join(h.dir, "missing-key")
	if _, _, err := runCLI(t, "", "secret", "list", "--db", h.db, "--encryption-key-file", badKey); err != nil {
		t.Fatalf("public list loaded the key: %v", err)
	}
	if _, _, err := runCLI(t, "", "secret", "show", "bootstrap", "--db", h.db, "--encryption-key-file", badKey); err != nil {
		t.Fatalf("public show loaded the key: %v", err)
	}
	stdout, _, err = runCLI(t, "", h.args("show", "bootstrap")...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "ACCESS") || !strings.Contains(stdout, "public") || !strings.Contains(stdout, "ephemeral") {
		t.Fatalf("show table lacks per-KV access:\n%s", stdout)
	}
	stdout, _, err = runCLI(t, "", h.args("show", "bootstrap", "-o", "yaml")...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "is_ephemeral: false") || !strings.Contains(stdout, "is_ephemeral: true") {
		t.Fatalf("show YAML lacks per-KV is_ephemeral:\n%s", stdout)
	}

	for _, target := range []string{sqliteBackup, pathBackup} {
		backupStore, err := openSecretStoreForTest(target, h.key)
		if err != nil {
			t.Fatal(err)
		}
		details, err := backupStore.ShowSecret("bootstrap")
		if err != nil {
			backupStore.Close()
			t.Fatal(err)
		}
		value, err := backupStore.GetSecretValue("bootstrap", "key")
		closeErr := backupStore.Close()
		if err != nil {
			t.Fatal(err)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
		keyMarks := map[string]bool{}
		for _, key := range details.Keys {
			keyMarks[key.Key] = key.IsEphemeral
		}
		if keyMarks["key"] {
			t.Fatalf("%s backup kept the KV ephemeral", target)
		}
		if !keyMarks["other"] {
			t.Fatalf("%s backup changed another KV", target)
		}
		if string(value) != "after" {
			t.Fatalf("%s backup value = %q", target, value)
		}
	}

	// Failed writes must not mark a KV public.
	if _, _, err := runCLI(t, "", h.args("create", "stays")...); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, "", h.args("set", "stays/key", "original")...); err != nil {
		t.Fatal(err)
	}
	otherKey := writeTestIdentity(t, nil)
	otherArgs := func(rest ...string) []string {
		base := []string{"secret"}
		base = append(base, rest...)
		base = append(base, "--db", h.db, "--encryption-key-file", otherKey)
		return base
	}
	if _, _, err := runCLI(t, "", otherArgs("set", "stays/key", "changed", "--public")...); err == nil {
		t.Fatal("wrong recipient marked the KV public")
	}
	if view := showJSON("stays"); !keyMark(view, "key") {
		t.Fatalf("failed recipient change is_ephemeral = %#v", view["keys"])
	}
	tooLarge := strings.Repeat("x", tacos.MaxSecretValueBytes+1)
	if _, _, err := runCLI(t, tooLarge, h.args("set", "stays/key", "--stdin", "--public")...); err == nil {
		t.Fatal("oversized --stdin --public value was accepted")
	}
	if view := showJSON("stays"); !keyMark(view, "key") {
		t.Fatalf("failed oversized write is_ephemeral = %#v", view["keys"])
	}
	stdout, _, err = runCLI(t, "", h.args("get", "stays/key")...)
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "original" {
		t.Fatalf("failed writes changed the value: %q", stdout)
	}

	// --public works with --stdin and keeps the stdin semantics.
	if _, _, err := runCLI(t, "", h.args("create", "via-stdin")...); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, "from-stdin\n", h.args("set", "via-stdin/key", "--stdin", "--public")...); err != nil {
		t.Fatal(err)
	}
	stdout, _, err = runCLI(t, "", h.args("get", "via-stdin/key")...)
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "from-stdin\n" {
		t.Fatalf("stdin value = %q", stdout)
	}
	if view := showJSON("via-stdin"); keyMark(view, "key") {
		t.Fatalf("stdin --public is_ephemeral = %#v", view["keys"])
	}
}

func TestSecretCLIEndToEnd(t *testing.T) {
	h := newSecretCLI(t)
	stubNonTTY(t)

	stdout, stderr, err := runCLI(t, "", h.args("create", "bootstrap")...)
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "" {
		t.Fatalf("create stdout = %q", stdout)
	}
	if !strings.Contains(stderr, `Created secret "bootstrap"`) {
		t.Fatalf("create stderr = %q", stderr)
	}
	stdout, _, err = runCLI(t, "", "secret", "--db", h.db, "--encryption-key-file", h.key, "show", "bootstrap", "-o", "json")
	if err != nil {
		t.Fatalf("persistent flags before subcommand: %v", err)
	}
	if !strings.Contains(stdout, `"keys": []`) {
		t.Fatalf("empty secret JSON keys = %s", stdout)
	}
	if _, _, err := runCLI(t, "", h.args("create", "bootstrap")...); err == nil {
		t.Fatal("duplicate create succeeded")
	}

	value := "line1\nline2\n"
	stdout, stderr, err = runCLI(t, "", h.args("set", "bootstrap/api_key", value)...)
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "" || !strings.Contains(stderr, "Set bootstrap/api_key.") {
		t.Fatalf("set stdout/stderr = %q/%q", stdout, stderr)
	}

	stdout, _, err = runCLI(t, "", h.args("get", "bootstrap/api_key")...)
	if err != nil {
		t.Fatal(err)
	}
	if stdout != value {
		t.Fatalf("get = %q, want %q", stdout, value)
	}

	stdout, _, err = runCLI(t, "", h.args("list")...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "NAME") || !strings.Contains(stdout, "bootstrap") {
		t.Fatalf("list = %q", stdout)
	}

	stdout, _, err = runCLI(t, "", h.args("show", "bootstrap", "-o", "json")...)
	if err != nil {
		t.Fatal(err)
	}
	var view map[string]any
	if err := json.Unmarshal([]byte(stdout), &view); err != nil {
		t.Fatal(err)
	}
	if view["name"] != "bootstrap" {
		t.Fatalf("show json = %s", stdout)
	}
	if _, exists := view["is_ephemeral"]; exists {
		t.Fatalf("show JSON has Secret-level is_ephemeral: %s", stdout)
	}
	if strings.Contains(stdout, "\"keys\": null") {
		t.Fatalf("show JSON keys is null: %s", stdout)
	}
	if strings.Contains(stdout, value) || strings.Contains(strings.ToLower(stdout), "dek") || strings.Contains(stdout, "nonce") {
		t.Fatalf("show JSON leaked encryption metadata or value: %s", stdout)
	}
	keys, ok := view["keys"].([]any)
	if !ok || len(keys) != 1 {
		t.Fatalf("show keys = %#v", view["keys"])
	}
	if entry, _ := keys[0].(map[string]any); entry["is_ephemeral"] != true {
		t.Fatalf("show key mark = %#v", keys[0])
	}

	stdout, _, err = runCLI(t, "", h.args("show", "bootstrap", "-o", "yaml")...)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"id:", "name: bootstrap", "keys:", "key: api_key"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("show yaml missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, value) {
		t.Fatal("show YAML leaked the value")
	}

	stdout, _, err = runCLI(t, "from-stdin\n", h.args("set", "bootstrap/stdin", "--stdin")...)
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "" {
		t.Fatalf("set --stdin stdout = %q", stdout)
	}
	stdout, _, err = runCLI(t, "", h.args("get", "bootstrap/stdin")...)
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "from-stdin\n" {
		t.Fatalf("stdin value = %q", stdout)
	}

	stdout, _, err = runCLI(t, "", h.args("unset", "bootstrap/stdin")...)
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "" {
		t.Fatalf("unset stdout = %q", stdout)
	}
	if _, _, err := runCLI(t, "", h.args("get", "bootstrap/stdin")...); err == nil {
		t.Fatal("unset key is still readable")
	}

	stdout, stderr, err = runCLI(t, "", h.args("delete", "bootstrap", "--yes")...)
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "" || !strings.Contains(stderr, `Deleted secret "bootstrap"`) {
		t.Fatalf("delete stdout/stderr = %q/%q", stdout, stderr)
	}
	stdout, _, err = runCLI(t, "", h.args("list")...)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout, "bootstrap") {
		t.Fatalf("list after delete = %q", stdout)
	}
}

func TestSecretCLISetInputModes(t *testing.T) {
	h := newSecretCLI(t)
	stubNonTTY(t)
	if _, _, err := runCLI(t, "", h.args("create", "inputs")...); err != nil {
		t.Fatal(err)
	}

	if _, _, err := runCLI(t, "x", h.args("set", "inputs/a", "value", "--stdin")...); err == nil {
		t.Fatal("positional value and --stdin were accepted together")
	}
	if _, _, err := runCLI(t, "", h.args("set", "inputs/a")...); err == nil {
		t.Fatal("missing value on non-TTY was accepted")
	}

	stubTTY(t, "yes", []byte("tty-secret"))
	stdout, stderr, err := runCLI(t, "", h.args("set", "inputs/tty")...)
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "" {
		t.Fatalf("TTY set stdout = %q", stdout)
	}
	if strings.Contains(stderr, "tty-secret") {
		t.Fatal("TTY prompt echoed the value")
	}
	if !strings.Contains(stderr, "Enter value for inputs/tty") {
		t.Fatalf("TTY prompt = %q", stderr)
	}
	stdout, _, err = runCLI(t, "", h.args("get", "inputs/tty")...)
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "tty-secret" {
		t.Fatalf("TTY value = %q", stdout)
	}
}

func TestSecretCLIDeleteConfirmation(t *testing.T) {
	h := newSecretCLI(t)
	if _, _, err := runCLI(t, "", h.args("create", "confirm")...); err != nil {
		t.Fatal(err)
	}

	stubNonTTY(t)
	if _, _, err := runCLI(t, "", h.args("delete", "confirm")...); err == nil {
		t.Fatal("non-TTY delete without --yes was accepted")
	}
	if _, _, err := runCLI(t, "", h.args("show", "confirm")...); err != nil {
		t.Fatalf("secret was deleted: %v", err)
	}

	stubTTY(t, "no", nil)
	if _, _, err := runCLI(t, "", h.args("delete", "confirm")...); err == nil {
		t.Fatal("declined delete succeeded")
	}
	if _, _, err := runCLI(t, "", h.args("show", "confirm")...); err != nil {
		t.Fatalf("secret was deleted after decline: %v", err)
	}

	stubTTY(t, "YES", nil)
	_, stderr, err := runCLI(t, "", h.args("delete", "confirm")...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "Delete secret") || !strings.Contains(stderr, "0 key(s)") {
		t.Fatalf("confirmation prompt = %q", stderr)
	}

	if _, _, err := runCLI(t, "", h.args("create", "skip")...); err != nil {
		t.Fatal(err)
	}
	stubTTY(t, "no", nil)
	if _, _, err := runCLI(t, "", h.args("delete", "skip", "--yes")...); err != nil {
		t.Fatalf("--yes still prompted: %v", err)
	}
}

func TestSecretCLIMissingDatabase(t *testing.T) {
	h := newSecretCLI(t)
	stubNonTTY(t)
	if _, _, err := runCLI(t, "", h.args("create", "make")...); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.db); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(h.db); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(h.db + "-wal")
	_ = os.Remove(h.db + "-shm")

	for _, args := range [][]string{
		{"list"},
		{"show", "make"},
		{"set", "make/key", "value"},
		{"get", "make/key"},
		{"unset", "make/key"},
		{"delete", "make"},
		{"delete", "make", "--yes"},
	} {
		if _, _, err := runCLI(t, "", h.args(args...)...); err == nil {
			t.Errorf("%v succeeded on a missing database", args)
		}
		if _, err := os.Stat(h.db); !os.IsNotExist(err) {
			t.Errorf("%v created the database", args)
		}
	}
}

func TestSecretCLIListShowIgnoreBadKey(t *testing.T) {
	h := newSecretCLI(t)
	stubNonTTY(t)
	if _, _, err := runCLI(t, "", h.args("create", "meta")...); err != nil {
		t.Fatal(err)
	}
	badKey := filepath.Join(h.dir, "missing-key")
	if _, _, err := runCLI(t, "", "secret", "list", "--db", h.db, "--encryption-key-file", badKey); err != nil {
		t.Fatalf("list loaded the key: %v", err)
	}
	if _, _, err := runCLI(t, "", "secret", "show", "meta", "--db", h.db, "--encryption-key-file", badKey); err != nil {
		t.Fatalf("show loaded the key: %v", err)
	}
	t.Setenv("HOME", "")
	if _, _, err := runCLI(t, "", "secret", "list", "--db", h.db); err != nil {
		t.Fatalf("list required a resolvable home/key: %v", err)
	}
	if _, _, err := runCLI(t, "", "secret", "show", "meta", "--db", h.db); err != nil {
		t.Fatalf("show required a resolvable home/key: %v", err)
	}
	if _, _, err := runCLI(t, "", "secret", "get", "meta/x", "--db", h.db, "--encryption-key-file", badKey); err == nil {
		t.Fatal("get did not load the key")
	}

	missingDB := filepath.Join(h.dir, "missing.db")
	if _, _, err := runCLI(t, "", "secret", "list", "--db", missingDB, "--encryption-key-file", badKey); err == nil {
		t.Fatal("list on missing DB succeeded")
	}
	if _, err := os.Stat(missingDB); !os.IsNotExist(err) {
		t.Fatal("list created a missing DB")
	}
}

func TestSecretCLIWrongRecipientDoesNotModify(t *testing.T) {
	h := newSecretCLI(t)
	stubNonTTY(t)
	if _, _, err := runCLI(t, "", h.args("create", "owner")...); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, "", h.args("set", "owner/key", "original")...); err != nil {
		t.Fatal(err)
	}
	otherKey := writeTestIdentity(t, nil)
	otherArgs := func(rest ...string) []string {
		base := []string{"secret"}
		base = append(base, rest...)
		base = append(base, "--db", h.db, "--encryption-key-file", otherKey)
		return base
	}
	if stdout, _, err := runCLI(t, "", otherArgs("get", "owner/key")...); err == nil {
		t.Fatal("wrong recipient read the value")
	} else if stdout != "" {
		t.Fatalf("failed get wrote stdout: %q", stdout)
	} else if strings.Contains(err.Error(), "original") {
		t.Fatal("get error leaked the value")
	}
	if _, _, err := runCLI(t, "", otherArgs("set", "owner/key", "changed")...); err == nil {
		t.Fatal("wrong recipient modified the value")
	}
	if _, _, err := runCLI(t, "", otherArgs("unset", "owner/key")...); err == nil {
		t.Fatal("wrong recipient unset the value")
	}
	if _, _, err := runCLI(t, "", otherArgs("delete", "owner", "--yes")...); err == nil {
		t.Fatal("wrong recipient deleted the secret")
	}
	stdout, _, err := runCLI(t, "", h.args("get", "owner/key")...)
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "original" {
		t.Fatalf("owner value = %q", stdout)
	}
}

func TestSecretCLICreateWithMissingKeyDoesNotCreateDatabase(t *testing.T) {
	h := newSecretCLI(t)
	missingKey := filepath.Join(h.dir, "missing-key")
	if _, _, err := runCLI(t, "", "secret", "create", "x", "--db", h.db, "--encryption-key-file", missingKey); err == nil {
		t.Fatal("create with missing key succeeded")
	}
	if _, err := os.Stat(h.db); !os.IsNotExist(err) {
		t.Fatal("create created a database before key validation")
	}
}

func TestSecretCLIPassphraseAndStdin(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("MGCL_DB", "")
	t.Setenv("MGCL_ENCRYPTION_KEY_FILE", "")
	stubNonTTY(t)
	db := filepath.Join(dir, "mgcl.db")
	key := writeTestIdentity(t, []byte("passphrase"))

	t.Setenv(passphraseEnv, "passphrase")
	if _, _, err := runCLI(t, "", "secret", "create", "locked", "--db", db, "--encryption-key-file", key); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, "value\n", "secret", "set", "locked/key", "--stdin", "--db", db, "--encryption-key-file", key); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, "", "secret", "delete", "locked", "--yes", "--db", db, "--encryption-key-file", key); err != nil {
		t.Fatal(err)
	}

	unsetEnv(t, passphraseEnv)
	if _, _, err := runCLI(t, "", "secret", "create", "locked2", "--db", db, "--encryption-key-file", key); err == nil {
		t.Fatal("create without passphrase succeeded")
	}
	t.Setenv(passphraseEnv, "passphrase")
	if _, _, err := runCLI(t, "", "secret", "create", "locked2", "--db", db, "--encryption-key-file", key); err != nil {
		t.Fatal(err)
	}
	unsetEnv(t, passphraseEnv)
	if _, _, err := runCLI(t, "value\n", "secret", "set", "locked2/key", "--stdin", "--db", db, "--encryption-key-file", key); err == nil {
		t.Fatal("set --stdin without a passphrase succeeded")
	}
	stubTTY(t, "", nil)
	if _, _, err := runCLI(t, "value\n", "secret", "set", "locked2/key", "--stdin", "--db", db, "--encryption-key-file", key); err == nil {
		t.Fatal("set --stdin prompted for a passphrase on the consumed TTY")
	} else if !strings.Contains(err.Error(), passphraseEnv) {
		t.Fatalf("error does not mention %s: %v", passphraseEnv, err)
	}
	stubNonTTY(t)
	t.Setenv(passphraseEnv, "passphrase")
	if _, _, err := runCLI(t, "value\n", "secret", "set", "locked2/key", "--stdin", "--db", db, "--encryption-key-file", key); err != nil {
		t.Fatal(err)
	}
	unsetEnv(t, passphraseEnv)
	if _, _, err := runCLI(t, "", "secret", "delete", "locked2", "--yes", "--db", db, "--encryption-key-file", key); err == nil {
		t.Fatal("non-TTY delete --yes without a passphrase succeeded")
	}
}

func unsetEnv(t *testing.T, key string) {
	t.Helper()
	value, ok := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if ok {
			_ = os.Setenv(key, value)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

func TestSecretCLIConfigPriorityAndGitRootDB(t *testing.T) {
	root := t.TempDir()
	initTestGitRepo(t, root)
	key := writeTestIdentity(t, nil)
	if err := os.WriteFile(filepath.Join(root, ".mgcl.yaml"), []byte(
		"db: config.db\nencryption_key_file: "+key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(root, "sub")
	if err := os.MkdirAll(subdir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(subdir)
	stubNonTTY(t)
	t.Setenv("MGCL_DB", "")

	if _, _, err := runCLI(t, "", "secret", "create", "conf"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "config.db")); err != nil {
		t.Fatalf("config-relative DB was not anchored at the Git root: %v", err)
	}

	flagDB := filepath.Join(root, "flag.db")
	if _, _, err := runCLI(t, "", "secret", "create", "flag", "--db", flagDB); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(flagDB); err != nil {
		t.Fatalf("flag DB was not used: %v", err)
	}

	envDB := filepath.Join(root, "env.db")
	t.Setenv("MGCL_DB", envDB)
	if _, _, err := runCLI(t, "", "secret", "create", "env"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(envDB); err != nil {
		t.Fatalf("env DB was not used: %v", err)
	}
}

func TestSecretCLIBackupIntegration(t *testing.T) {
	h := newSecretCLI(t)
	stubNonTTY(t)
	sqliteTarget := filepath.Join(h.dir, "backup-sqlite.db")
	pathTarget := filepath.Join(h.dir, "backup-copy.db")

	args := append(h.args("create", "backed"),
		"--backup-to", "sqlite:"+sqliteTarget,
		"--backup-to", "path:"+pathTarget,
	)
	if _, _, err := runCLI(t, "", args...); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sqliteTarget); err != nil {
		t.Fatalf("sqlite backup target missing: %v", err)
	}
	if _, err := os.Stat(pathTarget); err != nil {
		t.Fatalf("path backup target missing: %v", err)
	}

	// Read the backup with the same recipient and confirm it is a complete copy.
	backupStore, err := openSecretStoreForTest(sqliteTarget, h.key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backupStore.ShowSecret("backed"); err != nil {
		backupStore.Close()
		t.Fatalf("sqlite backup does not contain the secret: %v", err)
	}
	backupStore.Close()
	pathStore, err := openSecretStoreForTest(pathTarget, h.key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pathStore.ShowSecret("backed"); err != nil {
		pathStore.Close()
		t.Fatalf("path backup does not contain the secret: %v", err)
	}
	pathStore.Close()

	// Read-only commands must not create or touch backup targets.
	readTarget := filepath.Join(h.dir, "never.db")
	if _, _, err := runCLI(t, "", h.args("set", "backed/key", "value")...); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, "", append(h.args("list"), "--backup-to", "sqlite:"+readTarget)...); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(readTarget); !os.IsNotExist(err) {
		t.Fatal("list created a backup target")
	}
	if _, _, err := runCLI(t, "", append(h.args("show", "backed"), "--backup-to", "sqlite:"+readTarget)...); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(readTarget); !os.IsNotExist(err) {
		t.Fatal("show created a backup target")
	}
	if stdout, _, err := runCLI(t, "", append(h.args("get", "backed/key"), "--backup-to", "sqlite:"+readTarget)...); err != nil {
		t.Fatal(err)
	} else if stdout != "value" {
		t.Fatalf("get = %q", stdout)
	}
	if _, err := os.Stat(readTarget); !os.IsNotExist(err) {
		t.Fatal("get created a backup target")
	}

	// A malformed target fails before the database is created.
	badDB := filepath.Join(h.dir, "bad.db")
	if _, _, err := runCLI(t, "", "secret", "create", "bad", "--db", badDB,
		"--encryption-key-file", h.key, "--backup-to", "sqlite:"+filepath.Join(h.dir, "missing", "x.db")); err == nil {
		t.Fatal("invalid backup target was accepted")
	} else if !strings.Contains(err.Error(), "backup destination") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(badDB); !os.IsNotExist(err) {
		t.Fatal("database was created despite backup validation failure")
	}

	// MGCL_BACKUP_TO is not a configuration source.
	envTarget := filepath.Join(h.dir, "env-backup.db")
	t.Setenv("MGCL_BACKUP_TO", "sqlite:"+envTarget)
	if _, _, err := runCLI(t, "", h.args("set", "backed/key", "value")...); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(envTarget); !os.IsNotExist(err) {
		t.Fatal("MGCL_BACKUP_TO created a backup target")
	}

	// A YAML backup_to type error still fails even when --backup-to replaces it.
	repo := t.TempDir()
	initTestGitRepo(t, repo)
	if err := os.WriteFile(filepath.Join(repo, ".mgcl.yaml"), []byte("backup_to: not-a-list\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	yamlDB := filepath.Join(repo, "yaml.db")
	if _, _, err := runCLI(t, "", "secret", "create", "yaml", "--db", yamlDB, "--encryption-key-file", h.key,
		"--backup-to", "sqlite:"+filepath.Join(repo, "ok.db")); err == nil {
		t.Fatal("YAML backup_to type error was ignored")
	} else if !strings.Contains(err.Error(), "backup_to") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(yamlDB); !os.IsNotExist(err) {
		t.Fatal("database was created despite YAML backup config error")
	}
}

func TestSecretCLIBackupFailureFailsClosed(t *testing.T) {
	h := newSecretCLI(t)
	stubNonTTY(t)
	target := filepath.Join(h.dir, "backup.db")
	backupFlag := "path:" + target

	if _, _, err := runCLI(t, "", append(h.args("create", "backed"), "--backup-to", backupFlag)...); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, "", append(h.args("set", "backed/key", "old"), "--backup-to", backupFlag)...); err != nil {
		t.Fatal(err)
	}

	oldHook := sqlitebackup.BeforePathCopy
	sqlitebackup.BeforePathCopy = func() error { return errors.New("injected path copy failure") }
	t.Cleanup(func() { sqlitebackup.BeforePathCopy = oldHook })

	stdout, stderr, err := runCLI(t, "", append(h.args("set", "backed/key", "new"), "--backup-to", backupFlag)...)
	if err == nil {
		t.Fatal("write with failing path backup succeeded")
	}
	if !strings.Contains(err.Error(), "backup failed") {
		t.Fatalf("unexpected error: %v", err)
	}
	if stdout != "" {
		t.Fatalf("failed write wrote stdout: %q", stdout)
	}
	if strings.Contains(stderr, "Set backed/key.") {
		t.Fatalf("failed write printed success: %q", stderr)
	}
	backupStore, err := openSecretStoreForTest(target, h.key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := backupStore.GetSecretValue("backed", "key")
	closeErr := backupStore.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if string(got) != "old" {
		t.Fatalf("backup target was updated despite failure: %q", got)
	}

	// A failed --public write must not print success or change visibility.
	stdout, stderr, err = runCLI(t, "", append(h.args("set", "backed/key", "new", "--public"), "--backup-to", backupFlag)...)
	if err == nil {
		t.Fatal("public write with failing path backup succeeded")
	}
	if stdout != "" {
		t.Fatalf("failed public write wrote stdout: %q", stdout)
	}
	if strings.Contains(stderr, "Set backed/key.") || strings.Contains(stderr, "is public") {
		t.Fatalf("failed public write printed success: %q", stderr)
	}
	backupStore, err = openSecretStoreForTest(target, h.key)
	if err != nil {
		t.Fatal(err)
	}
	details, err := backupStore.ShowSecret("backed")
	if err != nil {
		backupStore.Close()
		t.Fatal(err)
	}
	got, err = backupStore.GetSecretValue("backed", "key")
	closeErr = backupStore.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if len(details.Keys) != 1 || !details.Keys[0].IsEphemeral {
		t.Fatal("failed public write changed the backup KV mark")
	}
	if string(got) != "old" {
		t.Fatalf("failed public write changed the backup value: %q", got)
	}

	// A target that did not exist must not be generated when the copy fails.
	newTarget := filepath.Join(h.dir, "never-created.db")
	_, stderr, err = runCLI(t, "", "secret", "create", "fresh", "--db", filepath.Join(h.dir, "fresh.db"),
		"--encryption-key-file", h.key, "--backup-to", "path:"+newTarget)
	if err == nil {
		t.Fatal("create with failing path backup succeeded")
	}
	if strings.Contains(stderr, `Created secret "fresh"`) {
		t.Fatalf("failed create printed success: %q", stderr)
	}
	if _, statErr := os.Stat(newTarget); !os.IsNotExist(statErr) {
		t.Fatalf("failed path backup generated target: %v", statErr)
	}
}

func openSecretStoreForTest(path, key string) (*tacos.SecretStore, error) {
	identity, err := loadEncryptionIdentity(key)
	if err != nil {
		return nil, err
	}
	return tacos.OpenSecretStoreReadOnly(path, identity)
}
