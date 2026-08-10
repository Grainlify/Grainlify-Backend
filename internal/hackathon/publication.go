package hackathon

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// publicationPlan is what gets stamped onto an issue at the moment it
// publishes: whether it's newcomer-reserved (§3.8) and its application
// window (§3.7).
type publicationPlan struct {
	reserved     bool
	windowOpens  time.Time
	windowCloses time.Time
}

// decideReserved implements AI-specs.md §3.8's newcomer reservation.
//
// The percentage is applied as a *running ratio over already-published
// issues of the same difficulty tier*, evaluated at publication time. §3.8
// is explicit that reserved status is assigned "at issue publication, not at
// draw time, so it cannot be steered by who happens to apply" - which also
// rules out picking a random subset once the event is under way.
//
// Publishing issues one at a time means we can't know the final tier total,
// so the target is recomputed against the count *including* this issue and
// this issue is reserved whenever the tier is still short of it.
//
//	target(n) = 0                          for n < 2
//	target(n) = max(1, floor(n * pct/100)) for n >= 2
//
// The max(1, ...) floor is the important part. A pure ratio rounds down to
// zero in a small tier, so a first event - which is exactly the event most
// likely to have two or three easy issues - would ship with no newcomer
// reservation at all, in the tier where it matters most. The floor
// guarantees at least one reserved issue as soon as a tier has two.
//
// A tier with exactly one issue still reserves nothing: reserving it would
// make the only issue at that difficulty newcomer-only, locking every other
// contributor out of the tier entirely.
func decideReserved(publishedInTier, reservedInTier, pct int) bool {
	if pct <= 0 {
		return false
	}
	total := publishedInTier + 1
	if total < 2 {
		return false
	}
	if pct >= 100 {
		return true
	}
	target := total * pct / 100
	if target < 1 {
		target = 1
	}
	return reservedInTier < target
}

// planPublication resolves the reserved flag and application window for an
// issue about to publish.
func planPublication(ctx context.Context, pool db.DBPool, hackathonID, issueID uuid.UUID, tier string) (*publicationPlan, error) {
	vals, err := EffectiveValues(ctx, pool, &hackathonID)
	if err != nil {
		return nil, fmt.Errorf("hackathon.planPublication: config: %w", err)
	}

	plan := &publicationPlan{}

	if vals["newcomer_reservation_enabled"] == "true" {
		pctKey := map[string]string{
			"easy":     "reserved_pct_easy",
			"standard": "reserved_pct_standard",
			"advanced": "reserved_pct_advanced",
		}[tier]
		if pctKey != "" {
			var publishedInTier, reservedInTier int
			if err := pool.QueryRow(ctx, `
SELECT count(*), count(*) FILTER (WHERE reserved)
FROM hackathon_issues
WHERE hackathon_id = $1 AND difficulty_tier = $2 AND status = 'published' AND id <> $3
`, hackathonID, tier, issueID).Scan(&publishedInTier, &reservedInTier); err != nil {
				return nil, fmt.Errorf("hackathon.planPublication: tier counts: %w", err)
			}
			plan.reserved = decideReserved(publishedInTier, reservedInTier, atoiOr(vals[pctKey], 0))
		}
	}

	// The window can't open before the event is live, however early the
	// issue was prepared.
	var startsAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT starts_at FROM hackathons WHERE id = $1`, hackathonID).Scan(&startsAt); err != nil {
		return nil, fmt.Errorf("hackathon.planPublication: load starts_at: %w", err)
	}
	opens := time.Now().UTC()
	if startsAt != nil && startsAt.After(opens) {
		opens = startsAt.UTC()
	}
	plan.windowOpens = opens
	plan.windowCloses = opens.Add(time.Duration(atoiOr(vals["application_window_hours"], 24)) * time.Hour)
	return plan, nil
}

// ReopenWindow extends a closed window by another application_window_hours,
// used by the draw runner when a window closed with nobody in the pool
// (§3.7's empty_window_retries).
func ReopenWindow(ctx context.Context, pool db.DBPool, hackathonID, issueID uuid.UUID) error {
	hours, err := EffectiveValue(ctx, pool, &hackathonID, "application_window_hours")
	if err != nil {
		return err
	}
	opens := time.Now().UTC()
	closes := opens.Add(time.Duration(atoiOr(hours, 24)) * time.Hour)
	_, err = pool.Exec(ctx, `
UPDATE hackathon_issues
SET application_window_opens_at = $2, application_window_closes_at = $3, updated_at = now()
WHERE id = $1
`, issueID, opens, closes)
	return err
}
