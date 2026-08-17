package founding

import (
	"context"
	"errors"
	"math/big"
	"sort"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
)

func defaults() map[string]string {
	return map[string]string{
		"founding_pool_usdc":                    "3000",
		"founding_share_verified_account":       "0.1",
		"founding_share_merged_pr":              "5",
		"founding_share_referral_verified":      "0.5",
		"founding_share_referral_merged_pr":     "5",
		"founding_referral_verified_cap_shares": "10",
		"founding_wave_founding_slots":          "100",
		"founding_wave_two_slots":               "400",
		"founding_multiplier_founding":          "1.5",
		"founding_multiplier_wave_two":          "1.25",
		"founding_multiplier_open":              "1.0",
		"founding_require_social_follow":        "false",
	}
}

// resetFounding gives each test an empty programme.
//
// dbtest.DB hands out a shared local database, so wave membership, the
// boundary lock and the share ledger all persist between tests. That is
// especially corrosive here: sequence numbers are global and gapless by
// design, so one test's members silently shift every later test's expected
// sequence, and the boundary lock is by construction write-once.
//
// Truncating rather than asserting against a baseline keeps the tests
// readable - "the third member holds sequence 3" is the property being
// pinned, and a baseline offset would obscure exactly the off-by-one this
// suite exists to catch.
func resetFounding(t *testing.T, d *db.DB) {
	t.Helper()
	if _, err := d.Pool.Exec(context.Background(), `
TRUNCATE founding_settlement_lines, founding_settlements, founding_shares,
         founding_wave_lock, founding_members RESTART IDENTITY CASCADE
`); err != nil {
		t.Fatalf("reset founding tables: %v", err)
	}
}

func newUser(t *testing.T, d *db.DB) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := d.Pool.QueryRow(context.Background(), `
INSERT INTO users (email, role) VALUES ($1, 'contributor') RETURNING id
`, "founding-"+uuid.NewString()+"@example.com").Scan(&id); err != nil {
		t.Fatalf("create user: %v", err)
	}
	return id
}

// socialFollowWithStatus gives a user a submission in a given state.
func socialFollowWithStatus(t *testing.T, d *db.DB, userID uuid.UUID, status string) {
	t.Helper()
	if _, err := d.Pool.Exec(context.Background(), `
INSERT INTO social_follow_submissions (user_id, linkedin_screenshot, x_screenshot, status)
VALUES ($1, 'data:image/png;base64,x', 'data:image/png;base64,x', $2)
ON CONFLICT (user_id) DO UPDATE SET status = EXCLUDED.status
`, userID, status); err != nil {
		t.Fatalf("set social follow status: %v", err)
	}
}

func completeSocialFollow(t *testing.T, d *db.DB, userID uuid.UUID) {
	t.Helper()
	socialFollowWithStatus(t, d, userID, "approved")
}

// TestWaveFor_BoundariesAtTheEdges pins the wave ranges, including the exact
// slots where one wave ends and the next begins - the boundary is where an
// off-by-one would silently hand somebody the wrong multiplier forever.
func TestWaveFor_BoundariesAtTheEdges(t *testing.T) {
	b := Boundaries{FoundingSlots: 100, WaveTwoSlots: 400,
		MultiplierFounding: 1.5, MultiplierWaveTwo: 1.25, MultiplierOpen: 1.0}

	for _, tc := range []struct {
		seq        int
		wantWave   string
		wantFactor float64
	}{
		{1, WaveFounding, 1.5},
		{100, WaveFounding, 1.5}, // last Founding slot
		{101, WaveTwo, 1.25},     // first Wave 2 slot
		{500, WaveTwo, 1.25},     // last Wave 2 slot
		{501, WaveOpen, 1.0},     // first open slot
		{100000, WaveOpen, 1.0},  // nobody is ever turned away
	} {
		gotWave, gotFactor := b.WaveFor(tc.seq)
		if gotWave != tc.wantWave || gotFactor != tc.wantFactor {
			t.Errorf("WaveFor(%d) = %s/%.2f, want %s/%.2f", tc.seq, gotWave, gotFactor, tc.wantWave, tc.wantFactor)
		}
	}
}

