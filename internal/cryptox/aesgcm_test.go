package cryptox

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"testing"
)

// errReader is a fake io.Reader that always returns an error.
// Used to exercise the random-reader-failure path in encryptAESGCM.
type errReader struct{}

func (errReader) Read([]byte) (int, error) {
	return 0, errors.New("random source failed")
}

// fixedReader is a fake io.Reader that returns a deterministic sequence of bytes.
// Used to inject a known nonce in KAT tests without relying on crypto/rand.
type fixedReader struct{ data []byte }

func (r *fixedReader) Read(p []byte) (int, error) {
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

// testKey returns a 32-byte key where every byte equals seed.
func testKey(seed byte) []byte {
	return bytes.Repeat([]byte{seed}, 32)
}

// testKeyB64 returns the base64-encoded form of testKey(seed).
func testKeyB64(seed byte) string {
	return base64.StdEncoding.EncodeToString(testKey(seed))
}

// mustDecodeHex panics if s is not valid hex – used only in test initialisers.
func mustDecodeHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic("mustDecodeHex: " + err.Error())
	}
	return b
}

// ---------------------------------------------------------------------------
// KeyFromB64
// ---------------------------------------------------------------------------

func TestKeyFromB64AcceptsThirtyTwoByteKey(t *testing.T) {
	key, err := KeyFromB64(testKeyB64(0x42))
	if err != nil {
		t.Fatalf("KeyFromB64 returned error: %v", err)
	}
	if !bytes.Equal(key, testKey(0x42)) {
		t.Fatalf("decoded key mismatch")
	}
}

