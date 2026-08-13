package calibration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/jagadeesh/grainlify/backend/internal/hackathon"
)

// Shadow judging for the calibration set.
//
// The model is scored against labels already recorded. Nothing here writes to
// calibration_labels: the human judgement is the fixed point, and a comparison
// that could edit its own reference is not a comparison.
//
// **The prompt is not a copy.** The system prompt and the output schema are
// hackathon.JudgingSystemPrompt and hackathon.JudgeToolSchema - the same values
// the production judging stage uses. The user content is assembled here,
// because production builds it from hackathon tables that do not exist for a
// calibration sample; the report says so rather than implying the whole path
// is shared.

// LongContextThresholdTokens is where this provider's long-context pricing
// begins. Config rather than a literal at the call site, because it is a
// pricing fact that changes, and because a run records the value it used so a
// cost can be recomputed later.
const LongContextThresholdTokens = 128_000

// ModelPricing is per-million-token pricing, as published.
type ModelPricing struct {
	InputPerMTok  float64
	OutputPerMTok float64
}

// JudgeRequest is one pull request presented for judgement.
type JudgeRequest struct {
	SamplePRID string
	Project    string
	Number     int
	Title      string
	Body       string
	IssueTitle string
	IssueBody  string
	HasIssue   bool
	Diff       string
	Additions  int
	Deletions  int
	FileCount  int
	Truncated  bool
}

// JudgeResponse is what a judge returns, plus what the call cost.
type JudgeResponse struct {
	Verdict          string
	Reason           string
	PromptTokens     int
	CompletionTokens int
	DurationMS       int
}

// Judge is the seam. Provider and model are configuration; the runner knows
// only this interface, so adding a provider never touches the run loop.
type Judge interface {
	Provider() string
	Model() string
	Judge(ctx context.Context, req JudgeRequest) (JudgeResponse, error)
}

// reasoningEffortFor is the minimum effort each model will accept alongside a
// forced function call.
//
// The models genuinely disagree, and the API enforces it: gpt-5.6-luna refuses
// function tools unless this is "none", while gpt-5 and gpt-5-mini reject
// "none" and accept only minimal/low/medium/high. There is therefore no single
// value common to all three, and "model is the only variable" cannot be
// literally true for this comparison.
//
// The minimum each supports is the nearest honest equivalent, and the value
// used is recorded on the run so the difference is visible in the data rather
// than hidden in this map.
func reasoningEffortFor(model string) string {
	switch model {
	case "gpt-5.6-luna":
		return "none"
	default:
		return "minimal"
	}
}

// OpenAIJudge scores with one OpenAI model.
type OpenAIJudge struct {
	call   *openAICall
	model  string
	effort string
}

func NewOpenAIJudge(apiKey, model string) *OpenAIJudge {
	return &OpenAIJudge{call: newOpenAICall(apiKey), model: model, effort: reasoningEffortFor(model)}
}

func (j *OpenAIJudge) Provider() string { return "openai" }
func (j *OpenAIJudge) Model() string    { return j.model }

// ReasoningEffort is the value this judge sends. Reported, because it differs
// between models by necessity.
func (j *OpenAIJudge) ReasoningEffort() string { return j.effort }

// calibrationToolSchema is the production judging schema narrowed to what a
// calibration comparison can use.
//
// The human labels are accept/reject, so the model is asked for accept/reject.
// Scoring a four-bucket verdict against a two-value label would require a
// mapping invented here, and an invented mapping is a place for a number to
// come from nowhere. The reason field is kept because the reasons are the
// point: an agreement rate says how often, and only the reason says why.
var calibrationToolSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"verdict": map[string]any{
			"type":        "string",
			"enum":        []string{"accept", "reject"},
			"description": "Whether this pull request should be accepted as a complete, correct contribution.",
		},
		"reason": map[string]any{
			"type":        "string",
			"description": "One or two sentences. What decided it.",
		},
	},
	"required": []string{"verdict", "reason"},
}

func (j *OpenAIJudge) Judge(ctx context.Context, req JudgeRequest) (JudgeResponse, error) {
	var out struct {
		Verdict string `json:"verdict"`
		Reason  string `json:"reason"`
	}

	start := time.Now()
	pTok, cTok, err := j.call.structured(ctx, j.model, j.effort,
		hackathon.JudgingSystemPrompt, BuildJudgeUserContent(req),
		"record_judgement", calibrationToolSchema, &out)
	resp := JudgeResponse{
		DurationMS:       int(time.Since(start).Milliseconds()),
		PromptTokens:     pTok,
		CompletionTokens: cTok,
	}
	if err != nil {
		return resp, err
	}
	resp.Verdict = strings.ToLower(strings.TrimSpace(out.Verdict))
	resp.Reason = strings.TrimSpace(out.Reason)
	if resp.Verdict != "accept" && resp.Verdict != "reject" {
		return resp, fmt.Errorf("model returned verdict %q, which is neither accept nor reject", out.Verdict)
	}
	return resp, nil
}

// BuildJudgeUserContent assembles the pull request for judgement.
//
// Deliberately mirrors what a labeller sees on screen - same fields, same
// order, same "no linked issue" statement in words. A model judged on more or
// less than the human saw would produce a disagreement rate that measures the
// difference in inputs rather than the difference in judgement.
func BuildJudgeUserContent(r JudgeRequest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Repository: %s\nPull request: #%d\nTitle: %s\n\n", r.Project, r.Number, r.Title)

	b.WriteString("Description:\n")
	if strings.TrimSpace(r.Body) == "" {
		b.WriteString("(none)\n")
	} else {
		b.WriteString(r.Body + "\n")
	}

	b.WriteString("\nLinked issue and acceptance criteria:\n")
	if r.HasIssue {
		fmt.Fprintf(&b, "%s\n\n%s\n", r.IssueTitle, r.IssueBody)
	} else {
		b.WriteString("No issue is linked to this pull request, so there are no stated acceptance criteria. Judge it on its own terms.\n")
	}

	fmt.Fprintf(&b, "\nSize: +%d / -%d across %d files\n", r.Additions, r.Deletions, r.FileCount)
	if r.Truncated {
		b.WriteString("NOTE: the diff below is truncated and does not show the whole change.\n")
	}
	b.WriteString("\nDiff:\n")
	b.WriteString(r.Diff)
	return b.String()
}

// PromptFingerprint identifies the exact prompt text a run used.
//
// The version string names it; the hash proves it. Two runs claiming one
// version but disagreeing are then answerable from the log rather than from
// memory.
func PromptFingerprint() (version, sha string) {
	sum := sha256.Sum256([]byte(hackathon.JudgingSystemPrompt))
	full := hex.EncodeToString(sum[:])
	return "hackathon-judging@" + full[:8], full
}
