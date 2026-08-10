package hackathon

import (
	"context"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// AutoRevertOOBAssignment reports whether Grainlify may remove an out-of-band
// assignee from a GitHub issue and comment explaining why.
//
// AI-specs.md §2.3, config key auto_revert_oob_assignment: "Automatically
// remove and comment on GitHub-direct (out-of-band) assignments to GrainHack
// issues."
//
// The gate covers the GitHub write only. It deliberately does *not* gate
// Grainlify's own issue_applications reconciliation: an admin switching this
// off is asking us to stop touching their repository, not asking us to stop
// keeping accurate records. The out-of-band assignment is still counted
// either way (§2.3), which is what makes the switch safe to turn off - the
// evidence survives even when the enforcement does not.
//
// **Fails closed.** Any error - missing config, unreachable database, a
// malformed value - returns false, meaning no comment and no assignee
// removal. The two failure directions are not symmetric: not reverting lapses
// for one sync tick and re-converges on the next, while wrongly reverting
// writes to a repository we do not own and notifies a real contributor that
// they were removed. A posted comment cannot be recalled from the inboxes it
// already reached.
//
// Resolution follows the usual cascade. A project inside an active hackathon
// reads that hackathon's value; a project carrying the literal "GrainHack"
// label without any hackathon (see EffectiveGrainHackLabels, which widens
// protection to those) falls through to the global default, which ships as
// true - so nothing changes for anyone who has not turned this off.
func AutoRevertOOBAssignment(ctx context.Context, pool db.DBPool, projectID uuid.UUID) bool {
	hackathonID, _, _, _, found, err := findActiveAcceptedHackathon(ctx, pool, projectID)
	if err != nil {
		return false
	}

	var scope *uuid.UUID
	if found {
		scope = &hackathonID
	}

	v, err := EffectiveValue(ctx, pool, scope, "auto_revert_oob_assignment")
	if err != nil {
		return false
	}
	return v == "true"
}
