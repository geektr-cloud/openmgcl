package tacos

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"testing"
)

func deterministicReader() *bytes.Reader {
	return deterministicReaderByte(0x5a)
}

func deterministicReaderByte(value byte) *bytes.Reader {
	return bytes.NewReader(bytes.Repeat([]byte{value}, 1024))
}

func testKeyFingerprint(dek []byte) string {
	fp := lineageKeyFingerprint(dek)
	return fingerprintString(fp[:])
}

func testKeyFingerprintRaw(dek []byte) []byte {
	fp := lineageKeyFingerprint(dek)
	return fp[:]
}

func TestLineageKeyWrapRoundTrip(t *testing.T) {
	identity := testIdentity(t)
	dek, record, err := newWrappedLineageKeyWithRand(identity, "lineage-a", deterministicReader())
	if err != nil {
		t.Fatal(err)
	}
	unwrapped, err := unwrapLineageKey(identity, "lineage-a", record)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dek, unwrapped) {
		t.Fatal("unwrapped DEK differs")
	}
	if got := testKeyFingerprint(dek); got != record.KeyFingerprint {
		t.Fatalf("fingerprint = %q, want %q", got, record.KeyFingerprint)
	}
}

func TestLineageKeyWrapGoldenVector(t *testing.T) {
	identity, err := newIdentity(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x42}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	_, record, err := newWrappedLineageKeyWithRand(identity, "lineage-golden", deterministicReader())
	if err != nil {
		t.Fatal(err)
	}
	const want = "b0d08f35b4683381489afb32825e59152d47d19bc9e050d6d5a954984c9d1e2c:5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a:5a5a5a5a5a5a5a5a5a5a5a5a:1f6b5d97b6b2fbd2ed17d389b8d91e6078e9ab36568c972a4443cc00ca982ac3166cdcdfb94f6d918a0b394b10197614:SHA256:ZsrOVCtcb1bouzun0GIHz5vL5oCjVhVIQ3jfIBIgZ8g:SHA256:UGM9U3KlV4Lc+X4DVLyVo2SjtNxarBRVb08FGWSvs0Q"
	got := hex.EncodeToString(record.EphemeralPublicKey) + ":" +
		hex.EncodeToString(record.HKDFSalt) + ":" +
		hex.EncodeToString(record.Nonce) + ":" +
		hex.EncodeToString(record.WrappedKey) + ":" +
		record.RecipientFingerprint + ":" + record.KeyFingerprint
	if got != want {
		t.Fatalf("wrap golden changed:\n got %s\nwant %s", got, want)
	}
}

func TestLineageKeyWrongRecipient(t *testing.T) {
	identity := testIdentity(t)
	_, record, err := newWrappedLineageKeyWithRand(identity, "lineage-a", deterministicReader())
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
	_, err = unwrapLineageKey(other, "lineage-a", record)
	if !IsAPIError(err, http.StatusForbidden) {
		t.Fatalf("wrong recipient error = %v, want 403", err)
	}
}

func TestLineageKeyTampering(t *testing.T) {
	identity := testIdentity(t)
	_, record, err := newWrappedLineageKeyWithRand(identity, "lineage-a", deterministicReader())
	if err != nil {
		t.Fatal(err)
	}

	mutations := map[string]func(*wrappedKeyRecord){
		"suite":                 func(r *wrappedKeyRecord) { r.Suite++ },
		"ephemeral public":      func(r *wrappedKeyRecord) { r.EphemeralPublicKey[0] ^= 1 },
		"hkdf salt":             func(r *wrappedKeyRecord) { r.HKDFSalt[0] ^= 1 },
		"nonce":                 func(r *wrappedKeyRecord) { r.Nonce[0] ^= 1 },
		"wrapped key":           func(r *wrappedKeyRecord) { r.WrappedKey[0] ^= 1 },
		"key fingerprint":       func(r *wrappedKeyRecord) { r.KeyFingerprint = fingerprintString(bytes.Repeat([]byte{1}, 32)) },
		"recipient fingerprint": func(r *wrappedKeyRecord) { r.RecipientFingerprint = "SHA256:changed" },
	}
	if _, err := unwrapLineageKey(identity, "different-lineage", record); err == nil {
		t.Fatal("lineage AAD mismatch was accepted")
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			copyRecord := *record
			copyRecord.EphemeralPublicKey = append([]byte(nil), record.EphemeralPublicKey...)
			copyRecord.HKDFSalt = append([]byte(nil), record.HKDFSalt...)
			copyRecord.Nonce = append([]byte(nil), record.Nonce...)
			copyRecord.WrappedKey = append([]byte(nil), record.WrappedKey...)
			mutate(&copyRecord)
			if _, err := unwrapLineageKey(identity, "lineage-a", &copyRecord); err == nil {
				t.Fatal("tampered record was accepted")
			}
		})
	}
}

