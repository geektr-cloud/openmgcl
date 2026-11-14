package tacos

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"testing"
)

func TestSecretDEKWrapRoundTripAndGolden(t *testing.T) {
	identity, err := newIdentity(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x42}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	dek, record, err := newWrappedSecretDEKWithRand(identity, "secret-golden", deterministicReader())
	if err != nil {
		t.Fatal(err)
	}
	unwrapped, err := unwrapSecretDEK(identity, "secret-golden", record)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dek, unwrapped) {
		t.Fatal("unwrapped Secret DEK differs")
	}
	fp := secretDEKFingerprint(dek)
	if got := fingerprintString(fp[:]); got != record.DEKFingerprint {
		t.Fatalf("fingerprint = %q, want %q", got, record.DEKFingerprint)
	}

	// Fixed seed, deterministic reader and independent secret labels must not
	// drift into the state/lineage envelope format.
	const want = "b0d08f35b4683381489afb32825e59152d47d19bc9e050d6d5a954984c9d1e2c:5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a:5a5a5a5a5a5a5a5a5a5a5a5a:e1999cece130e01171c673c15c47aafd6b71dcbe810a86f798ed724e2783543fcb9192c9804bf34aad01dfac5e4bd28d:SHA256:ZsrOVCtcb1bouzun0GIHz5vL5oCjVhVIQ3jfIBIgZ8g:SHA256:5tjoTMfOtPts9VFQUGKcU31rM0G7y6x3e+glh41nD+Y"
	got := hex.EncodeToString(record.EphemeralPublicKey) + ":" +
		hex.EncodeToString(record.HKDFSalt) + ":" +
		hex.EncodeToString(record.Nonce) + ":" +
		hex.EncodeToString(record.WrappedDEK) + ":" +
		record.RecipientFingerprint + ":" + record.DEKFingerprint
	if got != want {
		t.Fatalf("secret wrap golden changed:\n got %s\nwant %s", got, want)
	}
}

func TestSecretDEKWrongRecipientAndTampering(t *testing.T) {
	identity := testIdentity(t)
	_, record, err := newWrappedSecretDEKWithRand(identity, "secret-a", deterministicReader())
	if err != nil {
		t.Fatal(err)
	}

	_, otherPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	other, err := newIdentity(otherPrivate)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unwrapSecretDEK(other, "secret-a", record); !errors.Is(err, ErrSecretRecipientMismatch) {
		t.Fatalf("wrong recipient error = %v", err)
	}
	if _, err := unwrapSecretDEK(identity, "different-secret", record); !errors.Is(err, ErrSecretCorrupt) {
		t.Fatal("secret ID AAD mismatch was accepted")
	}

	mutations := map[string]func(*wrappedSecretDEKRecord){
		"suite":                 func(r *wrappedSecretDEKRecord) { r.Suite++ },
		"ephemeral public":      func(r *wrappedSecretDEKRecord) { r.EphemeralPublicKey[0] ^= 1 },
		"ephemeral length":      func(r *wrappedSecretDEKRecord) { r.EphemeralPublicKey = r.EphemeralPublicKey[:31] },
		"hkdf salt":             func(r *wrappedSecretDEKRecord) { r.HKDFSalt[0] ^= 1 },
		"salt length":           func(r *wrappedSecretDEKRecord) { r.HKDFSalt = r.HKDFSalt[:31] },
		"nonce":                 func(r *wrappedSecretDEKRecord) { r.Nonce[0] ^= 1 },
		"nonce length":          func(r *wrappedSecretDEKRecord) { r.Nonce = r.Nonce[:11] },
		"wrapped key":           func(r *wrappedSecretDEKRecord) { r.WrappedDEK[0] ^= 1 },
		"wrapped key length":    func(r *wrappedSecretDEKRecord) { r.WrappedDEK = r.WrappedDEK[:47] },
		"key fingerprint":       func(r *wrappedSecretDEKRecord) { r.DEKFingerprint = fingerprintString(bytes.Repeat([]byte{1}, 32)) },
		"recipient fingerprint": func(r *wrappedSecretDEKRecord) { r.RecipientFingerprint = "SHA256:changed" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			copyRecord := *record
			copyRecord.EphemeralPublicKey = append([]byte(nil), record.EphemeralPublicKey...)
			copyRecord.HKDFSalt = append([]byte(nil), record.HKDFSalt...)
			copyRecord.Nonce = append([]byte(nil), record.Nonce...)
			copyRecord.WrappedDEK = append([]byte(nil), record.WrappedDEK...)
			mutate(&copyRecord)
			if _, err := unwrapSecretDEK(identity, "secret-a", &copyRecord); err == nil {
				t.Fatal("tampered secret key record was accepted")
			}
		})
	}
}

