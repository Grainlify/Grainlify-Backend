package hackathon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// PriorCompletionCap bounds how many completions compound into the
// prior_completion draw weight. Structural rather than configurable: it is
// what keeps accumulated wins from overtaking demonstrated capability, and
// that ordering must not be something a config edit can silently invert
// mid-event. See weightsFor for the measurements behind the value.
//
// Exported because the public rules page publishes it - a structural rule
// contributors plan around has to be visible, not buried in code.
const PriorCompletionCap = 2

// Candidate is one applicant in a draw pool, with the ticket arithmetic that
// produced their odds. Weights is kept broken out per factor (not just the
// product) because an appeal asks "why did they have more tickets than me",
// and a single float can't answer that.
type Candidate struct {
	UserID      uuid.UUID          `json:"user_id"`
	GitHubLogin string             `json:"github_login"`
	Fit         string             `json:"fit"`
	IsNewcomer  bool               `json:"is_newcomer"`
	Weights     map[string]float64 `json:"weights"`
	Tickets     float64            `json:"tickets"`
}

// DrawResult is one issue's draw. It is written to hackathon_draws whether
// or not it produced a winner, and whether or not it was a simulation - an
// issue nobody won is exactly the case that needs explaining later.
type DrawResult struct {
	DrawID             uuid.UUID   `json:"draw_id"`
	IssueID            uuid.UUID   `json:"hackathon_issue_id"`
	Seed               int64       `json:"seed"`
	Pool               []Candidate `json:"pool"`
	WinnerUserID       *uuid.UUID  `json:"winner_user_id"`
	WinnerLogin        string      `json:"winner_login,omitempty"`
	UsedWeakPool       bool        `json:"used_weak_pool"`
	ReservationApplied bool        `json:"reservation_applied"`
	ReservationFellBk  bool        `json:"reservation_fell_back"`
	FirstComeFallback  bool        `json:"first_come_fallback"`
	NoWinnerReason     string      `json:"no_winner_reason,omitempty"`
	IsSimulation       bool        `json:"is_simulation"`
}

// drawApplicant is a row from the applications table plus the per-user
// history the weights need.
type drawApplicant struct {
	applicationID uuid.UUID
	userID        uuid.UUID
	login         string
	fit           string
	diffMatch     string
	appliedAt     time.Time
	completions   int
	abandons      int
	// priorAssignments is how many GrainHack issues this contributor has ever
	// been assigned, across every event. Zero is what "newcomer" means for
	// the first-ever bonus - see weightsFor.
	priorAssignments int
}

