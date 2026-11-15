package cmd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// confirmTerminalExact asks for the exact confirmation phrase (normally the
// full state reference). Anything else, including y/yes, cancels.
func confirmTerminalExact(stderr io.Writer, prompt, expected string) (bool, error) {
	fmt.Fprintf(stderr, "%s Type %q to confirm: ", prompt, expected)
	answer, err := readTerminalLine()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(answer) == expected, nil
}

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