func TestSecretValueRoundTripAADAndNonce(t *testing.T) {
	dek := bytes.Repeat([]byte{0x11}, 32)
	const secretID, key, plaintext = "secret-id", "api_key", "hello\nworld"

	nonce, ciphertext, err := sealSecretValueWithRand(dek, secretID, key, []byte(plaintext), deterministicReader())
	if err != nil {
		t.Fatal(err)
	}
	if len(nonce) != 12 || len(ciphertext) != len(plaintext)+aeadOverhead {
		t.Fatalf("nonce/ciphertext lengths = %d/%d", len(nonce), len(ciphertext))
	}
	got, err := openSecretValue(dek, secretID, key, nonce, ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != plaintext {
		t.Fatalf("decrypted = %q", got)
	}

	secondNonce, secondCiphertext, err := sealSecretValueWithRand(dek, secretID, key, []byte(plaintext), deterministicReaderByte(0x6b))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(nonce, secondNonce) || bytes.Equal(ciphertext, secondCiphertext) {
		t.Fatal("two encryptions reused nonce/ciphertext")
	}

	cases := map[string]func() ([]byte, error){
		"secret id": func() ([]byte, error) { return openSecretValue(dek, "other", key, nonce, ciphertext) },
		"key":       func() ([]byte, error) { return openSecretValue(dek, secretID, "other", nonce, ciphertext) },
		"dek": func() ([]byte, error) {
			return openSecretValue(bytes.Repeat([]byte{0x22}, 32), secretID, key, nonce, ciphertext)
		},
		"nonce": func() ([]byte, error) {
			changed := append([]byte(nil), nonce...)
			changed[0] ^= 1
			return openSecretValue(dek, secretID, key, changed, ciphertext)
		},
		"ciphertext": func() ([]byte, error) {
			changed := append([]byte(nil), ciphertext...)
			changed[len(changed)-1] ^= 1
			return openSecretValue(dek, secretID, key, nonce, changed)
		},
		"short nonce":      func() ([]byte, error) { return openSecretValue(dek, secretID, key, nonce[:11], ciphertext) },
		"short ciphertext": func() ([]byte, error) { return openSecretValue(dek, secretID, key, nonce, ciphertext[:15]) },
	}
	for name, open := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := open(); err == nil {
				t.Fatal("AAD/corruption mismatch was accepted")
			}
		})
	}
}

func TestSecretValueEmptyPlaintext(t *testing.T) {
	dek := bytes.Repeat([]byte{0x11}, 32)
	nonce, ciphertext, err := sealSecretValueWithRand(dek, "id", "key", []byte{}, deterministicReader())
	if err != nil {
		t.Fatal(err)
	}
	if len(ciphertext) != aeadOverhead {
		t.Fatalf("empty ciphertext length = %d", len(ciphertext))
	}
	got, err := openSecretValue(dek, "id", "key", nonce, ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("empty plaintext = %#v", got)
	}
}

func TestSecretValueGoldenVector(t *testing.T) {
	dek := make([]byte, 32)
	for i := range dek {
		dek[i] = byte(i)
	}
	random := bytes.NewReader(bytes.Repeat([]byte{0x2a}, 12))
	nonce, ciphertext, err := sealSecretValueWithRand(dek, "secret-id", "key", []byte("golden"), random)
	if err != nil {
		t.Fatal(err)
	}
	const wantNonce = "2a2a2a2a2a2a2a2a2a2a2a2a"
	const wantCiphertext = "fa506bf6c7e258b43f82e1863b934621b83c5b2dfc53"
	if got := hex.EncodeToString(nonce); got != wantNonce {
		t.Fatalf("nonce = %s", got)
	}
	if got := hex.EncodeToString(ciphertext); got != wantCiphertext {
		t.Fatalf("ciphertext = %s", got)
	}
}
