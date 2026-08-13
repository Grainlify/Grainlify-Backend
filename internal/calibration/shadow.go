package calibration

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// Running a model over the labelled set, and scoring it.

// LabelledPR is one pull request with the human label already on it.
type LabelledPR struct {
	SamplePRID uuid.UUID
	Project    string
	Number     int
	SizeBand   SizeBand
	Merged     bool
	Verdict    string
	Confidence string
	Reason     string
	Request    JudgeRequest
}

// Scope selects which rows a run scores. The two framings must never be
// pooled: the first 20 were labelled as "should this contribution have been
// accepted" (which folds in coordination), the released rows as "given
// coordination passed, is the work acceptable". Averaging them would produce a
// number that answers neither question.
type Scope string

const (
	// ScopeMain is the originally labelled rows - never held back.
	ScopeMain Scope = "main"
	// ScopeHeldBack is the released hold-back, labelled under the judge's own
	// question.
	ScopeHeldBack Scope = "held-back"
)

func (sc Scope) clause() string {
	if sc == ScopeHeldBack {
		return "AND sp.held_back AND sp.released_at IS NOT NULL"
	}
	return "AND NOT sp.held_back"
}

// LabelledPRs returns the labelled rows for one scope, in a fixed order.
//
// Ordered by created_at so every model sees the same pull requests in the same
// order. Model is meant to be the only variable between runs; presentation
// order is one of the things that must therefore not vary.
//
// Held-back rows are excluded here as everywhere: they are unreleased, and a
// comparison that quietly included them would spend the one honest check the
// hold-back exists to provide.
func LabelledPRs(ctx context.Context, local db.DBPool, sampleName string, scope Scope) ([]LabelledPR, error) {
	rows, err := local.Query(ctx, `
SELECT sp.id, sp.project_full_name, sp.pr_number, sp.size_band, sp.merged,
       l.verdict, l.confidence, l.reason,
       snap.title, COALESCE(snap.body,''), snap.additions, snap.deletions, snap.changed_files,
       snap.diff, snap.diff_truncated,
       snap.issue_number, COALESCE(snap.issue_title,''), COALESCE(snap.issue_body,'')
FROM calibration_sample_prs sp
JOIN calibration_samples s ON s.id = sp.sample_id
JOIN calibration_pr_snapshots snap ON snap.sample_pr_id = sp.id
JOIN LATERAL (
  SELECT verdict, confidence, reason
  FROM calibration_labels
  WHERE sample_pr_id = sp.id
  ORDER BY created_at DESC LIMIT 1
) l ON true
WHERE s.name = $1 `+scope.clause()+`
ORDER BY sp.created_at
`, sampleName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LabelledPR
	for rows.Next() {
		var p LabelledPR
		var band string
		var issueNumber *int
		var issueTitle, issueBody string
		if err := rows.Scan(&p.SamplePRID, &p.Project, &p.Number, &band, &p.Merged,
			&p.Verdict, &p.Confidence, &p.Reason,
			&p.Request.Title, &p.Request.Body, &p.Request.Additions, &p.Request.Deletions, &p.Request.FileCount,
			&p.Request.Diff, &p.Request.Truncated,
			&issueNumber, &issueTitle, &issueBody); err != nil {
			return nil, err
		}
		p.SizeBand = SizeBand(band)
		p.Request.SamplePRID = p.SamplePRID.String()
		p.Request.Project, p.Request.Number = p.Project, p.Number
		if issueNumber != nil {
			p.Request.HasIssue = true
			p.Request.IssueTitle, p.Request.IssueBody = issueTitle, issueBody
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RunShadow scores one model over the labelled set and records every call.
//
// A failed call is recorded with its error rather than dropped, so a run of 18
// successes out of 20 can never be reported as 18 of 18.
func RunShadow(ctx context.Context, local db.DBPool, sampleID uuid.UUID, sampleName string, scope Scope,
	j Judge, pricing ModelPricing, prs []LabelledPR, progress func(i int, p LabelledPR, r JudgeResponse, err error),
) (uuid.UUID, error) {
	version, sha := PromptFingerprint()

	var runID uuid.UUID
	err := local.QueryRow(ctx, `
INSERT INTO calibration_model_runs
  (sample_id, provider, model, prompt_version, prompt_sha256, long_context_threshold,
   input_price_per_mtok, output_price_per_mtok, reasoning_effort, scope, notes)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING id
`, sampleID, j.Provider(), j.Model(), version, sha, LongContextThresholdTokens,
		pricing.InputPerMTok, pricing.OutputPerMTok, effortOf(j), string(scope),
		"Single-labeller benchmark. Shadow run: the model is scored against one person's labels, and this is not inter-rater agreement. System prompt and output contract are the production judging ones; the user content is assembled by the calibration harness from frozen snapshots.",
	).Scan(&runID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("open run: %w", err)
	}

	for i, p := range prs {
		resp, jerr := j.Judge(ctx, p.Request)
		if progress != nil {
			progress(i, p, resp, jerr)
		}

		var verdict, reason, errText *string
		if jerr != nil {
			s := jerr.Error()
			errText = &s
		} else {
			v, r := resp.Verdict, resp.Reason
			verdict, reason = &v, &r
		}
		if _, err := local.Exec(ctx, `
INSERT INTO calibration_model_verdicts
  (run_id, sample_pr_id, verdict, reason, error, prompt_tokens, completion_tokens, long_context, duration_ms)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
`, runID, p.SamplePRID, verdict, reason, errText,
			resp.PromptTokens, resp.CompletionTokens,
			resp.PromptTokens > LongContextThresholdTokens, resp.DurationMS); err != nil {
			return runID, fmt.Errorf("record verdict for %s#%d: %w", p.Project, p.Number, err)
		}
	}

	if _, err := local.Exec(ctx, `UPDATE calibration_model_runs SET finished_at = now() WHERE id = $1`, runID); err != nil {
		return runID, err
	}
	return runID, nil
}

// effortOf reports a judge's reasoning effort when it has one, so the run
// record can carry it. Not on the Judge interface: a provider without the
// concept should not have to pretend to have it.
func effortOf(j Judge) *string {
	type hasEffort interface{ ReasoningEffort() string }
	if e, ok := j.(hasEffort); ok {
		v := e.ReasoningEffort()
		return &v
	}
	return nil
}

// Disagreement is one pull request where model and human differ.
type Disagreement struct {
	Project     string
	Number      int
	SizeBand    SizeBand
	Merged      bool
	Human       string
	HumanReason string
	HumanConf   string
	Model       string
	ModelReason string
}

// RunScore is one model's result.
type RunScore struct {
	RunID            uuid.UUID
	Provider         string
	Model            string
	PromptVersion    string
	PromptSHA        string
	StartedAt        time.Time
	ReasoningEffort  string
	Scope            string
	Total            int
	Scored           int
	Failed           int
	Agree            int
	ByBand           map[SizeBand][2]int // [agree, total]
	ByMerged         map[bool][2]int
	ByConfidence     map[string][2]int
	PromptTokens     int
	CompletionTokens int
	LongContextCalls int
	MaxPromptTokens  int
	CostUSD          float64
	Disagreements    []Disagreement

	// The always-accept baseline for THIS run, computed from the same as-of
	// labels the agreement is, so the two are the same vintage.
	//
	// Computed here rather than passed in. When it was passed in, it came from
	// today's labels while agreement came from run-time labels, so a corrected
	// label moved "vs base" on a historical run while leaving the agreement
	// alone - a report whose two halves disagreed about which day it was.
	BaselinePct     float64
	BaselineAccepts int
	BaselineTotal   int
}

// AgreementPct is agreement over the calls that produced a verdict.
//
// Failed calls are excluded from the denominator and reported separately -
// counting them as disagreements would blame the model for an outage, and
// counting them as agreements would flatter it.
func (r RunScore) AgreementPct() float64 {
	if r.Scored == 0 {
		return 0
	}
	return 100 * float64(r.Agree) / float64(r.Scored)
}

// ScoreRun reads one recorded run back and scores it.
func ScoreRun(ctx context.Context, local db.DBPool, runID uuid.UUID) (RunScore, error) {
	s := RunScore{
		RunID:        runID,
		ByBand:       map[SizeBand][2]int{},
		ByMerged:     map[bool][2]int{},
		ByConfidence: map[string][2]int{},
	}
	var inPrice, outPrice float64
	err := local.QueryRow(ctx, `
SELECT provider, model, prompt_version, prompt_sha256, started_at, input_price_per_mtok, output_price_per_mtok,
       COALESCE(reasoning_effort, ''), COALESCE(scope, '')
FROM calibration_model_runs WHERE id = $1
`, runID).Scan(&s.Provider, &s.Model, &s.PromptVersion, &s.PromptSHA, &s.StartedAt, &inPrice, &outPrice, &s.ReasoningEffort, &s.Scope)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, fmt.Errorf("no such run %s", runID)
	}
	if err != nil {
		return s, err
	}

	// **Scored against the label as it stood WHEN THE RUN HAPPENED**, not the
	// latest one.
	//
	// Labels are append-only and a labeller may correct themselves later - and
	// one did: a superseding label was added after a model disagreed and turned
	// out to be right. Reading the latest label would silently move a recorded
	// run's score every time that happens, so a number quoted last week would
	// not be the number the same command printed today.
	//
	// Constraining to created_at <= the run's start makes a historical run
	// reproducible by construction. Future runs pick up the correction, which
	// is what a correction is for.
	rows, err := local.Query(ctx, `
SELECT sp.project_full_name, sp.pr_number, sp.size_band, sp.merged,
       l.verdict, l.confidence, l.reason,
       mv.verdict, mv.reason, mv.error,
       COALESCE(mv.prompt_tokens,0), COALESCE(mv.completion_tokens,0), mv.long_context
FROM calibration_model_verdicts mv
JOIN calibration_sample_prs sp ON sp.id = mv.sample_pr_id
JOIN calibration_model_runs r ON r.id = mv.run_id
JOIN LATERAL (
  SELECT verdict, confidence, reason FROM calibration_labels
  WHERE sample_pr_id = sp.id AND created_at <= r.started_at
  ORDER BY created_at DESC LIMIT 1
) l ON true
WHERE mv.run_id = $1
ORDER BY sp.created_at
`, runID)
	if err != nil {
		return s, err
	}
	defer rows.Close()

	for rows.Next() {
		var project, band, humanVerdict, humanConf, humanReason string
		var number int
		var merged, longCtx bool
		var modelVerdict, modelReason, errText *string
		var pTok, cTok int
		if err := rows.Scan(&project, &number, &band, &merged,
			&humanVerdict, &humanConf, &humanReason,
			&modelVerdict, &modelReason, &errText, &pTok, &cTok, &longCtx); err != nil {
			return s, err
		}

		s.Total++
		s.PromptTokens += pTok
		s.CompletionTokens += cTok
		if pTok > s.MaxPromptTokens {
			s.MaxPromptTokens = pTok
		}
		if longCtx {
			s.LongContextCalls++
		}
		if modelVerdict == nil {
			s.Failed++
			continue
		}
		s.Scored++
		s.BaselineTotal++
		if humanVerdict == "accept" {
			s.BaselineAccepts++
		}

		agree := *modelVerdict == humanVerdict
		bump := func(m map[SizeBand][2]int, k SizeBand) {
			v := m[k]
			if agree {
				v[0]++
			}
			v[1]++
			m[k] = v
		}
		bump(s.ByBand, SizeBand(band))
		mv := s.ByMerged[merged]
		cv := s.ByConfidence[humanConf]
		if agree {
			s.Agree++
			mv[0]++
			cv[0]++
		}
		mv[1]++
		cv[1]++
		s.ByMerged[merged] = mv
		s.ByConfidence[humanConf] = cv

		if !agree {
			d := Disagreement{
				Project: project, Number: number, SizeBand: SizeBand(band), Merged: merged,
				Human: humanVerdict, HumanReason: humanReason, HumanConf: humanConf,
				Model: *modelVerdict,
			}
			if modelReason != nil {
				d.ModelReason = *modelReason
			}
			s.Disagreements = append(s.Disagreements, d)
		}
	}
	if err := rows.Err(); err != nil {
		return s, err
	}

	if s.BaselineTotal > 0 {
		s.BaselinePct = 100 * float64(s.BaselineAccepts) / float64(s.BaselineTotal)
	}
	s.CostUSD = float64(s.PromptTokens)/1_000_000*inPrice + float64(s.CompletionTokens)/1_000_000*outPrice
	sort.Slice(s.Disagreements, func(i, j int) bool {
		return s.Disagreements[i].Project < s.Disagreements[j].Project
	})
	return s, nil
}

// AlwaysAcceptBaseline is what a model scores by answering "accept" every
// time, computed over EXACTLY the rows being scored.
//
// Per scope, never carried across. The held-back run reported "+40 vs base"
// against a 60% baseline computed on the main 20; on those five the baseline
// happened to be 60% too, so the arithmetic was right by coincidence. A
// coincidence is not a method, and the next set will not be so obliging.
//
// Returned with its denominator so a report can say what it was computed on.
// A baseline whose sample size is invisible invites the same mistake again.
func AlwaysAcceptBaseline(prs []LabelledPR) (pct float64, accepts, total int) {
	total = len(prs)
	if total == 0 {
		return 0, 0, 0
	}
	for _, p := range prs {
		if p.Verdict == "accept" {
			accepts++
		}
	}
	return 100 * float64(accepts) / float64(total), accepts, total
}
