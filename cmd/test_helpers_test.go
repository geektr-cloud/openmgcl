package cmd

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/geektr-cloud/openmgcl/internal/tacos"
	"golang.org/x/crypto/ssh"
)

// initTestGitRepo marks dir as a Git working tree. The git_utils package only
// needs .git/HEAD to recognize a repository, so tests do not invoke Git.
func initTestGitRepo(t *testing.T, dir string) {
	t.Helper()
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(gitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeTestIdentity(t *testing.T, passphrase []byte) string {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var block *pem.Block
	if passphrase == nil {
		block, err = ssh.MarshalPrivateKey(private, "")
	} else {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(private, "", passphrase)
	}
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func testTacosIdentity(t *testing.T) *tacos.Identity {
	t.Helper()
	identity, err := tacos.LoadIdentity(writeTestIdentity(t, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}
