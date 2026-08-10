package hackathon

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/github"
	"github.com/jagadeesh/grainlify/backend/internal/notifications"
)

type hackathonIssueRow struct {
	ID                 uuid.UUID
	Status             string
	AcceptanceCriteria string
	DifficultyTier     string
}

// isAssigned reports whether issueNumber currently has a real Grainlify
// assignment. AI-specs.md §2.2: "Label removed: if the issue is unassigned,
// it leaves the hackathon. If already assigned, it stays in the hackathon...
// Flag it for admin visibility."
//
// Until the assignment pipeline existed this was a hard-false stub, which
// meant pulling the label off an actively-assigned issue silently dropped
// that contributor's in-flight work. It now consults the real assignment
// record, so that case flags for admin instead.
//
// A read failure returns true (assume assigned): flagging an unassigned
// issue for admin review is a harmless false positive, whereas the other
// direction discards someone's work.
func isAssigned(ctx context.Context, pool db.DBPool, hackathonID, projectID uuid.UUID, issueNumber int) bool {
	login, err := ActiveAssignee(ctx, pool, projectID, issueNumber)
	if err != nil {
		return true
	}
	return login != ""
}

// SyncIssueLabel is called once per (project, issue) from
// syncjobs.Worker.syncIssues()'s per-issue loop, immediately after the
// existing assignment-eligibility reconciliation. It keeps projectID's
// hackathon_issues row in sync with the issue's current label/state -
// AI-specs.md §2.2. A no-op (nil, nil) if projectID has no accepted
// application into a hackathon currently in issue_prep or live phase
// ("acceptance is the gate" - §2.1).
func SyncIssueLabel(
	ctx context.Context,
	pool db.DBPool,
	gh *github.Client,
	notifier *notifications.Service,
	getInstallationToken func() (string, error),
	projectID uuid.UUID,
	fullName string,
	issueNumber int,
	labelNames []string,
	isOpen bool,
	primaryLanguageFn func() (string, error),
) error {
	hackathonID, phase, endsAt, ownerUserID, found, err := findActiveAcceptedHackathon(ctx, pool, projectID)
	if err != nil {
		return fmt.Errorf("hackathon.SyncIssueLabel: %w", err)
	}
	if !found {
		return nil
	}

	label, err := EffectiveValue(ctx, pool, &hackathonID, "grainhack_label")
	if err != nil {
		return fmt.Errorf("hackathon.SyncIssueLabel: resolve label: %w", err)
	}
	hasLabel := HasLabel(labelNames, []string{label, LegacyGrainHackLabel})

	existing, err := loadHackathonIssue(ctx, pool, hackathonID, projectID, issueNumber)
	if err != nil {
		return fmt.Errorf("hackathon.SyncIssueLabel: load existing row: %w", err)
	}

	if !hasLabel {
		if existing == nil || existing.Status == "removed" {
			return nil
		}
		if isAssigned(ctx, pool, hackathonID, projectID, issueNumber) {
			_, err := pool.Exec(ctx, `
UPDATE hackathon_issues SET flagged_for_admin = true, flagged_reason = 'Label removed while assigned', updated_at = now()
WHERE id = $1
`, existing.ID)
			return err
		}
		_, err := pool.Exec(ctx, `
UPDATE hackathon_issues SET status = 'removed', removed_at = now(), updated_at = now()
WHERE id = $1
`, existing.ID)
		return err
	}

	// Label is present from here on.
	if existing != nil && existing.Status != "removed" {
		_, err := pool.Exec(ctx, `UPDATE hackathon_issues SET synced_at = now() WHERE id = $1`, existing.ID)
		return err
	}

	if !isOpen {
		// Don't create a fresh entry for an already-closed issue; an
		// existing removed row is left as-is rather than resurrected.
		return nil
	}

	if phase == "live" {
		allowLate, err := EffectiveValue(ctx, pool, &hackathonID, "allow_late_issue_entry")
		if err != nil {
			return err
		}
		if allowLate != "true" {
			return nil
		}
		if endsAt != nil {
			cutoffStr, err := EffectiveValue(ctx, pool, &hackathonID, "late_entry_cutoff_hours")
			if err != nil {
				return err
			}
			cutoffHours, _ := strconv.Atoi(cutoffStr)
			if time.Until(*endsAt) < time.Duration(cutoffHours)*time.Hour {
				return nil
			}
		}
	}

	orgLogin := orgLoginFromFullName(fullName)
	maxStr, err := EffectiveValue(ctx, pool, &hackathonID, "max_issues_per_org")
	if err != nil {
		return err
	}
	maxPerOrg, _ := strconv.Atoi(maxStr)

	var currentCount int
	if err := pool.QueryRow(ctx, `
SELECT COUNT(*) FROM hackathon_issues WHERE hackathon_id = $1 AND org_login = $2 AND status != 'removed'
`, hackathonID, orgLogin).Scan(&currentCount); err != nil {
		return fmt.Errorf("hackathon.SyncIssueLabel: count org issues: %w", err)
	}

	if currentCount >= maxPerOrg {
		rejectOverCapIssue(ctx, gh, notifier, getInstallationToken, ownerUserID, fullName, issueNumber, orgLogin, maxPerOrg)
		return nil
	}

	if existing != nil && existing.Status == "removed" {
		// Re-entry: restore to pending using whatever fields survived
		// removal (never cleared on removal), then let MaybePublish decide
		// whether they're already sufficient to republish immediately.
		if _, err := pool.Exec(ctx, `
UPDATE hackathon_issues
SET status = 'pending', removed_at = NULL, synced_at = now(), updated_at = now()
WHERE id = $1
`, existing.ID); err != nil {
			return err
		}
		return MaybePublish(ctx, pool, hackathonID, existing.ID)
	}

	primaryLanguage := ""
	if primaryLanguageFn != nil {
		if lang, err := primaryLanguageFn(); err == nil {
			primaryLanguage = lang
		}
	}

	_, err = pool.Exec(ctx, `
INSERT INTO hackathon_issues (hackathon_id, project_id, issue_number, org_login, status, primary_language, synced_at)
VALUES ($1, $2, $3, $4, 'pending', $5, now())
ON CONFLICT (hackathon_id, project_id, issue_number) DO NOTHING
`, hackathonID, projectID, issueNumber, orgLogin, primaryLanguage)
	if err != nil {
		return fmt.Errorf("hackathon.SyncIssueLabel: insert: %w", err)
	}
	return nil
}

