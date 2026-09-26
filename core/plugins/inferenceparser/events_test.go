package inferenceparser

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// switchCaseTypes extracts the string literals from the case clauses of
// foldAnthropicFrame's `switch ev.Type` in anthropic.go.
//
// Parsed from source rather than hand-listed on purpose: a hardcoded list only
// catches drift when the author edits the list, which is the same edit that
// would have fixed the map — so it catches nothing. Reading the switch itself
// means adding a case with no map entry fails here without anyone remembering
// to update a test.
func switchCaseTypes(t *testing.T) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "anthropic.go", nil, 0)
	if err != nil {
		t.Fatalf("parse anthropic.go: %v", err)
	}

	got := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "foldAnthropicFrame" {
			return true
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			sw, ok := n.(*ast.SwitchStmt)
			if !ok {
				return true
			}
			// Only the switch on ev.Type; the function also switches on
			// ev.Delta.Type for the delta kinds.
			sel, ok := sw.Tag.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Type" {
				return true
			}
			if ident, ok := sel.X.(*ast.Ident); !ok || ident.Name != "ev" {
				return true
			}
			for _, stmt := range sw.Body.List {
				cc, ok := stmt.(*ast.CaseClause)
				if !ok {
					continue
				}
				for _, expr := range cc.List {
					lit, ok := expr.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					got[lit.Value[1:len(lit.Value)-1]] = true // strip quotes
				}
			}
			return false
		})
		return false
	})

	if len(got) == 0 {
		t.Fatal("found no case clauses on foldAnthropicFrame's switch ev.Type — test is not looking where it thinks")
	}
	return got
}

// Every event type the switch handles must be in knownAnthropicEvents, or a
// healthy stream logs a spurious "unrecognized event type" line for an event
// the parser demonstrably understands.
func TestKnownAnthropicEvents_CoversSwitchCases(t *testing.T) {
	for typ := range switchCaseTypes(t) {
		if !knownAnthropicEvents[typ] {
			t.Errorf("foldAnthropicFrame handles %q but knownAnthropicEvents omits it — a handled event would log as unrecognized", typ)
		}
	}
}

// The reverse direction, which a one-way check misses: every map entry must
// either have a case in the switch or be named in intentionallyIgnored. Without
// this, a type can sit in the map with no case and be silently dropped —
// suppressed from the very Debug log meant to make that loss visible.
func TestKnownAnthropicEvents_NoUnhandledEntries(t *testing.T) {
	cases := switchCaseTypes(t)
	for typ := range knownAnthropicEvents {
		if cases[typ] || intentionallyIgnored[typ] {
			continue
		}
		t.Errorf("knownAnthropicEvents has %q with no case in foldAnthropicFrame and no entry in intentionallyIgnored — "+
			"it would be silently dropped AND suppressed from the unrecognized-type log", typ)
	}
}

// intentionallyIgnored must name only types that really have no case: an entry
// that also appears in the switch means the two have drifted and the exemption
// is now hiding a real handler.
func TestIntentionallyIgnored_HasNoSwitchCase(t *testing.T) {
	cases := switchCaseTypes(t)
	for typ := range intentionallyIgnored {
		if cases[typ] {
			t.Errorf("%q is listed as intentionally ignored but foldAnthropicFrame has a case for it", typ)
		}
		if !knownAnthropicEvents[typ] {
			t.Errorf("%q is intentionally ignored but missing from knownAnthropicEvents, so it logs as unrecognized", typ)
		}
	}
}