// weightsFor computes a candidate's ticket count from §3.9. Base 1.0,
// multiply every applicable factor.
//
// §3.9's "Not available as weights, by design" list (total PR count, merge
// rate, followers, stars, total contributions, application text quality) is
// enforced by omission: there is no code path here that can read them.
func weightsFor(a drawApplicant, cfg map[string]string) map[string]float64 {
	w := map[string]float64{}

	switch a.fit {
	case "strong":
		w["fit_strong"] = atofOr(cfg["weight_fit_strong"], 2.0)
	case "weak":
		w["fit_weak"] = atofOr(cfg["weight_fit_weak"], 0.25)
	default: // "plausible", and anything unassessed
		w["fit_plausible"] = atofOr(cfg["weight_fit_plausible"], 1.0)
	}

	// Only "above" is penalised: the issue is harder than the demonstrated
	// level. "below" is not penalised - an experienced contributor taking
	// an easy issue is fine, and taxing it would push newcomers off easy
	// issues by crowding.
	if a.diffMatch == "above" {
		w["difficulty_above"] = atofOr(cfg["weight_difficulty_above"], 0.5)
	}

	if a.completions > 0 {
		// §3.9 applies this "per prior completion", i.e. compounding - but
		// clamped, which the spec does not say and which is deliberate.
		//
		// Uncompounded it would be the only unbounded term in the formula:
		// every other factor multiplies once, so by mid-event accumulated
		// wins overtake demonstrated capability and keep growing. Measured
		// at spec defaults, an uncapped plausible-fit veteran passes a
		// strong-fit newcomer at 3 completions (3.375 vs 3.000) and reaches
		// 2.5x by 5. That inverts the ordering this pipeline exists to
		// protect - capability for *this issue* should outrank having won
		// before - and it inverts harder the longer an event runs, which is
		// precisely when newcomers most need a way in.
		//
		// It also interacts badly with max_issues_per_contributor_per_org:
		// someone who has hit the cap on one org would otherwise carry
		// their full compounded multiplier into every other org's pool.
		//
		// Clamped, the term maxes at 1.5^2 = 2.25, which stays below what a
		// strong fit buys a newcomer (2.0 x 1.5 = 3.0) no matter how long
		// the event runs. TestTicketOrdering_AccumulatedWinsNeverOutrankCapability
		// pins that ordering.
		n := a.completions
		if n > PriorCompletionCap {
			n = PriorCompletionCap
		}
		base := atofOr(cfg["weight_prior_completion"], 1.5)
		f := 1.0
		for i := 0; i < n; i++ {
			f *= base
		}
		w["prior_completion"] = f
	}

	// The newcomer bonus, anchored to never having been *assigned* an issue -
	// not to having no other application on record.
	//
	// It was originally keyed off the applicant's prior application count,
	// which inverted the whole point of the bonus: a genuine first-timer who
	// applied to a second issue immediately lost the 1.5x on *both*, because
	// each application counted as the other's prior. The bonus meant to widen
	// a newcomer's way in instead punished them for using more than one door,
	// and nothing in the product surfaced that - applications are free, the
	// cap allows five, and nothing warned that the second one devalued the
	// first.
	//
	// Anchoring on assignments makes the term mean what it says: a newcomer
	// keeps it across every application they hold until they actually win
	// one, which is the moment they stop being a newcomer. It also makes the
	// weight independent of how many issues someone applied to, which is what
	// lets the published rule "applying to more issues does not change your
	// odds on any one issue" be true.
	//
	// Assignments are counted across every event, not just this one, for the
	// same reason completions are: "first ever" is a claim about the
	// contributor, not about their conduct in one hackathon.
	//
	// The config key and the weight label both keep their original names.
	// The key is snapshotted into live events (§1.1) and the label is stored
	// in every past hackathon_draws row, so renaming either would break
	// replay of draws that have already happened.
	if a.priorAssignments == 0 {
		w["first_ever_application"] = atofOr(cfg["weight_first_ever_application"], 1.5)
	}

	if a.abandons > 0 {
		base := atofOr(cfg["weight_per_abandon"], 0.5)
		f := 1.0
		for i := 0; i < a.abandons; i++ {
			f *= base
		}
		w["per_abandon"] = f
	}

	return w
}

func ticketsFrom(w map[string]float64) float64 {
	t := 1.0
	// Iterate sorted so the product's floating-point rounding is identical
	// on every run - a replayed draw must reproduce bit-for-bit.
	keys := make([]string, 0, len(w))
	for k := range w {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t *= w[k]
	}
	if t < 0 {
		return 0
	}
	return t
}

