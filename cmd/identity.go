package cmd

import (
	"errors"
	"fmt"
	"os"

	"github.com/geektr-cloud/openmgcl/internal/tacos"
	"golang.org/x/term"
)

const passphraseEnv = "MGCL_ENCRYPTION_KEY_PASSPHRASE"

func loadEncryptionIdentity(path string) (*tacos.Identity, error) {
	return loadEncryptionIdentityTTY(path, true)
}

// loadEncryptionIdentityTTY loads the key without changing the passphrase
// rules. allowTTY=false is used by commands whose stdin is already consumed as
// data (for example secret set --stdin), so they never prompt on that stream.
func loadEncryptionIdentityTTY(path string, allowTTY bool) (*tacos.Identity, error) {
	identity, err := tacos.LoadIdentity(path, nil)
	if err == nil {
		return identity, nil
	}
	if !errors.Is(err, tacos.ErrPassphraseRequired) {
		return nil, err
	}

	if passphrase, ok := os.LookupEnv(passphraseEnv); ok {
		value := []byte(passphrase)
		defer clearPassphrase(value)
		return tacos.LoadIdentity(path, value)
	}
	if !allowTTY {
		return nil, fmt.Errorf("encryption key requires a passphrase; set %s when using --stdin or non-interactive stdin", passphraseEnv)
	}
	if !stdinIsTerminal() {
		return nil, errors.New("encryption key requires a passphrase, but stdin is not a terminal")
	}

	fmt.Fprintf(os.Stderr, "Enter passphrase for %s: ", path)
	passphrase, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("read passphrase: %w", err)
	}
	defer clearPassphrase(passphrase)
	return tacos.LoadIdentity(path, passphrase)
}

func clearPassphrase(passphrase []byte) {
	for i := range passphrase {
		passphrase[i] = 0
	}
}
