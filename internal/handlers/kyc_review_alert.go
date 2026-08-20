package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// Telling an admin that a verification is waiting on them.
//
// "In Review" does not mean Didit is working on it. Their documentation is
// explicit - `status='In Review'` means route it to YOUR manual-review queue -
// so the reviewer is us, in Didit's console. Nothing surfaced that queue: the
// webhook handler updated the row and notified nobody, and the only path that
// re-read Didit was the status poll, which runs when the contributor opens
// their billing page. Somebody who has been told to wait has no reason to open
// it, so the queue became visible only when a contributor complained.
//
// The cost is not just delay. Founding-pool waves are allocated first-come at
// the moment verification completes, under a global lock, and the multiplier
// that comes with a wave is permanent. Every hour a flagged session sits
// unreviewed, other people take slots ahead of that contributor.

// kycReviewDetail is what an admin needs in order to decide before opening the
// console, extracted from the Didit decision we already store in kyc_data.
type kycReviewDetail struct {
	// BlockingCheck names the sub-check sitting in review - "face match",
	// "ID verification" - rather than reporting the session as a whole.
	BlockingCheck string
	// Warnings are Didit's own machine-readable codes.
	Warnings []string
	// AttemptsExceeded is the one that changes the answer.
	// FACE_MATCH_MAX_ATTEMPTS_EXCEEDED means the contributor cannot retry even
	// if handed a working link, so a reviewer has to decide. It is the reason
	// "there is a resume URL" is not the same claim as "the user can act".
	AttemptsExceeded bool
	// SessionStartedAt is Didit's own created_at for the session. Age is read
	// from this rather than users.updated_at, which moves for unrelated
	// reasons.
	SessionStartedAt *time.Time
}

// kycCheckLabels turns Didit's field names into something readable. It is a
// display aid ONLY - the set of checks is discovered from the payload, never
// from this map. Charles's session carried an `ip_analysis` check that a
// hardcoded list did not have, and a list is a guess about what Didit runs:
// when it is wrong the alert says "Blocked on:" and names nothing, which is
// the one line the reviewer needs.
var kycCheckLabels = map[string]string{
	"face_match":      "face match (selfie)",
	"id_verification": "ID document",
	"liveness":        "liveness",
	"aml":             "AML screening",
	"poa":             "proof of address",
	"nfc":             "NFC chip read",
	"ip_analysis":     "IP analysis",
	"phone":           "phone verification",
	"email":           "email verification",
}

func kycCheckLabel(key string) string {
	if l, ok := kycCheckLabels[key]; ok {
		return l
	}
	return strings.ReplaceAll(key, "_", " ")
}

// extractKYCReviewDetail reads the stored decision. Best-effort by design: a
// missing or reshaped field costs detail in a notification, and must never stop
// the notification being sent.
func extractKYCReviewDetail(kycData []byte) kycReviewDetail {
	var d kycReviewDetail
	if len(kycData) == 0 {
		return d
	}
	var m map[string]any
	if err := json.Unmarshal(kycData, &m); err != nil {
		return d
	}

	if s, ok := m["created_at"].(string); ok && s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			d.SessionStartedAt = &t
		}
	}

	// Any top-level object carrying its own status is a check. Discovering them
	// this way means a check we have never seen still gets named.
	blocking := []string{}
	for key, v := range m {
		obj, ok := v.(map[string]any)
		if !ok {
			continue
		}
		st, ok := obj["status"].(string)
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(st), "In Review") {
			blocking = append(blocking, kycCheckLabel(key))
		}
	}
	sort.Strings(blocking) // Map iteration order is random; the message must not be.
	d.BlockingCheck = strings.Join(blocking, ", ")

	// Warnings are nested per check, so collect them wherever they appear
	// rather than assuming a shape that has already changed once.
	seen := map[string]bool{}
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			if ws, ok := t["warnings"].([]any); ok {
				for _, w := range ws {
					if wm, ok := w.(map[string]any); ok {
						if risk, _ := wm["risk"].(string); risk != "" && !seen[risk] {
							seen[risk] = true
							d.Warnings = append(d.Warnings, risk)
						}
					}
				}
			}
			for _, vv := range t {
				walk(vv)
			}
		case []any:
			for _, vv := range t {
				walk(vv)
			}
		}
	}
	walk(m)
	sort.Strings(d.Warnings)

	// Matched on ATTEMPTS_EXCEEDED alone. The stored payload says
	// FACE_MATCH_MAX_ATTEMPTS_EXCEEDED and the Didit console displays the same
	// condition as MAXIMUM_FACE_MATCH_ATTEMPTS_EXCEEDED, so there are already
	// two spellings in circulation for one fact. Matching the narrower of them
	// fails silently: no flag, and the interface then offers a retry to
	// somebody who has no attempts left.
	//
	// Note also that Didit tags this warning log_type "information", not
	// "warning". Filtering on severity would drop the one code that decides
	// whether a human has to intervene.
	for _, w := range d.Warnings {
		if strings.Contains(w, "ATTEMPTS_EXCEEDED") {
			d.AttemptsExceeded = true
		}
	}
	return d
}

