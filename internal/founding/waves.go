// Package founding implements the Founding Contributor Pool: one fixed,
// announced USDC pool, shares earned overwhelmingly through merged pull
// requests, and a share value nobody can compute until settlement.
//
// It replaces the fixed-rate points programme, which paid a known amount for
// an action that costs nothing and produces nothing. The governing principle
// is the one the whole GrainHack design rests on: a reward you can calculate
// in advance is a reward you can farm.
package founding

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// Wave names. Membership is permanent and assigned once, at verification.
const (
	WaveFounding = "founding"
	WaveTwo      = "wave_two"
	WaveOpen     = "open"
)

// ErrWaveBoundariesLocked is returned when config disagrees with the
// boundaries frozen at first assignment.
//
// This is a loud failure rather than a silent preference for one value.
// §4.1: a wave that quietly widens after launch tells the community that
// Grainlify's announced limits are not real, and every published rule after
// that reads as provisional - which is unusually expensive here, because the
// anti-farming design depends on published rules being credible.
var ErrWaveBoundariesLocked = errors.New("wave boundaries are locked and cannot be changed after the first member is assigned")

// Boundaries is the wave configuration in force.
type Boundaries struct {
	FoundingSlots      int
	WaveTwoSlots       int
	MultiplierFounding float64
	MultiplierWaveTwo  float64
	MultiplierOpen     float64
}

// WaveFor returns the wave and multiplier for a given join position.
//
// Sequence numbers are 1-based and gapless, so the boundaries are simple
// ranges. Nobody is ever turned away: past Wave 2 everyone joins at the open
// multiplier and can still earn fully from merged pull requests.
func (b Boundaries) WaveFor(sequence int) (wave string, multiplier float64) {
	switch {
	case sequence <= b.FoundingSlots:
		return WaveFounding, b.MultiplierFounding
	case sequence <= b.FoundingSlots+b.WaveTwoSlots:
		return WaveTwo, b.MultiplierWaveTwo
	default:
		return WaveOpen, b.MultiplierOpen
	}
}

// Membership is one member's permanent wave assignment.
type Membership struct {
	UserID     uuid.UUID `json:"-"`
	Sequence   int       `json:"sequence_number"`
	Wave       string    `json:"wave"`
	Multiplier float64   `json:"multiplier"`
}

// boundariesFromConfig reads the configured wave sizes and multipliers.
func boundariesFromConfig(cfg map[string]string) Boundaries {
	return Boundaries{
		FoundingSlots:      atoiOr(cfg["founding_wave_founding_slots"], 100),
		WaveTwoSlots:       atoiOr(cfg["founding_wave_two_slots"], 400),
		MultiplierFounding: atofOr(cfg["founding_multiplier_founding"], 1.5),
		MultiplierWaveTwo:  atofOr(cfg["founding_multiplier_wave_two"], 1.25),
		MultiplierOpen:     atofOr(cfg["founding_multiplier_open"], 1.0),
	}
}

// LockedBoundaries returns the frozen boundaries, or false if nobody has been
// assigned yet and they are therefore still editable.
func LockedBoundaries(ctx context.Context, pool db.DBPool) (Boundaries, bool, error) {
	var b Boundaries
	err := pool.QueryRow(ctx, `
SELECT founding_slots, wave_two_slots, multiplier_founding, multiplier_wave_two, multiplier_open
FROM founding_wave_lock WHERE id = true
`).Scan(&b.FoundingSlots, &b.WaveTwoSlots, &b.MultiplierFounding, &b.MultiplierWaveTwo, &b.MultiplierOpen)
	if errors.Is(err, pgx.ErrNoRows) {
		return Boundaries{}, false, nil
	}
	if err != nil {
		return Boundaries{}, false, fmt.Errorf("founding.LockedBoundaries: %w", err)
	}
	return b, true, nil
}

