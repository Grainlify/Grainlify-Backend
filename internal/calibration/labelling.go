package calibration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// The labelling read/write layer.
//
// Two rules govern every query in this file, and both are enforced here rather
// than in the screen, because a rule enforced in a template is a rule one
// refactor away from being gone:
//
//  1. **No model output reaches a labeller.** Not a score, not a verdict, not
//     a suggestion, in any field, at any point. A label written by someone who
//     saw the model's opinion measures agreement with the model, not
//     judgement, and the whole set is void.
//
//  2. **No labeller sees another's work on a pull request before submitting
//     their own.** If one person labels first and the second sees that verdict
//     while deciding, the agreement rate measures influence. That is the same
//     failure as (1) wearing different clothes.
//
// Neither rule is expressed as "we do not select that column" alone. The
// queries name their columns explicitly, never SELECT *, never join
// hackathon_verdicts, and the blindness is a WHERE clause rather than a
// filter applied afterwards.

var (
	ErrNoSuchLabeller = errors.New("calibration: no such labeller")
	ErrNotInSample    = errors.New("calibration: that pull request is not in this sample")
	ErrSampleNotReady = errors.New("calibration: sample is not fully snapshotted")
)

// Labeller identifies who is labelling. Not a user, not a role.
type Labeller struct {
	ID          uuid.UUID `json:"id"`
	Handle      string    `json:"handle"`
	DisplayName string    `json:"display_name"`
}

func LabellerByHandle(ctx context.Context, local db.DBPool, handle string) (Labeller, error) {
	var l Labeller
	err := local.QueryRow(ctx, `
SELECT id, handle, display_name FROM calibration_labellers
WHERE handle = $1 AND retired_at IS NULL
`, handle).Scan(&l.ID, &l.Handle, &l.DisplayName)
	if errors.Is(err, pgx.ErrNoRows) {
		return l, fmt.Errorf("%w: %q", ErrNoSuchLabeller, handle)
	}
	return l, err
}

// QueueItem is one entry in a labeller's own queue.
//
// It carries no verdict - not even the labeller's own - because the queue is
// rendered before a decision is made and the screen must not prime it.
type QueueItem struct {
	SamplePRID uuid.UUID `json:"sample_pr_id"`
	Project    string    `json:"project"`
	Number     int       `json:"number"`
	// Labelled is whether *this* labeller has already submitted. Never whether
	// anybody else has: "two people have done this one" is itself information
	// about another labeller's activity, and a queue that shows it invites
	// working the same order as someone else.
	Labelled bool `json:"labelled"`
}

