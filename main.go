package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/geektr-cloud/openmgcl/cmd"
)

func main() {
	err := cmd.Execute()
	if err == nil {
		return
	}

	var exitErr interface{ ExitCode() int }
	if errors.As(err, &exitErr) {
		code := exitErr.ExitCode()
		if code <= 0 {
			code = 1
		}
		os.Exit(code)
	}

	fmt.Fprintln(os.Stderr, "Error:", err)
	os.Exit(1)
}
