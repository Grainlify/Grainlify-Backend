package handlers

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The reason codes exist in two places: this Go list, and the CHECK constraint
// in migration 000065. Two copies of one rule is the shape that had the
// leaderboard and the profile disagreeing about the same contributor's rank,
// so it is asserted rather than trusted.
//
// The failure it prevents is quiet: a code accepted by Go and refused by the
// constraint turns a rejection into a 500 at the moment an admin presses the
// button, and a code the constraint allows but Go does not know about becomes
// an unlabelled row nothing can render.
func TestSocialFollowReasonCodes_MatchTheDatabaseConstraint(t *testing.T) {
	const path = "../../migrations/000065_social_follow_reason_codes.up.sql"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}

	// Every IN (...) list in the file - there is one per table, and both must
	// agree with Go, so checking only the first would miss a drifted second.
	lists := regexp.MustCompile(`(?s)reason_code IN \(([^)]*)\)`).FindAllStringSubmatch(string(raw), -1)
	if len(lists) == 0 {
		t.Fatalf("no reason_code IN (...) found in %s - if the constraint moved, this test needs to follow it rather than be deleted", path)
	}

	inGo := []string{}
	for _, r := range socialFollowReasons {
		inGo = append(inGo, r.Code)
	}
	sort.Strings(inGo)

	for i, list := range lists {
		inSQL := []string{}
		for _, part := range strings.Split(list[1], ",") {
			if v := strings.Trim(strings.TrimSpace(part), "'"); v != "" {
				inSQL = append(inSQL, v)
			}
		}
		sort.Strings(inSQL)

		if strings.Join(inSQL, ",") != strings.Join(inGo, ",") {
			t.Errorf(`constraint %d has drifted from socialFollowReasons.

sql: %v
go:  %v

Adding a reason means adding it to both.`, i+1, inSQL, inGo)
		}
	}
}

func TestSocialFollowReasonCodes_OnlyOtherRequiresANote(t *testing.T) {
	// If a code that names the problem also demanded a note, reviewers would
	// type "see above" to get past it and the note would stop meaning anything.
	// If 'other' did not, a contributor would be told their proof was rejected
	// for "Other" - which reads as an answer while saying nothing.
	for _, r := range socialFollowReasons {
		if r.Code == "other" && !r.NeedsNote {
			t.Error(`"other" must require a note; it names no problem on its own`)
		}
		if r.Code != "other" && r.NeedsNote {
			t.Errorf("%q requires a note but already names the problem", r.Code)
		}
		if strings.TrimSpace(r.Label) == "" {
			t.Errorf("%q has no label; it would render as blank to the contributor", r.Code)
		}
	}
}

// What the contributor actually reads, for every combination of code and note.
func TestSocialFollowDecisionText_ReadsSensiblyInEveryCombination(t *testing.T) {
	code := func(s string) *string { return &s }

	for _, tc := range []struct {
		name string
		code *string
		note string
		want string
	}{
		{"code and note", code("unreadable"), "the second one is a profile page",
			"Screenshot unreadable or wrong image - the second one is a profile page"},
		{"code alone", code("wrong_account"), "", "Wrong account followed"},
		// The legacy shape: three decisions predate codes and their note is
		// the entire reason. It must still render.
		{"note alone", nil, "blurry, cannot see the follow button", "blurry, cannot see the follow button"},
		// An unknown code renders as the note rather than as invented text.
		{"unknown code with note", code("nonsense"), "some explanation", "some explanation"},
		{"unknown code alone", code("nonsense"), "", ""},
		{"nothing at all", nil, "", ""},
	} {
		if got := socialFollowDecisionText(tc.code, tc.note); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The transition table, which both the single-row and bulk paths read.
func TestSocialFollowCanTransition(t *testing.T) {
	for _, tc := range []struct {
		from, to string
		want     bool
	}{
		{socialFollowPending, socialFollowApproved, true},
		{socialFollowPending, socialFollowRejected, true},
		{socialFollowApproved, socialFollowRevoked, true},

		// Approve and reject were previously unguarded. Each of these silently
		// overwrote an existing decision and fired a fresh notification.
		{socialFollowApproved, socialFollowApproved, false},
		{socialFollowRejected, socialFollowApproved, false},
		{socialFollowRevoked, socialFollowApproved, false},
		{socialFollowApproved, socialFollowRejected, false},
		{socialFollowRejected, socialFollowRejected, false},

		// Revocation stays limited to something actually approved.
		{socialFollowPending, socialFollowRevoked, false},
		{socialFollowRejected, socialFollowRevoked, false},
		{socialFollowRevoked, socialFollowRevoked, false},
	} {
		if got := socialFollowCanTransition(tc.from, tc.to); got != tc.want {
			t.Errorf("%s -> %s = %v, want %v", tc.from, tc.to, got, tc.want)
		}
	}
}