// loadDrawApplicants reads every applicant still in contention for issueID,
// with the per-user counts the weights need.
func loadDrawApplicants(ctx context.Context, pool db.DBPool, hackathonID, issueID uuid.UUID) ([]drawApplicant, error) {
	rows, err := pool.Query(ctx, `
SELECT
  a.id, a.user_id, a.github_login, COALESCE(a.fit, 'plausible'), COALESCE(a.difficulty_match, 'matched'), a.created_at,
  (SELECT count(*) FROM hackathon_assignments x
     WHERE x.user_id = a.user_id AND x.status = 'completed') AS completions,
  (SELECT count(*) FROM hackathon_assignments x
     WHERE x.user_id = a.user_id AND x.hackathon_id = a.hackathon_id AND x.abandon_recorded) AS abandons,
  (SELECT count(*) FROM hackathon_assignments x
     WHERE x.user_id = a.user_id) AS prior_assignments
FROM hackathon_issue_applications a
WHERE a.hackathon_id = $1 AND a.hackathon_issue_id = $2 AND a.status = 'applied'
ORDER BY a.created_at ASC, a.id ASC
`, hackathonID, issueID)
	if err != nil {
		return nil, fmt.Errorf("hackathon.loadDrawApplicants: %w", err)
	}
	defer rows.Close()

	var out []drawApplicant
	for rows.Next() {
		var a drawApplicant
		if err := rows.Scan(&a.applicationID, &a.userID, &a.login, &a.fit, &a.diffMatch, &a.appliedAt,
			&a.completions, &a.abandons, &a.priorAssignments); err != nil {
			return nil, fmt.Errorf("hackathon.loadDrawApplicants: scan: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// pickWeighted draws one index from tickets using rng. Returns -1 when every
// candidate has zero tickets (possible once penalties multiply down).
func pickWeighted(rng *rand.Rand, tickets []float64) int {
	total := 0.0
	for _, t := range tickets {
		total += t
	}
	if total <= 0 {
		return -1
	}
	roll := rng.Float64() * total
	acc := 0.0
	for i, t := range tickets {
		acc += t
		if roll < acc {
			return i
		}
	}
	return len(tickets) - 1
}

// RunDraw executes AI-specs.md §4.5 for one issue.
//
// simulate=true runs the identical pipeline against the identical real
// applicant pool and writes the same hackathon_draws record, but writes no
// assignment and consumes no slot. Assignment cannot run in shadow mode the
// way judging can - it either assigns or it doesn't - so this is the only
// way to sanity-check weights against a real pool before an event decides
// anything for real. Pass a non-zero seed to replay a previous draw exactly.
func RunDraw(
	ctx context.Context,
	pool db.DBPool,
	hackathonID, issueID uuid.UUID,
	seed int64,
	simulate bool,
) (*DrawResult, error) {
	cfg, err := EffectiveValues(ctx, pool, &hackathonID)
	if err != nil {
		return nil, fmt.Errorf("hackathon.RunDraw: config: %w", err)
	}

	var reserved *bool
	var projectID uuid.UUID
	var issueNumber int
	var orgLogin string
	if err := pool.QueryRow(ctx, `
SELECT reserved, project_id, issue_number, org_login FROM hackathon_issues WHERE id = $1 AND hackathon_id = $2
`, issueID, hackathonID).Scan(&reserved, &projectID, &issueNumber, &orgLogin); err != nil {
		return nil, fmt.Errorf("hackathon.RunDraw: load issue: %w", err)
	}

	applicants, err := loadDrawApplicants(ctx, pool, hackathonID, issueID)
	if err != nil {
		return nil, err
	}

	res := &DrawResult{IssueID: issueID, IsSimulation: simulate}
	if seed == 0 {
		seed = time.Now().UnixNano()
	}
	res.Seed = seed
	rng := rand.New(rand.NewSource(seed))

	// Step 1: build the pool. Prefer plausible-or-better; fall back to the
	// weak pool only if configured, since an unassigned issue helps nobody.
	strongEnough := make([]drawApplicant, 0, len(applicants))
	weak := make([]drawApplicant, 0, len(applicants))
	for _, a := range applicants {
		if a.fit == "weak" {
			weak = append(weak, a)
		} else {
			strongEnough = append(strongEnough, a)
		}
	}
	candidates := strongEnough
	if len(candidates) == 0 && len(weak) > 0 && cfg["draw_from_weak_pool_if_empty"] == "true" {
		candidates = weak
		res.UsedWeakPool = true
	}

	// Step 2: newcomer reservation. Restrict to contributors with zero
	// completed GrainHack issues; fall back to the open pool if none
	// applied and the config allows.
	if reserved != nil && *reserved && cfg["newcomer_reservation_enabled"] == "true" {
		res.ReservationApplied = true
		newcomers := make([]drawApplicant, 0, len(candidates))
		for _, a := range candidates {
			if a.completions == 0 {
				newcomers = append(newcomers, a)
			}
		}
		if len(newcomers) > 0 {
			candidates = newcomers
		} else if cfg["reservation_fallback_to_open_pool"] == "true" {
			res.ReservationFellBk = true
		} else {
			candidates = nil
		}
	}

	// Step 3: tickets.
	tickets := make([]float64, len(candidates))
	res.Pool = make([]Candidate, len(candidates))
	for i, a := range candidates {
		w := weightsFor(a, cfg)
		t := ticketsFrom(w)
		tickets[i] = t
		res.Pool[i] = Candidate{
			UserID:      a.userID,
			GitHubLogin: a.login,
			Fit:         a.fit,
			IsNewcomer:  a.completions == 0,
			Weights:     w,
			Tickets:     t,
		}
	}

	// Step 4: draw. With nobody in the pool, §3.7's first-come fallback
	// takes the earliest applicant that passed the gates.
	winnerIdx := -1
	if len(candidates) > 0 {
		winnerIdx = pickWeighted(rng, tickets)
	}
	if winnerIdx < 0 && len(applicants) > 0 && cfg["fallback_to_first_come"] == "true" {
		res.FirstComeFallback = true
		candidates = applicants[:1]
		winnerIdx = 0
		if len(res.Pool) == 0 {
			res.Pool = []Candidate{{
				UserID:      applicants[0].userID,
				GitHubLogin: applicants[0].login,
				Fit:         applicants[0].fit,
				IsNewcomer:  applicants[0].completions == 0,
				Weights:     map[string]float64{"first_come_fallback": 1},
				Tickets:     1,
			}}
		}
	}

	if winnerIdx < 0 {
		if len(applicants) == 0 {
			res.NoWinnerReason = "no applications"
		} else {
			res.NoWinnerReason = "no eligible applicants after pool and reservation filtering"
		}
	} else {
		w := candidates[winnerIdx]
		res.WinnerUserID = &w.userID
		res.WinnerLogin = w.login
	}

	// Persist the draw record before any assignment write, so even a
	// failure to assign leaves the reasoning on disk.
	poolJSON, err := json.Marshal(res.Pool)
	if err != nil {
		return nil, fmt.Errorf("hackathon.RunDraw: marshal pool: %w", err)
	}
	if err := pool.QueryRow(ctx, `
INSERT INTO hackathon_draws
  (hackathon_id, hackathon_issue_id, seed, pool, pool_size, winner_user_id,
   used_weak_pool, reservation_applied, reservation_fell_back, first_come_fallback,
   no_winner_reason, is_simulation)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,NULLIF($11,''),$12)
RETURNING id
`, hackathonID, issueID, res.Seed, poolJSON, len(res.Pool), res.WinnerUserID,
		res.UsedWeakPool, res.ReservationApplied, res.ReservationFellBk, res.FirstComeFallback,
		res.NoWinnerReason, simulate).Scan(&res.DrawID); err != nil {
		return nil, fmt.Errorf("hackathon.RunDraw: insert draw: %w", err)
	}

	if simulate || res.WinnerUserID == nil {
		return res, nil
	}

	// Step 5/6: consume the slot and write the assignment atomically.
	assigned, err := commitAssignment(ctx, pool, assignmentSpec{
		HackathonID: hackathonID,
		IssueID:     issueID,
		ProjectID:   projectID,
		IssueNumber: issueNumber,
		OrgLogin:    orgLogin,
		UserID:      *res.WinnerUserID,
		GitHubLogin: res.WinnerLogin,
		DrawID:      res.DrawID,
	})
	if err != nil {
		return nil, err
	}
	if !assigned {
		// The winner's slot went away between the draw and the write -
		// under sequential draws they won an earlier issue in this same
		// batch. §4.5.5 requires the check and the write be one
		// transaction; this is that check failing, which is correct
		// behaviour, not an error.
		res.WinnerUserID = nil
		res.WinnerLogin = ""
		res.NoWinnerReason = "winner's slot was consumed by an earlier draw in this batch"
		_, _ = pool.Exec(ctx, `
UPDATE hackathon_draws SET winner_user_id = NULL, no_winner_reason = $2 WHERE id = $1
`, res.DrawID, res.NoWinnerReason)
	}
	return res, nil
}

// assignmentSpec is the tuple commitAssignment needs; grouped into a struct
// because a 9-positional-argument call is unreadable at the call site.
type assignmentSpec struct {
	HackathonID uuid.UUID
	IssueID     uuid.UUID
	ProjectID   uuid.UUID
	IssueNumber int
	OrgLogin    string
	UserID      uuid.UUID
	GitHubLogin string
	DrawID      uuid.UUID
}

// commitAssignment performs AI-specs.md §4.5.5: "The slot check and
// assignment write must be one transaction. Under sequential draws, a
// contributor who wins issue A must be unable to win issue B in the same
// batch if that exceeds their slots."
//
// Returns false (not an error) when the winner no longer has a free slot or
// the issue was assigned by a concurrent draw - both are ordinary outcomes.
func commitAssignment(ctx context.Context, pool db.DBPool, s assignmentSpec) (bool, error) {
	env, err := loadGateEnv(ctx, pool, s.HackathonID)
	if err != nil {
		return false, err
	}
	staleDays, err := EffectiveValue(ctx, pool, &s.HackathonID, "stale_assignment_days")
	if err != nil {
		return false, err
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("hackathon.commitAssignment: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Serialise concurrent draws for the same contributor. Without this,
	// two draws running at once both read "1 slot held" and both write,
	// putting the contributor over their cap.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		s.HackathonID.String()+":"+s.UserID.String()); err != nil {
		return false, fmt.Errorf("hackathon.commitAssignment: lock: %w", err)
	}

	slots, err := EffectiveSlots(ctx, tx, s.HackathonID, s.UserID, env)
	if err != nil {
		return false, err
	}
	var held int
	if err := tx.QueryRow(ctx, `
SELECT count(*) FROM hackathon_assignments
WHERE hackathon_id = $1 AND user_id = $2 AND holds_slot
`, s.HackathonID, s.UserID).Scan(&held); err != nil {
		return false, fmt.Errorf("hackathon.commitAssignment: recount slots: %w", err)
	}
	if held >= slots {
		return false, nil
	}

	// The per-org cap has to be re-checked here too, for the same reason.
	var wonFromOrg int
	if err := tx.QueryRow(ctx, `
SELECT count(*) FROM hackathon_assignments
WHERE hackathon_id = $1 AND user_id = $2 AND org_login = $3
`, s.HackathonID, s.UserID, s.OrgLogin).Scan(&wonFromOrg); err != nil {
		return false, fmt.Errorf("hackathon.commitAssignment: recount org cap: %w", err)
	}
	if env.maxPerOrg > 0 && wonFromOrg >= env.maxPerOrg {
		return false, nil
	}

	staleAt := time.Now().UTC().AddDate(0, 0, atoiOr(staleDays, 5))
	pa := ComputePriorAssociation(ctx, pool, s.HackathonID, s.UserID, s.GitHubLogin, s.OrgLogin)
	paJSON, _ := json.Marshal(pa)

	var assignmentID uuid.UUID
	err = tx.QueryRow(ctx, `
INSERT INTO hackathon_assignments
  (hackathon_id, hackathon_issue_id, project_id, issue_number, user_id, github_login,
   org_login, draw_id, status, holds_slot, stale_at, prior_association)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'active',true,$9,$10)
ON CONFLICT (hackathon_issue_id) WHERE status IN ('active','pr_submitted') DO NOTHING
RETURNING id
`, s.HackathonID, s.IssueID, s.ProjectID, s.IssueNumber, s.UserID, s.GitHubLogin,
		s.OrgLogin, s.DrawID, staleAt, paJSON).Scan(&assignmentID)
	if errors.Is(err, pgx.ErrNoRows) {
		// A concurrent draw already assigned this issue.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("hackathon.commitAssignment: insert: %w", err)
	}

	// Mark the winner won and everyone else lost, in the same transaction.
	if _, err := tx.Exec(ctx, `
UPDATE hackathon_issue_applications
SET status = CASE WHEN user_id = $2 THEN 'won' ELSE 'lost' END, updated_at = now()
WHERE hackathon_issue_id = $1 AND status = 'applied'
`, s.IssueID, s.UserID); err != nil {
		return false, fmt.Errorf("hackathon.commitAssignment: resolve applications: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("hackathon.commitAssignment: commit: %w", err)
	}
	return true, nil
}
