package grainhack

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/hackathon"
	"github.com/jagadeesh/grainlify/backend/internal/notifications"
	"github.com/jagadeesh/grainlify/backend/internal/settlement"
)

// Notifier is the part of notifications.Service this package uses: in-app
// only, by decision (see notifications.Service.NotifyInApp).
type Notifier interface {
	NotifyInApp(ctx context.Context, userID uuid.UUID, t notifications.Type, title, body string, link notifications.Link) notifications.InAppResult
}

// Service issues statements and records the agent's reports.
type Service struct {
	Pool db.DBPool
	// Key signs statements. Nil means GRAINHACK_RESULTS_SIGNING_KEY is unset:
	// issuing and serving statements answer ErrNotConfigured.
	Key ed25519.PrivateKey
	// Network is GRAINHACK_PAYOUT_NETWORK, already validated by NewService.
	Network string
	Notify  Notifier
	Now     func() time.Time
}

// NewService builds a Service from configuration. A missing or malformed key
// or network is not a boot failure: the service answers ErrNotConfigured, with
// the reason, and everything else keeps working.
func NewService(pool db.DBPool, keyB64, network string, n Notifier) (*Service, error) {
	s := &Service{Pool: pool, Notify: n, Now: time.Now}
	key, err := ParseSigningKey(keyB64)
	if err != nil {
		return s, err
	}
	s.Key = key
	network = strings.TrimSpace(network)
	if network == "" {
		// Devnet unless somebody says otherwise. The signer refuses a
		// statement whose network is not its own, so the wrong default is
		// one that cannot pay real money.
		network = NetworkSolanaDevnet
	}
	if !ValidNetwork(network) {
		s.Key = nil
		return s, fmt.Errorf("GRAINHACK_PAYOUT_NETWORK %q is not %s or %s", network, NetworkSolanaDevnet, NetworkSolanaMainnet)
	}
	s.Network = network
	return s, nil
}

// Errors an admin or the agent can be told about by name.
var (
	ErrNotConfigured        = errors.New("grainhack: results signing is not configured")
	ErrUnsupportedPool      = errors.New("grainhack: only the contributor pool is paid by results statement")
	ErrNoComputation        = errors.New("grainhack: the event has no payout computation")
	ErrPayoutRunNotCurrent  = errors.New("grainhack: that payout run is not the event's current computation")
	ErrPaidOnOtherRail      = errors.New("grainhack: the event pool is paid on another rail")
	ErrComputationChanged   = errors.New("grainhack: the event's computation changed since the last statement")
	ErrNetworkChanged       = errors.New("grainhack: the payout network changed since the last statement")
	ErrSettlementChanged    = errors.New("grainhack: the settlement no longer matches the last statement")
	ErrNothingToSupersede   = errors.New("grainhack: nothing has changed since the last statement")
	ErrConcurrentIssue      = errors.New("grainhack: another statement was issued for this event at the same time")
	ErrNotFound             = errors.New("grainhack: no such statement")
	ErrWinnersWithoutGitHub = errors.New("grainhack: winners without a GitHub account cannot be paid by this path")
)

// Rail exclusion SQLSTATEs (migration 20261003120100).
const (
	SQLStateStatementRefused  = "GH001" // statement refused: KeeperHub run or settlement exists
	SQLStateKeeperHubRefused  = "GH002" // KeeperHub run refused: statement exists
	SQLStateSettlementRefused = "GH003" // settlement refused: statement exists
	SQLStateImmutable         = "GH010"
	SQLStateSumMismatch       = "GH011"
)

// OtherRailError says which rail already pays the event pool.
type OtherRailError struct {
	HackathonID uuid.UUID
	Pool        string
	Rail        string // "keeperhub" or "aptos"
	Ref         string
}

func (e *OtherRailError) Error() string {
	name := "KeeperHub (Base)"
	if e.Rail == "aptos" {
		name = "Aptos"
	}
	return fmt.Sprintf("refused: hackathon %s (%s pool) is already being paid on the %s rail (%s), "+
		"so a GrainHack results statement cannot be issued for it. An event is paid on one rail only. Nothing was written.",
		e.HackathonID, e.Pool, name, e.Ref)
}

