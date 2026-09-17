package keeperhubrail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/keeperhub"
)

// partialFailureRun reproduces the real run's shape: leg 0 confirmed with a tx,
// leg 1 failed on balance with no tx, leg 2 unknown (no result), and the
// fixture's fourth person excluded for having no address.
func partialFailureRun(t *testing.T, f *fx) (*Service, *fakeRail, *ReleaseResult) {
	t.Helper()
	ctx := context.Background()
	rail := &fakeRail{chainID: f.evmChainID}
	rail.result = func(i int, sent []keeperhub.Recipient) keeperhub.Execution {
		if i == 0 {
			return execOf("error", sent, leg(0, true, "0xaaa1"), leg(1, false, ""))
		}
		var steps [][]keeperhub.LegResult
		for j := range sent {
			steps = append(steps, leg(j, true, fmt.Sprintf("0xbbb%d", j)))
		}
		return execOf("success", sent, steps...)
	}
	s := &Service{Pool: f.d.Pool, Rail: rail}
	res, err := s.Release(ctx, f.req())
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := s.Intake(ctx, f.hid, res.AttemptID); err == nil {
		t.Fatal("intake should report the leg with no result")
	}
	// A chain explorer, so explorer_url can be asserted.
	f.d.Pool.Exec(ctx, `UPDATE chain_configs SET explorer_url_template = 'https://sepolia.basescan.org/tx/%s', network = 'testnet' WHERE chain_id = $1`, f.chain)
	return s, rail, res
}

func legByID(v *RunView, id uuid.UUID) *LegView {
	for i := range v.Legs {
		if v.Legs[i].ID == id {
			return &v.Legs[i]
		}
	}
	return nil
}

