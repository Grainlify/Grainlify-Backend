package keeperhubrail

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
)

// RunView is what the payout admin screen reads for one event and pool.
//
// # Assembled only from our own per-leg rows
//
// KeeperHub has no aggregate for a partially failed run - its post-loop summary
// step does not run when any leg fails - so nothing here comes from KeeperHub,
// and nothing here is a total the system cannot compute. In particular there is
// no "paid", "total paid" or "remaining" figure: money in an unknown leg is
// neither paid nor unpaid until a person resolves it, and folding it into
// either side would state something nobody knows. What IS computable - counts
// and sums per status - is under DerivedFromLegs, labelled as such.
//
// Amounts are strings of integer minor units, never formatted.
type RunView struct {
	Run             RunHeader       `json:"run"`
	DerivedFromLegs DerivedFromLegs `json:"derived_from_legs"`
	Resume          ResumeSummary   `json:"resume"`
	Legs            []LegView       `json:"legs"`
	Attempts        []AttemptView   `json:"attempts"`
	Exclusions      []ExclusionView `json:"exclusions"`
}

// RunHeader is the run row and the chain it pays on.
type RunHeader struct {
	ID          uuid.UUID  `json:"id"`
	HackathonID uuid.UUID  `json:"hackathon_id"`
	Pool        string     `json:"pool"`
	PoolMinor   string     `json:"pool_minor"`
	ChainID     string     `json:"chain_id"`
	EVMChainID  int64      `json:"evm_chain_id"`
	State       string     `json:"state"`
	PayoutRunID uuid.UUID  `json:"payout_run_id"`
	ReleasedBy  *uuid.UUID `json:"released_by"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`

	// From chain_configs. Null when the row does not carry the value; never a
	// guessed default.
	Network             *string `json:"network"`
	AssetSymbol         *string `json:"asset_symbol"`
	AssetDecimals       *int32  `json:"asset_decimals"`
	ExplorerURLTemplate *string `json:"explorer_url_template"`
}

// DerivedFromLegs is every figure computable from the leg rows, per status.
type DerivedFromLegs struct {
	// Note says, in the response itself, what these numbers are and are not.
	Note     string                  `json:"note"`
	LegCount int                     `json:"leg_count"`
	ByStatus map[string]StatusTotals `json:"by_status"`
}

// StatusTotals is the count and the summed amount of the legs in one status.
type StatusTotals struct {
	Count       int    `json:"count"`
	AmountMinor string `json:"amount_minor"`
}

// ResumeSummary is what a release would do right now.
//
// Computed with the same code release runs (resume.go), except for the two
// facts only a release request supplies: the explicit confirmation and the
// acting admin. It is evaluated as though those were given, and says so.
type ResumeSummary struct {
	Allowed bool `json:"allowed"`

	// Reason is the same name the release endpoint would refuse with; empty
	// when allowed.
	Reason string `json:"reason"`
	Detail string `json:"detail"`

	// SendableLegIDs and SendableAmountMinor are what a release would send, in
	// dispatch order. Only set when Allowed: a refused release sends nothing.
	SendableLegIDs      []uuid.UUID `json:"sendable_leg_ids"`
	SendableAmountMinor *string     `json:"sendable_amount_minor"`

	// BlockingLegIDs are the dispatched and unknown legs, whatever else is
	// true.
	BlockingLegIDs []uuid.UUID `json:"blocking_leg_ids"`

	// Assumes is what the summary took as given.
	Assumes string `json:"assumes"`
}

// LegView is one leg.
type LegView struct {
	ID     uuid.UUID `json:"id"`
	UserID uuid.UUID `json:"user_id"`
	// GitHubLogin is read live and is null when the person has no linked
	// GitHub account now. It is display only; the leg is keyed by user id.
	GitHubLogin *string `json:"github_login"`
	Address     string  `json:"address"`
	AmountMinor string  `json:"amount_minor"`

	Status       string  `json:"status"`
	BlocksResume bool    `json:"blocks_resume"`
	BlockReason  *string `json:"block_reason"`
	Resendable   bool    `json:"resendable"`

	TxHash *string `json:"tx_hash"`
	// ExplorerURL is set only when there is a transaction AND the chain has a
	// template with a %s in it.
	ExplorerURL *string `json:"explorer_url"`

	ExecutionID   *string    `json:"execution_id"`
	LastAttemptID *uuid.UUID `json:"last_attempt_id"`
	LastError     *string    `json:"last_error"`
	DispatchedAt  *time.Time `json:"dispatched_at"`
	ConfirmedAt   *time.Time `json:"confirmed_at"`

	ResolutionNote *string    `json:"resolution_note"`
	ResolvedBy     *uuid.UUID `json:"resolved_by"`
}