// TestAssignWave_SequentialGaplessAndPermanent covers the three properties a
// wave assignment must have, all of which are unrecoverable once announced.
func TestAssignWave_SequentialGaplessAndPermanent(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	resetFounding(t, d)
	cfg := defaults()

	var ids []uuid.UUID
	for i := 0; i < 5; i++ {
		u := newUser(t, d)
		ids = append(ids, u)
		m, err := AssignWave(ctx, d.Pool, u, cfg)
		if err != nil {
			t.Fatalf("AssignWave: %v", err)
		}
		if m.Sequence != i+1 {
			t.Errorf("member %d got sequence %d, want %d - the sequence must be gapless", i, m.Sequence, i+1)
		}
		if m.Wave != WaveFounding {
			t.Errorf("member %d got wave %s, want founding", i, m.Wave)
		}
	}

	// Permanent: re-assigning returns the same seat rather than a new one.
	again, err := AssignWave(ctx, d.Pool, ids[0], cfg)
	if err != nil {
		t.Fatalf("re-AssignWave: %v", err)
	}
	if again.Sequence != 1 {
		t.Errorf("re-assignment moved the member to sequence %d, want 1 - membership is permanent", again.Sequence)
	}
}

// TestAssignWave_ConcurrentAssignmentsStayGapless is the property the
// advisory lock exists for. Two verifications landing together must not both
// claim the same slot, nor burn one between them: with 100 Founding slots a
// gap is a seat nobody can ever hold, in the tier whose whole value is that
// it is countable.
func TestAssignWave_ConcurrentAssignmentsStayGapless(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	resetFounding(t, d)
	cfg := defaults()

	const n = 12
	users := make([]uuid.UUID, n)
	for i := range users {
		users[i] = newUser(t, d)
	}

	var wg sync.WaitGroup
	seqs := make([]int, n)
	errs := make([]error, n)
	for i := range users {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m, err := AssignWave(ctx, d.Pool, users[i], cfg)
			seqs[i], errs[i] = m.Sequence, err
		}(i)
	}
	wg.Wait()

	seen := map[int]bool{}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent AssignWave[%d]: %v", i, err)
		}
		if seen[seqs[i]] {
			t.Errorf("sequence %d was handed out twice", seqs[i])
		}
		seen[seqs[i]] = true
	}
	for want := 1; want <= n; want++ {
		if !seen[want] {
			t.Errorf("sequence %d missing - the allocation left a gap", want)
		}
	}
}

// TestWaveBoundaries_LockedAfterFirstAssignment is §4.1 in code: an announced
// wave must not widen. A boundary that can move after launch tells everyone
// that Grainlify's published limits are provisional, which is expensive here
// precisely because the anti-farming design depends on published rules being
// believed.
func TestWaveBoundaries_LockedAfterFirstAssignment(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	resetFounding(t, d)
	cfg := defaults()

	if err := CheckBoundaryConfig(ctx, d.Pool, cfg); err != nil {
		t.Fatalf("boundaries should be editable before anyone is assigned: %v", err)
	}

	if _, err := AssignWave(ctx, d.Pool, newUser(t, d), cfg); err != nil {
		t.Fatalf("AssignWave: %v", err)
	}

	widened := defaults()
	widened["founding_wave_founding_slots"] = "500"
	if err := CheckBoundaryConfig(ctx, d.Pool, widened); err == nil {
		t.Error("widening the Founding wave after launch was accepted; it must be refused")
	}

	// And the refusal is not merely advisory: a later assignment still uses
	// the locked boundaries, so the widened config cannot take effect by
	// going around the check.
	for i := 0; i < 3; i++ {
		m, err := AssignWave(ctx, d.Pool, newUser(t, d), widened)
		if err != nil {
			t.Fatalf("AssignWave with widened config: %v", err)
		}
		if m.Multiplier != 1.5 {
			t.Errorf("multiplier = %v, want the locked 1.5", m.Multiplier)
		}
	}
	locked, ok, err := LockedBoundaries(ctx, d.Pool)
	if err != nil || !ok {
		t.Fatalf("LockedBoundaries: %v (ok=%v)", err, ok)
	}
	if locked.FoundingSlots != 100 {
		t.Errorf("locked founding slots = %d, want the original 100", locked.FoundingSlots)
	}
}

