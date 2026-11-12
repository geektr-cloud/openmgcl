package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewConfigReadsWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("host: workspace-config\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	config, err := newConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := config.GetString("host"); got != "workspace-config" {
		t.Errorf("host = %q, want workspace-config", got)
	}
}

func TestNewConfigInvalidFileFails(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("host: [unterminated\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := newConfig()
	if err == nil {
		t.Fatal("invalid config did not fail")
	}
	if !strings.Contains(err.Error(), "read config") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestNewConfigEnvironmentOverridesFile(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("host: file-host\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MGCL_HOST", "env-host")

	config, err := newConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := config.GetString("host"); got != "env-host" {
		t.Errorf("host = %q, want env-host", got)
	}
}
