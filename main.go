package main

import (
	"os"

	"github.com/geektr-cloud/openmgcl/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
