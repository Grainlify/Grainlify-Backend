package handlers

import (
	"errors"
	"fmt"
	"testing"
)

// A session that can never answer must release the contributor.
//
// This is the rule that decides whether a stuck verification self-heals on the
// next status poll or waits for an admin to notice. It got that decision wrong
// for three contributors: after the Didit API key migration their sessions
// belonged to the retired account, Didit answered 403 "You do not have
// permission to perform this action", and the match list contained only
// gone-shaped strings (404 / not found / invalid / deleted). 403 matched none
// of them, so the sessions were treated as live and the contributors stayed in
// in_review with no path forward.

// The exact error didit.Client produces, so these cases are the real strings
// rather than an idealised version of them.
func diditErr(status int, body string) error {
	return fmt.Errorf("didit get decision failed: status %d, error: %s, body: %s", status, body, body)
}

func TestDiditSessionUnreachable_ReleasesSessionsFromARetiredAccount(t *testing.T) {
	// The case that stranded Rufai-Ahmed, Hollujay and Unclebaffa.
	err := diditErr(403, "You do not have permission to perform this action.")
	if !diditSessionUnreachable(err) {
		t.Fatalf("a 403 from a retired Didit account must release the session; got false for %v", err)
	}
}

func TestDiditSessionUnreachable_TrueForEveryUnanswerableShape(t *testing.T) {
	cases := []error{
		diditErr(404, "Not found"),
		diditErr(404, "session not_found"),
		diditErr(403, "Forbidden"),
		diditErr(401, "Unauthorized"),
		diditErr(400, "invalid session id"),
		errors.New("didit get decision failed: session was deleted"),
		errors.New("session does not exist"),
		errors.New("no such session"),
		errors.New("resource not available"),
	}
	for _, err := range cases {
		if !diditSessionUnreachable(err) {
			t.Errorf("expected release for: %v", err)
		}
	}
}

func TestDiditSessionUnreachable_FalseForErrorsThatMayResolve(t *testing.T) {
	// A transient failure must NOT release the session: doing so would let a
	// contributor start a second verification while the first is still live,
	// and the decision for the abandoned one would then have nowhere to land.
	cases := []error{
		nil,
		errors.New("dial tcp: i/o timeout"),
		errors.New("context deadline exceeded"),
		diditErr(500, "Internal Server Error"),
		diditErr(502, "Bad Gateway"),
		diditErr(429, "Too Many Requests"),
	}
	for _, err := range cases {
		if diditSessionUnreachable(err) {
			t.Errorf("a recoverable failure must not release the session: %v", err)
		}
	}
}

// Both endpoints must agree. Start() and Status() each carried their own list
// and had already drifted - Status() matched "does not exist", "no such" and
// "not available" while Start() did not - so the same dead session could
// release a contributor on one endpoint and block them on the other.
func TestDiditSessionUnreachable_IsTheOnlyDefinition(t *testing.T) {
	// A representative from each half of the old Status()-only set. If someone
	// reintroduces a local list at either call site, these still pass here but
	// the behaviour diverges - so this is paired with the greps below.
	for _, err := range []error{
		errors.New("session does not exist"),
		errors.New("no such session"),
		errors.New("not available"),
	} {
		if !diditSessionUnreachable(err) {
			t.Errorf("shared definition lost a case Status() used to handle: %v", err)
		}
	}
}