func TestKeyFromB64RejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "empty", value: ""},
		{name: "invalid base64", value: "not base64!"},
		{name: "short key", value: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x11}, 16))},
		{name: "long key", value: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x22}, 33))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := KeyFromB64(tt.value); err == nil {
				t.Fatalf("KeyFromB64(%q) succeeded, want error", tt.name)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Known-Answer Test (KAT) vectors
//
// These vectors lock the output of AES-256-GCM for specific (key, nonce, plaintext)
// triples. If the algorithm, key schedule, or output framing ever changes
// accidentally, these tests will catch it.
//
// Vectors were generated once with Go's standard crypto/aes + crypto/cipher.NewGCM
// and committed as ground truth. The blob format is: 12-byte nonce || GCM ciphertext
// (ciphertext already includes the 16-byte authentication tag appended by Seal).
//
// To regenerate: see /tmp/genkat.go in the repository history, or reproduce with:
//
//	block, _ := aes.NewCipher(key)
//	gcm, _ := cipher.NewGCM(block)
//	blob := append(nonce, gcm.Seal(nil, nonce, plaintext, nil)...)
// ---------------------------------------------------------------------------

// katVector describes one known-answer test case for AES-256-GCM.
type katVector struct {
	name      string
	keyHex    string // 64 hex chars = 32 bytes
	nonceHex  string // 24 hex chars = 12 bytes (injected via fixedReader)
	plaintext []byte
	blobHex   string // expected output of encryptAESGCM: nonce || ciphertext+tag
}

// aesgcmKATVectors contains the committed known-answer test vectors.
// Do NOT edit blobHex values – their purpose is to detect drift.
var aesgcmKATVectors = []katVector{
	{
		// Vector 1: all-zero key and nonce, empty plaintext.
		// Only the 16-byte GCM authentication tag is produced.
		name:      "all-zeros key+nonce, empty plaintext",
		keyHex:    "0000000000000000000000000000000000000000000000000000000000000000",
		nonceHex:  "000000000000000000000000",
		plaintext: []byte{},
		blobHex:   "000000000000000000000000530f8afbc74536b9a963b4f1c4cb738b",
	},
	{
		// Vector 2: sequential key and nonce bytes, ASCII plaintext.
		name:      "sequential key+nonce, ASCII plaintext",
		keyHex:    "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f",
		nonceHex:  "0102030405060708090a0b0c",
		plaintext: []byte("grainlify-token-test"),
		blobHex:   "0102030405060708090a0b0c62983bbc82f899e0358f17287b7684053626928af2cd006f7f2945da6462aab9fbecfe8d",
	},
	{
		// Vector 3: all-0xff key, repeated 0xab nonce, ASCII plaintext.
		name:      "all-0xff key, repeated 0xab nonce, ASCII plaintext",
		keyHex:    "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		nonceHex:  "abababababababababababab",
		plaintext: []byte("sensitive oauth token"),
		blobHex:   "ababababababababababababf0fdab1e17aba7d9f630d311710e93487889b1443d6df06cecb55979f385ea1180a23f4afe",
	},
}

// TestEncryptAESGCMKnownAnswerVectors verifies that encryptAESGCM produces exactly
// the expected blob for each committed vector. A failure here means the algorithm,
// key schedule, or output format has changed unexpectedly.
func TestEncryptAESGCMKnownAnswerVectors(t *testing.T) {
	for _, v := range aesgcmKATVectors {
		v := v
		t.Run(v.name, func(t *testing.T) {
			key := mustDecodeHex(v.keyHex)
			nonce := mustDecodeHex(v.nonceHex)
			want := mustDecodeHex(v.blobHex)

			rdr := &fixedReader{data: nonce}
			got, err := encryptAESGCM(key, v.plaintext, rdr)
			if err != nil {
				t.Fatalf("encryptAESGCM error: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("blob mismatch\n got:  %x\n want: %x", got, want)
			}
		})
	}
}

// TestDecryptAESGCMKnownAnswerVectors verifies that DecryptAESGCM recovers the
// original plaintext from each committed blob. This is the inverse of the encrypt
// KAT and guards against regressions in the decrypt path.
func TestDecryptAESGCMKnownAnswerVectors(t *testing.T) {
	for _, v := range aesgcmKATVectors {
		v := v
		t.Run(v.name, func(t *testing.T) {
			key := mustDecodeHex(v.keyHex)
			blob := mustDecodeHex(v.blobHex)

			got, err := DecryptAESGCM(key, blob)
			if err != nil {
				t.Fatalf("DecryptAESGCM error: %v", err)
			}
			if !bytes.Equal(got, v.plaintext) {
				t.Errorf("plaintext mismatch\n got:  %q\n want: %q", got, v.plaintext)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Round-trip tests
// ---------------------------------------------------------------------------

func TestEncryptDecryptAESGCMRoundTrip(t *testing.T) {
	key := testKey(0x33)
	plaintexts := [][]byte{
		nil,
		[]byte("stored github token"),
		bytes.Repeat([]byte("larger plaintext block "), 128),
	}

	for _, plaintext := range plaintexts {
		ciphertext, err := EncryptAESGCM(key, plaintext)
		if err != nil {
			t.Fatalf("EncryptAESGCM returned error: %v", err)
		}

		decrypted, err := DecryptAESGCM(key, ciphertext)
		if err != nil {
			t.Fatalf("DecryptAESGCM returned error: %v", err)
		}
		if !bytes.Equal(decrypted, plaintext) {
			t.Fatalf("decrypted plaintext mismatch: got %q want %q", decrypted, plaintext)
		}
	}
}

// TestEncryptDecryptAESGCMEmptyPlaintext confirms that a nil and a zero-length slice
// both round-trip without error (the ciphertext in both cases is just the 16-byte tag).
func TestEncryptDecryptAESGCMEmptyPlaintext(t *testing.T) {
	key := testKey(0x01)
	for _, pt := range [][]byte{nil, {}} {
		ct, err := EncryptAESGCM(key, pt)
		if err != nil {
			t.Fatalf("EncryptAESGCM(%v) error: %v", pt, err)
		}
		// blob must be at least nonce (12) + tag (16) = 28 bytes
		if len(ct) < 28 {
			t.Fatalf("ciphertext too short for empty plaintext: got %d bytes", len(ct))
		}
		got, err := DecryptAESGCM(key, ct)
		if err != nil {
			t.Fatalf("DecryptAESGCM error: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("expected empty plaintext, got %q", got)
		}
	}
}

// ---------------------------------------------------------------------------
// Nonce uniqueness / randomness
// ---------------------------------------------------------------------------

// TestEncryptAESGCMUsesRandomNonce is a basic sanity check: two encryptions of the
// same plaintext must produce different blobs (and therefore different nonces).
func TestEncryptAESGCMUsesRandomNonce(t *testing.T) {
	key := testKey(0x44)
	plaintext := []byte("same plaintext")

	first, err := EncryptAESGCM(key, plaintext)
	if err != nil {
		t.Fatalf("first EncryptAESGCM returned error: %v", err)
	}
	second, err := EncryptAESGCM(key, plaintext)
	if err != nil {
		t.Fatalf("second EncryptAESGCM returned error: %v", err)
	}
	if bytes.Equal(first, second) {
		t.Fatalf("two encryptions of the same plaintext produced identical blobs")
	}

	for _, ciphertext := range [][]byte{first, second} {
		decrypted, err := DecryptAESGCM(key, ciphertext)
		if err != nil {
			t.Fatalf("DecryptAESGCM returned error: %v", err)
		}
		if !bytes.Equal(decrypted, plaintext) {
			t.Fatalf("decrypted plaintext mismatch: got %q want %q", decrypted, plaintext)
		}
	}
}

// TestEncryptAESGCMNonceUniquenessStatistical performs a statistical uniqueness check
// over a large number of encryptions. It confirms that:
//  1. No two nonces collide across n encryptions.
//  2. Not all nonce bytes are identical (catches a stuck/zero RNG).
//
// This is not a proof of CSPRNG quality, but it will catch any trivial regression
// such as a counter that wraps, a zeroed nonce, or a repeated seed.
//
// With a 96-bit (12-byte) nonce from crypto/rand the birthday-bound collision
// probability over 10 000 samples is approximately 2^(-96) * C(10000,2) ≈ 6×10⁻²⁴,
// which is negligible. Any collision here indicates a broken RNG source.
func TestEncryptAESGCMNonceUniquenessStatistical(t *testing.T) {
	const n = 10_000
	const nonceSize = 12 // AES-GCM standard nonce length

	key := testKey(0xcc)
	plaintext := []byte("nonce uniqueness probe")

	seen := make(map[[nonceSize]byte]struct{}, n)
	allIdentical := true // tracks whether every nonce byte is the same value
	var firstNonce [nonceSize]byte

	for i := 0; i < n; i++ {
		blob, err := EncryptAESGCM(key, plaintext)
		if err != nil {
			t.Fatalf("EncryptAESGCM error on iteration %d: %v", i, err)
		}
		if len(blob) < nonceSize {
			t.Fatalf("blob too short on iteration %d: %d bytes", i, len(blob))
		}

		var nonce [nonceSize]byte
		copy(nonce[:], blob[:nonceSize])

		if i == 0 {
			firstNonce = nonce
		} else if nonce != firstNonce {
			allIdentical = false
		}

		if _, dup := seen[nonce]; dup {
			t.Fatalf("nonce collision detected on iteration %d: %x", i, nonce)
		}
		seen[nonce] = struct{}{}
	}

	if allIdentical {
		t.Fatalf("all %d nonces were identical (%x) — RNG appears broken", n, firstNonce)
	}
}

// ---------------------------------------------------------------------------
// Encrypt error paths
// ---------------------------------------------------------------------------

func TestEncryptAESGCMRejectsInvalidKey(t *testing.T) {
	if _, err := EncryptAESGCM([]byte("short key"), []byte("plaintext")); err == nil {
		t.Fatalf("EncryptAESGCM succeeded with invalid key, want error")
	}
}

func TestEncryptAESGCMReturnsRandomReaderError(t *testing.T) {
	if _, err := encryptAESGCM(testKey(0x99), []byte("plaintext"), errReader{}); err == nil {
		t.Fatalf("encryptAESGCM succeeded with failing random reader, want error")
	}
}

// ---------------------------------------------------------------------------
// Decrypt error paths
// ---------------------------------------------------------------------------

func TestDecryptAESGCMRejectsInvalidKey(t *testing.T) {
	if _, err := DecryptAESGCM([]byte("short key"), []byte("ciphertext")); err == nil {
		t.Fatalf("DecryptAESGCM succeeded with invalid key, want error")
	}
}

func TestDecryptAESGCMRejectsTamperedCiphertext(t *testing.T) {
	key := testKey(0x55)
	ciphertext, err := EncryptAESGCM(key, []byte("sensitive token"))
	if err != nil {
		t.Fatalf("EncryptAESGCM returned error: %v", err)
	}

	ciphertext[len(ciphertext)-1] ^= 0x01
	if _, err := DecryptAESGCM(key, ciphertext); err == nil {
		t.Fatalf("DecryptAESGCM succeeded for tampered ciphertext, want error")
	}
}

// TestDecryptAESGCMRejectsTamperedNonce verifies that flipping a single nonce bit
// causes authentication to fail (the tag was computed over the original nonce).
func TestDecryptAESGCMRejectsTamperedNonce(t *testing.T) {
	key := testKey(0xbb)
	blob, err := EncryptAESGCM(key, []byte("oauth token"))
	if err != nil {
		t.Fatalf("EncryptAESGCM error: %v", err)
	}
	blob[0] ^= 0xff // flip first nonce byte
	if _, err := DecryptAESGCM(key, blob); err == nil {
		t.Fatalf("DecryptAESGCM succeeded with tampered nonce, want auth failure")
	}
}

func TestDecryptAESGCMRejectsWrongKey(t *testing.T) {
	ciphertext, err := EncryptAESGCM(testKey(0x66), []byte("sensitive token"))
	if err != nil {
		t.Fatalf("EncryptAESGCM returned error: %v", err)
	}

	if _, err := DecryptAESGCM(testKey(0x77), ciphertext); err == nil {
		t.Fatalf("DecryptAESGCM succeeded with wrong key, want error")
	}
}

func TestDecryptAESGCMRejectsShortBlob(t *testing.T) {
	if _, err := DecryptAESGCM(testKey(0x88), []byte("short")); err == nil {
		t.Fatalf("DecryptAESGCM succeeded for short blob, want error")
	}
}

func TestDecryptAESGCMRejectsNonceOnlyBlob(t *testing.T) {
	if _, err := DecryptAESGCM(testKey(0xaa), bytes.Repeat([]byte{0x00}, 12)); err == nil {
		t.Fatalf("DecryptAESGCM succeeded for nonce-only blob, want error")
	}
}

// TestDecryptAESGCMRejectsTruncatedTag ensures that a blob truncated in the middle of
// the GCM authentication tag is rejected rather than silently succeeding.
func TestDecryptAESGCMRejectsTruncatedTag(t *testing.T) {
	key := testKey(0xdd)
	blob, err := EncryptAESGCM(key, []byte("token"))
	if err != nil {
		t.Fatalf("EncryptAESGCM error: %v", err)
	}
	// Remove the last 8 bytes (half of the 16-byte tag).
	truncated := blob[:len(blob)-8]
	if _, err := DecryptAESGCM(key, truncated); err == nil {
		t.Fatalf("DecryptAESGCM succeeded with truncated auth tag, want error")
	}
}

// Compile-time interface assertion.
var _ io.Reader = errReader{}
var _ io.Reader = (*fixedReader)(nil)