// CheckBoundaryConfig reports whether the configured boundaries still match
// the locked ones.
//
// Called by the admin config path so an attempted edit fails at the point
// somebody makes it, with an explanation, rather than being silently ignored
// at assignment time. A rule that is quietly not applied is worse than one
// that refuses: the admin believes they changed something.
func CheckBoundaryConfig(ctx context.Context, pool db.DBPool, cfg map[string]string) error {
	locked, ok, err := LockedBoundaries(ctx, pool)
	if err != nil || !ok {
		return err
	}
	want := boundariesFromConfig(cfg)
	if want != locked {
		return fmt.Errorf("%w: locked at %d/%d slots and x%.3g/x%.3g/x%.3g multipliers",
			ErrWaveBoundariesLocked, locked.FoundingSlots, locked.WaveTwoSlots,
			locked.MultiplierFounding, locked.MultiplierWaveTwo, locked.MultiplierOpen)
	}
	return nil
}

// AssignWave gives userID their permanent wave, or returns their existing one.
//
// Idempotent by primary key: verification can fire from both the status-poll
// path and the webhook, and both may observe the same transition. Assigning
// twice would either move somebody between waves or leave a gap in the
// sequence, and both are unrecoverable once announced.
//
// The sequence is allocated under a transaction-scoped advisory lock rather
// than a Postgres sequence, because a sequence gaps on rollback: two
// concurrent verifications where one fails would burn a number and leave
// members 1, 2, 4. With 100 Founding slots that is a slot nobody can ever
// hold, in the tier whose entire value is that it is countable.
func AssignWave(ctx context.Context, pool db.DBPool, userID uuid.UUID, cfg map[string]string) (Membership, error) {
	// Existing members short-circuit BEFORE the gate, and that is deliberate
	// rather than incidental. It is what makes the gate apply only to
	// allocations that have not happened yet: everybody who already holds a
	// number keeps it, unexamined, whether or not they would pass the check
	// today. "Permanent" stays true.
	if existing, ok, err := MembershipFor(ctx, pool, userID); err != nil || ok {
		return existing, err
	}

	// The gate. Same key and same definition settlement uses - see
	// internal/founding/gate.go for why there is exactly one switch.
	//
	// A sequence number therefore records THE ORDER IN WHICH THE SECOND OF
	// (verify, approve) COMPLETED. That is a change in what the number means,
	// and it is the same principle migration 000071 already recorded when it
	// appended a pre-pool member: sequence is observation order, not
	// verification order. This widens what "observed" means; it does not
	// reverse it.
	//
	// Both events call this function, so whichever happens second does the
	// work and the first is a no-op. Without the second caller
	// (OnSocialFollowApproved) a contributor who verified before being
	// approved would never be assigned at all, because verification is the
	// only thing that used to reach here - which would have silently stranded
	// every person in that order.
	eligible, reason, err := approvedForFoundingPool(ctx, pool, userID, cfg)
	if err != nil {
		return Membership{}, fmt.Errorf("founding.AssignWave: eligibility: %w", err)
	}
	if !eligible {
		return Membership{}, fmt.Errorf("%w (%s)", ErrNotEligibleForWave, reason)
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Membership{}, fmt.Errorf("founding.AssignWave: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// One allocator at a time, across the whole programme.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('founding_wave_allocation', 0))`); err != nil {
		return Membership{}, fmt.Errorf("founding.AssignWave: lock: %w", err)
	}

	// Freeze the boundaries on the first assignment; every later one reads
	// the frozen row rather than config.
	b := boundariesFromConfig(cfg)
	if _, err := tx.Exec(ctx, `
INSERT INTO founding_wave_lock
  (id, founding_slots, wave_two_slots, multiplier_founding, multiplier_wave_two, multiplier_open)
VALUES (true, $1, $2, $3, $4, $5)
ON CONFLICT (id) DO NOTHING
`, b.FoundingSlots, b.WaveTwoSlots, b.MultiplierFounding, b.MultiplierWaveTwo, b.MultiplierOpen); err != nil {
		return Membership{}, fmt.Errorf("founding.AssignWave: lock boundaries: %w", err)
	}
	if err := tx.QueryRow(ctx, `
SELECT founding_slots, wave_two_slots, multiplier_founding, multiplier_wave_two, multiplier_open
FROM founding_wave_lock WHERE id = true
`).Scan(&b.FoundingSlots, &b.WaveTwoSlots, &b.MultiplierFounding, &b.MultiplierWaveTwo, &b.MultiplierOpen); err != nil {
		return Membership{}, fmt.Errorf("founding.AssignWave: read locked boundaries: %w", err)
	}

	var next int
	if err := tx.QueryRow(ctx, `
SELECT COALESCE(max(sequence_number), 0) + 1 FROM founding_members
`).Scan(&next); err != nil {
		return Membership{}, fmt.Errorf("founding.AssignWave: next sequence: %w", err)
	}

	wave, multiplier := b.WaveFor(next)
	m := Membership{UserID: userID, Sequence: next, Wave: wave, Multiplier: multiplier}

	if _, err := tx.Exec(ctx, `
INSERT INTO founding_members (user_id, sequence_number, wave, multiplier)
VALUES ($1, $2, $3, $4)
ON CONFLICT (user_id) DO NOTHING
`, userID, next, wave, multiplier); err != nil {
		return Membership{}, fmt.Errorf("founding.AssignWave: insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Membership{}, fmt.Errorf("founding.AssignWave: commit: %w", err)
	}

	// Re-read rather than trusting the computed value: if a concurrent
	// assignment won the ON CONFLICT race, the stored row is authoritative.
	stored, ok, err := MembershipFor(ctx, pool, userID)
	if err != nil {
		return Membership{}, err
	}
	if ok {
		return stored, nil
	}
	return m, nil
}

// MembershipFor returns userID's wave assignment, if they have one.
func MembershipFor(ctx context.Context, pool db.DBPool, userID uuid.UUID) (Membership, bool, error) {
	m := Membership{UserID: userID}
	err := pool.QueryRow(ctx, `
SELECT sequence_number, wave, multiplier FROM founding_members WHERE user_id = $1
`, userID).Scan(&m.Sequence, &m.Wave, &m.Multiplier)
	if errors.Is(err, pgx.ErrNoRows) {
		return Membership{}, false, nil
	}
	if err != nil {
		return Membership{}, false, fmt.Errorf("founding.MembershipFor: %w", err)
	}
	return m, true, nil
}

// WaveProgress is the public view of how full the waves are.
//
// Reports slots **claimed**, never slots remaining. With a community this
// small a remaining-count advertises emptiness - "492 remaining" reads as
// nobody came - while a rising claimed-count reads as momentum. Same data,
// opposite signal. It also matches how applicant counts are already presented
// during a draw: enough to inform, never enough to time an entry against.
type WaveProgress struct {
	CurrentWave      string  `json:"current_wave"`
	Claimed          int     `json:"claimed"`
	Total            int     `json:"total"`
	Multiplier       float64 `json:"multiplier"`
	NextWave         string  `json:"next_wave,omitempty"`
	NextMultiplier   float64 `json:"next_multiplier,omitempty"`
	MembersTotal     int     `json:"members_total"`
	BoundariesLocked bool    `json:"boundaries_locked"`
}

// Progress reports the current wave's fill state.
func Progress(ctx context.Context, pool db.DBPool, cfg map[string]string) (WaveProgress, error) {
	b, locked, err := LockedBoundaries(ctx, pool)
	if err != nil {
		return WaveProgress{}, err
	}
	if !locked {
		b = boundariesFromConfig(cfg)
	}

	var total int
	if err := pool.QueryRow(ctx, `SELECT count(*)::int FROM founding_members`).Scan(&total); err != nil {
		return WaveProgress{}, fmt.Errorf("founding.Progress: count: %w", err)
	}

	p := WaveProgress{MembersTotal: total, BoundariesLocked: locked}
	switch {
	case total < b.FoundingSlots:
		p.CurrentWave, p.Claimed, p.Total, p.Multiplier = WaveFounding, total, b.FoundingSlots, b.MultiplierFounding
		p.NextWave, p.NextMultiplier = WaveTwo, b.MultiplierWaveTwo
	case total < b.FoundingSlots+b.WaveTwoSlots:
		p.CurrentWave = WaveTwo
		p.Claimed, p.Total, p.Multiplier = total-b.FoundingSlots, b.WaveTwoSlots, b.MultiplierWaveTwo
		p.NextWave, p.NextMultiplier = WaveOpen, b.MultiplierOpen
	default:
		p.CurrentWave = WaveOpen
		p.Claimed, p.Total, p.Multiplier = total-b.FoundingSlots-b.WaveTwoSlots, 0, b.MultiplierOpen
	}
	return p, nil
}
