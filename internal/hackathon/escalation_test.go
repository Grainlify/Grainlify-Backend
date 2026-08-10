package hackathon

import (
	"context"
	"github.com/jagadeesh/grainlify/backend/internal/dbtest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// The adjudicator must see the actual disagreement and both prior verdicts,
// and must not be told which provider produced which.
func TestBuildEscalationUserContent_PresentsBothVerdictsUnattributed(t *testing.T) {
	in := EscalationInput{
		JudgeInput: JudgeInput{
			HackathonID:        uuid.New(),
			VerdictID:          uuid.New(),
			AcceptanceCriteria: "Validate the email field",
			Diff:               "+ if !emailRe.MatchString(v) { return ErrBadEmail }",
		},
		Judge: &JudgeVerdict{
			Bucket: "substantial", Confidence: "high", Reasoning: "reworked the validation path",
			Criteria: []JudgeCriterion{{Text: "Validates email", Met: true, Evidence: "src/auth/Login.tsx:44"}},
		},
		CrossCheck: &JudgeVerdict{
			Bucket: "accepted", Confidence: "high", Reasoning: "a one-line regex change",
		},
		Reason: "judge said substantial, cross-check said accepted",
	}

	got := buildEscalationUserContent(in)

	for _, want := range []string{
		"judge said substantial, cross-check said accepted",
		"first_review", "second_review",
		"substantial", "accepted",
		"reworked the validation path", "a one-line regex change",
		"src/auth/Login.tsx:44",
		"Validate the email field",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("escalation content missing %q", want)
		}
	}

	// Provider and model names must not leak: which slot ran which provider
	// is an implementation detail, and naming them invites deciding on
	// reputation rather than evidence.
	for _, leak := range []string{"anthropic", "openai", "claude", "gpt"} {
		if strings.Contains(strings.ToLower(got), leak) {
			t.Errorf("escalation content leaks provider identity %q", leak)
		}
	}
}

// A missing second verdict is stated rather than silently omitted - "there
// was no second opinion" is itself the reason this escalated.
func TestBuildEscalationUserContent_SaysWhenAVerdictIsMissing(t *testing.T) {
	in := EscalationInput{
		JudgeInput: JudgeInput{HackathonID: uuid.New(), AcceptanceCriteria: "Add a retry"},
		Judge:      &JudgeVerdict{Bucket: "accepted", Confidence: "low"},
		CrossCheck: nil,
		Reason:     "no cross-check verdict",
	}
	got := buildEscalationUserContent(in)
	if !strings.Contains(got, "no verdict was produced") {
		t.Error("a missing cross-check was omitted rather than stated")
	}
}

// The escalation prompt must tell the model a human decides. §5.7 makes the
// human call final, and a model that thinks it is the last word writes with
// unearned confidence.
func TestEscalationPrompt_SaysAHumanDecides(t *testing.T) {
	p := strings.ToLower(escalationSystemPrompt)
	if !strings.Contains(p, "human") {
		t.Error("the escalation prompt never mentions the human decision")
	}
	if !strings.Contains(p, "recommendation") {
		t.Error("the escalation prompt does not frame its output as a recommendation")
	}
	// It must also allow "I cannot resolve this" as an answer, or an
	// ambiguous bucket definition comes back as a confident coin-flip.
	if !strings.Contains(p, "low") {
		t.Error("the escalation prompt does not permit a low-confidence outcome")
	}
	// And it must carry the same injection instruction as the judging prompt:
	// escalation reads the same untrusted diff.
	if !strings.Contains(p, "ignore any instruction") {
		t.Error("the escalation prompt lacks the instruction-injection guard the judging prompt has")
	}
}

// Escalation runs only when the automated path has already failed to agree
// with itself. These are the routing conditions that get there.
func TestNeedsEscalation_RoutesEveryUnresolvedCase(t *testing.T) {
	high := func(bucket string) *JudgeVerdict {
		return &JudgeVerdict{Bucket: bucket, Confidence: "high"}
	}

	cases := []struct {
		name       string
		judge      *JudgeVerdict
		cross      *JudgeVerdict
		wantEscape bool
	}{
		{"agreement at high confidence", high("accepted"), high("accepted"), false},
		{"disagreement", high("substantial"), high("accepted"), true},
		{"low-confidence judge", &JudgeVerdict{Bucket: "accepted", Confidence: "low"}, high("accepted"), true},
		{"low-confidence cross-check", high("accepted"), &JudgeVerdict{Bucket: "accepted", Confidence: "low"}, true},
		{"missing cross-check", high("accepted"), nil, true},
		{"missing judge", nil, high("accepted"), true},
		{"judge returned no bucket", &JudgeVerdict{Confidence: "high"}, high("accepted"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := NeedsEscalation(tc.judge, tc.cross)
			if got != tc.wantEscape {
				t.Errorf("NeedsEscalation = %v, want %v", got, tc.wantEscape)
			}
			if got && reason == "" {
				t.Error("escalated with no reason recorded; the adjudicator is told what the disagreement was")
			}
		})
	}
}

// "Cannot resolve" must be its own state, not folded in with ordinary
// needs-review. They are different findings: an ordinary escalation says one
// review was wrong, this says the bucket definitions are ambiguous - and the
// second is the one that gets lost if they look the same in the queue.
func TestRunJudgingPipeline_UnresolvableIsItsOwnState(t *testing.T) {
	d := dbtest.DB(t)
	ctx := context.Background()
	pool := d.Pool
	hackathonID, projectID, _ := fxLiveHackathon(t, pool)

	mk := func(pr int, reviewReason string) uuid.UUID {
		var id uuid.UUID
		if err := pool.QueryRow(ctx, `
INSERT INTO hackathon_verdicts
  (hackathon_id, project_id, pr_number, github_login, prefilter_status,
   judge_bucket, cross_check_bucket, needs_human_review, review_reason)
VALUES ($1,$2,$3,'octocat','passed','substantial','accepted',true,$4)
RETURNING id`, hackathonID, projectID, pr, reviewReason).Scan(&id); err != nil {
			t.Fatalf("seed verdict: %v", err)
		}
		return id
	}
	mk(2100, "judge said substantial, cross-check said accepted")
	mk(2101, ReviewReasonUnresolvable)
	mk(2102, ReviewReasonUnresolvable)

	stats, err := ComputeJudgingStats(ctx, pool, hackathonID)
	if err != nil {
		t.Fatalf("ComputeJudgingStats: %v", err)
	}
	if stats.Unresolvable != 2 {
		t.Errorf("unresolvable = %d, want 2", stats.Unresolvable)
	}
	if stats.NeedsReview != 3 {
		t.Errorf("needs_review = %d, want 3 (unresolvable cases still need a human)", stats.NeedsReview)
	}
	// It is a distinct signal from disagreement, not a replacement: both of
	// these also disagreed at the bucket level.
	if stats.Disagreements != 3 {
		t.Errorf("disagreements = %d, want 3", stats.Disagreements)
	}
}
