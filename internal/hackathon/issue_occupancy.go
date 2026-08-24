package hackathon

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// The rule about who holds an issue, in one place.
//
// # The two concepts, and why they are not one
//
// An assignment status answers two different questions, and the old code used
// one list for both:
//
//	OCCUPIES the issue   - active, pr_submitted, completed   (blocks a re-draw)
//	IN FLIGHT             - active, pr_submitted             (open work to release or warn about)
//
// They differ by exactly one state - completed - and that difference is the
// whole defect. Nine call sites wrote status IN ('active','pr_submitted').
// Three of them meant "occupies" and were wrong; the rest meant "in flight" and
// were right. Applying either list uniformly breaks the other half: teach the
// close path that completed work is in flight and it releases finished
// assignments and emails those contributors a deadline warning about a PR they
// already merged.
//
// So both are derived from one enumerated set - the released states - defined
// in SQL by hackathon_assignment_released() and mirrored here.
//
// # Why the released set is the one written down
//
// Enumerating the occupying states means a status nobody updated here defaults
// to "issue is free", which is how completed slipped through. Enumerating the
// released states means it defaults to "issue is taken". The forgotten case
// lands on the cautious side, where the symptom is an issue that stayed closed
// too long and a person notices, rather than finished work handed to a second
// contributor.
//
// The evidence that this is not hypothetical: judging_intake.go has always
// written ('active','pr_submitted','completed') - the correct three - while
// every other site stayed at two. The knowledge existed in the repository and
// did not propagate, which is what a hand-maintained list does.
const releasedStatusesSQL = `('released_stale', 'released_voluntary', 'released_event_end')`

// occupiesIssueSQL is the predicate for "this row still holds its issue".
// Written against the SQL function so the index and the queries cannot drift.
const occupiesIssueSQL = `NOT hackathon_assignment_released(status)`

// inFlightSQL is "open work": occupies the issue and is not finished.
const inFlightSQL = `NOT hackathon_assignment_released(status) AND status <> 'completed'`

// ErrIssueFinished means the issue has an assignment that was carried to
// completion, and must never be advertised again.
var ErrIssueFinished = errors.New("issue already has a completed assignment")

// IssueIsFinished reports whether an issue's work is done.
//
// # Why this is exported and not a query inlined at each caller
//
// The gate this replaces was going to live in runDueDraws, on the reasoning
// that runDueDraws is what calls ReopenWindow. That reasoning was wrong, and
// checking rather than assuming is what showed it: ReopenWindow has THREE
// callers.
//
//	runner.go       the empty-window retry      (the path the defect was found on)
//	runner.go       after a stale release
//	handlers        after a contributor releases voluntarily, over HTTP
//
// A gate at runDueDraws would have covered one of the three and left a handler
// able to re-advertise finished work directly. "Its only caller checks first"
// is a fact about today, not an invariant, and the next caller will not know to
// ask.
//
// So the check lives at the choke point instead: ReopenWindow itself calls
// this, which covers all three and every caller added later, and the draw
// selection uses the same underlying predicate so a finished issue is never
// even considered. One rule; nothing to keep in step.
func IssueIsFinished(ctx context.Context, pool db.DBPool, issueID uuid.UUID) (bool, error) {
	var finished bool
	if err := pool.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1 FROM hackathon_assignments
  WHERE hackathon_issue_id = $1 AND status = 'completed'
)`, issueID).Scan(&finished); err != nil {
		return false, fmt.Errorf("hackathon.IssueIsFinished: %w", err)
	}
	return finished, nil
}