// Queue lists the pull requests this labeller may work on.
//
// Held-back rows are excluded entirely: they are withheld from labelling and
// comparison until released, and a labeller should not be able to see them at
// all, let alone label them by accident.
func Queue(ctx context.Context, local db.DBPool, sampleName string, labellerID uuid.UUID) ([]QueueItem, error) {
	rows, err := local.Query(ctx, `
SELECT sp.id, sp.project_full_name, sp.pr_number,
       EXISTS (
         SELECT 1 FROM calibration_labels l
         WHERE l.sample_pr_id = sp.id AND l.labeller_id = $2
       ) AS labelled_by_me
FROM calibration_sample_prs sp
JOIN calibration_samples s ON s.id = sp.sample_id
JOIN calibration_pr_snapshots snap ON snap.sample_pr_id = sp.id
WHERE s.name = $1 AND (NOT sp.held_back OR sp.released_at IS NOT NULL)
ORDER BY sp.created_at
`, sampleName, labellerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []QueueItem
	for rows.Next() {
		var q QueueItem
		if err := rows.Scan(&q.SamplePRID, &q.Project, &q.Number, &q.Labelled); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// PRForLabelling is everything the screen shows, and nothing else.
//
// Every field is listed explicitly. There is no map, no passthrough, and no
// embedded row from another table - so a column added to the database later
// cannot arrive here by accident.
type PRForLabelling struct {
	SamplePRID   uuid.UUID `json:"sample_pr_id"`
	Project      string    `json:"project"`
	Number       int       `json:"number"`
	Title        string    `json:"title"`
	Body         string    `json:"body"`
	Author       string    `json:"author"`
	URL          string    `json:"url"`
	Additions    int       `json:"additions"`
	Deletions    int       `json:"deletions"`
	ChangedFiles int       `json:"changed_files"`
	Files        []struct {
		Filename  string `json:"filename"`
		Status    string `json:"status"`
		Additions int    `json:"additions"`
		Deletions int    `json:"deletions"`
	} `json:"files"`
	Diff          string `json:"diff"`
	DiffTruncated bool   `json:"diff_truncated"`

	// HasIssue is false when the pull request links none, or links one that
	// could not be resolved. The screen says so in words. An empty panel would
	// read as a loading failure, and a labeller who thinks the criteria failed
	// to load judges differently from one who knows there are none.
	HasIssue    bool   `json:"has_issue"`
	IssueNumber int    `json:"issue_number,omitempty"`
	IssueTitle  string `json:"issue_title,omitempty"`
	IssueBody   string `json:"issue_body,omitempty"`

	// Framing is the question this row is to be labelled under, when it is not
	// the default one. Empty means the original framing: judge the
	// contribution as a whole.
	//
	// It is stored on the row and rendered above the diff rather than being
	// something a labeller is told once and expected to hold in mind. Two
	// batches labelled under two questions, with the difference living only in
	// someone's memory, is how the two get pooled later.
	Framing string `json:"framing,omitempty"`

	// MyLabel is this labeller's own previous verdict, if any, so a returning
	// labeller can see what they said. Never anyone else's.
	MyLabel *Label `json:"my_label,omitempty"`
}

// Label is one verdict.
type Label struct {
	ID         uuid.UUID `json:"id"`
	Verdict    string    `json:"verdict"`
	Reason     string    `json:"reason"`
	Confidence string    `json:"confidence"`
	CreatedAt  string    `json:"created_at"`
}

// GetPRForLabelling reads one pull request's frozen snapshot.
//
// The only rows it touches are calibration_* tables. It does not join
// hackathon_verdicts, does not read judge_bucket or any sibling, and returns
// no other labeller's label - the my_label subquery is constrained to the
// requesting labeller in SQL, not filtered in Go afterwards.
func GetPRForLabelling(ctx context.Context, local db.DBPool, sampleName string, samplePRID, labellerID uuid.UUID) (PRForLabelling, error) {
	var p PRForLabelling
	var filesJSON []byte
	var issueNumber *int
	var issueTitle, issueBody *string

	err := local.QueryRow(ctx, `
SELECT sp.id, sp.project_full_name, sp.pr_number,
       snap.title, COALESCE(snap.body, ''), COALESCE(snap.author_login, ''), COALESCE(snap.url, ''),
       snap.additions, snap.deletions, snap.changed_files,
       snap.files, snap.diff, snap.diff_truncated,
       snap.issue_number, snap.issue_title, snap.issue_body,
       COALESCE(sp.labelling_framing, '')
FROM calibration_sample_prs sp
JOIN calibration_samples s ON s.id = sp.sample_id
JOIN calibration_pr_snapshots snap ON snap.sample_pr_id = sp.id
WHERE s.name = $1 AND sp.id = $2 AND (NOT sp.held_back OR sp.released_at IS NOT NULL)
`, sampleName, samplePRID).Scan(
		&p.SamplePRID, &p.Project, &p.Number,
		&p.Title, &p.Body, &p.Author, &p.URL,
		&p.Additions, &p.Deletions, &p.ChangedFiles,
		&filesJSON, &p.Diff, &p.DiffTruncated,
		&issueNumber, &issueTitle, &issueBody, &p.Framing,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrNotInSample
	}
	if err != nil {
		return p, err
	}

	if err := json.Unmarshal(filesJSON, &p.Files); err != nil {
		return p, fmt.Errorf("decode files: %w", err)
	}
	if issueNumber != nil {
		p.HasIssue = true
		p.IssueNumber = *issueNumber
		if issueTitle != nil {
			p.IssueTitle = *issueTitle
		}
		if issueBody != nil {
			p.IssueBody = *issueBody
		}
	}

	// This labeller's own most recent label, and only theirs.
	var l Label
	err = local.QueryRow(ctx, `
SELECT id, verdict, reason, confidence, created_at::text
FROM calibration_labels
WHERE sample_pr_id = $1 AND labeller_id = $2
ORDER BY created_at DESC LIMIT 1
`, samplePRID, labellerID).Scan(&l.ID, &l.Verdict, &l.Reason, &l.Confidence, &l.CreatedAt)
	if err == nil {
		p.MyLabel = &l
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return p, err
	}
	return p, nil
}

// SubmitLabel appends a verdict.
//
// Append-only is enforced by the database; this supplies supersedes_id so a
// changed mind is linked to what it replaced rather than merely landing beside
// it.
func SubmitLabel(ctx context.Context, local db.DBPool, samplePRID, labellerID uuid.UUID, verdict, reason, confidence string) (uuid.UUID, error) {
	var prior *uuid.UUID
	var priorID uuid.UUID
	err := local.QueryRow(ctx, `
SELECT id FROM calibration_labels
WHERE sample_pr_id = $1 AND labeller_id = $2
ORDER BY created_at DESC LIMIT 1
`, samplePRID, labellerID).Scan(&priorID)
	if err == nil {
		prior = &priorID
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, err
	}

	var id uuid.UUID
	err = local.QueryRow(ctx, `
INSERT INTO calibration_labels (sample_pr_id, labeller_id, verdict, reason, confidence, supersedes_id)
VALUES ($1, $2, $3, $4, $5, $6) RETURNING id
`, samplePRID, labellerID, verdict, reason, confidence, prior).Scan(&id)
	return id, err
}

// Inter-rater agreement is deliberately not computed here.
//
// It was, and the function is gone. This set has ONE labeller, so there is no
// second human verdict to agree with and any "agreement rate" would be a
// number with nothing on the other side of it. A UI that showed 0%, or 100%,
// or "n/a" would all invite the same misreading - that humans were compared
// and something was learned.
//
// What this set supports is a single-labeller benchmark: one person's
// judgement, against which a model can be scored. That is a weaker claim than
// inter-rater agreement and must never be presented as the stronger one.
//
// The only integrity check available with one labeller is self-consistency -
// re-labelling rows already decided, and comparing a person against their own
// earlier self. The append-only table already supports it: a re-label is a new
// row with supersedes_id set, and the original stays.
