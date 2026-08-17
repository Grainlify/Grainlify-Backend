package handlers

import "strings"

// The closed list of things we will tell a contributor about a refused or
// stuck verification.
//
// A fixed list rather than free text, for the same reason the social-follow
// review queue has one: the message is written once, in advance, by somebody
// thinking about how it reads - not improvised per contributor at the moment
// of a decision. The admin's own words are still available as an optional
// note; the code is what carries the meaning.
//
// # These are OUR words, never the provider's
//
// Didit's warnings are diagnostic strings written for an integrator
// ("MRZ_VALIDATION_FAILED", "OCR data in the document is not consistent").
// They are not addressed to the person who has to fix something, and they are
// no longer returned by /kyc/status at all. Every code below maps to a
// sentence that names an action.
//
// # What deliberately has no code
//
// Nothing here corresponds to an ip_analysis warning. See
// kycReasonForWarning.
type kycResetReason struct {
	Code  string
	Label string
	// Message is what the contributor reads. Written to be actionable on its
	// own, because it may arrive with no note attached.
	Message string
	// NeedsNote marks a code that says nothing by itself. Only 'other'
	// qualifies: every other code already names the problem, so the note is
	// optional detail rather than the whole content.
	NeedsNote bool
}

var kycResetReasons = []kycResetReason{
	{
		Code:  "document_unreadable",
		Label: "Document couldn't be read",
		Message: "Some details on your document couldn't be read. Take the photo " +
			"in bright, even light with all four corners in frame, and make sure " +
			"nothing is covering the text.",
	},
	{
		Code:  "document_type_not_recognised",
		Label: "Document type not recognised",
		Message: "We couldn't tell what kind of document was uploaded. Use a " +
			"passport, national ID card or driving licence, and upload the whole " +
			"document rather than a cropped section.",
	},
	{
		// The sixth reason, added because it is the most actionable failure we
		// have actually seen and it mapped to none of the original five. Both
		// contributors refused so far tripped SCREEN_CAPTURE_DETECTED: a
		// photograph of a document displayed on a screen, which no amount of
		// better lighting fixes. Telling those two people "your document
		// couldn't be read" would have been true and useless.
		Code:  "document_is_a_screen_photo",
		Label: "Photo of a screen, not the document",
		Message: "The upload looked like a photo of a screen or a photocopy rather " +
			"than the document itself. Photograph the original physical document " +
			"directly.",
	},
	{
		Code:  "face_did_not_match",
		Label: "Selfie didn't match the document",
		Message: "The selfie didn't match the photo on your document. Take it in " +
			"good light, face the camera directly, and remove anything covering " +
			"your face such as a hat, sunglasses or a mask.",
	},
	{
		Code:  "session_expired",
		Label: "Verification session expired",
		Message: "Your verification session expired before it was finished. " +
			"Starting a new one takes a couple of minutes.",
	},
	{
		Code:  "information_did_not_match",
		Label: "Details didn't match the document",
		Message: "The details read from your document weren't consistent. Check " +
			"that the document is valid and unexpired, and that nothing is " +
			"obscuring the printed details.",
	},
	{
		// Deliberately last, and the only code that demands a note. Without
		// one it is a refusal with no content, which is the dead end this
		// whole feature exists to remove.
		Code:      "other",
		Label:     "Something else (write a note)",
		Message:   "",
		NeedsNote: true,
	},
}

// kycResetMessage composes what the contributor reads: the fixed sentence for
// the chosen reason, then the admin's note if there is one.
//
// The fixed part comes first on purpose. It is the sentence somebody wrote
// carefully, in advance, and it is the part that names an action; a note
// written in the moment is context, not instruction. For 'other' there is no
// fixed part and the note is the whole message, which is why that code cannot
// be sent without one.
func kycResetMessage(r kycResetReason, note string) string {
	note = strings.TrimSpace(note)
	parts := make([]string, 0, 3)
	if r.Message != "" {
		parts = append(parts, r.Message)
	}
	if note != "" {
		parts = append(parts, note)
	}
	// Always last, and always present: without it the message explains a
	// problem and stops, which is how a refusal reads as final even when it
	// is not.
	parts = append(parts, "You can start a new verification from Settings → Billing Profiles.")
	return strings.Join(parts, "\n\n")
}

