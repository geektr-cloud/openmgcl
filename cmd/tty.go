package cmd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// Test hooks keep terminal-only branches testable without a real PTY.
var (
	stdinIsTerminal = func() bool {
		return term.IsTerminal(int(os.Stdin.Fd()))
	}
	readTerminalLine = func() (string, error) {
		reader := bufio.NewReader(os.Stdin)
		line, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return "", err
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	readTerminalPassword = func(prompt string, stderr io.Writer) ([]byte, error) {
		fmt.Fprint(stderr, prompt)
		value, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(stderr)
		return value, err
	}
)
