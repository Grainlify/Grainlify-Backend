package erasure

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DryRunReport is what one Retention.RunOnce would do, computed without doing
// any of it. It names no person: dates, counts, reason codes, row ids and the
// Didit session ids the pass would ask Didit to delete - never names, document
// data or previous_kyc_data.
type DryRunReport struct {
	GeneratedAt time.Time `json:"generated_at"`
	// DiditConfigured is whether this process has a Didit API key. Without
	// one the pass keeps every record whose session it would have to delete.
	DiditConfigured bool             `json:"didit_configured"`
	Categories      []DryRunCategory `json:"categories"`
}

// DryRunCategory is one retention period: the rule, the cutoff, and what the
// pass would do under it.
type DryRunCategory struct {
	Name   string    `json:"name"`
	Rule   string    `json:"rule"`
	Cutoff time.Time `json:"cutoff"`
	// NotApplicable is set when this database does not have what the
	// category needs yet (a table or column a later release adds); the pass
	// would fail this category until then, and nothing in it is reported.
	NotApplicable string        `json:"not_applicable,omitempty"`
	Tables        []DryRunTable `json:"tables"`
	// DueTotal and BatchLimit: a category that takes a batch per pass has
	// DueTotal rows past the cutoff, of which one pass handles BatchLimit.
	DueTotal   *int `json:"due_total,omitempty"`
	BatchLimit int  `json:"batch_limit,omitempty"`
	// Resets lists each reset record the pass would take on.
	Resets []DryRunReset `json:"resets,omitempty"`
}

// DryRunTable is what the pass would do to one table: delete or update Rows
// rows whose relevant dates run from Oldest to Newest.
type DryRunTable struct {
	Table  string     `json:"table"`
	Action string     `json:"action"`
	Rows   int        `json:"rows"`
	Oldest *time.Time `json:"oldest,omitempty"`
	Newest *time.Time `json:"newest,omitempty"`
	// ids are the rows selected; for the tests, not the report.
	ids []string
}

// DryRunReset is one reset record due for erasure.
type DryRunReset struct {
	AuditID    string    `json:"audit_id"`
	ResetAt    time.Time `json:"reset_at"`
	ReasonCode string    `json:"reason_code"`
	// DiditSession is the session the pass would ask Didit to delete; empty
	// when it would ask nothing.
	DiditSession string `json:"didit_session_id,omitempty"`
	Outcome      string `json:"outcome"`
}

// Outcomes of a reset record in the pass.
const (
	ResetOutcomeDidit         = "ask Didit to delete the session, then delete the record (kept for the next pass if Didit fails)"
	ResetOutcomeNoSession     = "delete the record (it names no Didit session)"
	ResetOutcomeSessionInUse  = "delete the record; its session is still in use, so Didit is not asked"
	ResetOutcomeNotConfigured = "keep: Didit is not configured here, so its session cannot be deleted"
)

// diditConfigurer is implemented by an External that can say whether Didit
// is configured without calling it (*Services).
type diditConfigurer interface{ DiditConfigured() bool }

// DiditConfigured reports whether a Didit client is configured. It calls
// nothing.
func (s *Services) DiditConfigured() bool { return s.Didit != nil }

// DryRun computes exactly what one RunOnce would do now, and does none of it.
//
// It reads inside a READ ONLY transaction, so the database refuses any write
// whatever the code does, and rolls it back. It never calls Didit or GitHub:
// r.ext is consulted only for whether Didit is configured. It selects through
// the same functions the pass does (selectResetRecords, planPayoutRecords),
// so the two cannot select differently.
//
// A category whose table or column this database does not have yet is
// reported as not applicable rather than failing the whole report.
func (r *Retention) DryRun(ctx context.Context) (*DryRunReport, error) {
	tx, err := beginReadOnly(ctx, r.pool)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // always rolled back; nothing to keep
	return r.dryRunIn(ctx, tx)
}

// beginReadOnly opens the dry run's transaction: BEGIN ISOLATION LEVEL
// REPEATABLE READ READ ONLY, so the whole report is one snapshot and the
// database itself refuses any write in it.
func beginReadOnly(ctx context.Context, pool interface {
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
}) (pgx.Tx, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("retention dry run: begin: %w", err)
	}
	return tx, nil
}

func (r *Retention) dryRunIn(ctx context.Context, tx pgx.Tx) (*DryRunReport, error) {
	now := r.now()
	rep := &DryRunReport{GeneratedAt: now.UTC(), DiditConfigured: true}
	if c, ok := r.ext.(diditConfigurer); ok {
		rep.DiditConfigured = c.DiditConfigured()
	}

	resets := DryRunCategory{
		Name:       "verification_reset_records",
		Rule:       fmt.Sprintf("identity-verification reset records older than %d days, for every account", int(ResetRecordRetention.Hours()/24)),
		Cutoff:     r.resetCutoff(now).UTC(),
		BatchLimit: resetBatch,
	}
	if err := category(ctx, tx, &resets, func(q pgx.Tx) error {
		return dryRunResets(ctx, q, &resets, rep.DiditConfigured)
	}); err != nil {
		return nil, err
	}

	payouts := DryRunCategory{
		Name:   "erased_account_payout_records",
		Rule:   fmt.Sprintf("payout records of erased accounts, %d years after the payment", PayoutRecordYears),
		Cutoff: r.payoutCutoff(now).UTC(),
	}
	if err := category(ctx, tx, &payouts, func(q pgx.Tx) error {
		plan, err := planPayoutRecords(ctx, q, payouts.Cutoff)
		if err != nil {
			return err
		}
		for _, p := range plan {
			t := DryRunTable{Table: p.step.name, Action: p.step.verb(), Rows: len(p.ids), Oldest: utc(p.oldest), Newest: utc(p.newest)}
			for _, id := range p.ids {
				t.ids = append(t.ids, id.String())
			}
			payouts.Tables = append(payouts.Tables, t)
		}
		return nil
	}); err != nil {
		return nil, err
	}

	rep.Categories = []DryRunCategory{resets, payouts}
	return rep, nil
}

