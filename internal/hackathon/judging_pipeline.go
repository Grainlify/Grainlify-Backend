package hackathon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/ai"
	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// ReviewReasonUnresolvable marks a verdict the escalation stage explicitly
// could not settle.
//
// Kept distinct from ordinary needs-review on purpose. An ordinary escalation
// says one of the two reviews was wrong, and a human picks which. This says
// the evidence supports neither reading over the other - which is a finding
// about the bucket definitions, not about this pull request. It belongs with
// the disagreement-rate signal (§5.6): if these accumulate, the fix is the
// definitions and the calibration set, not more review capacity. Folded in
// with ordinary review the more valuable finding is the one that gets lost,
// because it looks like just another queue item.
const ReviewReasonUnresolvable = "escalation_could_not_resolve"

// RunJudgingPipeline runs stages 3-5 over one verdict and persists the result.
//
// It never writes final_bucket for an escalated case. §5.7 sends those "to a
// human for the final call", so the pipeline's job ends at producing the
// evidence a human decides on.
//
// Auto-confirmation is deliberately narrow: it happens only when both reviews
// agree at non-low confidence. Everything else - disagreement, low
// confidence, a missing second opinion, a schema violation - is a human's
// call, because those are precisely the cases the automated path has already
// shown it cannot settle.
func RunJudgingPipeline(
	ctx context.Context,
	pool db.DBPool,
	anthropic *ai.Client,
	openai *ai.OpenAIClient,
	in JudgeInput,
) error {
	if !AIJudgingEnabled(ctx, pool, in.HackathonID) {
		return nil
	}

	judge, judgeErr := RunJudge(ctx, pool, anthropic, in)
	if judgeErr != nil {
		slog.Warn("judging: stage 3 failed", "verdict_id", in.VerdictID, "error", judgeErr)
	}

	crossCheck, crossErr := RunCrossCheck(ctx, pool, openai, in)
	if crossErr != nil {
		// A failed cross-check is an absent second opinion, not a
		// disagreement, and must not be recorded as one.
		slog.Warn("judging: stage 4 failed", "verdict_id", in.VerdictID, "error", crossErr)
	}

	if err := persistStageVerdicts(ctx, pool, in.VerdictID, judge, crossCheck); err != nil {
		return err
	}

	escalate, reason := NeedsEscalation(judge, crossCheck)
	if !escalate {
		// Both agreed, both confident. This is the only path that settles
		// without a human.
		_, err := pool.Exec(ctx, `
UPDATE hackathon_verdicts
SET final_bucket = $2, final_source = 'auto_confirmed',
    needs_human_review = false, review_reason = NULL, updated_at = now()
WHERE id = $1 AND final_source IS DISTINCT FROM 'human_override'
`, in.VerdictID, judge.Bucket)
		return err
	}

	esc, escErr := RunEscalation(ctx, pool, anthropic, EscalationInput{
		JudgeInput: in, Judge: judge, CrossCheck: crossCheck, Reason: reason,
	})
	if escErr != nil {
		slog.Warn("judging: stage 5 failed", "verdict_id", in.VerdictID, "error", escErr)
	}

	reviewReason := reason
	var escBucket *string
	var escPayload []byte
	if esc != nil {
		if b, err := json.Marshal(esc); err == nil {
			escPayload = b
		}
		if esc.Bucket != "" {
			b := esc.Bucket
			escBucket = &b
		}
		// The adjudicator saying it cannot resolve is a different finding
		// from it picking a side, and is surfaced as its own queue state.
		if esc.Confidence == "low" || esc.Bucket == "" {
			reviewReason = ReviewReasonUnresolvable
		}
	}

	_, err := pool.Exec(ctx, `
UPDATE hackathon_verdicts
SET escalation_bucket = $2,
    escalation_payload = COALESCE($3::jsonb, escalation_payload),
    needs_human_review = true,
    review_reason = $4,
    updated_at = now()
WHERE id = $1 AND final_source IS DISTINCT FROM 'human_override'
`, in.VerdictID, escBucket, escPayload, reviewReason)
	if err != nil {
		return fmt.Errorf("hackathon.RunJudgingPipeline: persist escalation: %w", err)
	}
	return nil
}

func persistStageVerdicts(ctx context.Context, pool db.DBPool, verdictID uuid.UUID, judge, crossCheck *JudgeVerdict) error {
	var (
		judgeBucket, judgeConf, crossBucket *string
		judgePayload, crossPayload          []byte
	)
	if judge != nil {
		if judge.Bucket != "" {
			b := judge.Bucket
			judgeBucket = &b
		}
		if judge.Confidence != "" {
			c := judge.Confidence
			judgeConf = &c
		}
		judgePayload, _ = json.Marshal(judge)
	}
	if crossCheck != nil {
		if crossCheck.Bucket != "" {
			b := crossCheck.Bucket
			crossBucket = &b
		}
		crossPayload, _ = json.Marshal(crossCheck)
	}

	_, err := pool.Exec(ctx, `
UPDATE hackathon_verdicts
SET judge_bucket = $2, judge_confidence = $3,
    judge_payload = COALESCE($4::jsonb, judge_payload),
    cross_check_bucket = $5,
    cross_check_payload = COALESCE($6::jsonb, cross_check_payload),
    updated_at = now()
WHERE id = $1
`, verdictID, judgeBucket, judgeConf, judgePayload, crossBucket, crossPayload)
	if err != nil {
		return fmt.Errorf("hackathon.persistStageVerdicts: %w", err)
	}
	return nil
}