// AttemptView is one dispatch attempt.
type AttemptView struct {
	// Ordinal is DERIVED: 1-based position in created_at order. There is no
	// attempt-number column.
	Ordinal        int                  `json:"ordinal"`
	ID             uuid.UUID            `json:"id"`
	State          string               `json:"state"`
	ExecutionID    *string              `json:"execution_id"`
	IdempotencyKey *string              `json:"idempotency_key"`
	Error          *string              `json:"error"`
	ActorUserID    *uuid.UUID           `json:"actor_user_id"`
	CreatedAt      time.Time            `json:"created_at"`
	ReconciledAt   *time.Time           `json:"reconciled_at"`
	LegCount       int                  `json:"leg_count"`
	Legs           []AttemptLegPosition `json:"legs"`
}

// AttemptLegPosition is a leg's place in an attempt's dispatched list, which is
// its iteration index in the execution.
type AttemptLegPosition struct {
	Position int       `json:"position"`
	LegID    uuid.UUID `json:"leg_id"`
}

// ExclusionView is somebody who earned an amount and was not made a leg.
type ExclusionView struct {
	UserID      uuid.UUID `json:"user_id"`
	GitHubLogin *string   `json:"github_login"`
	AmountMinor string    `json:"amount_minor"`
	Reason      string    `json:"reason"`
}

// legStatuses is every status a leg can have, so by_status always names all of
// them - a zero count is a real zero here, computed, not an absent key.
var legStatuses = []string{"pending", "dispatched", "confirmed", "failed", "unknown"}

const derivedNote = "Counts and sums of amount_minor per leg status, computed from the leg rows. " +
	"There is deliberately no paid or remaining total: a leg in 'unknown' may or may not have paid, " +
	"and 'dispatched' is not yet read back, so neither can be counted on either side."

const resumeAssumes = "Evaluated as though the release request were explicitly confirmed by the reading admin; " +
	"everything else is checked exactly as release checks it."

// RunView reads one event and pool's run for the admin screen. It calls nothing
// outside this database, so it works whether or not KeeperHub is configured.
//
// railUnavailable is why the rail cannot dispatch at all (unset keys), or nil.
// It changes only the resume summary.
func (s *Service) RunView(ctx context.Context, hackathonID uuid.UUID, pool string, reader uuid.UUID, railUnavailable error) (*RunView, error) {
	if pool != PoolContributor {
		return nil, fmt.Errorf("%w (got %q)", ErrUnsupportedPool, pool)
	}

	var (
		h        RunHeader
		explorer *string
	)
	err := s.Pool.QueryRow(ctx, `
		SELECT r.id, r.hackathon_id, r.pool, r.pool_minor::text, r.chain_id, r.evm_chain_id, r.state,
		       r.hackathon_payout_run_id, r.released_by, r.created_at, r.updated_at,
		       c.network, c.asset->>'symbol', (c.asset->>'decimals')::int, c.explorer_url_template
		FROM keeperhub_payout_runs r
		LEFT JOIN chain_configs c ON c.chain_id = r.chain_id
		WHERE r.hackathon_id = $1 AND r.pool = $2`, hackathonID, pool).
		Scan(&h.ID, &h.HackathonID, &h.Pool, &h.PoolMinor, &h.ChainID, &h.EVMChainID, &h.State,
			&h.PayoutRunID, &h.ReleasedBy, &h.CreatedAt, &h.UpdatedAt,
			&h.Network, &h.AssetSymbol, &h.AssetDecimals, &explorer)
	if isNotFound(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("keeperhubrail: load run: %w", err)
	}
	h.ExplorerURLTemplate = explorer
	v := &RunView{Run: h, Legs: []LegView{}, Attempts: []AttemptView{}, Exclusions: []ExclusionView{}}

	if err := s.loadLegs(ctx, v); err != nil {
		return nil, err
	}
	if err := s.loadAttempts(ctx, v); err != nil {
		return nil, err
	}
	if err := s.loadExclusions(ctx, v); err != nil {
		return nil, err
	}
	v.DerivedFromLegs = deriveFromLegs(v.Legs)

	resume, err := s.resumeSummary(ctx, v.Run, reader, railUnavailable)
	if err != nil {
		return nil, err
	}
	v.Resume = resume
	return v, nil
}