// TestGrant_ReferralVerifiedCapHoldsUnderConcurrency is the cap the whole
// anti-farming argument rests on. Referrals arrive in bursts exactly when
// somebody is farming, so a check-then-act race here is the failure that
// matters most.
func TestGrant_ReferralVerifiedCapHoldsUnderConcurrency(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	resetFounding(t, d)
	cfg := defaults() // cap 10 shares, 0.5 each => 20 referrals max

	referrer := newUser(t, d)

	const attempts = 40
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			src := uuid.New()
			_, _ = Grant(ctx, d.Pool, referrer, 0.5, ReasonReferralVerified, &src, cfg)
		}()
	}
	wg.Wait()

	var total float64
	if err := d.Pool.QueryRow(ctx, `
SELECT COALESCE(sum(shares), 0)::float8 FROM founding_shares WHERE user_id = $1 AND reason = $2
`, referrer, ReasonReferralVerified).Scan(&total); err != nil {
		t.Fatalf("sum: %v", err)
	}
	if total > 10.0001 {
		t.Errorf("verify-only referral shares = %v, want at most the 10-share cap - the cap raced", total)
	}
	if total < 9.9999 {
		t.Errorf("verify-only referral shares = %v, want the cap to be reached at 10 with 40 attempts", total)
	}
}

// TestGrant_MergedPRSharesAreUncapped covers the deliberate asymmetry:
// merged-PR shares are self-limiting because producing them takes real work,
// which is the outcome being bought.
func TestGrant_MergedPRSharesAreUncapped(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	resetFounding(t, d)
	cfg := defaults()
	u := newUser(t, d)

	for i := 0; i < 25; i++ {
		src := uuid.New()
		if _, err := Grant(ctx, d.Pool, u, 5, ReasonMergedPR, &src, cfg); err != nil {
			t.Fatalf("Grant merged_pr: %v", err)
		}
	}
	total, err := TotalFor(ctx, d.Pool, u)
	if err != nil {
		t.Fatalf("TotalFor: %v", err)
	}
	if total != 125 {
		t.Errorf("merged-PR shares = %v, want 125 (uncapped)", total)
	}
}

// TestGrant_IsIdempotentPerSourceEvent: a retried webhook must not pay twice.
func TestGrant_IsIdempotentPerSourceEvent(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	resetFounding(t, d)
	cfg := defaults()
	u := newUser(t, d)
	src := uuid.New()

	first, err := Grant(ctx, d.Pool, u, 5, ReasonMergedPR, &src, cfg)
	if err != nil || first != 5 {
		t.Fatalf("first grant = %v, %v; want 5", first, err)
	}
	second, err := Grant(ctx, d.Pool, u, 5, ReasonMergedPR, &src, cfg)
	if err != nil {
		t.Fatalf("second grant: %v", err)
	}
	if second != 0 {
		t.Errorf("second grant for the same event granted %v, want 0", second)
	}
	total, _ := TotalFor(ctx, d.Pool, u)
	if total != 5 {
		t.Errorf("total after a duplicate = %v, want 5", total)
	}
}

