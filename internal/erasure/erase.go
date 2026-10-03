package erasure

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/useremail"
)

// step is one statement of the erasure. Listed rather than discovered: an
// erasure that deletes from a table nobody chose is worse than one that misses
// a table, because the miss is visible in TestErase_EveryUserTableIsDecided
// and the over-deletion is not visible anywhere.
type step struct {
	// name is the key in the request's "erased" summary.
	name string
	sql  string
}

// eraseSteps run in order inside one transaction, each with $1 = user id.
// Order matters where one table is found through another: payout addresses
// are read for auth_nonces before they are deleted, and model calls are found
// through the applications that are deleted after them.
var eraseSteps = []step{
	// Sign-in nonces are keyed by address, not user, so they are found
	// through the account's addresses before those go.
	{"auth_nonces", `
DELETE FROM auth_nonces
WHERE lower(address) IN (
  SELECT lower(address) FROM wallets WHERE user_id = $1
  UNION
  SELECT lower(address) FROM contributor_addresses WHERE user_id = $1)`},

	// The access token is here, encrypted. The grant behind it was revoked at
	// GitHub before this ran (see Executor); this removes our copy, and with
	// it the login, avatar and the link GitHub sign-in uses to find the
	// account. After this, signing in with the same GitHub account creates a
	// new, empty one.
	{"github_accounts", `DELETE FROM github_accounts WHERE user_id = $1`},
	{"wallets", `DELETE FROM wallets WHERE user_id = $1`},
	{"contributor_addresses", `DELETE FROM contributor_addresses WHERE user_id = $1`},
	{"oauth_states", `DELETE FROM oauth_states WHERE user_id = $1`},
	{"notifications", `DELETE FROM notifications WHERE user_id = $1`},
	{"notification_preferences", `DELETE FROM notification_preferences WHERE user_id = $1`},
	{"terms_acceptances", `DELETE FROM terms_acceptances WHERE user_id = $1`},
	{"claim_deadline_reminders", `DELETE FROM claim_deadline_reminders WHERE user_id = $1`},
	{"kyc_review_alerts", `DELETE FROM kyc_review_alerts WHERE user_id = $1`},

	// The copies of the KYC decision made when an administrator reset a
	// verification. The audit row itself stays - who reset, when, from which
	// status, under which reason code - because that is the fraud-relevant
	// fact; what goes is the copy of the person's data, the session id, and
	// the two free-text fields an administrator may have written a name into.
	// The row then goes too, ResetRecordRetention after the reset, as it
	// does for every account (retention.go).
	{"kyc_reset_audit", `
UPDATE kyc_reset_audit
SET previous_kyc_data = NULL, previous_session_id = NULL, reason = NULL, note = NULL
WHERE subject_user_id = $1`},
	{"admin_role_audit", `UPDATE admin_role_audit SET note = NULL WHERE subject_user_id = $1 AND note IS NOT NULL`},

	{"issue_applications", `DELETE FROM issue_applications WHERE user_id = $1`},
	// Model calls for the person's GrainHack applications carry the
	// application text and the profile evidence in their request. Their FK is
	// SET NULL, so deleting the applications alone would orphan the copies.
	{"hackathon_model_calls", `
DELETE FROM hackathon_model_calls
WHERE application_id IN (SELECT id FROM hackathon_issue_applications WHERE user_id = $1)`},
	{"hackathon_issue_applications", `DELETE FROM hackathon_issue_applications WHERE user_id = $1`},
	{"hackathon_contributor_profiles", `DELETE FROM hackathon_contributor_profiles WHERE user_id = $1`},
	{"social_follow_submissions", `DELETE FROM social_follow_submissions WHERE user_id = $1`},
	{"org_ratings", `DELETE FROM org_ratings WHERE user_id = $1`},
	{"support_requests", `DELETE FROM support_requests WHERE user_id = $1`},
	// The points programme is frozen and never paid anybody; a balance that
	// can never be redeemed is not a payout record.
	{"point_ledger", `DELETE FROM point_ledger WHERE user_id = $1`},

	// GrainHack records that decided payouts are kept, so the event still
	// adds up; the login in them is replaced. user_id stays, pointing at the
	// tombstone, because the payout lines join through it.
	{"hackathon_assignments", `
UPDATE hackathon_assignments SET github_login = '` + ErasedPlaceholder + `', prior_association = NULL, updated_at = now()
WHERE user_id = $1`},
	{"hackathon_verdicts", `
UPDATE hackathon_verdicts SET github_login = '` + ErasedPlaceholder + `', updated_at = now()
WHERE user_id = $1`},
	{"hackathon_appeals", `
UPDATE hackathon_appeals SET github_login = '` + ErasedPlaceholder + `', reason = '[erased]', updated_at = now()
WHERE user_id = $1`},
	{"hackathon_issue_clarity_ratings", `
UPDATE hackathon_issue_clarity_ratings SET comment = NULL WHERE user_id = $1 AND comment IS NOT NULL`},
	// The draw's pool is kept so the draw can be re-run and checked; the
	// person's login inside it is replaced. The draw does not order by
	// login (internal/hackathon/draw.go), so a replay is unaffected.
	{"hackathon_draws", `
UPDATE hackathon_draws d
SET pool = (
  SELECT jsonb_agg(
           CASE WHEN e->>'user_id' = $1::text
                THEN jsonb_set(e, '{github_login}', to_jsonb('` + ErasedPlaceholder + `'::text))
                ELSE e END
           ORDER BY ord)
  FROM jsonb_array_elements(d.pool) WITH ORDINALITY AS t(e, ord))
WHERE d.pool @> jsonb_build_array(jsonb_build_object('user_id', $1::text))`},
	// A project application belongs to the project, but the contact line is
	// the maintainer's own.
	{"hackathon_project_applications", `
UPDATE hackathon_project_applications SET maintainer_contact = '[erased]', updated_at = now()
WHERE applicant_user_id = $1`},

	// Last: the account row itself becomes the tombstone. The role drops to
	// contributor so an erased administrator holds no privilege even on paper.
	{"users", `
UPDATE users SET
  display_name = NULL, first_name = NULL, last_name = NULL, location = NULL,
  website = NULL, bio = NULL, avatar_url = NULL, telegram = NULL, linkedin = NULL,
  whatsapp = NULL, twitter = NULL, discord = NULL, payout_contact_email = NULL,
  github_user_id = NULL, referral_code = NULL,
  kyc_status = NULL, kyc_session_id = NULL, kyc_verified_at = NULL, kyc_data = NULL,
  kyc_reconciled_at = NULL,
  email_notifications_enabled = false,
  role = 'contributor',
  erased_at = now(), updated_at = now()
WHERE id = $1`},
}

