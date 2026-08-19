package expiry

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

func TestSweep_DeletesExpiredAndKeepsLive(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	s := New(d.Pool, time.Hour)

	old := uuid.New().String()
	fresh := uuid.New().String()
	justExpired := uuid.New().String()
	for _, tc := range []struct {
		nonce string
		exp   string
	}{
		{old, "now() - interval '30 days'"},
		{fresh, "now() + interval '10 minutes'"},
		// Inside the grace window: expired, but recent enough that a client
		// presenting it should still be told "expired" rather than "unknown".
		{justExpired, "now() - interval '1 hour'"},
	} {
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO auth_nonces (wallet_type, address, nonce, expires_at) VALUES ('evm','0xabc',$1,`+tc.exp+`)`,
			tc.nonce); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, n := range []string{old, fresh, justExpired} {
			d.Pool.Exec(ctx, `DELETE FROM auth_nonces WHERE nonce=$1`, n)
		}
	})

	if _, err := s.SweepOnce(ctx); err != nil {
		t.Fatal(err)
	}
	exists := func(n string) bool {
		var ok bool
		d.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM auth_nonces WHERE nonce=$1)`, n).Scan(&ok)
		return ok
	}
	if exists(old) {
		t.Error("a nonce expired 30 days ago survived the sweep")
	}
	if !exists(fresh) {
		t.Error("an UNEXPIRED nonce was deleted")
	}
	if !exists(justExpired) {
		t.Error("a nonce inside the grace window was deleted; " +
			"an expired nonce must stay findable long enough to report 'expired' rather than 'unknown'")
	}
}

// The sweeper must cover every table it claims to. A hand-written list is
// correct the day it is written and silently incomplete afterwards, so the set
// is asserted rather than assumed.
func TestSweep_CoversTheTablesItClaimsTo(t *testing.T) {
	want := map[string]bool{"auth_nonces": true, "oauth_states": true}
	for _, tbl := range Tables {
		if !want[tbl.Name] {
			t.Errorf("unexpected table %q in the sweep set", tbl.Name)
		}
		delete(want, tbl.Name)
	}
	for missing := range want {
		t.Errorf("%s is not swept; it has an expires_at that nothing acts on", missing)
	}
}

func TestSweep_ReportsPerTableCounts(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	n := uuid.New().String()
	d.Pool.Exec(ctx, `INSERT INTO auth_nonces (wallet_type,address,nonce,expires_at) VALUES ('evm','0xa',$1, now()-interval '30 days')`, n)
	got, err := New(d.Pool, time.Hour).SweepOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["auth_nonces"]; !ok {
		t.Error("no count reported for auth_nonces")
	}
	if _, ok := got["oauth_states"]; !ok {
		t.Error("no count reported for oauth_states")
	}
}
