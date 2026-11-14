package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/spf13/viper"
)

func TestParseRunArgsBackupToRepeated(t *testing.T) {
	opts, err := parseRunArgs([]string{
		"--backup-to", "sqlite:/tmp/a.db",
		"--backup-to", "path:/mnt/b.db",
		"--", "tofu", "plan",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"sqlite:/tmp/a.db", "path:/mnt/b.db"}
	if !reflect.DeepEqual(opts.backupTo, want) {
		t.Errorf("backupTo = %v, want %v", opts.backupTo, want)
	}
	if !opts.explicit["backup_to"] {
		t.Error("backup_to was not recorded as explicit")
	}
	if !reflect.DeepEqual(opts.command, []string{"tofu", "plan"}) {
		t.Errorf("command = %v", opts.command)
	}
}

func TestParseRunArgsBackupToRequiresValue(t *testing.T) {
	if _, err := parseRunArgs([]string{"--backup-to"}); err == nil {
		t.Fatal("--backup-to without a value did not fail")
	}
}

func TestParseRunArgsBackupToAfterDoubleDashIsChildArg(t *testing.T) {
	opts, err := parseRunArgs([]string{"--", "--backup-to", "sqlite:/tmp/a.db"})
	if err != nil {
		t.Fatal(err)
	}
	if len(opts.backupTo) != 0 {
		t.Errorf("backupTo = %v, want empty", opts.backupTo)
	}
	if !reflect.DeepEqual(opts.command, []string{"--backup-to", "sqlite:/tmp/a.db"}) {
		t.Errorf("command = %v", opts.command)
	}
}

func TestBackupDestinationsFlagReplacesYAML(t *testing.T) {
	config := viper.New()
	config.Set("backup_to", []any{"sqlite:/from-yaml/one.db", "path:/from-yaml/two.db"})

	opts, err := parseRunArgs([]string{"--backup-to", "sqlite:/from-flag/db"})
	if err != nil {
		t.Fatal(err)
	}
	specs, base, err := opts.backupDestinations(config, "/git/root")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(specs, []string{"sqlite:/from-flag/db"}) {
		t.Errorf("specs = %v", specs)
	}
	if base != "" {
		t.Errorf("flag base = %q, want cwd base", base)
	}
}

func TestBackupDestinationsFlagStillValidatesYAMLType(t *testing.T) {
	config := viper.New()
	config.Set("backup_to", "sqlite:/from-yaml.db")

	opts, err := parseRunArgs([]string{"--backup-to", "sqlite:/from-flag.db"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := opts.backupDestinations(config, "/git/root"); err == nil {
		t.Fatal("invalid YAML backup_to type was ignored because a flag was present")
	}
}

func TestBackupDestinationsFromYAML(t *testing.T) {
	config := viper.New()
	config.Set("backup_to", []any{"sqlite:backup/one.db", "path:backup/two.db"})

	opts, err := parseRunArgs(nil)
	if err != nil {
		t.Fatal(err)
	}
	specs, base, err := opts.backupDestinations(config, "/git/root")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(specs, []string{"sqlite:backup/one.db", "path:backup/two.db"}) {
		t.Errorf("specs = %v", specs)
	}
	if base != "/git/root" {
		t.Errorf("base = %q, want Git root", base)
	}
}

func TestBackupDestinationsEmptyAndInvalid(t *testing.T) {
	opts, err := parseRunArgs(nil)
	if err != nil {
		t.Fatal(err)
	}

	for name, value := range map[string]any{
		"missing":     nil,
		"null":        nil,
		"empty slice": []any{},
	} {
		config := viper.New()
		if value != nil {
			config.Set("backup_to", value)
		}
		specs, _, err := opts.backupDestinations(config, "/git/root")
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if len(specs) != 0 {
			t.Errorf("%s: specs = %v, want empty", name, specs)
		}
	}

	for name, value := range map[string]any{
		"scalar":          "sqlite:/tmp/a.db",
		"map":             map[string]any{"sqlite": "/tmp/a.db"},
		"non-string item": []any{"sqlite:/tmp/a.db", 42},
	} {
		config := viper.New()
		config.Set("backup_to", value)
		if _, _, err := opts.backupDestinations(config, "/git/root"); err == nil {
			t.Errorf("%s: invalid backup_to did not fail", name)
		}
	}
}

func TestBackupDestinationsYAMLTypeErrors(t *testing.T) {
	cases := map[string]string{
		"scalar":     "backup_to: sqlite:/tmp/a.db\n",
		"mapping":    "backup_to:\n  sqlite: /tmp/a.db\n",
		"non-string": "backup_to:\n  - sqlite:/tmp/a.db\n  - 42\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			initTestGitRepo(t, root)
			if err := os.WriteFile(filepath.Join(root, ".mgcl.yaml"), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Chdir(root)

			config, _, err := newConfig()
			if err != nil {
				t.Fatal(err)
			}
			opts, err := parseRunArgs(nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := opts.backupDestinations(config, root); err == nil {
				t.Fatalf("invalid backup_to type was accepted: %s", content)
			}
		})
	}
}

func TestMGCLBackupToEnvironmentIsIgnored(t *testing.T) {
	root := t.TempDir()
	initTestGitRepo(t, root)
	if err := os.WriteFile(filepath.Join(root, ".mgcl.yaml"), []byte("backup_to:\n  - sqlite:from-file.db\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	t.Setenv("MGCL_BACKUP_TO", "sqlite:from-env.db")

	config, _, err := newConfig()
	if err != nil {
		t.Fatal(err)
	}
	opts, err := parseRunArgs(nil)
	if err != nil {
		t.Fatal(err)
	}
	specs, base, err := opts.backupDestinations(config, root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(specs, []string{"sqlite:from-file.db"}) {
		t.Errorf("specs = %v, want YAML value only", specs)
	}
	if base != root {
		t.Errorf("base = %q, want %q", base, root)
	}
}

func TestMGCLBackupToEnvironmentAloneDoesNothing(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("MGCL_BACKUP_TO", "sqlite:from-env.db")

	config, _, err := newConfig()
	if err != nil {
		t.Fatal(err)
	}
	if value := config.Get("backup_to"); value != nil {
		t.Fatalf("MGCL_BACKUP_TO leaked into config: %v", value)
	}
}

func TestExistingEnvironmentBindingsStillWork(t *testing.T) {
	root := t.TempDir()
	initTestGitRepo(t, root)
	if err := os.WriteFile(filepath.Join(root, ".mgcl.yaml"), []byte("host: file-host\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	t.Setenv("MGCL_HOST", "")

	config, _, err := newConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := config.GetString("host"); got != "file-host" {
		t.Errorf("empty MGCL_HOST did not fall back to config: %q", got)
	}

	t.Setenv("MGCL_HOST", "env-host")
	config, _, err = newConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := config.GetString("host"); got != "env-host" {
		t.Errorf("MGCL_HOST = %q, want env-host", got)
	}
	if value := config.Get("backup_to"); value != nil {
		t.Errorf("MGCL_BACKUP_TO unexpectedly bound: %v", value)
	}
}
