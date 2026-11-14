package cmd

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

func TestParseRunArgsDefaults(t *testing.T) {
	opts, err := parseRunArgs(nil)
	if err != nil {
		t.Fatal(err)
	}
	if opts.host != defaultHost {
		t.Errorf("host = %q, want %q", opts.host, defaultHost)
	}
	if opts.dbPath != "" {
		t.Errorf("db = %q, want empty", opts.dbPath)
	}
	if opts.serverURL != "" || opts.token != "" || opts.encryptionKeyFile != "" {
		t.Errorf("unexpected non-empty defaults: %+v", opts)
	}
	if len(opts.explicit) != 0 {
		t.Errorf("explicit = %v, want empty", opts.explicit)
	}
	if len(opts.command) != 0 {
		t.Errorf("command = %v, want empty", opts.command)
	}
}

func TestParseRunArgsRecordsExplicitFlags(t *testing.T) {
	opts, err := parseRunArgs([]string{
		"--host", "127.0.0.1:9000",
		"--server", "",
		"--db", "state.db",
		"--token", "",
		"--encryption-key-file", "",
		"--", "sh", "-c", "true",
	})
	if err != nil {
		t.Fatal(err)
	}

	wantExplicit := map[string]bool{
		"host":                true,
		"server":              true,
		"db":                  true,
		"token":               true,
		"encryption_key_file": true,
	}
	if !reflect.DeepEqual(opts.explicit, wantExplicit) {
		t.Errorf("explicit = %v, want %v", opts.explicit, wantExplicit)
	}
	if opts.host != "127.0.0.1:9000" {
		t.Errorf("host = %q", opts.host)
	}
	if opts.dbPath != "state.db" {
		t.Errorf("db = %q, want state.db", opts.dbPath)
	}
	if opts.token != "" || opts.serverURL != "" || opts.encryptionKeyFile != "" {
		t.Errorf("explicit empty values were not preserved: %+v", opts)
	}
	wantCommand := []string{"sh", "-c", "true"}
	if !reflect.DeepEqual(opts.command, wantCommand) {
		t.Errorf("command = %v, want %v", opts.command, wantCommand)
	}
}

