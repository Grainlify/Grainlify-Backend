package keeperhubrail

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
	"github.com/jagadeesh/grainlify/backend/internal/hackathon"
	"github.com/jagadeesh/grainlify/backend/internal/keeperhub"
)

// fakeRail stands in for KeeperHub. It moves nothing.
type fakeRail struct {
	// chainID is stamped onto every transaction the fake reports without one,
	// as KeeperHub reports the chain a transfer was broadcast on.
	chainID     int64
	calls       [][]keeperhub.Recipient
	keys        []string
	dispatchErr error
	// result builds the execution read back for call i.
	result func(i int, sent []keeperhub.Recipient) keeperhub.Execution
}

func (f *fakeRail) Dispatch(_ context.Context, r []keeperhub.Recipient) (keeperhub.DispatchAck, error) {
	f.calls = append(f.calls, append([]keeperhub.Recipient(nil), r...))
	key := uuid.NewString()
	f.keys = append(f.keys, key)
	if f.dispatchErr != nil {
		return keeperhub.DispatchAck{IdempotencyKey: key}, f.dispatchErr
	}
	return keeperhub.DispatchAck{
		ExecutionID:    fmt.Sprintf("exec-%d-%s", len(f.calls), key[:8]),
		Status:         "running",
		IdempotencyKey: key,
	}, nil
}

func (f *fakeRail) Execution(_ context.Context, id string) (keeperhub.Execution, error) {
	for i := range f.calls {
		if fmt.Sprintf("exec-%d-%s", i+1, f.keys[i][:8]) == id {
			ex := f.result(i, f.calls[i])
			ex.ID = id
			for j := range ex.Legs {
				if ex.Legs[j].TxHash != "" && ex.Legs[j].ChainID == 0 {
					ex.Legs[j].ChainID = f.chainID
				}
			}
			return ex, nil
		}
	}
	return keeperhub.Execution{}, fmt.Errorf("fake: no execution %s", id)
}

// leg builds one iteration's steps: a conversion step and a transfer step, as
// the real payout workflow has.
func leg(i int, ok bool, tx string) []keeperhub.LegResult {
	status, errMsg := "success", ""
	if !ok {
		status, errMsg = "error", "Insufficient USDC balance"
	}
	return []keeperhub.LegResult{
		{IterationIndex: i, NodeID: "to-human", Status: "success"},
		{IterationIndex: i, NodeID: "transfer", Status: status, Error: errMsg, TxHash: tx},
	}
}

func execOf(status string, sent []keeperhub.Recipient, steps ...[]keeperhub.LegResult) keeperhub.Execution {
	ex := keeperhub.Execution{Status: status, Input: sent}
	for _, s := range steps {
		ex.Legs = append(ex.Legs, s...)
	}
	return ex
}

type fx struct {
	d         *db.DB
	hid       uuid.UUID
	payoutRun uuid.UUID
	chain     string
	actor     uuid.UUID
	// evmChainID is unique per fixture: the column is UNIQUE and this database
	// is never truncated.
	evmChainID int64
	// paid are the three verified people with an address, in user-id order,
	// which is the order legs are dispatched in.
	paid   []uuid.UUID
	noAddr uuid.UUID
	units  map[uuid.UUID]int
}

