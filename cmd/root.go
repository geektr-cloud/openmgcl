package cmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var version = "dev"

var rootCmd = &cobra.Command{
	Use:     "mgcl",
	Short:   "OpenMgCl CLI",
	Long:    "OpenMgCl - a local TACOS state backend for OpenTofu",
	Version: version,
	// Errors are handled in main so expected child-process exit codes can be
	// returned without Cobra printing an error or usage message.
	SilenceErrors: true,
	SilenceUsage:  true,
}

func init() {
	rootCmd.AddCommand(runCmd)
}

// newConfig builds a Viper instance for config file and environment lookup.
// ./config.yaml takes precedence over /etc/mgcl/config.yaml. A missing config
// file is not an error; unreadable or malformed config files are.
func newConfig() (*viper.Viper, error) {
	v := viper.New()
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath(".")
	v.AddConfigPath("/etc/mgcl")
	v.SetEnvPrefix("MGCL")
	v.AutomaticEnv()

	if err := v.ReadInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		if errors.As(err, &notFound) {
			return v, nil
		}
		return nil, fmt.Errorf("read config: %w", err)
	}
	return v, nil
}

func Execute() error {
	return rootCmd.Execute()
}
