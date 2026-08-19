package salt

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
)

// The claim this package makes is "a salt is never returned". That is a
// statement about the exported surface, so it is checked against the exported
// surface rather than trusted.
//
// A redacting String()/MarshalJSON would have defended the paths we thought of.
// This defends the shape: if there is nothing that hands out bytes, there is
// nothing to redact, log, or format by accident.
func parsePackage(t *testing.T) map[string]*ast.File {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	p, ok := pkgs["salt"]
	if !ok {
		t.Fatal("package salt not found")
	}
	return p.Files
}

func renderType(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.ArrayType:
		if id, ok := v.Elt.(*ast.Ident); ok && id.Name == "byte" && v.Len == nil {
			return "[]byte"
		}
		return "array"
	case *ast.StarExpr:
		return "*" + renderType(v.X)
	case *ast.SelectorExpr:
		return renderType(v.X) + "." + v.Sel.Name
	}
	return "other"
}

// No exported function or method may return raw bytes.
func TestSurface_NothingExportedReturnsBytes(t *testing.T) {
	for name, f := range parsePackage(t) {
		ast.Inspect(f, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || !fn.Name.IsExported() || fn.Type.Results == nil {
				return true
			}
			// A method on an unexported receiver is not reachable from outside.
			if fn.Recv != nil && len(fn.Recv.List) == 1 {
				if !strings.HasPrefix(strings.TrimPrefix(renderType(fn.Recv.List[0].Type), "*"), strings.ToUpper(string(renderType(fn.Recv.List[0].Type)[0]))) {
					return true
				}
			}
			for _, r := range fn.Type.Results.List {
				if renderType(r.Type) == "[]byte" {
					t.Errorf("%s: exported %s returns []byte — that is the accessor this package exists to not have",
						name, fn.Name.Name)
				}
			}
			return true
		})
	}
}

// The Hasher interface is the whole contract. If a method is added to it, that
// is a decision someone must make deliberately rather than a drive-by.
func TestSurface_HasherExposesOnlyIdentityHashes(t *testing.T) {
	found := false
	for _, f := range parsePackage(t) {
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok || ts.Name.Name != "Hasher" {
				return true
			}
			it, ok := ts.Type.(*ast.InterfaceType)
			if !ok {
				return true
			}
			found = true
			var methods []string
			for _, m := range it.Methods.List {
				for _, nm := range m.Names {
					methods = append(methods, nm.Name)
				}
			}
			if len(methods) != 1 || methods[0] != "IdentityHashes" {
				t.Errorf("Hasher exposes %v; want exactly [IdentityHashes]. "+
					"Adding a method here widens what escapes the closure.", methods)
			}
			return true
		})
	}
	if !found {
		t.Fatal("Hasher interface not found")
	}
}

// IdentityHashes must take and return SLICES. A per-login signature is what
// creates the honest argument for an accessor: "give me the salt and I will
// loop". Keeping it batched is what makes the closed API hold under pressure.
func TestSurface_IdentityHashesIsBatched(t *testing.T) {
	for _, f := range parsePackage(t) {
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok || ts.Name.Name != "Hasher" {
				return true
			}
			it := ts.Type.(*ast.InterfaceType)
			for _, m := range it.Methods.List {
				ft, ok := m.Type.(*ast.FuncType)
				if !ok {
					continue
				}
				if ft.Params.NumFields() == 0 {
					t.Fatal("IdentityHashes takes no parameters")
				}
				if _, isSlice := ft.Params.List[0].Type.(*ast.ArrayType); !isSlice {
					t.Error("IdentityHashes does not take a slice; a per-login signature " +
						"invites a caller to ask for the raw salt so they can loop")
				}
			}
			return true
		})
	}
}

// No exported struct may carry a byte field that could hold the plaintext.
func TestSurface_NoExportedTypeCarriesBytes(t *testing.T) {
	for name, f := range parsePackage(t) {
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok || !ts.Name.IsExported() {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				return true
			}
			for _, fld := range st.Fields.List {
				if renderType(fld.Type) != "[]byte" {
					continue
				}
				for _, nm := range fld.Names {
					if nm.IsExported() {
						t.Errorf("%s: exported type %s has exported []byte field %s",
							name, ts.Name.Name, nm.Name)
					}
				}
			}
			return true
		})
	}
}
