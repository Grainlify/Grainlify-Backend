package handlers

// The rejection reasons, defined once.
//
// The code is what gets stored and counted; the label is what a human reads.
// Both live here and the label is resolved server-side into every response
// that carries a code, so the admin picker, the contributor's status page and
// the notification email all render the same words without any of them
// holding their own copy. A second copy in TypeScript would be a rule written
// down twice, and this codebase has been bitten by that repeatedly - the
// leaderboard and the profile disagreeing about one contributor's rank came
// from exactly that shape.
//
// Adding a code means adding it here AND to the CHECK constraint in
// migration 000065. TestSocialFollowReasonCodes_MatchTheDatabaseConstraint
// asserts the two agree rather than trusting them to.
type socialFollowReason struct {
	Code  string
	Label string
	// NeedsNote marks a code that says nothing on its own. Only 'other' does:
	// every other code names the actual problem, so a note is optional colour
	// rather than the entire content of the message.
	NeedsNote bool
}

var socialFollowReasons = []socialFollowReason{
	{Code: "x_no_follow", Label: "X proof doesn't show a follow"},
	{Code: "linkedin_no_follow", Label: "LinkedIn proof doesn't show a follow"},
	{Code: "unreadable", Label: "Screenshot unreadable or wrong image"},
	{Code: "wrong_account", Label: "Wrong account followed"},
	{Code: "duplicate", Label: "Duplicate submission"},
	{Code: "other", Label: "Other", NeedsNote: true},
}

func socialFollowReasonByCode(code string) (socialFollowReason, bool) {
	for _, r := range socialFollowReasons {
		if r.Code == code {
			return r, true
		}
	}
	return socialFollowReason{}, false
}

// socialFollowReasonLabel resolves a stored code to its label.
//
// Returns "" for an empty or unknown code rather than inventing text. An
// unknown code means the row predates this list or was written by something
// that should not have been able to write it; either way the honest render is
// the free-text note on its own, not a guess.
func socialFollowReasonLabel(code *string) string {
	if code == nil || *code == "" {
		return ""
	}
	if r, ok := socialFollowReasonByCode(*code); ok {
		return r.Label
	}
	return ""
}

// socialFollowDecisionText is what the contributor is actually shown - in the
// notification, and on their rewards page.
//
// Code and note are combined rather than one replacing the other: the code
// says which category of problem it was, the note says what specifically. For
// 'other' the note IS the message, which is why it is required there.
func socialFollowDecisionText(code *string, note string) string {
	label := socialFollowReasonLabel(code)
	switch {
	case label != "" && note != "":
		return label + " - " + note
	case label != "":
		return label
	default:
		// Either a legacy free-text decision, or an unknown code. The note is
		// all there is, and it is better than nothing.
		return note
	}
}
