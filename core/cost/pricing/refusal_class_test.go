package pricing

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// TestRefusal_EveryDeclaredRefusalIsClassified makes a new Refusal a DECISION rather than a default.
//
// ImpossibleFigure splits the refusals in two: the figure was the wrong size, or an input was missing
// or unusable. A caller on the wrong side of that split is not hypothetical — cost/settle keyed on
// RefusalImplausibleTotal alone, which silently excluded RefusalUnrepresentable and with it every
// figure past ~$9.007e9, from the halves guard AND from the disclosure. A refused request then
// published a $5,000 half carrying no reason at all.
//
// A constant added later inherits false from ImpossibleFigure, which is that same failure arriving by
// omission. So the table below is checked against the DECLARATIONS in cost.go rather than maintained
// by hand: adding a Refusal without classifying it fails here, naming the constant.
func TestRefusal_EveryDeclaredRefusalIsClassified(t *testing.T) {
	// The intended classification, by constant name. Two claims per row: what the predicate must
	// answer, and — for the true rows — that the refusal really is about magnitude.
	want := map[string]bool{
		"RefusalNone":             false,
		"RefusalNoTokens":         false,
		"RefusalImpossibleCount":  false,
		"RefusalNoRate":           false,
		"RefusalUnrepresentable":  true,
		"RefusalImplausibleTotal": true,
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "cost.go", nil, 0)
	if err != nil {
		t.Fatalf("parse cost.go: %v", err)
	}

	declared := map[string]Refusal{}
	ast.Inspect(f, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		ident, ok := spec.Type.(*ast.Ident)
		if !ok || ident.Name != "Refusal" || len(spec.Names) != 1 || len(spec.Values) != 1 {
			return true
		}
		lit, ok := spec.Values[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		v, err := strconv.Unquote(lit.Value)
		if err != nil {
			t.Fatalf("%s: unquote %s: %v", spec.Names[0].Name, lit.Value, err)
		}
		declared[spec.Names[0].Name] = Refusal(v)
		return true
	})

	// The scan itself has to be load-bearing: an empty result would make every assertion below
	// vacuous, which is how a guard passes while checking nothing.
	if len(declared) < 2 {
		t.Fatalf("found %d Refusal constants in cost.go; the declaration scan is not working, so this test proves nothing", len(declared))
	}

	for name, r := range declared {
		expected, classified := want[name]
		if !classified {
			t.Errorf("%s (%q) is declared but not classified here: decide whether ImpossibleFigure covers it — the halves guard and the disclosure in settle.Settle both read that answer",
				name, r)
			continue
		}
		if got := r.ImpossibleFigure(); got != expected {
			t.Errorf("%s.ImpossibleFigure() = %v, want %v", name, got, expected)
		}
	}
	for name := range want {
		if _, ok := declared[name]; !ok {
			t.Errorf("%s is classified here but no longer declared in cost.go; a stale row hides the next unclassified one", name)
		}
	}
}
