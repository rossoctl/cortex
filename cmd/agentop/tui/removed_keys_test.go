package tui

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

func TestNoStringNamesARemovedKey(t *testing.T) {
	removed := []string{"[t]", "[C]", "[X]"}
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				for _, k := range removed {
					if strings.Contains(lit.Value, k) {
						t.Errorf("%s: %s names removed key %s", fset.Position(lit.Pos()), lit.Value, k)
					}
				}
				return true
			})
		}
	}
}
