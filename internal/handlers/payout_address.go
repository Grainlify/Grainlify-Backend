package handlers

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/auth"
	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/payoutaddr"
)

// PayoutAddressHandler collects and verifies where somebody's rewards are sent.
//
// Separate from wallet sign-in throughout: `wallets` answers "prove you hold this
// key", and this answers "send money here". Nothing here can create a user - see
// internal/auth/payout_nonce.go and its surface test.
type PayoutAddressHandler struct {
	db *db.DB
}

func NewPayoutAddressHandler(d *db.DB) *PayoutAddressHandler { return &PayoutAddressHandler{db: d} }

const payoutWalletType = auth.WalletType("aptos_ed25519")

// challengeMessage is what the wallet is asked to sign, before AIP-62 wraps it.
//
// It names the purpose, the chain and the address in plain words, because this
// string is shown to a person inside their wallet and "sign this random hex" is
// how people are phished.
//
// # The nonce appears TWICE in what gets signed, and that is fine
//
// Observed against a real Petra, not inferred. The wallet signs:
//
//	APTOS
//	message: Grainlify payout address verification
//	Chain: aptos-testnet
//	Address: 0x1b41…22c9
//	Nonce: 8f2c…            <- ours, the line below
//	nonce: 8f2c…            <- AIP-62's, appended by the envelope
//
// **Do not tidy this up**, and be precise about why, because the obvious reason
// is wrong. Deleting our `Nonce:` line would NOT break verification: this same
// function produces both the challenge we hand out and the string we rebuild at
// verification time, the client never supplies a message, so both sides would
// change together and signatures would still verify.
//
// The actual hazards are two, and neither is cryptographic:
//
//  1. **In-flight challenges break across the deploy.** Any nonce issued by the
//     old binary and submitted to the new one is checked against a message that
//     has changed, and fails as `signature_invalid` - for a person who did
//     nothing wrong, with an error naming the one thing that was fine. It lasts
//     the nonce TTL, which is long enough to look like an outage.
//  2. **The person loses the only per-attempt detail they can see.** The AIP-62
//     `nonce:` line is envelope plumbing; our `Nonce:` line is inside the text
//     the wallet renders as the message. Remove it and every prompt looks
//     identical, which is exactly the condition under which people stop reading
//     them.
//
// The binding itself comes from the envelope either way. This line is for the
// human and for deploy safety, and it costs one line to keep.
func challengeMessage(chainID, address, nonce string) string {
	return fmt.Sprintf(
		"Grainlify payout address verification\nChain: %s\nAddress: %s\nNonce: %s",
		chainID, address, nonce)
}

func userID(c *fiber.Ctx) (uuid.UUID, bool) {
	s, _ := c.Locals(auth.LocalUserID).(string)
	id, err := uuid.Parse(s)
	return id, err == nil
}

// PostChallenge issues a purpose-scoped nonce for one address.
func (h *PayoutAddressHandler) PostChallenge(c *fiber.Ctx) error {
	uid, ok := userID(c)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthenticated"})
	}
	var body struct {
		ChainID string `json:"chain_id"`
		Address string `json:"address"`
	}
	if err := c.BodyParser(&body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "bad_request"})
	}
	if strings.TrimSpace(body.ChainID) == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "chain_id_required"})
	}

	// Canonicalise BEFORE issuing the challenge, so the message commits to the
	// same 66-character form that will be stored. Challenging on "0x1b41" and
	// storing "0x0000…1b41" signs one string and saves another.
	addr, err := payoutaddr.Validate(body.Address)
	if err != nil {
		return addressError(c, err)
	}

	n, err := auth.CreateNonceForPurpose(c.Context(), h.db.Pool, payoutWalletType, addr,
		auth.PurposePayoutAddress, 10*time.Minute)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "nonce_failed"})
	}
	_ = uid
	return c.JSON(fiber.Map{
		"nonce":      n.Nonce,
		"message":    challengeMessage(body.ChainID, addr, n.Nonce),
		"expires_at": n.ExpiresAt,
	})
}

