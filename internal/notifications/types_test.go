package notifications

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// Every Type constant must appear in AllTypes.
//
// AllTypes is what the preferences API iterates, so a constant declared and
// left out of it produces a notification with no control anywhere to turn it
// off. types.go has warned about this in a comment since founding_position was
// added; the comment did not stop the seven bounty types from being declared
// and omitted, because a comment cannot fail a build.
//
// Go has no reflection over constants, so this reads the declarations out of
// the source file itself rather than keeping a third hand-maintained copy that
// could drift in its own right.
func TestAllTypesContainsEveryDeclaredType(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "types.go", nil, 0)
	if err != nil {
		t.Fatalf("parse types.go: %v", err)
	}

	declared := map[string]string{} // constant name -> string value
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			ident, ok := vs.Type.(*ast.Ident)
			if !ok || ident.Name != "Type" {
				continue
			}
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s: %v", lit.Value, err)
				}
				declared[name.Name] = value
			}
		}
	}

	if len(declared) == 0 {
		t.Fatal("parsed no Type constants out of types.go - the test, not the list, is broken")
	}

	listed := map[Type]bool{}
	for _, ty := range AllTypes {
		listed[ty] = true
	}

	for name, value := range declared {
		if !listed[Type(value)] {
			t.Errorf("%s (%q) is declared but missing from AllTypes: it would arrive with no way to turn it off", name, value)
		}
		if !Type(value).Valid() {
			t.Errorf("%s (%q) is not Valid(), so a preference update naming it is rejected", name, value)
		}
	}
}

// The reverse: nothing in AllTypes that no constant declares, which would show
// the user a control for a notification that cannot be sent.
func TestAllTypesHasNoDuplicatesOrStrays(t *testing.T) {
	seen := map[Type]bool{}
	for _, ty := range AllTypes {
		if seen[ty] {
			t.Errorf("%q appears in AllTypes twice, so the preferences API returns it twice", ty)
		}
		seen[ty] = true
	}
}
