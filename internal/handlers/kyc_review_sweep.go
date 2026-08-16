package handlers

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/config"
	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// The fallback for a webhook that never arrived.
//
// Didit's delivery guarantee is weak by their own documentation: on failure it
// retries twice - about a minute after the first failure, then about four
// minutes after that - and then the delivery is dropped. A five-minute outage
// on our side therefore loses the event permanently, and the only other thing
// that re-reads Didit is the status poll, which runs when the CONTRIBUTOR
// opens their billing page. Somebody who has been told to wait has no reason
// to open it.
//
// So a dropped webhook means nobody ever learns that a verification is waiting
// on a reviewer. That is the state this sweep exists to make impossible.
//
// What it deliberately does NOT do:
//
//   - It does not change anybody's kyc_status. It is an alerting backstop, not
//     a retry rule. Letting users re-attempt while a session is legitimately
//     under review produces duplicate sessions, and if the flagged document
//     flags again the second session lands in the same queue - turning
//     "stuck and visible" into "churning and invisible".
//   - It does not re-alert. Claiming is done through kyc_review_alerts, whose
//     primary key is the session, so a session already alerted about is silent
//     however many times this runs. An alert that repeats gets muted, and a
//     muted alert is the queue we already had.

// KYCReviewSweeper finds flagged sessions nobody has been told about.
type KYCReviewSweeper struct {
	db   *db.DB
	sink SupportSink
	// minAge is how long a session must have been in review before it is worth
	// mentioning. Not a staleness or retry threshold - purely "the webhook has
	// had time to arrive". Didit's own retry schedule finishes around five
	// minutes, so anything past that is a delivery that is not coming.
	minAge time.Duration
	// interval is how often the sweep runs.
	interval time.Duration
}

func NewKYCReviewSweeper(cfg config.Config, d *db.DB) *KYCReviewSweeper {
	return &KYCReviewSweeper{
		db:   d,
		sink: newTelegramSupportSink(telegramSinkConfigFrom(cfg)),
		// 15 minutes: comfortably past Didit's ~5-minute retry window, short
		// enough that a contributor is not sitting unnoticed for hours. The
		// cost of being wrong in either direction is small, because the claim
		// table makes a duplicate impossible.
		minAge:   15 * time.Minute,
		interval: 10 * time.Minute,
	}
}

// Run sweeps until the context is cancelled.
func (s *KYCReviewSweeper) Run(ctx context.Context) {
	if s.db == nil || s.db.Pool == nil {
		slog.Warn("kyc review sweep: no database, not starting")
		return
	}
	// Once at startup: a delivery dropped while the process was down is
	// exactly the case this exists for, and waiting a full interval to notice
	// would be the wrong way round.
	s.sweepOnce(ctx)

	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweepOnce(ctx)
		}
	}
}

func (s *KYCReviewSweeper) sweepOnce(ctx context.Context) {
	// LEFT JOIN rather than NOT IN: the claim in alertAdminOfKYCReview is what
	// actually guarantees once-per-session, and this only needs to avoid
	// obviously pointless work. A row that slips through is a no-op there.
	rows, err := s.db.Pool.Query(ctx, `
SELECT u.id, u.kyc_session_id
FROM users u
LEFT JOIN kyc_review_alerts a ON a.session_id = u.kyc_session_id
WHERE u.kyc_status = 'in_review'
  AND u.kyc_session_id IS NOT NULL
  AND a.session_id IS NULL
  AND u.updated_at < now() - $1::interval
`, s.minAge.String())
	if err != nil {
		slog.Error("kyc review sweep: query failed", "error", err)
		return
	}
	defer rows.Close()

	type pending struct {
		userID    uuid.UUID
		sessionID string
	}
	var found []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.userID, &p.sessionID); err != nil {
			slog.Error("kyc review sweep: scan failed", "error", err)
			return
		}
		found = append(found, p)
	}
	if err := rows.Err(); err != nil {
		slog.Error("kyc review sweep: iteration failed", "error", err)
		return
	}

	// Alert outside the row loop: alertAdminOfKYCReview writes to the same
	// pool, and holding a cursor open while doing so is how a sweep deadlocks
	// itself under a small connection pool.
	for _, p := range found {
		slog.Info("kyc review sweep: webhook never arrived for this session",
			"user_id", p.userID, "session_id", p.sessionID)
		alertAdminOfKYCReview(ctx, s.db, s.sink, p.userID, p.sessionID, "sweep")
	}
}