func TestPayloadRoundTripAndAAD(t *testing.T) {
	dek := bytes.Repeat([]byte{0x11}, 32)
	fingerprint := testKeyFingerprint(dek)
	const (
		wsID      = "ws-test"
		svID      = "sv-test"
		lineage   = "lineage-test"
		field     = "state"
		plaintext = `{"version":4,"serial":1}`
	)

	encrypted, err := sealPayloadWithRand(dek, fingerprint, wsID, svID, lineage, field, []byte(plaintext), deterministicReader())
	if err != nil {
		t.Fatal(err)
	}
	if len(encrypted) != payloadHeaderLen+len(plaintext)+aeadOverhead {
		t.Fatalf("envelope length = %d", len(encrypted))
	}
	decrypted, err := openPayload(dek, wsID, svID, lineage, field, encrypted)
	if err != nil {
		t.Fatal(err)
	}
	if string(decrypted) != plaintext {
		t.Fatalf("decrypted = %q, want %q", decrypted, plaintext)
	}

	second, err := sealPayloadWithRand(dek, fingerprint, wsID, svID, lineage, field, []byte(plaintext), deterministicReaderByte(0x6b))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(encrypted, second) {
		t.Fatal("two encryptions reused identical nonce/ciphertext")
	}

	cases := []struct {
		name string
		open func() ([]byte, error)
	}{
		{"workspace", func() ([]byte, error) { return openPayload(dek, "other", svID, lineage, field, encrypted) }},
		{"state version", func() ([]byte, error) { return openPayload(dek, wsID, "other", lineage, field, encrypted) }},
		{"lineage", func() ([]byte, error) { return openPayload(dek, wsID, svID, "other", field, encrypted) }},
		{"field", func() ([]byte, error) { return openPayload(dek, wsID, svID, lineage, "json_state", encrypted) }},
		{"key", func() ([]byte, error) {
			return openPayload(bytes.Repeat([]byte{0x22}, 32), wsID, svID, lineage, field, encrypted)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.open(); err == nil {
				t.Fatal("AAD mismatch was accepted")
			}
		})
	}
}

func TestPayloadRejectsPlaintextAndMalformedData(t *testing.T) {
	dek := bytes.Repeat([]byte{0x11}, 32)
	fingerprint := testKeyFingerprint(dek)
	encrypted, err := sealPayloadWithRand(dek, fingerprint, "ws", "sv", "lineage", "state", []byte("hello"), deterministicReader())
	if err != nil {
		t.Fatal(err)
	}

	for name, data := range map[string][]byte{
		"plaintext":   []byte(`{"version":4}`),
		"short":       []byte("MGCL"),
		"bad magic":   append([]byte("XXXX"), encrypted[4:]...),
		"bad version": append(append([]byte(nil), encrypted[:4]...), append([]byte{9}, encrypted[5:]...)...),
		"bad suite":   append(append([]byte(nil), encrypted[:5]...), append([]byte{9}, encrypted[6:]...)...),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := payloadFingerprint(data); err == nil {
				t.Fatal("malformed envelope was accepted")
			}
		})
	}

	tampered := append([]byte(nil), encrypted...)
	tampered[len(tampered)-1] ^= 1
	if _, err := openPayload(dek, "ws", "sv", "lineage", "state", tampered); err == nil {
		t.Fatal("tampered ciphertext was accepted")
	}

	tamperedFingerprint := append([]byte(nil), encrypted...)
	tamperedFingerprint[6] ^= 1
	if _, err := openPayload(dek, "ws", "sv", "lineage", "state", tamperedFingerprint); err == nil {
		t.Fatal("tampered envelope fingerprint was accepted")
	}
}

func TestPayloadEmptyPlaintext(t *testing.T) {
	dek := bytes.Repeat([]byte{0x11}, 32)
	fingerprint := testKeyFingerprint(dek)
	encrypted, err := sealPayloadWithRand(dek, fingerprint, "ws", "sv", "lineage", "state", []byte{}, deterministicReader())
	if err != nil {
		t.Fatal(err)
	}
	if len(encrypted) != payloadHeaderLen+aeadOverhead {
		t.Fatalf("empty envelope length = %d, want %d", len(encrypted), payloadHeaderLen+aeadOverhead)
	}
	plaintext, err := openPayload(dek, "ws", "sv", "lineage", "state", encrypted)
	if err != nil {
		t.Fatal(err)
	}
	if len(plaintext) != 0 {
		t.Fatalf("empty plaintext = %#v", plaintext)
	}
}

func TestPayloadGoldenVector(t *testing.T) {
	dek := make([]byte, 32)
	for i := range dek {
		dek[i] = byte(i)
	}
	fingerprint := testKeyFingerprint(dek)
	random := bytes.NewReader(append(bytes.Repeat([]byte{0x00}, 10), bytes.Repeat([]byte{0x2a}, 12)...))
	encrypted, err := sealPayloadWithRand(dek, fingerprint, "ws", "sv", "lineage", "state", []byte("golden"), random)
	if err != nil {
		t.Fatal(err)
	}

	const want = "4d47434c01010ac1a8e9393773a9f6adac94179ef5101dff39f193bfc0d8ba1906164cccf19d000000000000000000002a2a6774eff2a342f443ce9629865784e5c7464120c57ba3"
	if got := hex.EncodeToString(encrypted); got != want {
		t.Fatalf("golden envelope changed:\n got %s\nwant %s", got, want)
	}
}

func TestPayloadFingerprintMatchesHeader(t *testing.T) {
	dek := bytes.Repeat([]byte{0x11}, 32)
	fingerprint := testKeyFingerprint(dek)
	encrypted, err := sealPayloadWithRand(dek, fingerprint, "ws", "sv", "lineage", "state", []byte("hello"), deterministicReader())
	if err != nil {
		t.Fatal(err)
	}
	fp, err := payloadFingerprint(encrypted)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fp[:], testKeyFingerprintRaw(dek)) {
		t.Fatal("header fingerprint differs from DEK fingerprint")
	}
}
