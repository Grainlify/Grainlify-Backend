package handlers

import (
	"encoding/hex"
	"fmt"
	"math/big"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/payout"
	"github.com/jagadeesh/grainlify/backend/internal/payoutaddr"
)

type PayoutClaimsHandler struct{ db *db.DB }

func NewPayoutClaimsHandler(d *db.DB) *PayoutClaimsHandler { return &PayoutClaimsHandler{db: d} }

func hex0x(b []byte) string { return "0x" + hex.EncodeToString(b) }

// minorToDecimal renders exact minor units without ever becoming a float.
//
// The response carries BOTH this string and the raw minor units, and neither is
// a JSON number. JSON numbers are float64 in most clients, so a number here
// would round somebody's payout in their browser - the exact-integer rule
// reaching one boundary further than storage and Go, which were both treated as
// the fix. An exact-value guarantee has to be re-established wherever the
// representation changes hands.
func minorToDecimal(v int64, decimals int32) string {
	n := big.NewInt(v)
	neg := n.Sign() < 0
	abs := new(big.Int).Abs(n)
	d := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	q, r := new(big.Int).QuoRem(abs, d, new(big.Int))
	s := fmt.Sprintf("%s.%0*s", q, decimals, r)
	if neg {
		s = "-" + s
	}
	return s
}

// GetClaims returns published claims for every address this user has ever
// registered.
//
// # Why the join runs through address history
//
// claim_leaves deliberately has no user_id - that is what makes destroying the
// salt meaningful rather than theatre - so "this user's leaf" is not a direct
// lookup. contributor_addresses keeps superseded rows, and that history is the
// path: user -> every address they have registered -> claim_leaves by address.
//
// Keying on the CONNECTED address instead would strand the exact person this has
// to work for. Somebody who changed their payout address after a root was
// published connects their current wallet and is told they have no claim, while
// a real entitlement sits in the tree payable to the address they used to have.
func (h *PayoutClaimsHandler) GetClaims(c *fiber.Ctx) error {
	uid, ok := userID(c)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthenticated"})
	}
	claims, err := h.claimsFor(c, uid, nil)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "claims_failed", "detail": err.Error()})
	}
	return c.JSON(fiber.Map{"claims": claims})
}

// GetClaim returns one settlement's claim, or 404.
func (h *PayoutClaimsHandler) GetClaim(c *fiber.Ctx) error {
	uid, ok := userID(c)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthenticated"})
	}
	sid, err := uuid.Parse(c.Params("settlement_id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "bad_settlement_id"})
	}
	claims, err := h.claimsFor(c, uid, &sid)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "claims_failed"})
	}
	if len(claims) == 0 {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "no_claim"})
	}
	return c.JSON(claims[0])
}

