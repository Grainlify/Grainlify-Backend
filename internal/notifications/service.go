package notifications

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/email"
)

// Service creates in-app notifications and sends notification emails,
// honoring each user's per-type preferences.
type Service struct {
	db              *db.DB
	mailer          email.Mailer
	frontendBaseURL string
}

func New(d *db.DB, mailer email.Mailer, frontendBaseURL string) *Service {
	return &Service{db: d, mailer: mailer, frontendBaseURL: strings.TrimRight(frontendBaseURL, "/")}
}

// Notify creates an in-app notification and/or sends an email for userID,
// according to their preferences for t (both channels default to enabled
// when the user has no preference row for t - see migration 000029).
//
// This is a best-effort side effect: failures are logged, never returned.
// Callers should invoke Notify only after their primary action has already
// succeeded, and must not let notification delivery affect the response.
func (s *Service) Notify(ctx context.Context, userID uuid.UUID, t Type, title, body, linkPath string) {
	if s == nil || s.db == nil || s.db.Pool == nil {
		return
	}

	inApp, wantEmail := true, true
	err := s.db.Pool.QueryRow(ctx, `
SELECT in_app, email FROM notification_preferences WHERE user_id = $1 AND type = $2
`, userID, string(t)).Scan(&inApp, &wantEmail)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		slog.Warn("notifications: preference lookup failed, using defaults", "user_id", userID, "type", t, "error", err)
	}

	if inApp {
		if _, err := s.db.Pool.Exec(ctx, `
INSERT INTO notifications (user_id, type, title, body, link_path)
VALUES ($1, $2, $3, $4, $5)
`, userID, string(t), title, body, linkPath); err != nil {
			slog.Warn("notifications: insert failed", "user_id", userID, "type", t, "error", err)
		}
	}

	if wantEmail && s.mailer != nil {
		s.sendEmail(ctx, userID, t, title, body, linkPath)
	}
}

// InAppResult reports what one in-app notification attempt actually did.
//
// Notify is deliberately silent about this - a notification must never affect
// the caller's response - but "silent" and "unrecorded" are different things,
// and some callers need to write the outcome down.
type InAppResult struct {
	// Created is false when the user has in-app notifications switched off for
	// this type, which is a legitimate outcome and not a failure.
	Created bool
	// Suppressed distinguishes "the user asked not to receive this" from "we
	// tried and it failed". Both leave Created false and they are not the same
	// fact.
	Suppressed bool
	Err        error
}

// NotifyInApp writes an in-app notification and reports whether it landed.
//
// # No email, deliberately
//
// Unlike Notify, this attempts no email whatever the user's preference says.
// That is a decision rather than an omission: users.email is non-null for zero
// of our accounts today - the GitHub OAuth flow requests the user:email scope,
// fetches the address at login, and never persists it - so every email this
// system "sends" reaches nobody.
//
// Persisting an address is a new category of personal data and is being
// decided on its own terms, with its own deletion path. Until that decision is
// made, a caller that used Notify would silently begin emailing people the
// moment somebody wired persistence up, as an unnoticed side effect of
// unrelated work. This makes turning email on for these messages a deliberate
// act: delete this method's restriction, on purpose.
//
// Callers must still treat the result as advisory and never fail their primary
// action on it.
func (s *Service) NotifyInApp(ctx context.Context, userID uuid.UUID, t Type, title, body, linkPath string) InAppResult {
	if s == nil || s.db == nil || s.db.Pool == nil {
		return InAppResult{Err: errors.New("notifications: service not configured")}
	}

	inApp := true
	err := s.db.Pool.QueryRow(ctx, `
SELECT in_app FROM notification_preferences WHERE user_id = $1 AND type = $2
`, userID, string(t)).Scan(&inApp)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		slog.Warn("notifications: preference lookup failed, using defaults", "user_id", userID, "type", t, "error", err)
	}
	if !inApp {
		return InAppResult{Suppressed: true}
	}

	if _, err := s.db.Pool.Exec(ctx, `
INSERT INTO notifications (user_id, type, title, body, link_path)
VALUES ($1, $2, $3, $4, $5)
`, userID, string(t), title, body, linkPath); err != nil {
		slog.Warn("notifications: insert failed", "user_id", userID, "type", t, "error", err)
		return InAppResult{Err: err}
	}
	return InAppResult{Created: true}
}

func (s *Service) sendEmail(ctx context.Context, userID uuid.UUID, t Type, title, body, linkPath string) {
	var to *string
	if err := s.db.Pool.QueryRow(ctx, `SELECT email FROM users WHERE id = $1`, userID).Scan(&to); err != nil {
		slog.Warn("notifications: user lookup for email failed", "user_id", userID, "type", t, "error", err)
		return
	}
	if to == nil || *to == "" {
		return // no persisted email on file; skip silently, this is expected for many users today
	}

	html := fmt.Sprintf(`<p>%s</p>`, body)
	if linkPath != "" && s.frontendBaseURL != "" {
		html += fmt.Sprintf(`<p><a href="%s%s">View on Grainlify</a></p>`, s.frontendBaseURL, linkPath)
	}
	if err := s.mailer.Send(ctx, *to, title, html); err != nil {
		slog.Warn("notifications: email send failed", "user_id", userID, "type", t, "error", err)
	}
}

// ResolveUserIDByGitHubLogin looks up the Grainlify user linked to a GitHub
// login, for notifying an assignee/PR-author who is only known by their
// GitHub login at the call site. Returns (uuid.Nil, false) if no Grainlify
// account is linked to that login (not every issue assignee or PR author is
// necessarily a registered Grainlify user).
func ResolveUserIDByGitHubLogin(ctx context.Context, d *db.DB, login string) (uuid.UUID, bool) {
	if d == nil || d.Pool == nil || strings.TrimSpace(login) == "" {
		return uuid.Nil, false
	}
	var userID uuid.UUID
	err := d.Pool.QueryRow(ctx, `
SELECT user_id FROM github_accounts WHERE login = $1
`, login).Scan(&userID)
	if err != nil {
		return uuid.Nil, false
	}
	return userID, true
}
