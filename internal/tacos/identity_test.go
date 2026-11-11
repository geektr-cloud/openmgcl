package tacos

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
)

func testIdentity(t *testing.T) *Identity {
	t.Helper()
	return testIdentityNamed(t, "default")
}

func testIdentityNamed(t *testing.T, name string) *Identity {
	t.Helper()
	seed := sha256.Sum256([]byte(t.Name() + "/" + name))
	identity, err := newIdentity(ed25519.NewKeyFromSeed(seed[:]))
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func testIdentityPrivate(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return private
}

func writeTestIdentity(t *testing.T, private ed25519.PrivateKey, passphrase []byte) string {
	t.Helper()
	var block *pem.Block
	var err error
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

func TestLoadIdentity(t *testing.T) {
	private := testIdentityPrivate(t)
	expected, err := newIdentity(private)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("unencrypted", func(t *testing.T) {
		path := writeTestIdentity(t, private, nil)
		identity, err := LoadIdentity(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if identity.RecipientFingerprint() != expected.RecipientFingerprint() {
			t.Fatalf("fingerprint = %q, want %q", identity.RecipientFingerprint(), expected.RecipientFingerprint())
		}
	})

	t.Run("encrypted", func(t *testing.T) {
		passphrase := []byte("correct horse battery staple")
		path := writeTestIdentity(t, private, passphrase)

		if _, err := LoadIdentity(path, nil); !errors.Is(err, ErrPassphraseRequired) {
			t.Fatalf("missing passphrase error = %v, want ErrPassphraseRequired", err)
		}
		identity, err := LoadIdentity(path, passphrase)
		if err != nil {
			t.Fatal(err)
		}
		if identity.RecipientFingerprint() != expected.RecipientFingerprint() {
			t.Fatal("encrypted key loaded with a different fingerprint")
		}
		if _, err := LoadIdentity(path, []byte("wrong")); err == nil {
			t.Fatal("wrong passphrase succeeded")
		}
	})

	t.Run("rejects permissive file mode", func(t *testing.T) {
		path := writeTestIdentity(t, private, nil)
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadIdentity(path, nil); err == nil {
			t.Fatal("group/world-readable key was accepted")
		}
	})

	t.Run("rejects non-Ed25519 key", func(t *testing.T) {
		rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		block, err := ssh.MarshalPrivateKey(rsaKey, "")
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "id_rsa")
		if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadIdentity(path, nil); err == nil {
			t.Fatal("RSA key was accepted")
		}
	})

	t.Run("rejects trailing data", func(t *testing.T) {
		path := writeTestIdentity(t, private, nil)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(data, []byte("trailing")...), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadIdentity(path, nil); err == nil {
			t.Fatal("trailing data was accepted")
		}
	})
}

func TestNewIdentityRejectsMismatchedPublicKey(t *testing.T) {
	private := testIdentityPrivate(t)
	tampered := append(ed25519.PrivateKey(nil), private...)
	tampered[63] ^= 1
	if _, err := newIdentity(tampered); err == nil {
		t.Fatal("private key with mismatched embedded public key was accepted")
	}
}

func TestRecipientFingerprintMatchesSSHPublicKey(t *testing.T) {
	private := testIdentityPrivate(t)
	identity, err := newIdentity(private)
	if err != nil {
		t.Fatal(err)
	}
	public, err := ssh.NewPublicKey(private.Public())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := identity.RecipientFingerprint(), ssh.FingerprintSHA256(public); got != want {
		t.Fatalf("fingerprint = %q, want %q", got, want)
	}
}

func TestEd25519ToX25519Deterministic(t *testing.T) {
	seed := bytes.Repeat([]byte{0x42}, 32)
	private := ed25519.NewKeyFromSeed(seed)
	identity, err := newIdentity(private)
	if err != nil {
		t.Fatal(err)
	}

	// Known-answer value for seed 0x42... with the libsodium-compatible
	// Ed25519→X25519 conversion (RFC 7748 birational map).
	const wantXPublic = "cc4f2cdb695dd766f34118eb67b98652fed1d8bc49c330b119bbfa8a64989378"
	if got := hex.EncodeToString(identity.x25519Private.PublicKey().Bytes()); got != wantXPublic {
		t.Fatalf("X25519 public key = %s, want %s", got, wantXPublic)
	}

	// The public conversion must agree with an independent Ed25519 point
	// operation; newIdentity already enforces that equality, so this catches
	// accidental changes to the private scalar derivation without only
	// comparing a value to itself.
	second, err := newIdentity(ed25519.NewKeyFromSeed(seed))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(identity.x25519Private.Bytes(), second.x25519Private.Bytes()) {
		t.Fatal("deterministic seed produced different X25519 keys")
	}
}