// TestCompute_DividesPoolByEffectiveShares checks the arithmetic and the two
// things around it that are easy to get wrong: the multiplier applies to a
// member's whole total, and an ineligible member is excluded from the divisor
// rather than silently shrinking everyone else's payout.
func TestDryRun_DividesPoolByEffectiveShares(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	resetFounding(t, d)
	cfg := defaults()
	cfg["founding_pool_usdc"] = "1000"
	cfg["founding_require_social_follow"] = "true"

	// Founding member, follows, 10 raw shares -> 15 effective.
	a := newUser(t, d)
	completeSocialFollow(t, d, a)
	if _, err := AssignWave(ctx, d.Pool, a, cfg); err != nil {
		t.Fatalf("assign a: %v", err)
	}
	srcA := uuid.New()
	if _, err := Grant(ctx, d.Pool, a, 10, ReasonMergedPR, &srcA, cfg); err != nil {
		t.Fatalf("grant a: %v", err)
	}

	// Same shares, but never followed -> ineligible, excluded entirely.
	//
	// Assigned with the gate OFF, then settled with it on. That is not a
	// contrivance: it is exactly how the seventeen members who hold a position
	// without an approved submission came to exist - they were assigned before
	// the gate existed, and AssignWave's short-circuit deliberately never
	// re-examines an existing member, so they keep their number for good.
	//
	// Entry is permanent; payment is not. This test pins the second half, and
	// the gate does not change it: a member who cannot be paid is excluded
	// from the divisor rather than silently shrinking everyone else's payout.
	b := newUser(t, d)
	ungated := defaults()
	ungated["founding_require_social_follow"] = "false"
	if _, err := AssignWave(ctx, d.Pool, b, ungated); err != nil {
		t.Fatalf("assign b: %v", err)
	}
	srcB := uuid.New()
	if _, err := Grant(ctx, d.Pool, b, 10, ReasonMergedPR, &srcB, cfg); err != nil {
		t.Fatalf("grant b: %v", err)
	}

	res, err := DryRun(ctx, d.Pool, cfg)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if want := big.NewRat(15, 1); res.TotalEffective.Cmp(want) != 0 {
		t.Errorf("total effective shares = %v, want 15 (the ineligible member must not be in the divisor)", res.TotalEffective)
	}

	byUser := map[uuid.UUID]Line{}
	for _, l := range res.Lines {
		byUser[l.UserID] = l
	}
	// The whole 1000 pool, exactly, in minor units - not "about 1000".
	if got := byUser[a].AmountMinor; got.Cmp(big.NewInt(1_000_000_000)) != 0 {
		t.Errorf("eligible member payout = %s minor units, want exactly 1000000000", got)
	}
	if got := byUser[b].AmountMinor; got.Sign() != 0 {
		t.Errorf("ineligible member payout = %s, want 0", got)
	}
	if byUser[b].IneligibleReason == "" {
		t.Error("ineligible member has no recorded reason; why somebody got nothing has to be answerable")
	}

	// A zero line must never reach a tree: the contract rejects amount <= 0,
	// so the leaf would be permanently unclaimable, and it would commit "this
	// person got nothing" to a root that cannot be edited.
	for _, l := range res.PayableLines() {
		if l.UserID == b {
			t.Error("the ineligible member appears in PayableLines")
		}
	}

	// DryRun writes nothing. This is the property that lets the output be read
	// and re-read before a chain step without recording a settlement.
	var settlements int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*)::int FROM founding_settlements`).Scan(&settlements); err != nil {
		t.Fatalf("count settlements: %v", err)
	}
	if settlements != 0 {
		t.Errorf("DryRun wrote %d settlement rows; it must write none", settlements)
	}
	if res.SettlementID != uuid.Nil {
		t.Error("DryRun set a SettlementID; only Persist may do that")
	}
}

// TestDryRun_LinesSumExactlyToThePool is the invariant that cannot be checked
// after the fact.
//
// A root larger than the escrow is refused by the contract, so an over-
// allocation will simply not publish. An under-allocation is worse: it
// publishes, and the difference is stranded in escrow behind the sweep
// timelock with no way to pay it to the people it belonged to.
//
// The share counts here are chosen to force a remainder - three members
// splitting a pool that does not divide by three.
func TestDryRun_LinesSumExactlyToThePool(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	resetFounding(t, d)
	cfg := defaults()
	cfg["founding_pool_usdc"] = "1000"
	cfg["founding_require_social_follow"] = "true"

	for i := 0; i < 3; i++ {
		u := newUser(t, d)
		completeSocialFollow(t, d, u)
		if _, err := AssignWave(ctx, d.Pool, u, cfg); err != nil {
			t.Fatalf("assign: %v", err)
		}
		src := uuid.New()
		if _, err := Grant(ctx, d.Pool, u, 1, ReasonMergedPR, &src, cfg); err != nil {
			t.Fatalf("grant: %v", err)
		}
	}

	res, err := DryRun(ctx, d.Pool, cfg)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}

	want := big.NewInt(1_000_000_000) // 1000 USDC at 6 decimals
	if got := res.TotalAllocatedMinor(); got.Cmp(want) != 0 {
		t.Fatalf("allocated %s minor units, want exactly %s", got, want)
	}
	if res.PoolMinor.Cmp(want) != 0 {
		t.Fatalf("pool = %s minor units, want %s", res.PoolMinor, want)
	}

	// 1000000000 / 3 = 333333333 remainder 1. Exactly one member receives the
	// extra unit; the other two receive the floor.
	var extra int
	for _, l := range res.Lines {
		switch l.AmountMinor.Int64() {
		case 333_333_334:
			extra++
		case 333_333_333:
		default:
			t.Errorf("unexpected allocation %s", l.AmountMinor)
		}
	}
	if extra != 1 {
		t.Errorf("%d members received the leftover unit, want exactly 1", extra)
	}
}

// TestDryRun_RemainderGoesToTheLowestUserIDOnATie pins the tie-break rule.
//
// Three identical members means three identical remainders, so the rule is the
// only thing deciding who gets the odd minor unit. Left to map or query order
// it would differ between runs, and a settlement nobody can recompute cannot
// answer a dispute months later.
func TestDryRun_RemainderGoesToTheLowestUserIDOnATie(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	resetFounding(t, d)
	cfg := defaults()
	cfg["founding_pool_usdc"] = "1000"
	cfg["founding_require_social_follow"] = "true"

	var ids []uuid.UUID
	for i := 0; i < 3; i++ {
		u := newUser(t, d)
		completeSocialFollow(t, d, u)
		if _, err := AssignWave(ctx, d.Pool, u, cfg); err != nil {
			t.Fatalf("assign: %v", err)
		}
		src := uuid.New()
		if _, err := Grant(ctx, d.Pool, u, 1, ReasonMergedPR, &src, cfg); err != nil {
			t.Fatalf("grant: %v", err)
		}
		ids = append(ids, u)
	}
	sort.Slice(ids, func(a, b int) bool { return ids[a].String() < ids[b].String() })

	// Same inputs, twice: the same person must win both times.
	for run := 0; run < 2; run++ {
		res, err := DryRun(ctx, d.Pool, cfg)
		if err != nil {
			t.Fatalf("DryRun: %v", err)
		}
		for _, l := range res.Lines {
			if l.AmountMinor.Int64() == 333_333_334 && l.UserID != ids[0] {
				t.Errorf("run %d: leftover unit went to %s, want the lowest user id %s", run, l.UserID, ids[0])
			}
		}
	}
}

// TestDryRun_LargestRemainderWinsTheLeftoverUnit pins the apportionment rule
// itself, as distinct from the tie-break.
//
// Three members at 1, 2 and 4 raw shares split 1000 USDC in a ratio of 1:2:4,
// which divides by seven and so leaves a remainder for everybody:
//
//	1/7 of 1e9 = 142857142.857...  floor 142857142  remainder 6/7  <- largest
//	2/7 of 1e9 = 285714285.714...  floor 285714285  remainder 5/7
//	4/7 of 1e9 = 571428571.428...  floor 571428571  remainder 3/7  <- smallest
//
// The floors sum to 999999998, so exactly two leftover units exist and the two
// largest remainders take them. The member with the FEWEST shares therefore
// gains a unit and the member with the MOST does not, which is the assertion
// that separates largest-remainder from smallest-remainder - a distinction the
// identical-member tie test above cannot see, because there every remainder is
// equal and any ordering produces the same totals.
func TestDryRun_LargestRemainderWinsTheLeftoverUnit(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	resetFounding(t, d)
	cfg := defaults()
	cfg["founding_pool_usdc"] = "1000"
	cfg["founding_require_social_follow"] = "true"

	rawByUser := map[uuid.UUID]int64{}
	for _, raw := range []int64{1, 2, 4} {
		u := newUser(t, d)
		completeSocialFollow(t, d, u)
		if _, err := AssignWave(ctx, d.Pool, u, cfg); err != nil {
			t.Fatalf("assign: %v", err)
		}
		src := uuid.New()
		if _, err := Grant(ctx, d.Pool, u, float64(raw), ReasonMergedPR, &src, cfg); err != nil {
			t.Fatalf("grant: %v", err)
		}
		rawByUser[u] = raw
	}

	res, err := DryRun(ctx, d.Pool, cfg)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}

	got := map[int64]int64{}
	for _, l := range res.Lines {
		got[rawByUser[l.UserID]] = l.AmountMinor.Int64()
	}

	// The smallest holder took a leftover unit; the largest did not.
	want := map[int64]int64{
		1: 142_857_143,
		2: 285_714_286,
		4: 571_428_571,
	}
	for raw, w := range want {
		if got[raw] != w {
			t.Errorf("member with %d raw shares got %d minor units, want %d", raw, got[raw], w)
		}
	}
	if total := res.TotalAllocatedMinor(); total.Cmp(big.NewInt(1_000_000_000)) != 0 {
		t.Errorf("allocated %s, want exactly 1000000000", total)
	}
}

// TestApportion_RefusesAnInconsistentDivisor exercises the over-allocation
// guard directly.
//
// The guard cannot be reached through DryRun while the apportionment is
// correct, so it is driven here with a divisor smaller than the shares it is
// meant to divide - the shape a future change to the method could produce. No
// database is involved.
func TestApportion_RefusesAnInconsistentDivisor(t *testing.T) {
	lines := []Line{
		{UserID: uuid.New(), EffectiveShares: big.NewRat(3, 1)},
		{UserID: uuid.New(), EffectiveShares: big.NewRat(3, 1)},
	}
	// A divisor of 1 against 6 shares means the floors alone claim six times
	// the pool.
	err := apportion(lines, big.NewInt(1_000_000), big.NewRat(1, 1))
	if !errors.Is(err, ErrAllocationMismatch) {
		t.Errorf("apportion error = %v, want ErrAllocationMismatch", err)
	}
}

// TestDryRun_RefusesAPoolItCannotExpressExactly covers the other end of the
// conversion. A pool configured with more precision than the asset carries
// would settle a different number from the one announced, so it is refused
// rather than rounded.
func TestDryRun_RefusesAPoolItCannotExpressExactly(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	resetFounding(t, d)
	cfg := defaults()
	cfg["founding_pool_usdc"] = "1000.0000001" // 7 dp, USDC has 6
	cfg["founding_require_social_follow"] = "true"

	u := newUser(t, d)
	completeSocialFollow(t, d, u)
	if _, err := AssignWave(ctx, d.Pool, u, cfg); err != nil {
		t.Fatalf("assign: %v", err)
	}
	src := uuid.New()
	if _, err := Grant(ctx, d.Pool, u, 1, ReasonMergedPR, &src, cfg); err != nil {
		t.Fatalf("grant: %v", err)
	}

	if _, err := DryRun(ctx, d.Pool, cfg); err == nil {
		t.Error("DryRun accepted a pool with more precision than USDC can express")
	}
}

// TestPersist_RecordsWhatWasApproved covers the half DryRun deliberately does
// not do, including that the exact integer reaches the row rather than only a
// decimal rendering of it.
func TestPersist_RecordsWhatWasApproved(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	resetFounding(t, d)
	cfg := defaults()
	cfg["founding_pool_usdc"] = "1000"
	cfg["founding_require_social_follow"] = "true"

	u := newUser(t, d)
	completeSocialFollow(t, d, u)
	if _, err := AssignWave(ctx, d.Pool, u, cfg); err != nil {
		t.Fatalf("assign: %v", err)
	}
	src := uuid.New()
	if _, err := Grant(ctx, d.Pool, u, 3, ReasonMergedPR, &src, cfg); err != nil {
		t.Fatalf("grant: %v", err)
	}

	res, err := DryRun(ctx, d.Pool, cfg)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if err := Persist(ctx, d.Pool, nil, res); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if res.SettlementID == uuid.Nil {
		t.Fatal("Persist did not set a SettlementID")
	}

	var poolMinor, amountMinor int64
	if err := d.Pool.QueryRow(ctx,
		`SELECT pool_minor FROM founding_settlements WHERE id = $1`, res.SettlementID,
	).Scan(&poolMinor); err != nil {
		t.Fatalf("read settlement: %v", err)
	}
	if err := d.Pool.QueryRow(ctx,
		`SELECT amount_minor FROM founding_settlement_lines WHERE settlement_id = $1 AND user_id = $2`,
		res.SettlementID, u,
	).Scan(&amountMinor); err != nil {
		t.Fatalf("read line: %v", err)
	}
	if poolMinor != 1_000_000_000 {
		t.Errorf("stored pool_minor = %d, want 1000000000", poolMinor)
	}
	if amountMinor != 1_000_000_000 {
		t.Errorf("stored amount_minor = %d, want the whole pool", amountMinor)
	}
}

// TestPersist_RefusesAResultThatDoesNotSumToThePool checks the invariant at
// the moment of recording, not only at the moment of computation. A Result is
// an ordinary struct a caller can mutate between the two.
func TestPersist_RefusesAResultThatDoesNotSumToThePool(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	resetFounding(t, d)
	cfg := defaults()
	cfg["founding_pool_usdc"] = "1000"
	cfg["founding_require_social_follow"] = "true"

	u := newUser(t, d)
	completeSocialFollow(t, d, u)
	if _, err := AssignWave(ctx, d.Pool, u, cfg); err != nil {
		t.Fatalf("assign: %v", err)
	}
	src := uuid.New()
	if _, err := Grant(ctx, d.Pool, u, 1, ReasonMergedPR, &src, cfg); err != nil {
		t.Fatalf("grant: %v", err)
	}

	res, err := DryRun(ctx, d.Pool, cfg)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	res.Lines[0].AmountMinor.Sub(res.Lines[0].AmountMinor, big.NewInt(1))

	if err := Persist(ctx, d.Pool, nil, res); !errors.Is(err, ErrAllocationMismatch) {
		t.Errorf("Persist error = %v, want ErrAllocationMismatch", err)
	}
	var settlements int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*)::int FROM founding_settlements`).Scan(&settlements); err != nil {
		t.Fatalf("count: %v", err)
	}
	if settlements != 0 {
		t.Errorf("a refused Persist wrote %d settlement rows; it must write none", settlements)
	}
}

