package tui

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The key grammar: docs/superpowers/specs/2026-11-19-agentop-key-grammar-design.md.

// realGlobalKeys may act on every pane without checking it (rule 5).
var realGlobalKeys = []string{
	"q", "ctrl+c", "p", "?", "/", "n", "N", "$",
	"u", "T", "P", "A",
	"g", "G", "b", "f", "pgup", "pgdown", "pgdn",
	"esc", "left", "h", "enter", "right", "l",
}

// sharedPaneKeys are the letters that more than one pane's `?` table may list, each with
// the one meaning it keeps there.
var sharedPaneKeys = map[string]string{
	"h": "back on a list, the axis on a chart (rule 4)",
	"l": "open on a list, the axis on a chart (rule 4)",
	"j": "down", "k": "up",
	"r": "reload the agent list, on the two pickers",
	"x": "erase the row under the cursor",
	"S": "choose the inference server for the row",
	"w": "cycle the pane's window",
}

func parseTUI(t *testing.T) (*token.FileSet, map[string]*ast.FuncDecl) {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	funcs := map[string]*ast.FuncDecl{}
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, d := range f.Decls {
				if fd, ok := d.(*ast.FuncDecl); ok {
					funcs[fd.Name.Name] = fd
				}
			}
		}
	}
	return fset, funcs
}

// isKeySwitch reports whether s is `switch msg.String()` or `switch key` style: a switch on a
// call to String() or on an identifier named msg or key.
func isKeySwitch(s *ast.SwitchStmt) bool {
	switch tag := s.Tag.(type) {
	case *ast.CallExpr:
		sel, ok := tag.Fun.(*ast.SelectorExpr)
		return ok && sel.Sel.Name == "String"
	case *ast.Ident:
		return tag.Name == "msg" || tag.Name == "key"
	}
	return false
}

func caseKeys(cc *ast.CaseClause) []string {
	var out []string
	for _, e := range cc.List {
		if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if s, err := strconv.Unquote(lit.Value); err == nil {
				out = append(out, s)
			}
		}
	}
	return out
}

// keysHandledIn lists the case keys of every key switch below n.
func keysHandledIn(n ast.Node) []string {
	var out []string
	ast.Inspect(n, func(n ast.Node) bool {
		if s, ok := n.(*ast.SwitchStmt); ok && isKeySwitch(s) {
			for _, st := range s.Body.List {
				out = append(out, caseKeys(st.(*ast.CaseClause))...)
			}
		}
		return true
	})
	return out
}

func refersToMField(n ast.Node, field string) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == field {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "m" {
				found = true
			}
		}
		return !found
	})
	return found
}

func stringLits(n ast.Node) string {
	var b strings.Builder
	ast.Inspect(n, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			b.WriteString(lit.Value)
		}
		return true
	})
	return b.String()
}

// Rule 1: a letter that two panes' `?` tables list keeps one meaning. A new letter on a
// second pane must join sharedPaneKeys with that meaning.
func TestKeyGrammar_OneMeaningPerKey(t *testing.T) {
	panes := map[string][]string{} // letter -> "pane: desc"
	for _, g := range paneKeys {
		for _, kb := range g.bindings {
			// "c then s": the second key belongs to the modal the first one opens.
			first, _, _ := strings.Cut(kb.keys, " then ")
			for _, tok := range strings.FieldsFunc(first, func(r rune) bool { return r == ' ' || r == '/' }) {
				if len(tok) != 1 || !(tok[0] >= 'a' && tok[0] <= 'z' || tok[0] >= 'A' && tok[0] <= 'Z') {
					continue
				}
				if !slices.ContainsFunc(panes[tok], func(u string) bool { return strings.HasPrefix(u, g.title+":") }) {
					panes[tok] = append(panes[tok], g.title+": "+kb.desc)
				}
			}
		}
	}
	for k, uses := range panes {
		if len(uses) < 2 {
			continue
		}
		if _, ok := sharedPaneKeys[k]; !ok {
			t.Errorf("%q is listed on %d panes and is not in sharedPaneKeys:\n  %s",
				k, len(uses), strings.Join(uses, "\n  "))
		}
	}
}

