package handlers

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestPointsBalance_SumsLedgerEntries(t *testing.T) {
	d := referralsTestDB(t)
	userID := referralsCreateUser(t, d)

	ref1 := uuid.New()
	if err := insertLedgerEntry(context.Background(), d.Pool, userID, 100, "referral", &ref1); err != nil {
		t.Fatalf("insertLedgerEntry: %v", err)
	}
	if err := insertLedgerEntry(context.Background(), d.Pool, userID, 500, "social_follow", nil); err != nil {
		t.Fatalf("insertLedgerEntry: %v", err)
	}
	if err := insertLedgerEntry(context.Background(), d.Pool, userID, -200, "redemption", nil); err != nil {
		t.Fatalf("insertLedgerEntry: %v", err)
	}

	balance, err := pointsBalance(context.Background(), d, userID)
	if err != nil {
		t.Fatalf("pointsBalance: %v", err)
	}
	if balance != 400 {
		t.Errorf("balance = %d, want 400 (100 + 500 - 200)", balance)
	}
}

func TestPointsBalance_ZeroForUserWithNoLedgerEntries(t *testing.T) {
	d := referralsTestDB(t)
	userID := referralsCreateUser(t, d)

	balance, err := pointsBalance(context.Background(), d, userID)
	if err != nil {
		t.Fatalf("pointsBalance: %v", err)
	}
	if balance != 0 {
		t.Errorf("balance = %d, want 0", balance)
	}
}