func (e *OtherRailError) Unwrap() error { return ErrPaidOnOtherRail }

// WinnerWithoutGitHub names one winner the statement could not include.
type WinnerWithoutGitHub struct {
	UserID uuid.UUID `json:"user_id"`
	// Logins are the GitHub logins their verdicts were recorded under, which
	// is how an admin recognises them; empty if none was recorded.
	Logins      []string `json:"verdict_logins"`
	AmountMinor string   `json:"amount_minor"`
}

// MissingGitHubError refuses a statement and names everyone it would drop.
type MissingGitHubError struct{ Winners []WinnerWithoutGitHub }

func (e *MissingGitHubError) Error() string {
	names := make([]string, 0, len(e.Winners))
	for _, w := range e.Winners {
		label := w.UserID.String()
		if len(w.Logins) > 0 {
			label = strings.Join(w.Logins, "/") + " (" + label + ")"
		}
		names = append(names, label)
	}
	return fmt.Sprintf("refused: %d winner(s) have no linked GitHub account and cannot be paid by results statement: %s. "+
		"Nothing was issued; link or resolve them first.", len(e.Winners), strings.Join(names, ", "))
}

func (e *MissingGitHubError) Unwrap() error { return ErrWinnersWithoutGitHub }

// IssueRequest is the explicit admin action that issues a statement.
type IssueRequest struct {
	HackathonID uuid.UUID
	Pool        string
	ActorID     uuid.UUID
	Confirm     bool
	// PayoutRunID, when set, must be the event's current computation. It lets
	// an admin say "the figures I looked at" and be refused if they moved.
	PayoutRunID *uuid.UUID
}

// Issued is a statement as stored.
type Issued struct {
	StatementID     uuid.UUID    `json:"statement_id"`
	Supersedes      *uuid.UUID   `json:"supersedes"`
	HackathonID     uuid.UUID    `json:"hackathon_id"`
	Pool            string       `json:"pool"`
	ComputationID   uuid.UUID    `json:"computation_id"`
	Currency        string       `json:"currency"`
	Network         string       `json:"network"`
	PoolMinor       string       `json:"pool_minor"`
	Statement       string       `json:"statement"`
	Signature       string       `json:"signature"`
	PublicKey       string       `json:"public_key"`
	StatementSHA256 string       `json:"statement_sha256"`
	IssuedBy        uuid.UUID    `json:"issued_by"`
	IssuedAt        time.Time    `json:"issued_at"`
	Lines           []IssuedLine `json:"lines"`
}

// IssuedLine is one stored line.
type IssuedLine struct {
	GitHubUserID int64     `json:"github_user_id"`
	UserID       uuid.UUID `json:"user_id"`
	Login        string    `json:"login"`
	AmountMinor  string    `json:"amount_minor"`
	Status       string    `json:"status"`
}