func TestRunView_PartialFailureRun(t *testing.T) {
	f := fixture(t)
	s, _, res := partialFailureRun(t, f)

	v, err := s.RunView(context.Background(), f.hid, PoolContributor, f.actor, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := json.MarshalIndent(v, "", "  "); err == nil {
		t.Logf("partial-failure RunView:\n%s", b)
	}

	confirmed, failed, unknown := legByID(v, res.LegIDs[0]), legByID(v, res.LegIDs[1]), legByID(v, res.LegIDs[2])
	if confirmed == nil || failed == nil || unknown == nil {
		t.Fatalf("expected all three legs in the view, got %d legs", len(v.Legs))
	}

	// Three distinct statuses.
	if confirmed.Status != "confirmed" || failed.Status != "failed" || unknown.Status != "unknown" {
		t.Fatalf("statuses = %s / %s / %s, want confirmed / failed / unknown", confirmed.Status, failed.Status, unknown.Status)
	}

	// unknown blocks, because it may have paid; failed is resendable, because
	// nothing moved. They are different things.
	if !unknown.BlocksResume || unknown.BlockReason == nil || *unknown.BlockReason != BlockMayHavePaid || unknown.Resendable {
		t.Errorf("unknown leg: blocks=%v reason=%v resendable=%v, want true / may_have_paid / false",
			unknown.BlocksResume, unknown.BlockReason, unknown.Resendable)
	}
	if failed.BlocksResume || failed.BlockReason != nil || !failed.Resendable {
		t.Errorf("failed leg: blocks=%v reason=%v resendable=%v, want false / null / true",
			failed.BlocksResume, failed.BlockReason, failed.Resendable)
	}
	if confirmed.BlocksResume || confirmed.Resendable {
		t.Errorf("confirmed leg: blocks=%v resendable=%v, want both false", confirmed.BlocksResume, confirmed.Resendable)
	}

	// Resume is refused, naming the unknown leg.
	if v.Resume.Allowed || v.Resume.Reason != ReasonUnreconciledLegs {
		t.Errorf("resume = %+v, want refused with %s", v.Resume, ReasonUnreconciledLegs)
	}
	if !slices.Equal(v.Resume.BlockingLegIDs, []uuid.UUID{res.LegIDs[2]}) {
		t.Errorf("blocking = %v, want exactly the unknown leg %s", v.Resume.BlockingLegIDs, res.LegIDs[2])
	}
	if len(v.Resume.SendableLegIDs) != 0 || v.Resume.SendableAmountMinor != nil {
		t.Errorf("a refused resume offers to send %v / %v", v.Resume.SendableLegIDs, v.Resume.SendableAmountMinor)
	}

	// explorer_url only where there is a transaction.
	if confirmed.ExplorerURL == nil || *confirmed.ExplorerURL != "https://sepolia.basescan.org/tx/0xaaa1" {
		t.Errorf("confirmed explorer_url = %v", confirmed.ExplorerURL)
	}
	if failed.ExplorerURL != nil || unknown.ExplorerURL != nil {
		t.Errorf("explorer_url on a leg with no tx: failed=%v unknown=%v", failed.ExplorerURL, unknown.ExplorerURL)
	}

	// Per-status figures, derived from legs; unknown is its own bucket.
	by := v.DerivedFromLegs.ByStatus
	// Legs are dispatched in user-id order, so leg i belongs to f.paid[i] and
	// carries that person's units - not a fixed 1/2/3.
	amt := func(i int) string { return fmt.Sprint(f.units[f.paid[i]] * 1_000_000) }
	want := map[string]StatusTotals{
		"confirmed": {1, amt(0)}, "failed": {1, amt(1)}, "unknown": {1, amt(2)},
		"pending": {0, "0"}, "dispatched": {0, "0"},
	}
	for st, w := range want {
		if by[st] != w {
			t.Errorf("by_status[%s] = %+v, want %+v", st, by[st], w)
		}
	}
	if v.DerivedFromLegs.LegCount != 3 || v.DerivedFromLegs.Note == "" {
		t.Errorf("derived_from_legs = %+v", v.DerivedFromLegs)
	}

	// Exclusions.
	if len(v.Exclusions) != 1 || v.Exclusions[0].UserID != f.noAddr ||
		v.Exclusions[0].Reason != ReasonNoAddress || v.Exclusions[0].AmountMinor != "1000000" {
		t.Errorf("exclusions = %+v", v.Exclusions)
	}
	if v.Exclusions[0].GitHubLogin == nil || *v.Exclusions[0].GitHubLogin == "" {
		t.Error("exclusion has no github login though the person has one")
	}

	// One attempt, ordinal derived, three legs in dispatch order.
	if len(v.Attempts) != 1 || v.Attempts[0].Ordinal != 1 || v.Attempts[0].LegCount != 3 ||
		v.Attempts[0].ID != res.AttemptID || v.Attempts[0].IdempotencyKey == nil {
		t.Fatalf("attempts = %+v", v.Attempts)
	}
	for i, p := range v.Attempts[0].Legs {
		if p.Position != i || p.LegID != res.LegIDs[i] {
			t.Errorf("attempt leg %d = %+v, want %s", i, p, res.LegIDs[i])
		}
	}

	// Header, including the chain.
	if v.Run.EVMChainID != f.evmChainID || v.Run.PoolMinor != "7000000" || v.Run.State != "dispatching" ||
		v.Run.Network == nil || *v.Run.Network != "testnet" || v.Run.AssetSymbol == nil || *v.Run.AssetSymbol != "USDC" {
		t.Errorf("header = %+v", v.Run)
	}
}

// Nothing in the response folds unknown money into a paid or unpaid figure.
// Checked on the JSON a client actually receives, by key name.
func TestRunView_NoFieldFoldsUnknownIntoPaidOrUnpaid(t *testing.T) {
	f := fixture(t)
	s, _, _ := partialFailureRun(t, f)
	v, err := s.RunView(context.Background(), f.hid, PoolContributor, f.actor, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(v)
	var anyJSON any
	json.Unmarshal(b, &anyJSON)

	var keys []string
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case map[string]any:
			for k, val := range t {
				keys = append(keys, k)
				walk(val)
			}
		case []any:
			for _, val := range t {
				walk(val)
			}
		}
	}
	walk(anyJSON)
	for _, k := range keys {
		lk := strings.ToLower(k)
		for _, bad := range []string{"paid", "remaining", "outstanding", "unpaid", "total"} {
			if strings.Contains(lk, bad) {
				t.Errorf("response has a %q field; the money in an unknown leg is neither paid nor unpaid", k)
			}
		}
	}
	// And every key is snake_case: no struct field leaked without a json tag.
	for _, k := range keys {
		if k != strings.ToLower(k) {
			t.Errorf("key %q is not snake_case - a field is missing its json tag", k)
		}
	}
}