// fixture: a settled event with a 7 USDC pool and four people earning 1/2/3/1
// units. Three are payable; the fourth has no address on the chain.
func fixture(t *testing.T) *fx {
	t.Helper()
	d := dbtest.DB(t)
	ctx := context.Background()
	must := func(sql string, args ...any) {
		t.Helper()
		if _, err := d.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("fixture: %v\n  %.100s", err, sql)
		}
	}

	f := &fx{d: d, chain: "khtest-evm-" + uuid.NewString()[:8], units: map[uuid.UUID]int{},
		evmChainID: 800_000_000_000 + int64(uuid.New().ID())}
	must(`INSERT INTO chain_configs (chain_id, family, enabled, asset, min_confirmations, evm_chain_id)
	      VALUES ($1, 'evm', true, '{"symbol":"USDC","decimals":6}'::jsonb, 1, $2)`, f.chain, f.evmChainID)

	var owner, project uuid.UUID
	d.Pool.QueryRow(ctx, `INSERT INTO users DEFAULT VALUES RETURNING id`).Scan(&owner)
	f.actor = owner
	if err := d.Pool.QueryRow(ctx, `INSERT INTO projects (owner_user_id, github_full_name) VALUES ($1,$2) RETURNING id`,
		owner, "acme/kh-"+uuid.NewString()[:8]).Scan(&project); err != nil {
		t.Fatalf("project: %v", err)
	}
	if err := d.Pool.QueryRow(ctx, `
		INSERT INTO hackathons (name, contributor_prize_pool, phase, appeals_closed_at)
		VALUES ($1, 7, 'settled', now()) RETURNING id`, "kh-"+uuid.NewString()[:8]).Scan(&f.hid); err != nil {
		t.Fatalf("hackathon: %v", err)
	}
	must(`INSERT INTO hackathon_config_settings (hackathon_id, key, value) VALUES ($1, 'judging_shadow_mode', 'false')`, f.hid)
	if err := d.Pool.QueryRow(ctx, `
		INSERT INTO hackathon_payout_runs (hackathon_id, contributor_prize_pool, total_units, unit_value)
		VALUES ($1, 7, 7, 1) RETURNING id`, f.hid).Scan(&f.payoutRun); err != nil {
		t.Fatalf("payout run: %v", err)
	}

	var users []uuid.UUID
	person := func(pr, units int, withAddress bool) uuid.UUID {
		uid := uuid.New()
		must(`INSERT INTO users (id, role, kyc_status) VALUES ($1, 'contributor', 'verified')`, uid)
		login := "kh-" + uuid.NewString()[:8]
		must(`INSERT INTO github_accounts (user_id, github_user_id, login, access_token)
		      VALUES ($1, $2, $3, '\x00')`, uid, int64(uuid.New().ID()), login)
		must(`INSERT INTO hackathon_verdicts (hackathon_id, project_id, pr_number, user_id, github_login, final_bucket, units, curve_multiplier)
		      VALUES ($1, $2, $3, $4, $5, 'accepted', $6, 1)`, f.hid, project, pr, uid, login, units)
		if withAddress {
			k, _ := crypto.GenerateKey()
			must(`INSERT INTO contributor_addresses (user_id, chain_id, chain_family, address, verified_nonce)
			      VALUES ($1, $2, 'evm', $3, 'n')`, uid, f.chain, crypto.PubkeyToAddress(k.PublicKey).Hex())
		}
		f.units[uid] = units
		users = append(users, uid)
		return uid
	}
	a := person(1, 1, true)
	b := person(2, 2, true)
	c := person(3, 3, true)
	f.noAddr = person(4, 1, false)
	f.paid = []uuid.UUID{a, b, c}
	sort.Slice(f.paid, func(i, j int) bool { return f.paid[i].String() < f.paid[j].String() })

	t.Cleanup(func() {
		ctx := context.Background()
		d.Pool.Exec(ctx, `DELETE FROM hackathons WHERE id = $1`, f.hid)
		d.Pool.Exec(ctx, `DELETE FROM settlements WHERE hackathon_id = $1`, f.hid)
		for _, u := range users {
			d.Pool.Exec(ctx, `DELETE FROM contributor_addresses WHERE user_id = $1`, u)
			d.Pool.Exec(ctx, `DELETE FROM github_accounts WHERE user_id = $1`, u)
			d.Pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, u)
		}
		d.Pool.Exec(ctx, `DELETE FROM projects WHERE id = $1`, project)
		d.Pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, owner)
		d.Pool.Exec(ctx, `DELETE FROM chain_configs WHERE chain_id = $1`, f.chain)
	})
	return f
}

func (f *fx) req() ReleaseRequest {
	return ReleaseRequest{HackathonID: f.hid, PayoutRunID: f.payoutRun, ActorID: f.actor,
		Pool: PoolContributor, ChainID: f.chain, Confirm: true}
}

func (f *fx) legStatuses(t *testing.T) map[uuid.UUID]string {
	t.Helper()
	rows, err := f.d.Pool.Query(context.Background(), `
		SELECT l.user_id, l.status FROM keeperhub_payout_legs l
		JOIN keeperhub_payout_runs r ON r.id = l.run_id WHERE r.hackathon_id = $1`, f.hid)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[uuid.UUID]string{}
	for rows.Next() {
		var u uuid.UUID
		var s string
		rows.Scan(&u, &s)
		out[u] = s
	}
	return out
}

