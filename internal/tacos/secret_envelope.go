package tacos

import (
	"bytes"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
)

// Secret encryption reuses the Ed25519 -> X25519, ECDH, HKDF-SHA-256 and
// AES-256-GCM construction from state-encryption.md, but every label, AAD and
// fingerprint domain is independent from the state/lineage suite.
var (
	secretDEKFingerprintPrefix = []byte("openmgcl/secret-dek-fingerprint/v1\x00")
	secretWrapInfoLabel        = []byte("openmgcl/secret-wrap/v1\x00")
	secretWrapAADLabel         = []byte("openmgcl/secret-wrap-aad/v1\x00")
	secretValueAADLabel        = []byte("openmgcl/secret-value/v1\x00")
)

type wrappedSecretDEKRecord struct {
	DEKFingerprint       string
	RecipientFingerprint string
	Suite                int
	EphemeralPublicKey   []byte
	HKDFSalt             []byte
	Nonce                []byte
	WrappedDEK           []byte
}

func secretDEKFingerprint(dek []byte) [sha256.Size]byte {
	h := sha256.New()
	h.Write(secretDEKFingerprintPrefix)
	h.Write(dek)
	var fp [sha256.Size]byte
	copy(fp[:], h.Sum(nil))
	return fp
}

// newWrappedSecretDEK creates a random Secret DEK and wraps it for the current
// recipient.
func newWrappedSecretDEK(identity *Identity, secretID string) ([]byte, *wrappedSecretDEKRecord, error) {
	return newWrappedSecretDEKWithRand(identity, secretID, rand.Reader)
}

func newWrappedSecretDEKWithRand(identity *Identity, secretID string, random io.Reader) ([]byte, *wrappedSecretDEKRecord, error) {
	dek := make([]byte, 32)
	if _, err := io.ReadFull(random, dek); err != nil {
		return nil, nil, fmt.Errorf("generate secret key: %w", err)
	}
	fingerprint := secretDEKFingerprint(dek)

	// Read the ephemeral scalar directly for deterministic golden tests; see
	// newWrappedLineageKeyWithRand for why GenerateKey is not used.
	ephemeralScalar := make([]byte, 32)
	if _, err := io.ReadFull(random, ephemeralScalar); err != nil {
		clearBytes(dek)
		return nil, nil, fmt.Errorf("generate secret wrapping key: %w", err)
	}
	defer clearBytes(ephemeralScalar)
	ephemeral, err := ecdh.X25519().NewPrivateKey(ephemeralScalar)
	if err != nil {
		clearBytes(dek)
		return nil, nil, fmt.Errorf("generate secret wrapping key: %w", err)
	}
	shared, err := ephemeral.ECDH(identity.x25519Private.PublicKey())
	if err != nil {
		clearBytes(dek)
		return nil, nil, fmt.Errorf("derive secret wrapping secret: %w", err)
	}
	defer clearBytes(shared)

	salt := make([]byte, 32)
	if _, err := io.ReadFull(random, salt); err != nil {
		clearBytes(dek)
		return nil, nil, fmt.Errorf("generate secret wrapping salt: %w", err)
	}
	kek, err := hkdf.Key(
		sha256.New,
		shared,
		salt,
		string(buildSecretWrapInfo(secretID, ephemeral.PublicKey().Bytes(), identity.x25519Private.PublicKey().Bytes(), identity.recipientFingerprintRaw(), fingerprint[:])),
		32,
	)
	if err != nil {
		clearBytes(dek)
		return nil, nil, fmt.Errorf("derive secret wrapping key: %w", err)
	}
	defer clearBytes(kek)

	nonce := make([]byte, 12)
	if _, err := io.ReadFull(random, nonce); err != nil {
		clearBytes(dek)
		return nil, nil, fmt.Errorf("generate secret wrapping nonce: %w", err)
	}
	wrapped, err := sealAEAD(kek, nonce, dek, buildSecretWrapAAD(secretID, fingerprint[:], identity.recipientFingerprintRaw(), ephemeral.PublicKey().Bytes()))
	if err != nil {
		clearBytes(dek)
		return nil, nil, fmt.Errorf("wrap secret key: %w", err)
	}

	return dek, &wrappedSecretDEKRecord{
		DEKFingerprint:       fingerprintString(fingerprint[:]),
		RecipientFingerprint: identity.recipient,
		Suite:                cryptSuite,
		EphemeralPublicKey:   ephemeral.PublicKey().Bytes(),
		HKDFSalt:             salt,
		Nonce:                nonce,
		WrappedDEK:           wrapped,
	}, nil
}