func (h *PayoutClaimsHandler) claimsFor(c *fiber.Ctx, uid uuid.UUID, only *uuid.UUID) ([]fiber.Map, error) {
	ctx := c.Context()

	// Every address, live and superseded, plus which one is live now.
	type addrRow struct {
		addr    string
		chainID string
		live    bool
	}
	rows, err := h.db.Pool.Query(ctx, `
		SELECT address, chain_id, superseded_at IS NULL
		FROM contributor_addresses WHERE user_id = $1`, uid)
	if err != nil {
		return nil, err
	}
	var addrs []addrRow
	current := map[string]string{} // chain -> live address
	for rows.Next() {
		var a addrRow
		if err := rows.Scan(&a.addr, &a.chainID, &a.live); err != nil {
			rows.Close()
			return nil, err
		}
		addrs = append(addrs, a)
		if a.live {
			current[a.chainID] = a.addr
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := []fiber.Map{}
	for _, a := range addrs {
		// Published settlements only. Nothing appears that a person cannot act
		// on - and the absence of a claim is never the only signal they get,
		// which is what /me/payout-readiness is for.
		q := `
			SELECT r.settlement_id, r.chain_id, r.escrow_address, r.root, r.published_tx,
			       l.pool, l.leaf_index, l.leaf_hash, l.identity_hash, l.claim_address, l.amount_minor
			FROM claim_leaves l
			JOIN payout_event_roots r ON r.settlement_id = l.settlement_id
			WHERE lower(l.claim_address) = lower($1)
			  AND r.chain_id = $2
			  AND r.published_tx IS NOT NULL`
		args := []any{a.addr, a.chainID}
		if only != nil {
			q += ` AND r.settlement_id = $3`
			args = append(args, *only)
		}
		lrows, err := h.db.Pool.Query(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		for lrows.Next() {
			var (
				sid                              uuid.UUID
				chainID, escrow, pool, claimAddr string
				root, leafHash, identHash        []byte
				publishedTx                      *string
				leafIndex                        int
				amount                           int64
			)
			if err := lrows.Scan(&sid, &chainID, &escrow, &root, &publishedTx, &pool,
				&leafIndex, &leafHash, &identHash, &claimAddr, &amount); err != nil {
				lrows.Close()
				return nil, err
			}
			// Decimals come from the chain config: they are a property of the
			// token, not of one event's arithmetic.
			decimals, err := payout.AssetDecimalsFor(ctx, h.db.Pool, chainID)
			if err != nil {
				lrows.Close()
				return nil, err
			}
			cl, err := payout.ClaimFor(ctx, h.db.Pool, sid, claimAddr)
			if err != nil {
				lrows.Close()
				return nil, err
			}
			proof := make([]string, len(cl.Proof))
			for i, p := range cl.Proof {
				proof[i] = hex0x(p[:])
			}

			status := "current"
			var currentAddr any
			if cur, ok := current[chainID]; !ok || cur != claimAddr {
				status = "superseded"
				if ok {
					currentAddr = cur
				}
			}

			out = append(out, fiber.Map{
				"settlement_id":   sid,
				"chain_id":        chainID,
				"pool":            pool,
				"escrow_address":  escrow,
				"asset":           fiber.Map{"symbol": "USDC", "decimals": decimals},
				"amount_minor":    fmt.Sprintf("%d", amount),
				"amount":          minorToDecimal(amount, decimals),
				"claim_address":   claimAddr,
				"address_status":  status,
				"current_address": currentAddr,
				"identity_hash":   hex0x(cl.IdentityHash[:]),
				"leaf_hash":       hex0x(cl.LeafHash[:]),
				"leaf_index":      cl.LeafIndex,
				"proof":           proof,
				"root":            hex0x(cl.Root[:]),
				"published_tx":    publishedTx,
			})
		}
		lrows.Close()
		if err := lrows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// GetReadiness answers "does this person still need to register an address?"
//
// # Why this exists at all
//
// An empty /me/claims before publication and an empty one after look identical
// and mean opposite things. Before, somebody can still register and be included.
// After, they are permanently excluded from that tree and their share is residue
// - a root cannot be edited. Publication converts a recoverable gap into an
// irreversible one, and nothing in a list of claims says which side of that line
// the reader is on.
//
// So silence must not be the carrier of that distinction. This route is the only
// one that speaks to somebody who has NOT acted, and every other payout route is
// useless to a person who never registers an address.
//
// may_be_owed comes from entitlement state - founding membership - and NOT from
// settlements, so it is true from the day somebody becomes eligible, long before
// a tree exists. That is the entire reason for collecting addresses early: an
// address collected the week before a settlement costs nothing, and one collected
// the week after costs somebody their payout.
func (h *PayoutClaimsHandler) GetReadiness(c *fiber.Ctx) error {
	uid, ok := userID(c)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthenticated"})
	}
	chainID := c.Query("chain_id")
	if chainID == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "chain_id_required"})
	}
	ctx := c.Context()

	var isMember bool
	if err := h.db.Pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM founding_members WHERE user_id=$1)`, uid).Scan(&isMember); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "readiness_failed"})
	}

	var addr string
	hasAddr := h.db.Pool.QueryRow(ctx,
		`SELECT address FROM contributor_addresses WHERE user_id=$1 AND chain_id=$2 AND superseded_at IS NULL`,
		uid, chainID).Scan(&addr) == nil

	// No amount. §6 forbids a computed per-person figure reaching a UI, and an
	// exclusion amount is the sharpest case of it: money the person will not
	// receive, with no disbursement path to honour it. They are told they were
	// excluded and what to do; the number stays in the database.
	exs, err := payout.ExclusionsFor(ctx, h.db.Pool, uid)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "readiness_failed"})
	}
	excluded := []fiber.Map{}
	for _, e := range exs {
		excluded = append(excluded, fiber.Map{
			"settlement_id":   e.SettlementID,
			"excluded_reason": e.Reason,
			"remedy":          "contact_support",
		})
	}

	state := "not_applicable"
	switch {
	case len(excluded) > 0:
		state = "excluded_from_published"
	case isMember && hasAddr:
		state = "ready"
	case isMember:
		state = "register_now"
	}

	return c.JSON(fiber.Map{
		"chain_id":             chainID,
		"may_be_owed":          isMember,
		"basis":                map[bool]string{true: "founding_member", false: ""}[isMember],
		"has_verified_address": hasAddr,
		"action_required":      state == "register_now" || state == "excluded_from_published",
		"state":                state,
		"excluded_from":        excluded,
	})
}

var _ = payoutaddr.Validate
var _ = time.Now