// alertAdminOfKYCReview sends at most one notification per session.
//
// The insert is the lock. ON CONFLICT DO NOTHING means the first caller to
// claim a session id wins and everybody else is a no-op, so the webhook and
// the sweep racing on the same session produce one message rather than two.
// Sending only when the insert actually happened is what makes "once" true
// rather than intended.
//
// Best-effort throughout: the row is already written by the time this runs,
// and a failed notification must never fail a verification update.
func alertAdminOfKYCReview(
	ctx context.Context, d *db.DB, sink SupportSink, userID uuid.UUID, sessionID, source string,
) {
	if d == nil || d.Pool == nil || sessionID == "" {
		return
	}

	tag, err := d.Pool.Exec(ctx, `
INSERT INTO kyc_review_alerts (session_id, user_id, source)
VALUES ($1, $2, $3)
ON CONFLICT (session_id) DO NOTHING
`, sessionID, userID, source)
	if err != nil {
		slog.Error("kyc review alert: could not claim session", "session_id", sessionID, "error", err)
		return
	}
	if tag.RowsAffected() == 0 {
		return // Already alerted for this session.
	}

	if sink == nil || !sink.Configured() {
		slog.Warn("kyc review alert: no sink configured, nobody was told a verification is waiting",
			"session_id", sessionID, "user_id", userID)
		return
	}

	var login string
	var kycData []byte
	_ = d.Pool.QueryRow(ctx, `
SELECT COALESCE(ga.login, ''), u.kyc_data
FROM users u LEFT JOIN github_accounts ga ON ga.user_id = u.id
WHERE u.id = $1
`, userID).Scan(&login, &kycData)

	detail := extractKYCReviewDetail(kycData)
	if login == "" {
		login = userID.String()
	}

	if _, err := sink.Deliver(ctx, SupportRequest{
		ID:       uuid.New(),
		Category: "kyc", // Routes to the admin DM and never to the public group.
		Message:  buildKYCReviewMessage(login, sessionID, source, detail),
	}); err != nil {
		// The row stays claimed. Re-sending on the next sweep would be worse:
		// it turns one failure into a repeating alert, and a repeating alert
		// gets muted.
		slog.Error("kyc review alert: DELIVERY FAILED - a verification is waiting and nobody was told",
			"session_id", sessionID, "user_id", userID, "login", login, "error", err)
	}
}