// PostAddress verifies a signature and stores the address.
func (h *PayoutAddressHandler) PostAddress(c *fiber.Ctx) error {
	uid, ok := userID(c)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthenticated"})
	}
	var body struct {
		ChainID   string `json:"chain_id"`
		Address   string `json:"address"`
		PublicKey string `json:"public_key"`
		Signature string `json:"signature"`
		Nonce     string `json:"nonce"`
	}
	if err := c.BodyParser(&body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "bad_request"})
	}
	addr, err := payoutaddr.Validate(body.Address)
	if err != nil {
		return addressError(c, err)
	}

	// Verify BEFORE consuming: a signature that fails should not burn the nonce,
	// or a mistyped wallet costs the person a round trip for no reason.
	msg := challengeMessage(body.ChainID, addr, body.Nonce)
	derived, verr := auth.VerifyAptosSignature(addr, msg, body.Nonce, body.Signature, body.PublicKey)
	switch {
	case errors.Is(verr, auth.ErrAptosAddrMismatch):
		// Never silently store `derived`. Redirecting somebody's money to an
		// address they did not name is the worst available behaviour here.
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error":   "signature_address_mismatch",
			"claimed": addr,
			"derived": derived,
			"detail": "The signature is valid, but the key that produced it belongs to a different " +
				"address. Nothing has been saved. Connect the wallet holding " + addr +
				", or register " + derived + " instead.",
		})
	case errors.Is(verr, auth.ErrAptosBadPublicKey):
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "unsupported_scheme", "detail": verr.Error()})
	case errors.Is(verr, auth.ErrAptosBadSignature), errors.Is(verr, auth.ErrAptosBadSig):
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "signature_invalid"})
	case verr != nil:
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "signature_invalid"})
	}

	if err := auth.ConsumeNonceForPurpose(c.Context(), h.db.Pool, payoutWalletType, addr,
		body.Nonce, auth.PurposePayoutAddress); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": nonceErrorName(err)})
	}

	tx, err := h.db.Pool.BeginTx(c.Context(), pgxTxOptions())
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "store_failed"})
	}
	defer tx.Rollback(c.Context())

	var prevAddr string
	var prevAt *time.Time
	err = tx.QueryRow(c.Context(), `
		SELECT address FROM contributor_addresses
		WHERE user_id=$1 AND chain_id=$2 AND superseded_at IS NULL`, uid, body.ChainID).Scan(&prevAddr)
	if err == nil && prevAddr == addr {
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "address_unchanged", "address": addr})
	}
	if err == nil {
		now := time.Now().UTC()
		prevAt = &now
		if _, err := tx.Exec(c.Context(), `
			UPDATE contributor_addresses SET superseded_at=now()
			WHERE user_id=$1 AND chain_id=$2 AND superseded_at IS NULL`, uid, body.ChainID); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "store_failed"})
		}
	}

	var verifiedAt time.Time
	if err := tx.QueryRow(c.Context(), `
		INSERT INTO contributor_addresses (user_id, chain_id, address, verified_nonce)
		VALUES ($1,$2,$3,$4) RETURNING verified_at`,
		uid, body.ChainID, addr, body.Nonce).Scan(&verifiedAt); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "store_failed"})
	}
	if err := tx.Commit(c.Context()); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "store_failed"})
	}

	resp := fiber.Map{
		"chain_id":    body.ChainID,
		"address":     addr,
		"verified_at": verifiedAt,
		"replaced":    nil,
	}
	if prevAt != nil {
		resp["replaced"] = fiber.Map{"address": prevAddr, "superseded_at": *prevAt}
	}
	return c.Status(fiber.StatusCreated).JSON(resp)
}

// GetAddress returns the live payout address for a chain.
func (h *PayoutAddressHandler) GetAddress(c *fiber.Ctx) error {
	uid, ok := userID(c)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthenticated"})
	}
	chainID := c.Query("chain_id")
	if chainID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "chain_id_required"})
	}
	var addr string
	var at time.Time
	if err := h.db.Pool.QueryRow(c.Context(), `
		SELECT address, verified_at FROM contributor_addresses
		WHERE user_id=$1 AND chain_id=$2 AND superseded_at IS NULL`, uid, chainID).Scan(&addr, &at); err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "no_payout_address"})
	}
	return c.JSON(fiber.Map{"chain_id": chainID, "address": addr, "verified_at": at})
}

func addressError(c *fiber.Ctx, err error) error {
	name := "address_malformed"
	if errors.Is(err, payoutaddr.ErrReserved) {
		name = "address_reserved"
	}
	return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": name, "detail": err.Error()})
}

// One name per cause, never a name that could cover two: "never had one", "it
// expired", "already used" and "issued for something else" are four facts.
func nonceErrorName(err error) string {
	switch {
	case errors.Is(err, auth.ErrNonceWrongPurpose):
		return "nonce_wrong_purpose"
	case errors.Is(err, auth.ErrNonceExpired):
		return "nonce_expired"
	case errors.Is(err, auth.ErrNonceUsed):
		return "nonce_used"
	default:
		return "nonce_unknown"
	}
}