// eraseRows runs every step and returns the rows each touched.
func eraseRows(ctx context.Context, pool Pool, userID uuid.UUID) (map[string]int64, error) {
	counts := make(map[string]int64, len(eraseSteps)+1)
	for _, s := range eraseSteps {
		tag, err := pool.Exec(ctx, s.sql, userID)
		if err != nil {
			return nil, fmt.Errorf("erasure: %s: %w", s.name, err)
		}
		counts[s.name] = tag.RowsAffected()
	}
	// The address goes through the package that owns the column, which is the
	// only code allowed to write it (useremail's reader guard).
	if err := useremail.Remove(ctx, pool, userID); err != nil {
		return nil, fmt.Errorf("erasure: email: %w", err)
	}
	counts["users.email"] = 1
	return counts, nil
}

// keptTables are tables that reference a person and that erasure leaves as
// they are, each with the reason. Together with eraseSteps it must cover
// every table with a foreign key into users; the test that checks it is what
// makes a table added next year a decision rather than an omission.
var keptTables = map[string]string{
	"settlement_lines":               "payout record: what each person was allotted in a settlement",
	"settlement_holds":               "payout record: an amount held back, and when it was released",
	"sponsored_claims":               "payout record: the transaction that collected a claim",
	"keeperhub_payout_legs":          "payout record: amount, address and transaction of a transfer",
	"keeperhub_payout_exclusions":    "payout record: why a person was left out of a run",
	"redemptions":                    "payout record (points redemptions; none were ever made)",
	"hackathon_maintainer_payouts":   "payout record for a project's maintainers",
	"founding_members":               "Founding Pool wave counts are public and computed from these; no personal data",
	"founding_shares":                "other members' shares are computed alongside these; no personal data",
	"referrals":                      "the other person's Founding Pool shares are computed from the link; no personal data",
	"projects":                       "a project belongs to its repository, not to the account that listed it",
	"hackathon_oob_assignments":      "a record about a maintainer's conduct, keyed by the maintainer",
	"kyc_reset_audit":                "audit of an administrator decision; its personal fields are cleared by an erase step, the row by retention after 90 days",
	"admin_role_audit":               "audit of a role change; its note is cleared by an erase step",
	"chain_operations":               "audit of an administrator action (actor only)",
	"config_audit":                   "audit of an administrator action (actor only)",
	"hackathon_config_settings":      "who last changed a setting (actor only)",
	"hackathons":                     "who created or changed an event (actor only)",
	"hackathon_payout_runs":          "who computed a payout run (actor only)",
	"keeperhub_dispatch_attempts":    "who dispatched a payout (actor only)",
	"keeperhub_payout_runs":          "who released a payout run (actor only)",
	"org_social_links":               "who last edited an organisation's links (actor only)",
	"social_follow_decisions":        "who decided a submission (actor only); the person's own submissions are deleted",
	"social_follow_submissions":      "deleted by an erase step when the person is the submitter; kept when they only decided one",
	"hackathon_project_applications": "the project's application; the person's contact line is cleared by an erase step",
	"account_deletion_requests":      "the record of this deletion",
}