// Rule 5: a case of handleKey's global switch acts on one pane's state only after it checks
// the pane, unless its key is a real global key.
func TestKeyGrammar_GlobalCasesCheckThePane(t *testing.T) {
	fset, funcs := parseTUI(t)
	hk := funcs["handleKey"]
	if hk == nil {
		t.Fatal("handleKey not found")
	}
	var global *ast.SwitchStmt
	for _, st := range hk.Body.List {
		if s, ok := st.(*ast.SwitchStmt); ok && isKeySwitch(s) {
			global = s
		}
	}
	if global == nil {
		t.Fatal("handleKey has no top-level key switch")
	}
	for _, st := range global.Body.List {
		cc := st.(*ast.CaseClause)
		keys := caseKeys(cc)
		allGlobal := len(keys) > 0
		for _, k := range keys {
			if !slices.Contains(realGlobalKeys, k) {
				allGlobal = false
			}
		}
		if allGlobal {
			continue
		}
		body := &ast.BlockStmt{List: cc.Body}
		if !refersToMField(body, "pane") {
			t.Errorf("%s: case %q acts without checking m.pane and is not a real global key",
				fset.Position(cc.Pos()), keys)
		}
	}
}

// Rule 2: a modal names in its hint line each key it gives a meaning of its own, and it never
// takes `x`, the erase key.
func TestKeyGrammar_ModalHintsNameTheirKeys(t *testing.T) {
	_, funcs := parseTUI(t)

	// The column picker's keys are a block of handleKey; find it by its condition.
	var picker ast.Node
	ast.Inspect(funcs["handleKey"], func(n ast.Node) bool {
		if is, ok := n.(*ast.IfStmt); ok && picker == nil {
			cond := is.Cond
			for {
				b, ok := cond.(*ast.BinaryExpr)
				if !ok {
					break
				}
				cond = b.X
			}
			if sel, ok := cond.(*ast.SelectorExpr); ok && sel.Sel.Name == "colPicker" {
				picker = is.Body
			}
		}
		return picker == nil
	})
	if picker == nil {
		t.Fatal("column picker block not found in handleKey")
	}

	modals := []struct {
		name   string
		keys   ast.Node
		hints  []string // functions whose string literals hold the hint line
		opener string   // the key that opened the modal also closes it
	}{
		{"column picker", picker, []string{"renderColumnPicker"}, "c"},
		{"erase confirm", funcs["clearConfirmKey"], []string{"confirmText"}, ""},
		{"edit overlay", funcs["handleEditKey"], []string{"renderEditOverlay"}, ""},
		{"server picker", funcs["serverPickerKey"], []string{"renderServerPicker", "renderScopeStep"}, ""},
	}
	// Moving and leaving work the same in every modal, so a hint line need not repeat them.
	common := []string{"up", "down", "k", "j", "home", "end", "g", "G", "esc", "q", "ctrl+c"}
	for _, md := range modals {
		if md.keys == nil {
			t.Fatalf("%s: key handler not found", md.name)
		}
		var hint strings.Builder
		for _, fn := range md.hints {
			if funcs[fn] == nil {
				t.Fatalf("%s: hint function %s not found", md.name, fn)
			}
			hint.WriteString(stringLits(funcs[fn]))
		}
		h := hint.String()
		for _, k := range keysHandledIn(md.keys) {
			if k == "x" {
				t.Errorf("%s takes `x`, the erase key", md.name)
			}
			if slices.Contains(common, k) || k == md.opener {
				continue
			}
			want := []string{"[" + strings.ToLower(k) + "]"}
			switch k {
			case " ":
				want = []string{"[space]"}
			case "enter":
				want = []string{"[enter]", "[↵]"}
			}
			if !slices.ContainsFunc(want, func(w string) bool { return strings.Contains(h, w) }) {
				t.Errorf("%s handles %q but its hint line does not name it (want one of %v)", md.name, k, want)
			}
		}
	}
}
