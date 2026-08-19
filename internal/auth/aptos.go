package auth

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/sha3"
)

// Aptos address derivation and AIP-62 message verification.
//
// Every one of the three rules below produces a VALID SIGNATURE OVER THE WRONG
// BYTES when broken, which is the failure that looks like a wallet bug and is
// not. They were established against a real Petra wallet, not from the spec.

var (
	ErrAptosBadPublicKey = errors.New("public key is not 32 bytes of Ed25519")
	ErrAptosBadSignature = errors.New("signature is not 64 bytes")
	ErrAptosAddrMismatch = errors.New("signature_address_mismatch")
	ErrAptosBadSig       = errors.New("signature_invalid")
)

// Ed25519 single-signer scheme byte, appended before hashing to derive an
// address. SingleKey (AIP-61 generalised keys) uses a different one, which is why
// both are tried below rather than one being assumed.
const (
	schemeEd25519   byte = 0x00
	schemeSingleKey byte = 0x02
)

// AptosAddressFromEd25519 is sha3-256(pubkey || 0x00).
func AptosAddressFromEd25519(pub []byte) string {
	h := sha3.New256()
	h.Write(pub)
	h.Write([]byte{schemeEd25519})
	return "0x" + hex.EncodeToString(h.Sum(nil))
}

// AptosAddressFromSingleKey is sha3-256(0x00 || pubkey || 0x02), where the
// leading byte is the AnyPublicKey variant index for Ed25519.
func AptosAddressFromSingleKey(pub []byte) string {
	h := sha3.New256()
	h.Write([]byte{0x00}) // AnyPublicKey::Ed25519
	h.Write(pub)
	h.Write([]byte{schemeSingleKey})
	return "0x" + hex.EncodeToString(h.Sum(nil))
}

// AptosFullMessage rebuilds the envelope AIP-62 wallets actually sign.
//
// # The server reconstructs this; it never accepts the client's copy
//
// `signMessage({message, nonce})` does not sign `message`. It signs:
//
//	APTOS
//	message: <message>
//	nonce: <nonce>
//
// A client that supplies both the payload and the signature over it has been
// asked to mark its own homework: any string it likes verifies against a
// signature it made over that string. So this is built from the message WE
// composed and the nonce WE issued, and the client's `fullMessage`, if it sends
// one, is ignored.
//
// # No optional fields
//
// AIP-62 lets a caller request `address`, `application` and `chainId` be folded
// into the envelope, each adding a line. The server cannot rebuild what it did
// not ask for, so the frontend must call signMessage with none of them. If one
// is ever needed it becomes part of this function and of the contract.
func AptosFullMessage(message, nonce string) string {
	return "APTOS\nmessage: " + message + "\nnonce: " + nonce
}

// VerifyAptosSignature checks that `signature` is a valid Ed25519 signature over
// the reconstructed envelope, and that the key behind it derives to
// `claimedAddress`.
//
// Returns ErrAptosAddrMismatch carrying the derived address, so the caller can
// name both and store neither.
func VerifyAptosSignature(claimedAddress, message, nonce, signatureHex, publicKeyHex string) (derived string, err error) {
	pub, err := decodeHex(publicKeyHex)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return "", fmt.Errorf("%w: got %d bytes", ErrAptosBadPublicKey, len(pub))
	}
	sig, err := decodeHex(signatureHex)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return "", fmt.Errorf("%w: got %d bytes", ErrAptosBadSignature, len(sig))
	}

	full := AptosFullMessage(message, nonce)
	if !ed25519.Verify(pub, []byte(full), sig) {
		return "", ErrAptosBadSig
	}

	// Which derivation an account uses is a property of the account, not
	// something to guess. Accept whichever reproduces the claimed address - the
	// same approach the wallet check arrived at, for the same reason.
	want := strings.ToLower(strings.TrimSpace(claimedAddress))
	ed := AptosAddressFromEd25519(pub)
	sk := AptosAddressFromSingleKey(pub)
	if ed == want {
		return ed, nil
	}
	if sk == want {
		return sk, nil
	}
	// Report the Ed25519 derivation: it is the common case, and naming one
	// concrete address is more use than naming two and leaving the reader to
	// work out which applies.
	return ed, ErrAptosAddrMismatch
}
