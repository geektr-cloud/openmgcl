package tacos

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	cryptSuite = 1

	payloadMagic     = "MGCL"
	payloadVersion   = 1
	payloadSuite     = 1
	payloadHeaderLen = 4 + 1 + 1 + sha256.Size + 12
	aeadOverhead     = 16
)

var (
	keyFingerprintPrefix = []byte("openmgcl/lineage-key-fingerprint/v1\x00")
	wrapInfoLabel        = []byte("openmgcl/wrap/v1\x00")
	wrapAADLabel         = []byte("openmgcl/wrap-aad/v1\x00")
	payloadAADLabel      = []byte("openmgcl/state/v1\x00")
)

type wrappedKeyRecord struct {
	KeyFingerprint       string
	RecipientFingerprint string
	Suite                int
	EphemeralPublicKey   []byte
	HKDFSalt             []byte
	Nonce                []byte
	WrappedKey           []byte
}

func lineageKeyFingerprint(dek []byte) [sha256.Size]byte {
	h := sha256.New()
	h.Write(keyFingerprintPrefix)
	h.Write(dek)
	var fp [sha256.Size]byte
	copy(fp[:], h.Sum(nil))
	return fp
}

func parseKeyFingerprint(value string) ([sha256.Size]byte, error) {
	var fp [sha256.Size]byte
	const prefix = "SHA256:"
	if !strings.HasPrefix(value, prefix) {
		return fp, errors.New("invalid key fingerprint")
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(value, prefix))
	if err != nil || len(raw) != sha256.Size {
		return fp, errors.New("invalid key fingerprint")
	}
	copy(fp[:], raw)
	return fp, nil
}

// newWrappedLineageKey creates a random DEK and wraps it for the identity's
// converted X25519 public key.
func newWrappedLineageKey(identity *Identity, lineage string) ([]byte, *wrappedKeyRecord, error) {
	return newWrappedLineageKeyWithRand(identity, lineage, rand.Reader)
}

func newWrappedLineageKeyWithRand(identity *Identity, lineage string, random io.Reader) ([]byte, *wrappedKeyRecord, error) {
	dek := make([]byte, 32)
	if _, err := io.ReadFull(random, dek); err != nil {
		return nil, nil, fmt.Errorf("generate lineage key: %w", err)
	}
	fingerprint := lineageKeyFingerprint(dek)
	fingerprintText := fingerprintString(fingerprint[:])

	// Read the ephemeral scalar directly instead of using GenerateKey: newer Go
	// versions ignore the supplied reader unless GODEBUG=cryptocustomrand=1,
	// which makes deterministic wrapping tests (and the golden vector) unstable.
	// NewPrivateKey performs the same X25519 clamping as GenerateKey.
	ephemeralScalar := make([]byte, 32)
	if _, err := io.ReadFull(random, ephemeralScalar); err != nil {
		return nil, nil, fmt.Errorf("generate wrapping key: %w", err)
	}
	defer clearBytes(ephemeralScalar)
	ephemeral, err := ecdh.X25519().NewPrivateKey(ephemeralScalar)
	if err != nil {
		return nil, nil, fmt.Errorf("generate wrapping key: %w", err)
	}
	shared, err := ephemeral.ECDH(identity.x25519Private.PublicKey())
	if err != nil {
		return nil, nil, fmt.Errorf("derive wrapping secret: %w", err)
	}
	defer clearBytes(shared)

	salt := make([]byte, 32)
	if _, err := io.ReadFull(random, salt); err != nil {
		return nil, nil, fmt.Errorf("generate wrapping salt: %w", err)
	}
	kek, err := hkdf.Key(
		sha256.New,
		shared,
		salt,
		string(buildWrapInfo(lineage, ephemeral.PublicKey().Bytes(), identity.x25519Private.PublicKey().Bytes(), identity.recipientFingerprintRaw(), fingerprint[:])),
		32,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("derive wrapping key: %w", err)
	}
	defer clearBytes(kek)

	nonce := make([]byte, 12)
	if _, err := io.ReadFull(random, nonce); err != nil {
		return nil, nil, fmt.Errorf("generate wrapping nonce: %w", err)
	}
	wrapped, err := sealAEAD(kek, nonce, dek, buildWrapAAD(lineage, fingerprint[:], identity.recipientFingerprintRaw(), ephemeral.PublicKey().Bytes()))
	if err != nil {
		return nil, nil, fmt.Errorf("wrap lineage key: %w", err)
	}

	return dek, &wrappedKeyRecord{
		KeyFingerprint:       fingerprintText,
		RecipientFingerprint: identity.recipient,
		Suite:                cryptSuite,
		EphemeralPublicKey:   ephemeral.PublicKey().Bytes(),
		HKDFSalt:             salt,
		Nonce:                nonce,
		WrappedKey:           wrapped,
	}, nil
}