// category runs one category's selections in a savepoint, so a category this
// database cannot select yet does not abort the transaction for the others.
func category(ctx context.Context, tx pgx.Tx, c *DryRunCategory, f func(pgx.Tx) error) error {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return fmt.Errorf("retention dry run: savepoint: %w", err)
	}
	err = f(sp)
	if err == nil {
		return sp.Commit(ctx) // RELEASE SAVEPOINT; writes nothing
	}
	_ = sp.Rollback(ctx)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "42P01" || pgErr.Code == "42703") { // undefined table, undefined column
		c.NotApplicable = "not applicable before this release: " + pgErr.Message
		c.Tables, c.Resets, c.DueTotal = nil, nil, nil
		return nil
	}
	return fmt.Errorf("retention dry run: %s: %w", c.Name, err)
}

func dryRunResets(ctx context.Context, q pgx.Tx, c *DryRunCategory, diditConfigured bool) error {
	var due int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM kyc_reset_audit a WHERE a.created_at < $1`, c.Cutoff).Scan(&due); err != nil {
		return fmt.Errorf("retention: count reset records: %w", err)
	}
	c.DueTotal = &due
	list, err := selectResetRecords(ctx, q, c.Cutoff)
	if err != nil {
		return err
	}
	t := DryRunTable{Table: "kyc_reset_audit", Action: "delete"}
	for _, d := range list {
		rr := DryRunReset{AuditID: d.id.String(), ResetAt: d.at.UTC(), ReasonCode: d.reasonCode}
		switch {
		case d.needsDidit() && !diditConfigured:
			rr.Outcome = ResetOutcomeNotConfigured
		case d.needsDidit():
			rr.DiditSession = d.session
			rr.Outcome = ResetOutcomeDidit
		case d.session == "":
			rr.Outcome = ResetOutcomeNoSession
		default:
			rr.Outcome = ResetOutcomeSessionInUse
		}
		c.Resets = append(c.Resets, rr)
		if rr.Outcome == ResetOutcomeNotConfigured {
			continue
		}
		at := d.at
		t.Rows++
		t.ids = append(t.ids, rr.AuditID)
		if t.Oldest == nil || at.Before(*t.Oldest) {
			t.Oldest = utc(&at)
		}
		if t.Newest == nil || at.After(*t.Newest) {
			t.Newest = utc(&at)
		}
	}
	c.Tables = []DryRunTable{t}
	return nil
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// WriteText writes the report for a person to read.
func (rep *DryRunReport) WriteText(w io.Writer) {
	date := func(t *time.Time) string {
		if t == nil {
			return "-"
		}
		return t.Format("2006-01-02")
	}
	yes := map[bool]string{true: "yes", false: "no"}
	fmt.Fprintf(w, "Retention dry run, %s\n", rep.GeneratedAt.Format(time.RFC3339))
	fmt.Fprintln(w, "What one pass of the retention job would do now. Read-only transaction: nothing was written; Didit and GitHub were not called.")
	fmt.Fprintf(w, "Didit configured in this environment: %s\n", yes[rep.DiditConfigured])
	for i, c := range rep.Categories {
		fmt.Fprintf(w, "\n[%d] %s\n    rule:   %s\n    cutoff: before %s\n", i+1, c.Name, c.Rule, c.Cutoff.Format(time.RFC3339))
		if c.NotApplicable != "" {
			fmt.Fprintf(w, "    %s\n", c.NotApplicable)
			continue
		}
		if c.DueTotal != nil {
			fmt.Fprintf(w, "    past the cutoff: %d (one pass takes at most %d)\n", *c.DueTotal, c.BatchLimit)
		}
		var total int
		fmt.Fprintf(w, "    %-30s %-7s %6s  %-10s  %-10s\n", "table", "action", "rows", "oldest", "newest")
		for _, t := range c.Tables {
			total += t.Rows
			fmt.Fprintf(w, "    %-30s %-7s %6d  %-10s  %-10s\n", t.Table, t.Action, t.Rows, date(t.Oldest), date(t.Newest))
		}
		fmt.Fprintf(w, "    %-30s %-7s %6d\n", "total", "", total)
		if len(c.Resets) == 0 {
			continue
		}
		var didit, other []DryRunReset
		for _, r := range c.Resets {
			if r.DiditSession != "" {
				didit = append(didit, r)
			} else {
				other = append(other, r)
			}
		}
		fmt.Fprintf(w, "    Didit sessions it would ask Didit to delete (%d):\n", len(didit))
		for _, r := range didit {
			fmt.Fprintf(w, "      reset %s  reason %-24s session %s\n", r.ResetAt.Format("2006-01-02"), orDash(r.ReasonCode), r.DiditSession)
		}
		if len(other) > 0 {
			fmt.Fprintf(w, "    Other reset records (%d):\n", len(other))
			for _, r := range other {
				fmt.Fprintf(w, "      reset %s  reason %-24s %s\n", r.ResetAt.Format("2006-01-02"), orDash(r.ReasonCode), r.Outcome)
			}
		}
	}
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
