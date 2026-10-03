package grainhack

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/notifications"
)

func TestBaseSepoliaNotice_PreviewSendOnce(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()
	addr := func(uid uuid.UUID, chain, hex string, superseded bool) {
		t.Helper()
		sup := "NULL"
		if superseded {
			sup = "now()"
		}
		if _, err := f.d.Pool.Exec(ctx, `
			INSERT INTO contributor_addresses (user_id, chain_id, chain_family, address, verified_nonce, superseded_at)
			VALUES ($1, $2, 'evm', $3, 'n', `+sup+`)`, uid, chain, hex); err != nil {
			t.Fatalf("address: %v", err)
		}
	}
	live1, live2, gone := f.people[0].userID, f.people[1].userID, f.people[3].userID
	addr(live1, BaseSepoliaChainID, "0x1111111111111111111111111111111111111111", false)
	addr(live2, BaseSepoliaChainID, "0x2222222222222222222222222222222222222222", false)
	// Superseded: no longer a live address, not told.
	addr(gone, BaseSepoliaChainID, "0x3333333333333333333333333333333333333333", true)

	p, err := PreviewBaseSepoliaNotice(ctx, f.d.Pool)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(p.UserIDs, live1.String()) || !slices.Contains(p.UserIDs, live2.String()) ||
		slices.Contains(p.UserIDs, gone.String()) || p.SentAt != nil {
		t.Fatalf("preview = %+v", p)
	}
	if p.Link != "/bounties/link" || p.Type != string(notifications.TypeGrainHackLinkWallet) {
		t.Fatalf("preview link/type = %q %q", p.Link, p.Type)
	}
	// Previewing sends nothing.
	if f.notificationsOf(live1, notifications.TypeGrainHackLinkWallet) != 0 {
		t.Fatal("preview sent a notice")
	}

	if _, err := SendBaseSepoliaNotice(ctx, f.d.Pool, f.admin, false); !errors.Is(err, ErrNotConfirmed) {
		t.Fatalf("unconfirmed: err = %v", err)
	}
	if f.notificationsOf(live1, notifications.TypeGrainHackLinkWallet) != 0 {
		t.Fatal("an unconfirmed send sent a notice")
	}

	sent, err := SendBaseSepoliaNotice(ctx, f.d.Pool, f.admin, true)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if sent.SentAt == nil || sent.SentCount == nil || *sent.SentCount != p.Recipients {
		t.Fatalf("sent = %+v", sent)
	}
	for _, u := range []uuid.UUID{live1, live2} {
		if n := f.notificationsOf(u, notifications.TypeGrainHackLinkWallet); n != 1 {
			t.Errorf("%s got %d notices, want 1", u, n)
		}
	}
	if f.notificationsOf(gone, notifications.TypeGrainHackLinkWallet) != 0 {
		t.Error("a superseded address owner was told")
	}

	if _, err := SendBaseSepoliaNotice(ctx, f.d.Pool, f.admin, true); !errors.Is(err, ErrNoticeAlreadySent) {
		t.Fatalf("second send: err = %v, want ErrNoticeAlreadySent", err)
	}
	if n := f.notificationsOf(live1, notifications.TypeGrainHackLinkWallet); n != 1 {
		t.Fatalf("second send notified again: %d", n)
	}
}