func validateWrappedKeyRecord(record *wrappedKeyRecord) ([sha256.Size]byte, error) {
	var fingerprint [sha256.Size]byte
	if record.Suite != cryptSuite {
		return fingerprint, errors.New("unsupported lineage key wrapping suite")
	}
	if len(record.EphemeralPublicKey) != 32 || len(record.HKDFSalt) != 32 || len(record.Nonce) != 12 || len(record.WrappedKey) != 32+aeadOverhead {
		return fingerprint, errors.New("invalid wrapped lineage key metadata")
	}
	fingerprint, err := parseKeyFingerprint(record.KeyFingerprint)
	if err != nil {
		return fingerprint, fmt.Errorf("invalid wrapped lineage key fingerprint: %w", err)
	}
	if _, err := parseKeyFingerprint(record.RecipientFingerprint); err != nil {
		return fingerprint, errors.New("invalid wrapped lineage key recipient fingerprint")
	}
	return fingerprint, nil
}

func unwrapLineageKey(identity *Identity, lineage string, record *wrappedKeyRecord) ([]byte, error) {
	fingerprint, err := validateWrappedKeyRecord(record)
	if err != nil {
		return nil, err
	}
	if record.RecipientFingerprint != identity.recipient {
		return nil, ErrForbidden("lineage state is not accessible with the configured recipient")
	}

	ephemeral, err := ecdh.X25519().NewPublicKey(record.EphemeralPublicKey)
	if err != nil {
		return nil, errors.New("invalid wrapped lineage key public key")
	}
	shared, err := identity.x25519Private.ECDH(ephemeral)
	if err != nil {
		return nil, fmt.Errorf("derive unwrapping secret: %w", err)
	}
	defer clearBytes(shared)

	kek, err := hkdf.Key(
		sha256.New,
		shared,
		record.HKDFSalt,
		string(buildWrapInfo(lineage, record.EphemeralPublicKey, identity.x25519Private.PublicKey().Bytes(), identity.recipientFingerprintRaw(), fingerprint[:])),
		32,
	)
	if err != nil {
		return nil, fmt.Errorf("derive unwrapping key: %w", err)
	}
	defer clearBytes(kek)

	dek, err := openAEAD(kek, record.Nonce, record.WrappedKey, buildWrapAAD(lineage, fingerprint[:], identity.recipientFingerprintRaw(), record.EphemeralPublicKey))
	if err != nil {
		return nil, errors.New("unwrap lineage key failed authentication")
	}
	if len(dek) != 32 {
		clearBytes(dek)
		return nil, errors.New("invalid lineage key length")
	}
	actual := lineageKeyFingerprint(dek)
	if !bytes.Equal(actual[:], fingerprint[:]) {
		clearBytes(dek)
		return nil, errors.New("lineage key fingerprint mismatch")
	}
	return dek, nil
}

func buildWrapInfo(lineage string, ephemeralPublic, recipientPublic, recipientFingerprint, keyFingerprint []byte) []byte {
	out := append([]byte(nil), wrapInfoLabel...)
	out = append(out, ephemeralPublic...)
	out = append(out, recipientPublic...)
	out = append(out, recipientFingerprint...)
	out = appendField(out, lineage)
	out = append(out, keyFingerprint...)
	return out
}

