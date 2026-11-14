package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewConfigReadsGitRootFromSubdirectory(t *testing.T) {
	root := t.TempDir()
	initTestGitRepo(t, root)
	if err := os.WriteFile(filepath.Join(root, ".mgcl.yaml"), []byte("host: root-config\ndb: state.db\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	subdir := filepath.Join(root, "nested", "workdir")
	if err := os.MkdirAll(subdir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(subdir)

	config, gotRoot, err := newConfig()
	if err != nil {
		t.Fatal(err)
	}
	if gotRoot != root {
		t.Errorf("root = %q, want %q", gotRoot, root)
	}
	if got := config.GetString("host"); got != "root-config" {
		t.Errorf("host = %q, want root-config", got)
	}
	if got := config.GetString("db"); got != "state.db" {
		t.Errorf("db = %q, want state.db", got)
	}
}

func TestNewConfigIgnoresLegacyConfigYAML(t *testing.T) {
	root := t.TempDir()
	initTestGitRepo(t, root)
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte("host: legacy-config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	config, _, err := newConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := config.GetString("host"); got != "" {
		t.Errorf("host = %q, want empty (config.yaml must be ignored)", got)
	}
}

func TestNewConfigProjectFileWinsOverLegacyConfigYAML(t *testing.T) {
	root := t.TempDir()
	initTestGitRepo(t, root)
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte("host: legacy-config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".mgcl.yaml"), []byte("host: project-config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	config, _, err := newConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := config.GetString("host"); got != "project-config" {
		t.Errorf("host = %q, want project-config", got)
	}
}

func TestNewConfigInvalidFileFails(t *testing.T) {
	root := t.TempDir()
	initTestGitRepo(t, root)
	if err := os.WriteFile(filepath.Join(root, ".mgcl.yaml"), []byte("host: [unterminated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	_, _, err := newConfig()
	if err == nil {
		t.Fatal("invalid config did not fail")
	}
	if !strings.Contains(err.Error(), "read config") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestNewConfigEnvironmentOverridesFile(t *testing.T) {
	root := t.TempDir()
	initTestGitRepo(t, root)
	if err := os.WriteFile(filepath.Join(root, ".mgcl.yaml"), []byte("host: file-host\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	t.Setenv("MGCL_HOST", "env-host")

	config, _, err := newConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := config.GetString("host"); got != "env-host" {
		t.Errorf("host = %q, want env-host", got)
	}
}

func TestNewConfigOutsideGitIgnoresProjectFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".mgcl.yaml"), []byte("host: stray-config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	config, root, err := newConfig()
	if err != nil {
		t.Fatal(err)
	}
	if root != "" {
		t.Skipf("temp directory is inside Git repository %q", root)
	}
	if got := config.GetString("host"); got != "" {
		t.Errorf("host = %q, want empty outside Git", got)
	}
}
