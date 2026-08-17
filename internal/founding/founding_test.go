package founding

import (
	"context"
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
func TestCompute_DividesPoolByEffectiveShares(t *testing.T) {
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

	res, err := Compute(ctx, d.Pool, nil, cfg)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if res.TotalShares != 15 {
		t.Errorf("total effective shares = %v, want 15 (the ineligible member must not be in the divisor)", res.TotalShares)
	}
	if res.ShareValueUSDC < 66.66 || res.ShareValueUSDC > 66.67 {
		t.Errorf("share value = %v, want ~66.667 (1000/15)", res.ShareValueUSDC)
	}

	byUser := map[uuid.UUID]Line{}
	for _, l := range res.Lines {
		byUser[l.UserID] = l
	}
	if got := byUser[a].USDCAmount; got < 999.99 || got > 1000.01 {
		t.Errorf("eligible member payout = %v, want the whole 1000 pool", got)
	}
	if got := byUser[b].USDCAmount; got != 0 {
		t.Errorf("ineligible member payout = %v, want 0", got)
	}
	if byUser[b].IneligibleReason == "" {
		t.Error("ineligible member has no recorded reason; why somebody got nothing has to be answerable")
	}
}

// TestCompute_RefusesWhenNobodyEarnedAnything: dividing by zero would hand
// the first person the entire pool.
func TestCompute_RefusesWhenNobodyEarnedAnything(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	resetFounding(t, d)
	cfg := defaults()

	if _, err := AssignWave(ctx, d.Pool, newUser(t, d), cfg); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if _, err := Compute(ctx, d.Pool, nil, cfg); err == nil {
		t.Error("Compute succeeded with zero shares earned; it must refuse")
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