func buildKYCReviewMessage(login, sessionID, source string, d kycReviewDetail) string {
	var b strings.Builder
	b.WriteString("🪪 Verification waiting for review\n\n")
	b.WriteString("Contributor: " + login + "\n")

	if d.SessionStartedAt != nil {
		age := time.Since(*d.SessionStartedAt)
		b.WriteString(fmt.Sprintf("Waiting: %dh %dm (since %s)\n",
			int(age.Hours()), int(age.Minutes())%60, d.SessionStartedAt.UTC().Format("2 Jan 15:04 MST")))
	}
	if d.BlockingCheck != "" {
		b.WriteString("Blocked on: " + d.BlockingCheck + "\n")
	}
	if len(d.Warnings) > 0 {
		b.WriteString("Warnings: " + strings.Join(d.Warnings, ", ") + "\n")
	}
	if d.AttemptsExceeded {
		// The line that decides the action, so it is stated rather than left
		// to be inferred from a code.
		b.WriteString("\n⚠️ Retry attempts exhausted - they cannot fix this themselves.\n")
	}

	// The session id, and deliberately no link.
	//
	// An earlier version built one from the shape of Didit's dashboard URLs,
	// which is a guess: nobody had opened it. A link that 404s in an alert is
	// worse than no link - it sends the reader somewhere useless at the moment
	// they are trying to act, and it looks authoritative while being wrong.
	// The id is what identifies the session in the console however that console
	// is addressed, so it is the part that cannot be wrong.
	b.WriteString("\nSession: " + sessionID)
	if source == "sweep" {
		// Worth knowing: it means Didit's webhook did not reach us. Didit
		// retries twice and then drops the delivery.
		b.WriteString("\n\n(found by the periodic sweep, not the webhook)")
	}
	return b.String()
}

// alertAdminOfKYCReversal tells an admin that somebody who was verified no
// longer is, and that we found out by asking rather than by being told.
//
// # No claim table, unlike its neighbour
//
// alertAdminOfKYCReview dedupes through kyc_review_alerts because it fires on
// a STATE ("this session is in review"), which stays true and would otherwise
// re-alert on every sweep. This fires on a TRANSITION, and the reconciler
// writes the new status immediately afterwards, so the condition that produced
// it is gone before the next tick. The state itself is the dedupe. Adding a
// claim keyed on session_id would be worse than redundant: it would collide
// with the review alerts already keyed there and silence one of the two.
//
// # What it deliberately does not say
//
// No warnings, no blocking check, no decision reason. The review alert carries
// those because they decide an action - which reason code to send. This one
// decides nothing: the action is "go and look", and the detail lives in the
// provider console where an admin reads it under their own login rather than
// in a Telegram message that outlives the decision.
//
// Category "kyc" routes to the admin DM and never to the public group.
func alertAdminOfKYCReversal(
	ctx context.Context, d *db.DB, sink SupportSink, userID uuid.UUID, sessionID, was, now string,
) {
	if d == nil || d.Pool == nil || sessionID == "" {
		return
	}
	if sink == nil || !sink.Configured() {
		slog.Warn("kyc reversal alert: no sink configured, nobody was told a verification was reversed",
			"session_id", sessionID, "user_id", userID, "was", was, "now", now)
		return
	}

	var login string
	_ = d.Pool.QueryRow(ctx, `
SELECT COALESCE(ga.login, '')
FROM users u LEFT JOIN github_accounts ga ON ga.user_id = u.id
WHERE u.id = $1
`, userID).Scan(&login)
	if login == "" {
		login = userID.String()
	}

	var b strings.Builder
	b.WriteString("⚠️ Verification reversed\n\n")
	b.WriteString("Contributor: " + login + "\n")
	b.WriteString("Was: " + was + "\n")
	b.WriteString("Now: " + now + "\n")
	b.WriteString("Session: " + sessionID + "\n\n")
	b.WriteString("Found by reconciliation, which means the webhook for this change never arrived.\n")
	b.WriteString("Open the session in the Didit console for the reason.")

	if _, err := sink.Deliver(ctx, SupportRequest{
		ID:       uuid.New(),
		Category: "kyc",
		Message:  b.String(),
	}); err != nil {
		slog.Error("kyc reversal alert: DELIVERY FAILED - a verification was reversed and nobody was told",
			"session_id", sessionID, "user_id", userID, "login", login, "was", was, "now", now, "error", err)
	}
}
