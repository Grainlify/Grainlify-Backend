package grainhack

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/notifications"
)

// The one-time notice to the people who registered a Base Sepolia payout
// address for GrainHack, when GrainHack moved to USDC on Solana.
//
// Built, not sent: an admin sends it once, deliberately, when KeeperHub is
// removed. The preview shows exactly who would receive what. A second send is
// refused (ErrNoticeAlreadySent) rather than repeated.

// BaseSepoliaNoticeKey identifies the notice in grainhack_broadcast_notices.
const BaseSepoliaNoticeKey = "base_sepolia_to_solana_2026_10"

// BaseSepoliaChainID is the chain whose live address owners receive it.
const BaseSepoliaChainID = "base-sepolia"

const (
	baseSepoliaNoticeTitle = "GrainHack now pays USDC on Solana"
	baseSepoliaNoticeBody  = "GrainHack now pays USDC on Solana. The Base Sepolia address you registered is no longer used for payouts. " +
		"Link a Solana wallet once at grainlify.com/bounties/link; it is used for GrainHack and Bounties alike."
)

var (
	ErrNoticeAlreadySent = errors.New("grainhack: this notice has already been sent")
	ErrNotConfirmed      = errors.New("grainhack: sending requires confirm: true")
)

// NoticePreview is what a send would do, or did.
type NoticePreview struct {
	Key        string     `json:"key"`
	Type       string     `json:"type"`
	Title      string     `json:"title"`
	Body       string     `json:"body"`
	Link       string     `json:"link"`
	Recipients int        `json:"recipients"`
	UserIDs    []string   `json:"user_ids"`
	SentAt     *time.Time `json:"sent_at"`
	SentCount  *int       `json:"sent_count"`
}

func baseSepoliaRecipients(ctx context.Context, q interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}) ([]uuid.UUID, error) {
	rows, err := q.Query(ctx, `
		SELECT DISTINCT user_id FROM contributor_addresses
		WHERE chain_id = $1 AND superseded_at IS NULL
		ORDER BY user_id`, BaseSepoliaChainID)
	if err != nil {
		return nil, fmt.Errorf("grainhack: notice recipients: %w", err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// PreviewBaseSepoliaNotice says who would receive the notice and whether it
// has been sent. Reads only.
func PreviewBaseSepoliaNotice(ctx context.Context, pool db.DBPool) (*NoticePreview, error) {
	ids, err := baseSepoliaRecipients(ctx, pool)
	if err != nil {
		return nil, err
	}
	p := &NoticePreview{
		Key: BaseSepoliaNoticeKey, Type: string(notifications.TypeGrainHackLinkWallet),
		Title: baseSepoliaNoticeTitle, Body: baseSepoliaNoticeBody, Link: notifications.WalletLinkLink().String(),
		Recipients: len(ids), UserIDs: make([]string, 0, len(ids)),
	}
	for _, id := range ids {
		p.UserIDs = append(p.UserIDs, id.String())
	}
	var sentAt time.Time
	var count int
	err = pool.QueryRow(ctx, `SELECT sent_at, recipients FROM grainhack_broadcast_notices WHERE key = $1`,
		BaseSepoliaNoticeKey).Scan(&sentAt, &count)
	if err == nil {
		p.SentAt, p.SentCount = &sentAt, &count
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("grainhack: notice state: %w", err)
	}
	return p, nil
}

// SendBaseSepoliaNotice sends the notice once, in-app, to every user with a
// live Base Sepolia address. The broadcast row and the notifications are
// written in one transaction: either everyone was told and it is recorded, or
// nobody was and it can be sent again.
//
// In-app only, and an in-app preference switched off for the type is honoured.
// It is written with the GrainHack link-wallet type, so the frontend renders it
// with that type's treatment and the preferences screen can mute it.
func SendBaseSepoliaNotice(ctx context.Context, pool db.DBPool, actor uuid.UUID, confirm bool) (*NoticePreview, error) {
	if !confirm {
		return nil, ErrNotConfirmed
	}
	if actor == uuid.Nil {
		return nil, fmt.Errorf("%w: no admin actor recorded", ErrNotConfirmed)
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("grainhack: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	ids, err := baseSepoliaRecipients(ctx, tx)
	if err != nil {
		return nil, err
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO grainhack_broadcast_notices (key, sent_by, recipients) VALUES ($1, $2, $3)
		ON CONFLICT (key) DO NOTHING`, BaseSepoliaNoticeKey, actor, len(ids))
	if err != nil {
		return nil, fmt.Errorf("grainhack: record notice: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrNoticeAlreadySent
	}
	for _, id := range ids {
		// Honours an in-app preference for the type, like NotifyInApp does.
		if _, err := tx.Exec(ctx, `
			INSERT INTO notifications (user_id, type, title, body, link_path)
			SELECT $1, $2, $3, $4, $5
			WHERE NOT EXISTS (SELECT 1 FROM notification_preferences
			                  WHERE user_id = $1 AND type = $2 AND in_app = false)`,
			id, string(notifications.TypeGrainHackLinkWallet), baseSepoliaNoticeTitle, baseSepoliaNoticeBody,
			notifications.WalletLinkLink().String()); err != nil {
			return nil, fmt.Errorf("grainhack: notify %s: %w", id, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("grainhack: commit notice: %w", err)
	}
	return PreviewBaseSepoliaNotice(ctx, pool)
}
