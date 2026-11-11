package tacos

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"os"

	"filippo.io/edwards25519"
	"golang.org/x/crypto/ssh"
)

// ErrPassphraseRequired reports that an OpenSSH private key is encrypted and
// no passphrase was supplied.
var ErrPassphraseRequired = errors.New("ssh private key requires a passphrase")

// Identity is the locally configured Ed25519 recipient and its X25519
// conversion. It never contains another recipient's private key.
type Identity struct {
	x25519Private    *ecdh.PrivateKey
	recipient        string
	recipientPEMHash [sha256.Size]byte
}

// RecipientFingerprint returns the OpenSSH SHA256 fingerprint of the identity's
// Ed25519 public key.
func (i *Identity) RecipientFingerprint() string {
	return i.recipient
}

// LoadIdentity reads and validates an OpenSSH Ed25519 private key. A nil
// passphrase means no passphrase was supplied; a non-nil empty slice attempts
// an empty passphrase.
func LoadIdentity(path string, passphrase []byte) (*Identity, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat encryption key: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("encryption key %q is not a regular file", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("encryption key %q must not be accessible by group or other users", path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read encryption key: %w", err)
	}
	defer clearBytes(data)
	identity, err := parseIdentity(data, passphrase)
	if err != nil {
		return nil, fmt.Errorf("parse encryption key %q: %w", path, err)
	}
	return identity, nil
}

func parseIdentity(data, passphrase []byte) (*Identity, error) {
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "OPENSSH PRIVATE KEY" {
		return nil, errors.New("encryption key must be an OpenSSH private key")
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("unexpected data after OpenSSH private key")
	}

	key, err := ssh.ParseRawPrivateKey(data)
	if err != nil {
		var missing *ssh.PassphraseMissingError
		if !errors.As(err, &missing) {
			return nil, err
		}
		if passphrase == nil {
			return nil, ErrPassphraseRequired
		}
		key, err = ssh.ParseRawPrivateKeyWithPassphrase(data, passphrase)
		if err != nil {
			return nil, err
		}
	}

	var edPrivate ed25519.PrivateKey
	switch k := key.(type) {
	case ed25519.PrivateKey:
		edPrivate = k
	case *ed25519.PrivateKey:
		edPrivate = *k
	default:
		return nil, errors.New("encryption key must be an Ed25519 private key")
	}
	return newIdentity(edPrivate)
}

func newIdentity(private ed25519.PrivateKey) (*Identity, error) {
	if len(private) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid Ed25519 private key length")
	}

	seed := private.Seed()
	rebuilt := ed25519.NewKeyFromSeed(seed)
	if !bytes.Equal(rebuilt, private) {
		return nil, errors.New("Ed25519 private key seed does not match embedded public key")
	}

	edPublic := ed25519.PublicKey(rebuilt[32:])
	point, err := new(edwards25519.Point).SetBytes(edPublic)
	if err != nil {
		return nil, fmt.Errorf("invalid Ed25519 public key: %w", err)
	}
	if !bytes.Equal(point.Bytes(), edPublic) {
		return nil, errors.New("non-canonical Ed25519 public key")
	}
	xPublic := point.BytesMontgomery()
	var zero [32]byte
	if bytes.Equal(xPublic, zero[:]) {
		return nil, errors.New("Ed25519 public key converts to an invalid X25519 key")
	}

	h := sha512.Sum512(seed)
	xBytes := append([]byte(nil), h[:32]...)
	xBytes[0] &= 248
	xBytes[31] &= 127
	xBytes[31] |= 64
	xPrivate, err := ecdh.X25519().NewPrivateKey(xBytes)
	if err != nil {
		return nil, fmt.Errorf("create X25519 private key: %w", err)
	}
	for i := range xBytes {
		xBytes[i] = 0
	}
	if !bytes.Equal(xPrivate.PublicKey().Bytes(), xPublic) {
		return nil, errors.New("Ed25519 to X25519 public key conversion mismatch")
	}

	sshPublic, err := ssh.NewPublicKey(edPublic)
	if err != nil {
		return nil, fmt.Errorf("create SSH public key: %w", err)
	}
	fingerprint := ssh.FingerprintSHA256(sshPublic)
	rawDigest := sha256.Sum256(sshPublic.Marshal())

	return &Identity{
		x25519Private:    xPrivate,
		recipient:        fingerprint,
		recipientPEMHash: rawDigest,
	}, nil
}

// recipientFingerprintRaw returns the raw SHA256 digest used by wrap AAD and
// wrap HKDF info.
func (i *Identity) recipientFingerprintRaw() []byte {
	return i.recipientPEMHash[:]
}

// fingerprintString formats a raw digest in the same notation used by OpenSSH.
func fingerprintString(raw []byte) string {
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(raw)
}
