package sponsor

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strings"
	"testing"
)

const keyFile = "key.go"

// The claim key.go makes is that the sponsor key never leaves it. That is a
// statement about the file, so it is checked against the file.
//
// A behavioural test could only show the key did not leak on the paths it
// happens to exercise, which is the weaker claim.
func keySource(t *testing.T) (string, *ast.File) {
	t.Helper()
	b, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatalf("read %s: %v", keyFile, err)
	}
	f, err := parser.ParseFile(token.NewFileSet(), keyFile, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	src := string(b)
	if i := strings.Index(src, "\npackage sponsor"); i >= 0 {
		src = src[i:] // drop the doc comment, which discusses the key at length
	}
	return src, f
}

// No exported function may return the private key.
func TestKeySurface_NothingExportedReturnsThePrivateKey(t *testing.T) {
	_, f := keySource(t)
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || !fn.Name.IsExported() || fn.Type.Results == nil {
			return true
		}
		for _, r := range fn.Type.Results.List {
			if sel, ok := r.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "PrivateKey" {
				t.Errorf("exported %s returns an ed25519.PrivateKey; the key must not leave this type",
					fn.Name.Name)
			}
		}
		return true
	})
}

// The key must never reach a formatted string. That is how secrets get into
// error bodies and logs.
func TestKeySurface_ThePrivateKeyNeverReachesAFormatString(t *testing.T) {
	src, _ := keySource(t)
	fmtCall := regexp.MustCompile(`(?s)(fmt\.(Errorf|Sprintf|Printf)|slog\.\w+)\([^)]*`)
	for _, m := range fmtCall.FindAllString(src, -1) {
		for _, banned := range []string{"s.priv", "priv)", "priv,", "raw", "b)"} {
			if strings.Contains(m, banned) {
				t.Errorf("a format call may carry the key or its raw input: %.90s", strings.TrimSpace(m))
			}
		}
	}
}

// Exactly one function reads the environment variable.
func TestKeySurface_OnlyOneFunctionReadsTheEnvironment(t *testing.T) {
	src, _ := keySource(t)
	if n := strings.Count(src, "os.Getenv"); n != 1 {
		t.Errorf("os.Getenv appears %d times in %s; exactly one function may read the key", n, keyFile)
	}
}

// An absent key must refuse, never fall back.
func TestLoadSigner_RefusesRatherThanFallingBack(t *testing.T) {
	t.Setenv(SponsorKeyEnv, "")
	if _, err := LoadSigner(); err == nil {
		t.Fatal("an unset sponsor key produced a signer; unsponsored fallback is this issue twice")
	}
	t.Setenv(SponsorKeyEnv, "not-hex")
	if _, err := LoadSigner(); err == nil {
		t.Fatal("a malformed key produced a signer")
	}
	t.Setenv(SponsorKeyEnv, "0x00112233")
	_, err := LoadSigner()
	if err == nil {
		t.Fatal("a short key produced a signer")
	}
	if strings.Contains(err.Error(), "00112233") {
		t.Errorf("the error body contains the key material: %v", err)
	}
}

func TestLoadSigner_AcceptsASeedAndSigns(t *testing.T) {
	seed := strings.Repeat("ab", 32)
	t.Setenv(SponsorKeyEnv, "0x"+seed)
	s, err := LoadSigner()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s.PublicKeyHex(), "0x") || len(s.PublicKeyHex()) != 66 {
		t.Errorf("public key %q", s.PublicKeyHex())
	}
	if len(s.Sign([]byte("message"))) != 64 {
		t.Error("signature is not 64 bytes")
	}
}
