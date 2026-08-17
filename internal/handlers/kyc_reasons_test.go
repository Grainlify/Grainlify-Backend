package handlers

import (
	"encoding/json"
	"strings"
	"testing"
)

// The closed reason list, and the one feature that must never map into it.

// Every fraud signal we have actually seen, and the rule that none of them may
// ever produce a suggestion.
//
// This is the assertion the comment in kycReasonForWarning exists to protect.
// Telling somebody "your document's country doesn't match your IP" hands them
// the instruction to use a VPN; "duplicated IP address from another session"
// tells them to switch networks. Neither is something an honest contributor
// can act on either - no amount of retaking the photo changes it - so the
// disclosure is pure downside.
//
// The risk list below deliberately includes codes that ARE mapped under other
// features. An earlier version of this test listed only the real ip_analysis
// risk names - and passed with the feature guard deleted, because none of
// those names appear in the switch anyway. It asserted something true for a
// reason unrelated to the rule it was named after: a check that agrees with
// you (VERIFICATION-TRAPS.md). Mixing in mapped codes makes the guard the only
// thing that can produce a pass.
func TestKYCReasonForWarning_IPAnalysisNeverMapsToAReason(t *testing.T) {
	for _, risk := range []string{
		// Real ip_analysis risks, seen in production.
		"COUNTRY_FROM_DOCUMENT_DOES_NOT_MATCH_COUNTRY_FROM_IP",
		"DUPLICATED_IP_ADDRESS",
		"MULTIPLE_DEVICES_IN_SESSION",
		"IP_LOCATION_MISMATCH",
		// Not a real code: whatever Didit adds next must also return nothing,
		// because the exclusion is on the feature rather than on a list of
		// risk names somebody has to maintain.
		"SOME_FUTURE_IP_SIGNAL",
		// Codes that DO map elsewhere. These are what make this test detect a
		// deleted guard rather than merely restate the switch's contents.
		"SCREEN_CAPTURE_DETECTED",
		"COULD_NOT_DETECT_DOCUMENT_TYPE",
		"DATA_INCONSISTENT",
		"LOW_FACE_MATCH_SIMILARITY",
		"MRZ_VALIDATION_FAILED",
	} {
		if code, ok := kycReasonForWarning("ip_analysis", risk); ok {
			t.Errorf("ip_analysis/%s suggested %q; fraud signals must never be disclosed to the contributor", risk, code)
		}
	}
}

// The same risk string under a non-ip_analysis feature is still excluded only
// if it is genuinely unmappable - the exclusion is on the feature, so this
// guards against implementing it as a risk-name blocklist by accident.
func TestKYCReasonForWarning_ExclusionIsOnTheFeatureNotTheRiskName(t *testing.T) {
	// SCREEN_CAPTURE_DETECTED maps under id_verification...
	if _, ok := kycReasonForWarning("id_verification", "SCREEN_CAPTURE_DETECTED"); !ok {
		t.Fatal("id_verification/SCREEN_CAPTURE_DETECTED should map to a reason")
	}
	// ...and must not map under ip_analysis, whatever it is called.
	if _, ok := kycReasonForWarning("ip_analysis", "SCREEN_CAPTURE_DETECTED"); ok {
		t.Error("ip_analysis must be excluded by feature, regardless of the risk name")
	}
}

// The sixth reason, and why it exists.
//
// Both contributors refused in production tripped SCREEN_CAPTURE_DETECTED - a
// photograph of a document on a screen, which no amount of better lighting
// fixes. Under the original five reasons the closest available answer was
// "document unreadable", which is true and useless.
func TestKYCReasonForWarning_ScreenCaptureHasItsOwnReason(t *testing.T) {
	code, ok := kycReasonForWarning("id_verification", "SCREEN_CAPTURE_DETECTED")
	if !ok {
		t.Fatal("SCREEN_CAPTURE_DETECTED produced no suggestion")
	}
	if code != "document_is_a_screen_photo" {
		t.Errorf("code = %q, want document_is_a_screen_photo", code)
	}
	r, ok := kycReasonByCode(code)
	if !ok {
		t.Fatalf("%q is not in kycResetReasons", code)
	}
	if !strings.Contains(strings.ToLower(r.Message), "physical document") {
		t.Errorf("message should tell them to photograph the physical document, got: %s", r.Message)
	}
}