// A dispatched leg is waiting for its result.
func TestRunView_DispatchedLegsAwaitTheirResult(t *testing.T) {
	f := fixture(t)
	s := &Service{Pool: f.d.Pool, Rail: &fakeRail{chainID: f.evmChainID}}
	res, err := s.Release(context.Background(), f.req())
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.RunView(context.Background(), f.hid, PoolContributor, f.actor, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range res.LegIDs {
		l := legByID(v, id)
		if l == nil || !l.BlocksResume || l.BlockReason == nil || *l.BlockReason != BlockAwaitingResult {
			t.Errorf("dispatched leg %s: %+v, want blocks with awaiting_result", id, l)
		}
	}
	if v.Resume.Allowed || v.Resume.Reason != ReasonUnreconciledLegs || len(v.Resume.BlockingLegIDs) != 3 {
		t.Errorf("resume = %+v", v.Resume)
	}
	if v.DerivedFromLegs.ByStatus["dispatched"].Count != 3 {
		t.Errorf("by_status = %+v", v.DerivedFromLegs.ByStatus)
	}
}

// A run that failed closed says so.
func TestRunView_AFailedRunCannotResume(t *testing.T) {
	f := fixture(t)
	rail := &fakeRail{chainID: f.evmChainID, result: func(_ int, sent []keeperhub.Recipient) keeperhub.Execution {
		var steps [][]keeperhub.LegResult
		for j := range sent {
			l := leg(j, true, fmt.Sprintf("0xccc%d", j))
			l[1].ChainID = 8453
			steps = append(steps, l)
		}
		return execOf("success", sent, steps...)
	}}
	s := &Service{Pool: f.d.Pool, Rail: rail}
	res, _ := s.Release(context.Background(), f.req())
	s.Intake(context.Background(), f.hid, res.AttemptID)

	v, err := s.RunView(context.Background(), f.hid, PoolContributor, f.actor, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v.Run.State != "failed" || v.Resume.Allowed || v.Resume.Reason != ReasonRunFailed {
		t.Errorf("state=%s resume=%+v, want failed / run_failed", v.Run.State, v.Resume)
	}
}

// A clean resumable run offers exactly the failed legs, and their amount.
func TestRunView_ACleanResumeOffersExactlyTheSendableLegs(t *testing.T) {
	f := fixture(t)
	s, _, res := partialFailureRun(t, f)
	if err := s.ResolveLeg(context.Background(), ResolveRequest{HackathonID: f.hid, LegID: res.LegIDs[2],
		ActorID: f.actor, Status: "failed", Note: "no transfer on chain"}); err != nil {
		t.Fatal(err)
	}
	v, err := s.RunView(context.Background(), f.hid, PoolContributor, f.actor, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Resume.Allowed || v.Resume.Reason != "" {
		t.Fatalf("resume = %+v, want allowed", v.Resume)
	}
	if !slices.Equal(v.Resume.SendableLegIDs, []uuid.UUID{res.LegIDs[1], res.LegIDs[2]}) {
		t.Errorf("sendable = %v, want the two failed legs %v", v.Resume.SendableLegIDs, res.LegIDs[1:])
	}
	wantSum := fmt.Sprint((f.units[f.paid[1]] + f.units[f.paid[2]]) * 1_000_000)
	if v.Resume.SendableAmountMinor == nil || *v.Resume.SendableAmountMinor != wantSum {
		got := "<nil>"
		if v.Resume.SendableAmountMinor != nil {
			got = *v.Resume.SendableAmountMinor
		}
		t.Errorf("sendable amount = %s, want %s (the two failed legs' amounts)", got, wantSum)
	}
	if len(v.Resume.BlockingLegIDs) != 0 {
		t.Errorf("blocking = %v, want none", v.Resume.BlockingLegIDs)
	}
}

// THE AGREEMENT TEST. For each state, what the read says a release would do is
// what release then does.
func TestRunView_AgreesWithRelease(t *testing.T) {
	check := func(t *testing.T, f *fx, s *Service, rail *fakeRail) {
		t.Helper()
		ctx := context.Background()
		v, err := s.RunView(ctx, f.hid, PoolContributor, f.actor, nil)
		if err != nil {
			t.Fatal(err)
		}
		before := len(rail.calls)
		_, relErr := s.Release(ctx, f.req())

		if got, want := v.Resume.Reason, RefusalReason(relErr); got != want {
			t.Fatalf("read says reason %q, release refused with %q (%v)", got, want, relErr)
		}
		if v.Resume.Allowed != (relErr == nil) {
			t.Fatalf("read says allowed=%v, release err=%v", v.Resume.Allowed, relErr)
		}
		var unreconciled *UnreconciledLegsError
		if errors.As(relErr, &unreconciled) && !slices.Equal(unreconciled.LegIDs, v.Resume.BlockingLegIDs) {
			t.Fatalf("read blocks on %v, release blocked on %v", v.Resume.BlockingLegIDs, unreconciled.LegIDs)
		}
		if relErr == nil {
			sent := rail.calls[before]
			var ids []uuid.UUID
			for _, r := range sent {
				ids = append(ids, uuid.MustParse(r.LegID))
			}
			if !slices.Equal(ids, v.Resume.SendableLegIDs) {
				t.Fatalf("read offered %v, release sent %v", v.Resume.SendableLegIDs, ids)
			}
		} else if len(rail.calls) != before {
			t.Fatal("a refused release reached KeeperHub")
		}
	}

	t.Run("unknown leg blocks", func(t *testing.T) {
		f := fixture(t)
		s, rail, _ := partialFailureRun(t, f)
		check(t, f, s, rail)
	})
	t.Run("dispatched legs block", func(t *testing.T) {
		f := fixture(t)
		rail := &fakeRail{chainID: f.evmChainID}
		s := &Service{Pool: f.d.Pool, Rail: rail}
		if _, err := s.Release(context.Background(), f.req()); err != nil {
			t.Fatal(err)
		}
		check(t, f, s, rail)
	})
	t.Run("clean resume sends what it offered", func(t *testing.T) {
		f := fixture(t)
		s, rail, res := partialFailureRun(t, f)
		s.ResolveLeg(context.Background(), ResolveRequest{HackathonID: f.hid, LegID: res.LegIDs[2],
			ActorID: f.actor, Status: "failed", Note: "checked"})
		check(t, f, s, rail)
	})
	t.Run("nothing unpaid", func(t *testing.T) {
		f := fixture(t)
		s, rail, res := partialFailureRun(t, f)
		s.ResolveLeg(context.Background(), ResolveRequest{HackathonID: f.hid, LegID: res.LegIDs[2],
			ActorID: f.actor, Status: "failed", Note: "checked"})
		second, err := s.Release(context.Background(), f.req())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Intake(context.Background(), f.hid, second.AttemptID); err != nil {
			t.Fatal(err)
		}
		check(t, f, s, rail)
	})
	t.Run("event no longer releasable (shadow mode)", func(t *testing.T) {
		f := fixture(t)
		s, rail, res := partialFailureRun(t, f)
		s.ResolveLeg(context.Background(), ResolveRequest{HackathonID: f.hid, LegID: res.LegIDs[2],
			ActorID: f.actor, Status: "failed", Note: "checked"})
		f.d.Pool.Exec(context.Background(), `DELETE FROM hackathon_config_settings WHERE hackathon_id = $1`, f.hid)
		check(t, f, s, rail)
	})
}

// With the rail unconfigured the state is still readable, and the resume box
// says why nothing can be sent.
func TestRunView_ReadsWithoutKeeperHubConfigured(t *testing.T) {
	f := fixture(t)
	s, _, res := partialFailureRun(t, f)
	s.ResolveLeg(context.Background(), ResolveRequest{HackathonID: f.hid, LegID: res.LegIDs[2],
		ActorID: f.actor, Status: "failed", Note: "checked"})

	reader := &Service{Pool: f.d.Pool} // no rail at all
	v, err := reader.RunView(context.Background(), f.hid, PoolContributor, f.actor, errors.New("KEEPERHUB_WEBHOOK_KEY not set"))
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Legs) != 3 {
		t.Errorf("legs = %d, want 3 - state must stay readable", len(v.Legs))
	}
	if v.Resume.Allowed || v.Resume.Reason != ReasonNotConfigured {
		t.Errorf("resume = %+v, want refused with %s", v.Resume, ReasonNotConfigured)
	}
}

func TestRunView_NoRunIsNotFound(t *testing.T) {
	f := fixture(t)
	s := &Service{Pool: f.d.Pool}
	if _, err := s.RunView(context.Background(), f.hid, PoolContributor, f.actor, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// The status classification the view uses is the one release uses: every
// blocking status has a reason, and no status is both blocking and resendable.
func TestLegClassification_IsOneDefinition(t *testing.T) {
	for _, st := range []string{"pending", "dispatched", "confirmed", "failed", "unknown"} {
		if LegBlocks(st) != (LegBlockReason(st) != "") {
			t.Errorf("%s: blocks=%v but reason=%q", st, LegBlocks(st), LegBlockReason(st))
		}
		if LegBlocks(st) && LegResendable(st) {
			t.Errorf("%s is both blocking and resendable", st)
		}
	}
	if LegResendable("unknown") || !LegResendable("failed") || LegBlocks("confirmed") || LegResendable("confirmed") {
		t.Error("unknown must never be resendable; failed must be; confirmed is neither")
	}
}
