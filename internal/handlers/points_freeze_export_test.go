package handlers

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// UnfreezePointsProgrammeForTest lets the external handlers_test package
// exercise the pre-freeze behaviour that is still worth covering: the
// redemption validation rules, which have to keep working if the freeze is
// ever lifted. Restores the frozen state when the test ends.
//
// Exported through an _test.go file so it exists only under `go test` and
// cannot be reached from production code.
func UnfreezePointsProgrammeForTest(t *testing.T) {
	t.Helper()
	prev := pointsProgrammeFrozen
	pointsProgrammeFrozen = false
	t.Cleanup(func() { pointsProgrammeFrozen = prev })
}

// PointsProgrammeFrozenForTest reports the current freeze state, so a test
// can assert the shipped default is frozen rather than assuming it.
func PointsProgrammeFrozenForTest() bool { return pointsProgrammeFrozen }

// InsertLedgerEntryForTest exposes the guarded accrual chokepoint so the
// external test package can assert the freeze applies at the write itself,
// not only at the HTTP handlers that happen to call it today.
func InsertLedgerEntryForTest(ctx context.Context, d *db.DB, userID uuid.UUID, amount int, reason string) error {
	return insertLedgerEntry(ctx, d.Pool, userID, amount, reason, nil)
}
