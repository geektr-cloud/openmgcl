package cmd

import (
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
	if opts.dbPath != defaultDBPath {
		t.Errorf("db = %q, want %q", opts.dbPath, defaultDBPath)
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
		"--db", defaultDBPath,
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
	if opts.dbPath != defaultDBPath {
		t.Errorf("db = %q", opts.dbPath)
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
		opts.applyConfig(config)

		if opts.host != "config-host" || opts.serverURL != "http://config-server" ||
			opts.dbPath != "/config/db" || opts.token != "config-token" ||
			opts.encryptionKeyFile != "/config/key" {
			t.Errorf("config not applied: %+v", opts)
		}
	})

	t.Run("explicit defaults win over config", func(t *testing.T) {
		opts, err := parseRunArgs([]string{
			"--host", defaultHost,
			"--db", defaultDBPath,
			"--server", "",
			"--token", "",
			"--encryption-key-file", "",
		})
		if err != nil {
			t.Fatal(err)
		}
		opts.applyConfig(config)

		if opts.host != defaultHost {
			t.Errorf("host = %q, want %q", opts.host, defaultHost)
		}
		if opts.dbPath != defaultDBPath {
			t.Errorf("db = %q, want %q", opts.dbPath, defaultDBPath)
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
		opts.applyConfig(config)

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
	opts.applyConfig(config)
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
	opts.applyConfig(config)
	if opts.host != defaultHost {
		t.Errorf("explicit host overridden by env: %q", opts.host)
	}
	if opts.token != "" {
		t.Errorf("explicit empty token overridden by env: %q", opts.token)
	}
}