func kycReasonByCode(code string) (kycResetReason, bool) {
	for _, r := range kycResetReasons {
		if r.Code == code {
			return r, true
		}
	}
	return kycResetReason{}, false
}

// kycReasonForWarning suggests a reason code for one provider warning, for the
// admin review UI. A SUGGESTION - the admin reads the console and chooses. See
// SuggestKYCReasons.
//
// # ip_analysis is excluded, permanently and on purpose
//
// The feature is not in this switch and must never be added to it. Its
// warnings - COUNTRY_FROM_DOCUMENT_DOES_NOT_MATCH_COUNTRY_FROM_IP,
// DUPLICATED_IP_ADDRESS, MULTIPLE_DEVICES_IN_SESSION - are fraud signals, not
// user errors.
//
// Naming which one tripped tells the person exactly what to change to get past
// it next time. A contributor who is told "your document's country doesn't
// match your IP" has been handed the instruction to use a VPN; one told
// "duplicated IP address from another session" has been told to switch
// networks. That is true whether or not they were doing anything wrong, and
// the honest ones cannot act on it either - none of these is a thing a person
// fixes by taking a better photo.
//
// So an ip_analysis-only case has NO suggestion and falls to 'other' with a
// note the admin writes deliberately. That is the intended outcome, not a gap
// waiting to be filled: if you are here because a case produced no suggestion
// and you are about to add ip_analysis to make it, this comment is addressed
// to you.
func kycReasonForWarning(feature, risk string) (string, bool) {
	if feature == "ip_analysis" {
		return "", false
	}
	switch strings.ToUpper(strings.TrimSpace(risk)) {
	case "SCREEN_CAPTURE_DETECTED":
		return "document_is_a_screen_photo", true
	case "COULD_NOT_DETECT_DOCUMENT_TYPE":
		return "document_type_not_recognised", true
	case "QR_NOT_DETECTED", "MRZ_NOT_DETECTED", "MRZ_VALIDATION_FAILED",
		"DOCUMENT_NUMBER_NOT_DETECTED", "DATE_OF_BIRTH_NOT_DETECTED",
		"NAME_NOT_DETECTED", "EXPIRY_DATE_NOT_DETECTED", "PORTRAIT_NOT_DETECTED":
		return "document_unreadable", true
	case "DATA_INCONSISTENT", "DOCUMENT_EXPIRED", "DATA_MISMATCH":
		return "information_did_not_match", true
	case "LOW_FACE_MATCH_SIMILARITY", "FACE_MATCH_MAX_ATTEMPTS_EXCEEDED",
		"MAXIMUM_FACE_MATCH_ATTEMPTS_EXCEEDED", "NO_FACE_DETECTED":
		return "face_did_not_match", true
	default:
		// Unknown risk codes get no suggestion rather than a guess. The admin
		// is reading the provider console anyway, and a wrong suggestion is
		// worse than none: it is the one an admin in a hurry accepts.
		return "", false
	}
}

// SuggestKYCReasons reads a stored decision and proposes reason codes, most
// relevant first, deduplicated.
//
// Never used to send anything. The admin review screen shows these
// pre-selected-as-candidates and the admin picks; the send path takes the code
// it is given and does not consult this at all. A suggestion that could send
// itself would be exactly the "provider text reaches the user" problem again,
// one level of indirection away.
//
// Returns nil when nothing maps - notably for an ip_analysis-only refusal.
func SuggestKYCReasons(kycData map[string]interface{}) []string {
	if kycData == nil {
		return nil
	}
	// Ordered so the suggestion list is stable across calls; a queue whose
	// suggestions reshuffle between refreshes is one an admin stops trusting.
	features := []string{"id_verification", "face_match", "liveness", "document_ai_documents"}
	seen := map[string]bool{}
	var out []string
	for _, feature := range features {
		block, ok := kycData[feature].(map[string]interface{})
		if !ok {
			continue
		}
		warnings, ok := block["warnings"].([]interface{})
		if !ok {
			continue
		}
		for _, w := range warnings {
			warning, ok := w.(map[string]interface{})
			if !ok {
				continue
			}
			risk, _ := warning["risk"].(string)
			code, ok := kycReasonForWarning(feature, risk)
			if !ok || seen[code] {
				continue
			}
			seen[code] = true
			out = append(out, code)
		}
	}
	return out
}