func buildWrapAAD(lineage string, keyFingerprint, recipientFingerprint, ephemeralPublic []byte) []byte {
	out := append([]byte(nil), wrapAADLabel...)
	out = appendField(out, lineage)
	out = append(out, keyFingerprint...)
	out = append(out, recipientFingerprint...)
	out = append(out, ephemeralPublic...)
	return out
}

// sealPayload encrypts one state_versions BLOB. It never accepts an empty
// output for present data: even zero-length plaintext gets a full envelope.
func sealPayload(dek []byte, fingerprint string, workspaceID, stateVersionID, lineage, fieldName string, plaintext []byte) ([]byte, error) {
	return sealPayloadWithRand(dek, fingerprint, workspaceID, stateVersionID, lineage, fieldName, plaintext, rand.Reader)
}

func sealPayloadWithRand(dek []byte, fingerprint string, workspaceID, stateVersionID, lineage, fieldName string, plaintext []byte, random io.Reader) ([]byte, error) {
	fp, err := parseKeyFingerprint(fingerprint)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, 12)
	if _, err := io.ReadFull(random, nonce); err != nil {
		return nil, fmt.Errorf("generate state nonce: %w", err)
	}

	out := make([]byte, payloadHeaderLen, payloadHeaderLen+len(plaintext)+aeadOverhead)
	copy(out[0:4], payloadMagic)
	out[4] = payloadVersion
	out[5] = payloadSuite
	copy(out[6:6+sha256.Size], fp[:])
	copy(out[6+sha256.Size:], nonce)

	aad := buildPayloadAAD(workspaceID, stateVersionID, lineage, fieldName, fp[:])
	sealed, err := sealAEAD(dek, nonce, plaintext, aad)
	if err != nil {
		return nil, err
	}
	return append(out, sealed...), nil
}

func openPayload(dek []byte, workspaceID, stateVersionID, lineage, fieldName string, data []byte) ([]byte, error) {
	if len(data) < payloadHeaderLen+aeadOverhead {
		return nil, errors.New("invalid encrypted state envelope")
	}
	if string(data[0:4]) != payloadMagic || data[4] != payloadVersion || data[5] != payloadSuite {
		return nil, errors.New("unsupported encrypted state format")
	}
	var fp [sha256.Size]byte
	copy(fp[:], data[6:6+sha256.Size])
	nonce := data[6+sha256.Size : payloadHeaderLen]
	ciphertext := data[payloadHeaderLen:]

	aad := buildPayloadAAD(workspaceID, stateVersionID, lineage, fieldName, fp[:])
	plaintext, err := openAEAD(dek, nonce, ciphertext, aad)
	if err != nil {
		return nil, errors.New("state decryption failed authentication")
	}
	return plaintext, nil
}

func payloadFingerprint(data []byte) ([sha256.Size]byte, error) {
	var fp [sha256.Size]byte
	if len(data) < payloadHeaderLen+aeadOverhead {
		return fp, errors.New("invalid encrypted state envelope")
	}
	if string(data[0:4]) != payloadMagic {
		return fp, errors.New("unsupported plaintext state; encryption is required")
	}
	if data[4] != payloadVersion || data[5] != payloadSuite {
		return fp, errors.New("unsupported encrypted state format")
	}
	copy(fp[:], data[6:6+sha256.Size])
	return fp, nil
}

func buildPayloadAAD(workspaceID, stateVersionID, lineage, fieldName string, keyFingerprint []byte) []byte {
	out := append([]byte(nil), payloadAADLabel...)
	out = appendField(out, workspaceID)
	out = appendField(out, stateVersionID)
	out = appendField(out, lineage)
	out = appendField(out, fieldName)
	out = append(out, keyFingerprint...)
	return out
}

func appendField(dst []byte, value string) []byte {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	dst = append(dst, length[:]...)
	return append(dst, value...)
}

func sealAEAD(key, nonce, plaintext, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return aead.Seal(nil, nonce, plaintext, aad), nil
}

func openAEAD(key, nonce, ciphertext, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return aead.Open(nil, nonce, ciphertext, aad)
}

func clearBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