func (s *Service) configured() error {
	if s == nil || s.Pool == nil || s.Key == nil || !ValidNetwork(s.Network) {
		return ErrNotConfigured
	}
	return nil
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// CurrentPayoutRun is the event's current computation: the newest
// hackathon_payout_runs row, ties broken by the larger id - the same definition
// the KeeperHub rail checks a release against.
func CurrentPayoutRun(ctx context.Context, q db.DBPool, hackathonID uuid.UUID) (*uuid.UUID, error) {
	var id uuid.UUID
	err := q.QueryRow(ctx, `
		SELECT id FROM hackathon_payout_runs WHERE hackathon_id = $1
		ORDER BY created_at DESC, id DESC LIMIT 1`, hackathonID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("grainhack: current payout run: %w", err)
	}
	return &id, nil
}

type winner struct {
	userID       uuid.UUID
	githubUserID int64
	login        string
	kycVerified  bool
	amount       *big.Int
}

// Issue issues the event pool's statement, or the one that supersedes the
// latest when a held winner's status has changed.
//
// Preconditions, in order: configured; contributor pool; a current
// computation (and the one the admin named, if they named one);
// hackathon.GuardPayoutRelease - explicit confirmation, an actor, not shadow
// mode, phase settled, appeals closed; no other rail paying the event pool;
// every winner has a GitHub account. The amounts are hackathon.SettlementFor's
// pro-rata settlement, the same figure the results page shows.
func (s *Service) Issue(ctx context.Context, req IssueRequest) (*Issued, error) {
	if err := s.configured(); err != nil {
		return nil, err
	}
	if req.Pool == "" {
		req.Pool = PoolContributor
	}
	if req.Pool != PoolContributor {
		return nil, fmt.Errorf("%w (got %q)", ErrUnsupportedPool, req.Pool)
	}

	current, err := CurrentPayoutRun(ctx, s.Pool, req.HackathonID)
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, ErrNoComputation
	}
	if req.PayoutRunID != nil && *req.PayoutRunID != *current {
		return nil, fmt.Errorf("%w: asked for %s, current is %s", ErrPayoutRunNotCurrent, *req.PayoutRunID, *current)
	}

	// The single chokepoint every payout release passes. A statement is what
	// the signer pays from, so issuing one is a release.
	if err := hackathon.GuardPayoutRelease(ctx, s.Pool, hackathon.PayoutReleaseRequest{
		HackathonID: req.HackathonID,
		PayoutRunID: *current,
		ActorID:     req.ActorID,
		Confirm:     req.Confirm,
	}); err != nil {
		return nil, err
	}

	// The Go-side half of the rail exclusion, for a readable refusal before
	// any work; the triggers are the half that holds for every writer.
	if err := s.otherRail(ctx, req.HackathonID, req.Pool); err != nil {
		return nil, err
	}

	res, err := hackathon.SettlementFor(ctx, s.Pool, req.HackathonID, req.Pool, s.Network)
	if err != nil {
		return nil, err
	}
	winners, err := s.loadWinners(ctx, req.HackathonID, res.PayableLines())
	if err != nil {
		return nil, err
	}

	var name string
	if err := s.Pool.QueryRow(ctx, `SELECT name FROM hackathons WHERE id = $1`, req.HackathonID).Scan(&name); err != nil {
		return nil, fmt.Errorf("grainhack: hackathon name: %w", err)
	}

	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("grainhack: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Same lock the rail triggers take: a KeeperHub run, a settlement or a
	// second statement for this event pool waits for this transaction.
	if _, err := tx.Exec(ctx, `SELECT grainhack_rail_lock($1, $2)`, req.HackathonID, req.Pool); err != nil {
		return nil, fmt.Errorf("grainhack: lock: %w", err)
	}

	// The computation is re-read under the lock: an appeal recompute that
	// landed after the figures above were taken would otherwise be signed over
	// as if they were still current.
	var stillCurrent uuid.UUID
	if err := tx.QueryRow(ctx, `
		SELECT id FROM hackathon_payout_runs WHERE hackathon_id = $1
		ORDER BY created_at DESC, id DESC LIMIT 1`, req.HackathonID).Scan(&stillCurrent); err != nil {
		return nil, fmt.Errorf("grainhack: re-read computation: %w", err)
	}
	if stillCurrent != *current {
		return nil, fmt.Errorf("%w: %s replaced %s while issuing", ErrPayoutRunNotCurrent, stillCurrent, *current)
	}

	prev, err := latestInTx(ctx, tx, req.HackathonID, req.Pool)
	if err != nil {
		return nil, err
	}

	lines := make([]Line, 0, len(winners))
	for _, w := range winners {
		status := StatusPayable
		if !w.kycVerified {
			status = StatusHeldKYC
		}
		lines = append(lines, Line{GitHubUserID: w.githubUserID, Login: w.login, AmountMinor: w.amount, Status: status})
	}

	var supersedes *uuid.UUID
	if prev != nil {
		if err := compareWithPrevious(prev, *current, s.Network, res.PoolMinor, lines); err != nil {
			return nil, err
		}
		id := prev.StatementID
		supersedes = &id
	}

	st := Statement{
		StatementID:   uuid.New(),
		Supersedes:    supersedes,
		HackathonID:   req.HackathonID,
		HackathonName: name,
		Pool:          req.Pool,
		ComputationID: *current,
		Currency:      CurrencyUSDC,
		Network:       s.Network,
		PoolMinor:     res.PoolMinor,
		Lines:         lines,
		IssuedAt:      s.now(),
	}
	canonical, err := st.Canonical()
	if err != nil {
		return nil, err
	}
	sig := Sign(s.Key, canonical)
	issuedAt, _ := time.Parse(time.RFC3339, FormatIssuedAt(st.IssuedAt))

	_, err = tx.Exec(ctx, `
		INSERT INTO grainhack_results_statements
		  (id, supersedes, hackathon_id, pool, computation_id, currency, network, pool_minor,
		   canonical_json, signature, signing_public_key, issued_by, issued_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::numeric, $9, $10, $11, $12, $13)`,
		st.StatementID, supersedes, st.HackathonID, st.Pool, st.ComputationID, st.Currency, st.Network,
		st.PoolMinor.String(), canonical, sig, PublicKeyB64(s.Key), req.ActorID, issuedAt)
	if err != nil {
		return nil, s.insertError(err, req.HackathonID, req.Pool)
	}
	for _, w := range winners {
		status := StatusPayable
		if !w.kycVerified {
			status = StatusHeldKYC
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO grainhack_results_statement_lines (statement_id, github_user_id, user_id, login, amount_minor, status)
			VALUES ($1, $2, $3, $4, $5::numeric, $6)`,
			st.StatementID, w.githubUserID, w.userID, w.login, w.amount.String(), status); err != nil {
			return nil, fmt.Errorf("grainhack: insert line: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, s.insertError(err, req.HackathonID, req.Pool)
	}

	issued, err := s.Get(ctx, st.StatementID)
	if err != nil {
		return nil, err
	}
	s.notifyHeld(ctx, issued, name)
	return issued, nil
}

// otherRail refuses when KeeperHub or Aptos already pays the event pool.
func (s *Service) otherRail(ctx context.Context, hid uuid.UUID, pool string) error {
	var ref uuid.UUID
	err := s.Pool.QueryRow(ctx, `SELECT id FROM keeperhub_payout_runs WHERE hackathon_id = $1 AND pool = $2 LIMIT 1`,
		hid, pool).Scan(&ref)
	if err == nil {
		return &OtherRailError{HackathonID: hid, Pool: pool, Rail: "keeperhub", Ref: "keeperhub_payout_runs " + ref.String()}
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("grainhack: keeperhub rail check: %w", err)
	}
	if id, _ := hackathon.ExistingSettlementID(ctx, s.Pool, hid, pool); id != nil {
		return &OtherRailError{HackathonID: hid, Pool: pool, Rail: "aptos", Ref: "settlements " + id.String()}
	}
	return nil
}

// insertError turns the database's refusals into the package's errors.
func (s *Service) insertError(err error, hid uuid.UUID, pool string) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch {
		case pg.Code == SQLStateStatementRefused:
			rail := "keeperhub"
			if strings.Contains(pg.Detail, "settlements") {
				rail = "aptos"
			}
			return &OtherRailError{HackathonID: hid, Pool: pool, Rail: rail, Ref: pg.Detail}
		case pg.Code == "23505": // unique_violation: one root, each superseded once
			return fmt.Errorf("%w: %s", ErrConcurrentIssue, pg.ConstraintName)
		}
	}
	return fmt.Errorf("grainhack: insert statement: %w", err)
}

// loadWinners joins each paid line to the person's GitHub account and KYC
// status, and refuses if anyone has no GitHub account.
func (s *Service) loadWinners(ctx context.Context, hid uuid.UUID, lines []settlement.Line) ([]winner, error) {
	ids := make([]uuid.UUID, 0, len(lines))
	for _, l := range lines {
		ids = append(ids, l.UserID)
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT u.id, g.github_user_id, COALESCE(g.login, ''), COALESCE(usr.kyc_status, '') = 'verified'
		FROM unnest($1::uuid[]) AS u(id)
		LEFT JOIN users usr ON usr.id = u.id
		LEFT JOIN github_accounts g ON g.user_id = u.id`, ids)
	if err != nil {
		return nil, fmt.Errorf("grainhack: join identities: %w", err)
	}
	type ident struct {
		ghID     *int64
		login    string
		verified bool
	}
	people := map[uuid.UUID]ident{}
	for rows.Next() {
		var id uuid.UUID
		var p ident
		if err := rows.Scan(&id, &p.ghID, &p.login, &p.verified); err != nil {
			rows.Close()
			return nil, fmt.Errorf("grainhack: scan identity: %w", err)
		}
		people[id] = p
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var out []winner
	var missing []WinnerWithoutGitHub
	for _, l := range lines {
		p := people[l.UserID]
		if p.ghID == nil || *p.ghID <= 0 || p.login == "" {
			missing = append(missing, WinnerWithoutGitHub{UserID: l.UserID, AmountMinor: l.AmountMinor.String()})
			continue
		}
		out = append(out, winner{userID: l.UserID, githubUserID: *p.ghID, login: p.login,
			kycVerified: p.verified, amount: new(big.Int).Set(l.AmountMinor)})
	}
	if len(missing) > 0 {
		for i := range missing {
			missing[i].Logins = s.verdictLogins(ctx, hid, missing[i].UserID)
		}
		return nil, &MissingGitHubError{Winners: missing}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].githubUserID < out[j].githubUserID })
	return out, nil
}

func (s *Service) verdictLogins(ctx context.Context, hid, uid uuid.UUID) []string {
	rows, err := s.Pool.Query(ctx, `
		SELECT DISTINCT github_login FROM hackathon_verdicts
		WHERE hackathon_id = $1 AND user_id = $2 AND github_login IS NOT NULL AND github_login <> ''
		ORDER BY 1`, hid, uid)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var l string
		if rows.Scan(&l) == nil {
			out = append(out, l)
		}
	}
	return out
}

