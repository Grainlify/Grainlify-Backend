package handlers

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// NewRoleLookup returns the live-role reader used by every admin route.
//
// One indexed primary-key read per admin request, uncached on purpose - see
// auth.RequireLiveRole. A missing user is an error rather than an empty role
// so the caller refuses instead of comparing against "".
func NewRoleLookup(d *db.DB) func(ctx context.Context, userID uuid.UUID) (string, error) {
	return func(ctx context.Context, userID uuid.UUID) (string, error) {
		if d == nil || d.Pool == nil {
			return "", errors.New("database not configured")
		}
		var role string
		err := d.Pool.QueryRow(ctx, `SELECT role FROM users WHERE id = $1`, userID).Scan(&role)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("no such user %s", userID)
		}
		if err != nil {
			return "", err
		}
		return role, nil
	}
}
