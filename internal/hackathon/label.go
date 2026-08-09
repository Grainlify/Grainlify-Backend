package hackathon

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// LegacyGrainHackLabel is the literal label string internal/syncjobs/worker.go
// has always checked, from before this hackathon data model existed. It
// stays permanently protected regardless of any hackathon's configured
// grainhack_label, so an issue outside the new structured system (or a
// hackathon that never overrides the default) keeps working exactly as
// before.
const LegacyGrainHackLabel = "GrainHack"

// EffectiveGrainHackLabels returns every label name that should be treated
// as "this issue belongs to GrainHack" for projectID: the permanent legacy
// literal, plus - only if projectID has an accepted application into a
// hackathon currently in issue_prep or live phase - that hackathon's
// configured grainhack_label (which may differ from the legacy literal, if
// an admin changed it for this event). This only ever widens what's
// protected; it never narrows or replaces the legacy check.
func EffectiveGrainHackLabels(ctx context.Context, pool db.DBPool, projectID uuid.UUID) ([]string, error) {
	labels := []string{LegacyGrainHackLabel}

	var hackathonID uuid.UUID
	err := pool.QueryRow(ctx, `
SELECT h.id
FROM hackathon_project_applications hpa
JOIN hackathons h ON h.id = hpa.hackathon_id
WHERE hpa.project_id = $1 AND hpa.status = 'accepted' AND h.phase IN ('issue_prep', 'live')
ORDER BY hpa.created_at DESC
LIMIT 1
`, projectID).Scan(&hackathonID)
	if err != nil {
		if err == pgx.ErrNoRows {
			return labels, nil
		}
		return nil, fmt.Errorf("hackathon.EffectiveGrainHackLabels: %w", err)
	}

	configuredLabel, err := EffectiveValue(ctx, pool, &hackathonID, "grainhack_label")
	if err != nil {
		return nil, fmt.Errorf("hackathon.EffectiveGrainHackLabels: %w", err)
	}
	if configuredLabel != "" && !strings.EqualFold(configuredLabel, LegacyGrainHackLabel) {
		labels = append(labels, configuredLabel)
	}
	return labels, nil
}

// HasLabel reports whether any of candidateLabels appears in issueLabels,
// case-insensitively.
func HasLabel(issueLabels []string, candidateLabels []string) bool {
	for _, il := range issueLabels {
		for _, cl := range candidateLabels {
			if strings.EqualFold(il, cl) {
				return true
			}
		}
	}
	return false
}
