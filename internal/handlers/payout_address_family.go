package handlers

import (
	"errors"

	"github.com/gofiber/fiber/v2"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/payout"
	"github.com/jagadeesh/grainlify/backend/internal/payoutaddr"
)

// Per-chain-family dispatch for payout address registration. Grainlify-Backend#548.
//
// # Why every branch is explicit and nothing has a default
//
// The defect being fixed is a default: chain_id was any non-empty string, and
// the one validator in existence was applied to whatever arrived. That
// validator pads, so an EVM address became a valid Aptos address belonging to
// somebody else.
//
// So these functions have no fallback arm. An unrecognised family is an error,
// never "treat it as Aptos" - the assumption is the bug.

// walletType per family, for the nonce.
//
// The nonce table is keyed on (wallet_type, address, purpose), so this must be
// stable per family: issuing a challenge under one wallet type and consuming it
// under another makes every registration fail as nonce_unknown.
const payoutWalletTypeEVM = auth.WalletTypeEVM

func walletTypeForFamily(family string) auth.WalletType {
	if family == payout.FamilyEVM {
		return payoutWalletTypeEVM
	}
	return payoutWalletType
}

// validateForFamily picks the validator the chain's family requires.
//
// **The EVM arm must never reach payoutaddr.Validate or payoutaddr.Canonical.**
// Those left-pad to 64 hex characters, which silently converts an EVM address
// into a different, valid Aptos address - the whole of #548.
func validateForFamily(family, raw string) (string, error) {
	switch family {
	case payout.FamilyAptos:
		return payoutaddr.Validate(raw)
	case payout.FamilyEVM:
		return payoutaddr.ValidateEVM(raw)
	default:
		return "", errors.New("unsupported chain family: " + family)
	}
}

// verifyForFamily proves control of the address.
//
// # It returns (handled, err), and the bool is the load-bearing half
//
// Fiber's c.Status(...).JSON(...) returns the error from WRITING the response,
// which is nil when the write succeeds. So a helper that signals failure by
// returning that value signals nothing: a rejected signature writes 400,
// returns nil, and the caller reads nil as "verified" and carries on to store
// the address and overwrite the body with 201.
//
// That is not hypothetical - it is what the first version of this function did,
// and it made an invalid signature register somebody else's address. It was
// caught by the pre-existing enumeration-oracle test rather than by reading,
// which is the point: `err != nil` is the wrong question to ask a fiber
// response, so the answer is carried separately and explicitly.
//
// Both families answer the same question - "does the key behind this signature
// own the address being claimed" - but they answer it differently, and the
// difference matters. Aptos DERIVES the address from the submitted public key
// and compares, which is why an EVM address padded into Aptos shape could never
// be stored even before this change. EVM RECOVERS the signer from the signature
// itself and needs no public key at all, so that protection does not exist
// here: on an EVM chain the signature check is the only thing standing between
// a typo and somebody else's address, which is why ValidateEVM enforces EIP-55
// before we ever get here.
func verifyForFamily(c *fiber.Ctx, family, addr, msg, nonce, signature, publicKey string) (handled bool, err error) {
	switch family {
	case payout.FamilyEVM:
		// publicKey is deliberately ignored: it is recovered from the
		// signature, so accepting one from the caller would let them submit a
		// key unrelated to the signature they actually produced.
		if verr := auth.VerifySignature(auth.WalletTypeEVM, addr, msg, signature, ""); verr != nil {
			return true, c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "signature_invalid"})
		}
		return false, nil

	case payout.FamilyAptos:
		derived, verr := auth.VerifyAptosSignature(addr, msg, nonce, signature, publicKey)
		switch {
		case errors.Is(verr, auth.ErrAptosAddrMismatch):
			// Never silently store `derived`. Redirecting somebody's money to an
			// address they did not name is the worst available behaviour here.
			return true, c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
				"error":   "signature_address_mismatch",
				"claimed": addr,
				"derived": derived,
				"detail": "The signature is valid, but the key that produced it belongs to a different " +
					"address. Nothing has been saved. Connect the wallet holding " + addr +
					", or register " + derived + " instead.",
			})
		case errors.Is(verr, auth.ErrAptosBadPublicKey):
			return true, c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "unsupported_scheme", "detail": verr.Error()})
		case errors.Is(verr, auth.ErrAptosBadSignature), errors.Is(verr, auth.ErrAptosBadSig):
			return true, c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "signature_invalid"})
		case verr != nil:
			return true, c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "signature_invalid"})
		}
		return false, nil

	default:
		return true, c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "chain_unsupported"})
	}
}

// payoutChainError turns a chain lookup failure into one name per cause.
//
// Named for this handler because package handlers already has a chainError with
// a different signature and a different job.
//
// Three names rather than one, following the rule this package already applies
// to nonces: "no such chain", "that chain is switched off" and "that chain's
// row is half-finished" are three different things to go and fix, and an
// operator who is told only "bad chain" has to discover which.
func payoutChainError(c *fiber.Ctx, chainID string, err error) error {
	name := "chain_unsupported"
	switch {
	case errors.Is(err, payout.ErrChainNotConfigured):
		name = "chain_not_configured"
	case errors.Is(err, payout.ErrChainNotEnabled):
		name = "chain_not_enabled"
	case errors.Is(err, payout.ErrChainFamilyUnknown):
		name = "chain_family_unknown"
	}
	return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
		"error":    name,
		"chain_id": chainID,
		"detail":   err.Error(),
	})
}
