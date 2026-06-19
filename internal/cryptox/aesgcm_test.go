package cryptox

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"testing"
)

type errReader struct{}

func (errReader) Read([]byte) (int, error) {
	return 0, errors.New("random source failed")
}

func testKey(seed byte) []byte {
	return bytes.Repeat([]byte{seed}, 32)
}

func testKeyB64(seed byte) string {
	return base64.StdEncoding.EncodeToString(testKey(seed))
}

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

func TestEncryptAESGCMRejectsInvalidKey(t *testing.T) {
	if _, err := EncryptAESGCM([]byte("short key"), []byte("plaintext")); err == nil {
		t.Fatalf("EncryptAESGCM succeeded with invalid key, want error")
	}
}

func TestEncryptAESGCMReturnsRandomReaderError(t *testing.T) {
	originalReader := randomReader
	randomReader = errReader{}
	defer func() {
		randomReader = originalReader
	}()

	if _, err := EncryptAESGCM(testKey(0x99), []byte("plaintext")); err == nil {
		t.Fatalf("EncryptAESGCM succeeded with failing random reader, want error")
	}
}

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

var _ io.Reader = errReader{}
