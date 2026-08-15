package handlers

import "testing"

// Every status Didit's v3 webhook documents must map somewhere deliberate.
//
// The mapping was incomplete and failed silently. mapDiditStatus returned
// "not_started" for anything it did not recognise, and "not_started" is one of
// the three states canStartNewKYCSession treats as free to begin - so an
// unrecognised status rewrote a live verification into "never started".
//
// Five of Didit's ten documented statuses fell through: "In Progress" (the
// switch had in_progress and inprogress but not the spaced form Didit actually
// sends), "Abandoned", "Kyc Expired", "Resubmitted" and "Awaiting User".
//
// This table is the whole documented set. A new Didit status now fails here
// rather than quietly becoming "never started", which is the property that was
// missing - the previous behaviour logged an error nobody was reading and
// carried on writing a wrong value.

// diditV3Statuses is Didit's documented set, verbatim and case-sensitive.
var diditV3Statuses = map[string]string{
	"Approved":      "verified",
	"Declined":      "rejected",
	"In Review":     "in_review",
	"Resubmitted":   "in_review",
	"In Progress":   "pending",
	"Awaiting User": "pending",
	"Not Started":   "not_started",
	"Expired":       "expired",
	"Kyc Expired":   "expired",
	"Abandoned":     "expired",
}

func TestMapDiditStatus_EveryDocumentedV3StatusIsRecognised(t *testing.T) {
	for raw, want := range diditV3Statuses {
		got, ok := mapDiditStatus(raw)
		if !ok {
			t.Errorf("mapDiditStatus(%q) is unrecognised; Didit documents it for v3 webhooks, "+
				"so it needs a deliberate mapping rather than falling through", raw)
			continue
		}
		if got != want {
			t.Errorf("mapDiditStatus(%q) = %q, want %q", raw, got, want)
		}
	}
}

// The values written must be ones the database will accept. A mapping that
// returns a status outside the CHECK constraint fails at write time, on a
// webhook, where the error is a 500 nobody sees.
func TestMapDiditStatus_OnlyProducesStatusesTheSchemaAllows(t *testing.T) {
	allowed := map[string]bool{
		"not_started": true, "pending": true, "in_review": true,
		"verified": true, "rejected": true, "expired": true,
	}
	for raw := range diditV3Statuses {
		got, ok := mapDiditStatus(raw)
		if ok && !allowed[got] {
			t.Errorf("mapDiditStatus(%q) = %q, which users_kyc_status_check would reject", raw, got)
		}
	}
}

// Case and surrounding whitespace must not matter: the same status arrives
// from the webhook body and from the decision API, and they have not always
// agreed on capitalisation.
func TestMapDiditStatus_IsCaseAndWhitespaceInsensitive(t *testing.T) {
	for _, raw := range []string{"APPROVED", "approved", "  Approved  ", "aPpRoVeD"} {
		got, ok := mapDiditStatus(raw)
		if !ok || got != "verified" {
			t.Errorf("mapDiditStatus(%q) = (%q, %v), want (\"verified\", true)", raw, got, ok)
		}
	}
	// The spaced form is the one that actually shipped broken.
	if got, ok := mapDiditStatus("In Progress"); !ok || got != "pending" {
		t.Errorf(`mapDiditStatus("In Progress") = (%q, %v), want ("pending", true)`, got, ok)
	}
}

// An unrecognised status must report itself rather than resolve to a state.
// This is the guard against the original bug returning in a new coat.
func TestMapDiditStatus_UnknownIsReportedNotGuessed(t *testing.T) {
	for _, raw := range []string{"", "Something Didit Added In 2027", "verified-ish", "ok"} {
		got, ok := mapDiditStatus(raw)
		if ok {
			t.Errorf("mapDiditStatus(%q) claimed to recognise it and returned %q", raw, got)
		}
		if got != "" {
			t.Errorf("mapDiditStatus(%q) returned %q alongside ok=false; callers must not have a "+
				"value to accidentally write", raw, got)
		}
	}
}

// A contributor midway through verification must never be recorded as not
// having started - that was the user-visible harm, because "not_started" lets
// a new session begin and discards the one in flight.
func TestMapDiditStatus_InFlightStatusesNeverBecomeNotStarted(t *testing.T) {
	inFlight := []string{"In Progress", "In Review", "Awaiting User", "Resubmitted"}
	for _, raw := range inFlight {
		got, ok := mapDiditStatus(raw)
		if !ok {
			t.Fatalf("mapDiditStatus(%q) unrecognised", raw)
		}
		if got == "not_started" {
			t.Errorf("mapDiditStatus(%q) = %q: a verification in flight must not read as never begun", raw, got)
		}
		// And it must be a status that blocks starting a second session.
		if canStartNewKYCSession(&got) {
			t.Errorf("mapDiditStatus(%q) = %q, which canStartNewKYCSession allows - "+
				"the contributor would be able to abandon a live session by starting another", raw, got)
		}
	}
}

// Conversely, the terminal-but-retryable statuses must let a contributor begin
// again. "Abandoned" is mapped to expired precisely so a walked-away attempt
// does not strand them behind a session they will never finish.
func TestMapDiditStatus_AbandonedAndExpiredAllowARetry(t *testing.T) {
	for _, raw := range []string{"Abandoned", "Expired", "Kyc Expired"} {
		got, ok := mapDiditStatus(raw)
		if !ok {
			t.Fatalf("mapDiditStatus(%q) unrecognised", raw)
		}
		if !canStartNewKYCSession(&got) {
			t.Errorf("mapDiditStatus(%q) = %q, which blocks a new session - a contributor whose "+
				"attempt lapsed must be able to try again without an admin", raw, got)
		}
	}
}