// MaybePublish flips a pending hackathon_issues row to published once both
// required fields are set (subject to require_acceptance_criteria/
// require_difficulty_tier). Called by the maintainer-facing UpdateFields
// handler after a field save, and by SyncIssueLabel's re-entry path (a
// removed row restores with whatever fields survived removal, which may
// already be sufficient to republish immediately).
func MaybePublish(ctx context.Context, pool db.DBPool, hackathonID uuid.UUID, issueID uuid.UUID) error {
	requireCriteria, err := EffectiveValue(ctx, pool, &hackathonID, "require_acceptance_criteria")
	if err != nil {
		return err
	}
	requireTier, err := EffectiveValue(ctx, pool, &hackathonID, "require_difficulty_tier")
	if err != nil {
		return err
	}

	var status, acceptanceCriteria, difficultyTier string
	if err := pool.QueryRow(ctx, `
SELECT status, COALESCE(acceptance_criteria, ''), COALESCE(difficulty_tier, '') FROM hackathon_issues WHERE id = $1
`, issueID).Scan(&status, &acceptanceCriteria, &difficultyTier); err != nil {
		return err
	}
	if status != "pending" {
		return nil
	}
	if requireCriteria == "true" && acceptanceCriteria == "" {
		return nil
	}
	if requireTier == "true" && difficultyTier == "" {
		return nil
	}

	// Newcomer reservation (§3.8) and the application window (§3.7) are
	// stamped here, at the moment of publication, and never recomputed -
	// §3.8 requires reserved status to be fixed before anyone can see who
	// applied, so it "cannot be steered by who happens to apply".
	plan, err := planPublication(ctx, pool, hackathonID, issueID, difficultyTier)
	if err != nil {
		return err
	}
	_, err = pool.Exec(ctx, `
UPDATE hackathon_issues
SET status = 'published',
    published_at = now(),
    reserved = $2,
    application_window_opens_at = $3,
    application_window_closes_at = $4,
    updated_at = now()
WHERE id = $1
`, issueID, plan.reserved, plan.windowOpens, plan.windowCloses)
	return err
}

func rejectOverCapIssue(ctx context.Context, gh *github.Client, notifier *notifications.Service, getInstallationToken func() (string, error), ownerUserID uuid.UUID, fullName string, issueNumber int, orgLogin string, maxPerOrg int) {
	body := fmt.Sprintf("This issue was not entered into GrainHack: %s has already reached its cap of %d issues for this event.", orgLogin, maxPerOrg)
	if gh != nil && getInstallationToken != nil {
		if tok, err := getInstallationToken(); err == nil {
			_, _ = gh.CreateIssueComment(ctx, tok, fullName, issueNumber, body)
		}
	}
	if notifier != nil {
		notifier.Notify(ctx, ownerUserID, notifications.TypeGrainHackIssueCapExceeded,
			"GrainHack issue cap reached",
			fmt.Sprintf("%s#%d was not entered into GrainHack: your org has reached its cap of %d issues.", fullName, issueNumber, maxPerOrg),
			"",
		)
	}
}

func loadHackathonIssue(ctx context.Context, pool db.DBPool, hackathonID, projectID uuid.UUID, issueNumber int) (*hackathonIssueRow, error) {
	var row hackathonIssueRow
	err := pool.QueryRow(ctx, `
SELECT id, status, COALESCE(acceptance_criteria, ''), COALESCE(difficulty_tier, '')
FROM hackathon_issues WHERE hackathon_id = $1 AND project_id = $2 AND issue_number = $3
`, hackathonID, projectID, issueNumber).Scan(&row.ID, &row.Status, &row.AcceptanceCriteria, &row.DifficultyTier)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &row, nil
}

// findActiveAcceptedHackathon returns the most recently accepted hackathon
// (issue_prep or live phase) for projectID, if any.
func findActiveAcceptedHackathon(ctx context.Context, pool db.DBPool, projectID uuid.UUID) (hackathonID uuid.UUID, phase string, endsAt *time.Time, ownerUserID uuid.UUID, found bool, err error) {
	err = pool.QueryRow(ctx, `
SELECT h.id, h.phase, h.ends_at, p.owner_user_id
FROM hackathon_project_applications hpa
JOIN hackathons h ON h.id = hpa.hackathon_id
JOIN projects p ON p.id = hpa.project_id
WHERE hpa.project_id = $1 AND hpa.status = 'accepted' AND h.phase IN ('issue_prep', 'live')
ORDER BY hpa.created_at DESC
LIMIT 1
`, projectID).Scan(&hackathonID, &phase, &endsAt, &ownerUserID)
	if err != nil {
		if err == pgx.ErrNoRows {
			return uuid.Nil, "", nil, uuid.Nil, false, nil
		}
		return uuid.Nil, "", nil, uuid.Nil, false, err
	}
	return hackathonID, phase, endsAt, ownerUserID, true, nil
}

func orgLoginFromFullName(fullName string) string {
	parts := strings.SplitN(fullName, "/", 2)
	return parts[0]
}
