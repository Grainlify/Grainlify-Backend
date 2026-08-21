package handlers

import (
	"errors"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/chainread"
	"github.com/jagadeesh/grainlify/backend/internal/payout"
)

// GetClaimChainState answers the two questions the chain is authoritative for,
// in one round trip.
//
// # Why one endpoint rather than two, and why no cache
//
// Claimed state and the deadline are both view calls against the same module for
// the same settlement. Returning them together is a single round trip, which
// removes the only argument a cached copy ever had.
//
// And a cache here is worse than neutral. `extend_deadline` is admin-gated,
// one-directional, and is the DOCUMENTED REMEDY for a late claimant. A stale
// deadline shows somebody the old date immediately after we extended it for
// them - the interface contradicting the fix we just applied, at the moment they
// are deciding whether their money is gone. The staleness works against the
// remedy it would be caching around.
//
// Claimed state has the same property one step removed: the chain is the system
// of record, and our own record exists to be reconciled against it rather than
// to answer for it.
func (h *PayoutClaimsHandler) GetClaimChainState(c *fiber.Ctx) error {
	uid, ok := userID(c)
	if !ok {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthenticated"})
	}
	sid, err := uuid.Parse(c.Params("settlement_id"))
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "bad_settlement_id"})
	}
	ctx := c.Context()

	// The leaf must be one of THIS user's, so the endpoint cannot be used to
	// enumerate other people's claim state by settlement id.
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
	leafHash, _ := cl["leaf_hash"].(string)

	cc, err := payout.ChainConfigFor(ctx, h.db.Pool, chainID)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "chain_not_configured", "detail": err.Error()})
	}

	// The node URL is resolved from the env var chain_configs NAMES, never from
	// a column: an endpoint carrying an API key must not live in a migration.
	nodeURL, err := chainread.EndpointFor(h.rpcEnvFor(c, chainID))
	if err != nil {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
			"error":  "chain_endpoint_not_configured",
			"detail": err.Error(),
		})
	}
	client := chainread.New(nodeURL)

	claimed, err := client.IsClaimed(ctx, cc.ContractAddress, escrow, leafHash)
	if err != nil {
		return chainError(c, err)
	}
	deadline, err := client.ClaimDeadline(ctx, cc.ContractAddress, escrow)
	if err != nil {
		return chainError(c, err)
	}

	return c.JSON(fiber.Map{
		"settlement_id":  sid,
		"escrow_address": escrow,
		"claimed":        claimed,
		"deadline":       time.Unix(deadline, 0).UTC(),
		"deadline_unix":  deadline,
		"read_at":        time.Now().UTC(),
		// Said out loud so nobody caches it downstream either. The deadline can
		// move later at any time, because extending it is how we help somebody
		// who missed it.
		"source": "read live from chain; not cached, and the deadline can be extended at any time",
	})
}

// chainError keeps the node's own failure distinguishable from ours.
func chainError(c *fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, chainread.ErrContractAborted):
		// The chain answered. Reporting this as an outage would send somebody to
		// check the network when the escrow does not exist or the argument was
		// wrong.
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{
			"error": "contract_aborted", "detail": err.Error()})
	case errors.Is(err, chainread.ErrNodeFailed):
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
			"error": "chain_node_unreachable", "detail": err.Error()})
	case errors.Is(err, chainread.ErrBadReply):
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{
			"error": "chain_node_returned_something_unexpected", "detail": err.Error()})
	default:
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"error": "chain_read_failed", "detail": err.Error()})
	}
}

// rpcEnvFor reads which environment variable names this chain's node.
func (h *PayoutClaimsHandler) rpcEnvFor(c *fiber.Ctx, chainID string) string {
	var ref *string
	_ = h.db.Pool.QueryRow(c.Context(), `SELECT rpc_endpoint_ref FROM chain_configs WHERE chain_id=$1`, chainID).Scan(&ref)
	if ref == nil {
		return ""
	}
	return *ref
}
