package sponsor

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/sha3"
)

// The sponsor key, and the only place it exists in this process.
//
// # Same treatment rpc_endpoint_ref got, for the same reason
//
//   - It lives in ONE environment variable, APTOS_SPONSOR_KEY.
//   - It is read by ONE function, LoadSigner, which returns a signer and never
//     the key.
//   - It never reaches a migration, a log line, an error body, a response, or a
//     test fixture.
//
// key_surface_test.go parses this file and fails if the key escapes into a
// format string or a returned value. The precedent is internal/salt, which has
// no accessor rather than a carefully-unused one, and
// internal/auth/payout_nonce.go, which cannot create a user rather than merely
// not doing so. **Make the capability absent, not unused.**
//
// # No fallback, ever
//
// An absent or malformed key REFUSES TO BOOT. It does not fall back to
// unsponsored claims, because that is this issue arriving a second time: a
// default that works in development and misleads in production. On testnet the
// misleading version even passes, since APT is free from a faucet.
const SponsorKeyEnv = "APTOS_SPONSOR_KEY"

var (
	ErrNoSponsorKey  = errors.New("sponsor_key_not_configured")
	ErrBadSponsorKey = errors.New("sponsor_key_malformed")
)

// Signer signs as fee payer. It exposes the public half and nothing else.
type Signer struct {
	priv ed25519.PrivateKey
}

// LoadSigner is the only function that reads the key.
func LoadSigner() (*Signer, error) {
	raw := strings.TrimSpace(os.Getenv(SponsorKeyEnv))
	if raw == "" {
		return nil, fmt.Errorf("%w: %s is not set. Claims cannot be sponsored, and this "+
			"deliberately does not fall back to unsponsored claims", ErrNoSponsorKey, SponsorKeyEnv)
	}
	b, err := hex.DecodeString(strings.TrimPrefix(strings.TrimPrefix(raw, "ed25519-priv-"), "0x"))
	if err != nil {
		// The key is NOT in this message, and must never be.
		return nil, fmt.Errorf("%w: %s is not hexadecimal", ErrBadSponsorKey, SponsorKeyEnv)
	}
	switch len(b) {
	case ed25519.SeedSize:
		return &Signer{priv: ed25519.NewKeyFromSeed(b)}, nil
	case ed25519.PrivateKeySize:
		return &Signer{priv: ed25519.PrivateKey(b)}, nil
	default:
		return nil, fmt.Errorf("%w: %s decodes to %d bytes, want %d or %d",
			ErrBadSponsorKey, SponsorKeyEnv, len(b), ed25519.SeedSize, ed25519.PrivateKeySize)
	}
}

// PublicKeyHex is the public half, which is not a secret.
func (s *Signer) PublicKeyHex() string {
	return "0x" + hex.EncodeToString(s.priv.Public().(ed25519.PublicKey))
}

// Sign produces a signature over an already-built signing message.
//
// It takes bytes and returns bytes: the key does not leave this type, and no
// caller can obtain it.
func (s *Signer) Sign(message []byte) []byte { return ed25519.Sign(s.priv, message) }

// AptosAddress is the sponsor's on-chain address, derived from the key.
//
// Derived rather than configured: an address set separately can drift from the
// key it is supposed to name, and the resulting signature is attributed to an
// account that did not sign.
func (s *Signer) AptosAddress() string {
	pub := s.priv.Public().(ed25519.PublicKey)
	h := sha3.New256()
	h.Write(pub)
	h.Write([]byte{0x00}) // single-Ed25519 scheme
	return "0x" + hex.EncodeToString(h.Sum(nil))
}