type prevStatement struct {
	StatementID   uuid.UUID
	ComputationID uuid.UUID
	Network       string
	PoolMinor     *big.Int
	Lines         map[int64]Line
}

// latestInTx reads the head of the event pool's statement chain.
func latestInTx(ctx context.Context, tx pgx.Tx, hid uuid.UUID, pool string) (*prevStatement, error) {
	var p prevStatement
	var poolMinor string
	err := tx.QueryRow(ctx, `
		SELECT s.id, s.computation_id, s.network, s.pool_minor::text
		FROM grainhack_results_statements s
		WHERE s.hackathon_id = $1 AND s.pool = $2
		  AND NOT EXISTS (SELECT 1 FROM grainhack_results_statements n WHERE n.supersedes = s.id)`,
		hid, pool).Scan(&p.StatementID, &p.ComputationID, &p.Network, &poolMinor)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("grainhack: latest statement: %w", err)
	}
	p.PoolMinor, _ = new(big.Int).SetString(poolMinor, 10)
	rows, err := tx.Query(ctx, `
		SELECT github_user_id, login, amount_minor::text, status
		FROM grainhack_results_statement_lines WHERE statement_id = $1`, p.StatementID)
	if err != nil {
		return nil, fmt.Errorf("grainhack: latest lines: %w", err)
	}
	defer rows.Close()
	p.Lines = map[int64]Line{}
	for rows.Next() {
		var l Line
		var amt string
		if err := rows.Scan(&l.GitHubUserID, &l.Login, &amt, &l.Status); err != nil {
			return nil, err
		}
		l.AmountMinor, _ = new(big.Int).SetString(amt, 10)
		p.Lines[l.GitHubUserID] = l
	}
	return &p, rows.Err()
}

