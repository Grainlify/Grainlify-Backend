package erasure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Pool is the subset of a connection pool the request functions need; a
// pgxpool.Pool and a pgx.Tx both satisfy it.
type Pool interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Request is one deletion request as the person sees it.
type Request struct {
	ID           uuid.UUID       `json:"id"`
	Status       string          `json:"status"`
	RequestedAt  time.Time       `json:"requested_at"`
	ExecuteAfter time.Time       `json:"execute_after"`
	CancelledAt  *time.Time      `json:"cancelled_at,omitempty"`
	CompletedAt  *time.Time      `json:"completed_at,omitempty"`
	HoldReason   *string         `json:"hold_reason,omitempty"`
	Erased       json.RawMessage `json:"erased,omitempty"`
	Retained     json.RawMessage `json:"retained,omitempty"`
	External     json.RawMessage `json:"external,omitempty"`
}

// Open reports whether the request can still be cancelled.
func (r Request) Open() bool { return r.Status == "scheduled" || r.Status == "held" }

var (
	// ErrNoOpenRequest is returned by Cancel when there is nothing to cancel.
	ErrNoOpenRequest = errors.New("erasure: no open deletion request")
	// ErrAlreadyErased is returned by Schedule for an account already erased.
	ErrAlreadyErased = errors.New("erasure: account already erased")
)

const requestColumns = `id, status, requested_at, execute_after, cancelled_at, completed_at,
	hold_reason, erased, retained, external`

func scanRequest(row pgx.Row) (Request, error) {
	var r Request
	var erased, retained, external []byte
	err := row.Scan(&r.ID, &r.Status, &r.RequestedAt, &r.ExecuteAfter, &r.CancelledAt, &r.CompletedAt,
		&r.HoldReason, &erased, &retained, &external)
	if err != nil {
		return Request{}, err
	}
	r.Erased, r.Retained, r.External = erased, retained, external
	return r, nil
}

// Latest returns the person's most recent request, or nil when they have
// never made one.
func Latest(ctx context.Context, pool Pool, userID uuid.UUID) (*Request, error) {
	r, err := scanRequest(pool.QueryRow(ctx, `
SELECT `+requestColumns+` FROM account_deletion_requests
WHERE user_id = $1
ORDER BY requested_at DESC
LIMIT 1
`, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// Schedule records a deletion request for userID, to be carried out after
// GracePeriod. Pressing the button again while one is open returns the open
// one unchanged: one request, one clock. created reports whether this call
// made it.
func Schedule(ctx context.Context, pool Pool, userID uuid.UUID, now time.Time) (req Request, created bool, err error) {
	var erasedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT erased_at FROM users WHERE id = $1`, userID).Scan(&erasedAt); err != nil {
		return Request{}, false, fmt.Errorf("erasure: read user: %w", err)
	}
	if erasedAt != nil {
		return Request{}, false, ErrAlreadyErased
	}

	// ON CONFLICT on the partial unique index: a concurrent second press lands
	// here rather than creating a second open request.
	row := pool.QueryRow(ctx, `
INSERT INTO account_deletion_requests (user_id, requested_at, execute_after)
VALUES ($1, $2, $3)
ON CONFLICT (user_id) WHERE status IN ('scheduled', 'held') DO NOTHING
RETURNING `+requestColumns,
		userID, now, now.Add(GracePeriod))
	req, err = scanRequest(row)
	if errors.Is(err, pgx.ErrNoRows) {
		open, lerr := Latest(ctx, pool, userID)
		if lerr != nil {
			return Request{}, false, lerr
		}
		if open == nil || !open.Open() {
			return Request{}, false, errors.New("erasure: request conflicted but no open request found")
		}
		return *open, false, nil
	}
	if err != nil {
		return Request{}, false, fmt.Errorf("erasure: insert request: %w", err)
	}
	if err := recordEvent(ctx, pool, req.ID, "requested", map[string]any{
		"execute_after": req.ExecuteAfter,
		"grace_period":  GracePeriod.String(),
	}); err != nil {
		return Request{}, false, err
	}
	return req, true, nil
}

// Cancel withdraws the person's open request. Only an open request can be
// cancelled: once it has completed there is nothing left to restore.
func Cancel(ctx context.Context, pool Pool, userID uuid.UUID, now time.Time) (Request, error) {
	req, err := scanRequest(pool.QueryRow(ctx, `
UPDATE account_deletion_requests
SET status = 'cancelled', cancelled_at = $2, hold_reason = NULL
WHERE user_id = $1 AND status IN ('scheduled', 'held')
RETURNING `+requestColumns,
		userID, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return Request{}, ErrNoOpenRequest
	}
	if err != nil {
		return Request{}, fmt.Errorf("erasure: cancel: %w", err)
	}
	if err := recordEvent(ctx, pool, req.ID, "cancelled", nil); err != nil {
		return Request{}, err
	}
	return req, nil
}

// recordEvent appends to the request's history.
func recordEvent(ctx context.Context, pool Pool, requestID uuid.UUID, kind string, detail any) error {
	if detail == nil {
		detail = map[string]any{}
	}
	b, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("erasure: encode event: %w", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO account_deletion_events (request_id, kind, detail) VALUES ($1, $2, $3)
`, requestID, kind, b); err != nil {
		return fmt.Errorf("erasure: record %s event: %w", kind, err)
	}
	return nil
}
