package calibration

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Snapshotting freezes what a labeller sees.
//
// Everything here is fetched once, at one moment, and stored. Nothing on the
// labelling screen is fetched live, for two reasons that are really the same
// reason: a force-push or a follow-up commit would mean two labellers labelled
// different artefacts under one id, and an edited issue would mean the
// acceptance criteria a label was written against no longer exist.

// maxDiffBytes caps the stored diff.
//
// A quarter of a megabyte is far more than anyone reads carefully, and the cap
// exists so one 7,000-line pull request cannot make the table unwieldy. It is
// recorded when it bites, because a labeller shown a partial diff must know it
// is partial - otherwise they are labelling something other than the change.
const maxDiffBytes = 256 * 1024

// maxSnapshotFiles bounds the file list for the same reason.
const maxSnapshotFiles = 300

// PendingSnapshot is one sample row that has no snapshot yet.
type PendingSnapshot struct {
	SamplePRID      uuid.UUID
	ProjectFullName string
	Number          int
}

// PendingSnapshots lists sample rows still to fetch.
//
// The query is the resumability: re-running after a failure picks up exactly
// what is missing, so a half-finished run is a state the tool can leave and
// return to rather than a mess someone has to unpick.
func PendingSnapshots(ctx context.Context, local *pgxpool.Pool, sampleName string) ([]PendingSnapshot, error) {
	rows, err := local.Query(ctx, `
SELECT sp.id, sp.project_full_name, sp.pr_number
FROM calibration_sample_prs sp
JOIN calibration_samples s ON s.id = sp.sample_id
LEFT JOIN calibration_pr_snapshots snap ON snap.sample_pr_id = sp.id
WHERE s.name = $1 AND snap.sample_pr_id IS NULL
ORDER BY sp.created_at
`, sampleName)
	if err != nil {
		return nil, fmt.Errorf("list pending snapshots: %w", err)
	}
	defer rows.Close()

	var out []PendingSnapshot
	for rows.Next() {
		var p PendingSnapshot
		if err := rows.Scan(&p.SamplePRID, &p.ProjectFullName, &p.Number); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SnapshotResult reports one attempt.
type SnapshotResult struct {
	SamplePRID      uuid.UUID
	ProjectFullName string
	Number          int
	Err             error
	HasIssue        bool
	Truncated       bool
}

// FetchAndStore snapshots one pull request.
//
// The pull request and its linked issue are fetched in the same pass, seconds
// apart, so the diff and the acceptance criteria a labeller reads describe the
// same instant. Fetching the issue later - at label time, or in a second run -
// would mean the criteria could have been edited after the diff was frozen,
// which is the failure this whole table exists to prevent.
func FetchAndStore(ctx context.Context, local *pgxpool.Pool, gh *GitHubClient, p PendingSnapshot) SnapshotResult {
	res := SnapshotResult{SamplePRID: p.SamplePRID, ProjectFullName: p.ProjectFullName, Number: p.Number}

	pr, err := gh.GetPR(ctx, p.ProjectFullName, p.Number)
	if err != nil {
		res.Err = fmt.Errorf("pull request: %w", err)
		return res
	}

	files, filesTruncated, err := gh.ListFiles(ctx, p.ProjectFullName, p.Number, maxSnapshotFiles)
	if err != nil {
		res.Err = fmt.Errorf("files: %w", err)
		return res
	}

	diff, diffTruncated := assembleDiff(files)
	truncated := filesTruncated || diffTruncated
	res.Truncated = truncated

	// The linked issue, fetched now rather than later. A pull request that
	// references none is the ordinary case for about one in nine, and the
	// columns stay NULL so the screen can say "no linked issue" rather than
	// render an empty panel - an empty panel reads as "the criteria are
	// missing", which changes how someone labels.
	var issueNumber *int
	var issueTitle, issueBody *string
	if n, ok := LinkedIssueNumber(pr.Body); ok {
		issue, ierr := gh.GetIssue(ctx, p.ProjectFullName, n)
		if ierr == nil {
			issueNumber, issueTitle, issueBody = &issue.Number, &issue.Title, &issue.Body
			res.HasIssue = true
		}
		// A reference we cannot resolve - a deleted issue, or a number that
		// belongs to another repository - is left absent rather than recorded
		// as an empty issue. "We could not find it" and "there is none" look
		// the same to a labeller, and both are honestly "no criteria shown".
	}

	filesJSON, err := json.Marshal(summariseFiles(files))
	if err != nil {
		res.Err = fmt.Errorf("encode files: %w", err)
		return res
	}

	_, err = local.Exec(ctx, `
INSERT INTO calibration_pr_snapshots
  (sample_pr_id, title, body, author_login, url, additions, deletions, changed_files,
   diff, diff_truncated, files, issue_number, issue_title, issue_body, head_sha)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
`, p.SamplePRID, pr.Title, pr.Body, pr.User.Login, pr.HTMLURL,
		pr.Additions, pr.Deletions, pr.ChangedFiles,
		diff, truncated, filesJSON, issueNumber, issueTitle, issueBody, pr.Head.SHA)
	if err != nil {
		res.Err = fmt.Errorf("store: %w", err)
		return res
	}
	return res
}

type fileSummary struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

func summariseFiles(files []PRFile) []fileSummary {
	out := make([]fileSummary, 0, len(files))
	for _, f := range files {
		out = append(out, fileSummary{f.Filename, f.Status, f.Additions, f.Deletions})
	}
	return out
}

// assembleDiff joins per-file patches into one unified diff.
//
// A file with no patch is listed with a note rather than skipped: GitHub omits
// patches for binary files and very large diffs, and a labeller needs to know
// a file changed even when its contents cannot be shown.
func assembleDiff(files []PRFile) (string, bool) {
	var b strings.Builder
	truncated := false
	for _, f := range files {
		header := fmt.Sprintf("--- %s (%s, +%d -%d)\n", f.Filename, f.Status, f.Additions, f.Deletions)
		if b.Len()+len(header)+len(f.Patch) > maxDiffBytes {
			truncated = true
			b.WriteString(fmt.Sprintf("\n[diff truncated at %d KB; %d files not shown below this point]\n", maxDiffBytes/1024, 1))
			break
		}
		b.WriteString(header)
		if f.Patch == "" {
			b.WriteString("[no patch shown: binary file, or a diff GitHub declined to render]\n")
		} else {
			b.WriteString(f.Patch)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	return b.String(), truncated
}

// SnapshotCoverage reports how complete a sample is.
type SnapshotCoverage struct {
	Total     int
	Snapshot  int
	Missing   []string
	Complete  bool
	WithIssue int
}

// Coverage answers the only question that matters before labelling starts: is
// this sample whole?
//
// A sample that is 20 of 25 snapshotted must never look ready. The screen
// refuses to open on an incomplete set, so a labeller cannot begin on a subset
// and produce a partial set that looks like a whole one.
func Coverage(ctx context.Context, local *pgxpool.Pool, sampleName string) (SnapshotCoverage, error) {
	var c SnapshotCoverage
	rows, err := local.Query(ctx, `
SELECT sp.project_full_name, sp.pr_number, (snap.sample_pr_id IS NOT NULL) AS has_snapshot,
       COALESCE(snap.issue_number IS NOT NULL, false) AS has_issue
FROM calibration_sample_prs sp
JOIN calibration_samples s ON s.id = sp.sample_id
LEFT JOIN calibration_pr_snapshots snap ON snap.sample_pr_id = sp.id
WHERE s.name = $1
ORDER BY sp.created_at
`, sampleName)
	if err != nil {
		return c, err
	}
	defer rows.Close()

	for rows.Next() {
		var project string
		var number int
		var has, hasIssue bool
		if err := rows.Scan(&project, &number, &has, &hasIssue); err != nil {
			return c, err
		}
		c.Total++
		if has {
			c.Snapshot++
			if hasIssue {
				c.WithIssue++
			}
		} else {
			c.Missing = append(c.Missing, fmt.Sprintf("%s#%d", project, number))
		}
	}
	c.Complete = c.Total > 0 && c.Snapshot == c.Total
	return c, rows.Err()
}
