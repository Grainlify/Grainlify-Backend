package handlers

import (
	"encoding/hex"
	"errors"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/chainread"
	"github.com/jagadeesh/grainlify/backend/internal/payout"
	"github.com/jagadeesh/grainlify/backend/internal/sponsor"
)

// PostSponsorClaim pays a contributor's gas so somebody who has never held APT
// can collect a payout.
//
// The four defences run in this order, and the order is the point: the free
// checks come before the ones that cost, and the cheapest refusal reaches the
// person with the clearest sentence.
//
//  0. simulate    free, catches every deterministic abort, BEFORE they signed
//  1. is_claimed  the common case, named precisely
//  2. rate limit  bounds the check-to-submit race and a bug in 0 or 1
//  3. balance     hard floor: stop sponsoring and say so
//
// Every outcome is recorded, refusals included: a burst of refusals is the shape
// of an attack, and a rate limit that cannot see them counts the wrong thing.
func (h *PayoutClaimsHandler) PostSponsorClaim(c *fiber.Ctx) error {
	uid, ok := userID(c)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthenticated"})
	}
	sid, err := uuid.Parse(c.Params("settlement_id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "bad_settlement_id"})
	}
	var body struct {
		PublicKey string `json:"public_key"`
		Signature string `json:"signature"`
		SeqNumber uint64 `json:"sequence_number"`
		MaxGas    uint64 `json:"max_gas_amount"`
		GasPrice  uint64 `json:"gas_unit_price"`
		Expires   uint64 `json:"expiration_timestamp_secs"`
	}
	if err := c.BodyParser(&body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "bad_request"})
	}
	ctx := c.Context()

	// The claim must be THIS user's. claimsFor runs through the addresses they
	// registered, so a settlement id alone cannot reach somebody else's leaf.
	claims, err := h.claimsFor(c, uid, &sid)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "claims_failed"})
	}
	if len(claims) == 0 {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "no_claim"})
	}
	if len(claims) > 1 {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "multiple_claims_for_settlement", "count": len(claims)})
	}
	cl := claims[0]
	chainID, _ := cl["chain_id"].(string)
	escrow, _ := cl["escrow_address"].(string)
	leafHex, _ := cl["leaf_hash"].(string)
	identHex, _ := cl["identity_hash"].(string)
	claimAddr, _ := cl["claim_address"].(string)
	amountStr, _ := cl["amount_minor"].(string)
	proofAny, _ := cl["proof"].([]string)

	cc, err := payout.ChainConfigFor(ctx, h.db.Pool, chainID)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "chain_not_configured", "detail": err.Error()})
	}
	nodeURL, err := chainread.EndpointFor(h.rpcEnvFor(c, chainID))
	if err != nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
			"error": "chain_endpoint_not_configured", "detail": err.Error()})
	}
	signer, err := sponsor.LoadSigner()
	if err != nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
			"error": "sponsorship_not_configured", "detail": err.Error()})
	}
	amount, err := sponsor.ParseU64(amountStr)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "bad_amount"})
	}
	leaf, _ := hex.DecodeString(strings.TrimPrefix(leafHex, "0x"))
	guard := sponsor.NewGuard(h.db.Pool)

	fields := sponsor.TxnFields{
		Sender: claimAddr, SeqNumber: body.SeqNumber, MaxGas: body.MaxGas,
		GasUnitPrice: body.GasPrice, Expiration: body.Expires, ChainID: 2,
		Module: cc.ContractAddress, ModuleName: "escrow", Function: "claim",
		Escrow: escrow, IdentityHash: identHex, AmountMinor: amount,
		Proof: proofAny, FeePayer: signer.AptosAddress(),
	}

	// --- 1. claim state -----------------------------------------------------
	client := chainread.New(nodeURL)
	claimed, err := client.IsClaimed(ctx, cc.ContractAddress, escrow, leafHex)
	if err == nil && claimed {
		_ = guard.Record(ctx, uid, sid, leaf, "refused_already_claimed", "", 0)
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{
			"error":  "already_claimed",
			"detail": "This reward has already been claimed. Nothing was submitted and nothing was spent.",
		})
	}

	// --- 0. simulate, BEFORE anything is signed on our side -----------------
	sim, simErr := sponsor.NewSimulator(nodeURL).SimulateClaim(ctx, sponsor.ClaimArgs{
		Module: cc.ContractAddress, Escrow: escrow, IdentityHash: identHex,
		AmountMinor: amountStr, Proof: proofAny, Sender: claimAddr,
		SenderPubKey: body.PublicKey, SeqNumber: uintToStr(body.SeqNumber),
		FeePayer: signer.AptosAddress(),
	})
	switch {
	case simErr != nil:
		// FAIL OPEN. A simulation we could not run is not evidence of a bad
		// claim, and refusing on our own outage would deny somebody their money
		// in the contract's voice.
	case !sim.Success:
		_ = guard.Record(ctx, uid, sid, leaf, "refused_simulation_failed", "", 0)
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{
			"error":     "claim_would_fail",
			"vm_status": sim.VMStatus,
			"detail":    "The chain reports this claim would not succeed, so nothing was submitted.",
		})
	}

	// --- 2. rate limit ------------------------------------------------------
	if err := guard.CheckRate(ctx, uid); err != nil {
		_ = guard.Record(ctx, uid, sid, leaf, "refused_rate_limited", "", 0)
		return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
			"error":  "rate_limited",
			"detail": "Too many sponsored attempts in a short time. Wait an hour and try again; your reward is not affected.",
		})
	}

	// --- 3. balance floor ---------------------------------------------------
	balance, balErr := client.AptosBalance(ctx, signer.AptosAddress())
	if balErr != nil {
		// A balance we cannot read is not a balance of zero. Refuse rather than
		// sponsor blind: this is the one defence with no upper bound on damage.
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
			"error": "sponsor_balance_unknown", "detail": balErr.Error()})
	}
	if err := guard.CheckBalance(balance); err != nil {
		_ = guard.Record(ctx, uid, sid, leaf, "refused_low_balance", "", 0)
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
			"error": "sponsor_balance_too_low",
			"detail": "We cannot cover the network fee right now. Please tell us — your reward is safe " +
				"and this affects everyone, not just you.",
		})
	}

	// --- verify WHICH form the sender signed, then submit -------------------
	pub, err := hex.DecodeString(strings.TrimPrefix(body.PublicKey, "0x"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "bad_public_key"})
	}
	sig, err := hex.DecodeString(strings.TrimPrefix(body.Signature, "0x"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "bad_signature"})
	}
	form, err := sponsor.VerifySenderForm(fields, pub, sig)
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"error":  "sender_signature_invalid",
			"detail": "The signature is over neither legal fee-payer message. Nothing was submitted.",
		})
	}

	hash, err := sponsor.NewSubmitter(nodeURL, signer).SubmitSponsored(ctx, fields, pub, sig)
	if err != nil {
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{
			"error": "submission_failed", "detail": err.Error()})
	}
	_ = guard.Record(ctx, uid, sid, leaf, "submitted", hash, sponsor.GasOctasPerClaim)

	return c.JSON(fiber.Map{
		"transaction_hash": hash,
		"fee_payer":        signer.AptosAddress(),
		// Recorded and returned: this is what tells us later whether a wallet
		// changed behaviour, which nobody can answer retrospectively from an
		// INVALID_SIGNATURE.
		"sender_signed_form": form,
		"gas_paid_by":        "grainlify",
	})
}

func uintToStr(v uint64) string { return strconv.FormatUint(v, 10) }

var _ = errors.Is
