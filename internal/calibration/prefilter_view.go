package calibration

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
	"github.com/jagadeesh/grainlify/backend/internal/hackathon"
)

// What production's deterministic prefilter would say about a calibration pull
// request, using only facts the frozen snapshot actually contains.
//
// The point is to separate two things the headline agreement rate confuses: a
// rejection that plain code already handles, and a rejection that required
// judgement. If most of a labeller's rejections are the former, the set barely
// exercises the judge at all - and that is worth knowing before spending the
// held-back rows on it.
//
// **Nothing here guesses.** A rule whose inputs the snapshot does not contain
// is reported as unknowable, not assumed satisfied and not assumed violated.
// Assuming either would manufacture the answer this analysis exists to find.

// PrefilterKnowability lists which §5.1 rules a snapshot can and cannot decide.
var (
	// KnowableRules can be evaluated from what was frozen at draw time.
	KnowableRules = []string{
		"not linked to an issue",
		"documentation only",
		"no meaningful code changes",
	}
	// UnknowableRules cannot, for these pull requests, at any confidence.
	UnknowableRules = map[string]string{
		"author was not the assigned contributor": "these pull requests never went through GrainHack, so no assignment record exists or could exist",
		"author opened the issue":                 "the issue's author was not snapshotted",
		"author is a repository admin":            "repository roles were not snapshotted",
		"still a draft":                           "draft state was not snapshotted",
		"CI was failing":                          "CI status was not snapshotted",
	}
)

// PrefilterView is one pull request's deterministic verdict, as far as it can
// be determined.
type PrefilterView struct {
	SamplePRID   uuid.UUID
	Project      string
	Number       int
	HumanVerdict string

	// WouldReject is true when a rule that IS knowable fires.
	WouldReject bool
	Rule        string

	// Stats as far as the snapshot allows. MeaningfulLines is an UPPER bound:
	// per-file patches were not stored separately, so whitespace-only lines and
	// banner-marked generated lines are not subtracted. That makes the
	// "no meaningful code" rule conservative - it under-rejects rather than
	// over-rejects, which is the safe direction for this question.
	Stats    hackathon.DiffStats
	HasIssue bool
}

// PrefilterViews computes the deterministic verdict for every labelled row.
func PrefilterViews(ctx context.Context, local db.DBPool, sampleName string, includeHeldBack bool) ([]PrefilterView, error) {
	heldClause := "AND NOT sp.held_back"
	if includeHeldBack {
		heldClause = ""
	}
	rows, err := local.Query(ctx, `
SELECT sp.id, sp.project_full_name, sp.pr_number,
       COALESCE(l.verdict, ''), snap.files, (snap.issue_number IS NOT NULL)
FROM calibration_sample_prs sp
JOIN calibration_samples s ON s.id = sp.sample_id
JOIN calibration_pr_snapshots snap ON snap.sample_pr_id = sp.id
LEFT JOIN LATERAL (
  SELECT verdict FROM calibration_labels
  WHERE sample_pr_id = sp.id ORDER BY created_at DESC LIMIT 1
) l ON true
WHERE s.name = $1 `+heldClause+`
ORDER BY sp.created_at
`, sampleName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PrefilterView
	for rows.Next() {
		var v PrefilterView
		var filesJSON []byte
		if err := rows.Scan(&v.SamplePRID, &v.Project, &v.Number, &v.HumanVerdict, &filesJSON, &v.HasIssue); err != nil {
			return nil, err
		}
		var files []struct {
			Filename  string `json:"filename"`
			Additions int    `json:"additions"`
			Deletions int    `json:"deletions"`
		}
		if err := json.Unmarshal(filesJSON, &files); err != nil {
			return nil, err
		}
		changes := make([]hackathon.FileChange, 0, len(files))
		for _, f := range files {
			changes = append(changes, hackathon.FileChange{
				Filename: f.Filename, Additions: f.Additions, Deletions: f.Deletions,
			})
		}
		// Production's own function, not a reimplementation of it.
		v.Stats = hackathon.ComputeDiffStats(changes)

		switch {
		case !v.HasIssue:
			v.WouldReject, v.Rule = true, "not linked to an issue"
		case v.Stats.DocsOnly:
			// The exemption (the issue was itself a docs issue) is not
			// decidable from the snapshot, so this is reported as a rejection
			// with that caveat stated once in the report rather than silently
			// applied either way.
			v.WouldReject, v.Rule = true, "documentation only"
		case v.Stats.MeaningfulLines == 0:
			v.WouldReject, v.Rule = true, "no meaningful code changes"
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// PrefilterSummary is the number the whole exercise is for.
type PrefilterSummary struct {
	Total int
	// HumanRejected is how many the labeller rejected.
	HumanRejected int
	// CoordinationRejections are human rejections that a knowable prefilter
	// rule also rejects - the ones plain code already handles.
	CoordinationRejections int
	// JudgementRejections are human rejections no knowable rule explains, so
	// they required judgement about the work itself.
	JudgementRejections int
	// PrefilterRejectsHumanAccepted is the disagreement in the other
	// direction, and worth seeing: code would have rejected something the
	// human accepted.
	PrefilterRejectsHumanAccepted int
	ByRule                        map[string]int
}

func SummarisePrefilter(views []PrefilterView) PrefilterSummary {
	s := PrefilterSummary{ByRule: map[string]int{}}
	for _, v := range views {
		s.Total++
		if v.WouldReject {
			s.ByRule[v.Rule]++
		}
		switch {
		case v.HumanVerdict == "reject" && v.WouldReject:
			s.HumanRejected++
			s.CoordinationRejections++
		case v.HumanVerdict == "reject":
			s.HumanRejected++
			s.JudgementRejections++
		case v.HumanVerdict == "accept" && v.WouldReject:
			s.PrefilterRejectsHumanAccepted++
		}
	}
	return s
}
