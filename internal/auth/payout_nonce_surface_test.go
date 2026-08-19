package auth

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strings"
	"testing"
)

// The claim payout_nonce.go makes is that it CANNOT create a user. That is a
// statement about the file, so it is checked against the file.
//
// A behavioural test cannot establish this: it can only show that no user was
// created on the paths it happens to exercise, which is the weaker claim and the
// one that stays true right up until somebody adds a path.
const payoutNonceFile = "payout_nonce.go"

func payoutNonceSource(t *testing.T) (string, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, payoutNonceFile, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", payoutNonceFile, err)
	}
	b, err := os.ReadFile(payoutNonceFile)
	if err != nil {
		t.Fatalf("read %s: %v", payoutNonceFile, err)
	}
	return string(b), f
}

var userWrite = regexp.MustCompile(`(?is)\b(insert\s+into|update)\s+users\b`)

func TestPayoutNonce_CannotWriteToUsers(t *testing.T) {
	src, _ := payoutNonceSource(t)
	// Strip the doc comment, which legitimately discusses users at length.
	body := src
	if i := strings.Index(src, "package auth"); i >= 0 {
		body = src[i:]
	}
	if m := userWrite.FindString(body); m != "" {
		t.Fatalf("%s contains a write to users (%q). Authentication mints identities; "+
			"a payout path must not be able to.", payoutNonceFile, m)
	}
}

// A call to the sign-in consumer would create a user by proxy.
func TestPayoutNonce_DoesNotCallTheUpsertingConsumer(t *testing.T) {
	src, _ := payoutNonceSource(t)
	body := src
	if i := strings.Index(src, "package auth"); i >= 0 {
		body = src[i:]
	}
	for _, banned := range []string{"ConsumeNonceAndUpsertUser", "upsertUser", "UpsertUser"} {
		if strings.Contains(body, banned) {
			t.Errorf("%s references %s, which creates a user", payoutNonceFile, banned)
		}
	}
}

// The two consumers must stay separate functions, not one behind a flag: a
// boolean deciding whether to create an account is one edit from being passed
// the wrong way, and the wrong way is silent.
func TestPayoutNonce_ConsumerTakesNoCreateUserFlag(t *testing.T) {
	_, f := payoutNonceSource(t)
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "ConsumeNonceForPurpose" {
			return true
		}
		for _, p := range fn.Type.Params.List {
			id, ok := p.Type.(*ast.Ident)
			if !ok || id.Name != "bool" {
				continue
			}
			for _, nm := range p.Names {
				t.Errorf("ConsumeNonceForPurpose takes a bool parameter %q; a flag deciding "+
					"whether to create an account is exactly what this split exists to prevent", nm.Name)
			}
		}
		return true
	})
}

// It returns only an error: nothing that could carry a freshly-minted user.
func TestPayoutNonce_ConsumerReturnsOnlyAnError(t *testing.T) {
	_, f := payoutNonceSource(t)
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "ConsumeNonceForPurpose" {
			return true
		}
		found = true
		if fn.Type.Results == nil || len(fn.Type.Results.List) != 1 {
			t.Fatalf("ConsumeNonceForPurpose returns %d values; it must return exactly one error, "+
				"so there is nothing for a user object to travel in", fn.Type.Results.NumFields())
		}
		if id, ok := fn.Type.Results.List[0].Type.(*ast.Ident); !ok || id.Name != "error" {
			t.Error("ConsumeNonceForPurpose must return error and nothing else")
		}
		return true
	})
	if !found {
		t.Fatal("ConsumeNonceForPurpose not found")
	}
}