func TestParseRunArgsShortFlags(t *testing.T) {
	opts, err := parseRunArgs([]string{
		"-h", "0.0.0.0:8080",
		"-s", "http://example.com",
		"-d", "/tmp/state.db",
		"-t", "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if opts.host != "0.0.0.0:8080" || opts.serverURL != "http://example.com" ||
		opts.dbPath != "/tmp/state.db" || opts.token != "secret" {
		t.Errorf("unexpected options: %+v", opts)
	}
}

func TestParseRunArgsErrors(t *testing.T) {
	for _, flag := range []string{
		"--host", "-h", "--server", "-s", "--db", "-d", "--token", "-t",
		"--encryption-key-file",
	} {
		t.Run(flag, func(t *testing.T) {
			if _, err := parseRunArgs([]string{flag}); err == nil {
				t.Fatalf("%s without value did not fail", flag)
			}
		})
	}

	if _, err := parseRunArgs([]string{"--unknown"}); err == nil {
		t.Fatal("unknown flag did not fail")
	} else if !strings.Contains(err.Error(), "unknown flag") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestParseRunArgsVersionAndHelp(t *testing.T) {
	opts, err := parseRunArgs([]string{"--version", "--not-a-flag"})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.showVersion {
		t.Error("showVersion not set")
	}

	opts, err = parseRunArgs([]string{"--help", "--not-a-flag"})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.showHelp {
		t.Error("showHelp not set")
	}
}

func TestApplyConfig(t *testing.T) {
	config := viper.New()
	config.Set("host", "config-host")
	config.Set("server", "http://config-server")
	config.Set("db", "/config/db")
	config.Set("token", "config-token")
	config.Set("encryption_key_file", "/config/key")

	t.Run("config fills unset values", func(t *testing.T) {
		opts, err := parseRunArgs(nil)
		if err != nil {
			t.Fatal(err)
		}
		opts.applyConfig(config, "/git/root")

		if opts.host != "config-host" || opts.serverURL != "http://config-server" ||
			opts.dbPath != "/config/db" || opts.token != "config-token" ||
			opts.encryptionKeyFile != "/config/key" {
			t.Errorf("config not applied: %+v", opts)
		}
	})

	t.Run("explicit defaults win over config", func(t *testing.T) {
		opts, err := parseRunArgs([]string{
			"--host", defaultHost,
			"--db", "",
			"--server", "",
			"--token", "",
			"--encryption-key-file", "",
		})
		if err != nil {
			t.Fatal(err)
		}
		opts.applyConfig(config, "/git/root")

		if opts.host != defaultHost {
			t.Errorf("host = %q, want %q", opts.host, defaultHost)
		}
		if opts.dbPath != "" {
			t.Errorf("db = %q, want empty", opts.dbPath)
		}
		if opts.serverURL != "" {
			t.Errorf("server = %q, want empty", opts.serverURL)
		}
		if opts.token != "" {
			t.Errorf("token = %q, want empty", opts.token)
		}
		if opts.encryptionKeyFile != "" {
			t.Errorf("encryption key = %q, want empty", opts.encryptionKeyFile)
		}
	})

	t.Run("partial flags only override matching values", func(t *testing.T) {
		opts, err := parseRunArgs([]string{"--token", "flag-token"})
		if err != nil {
			t.Fatal(err)
		}
		opts.applyConfig(config, "/git/root")

		if opts.token != "flag-token" {
			t.Errorf("token = %q, want flag-token", opts.token)
		}
		if opts.host != "config-host" {
			t.Errorf("host = %q, want config-host", opts.host)
		}
	})
}

func TestApplyConfigEnvironment(t *testing.T) {
	t.Setenv("MGCL_HOST", "env-host")
	t.Setenv("MGCL_TOKEN", "env-token")

	config := viper.New()
	config.SetEnvPrefix("MGCL")
	config.AutomaticEnv()

	opts, err := parseRunArgs(nil)
	if err != nil {
		t.Fatal(err)
	}
	opts.applyConfig(config, "")
	if opts.host != "env-host" {
		t.Errorf("host = %q, want env-host", opts.host)
	}
	if opts.token != "env-token" {
		t.Errorf("token = %q, want env-token", opts.token)
	}

	opts, err = parseRunArgs([]string{"--host", defaultHost, "--token", ""})
	if err != nil {
		t.Fatal(err)
	}
	opts.applyConfig(config, "")
	if opts.host != defaultHost {
		t.Errorf("explicit host overridden by env: %q", opts.host)
	}
	if opts.token != "" {
		t.Errorf("explicit empty token overridden by env: %q", opts.token)
	}
}

func TestApplyConfigDBPathResolution(t *testing.T) {
	newConfigWithDB := func(db string) *viper.Viper {
		config := viper.New()
		config.Set("db", db)
		return config
	}

	t.Run("config path is relative to git root", func(t *testing.T) {
		opts, err := parseRunArgs(nil)
		if err != nil {
			t.Fatal(err)
		}
		opts.applyConfig(newConfigWithDB("state/terraform.db"), "/git/root")

		want := filepath.Join("/git/root", "state", "terraform.db")
		if opts.dbPath != want {
			t.Errorf("db = %q, want %q", opts.dbPath, want)
		}
	})

	t.Run("absolute config path is unchanged", func(t *testing.T) {
		opts, err := parseRunArgs(nil)
		if err != nil {
			t.Fatal(err)
		}
		opts.applyConfig(newConfigWithDB("/var/lib/mgcl.db"), "/git/root")

		if opts.dbPath != "/var/lib/mgcl.db" {
			t.Errorf("db = %q, want /var/lib/mgcl.db", opts.dbPath)
		}
	})

	t.Run("empty config path is unspecified", func(t *testing.T) {
		opts, err := parseRunArgs(nil)
		if err != nil {
			t.Fatal(err)
		}
		opts.applyConfig(newConfigWithDB(""), "/git/root")

		if opts.dbPath != "" {
			t.Errorf("db = %q, want empty", opts.dbPath)
		}
	})

	t.Run("non-empty environment overrides config and stays relative to cwd", func(t *testing.T) {
		t.Setenv("MGCL_DB", "env/terraform.db")

		opts, err := parseRunArgs(nil)
		if err != nil {
			t.Fatal(err)
		}
		opts.applyConfig(newConfigWithDB("config.db"), "/git/root")

		if opts.dbPath != "env/terraform.db" {
			t.Errorf("db = %q, want env/terraform.db", opts.dbPath)
		}
	})

	t.Run("empty environment falls back to config", func(t *testing.T) {
		t.Setenv("MGCL_DB", "")

		opts, err := parseRunArgs(nil)
		if err != nil {
			t.Fatal(err)
		}
		opts.applyConfig(newConfigWithDB("config.db"), "/git/root")

		want := filepath.Join("/git/root", "config.db")
		if opts.dbPath != want {
			t.Errorf("db = %q, want %q", opts.dbPath, want)
		}
	})

	t.Run("flag path stays relative to cwd and wins over env and config", func(t *testing.T) {
		t.Setenv("MGCL_DB", "env/terraform.db")

		opts, err := parseRunArgs([]string{"--db", "flag/terraform.db"})
		if err != nil {
			t.Fatal(err)
		}
		opts.applyConfig(newConfigWithDB("config.db"), "/git/root")

		if opts.dbPath != "flag/terraform.db" {
			t.Errorf("db = %q, want flag/terraform.db", opts.dbPath)
		}
	})

	t.Run("explicit empty flag does not fall back to env or config", func(t *testing.T) {
		t.Setenv("MGCL_DB", "env/terraform.db")

		opts, err := parseRunArgs([]string{"--db", ""})
		if err != nil {
			t.Fatal(err)
		}
		opts.applyConfig(newConfigWithDB("config.db"), "/git/root")

		if opts.dbPath != "" {
			t.Errorf("db = %q, want empty", opts.dbPath)
		}
	})
}
