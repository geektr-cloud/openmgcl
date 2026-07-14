package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var version = "dev"

var rootCmd = &cobra.Command{
	Use:     "mgcl",
	Short:   "OpenMGCL CLI",
	Long:    "OpenMGCL - Open MetaGaming Cloud Local development helper",
	Version: version,
}

func init() {
	cobra.OnInitialize(initConfig)
	rootCmd.AddCommand(runCmd)
}

func initConfig() {
	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	viper.AddConfigPath("/etc/mgcl")
	viper.AddConfigPath(".")
	viper.SetEnvPrefix("MGCL")
	viper.AutomaticEnv()

	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			fmt.Fprintln(os.Stderr, "Error reading config:", err)
		}
	}
}

func Execute() error {
	return rootCmd.Execute()
}