func validateWrappedSecretDEKRecord(record *wrappedSecretDEKRecord) ([sha256.Size]byte, error) {
	var fingerprint [sha256.Size]byte
	if record.Suite != cryptSuite {
		return fingerprint, errors.New("unsupported secret key wrapping suite")
	}
	if len(record.EphemeralPublicKey) != 32 ||
		len(record.HKDFSalt) != 32 ||
		len(record.Nonce) != 12 ||
		len(record.WrappedDEK) != 32+aeadOverhead {
		return fingerprint, errors.New("invalid wrapped secret key metadata")
	}
	fingerprint, err := parseKeyFingerprint(record.DEKFingerprint)
	if err != nil {
		return fingerprint, fmt.Errorf("invalid secret key fingerprint: %w", err)
	}
	if _, err := parseKeyFingerprint(record.RecipientFingerprint); err != nil {
		return fingerprint, errors.New("invalid secret key recipient fingerprint")
	}
	return fingerprint, nil
}

func unwrapSecretDEK(identity *Identity, secretID string, record *wrappedSecretDEKRecord) ([]byte, error) {
	fingerprint, err := validateWrappedSecretDEKRecord(record)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSecretCorrupt, err)
	}
	if record.RecipientFingerprint != identity.recipient {
		return nil, ErrSecretRecipientMismatch
	}

	ephemeral, err := ecdh.X25519().NewPublicKey(record.EphemeralPublicKey)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid ephemeral public key", ErrSecretCorrupt)
	}
	shared, err := identity.x25519Private.ECDH(ephemeral)
	if err != nil {
		return nil, fmt.Errorf("%w: derive secret unwrapping key", ErrSecretCorrupt)
	}
	defer clearBytes(shared)

	kek, err := hkdf.Key(
		sha256.New,
		shared,
		record.HKDFSalt,
		string(buildSecretWrapInfo(secretID, record.EphemeralPublicKey, identity.x25519Private.PublicKey().Bytes(), identity.recipientFingerprintRaw(), fingerprint[:])),
		32,
	)
	if err != nil {
		return nil, fmt.Errorf("%w: derive secret unwrapping key", ErrSecretCorrupt)
	}
	defer clearBytes(kek)

	dek, err := openAEAD(kek, record.Nonce, record.WrappedDEK, buildSecretWrapAAD(secretID, fingerprint[:], identity.recipientFingerprintRaw(), record.EphemeralPublicKey))
	if err != nil {
		return nil, fmt.Errorf("%w: secret key authentication failed", ErrSecretCorrupt)
	}
	if len(dek) != 32 {
		clearBytes(dek)
		return nil, fmt.Errorf("%w: invalid secret key length", ErrSecretCorrupt)
	}
	actual := secretDEKFingerprint(dek)
	if !bytes.Equal(actual[:], fingerprint[:]) {
		clearBytes(dek)
		return nil, fmt.Errorf("%w: secret key fingerprint mismatch", ErrSecretCorrupt)
	}
	return dek, nil
}

func buildSecretWrapInfo(secretID string, ephemeralPublic, recipientPublic, recipientFingerprint, dekFingerprint []byte) []byte {
	out := append([]byte(nil), secretWrapInfoLabel...)
	out = append(out, ephemeralPublic...)
	out = append(out, recipientPublic...)
	out = append(out, recipientFingerprint...)
	out = appendField(out, secretID)
	out = append(out, dekFingerprint...)
	return out
}

func buildSecretWrapAAD(secretID string, dekFingerprint, recipientFingerprint, ephemeralPublic []byte) []byte {
	out := append([]byte(nil), secretWrapAADLabel...)
	out = appendField(out, secretID)
	out = append(out, dekFingerprint...)
	out = append(out, recipientFingerprint...)
	out = append(out, ephemeralPublic...)
	return out
}

// sealSecretValue encrypts one KV value with the Secret DEK and a fresh nonce.
func sealSecretValue(dek []byte, secretID, key string, plaintext []byte) (nonce, ciphertext []byte, err error) {
	return sealSecretValueWithRand(dek, secretID, key, plaintext, rand.Reader)
}

func sealSecretValueWithRand(dek []byte, secretID, key string, plaintext []byte, random io.Reader) (nonce, ciphertext []byte, err error) {
	nonce = make([]byte, 12)
	if _, err := io.ReadFull(random, nonce); err != nil {
		return nil, nil, fmt.Errorf("generate secret value nonce: %w", err)
	}
	ciphertext, err = sealAEAD(dek, nonce, plaintext, buildSecretValueAAD(secretID, key))
	if err != nil {
		return nil, nil, fmt.Errorf("encrypt secret value: %w", err)
	}
	return nonce, ciphertext, nil
}

func openSecretValue(dek []byte, secretID, key string, nonce, ciphertext []byte) ([]byte, error) {
	if len(nonce) != 12 || len(ciphertext) < aeadOverhead {
		return nil, fmt.Errorf("%w: invalid secret value envelope", ErrSecretCorrupt)
	}
	plaintext, err := openAEAD(dek, nonce, ciphertext, buildSecretValueAAD(secretID, key))
	if err != nil {
		return nil, fmt.Errorf("%w: secret value authentication failed", ErrSecretCorrupt)
	}
	return plaintext, nil
}

func buildSecretValueAAD(secretID, key string) []byte {
	out := append([]byte(nil), secretValueAADLabel...)
	out = appendField(out, secretID)
	out = appendField(out, key)
	out = appendField(out, "v1")
	return out
}
