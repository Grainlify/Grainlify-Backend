package handlers

import (
	"strings"
	"testing"
	"time"
)

// Built from a real declined session (Hollujay, 15 Aug), reduced to the fields
// this reads. The shape is the point: per-check objects each with their own
// status, warnings nested inside the check that raised them.
const realDiditDecision = `{
  "created_at": "2026-08-15T20:18:20.187973Z",
  "liveness":        {"status": "Approved"},
  "id_verification": {"status": "Approved"},
  "ip_analysis":     {"status": "Approved"},
  "face_match": {
    "status": "In Review",
    "warnings": [
      {"risk": "FACE_MATCH_MAX_ATTEMPTS_EXCEEDED", "log_type": "information"},
      {"risk": "LOW_FACE_MATCH_SIMILARITY",        "log_type": "warning"}
    ]
  }
}`

// What an admin needs in order to decide before opening the console.
func TestExtractKYCReviewDetail_NamesTheBlockingCheckAndTheDecidingWarning(t *testing.T) {
	d := extractKYCReviewDetail([]byte(realDiditDecision))

	if !strings.Contains(d.BlockingCheck, "face match") {
		t.Errorf("blocking check = %q, want the face match named", d.BlockingCheck)
	}
	// Three checks passed. Reporting the session as "in review" without saying
	// which check is the reason is the difference between an alert somebody can
	// act on and one they have to go and investigate.
	for _, passed := range []string{"ID document", "liveness", "IP analysis"} {
		if strings.Contains(d.BlockingCheck, passed) {
			t.Errorf("approved check %q reported as blocking: %q", passed, d.BlockingCheck)
		}
	}
	if len(d.Warnings) != 2 {
		t.Errorf("warnings = %v, want both codes", d.Warnings)
	}

	// The one that changes the answer: with attempts exhausted the contributor
	// cannot retry however good a link they are given, so a reviewer has to
	// decide. Note Didit tags this one log_type "information" - filtering on
	// severity would drop precisely the code that matters.
	if !d.AttemptsExceeded {
		t.Error("FACE_MATCH_MAX_ATTEMPTS_EXCEEDED did not set AttemptsExceeded")
	}

	if d.SessionStartedAt == nil {
		t.Fatal("no session start; age is read from Didit's created_at, not users.updated_at")
	}
	if got := d.SessionStartedAt.UTC().Format(time.RFC3339); got != "2026-08-15T20:18:20Z" {
		t.Errorf("session start = %s", got)
	}
}

// Didit has two spellings in circulation for one condition: the stored payload
// says FACE_MATCH_MAX_ATTEMPTS_EXCEEDED, the console displays
// MAXIMUM_FACE_MATCH_ATTEMPTS_EXCEEDED. Missing either is invisible - no flag,
// and the interface then offers a retry to somebody with no attempts left.
func TestExtractKYCReviewDetail_CatchesEitherSpellingOfAttemptsExceeded(t *testing.T) {
	for _, code := range []string{
		"FACE_MATCH_MAX_ATTEMPTS_EXCEEDED",
		"MAXIMUM_FACE_MATCH_ATTEMPTS_EXCEEDED",
		"LIVENESS_MAX_ATTEMPTS_EXCEEDED",
	} {
		raw := []byte(`{"face_match":{"status":"In Review","warnings":[{"risk":"` + code + `"}]}}`)
		if !extractKYCReviewDetail(raw).AttemptsExceeded {
			t.Errorf("%s did not set AttemptsExceeded", code)
		}
	}
	// And it must not fire on a warning that leaves a retry possible.
	raw := []byte(`{"face_match":{"status":"In Review","warnings":[{"risk":"LOW_FACE_MATCH_SIMILARITY"}]}}`)
	if extractKYCReviewDetail(raw).AttemptsExceeded {
		t.Error("a low-similarity warning was read as attempts exhausted; that hides a retry somebody could use")
	}
}

// The set of checks is discovered from the payload, not from a list we keep.
// Charles's session carried ip_analysis, which the first version of that list
// did not have.
func TestExtractKYCReviewDetail_NamesAnUnfamiliarCheck(t *testing.T) {
	raw := []byte(`{"some_new_check": {"status": "In Review"}}`)
	d := extractKYCReviewDetail(raw)
	if d.BlockingCheck == "" {
		t.Fatal("an unknown check produced an alert naming nothing")
	}
	if !strings.Contains(d.BlockingCheck, "some new check") {
		t.Errorf("blocking check = %q, want the raw field humanised", d.BlockingCheck)
	}
}

// Two checks in review must produce a stable line - map iteration order is
// randomised per run, and an alert whose text changes between runs looks like
// two different events.
func TestExtractKYCReviewDetail_OrdersMultipleBlockingChecksStably(t *testing.T) {
	raw := []byte(`{"face_match":{"status":"In Review"},"aml":{"status":"In Review"},"nfc":{"status":"In Review"}}`)
	first := extractKYCReviewDetail(raw).BlockingCheck
	for i := 0; i < 20; i++ {
		if got := extractKYCReviewDetail(raw).BlockingCheck; got != first {
			t.Fatalf("blocking check varies between runs: %q then %q", first, got)
		}
	}
}

func TestExtractKYCReviewDetail_SurvivesMissingAndReshapedData(t *testing.T) {
	// A notification must still be sent when the payload is not what we expect.
	// Losing detail is acceptable; losing the alert is the failure this whole
	// change exists to prevent.
	for _, raw := range [][]byte{
		nil,
		[]byte(``),
		[]byte(`not json`),
		[]byte(`{}`),
		[]byte(`{"face_match": "a string, not an object"}`),
		[]byte(`{"created_at": "not a timestamp"}`),
		[]byte(`{"face_match": {"status": "In Review", "warnings": "not a list"}}`),
	} {
		d := extractKYCReviewDetail(raw) // must not panic
		if d.AttemptsExceeded {
			t.Errorf("attempts-exceeded inferred from %q", raw)
		}
	}
}

func TestBuildKYCReviewMessage_StatesTheAttemptsLimitRatherThanImplyingIt(t *testing.T) {
	started := time.Now().Add(-17*time.Hour - 22*time.Minute)
	msg := buildKYCReviewMessage("Hollujay", "sess-123", "webhook", kycReviewDetail{
		BlockingCheck:    "face match (selfie)",
		Warnings:         []string{"FACE_MATCH_MAX_ATTEMPTS_EXCEEDED", "LOW_FACE_MATCH_SIMILARITY"},
		AttemptsExceeded: true,
		SessionStartedAt: &started,
	})

	for _, want := range []string{"Hollujay", "17h 22m", "face match", "FACE_MATCH_MAX_ATTEMPTS_EXCEEDED", "sess-123"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
	// Spelled out, not left to be inferred from a code, because it is the line
	// that decides whether the answer is "reset them" or "tell them to retry".
	if !strings.Contains(msg, "cannot fix this themselves") {
		t.Errorf("attempts-exhausted not stated in words:\n%s", msg)
	}
}

func TestBuildKYCReviewMessage_SaysWhenTheWebhookDidNotArrive(t *testing.T) {
	msg := buildKYCReviewMessage("someone", "s1", "sweep", kycReviewDetail{})
	if !strings.Contains(msg, "sweep") {
		t.Error("a sweep-sourced alert should say so - it means Didit's delivery was dropped")
	}
	if strings.Contains(buildKYCReviewMessage("someone", "s1", "webhook", kycReviewDetail{}), "sweep") {
		t.Error("a webhook-sourced alert claimed to come from the sweep")
	}
}