// resumeSummary answers "what would a release do now" with release's own code:
// checkEvent for the event, loadResumeState + refusal for the run.
func (s *Service) resumeSummary(ctx context.Context, run RunHeader, reader uuid.UUID, railUnavailable error) (ResumeSummary, error) {
	out := ResumeSummary{
		SendableLegIDs: []uuid.UUID{},
		BlockingLegIDs: []uuid.UUID{},
		Assumes:        resumeAssumes,
	}
	st, err := loadResumeState(ctx, s.Pool, run.ID, run.State)
	if err != nil {
		return out, err
	}
	if st.Blocking != nil {
		out.BlockingLegIDs = st.Blocking
	}

	refuse := func(reason string, detail error) (ResumeSummary, error) {
		out.Allowed, out.Reason = false, reason
		if detail != nil {
			out.Detail = detail.Error()
		}
		return out, nil
	}

	// Release answers 503 before it reaches the service when the rail is not
	// configured, so that comes first here too.
	if railUnavailable != nil {
		return refuse(ReasonNotConfigured, railUnavailable)
	}
	// The event-level checks, exactly as release runs them, with the request's
	// own values taken from the run: its chain and the computation it pays.
	actor := reader
	if actor == uuid.Nil {
		actor = uuid.Max // the guard only requires that an actor exists
	}
	if err := s.checkEvent(ctx, ReleaseRequest{
		HackathonID: run.HackathonID, PayoutRunID: run.PayoutRunID, ActorID: actor,
		Pool: run.Pool, ChainID: run.ChainID, Confirm: true,
	}); err != nil {
		return refuse(RefusalReason(err), err)
	}
	if err := st.refusal(); err != nil {
		return refuse(RefusalReason(err), err)
	}

	sum := new(big.Int)
	for _, l := range st.Sendable {
		n, ok := new(big.Int).SetString(l.AmountMinor, 10)
		if !ok {
			return out, fmt.Errorf("keeperhubrail: leg %s amount %q is not an integer", l.ID, l.AmountMinor)
		}
		sum.Add(sum, n)
		out.SendableLegIDs = append(out.SendableLegIDs, l.ID)
	}
	total := sum.String()
	out.Allowed = true
	out.SendableAmountMinor = &total
	return out, nil
}

