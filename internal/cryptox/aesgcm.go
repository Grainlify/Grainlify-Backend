// Package cryptox provides encryption helpers for protecting sensitive values at rest.
package cryptox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
)

func KeyFromB64(b64 string) ([]byte, error) {
	if b64 == "" {
		return nil, fmt.Errorf("TOKEN_ENC_KEY_B64 is required")
	}
	key, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("decode TOKEN_ENC_KEY_B64: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("TOKEN_ENC_KEY_B64 must decode to 32 bytes")
	}
	return key, nil
}

func newAESGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// EncryptAESGCM encrypts plaintext using AES-256-GCM and returns nonce||ciphertext,
// where ciphertext includes the 16-byte GCM authentication tag appended by cipher.AEAD.Seal.
//
// A fresh 96-bit (12-byte) nonce is drawn from crypto/rand for every call.
// AES-GCM is an authenticated cipher: the returned blob is both confidential and
// integrity-protected.
//
// ⚠ NONCE-REUSE HAZARD: AES-GCM catastrophically loses both confidentiality and
// authenticity if the same (key, nonce) pair is ever used for two different messages.
// Never call this function with a synthetic or counter-derived nonce unless you can
// guarantee global uniqueness across restarts and replicas. The internal variant
// encryptAESGCM accepts a custom io.Reader solely for deterministic testing; production
// callers MUST use the public EncryptAESGCM which sources nonces from crypto/rand.Reader.
func EncryptAESGCM(key []byte, plaintext []byte) ([]byte, error) {
	return encryptAESGCM(key, plaintext, rand.Reader)
}

// encryptAESGCM is the internal implementation of EncryptAESGCM. The random parameter
// is exposed only to allow deterministic known-answer tests; all production callers
// must use EncryptAESGCM (which passes crypto/rand.Reader). Passing a non-CSPRNG
// source violates the nonce-uniqueness invariant required by AES-GCM — see the hazard
// note on EncryptAESGCM.
func encryptAESGCM(key []byte, plaintext []byte, random io.Reader) ([]byte, error) {
	gcm, err := newAESGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(random, nonce); err != nil {
		return nil, err
	}
	ct := gcm.Seal(nil, nonce, plaintext, nil)
	out := make([]byte, 0, len(nonce)+len(ct))
	out = append(out, nonce...)
	out = append(out, ct...)
	return out, nil
}

func DecryptAESGCM(key []byte, blob []byte) ([]byte, error) {
	gcm, err := newAESGCM(key)
	if err != nil {
		return nil, err
	}
	if len(blob) < gcm.NonceSize() {
		return nil, fmt.Errorf("ciphertext too short")
	}
	nonce := blob[:gcm.NonceSize()]
	ct := blob[gcm.NonceSize():]
	return gcm.Open(nil, nonce, ct, nil)
}