// TestCompute_RefusesWhenNobodyEarnedAnything: dividing by zero would hand
// the first person the entire pool.
func TestDryRun_RefusesWhenNobodyEarnedAnything(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	resetFounding(t, d)
	cfg := defaults()

	if _, err := AssignWave(ctx, d.Pool, newUser(t, d), cfg); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if _, err := DryRun(ctx, d.Pool, cfg); !errors.Is(err, ErrNoShares) {
		t.Errorf("DryRun error = %v, want ErrNoShares; dividing by zero would pay the first person everything", err)
	}
}

// TestProgress_ReportsClaimedNotRemaining. With a small community a
// remaining-count advertises emptiness; a rising claimed-count reads as
// momentum. Same data, opposite signal.
func TestProgress_ReportsClaimedNotRemaining(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	resetFounding(t, d)
	cfg := defaults()

	for i := 0; i < 3; i++ {
		if _, err := AssignWave(ctx, d.Pool, newUser(t, d), cfg); err != nil {
			t.Fatalf("assign: %v", err)
		}
	}
	p, err := Progress(ctx, d.Pool, cfg)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if p.CurrentWave != WaveFounding || p.Claimed != 3 || p.Total != 100 {
		t.Errorf("progress = %+v, want founding wave with 3 claimed of 100", p)
	}
	if p.NextWave != WaveTwo || p.NextMultiplier != 1.25 {
		t.Errorf("next wave = %s/%v, want wave_two/1.25 - the drop is what creates the urgency", p.NextWave, p.NextMultiplier)
	}
}
