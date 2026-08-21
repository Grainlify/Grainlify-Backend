package sponsor

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

var (
	ErrAlreadyClaimed = errors.New("already_claimed")
	ErrRateLimited    = errors.New("rate_limited")
	ErrLowBalance     = errors.New("sponsor_balance_too_low")
	ErrSubmitFailed   = errors.New("submission_failed")
)

// GasOctasPerClaim is the measured cost of one sponsored claim.
//
// From the recorded milestone transaction, not an estimate: 14,920 gas units at
// 100 octas per unit. The first claim per recipient is the expensive one,
// because it creates their token store - which is the case this whole feature
// exists for, so the expensive number is the right one to plan with.
const GasOctasPerClaim = 14_920 * 100 // 1,492,000 octas = 0.01492 APT

// SafetyFactor covers gas-price movement (100 octas/unit was observed, not
// guaranteed) and retries.
const SafetyFactor = 3

// HardFloorOctas is where the service STOPS SPONSORING, not merely warns.
//
// # Why a floor and not only an alarm
//
// An alarm needs a recipient, and no channel currently reaches anyone off-site.
// An alarm nobody reads is the deadline problem again: a signal that exists,
// fires correctly, and changes nothing.
//
// A floor depends on nobody reading anything. It bounds the drain, and it
// surfaces as a clear message to a contributor - who will then tell us. **Failing
// visibly beats alerting into a void.**
//
// Basis: one full 38-person settlement at the measured cost, with the safety
// factor. 38 x 1,492,000 x 3 = 170,088,000 octas = 1.70 APT.
const FoundingPoolSize = 38
const HardFloorOctas = FoundingPoolSize * GasOctasPerClaim * SafetyFactor

// AlarmThresholdOctas is dynamic: enough to pay everyone still owed, three times
// over. It sits ABOVE the hard floor, so the warning arrives while there is
// still room to act.
func AlarmThresholdOctas(unclaimedLeaves int) int64 {
	dynamic := int64(unclaimedLeaves) * GasOctasPerClaim * SafetyFactor
	if dynamic < HardFloorOctas {
		return HardFloorOctas
	}
	return dynamic
}

// MaxSponsorshipsPerHour bounds what losing the check-to-submit race can cost.
//
// A person needs exactly one successful sponsorship per settlement, so this is
// generous by design: it is not a usage limit, it is a blast radius. At the
// measured cost, six submissions is under 0.09 APT per user per hour.
const MaxSponsorshipsPerHour = 6

// Guard holds the decisions that protect the sponsor account.
type Guard struct{ pool db.DBPool }

func NewGuard(pool db.DBPool) *Guard { return &Guard{pool: pool} }

// CheckRate enforces defence (2).
//
// # Why this survives trusting the simulation
//
// Simulation and the claim-state read are both time-of-check-to-time-of-use:
// between deciding and submitting, the same leaf can be claimed by another
// submission - including one we sponsored moments earlier. The window is small
// and non-zero.
//
// It also bounds the cost of a BUG in either of those checks, which is the
// reason it must exist even if both are trusted. A defence that only works when
// the other defences work is not a defence.
func (g *Guard) CheckRate(ctx context.Context, userID uuid.UUID) error {
	var n int
	if err := g.pool.QueryRow(ctx, `
		SELECT count(*) FROM sponsored_claims
		WHERE user_id = $1 AND outcome = 'submitted' AND created_at > now() - interval '1 hour'`,
		userID).Scan(&n); err != nil {
		return fmt.Errorf("rate check: %w", err)
	}
	if n >= MaxSponsorshipsPerHour {
		return fmt.Errorf("%w: %d sponsored submissions in the last hour", ErrRateLimited, n)
	}
	return nil
}

// CheckBalance enforces defence (3)'s hard floor.
//
// Returns ErrLowBalance when sponsoring would take the account below the point
// at which everyone still owed can be paid. The message is written for the
// contributor who receives it, because they are the person who will tell us.
func (g *Guard) CheckBalance(balanceOctas int64) error {
	if balanceOctas-GasOctasPerClaim < HardFloorOctas {
		return fmt.Errorf("%w: %s APT remaining, floor is %s APT",
			ErrLowBalance, aptString(balanceOctas), aptString(HardFloorOctas))
	}
	return nil
}

// Record writes the decision. Refusals are recorded too: a burst of them is the
// shape of an attack, and a rate limit that cannot see refusals is counting the
// wrong thing.
func (g *Guard) Record(ctx context.Context, userID, settlementID uuid.UUID, leaf []byte, outcome, txHash string, gas int64) error {
	var tx any
	if txHash != "" {
		tx = txHash
	}
	var gasv any
	if gas > 0 {
		gasv = gas
	}
	_, err := g.pool.Exec(ctx, `
		INSERT INTO sponsored_claims (user_id, settlement_id, leaf_hash, outcome, tx_hash, gas_octas)
		VALUES ($1,$2,$3,$4,$5,$6)`, userID, settlementID, leaf, outcome, tx, gasv)
	return err
}

func aptString(octas int64) string {
	r := new(big.Rat).SetFrac(big.NewInt(octas), big.NewInt(100_000_000))
	return r.FloatString(4)
}

var _ = time.Now