func (f *fx) count(t *testing.T, sql string) int {
	t.Helper()
	var n int
	if err := f.d.Pool.QueryRow(context.Background(), sql, f.hid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// GuardPayoutRelease is called first, and a refusal writes nothing and sends
// nothing.
func TestRelease_TheGuardComesFirst(t *testing.T) {
	f := fixture(t)
	rail := &fakeRail{chainID: f.evmChainID}
	s := &Service{Pool: f.d.Pool, Rail: rail}

	unconfirmed := f.req()
	unconfirmed.Confirm = false
	if _, err := s.Release(context.Background(), unconfirmed); !errors.Is(err, hackathon.ErrPayoutNotReleasable) {
		t.Fatalf("unconfirmed: err = %v, want ErrPayoutNotReleasable", err)
	}

	// Shadow mode is the default; removing the override must refuse too.
	f.d.Pool.Exec(context.Background(), `DELETE FROM hackathon_config_settings WHERE hackathon_id = $1`, f.hid)
	if _, err := s.Release(context.Background(), f.req()); !errors.Is(err, hackathon.ErrPayoutNotReleasable) {
		t.Fatalf("shadow mode: err = %v, want ErrPayoutNotReleasable", err)
	}

	if len(rail.calls) != 0 {
		t.Error("a refused release reached KeeperHub")
	}
	if n := f.count(t, `SELECT count(*) FROM keeperhub_payout_runs WHERE hackathon_id = $1`); n != 0 {
		t.Errorf("a refused release wrote %d run(s)", n)
	}
}

func TestRelease_PlansFreezesAndSendsOnlyEligibleLegs(t *testing.T) {
	f := fixture(t)
	rail := &fakeRail{chainID: f.evmChainID}
	s := &Service{Pool: f.d.Pool, Rail: rail}

	res, err := s.Release(context.Background(), f.req())
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if !res.Planned || len(rail.calls) != 1 || len(rail.calls[0]) != 3 {
		t.Fatalf("planned=%v calls=%d, want one call carrying 3 legs", res.Planned, len(rail.calls))
	}
	// Exact integer minor units, in the frozen order, with legIds that are the
	// leg rows. 1/2/3 units of a 7 USDC pool.
	for i, r := range rail.calls[0] {
		want := fmt.Sprint(f.units[f.paid[i]] * 1_000_000)
		if r.AmountMinor != want {
			t.Errorf("leg %d amountMinor = %s, want %s", i, r.AmountMinor, want)
		}
		if r.LegID != res.LegIDs[i].String() {
			t.Errorf("leg %d legId = %s, want the leg row id %s", i, r.LegID, res.LegIDs[i])
		}
	}
	// Excluded and reported, not dropped.
	if len(res.Exclusions) != 1 || res.Exclusions[0].UserID != f.noAddr ||
		res.Exclusions[0].Reason != ReasonNoAddress || res.Exclusions[0].AmountMinor != "1000000" {
		t.Errorf("exclusions = %+v, want the one person with no address, 1 USDC", res.Exclusions)
	}
	var legs, excluded int64
	f.d.Pool.QueryRow(context.Background(), `
		SELECT COALESCE((SELECT sum(amount_minor) FROM keeperhub_payout_legs WHERE run_id = $1),0),
		       COALESCE((SELECT sum(amount_minor) FROM keeperhub_payout_exclusions WHERE run_id = $1),0)`,
		res.RunID).Scan(&legs, &excluded)
	if legs+excluded != 7_000_000 {
		t.Errorf("legs %d + exclusions %d != the 7 USDC pool", legs, excluded)
	}

	// An acknowledgement is not payment.
	if res.AckStatus != "running" {
		t.Errorf("ack = %q", res.AckStatus)
	}
	for u, st := range f.legStatuses(t) {
		if st != "dispatched" {
			t.Errorf("leg for %s is %q straight after dispatch, want dispatched", u, st)
		}
	}

	// SettlementFor was a pure producer: the Aptos rail is still available.
	if n := f.count(t, `SELECT count(*) FROM settlements WHERE hackathon_id = $1`); n != 0 {
		t.Fatalf("%d settlements row(s) written - that would block the Aptos rail for this event", n)
	}
}

// The case the whole rail exists for: a partial failure, a blocked resume, a
// human resolution, and a resume that is a new attempt carrying only unpaid legs.
func TestRelease_PartialFailureThenResume(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()
	rail := &fakeRail{chainID: f.evmChainID}
	rail.result = func(i int, sent []keeperhub.Recipient) keeperhub.Execution {
		if i == 0 {
			// Leg 0 paid, leg 1 failed on balance, leg 2 has no result at all.
			return execOf("error", sent, leg(0, true, "0xaaa1"), leg(1, false, ""))
		}
		var steps [][]keeperhub.LegResult
		for j := range sent {
			steps = append(steps, leg(j, true, fmt.Sprintf("0xbbb%d", j)))
		}
		return execOf("success", sent, steps...)
	}
	s := &Service{Pool: f.d.Pool, Rail: rail}

	first, err := s.Release(ctx, f.req())
	if err != nil {
		t.Fatalf("release: %v", err)
	}

	in, err := s.Intake(ctx, f.hid, first.AttemptID)
	var missing *keeperhub.MissingResultsError
	if !errors.As(err, &missing) {
		t.Fatalf("intake err = %v, want MissingResultsError for the leg with no result", err)
	}
	st := f.legStatuses(t)
	if st[f.paid[0]] != "confirmed" || st[f.paid[1]] != "failed" || st[f.paid[2]] != "unknown" {
		t.Fatalf("after intake: %v / %v / %v, want confirmed / failed / unknown",
			st[f.paid[0]], st[f.paid[1]], st[f.paid[2]])
	}
	if len(in.Unknown) != 1 || in.Unknown[0] != first.LegIDs[2].String() {
		t.Errorf("unknown = %v, want the third leg", in.Unknown)
	}

	// The unknown leg may have paid. Nothing may be sent until it is resolved.
	_, err = s.Release(ctx, f.req())
	var blocked *UnreconciledLegsError
	if !errors.As(err, &blocked) || len(blocked.LegIDs) != 1 || blocked.LegIDs[0] != first.LegIDs[2] {
		t.Fatalf("resume with an unknown leg: err = %v, want it blocked on that leg", err)
	}
	if len(rail.calls) != 1 {
		t.Fatal("a blocked resume reached KeeperHub")
	}

	// A person checks the chain and finds nothing landed.
	if err := s.ResolveLeg(ctx, ResolveRequest{HackathonID: f.hid, LegID: first.LegIDs[2],
		ActorID: f.actor, Status: "failed"}); !errors.Is(err, ErrResolutionIncomplete) {
		t.Fatalf("resolution without a note: err = %v, want ErrResolutionIncomplete", err)
	}
	if err := s.ResolveLeg(ctx, ResolveRequest{HackathonID: f.hid, LegID: first.LegIDs[2],
		ActorID: f.actor, Status: "failed", Note: "no Transfer to this address on basescan"}); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	second, err := s.Release(ctx, f.req())
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	// Only the unpaid legs, as a genuinely new attempt.
	if len(rail.calls) != 2 || len(rail.calls[1]) != 2 {
		t.Fatalf("resume sent %d leg(s), want exactly the 2 unpaid ones", len(rail.calls[1]))
	}
	for _, r := range rail.calls[1] {
		if r.LegID == first.LegIDs[0].String() {
			t.Fatal("the resume re-sent a leg that had already paid")
		}
	}
	if second.AttemptID == first.AttemptID {
		t.Error("the resume reused the first attempt")
	}
	var keys, distinct int
	f.d.Pool.QueryRow(ctx, `
		SELECT count(idempotency_key), count(DISTINCT idempotency_key)
		FROM keeperhub_dispatch_attempts a JOIN keeperhub_payout_runs r ON r.id = a.run_id
		WHERE r.hackathon_id = $1`, f.hid).Scan(&keys, &distinct)
	if keys != 2 || distinct != 2 {
		t.Errorf("attempt keys: %d recorded, %d distinct - want two different keys", keys, distinct)
	}

	done, err := s.Intake(ctx, f.hid, second.AttemptID)
	if err != nil {
		t.Fatalf("second intake: %v", err)
	}
	if !done.RunComplete {
		t.Error("every leg is confirmed and the run is not complete")
	}
	if _, err := s.Release(ctx, f.req()); !errors.Is(err, ErrNothingUnpaid) {
		t.Fatalf("release of a complete run: err = %v, want ErrNothingUnpaid", err)
	}
	if len(rail.calls) != 2 {
		t.Error("a complete run was dispatched again")
	}
}

// A dispatch that errors may still have been accepted. Its legs are unknown, not
// pending, and they block the next release.
func TestRelease_ADispatchErrorLeavesLegsUnknown(t *testing.T) {
	f := fixture(t)
	rail := &fakeRail{chainID: f.evmChainID, dispatchErr: errors.New("context deadline exceeded")}
	s := &Service{Pool: f.d.Pool, Rail: rail}

	if _, err := s.Release(context.Background(), f.req()); !errors.Is(err, ErrDispatchUnknown) {
		t.Fatalf("err = %v, want ErrDispatchUnknown", err)
	}
	for u, st := range f.legStatuses(t) {
		if st != "unknown" {
			t.Errorf("leg for %s is %q after a failed dispatch, want unknown", u, st)
		}
	}
	var key *string
	f.d.Pool.QueryRow(context.Background(), `
		SELECT a.idempotency_key FROM keeperhub_dispatch_attempts a
		JOIN keeperhub_payout_runs r ON r.id = a.run_id WHERE r.hackathon_id = $1`, f.hid).Scan(&key)
	if key == nil || *key == "" {
		t.Error("the idempotency key of an unacknowledged attempt was not recorded - it is the only handle on it")
	}

	rail.dispatchErr = nil
	if _, err := s.Release(context.Background(), f.req()); !errors.Is(err, ErrUnreconciled) {
		t.Fatalf("resume after an unknown dispatch: err = %v, want ErrUnreconciled", err)
	}
	if len(rail.calls) != 1 {
		t.Error("legs of an unacknowledged dispatch were sent again")
	}
}

func TestIntake_ARunningExecutionWritesNothing(t *testing.T) {
	f := fixture(t)
	rail := &fakeRail{chainID: f.evmChainID, result: func(_ int, sent []keeperhub.Recipient) keeperhub.Execution {
		return execOf("running", sent, leg(0, true, "0x1"))
	}}
	s := &Service{Pool: f.d.Pool, Rail: rail}
	res, err := s.Release(context.Background(), f.req())
	if err != nil {
		t.Fatal(err)
	}
	in, err := s.Intake(context.Background(), f.hid, res.AttemptID)
	if err != nil || in.Finished {
		t.Fatalf("intake of a running execution: finished=%v err=%v", in.Finished, err)
	}
	for u, st := range f.legStatuses(t) {
		if st != "dispatched" {
			t.Errorf("leg for %s became %q while its execution was still running", u, st)
		}
	}
}

// An execution whose input is not what we sent is somebody else's run.
func TestIntake_AForeignExecutionMakesEveryLegUnknown(t *testing.T) {
	f := fixture(t)
	rail := &fakeRail{chainID: f.evmChainID, result: func(_ int, sent []keeperhub.Recipient) keeperhub.Execution {
		return execOf("success", sent[:1], leg(0, true, "0x1"))
	}}
	s := &Service{Pool: f.d.Pool, Rail: rail}
	res, err := s.Release(context.Background(), f.req())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Intake(context.Background(), f.hid, res.AttemptID); !errors.Is(err, keeperhub.ErrExecutionInputMismatch) {
		t.Fatalf("err = %v, want ErrExecutionInputMismatch", err)
	}
	for u, st := range f.legStatuses(t) {
		if st != "unknown" {
			t.Errorf("leg for %s is %q after a foreign execution, want unknown", u, st)
		}
	}
	// Idempotent afterwards: nothing more is written.
	again, err := s.Intake(context.Background(), f.hid, res.AttemptID)
	if err != nil || !again.AlreadyReconciled {
		t.Errorf("second intake: %+v %v", again, err)
	}
}

func TestRelease_RefusesAnEventAlreadySettledOnAptos(t *testing.T) {
	f := fixture(t)
	f.d.Pool.Exec(context.Background(), `
		INSERT INTO settlements (hackathon_id, pool, pool_usdc, total_weight, unit_value_usdc, pool_minor, asset_decimals)
		VALUES ($1, 'contributor', 7, 7, 1, 7000000, 6)`, f.hid)
	rail := &fakeRail{chainID: f.evmChainID}
	s := &Service{Pool: f.d.Pool, Rail: rail}
	if _, err := s.Release(context.Background(), f.req()); !errors.Is(err, ErrSettledOnAptos) {
		t.Fatalf("err = %v, want ErrSettledOnAptos", err)
	}
	if len(rail.calls) != 0 {
		t.Error("an event settled on Aptos was dispatched on KeeperHub")
	}
}

func TestRelease_RefusesASupersededPayoutRun(t *testing.T) {
	f := fixture(t)
	f.d.Pool.Exec(context.Background(), `
		INSERT INTO hackathon_payout_runs (hackathon_id, contributor_prize_pool, total_units, unit_value, created_at)
		VALUES ($1, 7, 7, 1, now() + interval '1 minute')`, f.hid)
	s := &Service{Pool: f.d.Pool, Rail: &fakeRail{chainID: f.evmChainID}}
	if _, err := s.Release(context.Background(), f.req()); !errors.Is(err, ErrPayoutRunNotCurrent) {
		t.Fatalf("err = %v, want ErrPayoutRunNotCurrent - paying a pre-appeal computation pays the wrong amounts", err)
	}
}

func TestRelease_RefusesANonEVMChain(t *testing.T) {
	f := fixture(t)
	req := f.req()
	req.ChainID = "aptos-testnet"
	s := &Service{Pool: f.d.Pool, Rail: &fakeRail{chainID: f.evmChainID}}
	if _, err := s.Release(context.Background(), req); !errors.Is(err, ErrNotEVMChain) {
		t.Fatalf("err = %v, want ErrNotEVMChain", err)
	}
}

func TestResolveLeg_AConfirmationNeedsItsTransaction(t *testing.T) {
	f := fixture(t)
	rail := &fakeRail{chainID: f.evmChainID, dispatchErr: errors.New("timeout")}
	s := &Service{Pool: f.d.Pool, Rail: rail}
	s.Release(context.Background(), f.req())
	var legID uuid.UUID
	f.d.Pool.QueryRow(context.Background(), `
		SELECT l.id FROM keeperhub_payout_legs l JOIN keeperhub_payout_runs r ON r.id = l.run_id
		WHERE r.hackathon_id = $1 LIMIT 1`, f.hid).Scan(&legID)
	err := s.ResolveLeg(context.Background(), ResolveRequest{HackathonID: f.hid, LegID: legID,
		ActorID: f.actor, Status: "confirmed", Note: "saw it"})
	if !errors.Is(err, ErrResolutionIncomplete) {
		t.Fatalf("err = %v, want ErrResolutionIncomplete", err)
	}
	// Scoped to the hackathon: another event's id cannot reach this leg.
	err = s.ResolveLeg(context.Background(), ResolveRequest{HackathonID: uuid.New(), LegID: legID,
		ActorID: f.actor, Status: "failed", Note: "x"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-event resolve: err = %v, want ErrNotFound", err)
	}
}

// The run records the numeric chain it pays on.
func TestRelease_FreezesTheNumericChainOntoTheRun(t *testing.T) {
	f := fixture(t)
	s := &Service{Pool: f.d.Pool, Rail: &fakeRail{chainID: f.evmChainID}}
	res, err := s.Release(context.Background(), f.req())
	if err != nil {
		t.Fatal(err)
	}
	var got int64
	f.d.Pool.QueryRow(context.Background(), `SELECT evm_chain_id FROM keeperhub_payout_runs WHERE id = $1`, res.RunID).Scan(&got)
	if got != f.evmChainID {
		t.Fatalf("run evm_chain_id = %d, want %d", got, f.evmChainID)
	}
}

// A transfer reported on a different network is not a payment on this one. The
// run fails closed: nothing is confirmed, every leg is unknown, and nothing more
// may be sent under it.
func TestIntake_AWrongChainFailsTheRun(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()
	rail := &fakeRail{chainID: f.evmChainID, result: func(_ int, sent []keeperhub.Recipient) keeperhub.Execution {
		steps := [][]keeperhub.LegResult{}
		for j := range sent {
			l := leg(j, true, fmt.Sprintf("0xccc%d", j))
			if j == 1 {
				l[1].ChainID = 8453 // mainnet, while the run pays on the fixture chain
			}
			steps = append(steps, l)
		}
		return execOf("success", sent, steps...)
	}}
	s := &Service{Pool: f.d.Pool, Rail: rail}
	res, err := s.Release(ctx, f.req())
	if err != nil {
		t.Fatal(err)
	}

	in, err := s.Intake(ctx, f.hid, res.AttemptID)
	if !errors.Is(err, ErrChainMismatch) {
		t.Fatalf("err = %v, want ErrChainMismatch", err)
	}
	if len(in.Outcomes) != 0 || len(in.Unknown) != 3 {
		t.Errorf("outcomes=%d unknown=%d, want 0 and 3", len(in.Outcomes), len(in.Unknown))
	}
	for u, st := range f.legStatuses(t) {
		if st != "unknown" {
			t.Errorf("leg for %s is %q after a chain mismatch, want unknown - even the legs "+
				"reported on the right chain belong to a run that reached the wrong one", u, st)
		}
	}
	var runState string
	f.d.Pool.QueryRow(ctx, `SELECT state FROM keeperhub_payout_runs WHERE id = $1`, res.RunID).Scan(&runState)
	if runState != "failed" {
		t.Errorf("run state = %q, want failed", runState)
	}
	// Resolving the legs is not enough to resume a failed run.
	var ids []uuid.UUID
	rows, _ := f.d.Pool.Query(ctx, `SELECT id FROM keeperhub_payout_legs WHERE run_id = $1`, res.RunID)
	for rows.Next() {
		var id uuid.UUID
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if err := s.ResolveLeg(ctx, ResolveRequest{HackathonID: f.hid, LegID: id, ActorID: f.actor,
			Status: "failed", Note: "checked"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Release(ctx, f.req()); !errors.Is(err, ErrRunFailed) {
		t.Fatalf("release after a chain mismatch: err = %v, want ErrRunFailed", err)
	}
	if len(rail.calls) != 1 {
		t.Error("a failed run was dispatched again")
	}
}

// A transaction reported with no chain at all fails closed the same way.
func TestIntake_ATransactionWithNoChainFailsTheRun(t *testing.T) {
	f := fixture(t)
	rail := &fakeRail{chainID: -1, result: func(_ int, sent []keeperhub.Recipient) keeperhub.Execution {
		var steps [][]keeperhub.LegResult
		for j := range sent {
			steps = append(steps, leg(j, true, fmt.Sprintf("0xddd%d", j)))
		}
		return execOf("success", sent, steps...)
	}}
	s := &Service{Pool: f.d.Pool, Rail: rail}
	res, err := s.Release(context.Background(), f.req())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Intake(context.Background(), f.hid, res.AttemptID); !errors.Is(err, ErrChainMismatch) {
		t.Fatalf("err = %v, want ErrChainMismatch for a transaction with no reported chain", err)
	}
}

// The chain rows the payout workflow depends on exist with the right ids.
func TestChainConfigs_BaseSepoliaIsRegisteredAsItsOwnChain(t *testing.T) {
	d := dbtest.DB(t)
	rows, err := d.Pool.Query(context.Background(),
		`SELECT chain_id, family, evm_chain_id, network FROM chain_configs WHERE chain_id IN ('base', 'base-sepolia') ORDER BY chain_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var id, fam, net string
		var n int64
		rows.Scan(&id, &fam, &n, &net)
		got[id] = fmt.Sprintf("%s/%d/%s", fam, n, net)
	}
	if got["base-sepolia"] != "evm/84532/testnet" {
		t.Errorf("base-sepolia = %q, want evm/84532/testnet", got["base-sepolia"])
	}
	if got["base"] != "evm/8453/mainnet" {
		t.Errorf("base = %q, want evm/8453/mainnet - if this is empty, 20260916090100 was skipped "+
			"(see the merge-order note in the migration)", got["base"])
	}
}