// teethaking's actual decision, as stored in production.
//
// Four warnings across two features, one of which is a fraud signal. The
// suggestions must name the screen photo and the inconsistent data, and must
// say nothing whatsoever about the IP mismatch.
func TestSuggestKYCReasons_TeethakingWorkedExample(t *testing.T) {
	var decision map[string]interface{}
	if err := json.Unmarshal([]byte(`{
	  "id_verification": {"status":"Declined","warnings":[
	    {"risk":"DATA_INCONSISTENT"},
	    {"risk":"DOCUMENT_NUMBER_NOT_DETECTED"},
	    {"risk":"SCREEN_CAPTURE_DETECTED"}
	  ]},
	  "ip_analysis": {"warnings":[
	    {"risk":"COUNTRY_FROM_DOCUMENT_DOES_NOT_MATCH_COUNTRY_FROM_IP"}
	  ]}
	}`), &decision); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	got := SuggestKYCReasons(decision)
	want := map[string]bool{
		"information_did_not_match":  true,
		"document_unreadable":        true,
		"document_is_a_screen_photo": true,
	}
	if len(got) != len(want) {
		t.Errorf("got %v, want exactly %d suggestions", got, len(want))
	}
	for _, c := range got {
		if !want[c] {
			t.Errorf("unexpected suggestion %q", c)
		}
	}
}

// A refusal whose only warnings are fraud signals produces NO suggestion, and
// the admin is pushed to 'other' with a note they write deliberately. This is
// the intended outcome, not a gap.
func TestSuggestKYCReasons_IPOnlyRefusalSuggestsNothing(t *testing.T) {
	var decision map[string]interface{}
	if err := json.Unmarshal([]byte(`{
	  "ip_analysis": {"warnings":[
	    {"risk":"DUPLICATED_IP_ADDRESS"},
	    {"risk":"MULTIPLE_DEVICES_IN_SESSION"}
	  ]}
	}`), &decision); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if got := SuggestKYCReasons(decision); len(got) != 0 {
		t.Errorf("got %v, want no suggestions at all for an ip_analysis-only refusal", got)
	}
}

func TestSuggestKYCReasons_SurvivesMissingAndReshapedData(t *testing.T) {
	for name, raw := range map[string]string{
		"empty":                  `{}`,
		"warnings not an array":  `{"id_verification":{"warnings":"nope"}}`,
		"feature not an object":  `{"id_verification":"nope"}`,
		"warning not an object":  `{"id_verification":{"warnings":["nope"]}}`,
		"risk missing":           `{"id_verification":{"warnings":[{"foo":"bar"}]}}`,
		"unrecognised risk code": `{"id_verification":{"warnings":[{"risk":"SOMETHING_NEW"}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			var decision map[string]interface{}
			if err := json.Unmarshal([]byte(raw), &decision); err != nil {
				t.Fatalf("fixture: %v", err)
			}
			if got := SuggestKYCReasons(decision); len(got) != 0 {
				t.Errorf("got %v, want none", got)
			}
		})
	}
	if got := SuggestKYCReasons(nil); got != nil {
		t.Errorf("nil decision: got %v, want nil", got)
	}
}

// Every code carries a message a person can act on - except 'other', which is
// the one code that demands a note precisely because it carries none.
func TestKYCResetReasons_EveryCodeSaysSomethingActionable(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range kycResetReasons {
		if seen[r.Code] {
			t.Errorf("duplicate code %q", r.Code)
		}
		seen[r.Code] = true
		if r.Label == "" {
			t.Errorf("%s: no label", r.Code)
		}
		if r.NeedsNote {
			if r.Message != "" {
				t.Errorf("%s: needs a note AND carries a message; pick one", r.Code)
			}
			continue
		}
		if r.Message == "" {
			t.Errorf("%s: no message and does not require a note - it would send an empty refusal", r.Code)
		}
	}
	if !seen["other"] {
		t.Error("no 'other' code: an admin facing something the list does not cover has nowhere to go")
	}
}

// The message the contributor reads always ends by telling them they can start
// again. Without it, the message explains a problem and stops - which is how a
// refusal reads as final even when it is not, the exact dead end this feature
// exists to remove.
func TestKYCResetMessage_AlwaysNamesTheWayForward(t *testing.T) {
	for _, r := range kycResetReasons {
		note := ""
		if r.NeedsNote {
			note = "we could not read the expiry date"
		}
		msg := kycResetMessage(r, note)
		if !strings.Contains(msg, "start a new verification") {
			t.Errorf("%s: message does not tell them how to proceed:\n%s", r.Code, msg)
		}
		if r.NeedsNote && !strings.Contains(msg, note) {
			t.Errorf("%s: note omitted from the message the contributor reads", r.Code)
		}
	}
}

// The fixed sentence comes first: it is the part somebody wrote carefully, in
// advance, and the part that names an action.
func TestKYCResetMessage_FixedSentencePrecedesTheNote(t *testing.T) {
	r, ok := kycReasonByCode("document_is_a_screen_photo")
	if !ok {
		t.Fatal("missing code")
	}
	const note = "the second upload was the same screenshot"
	msg := kycResetMessage(r, note)
	if strings.Index(msg, r.Message) > strings.Index(msg, note) {
		t.Errorf("the note precedes the written message:\n%s", msg)
	}
}