// compareWithPrevious allows a superseding statement only for a change of
// status (or of a renamed login) on the same computation, network, pool and
// amounts. Anything else is a different settlement, and an admin decides what
// that means: lines already paid under the old statement stay paid.
func compareWithPrevious(prev *prevStatement, computation uuid.UUID, network string, poolMinor *big.Int, lines []Line) error {
	if prev.ComputationID != computation {
		return fmt.Errorf("%w: statement %s was issued from %s, current is %s",
			ErrComputationChanged, prev.StatementID, prev.ComputationID, computation)
	}
	if prev.Network != network {
		return fmt.Errorf("%w: statement %s is %s, configured is %s", ErrNetworkChanged, prev.StatementID, prev.Network, network)
	}
	if prev.PoolMinor == nil || prev.PoolMinor.Cmp(poolMinor) != 0 || len(prev.Lines) != len(lines) {
		return fmt.Errorf("%w: statement %s", ErrSettlementChanged, prev.StatementID)
	}
	changed := false
	for _, l := range lines {
		old, ok := prev.Lines[l.GitHubUserID]
		if !ok || old.AmountMinor == nil || old.AmountMinor.Cmp(l.AmountMinor) != 0 {
			return fmt.Errorf("%w: statement %s, github_user_id %d", ErrSettlementChanged, prev.StatementID, l.GitHubUserID)
		}
		if old.Status != l.Status || old.Login != l.Login {
			changed = true
		}
	}
	if !changed {
		return fmt.Errorf("%w: statement %s still stands", ErrNothingToSupersede, prev.StatementID)
	}
	return nil
}