func (s *Service) loadLegs(ctx context.Context, v *RunView) error {
	rows, err := s.Pool.Query(ctx, `
		SELECT l.id, l.user_id,
		       (SELECT g.login FROM github_accounts g WHERE g.user_id = l.user_id ORDER BY g.created_at LIMIT 1),
		       l.address, l.amount_minor::text, l.status, l.tx_hash, l.execution_id, l.last_attempt_id,
		       l.last_error, l.dispatched_at, l.confirmed_at, l.resolution_note, l.resolved_by
		FROM keeperhub_payout_legs l
		WHERE l.run_id = $1
		ORDER BY l.user_id, l.id`, v.Run.ID)
	if err != nil {
		return fmt.Errorf("keeperhubrail: load legs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var l LegView
		if err := rows.Scan(&l.ID, &l.UserID, &l.GitHubLogin, &l.Address, &l.AmountMinor, &l.Status,
			&l.TxHash, &l.ExecutionID, &l.LastAttemptID, &l.LastError, &l.DispatchedAt, &l.ConfirmedAt,
			&l.ResolutionNote, &l.ResolvedBy); err != nil {
			return err
		}
		// The same classification release uses.
		l.BlocksResume = LegBlocks(l.Status)
		if r := LegBlockReason(l.Status); r != "" {
			l.BlockReason = &r
		}
		l.Resendable = LegResendable(l.Status)
		l.ExplorerURL = explorerURL(v.Run.ExplorerURLTemplate, l.TxHash)
		v.Legs = append(v.Legs, l)
	}
	return rows.Err()
}

// explorerURL fills the chain's template, but only when there is a transaction
// and the template actually has a place for it.
func explorerURL(template, tx *string) *string {
	if template == nil || tx == nil || *tx == "" || strings.Count(*template, "%s") != 1 {
		return nil
	}
	u := strings.Replace(*template, "%s", *tx, 1)
	return &u
}

func (s *Service) loadAttempts(ctx context.Context, v *RunView) error {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, state, execution_id, idempotency_key, error, actor_user_id, created_at, reconciled_at
		FROM keeperhub_dispatch_attempts
		WHERE run_id = $1
		ORDER BY created_at, id`, v.Run.ID)
	if err != nil {
		return fmt.Errorf("keeperhubrail: load attempts: %w", err)
	}
	index := map[uuid.UUID]int{}
	for rows.Next() {
		var a AttemptView
		if err := rows.Scan(&a.ID, &a.State, &a.ExecutionID, &a.IdempotencyKey, &a.Error,
			&a.ActorUserID, &a.CreatedAt, &a.ReconciledAt); err != nil {
			rows.Close()
			return err
		}
		a.Ordinal = len(v.Attempts) + 1
		a.Legs = []AttemptLegPosition{}
		index[a.ID] = len(v.Attempts)
		v.Attempts = append(v.Attempts, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	rows, err = s.Pool.Query(ctx, `
		SELECT al.attempt_id, al.position, al.leg_id
		FROM keeperhub_dispatch_attempt_legs al
		JOIN keeperhub_dispatch_attempts a ON a.id = al.attempt_id
		WHERE a.run_id = $1
		ORDER BY al.attempt_id, al.position`, v.Run.ID)
	if err != nil {
		return fmt.Errorf("keeperhubrail: load attempt legs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var attemptID uuid.UUID
		var p AttemptLegPosition
		if err := rows.Scan(&attemptID, &p.Position, &p.LegID); err != nil {
			return err
		}
		i, ok := index[attemptID]
		if !ok {
			continue
		}
		v.Attempts[i].Legs = append(v.Attempts[i].Legs, p)
		v.Attempts[i].LegCount++
	}
	return rows.Err()
}

func (s *Service) loadExclusions(ctx context.Context, v *RunView) error {
	rows, err := s.Pool.Query(ctx, `
		SELECT e.user_id,
		       (SELECT g.login FROM github_accounts g WHERE g.user_id = e.user_id ORDER BY g.created_at LIMIT 1),
		       e.amount_minor::text, e.reason
		FROM keeperhub_payout_exclusions e
		WHERE e.run_id = $1
		ORDER BY e.user_id`, v.Run.ID)
	if err != nil {
		return fmt.Errorf("keeperhubrail: load exclusions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var e ExclusionView
		if err := rows.Scan(&e.UserID, &e.GitHubLogin, &e.AmountMinor, &e.Reason); err != nil {
			return err
		}
		v.Exclusions = append(v.Exclusions, e)
	}
	return rows.Err()
}

// deriveFromLegs counts and sums per status. Nothing is added across statuses.
func deriveFromLegs(legs []LegView) DerivedFromLegs {
	sums := map[string]*big.Int{}
	counts := map[string]int{}
	for _, st := range legStatuses {
		sums[st] = new(big.Int)
	}
	for _, l := range legs {
		if _, known := sums[l.Status]; !known {
			sums[l.Status] = new(big.Int)
		}
		if n, ok := new(big.Int).SetString(l.AmountMinor, 10); ok {
			sums[l.Status].Add(sums[l.Status], n)
		}
		counts[l.Status]++
	}
	out := DerivedFromLegs{Note: derivedNote, LegCount: len(legs), ByStatus: map[string]StatusTotals{}}
	for st, sum := range sums {
		out.ByStatus[st] = StatusTotals{Count: counts[st], AmountMinor: sum.String()}
	}
	return out
}