// Get reads one stored statement.
func (s *Service) Get(ctx context.Context, statementID uuid.UUID) (*Issued, error) {
	if s == nil || s.Pool == nil {
		return nil, ErrNotConfigured
	}
	var is Issued
	err := s.Pool.QueryRow(ctx, `
		SELECT id, supersedes, hackathon_id, pool, computation_id, currency, network, pool_minor::text,
		       canonical_json, signature, signing_public_key, issued_by, issued_at
		FROM grainhack_results_statements WHERE id = $1`, statementID).Scan(
		&is.StatementID, &is.Supersedes, &is.HackathonID, &is.Pool, &is.ComputationID, &is.Currency, &is.Network,
		&is.PoolMinor, &is.Statement, &is.Signature, &is.PublicKey, &is.IssuedBy, &is.IssuedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("grainhack: read statement: %w", err)
	}
	is.IssuedAt = is.IssuedAt.UTC()
	is.StatementSHA256 = SHA256Hex(is.Statement)
	rows, err := s.Pool.Query(ctx, `
		SELECT github_user_id, user_id, login, amount_minor::text, status
		FROM grainhack_results_statement_lines WHERE statement_id = $1 ORDER BY github_user_id`, statementID)
	if err != nil {
		return nil, fmt.Errorf("grainhack: read lines: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var l IssuedLine
		if err := rows.Scan(&l.GitHubUserID, &l.UserID, &l.Login, &l.AmountMinor, &l.Status); err != nil {
			return nil, err
		}
		is.Lines = append(is.Lines, l)
	}
	return &is, rows.Err()
}

// Latest reads the head of an event pool's statement chain, or ErrNotFound.
func (s *Service) Latest(ctx context.Context, hid uuid.UUID, pool string) (*Issued, error) {
	if s == nil || s.Pool == nil {
		return nil, ErrNotConfigured
	}
	var id uuid.UUID
	err := s.Pool.QueryRow(ctx, `
		SELECT s.id FROM grainhack_results_statements s
		WHERE s.hackathon_id = $1 AND s.pool = $2
		  AND NOT EXISTS (SELECT 1 FROM grainhack_results_statements n WHERE n.supersedes = s.id)`,
		hid, pool).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("grainhack: latest: %w", err)
	}
	return s.Get(ctx, id)
}

// Chain lists an event pool's statements, oldest first.
func (s *Service) Chain(ctx context.Context, hid uuid.UUID, pool string) ([]uuid.UUID, error) {
	rows, err := s.Pool.Query(ctx, `
		WITH RECURSIVE c AS (
		  SELECT id, 1 AS n FROM grainhack_results_statements
		  WHERE hackathon_id = $1 AND pool = $2 AND supersedes IS NULL
		  UNION ALL
		  SELECT s.id, c.n + 1 FROM grainhack_results_statements s JOIN c ON s.supersedes = c.id
		)
		SELECT id FROM c ORDER BY n`, hid, pool)
	if err != nil {
		return nil, fmt.Errorf("grainhack: chain: %w", err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// claimNotice records that a notice is being sent, once per event pool,
// winner and kind. False means it was already sent.
func (s *Service) claimNotice(ctx context.Context, is *Issued, l IssuedLine, kind string) bool {
	tag, err := s.Pool.Exec(ctx, `
		INSERT INTO grainhack_notices (hackathon_id, pool, github_user_id, kind, user_id, statement_id)
		VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING`,
		is.HackathonID, is.Pool, l.GitHubUserID, kind, l.UserID, is.StatementID)
	if err != nil {
		slog.Warn("grainhack: notice claim failed", "kind", kind, "github_user_id", l.GitHubUserID, "error", err)
		return false
	}
	return tag.RowsAffected() == 1
}

// releaseNotice gives a claim back when the notice itself failed, so a later
// statement or report can try again.
func (s *Service) releaseNotice(ctx context.Context, is *Issued, l IssuedLine, kind string) {
	_, _ = s.Pool.Exec(ctx, `
		DELETE FROM grainhack_notices WHERE hackathon_id = $1 AND pool = $2 AND github_user_id = $3 AND kind = $4`,
		is.HackathonID, is.Pool, l.GitHubUserID, kind)
}

// send delivers one claimed notice. Returns whether it was created.
func (s *Service) send(ctx context.Context, is *Issued, l IssuedLine, kind string, t notifications.Type, title, body string, link notifications.Link) bool {
	if s.Notify == nil {
		return false
	}
	if !s.claimNotice(ctx, is, l, kind) {
		return false
	}
	r := s.Notify.NotifyInApp(ctx, l.UserID, t, title, body, link)
	if r.Err != nil {
		s.releaseNotice(ctx, is, l, kind)
		return false
	}
	return r.Created
}

// notifyHeld tells each held winner, once, that their share is held for KYC.
// Best effort: the statement is issued whether or not this lands.
func (s *Service) notifyHeld(ctx context.Context, is *Issued, name string) int {
	n := 0
	for _, l := range is.Lines {
		if l.Status != StatusHeldKYC {
			continue
		}
		title := "Your GrainHack payout is held until you verify your identity"
		body := fmt.Sprintf(
			"Your share of the %s contributor pool is %s. It is held, not cancelled: GrainHack pays verified contributors only. "+
				"Verify your identity under Settings, Billing; once it clears, your payout is released in a new results statement.",
			name, FormatMinor(l.AmountMinor, is.Currency))
		if s.send(ctx, is, l, "held_kyc", notifications.TypeGrainHackPayoutHeldKYC, title, body,
			notifications.SettingsLink(notifications.SubtabBilling)) {
			n++
		}
	}
	return n
}

// FormatMinor renders minor units (6 decimals) the way a person reads money.
func FormatMinor(minor, currency string) string {
	minor = strings.TrimLeft(minor, "0")
	if minor == "" {
		minor = "0"
	}
	whole, frac := "0", ""
	if len(minor) > 6 {
		whole, frac = minor[:len(minor)-6], minor[len(minor)-6:]
	} else {
		frac = strings.Repeat("0", 6-len(minor)) + minor
	}
	frac = strings.TrimRight(frac, "0")
	if frac == "" {
		return whole + " " + currency
	}
	return whole + "." + frac + " " + currency
}
