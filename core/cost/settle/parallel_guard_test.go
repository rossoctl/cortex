package settle

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// NO TEST IN THIS PACKAGE MAY CALL t.Parallel, AND THAT IS CHECKED RATHER THAN ASKED.
//
// implausible_test.go resets the package-level implausibleWarnOnce and replaces slog.Default()
// so it can observe the single operator warning. Both are process-wide, so a parallel test in
// this package would see another test's logger, or lose its own warning to another test's Once
// — the kind of failure that appears as a flake in whichever test happened to run second.
//
// A comment saying "do not add t.Parallel here" is exactly the instruction that stops being
// true. This is the same instruction, in a form that fails. It IS a lint and not a concurrency
// test: it runs no goroutines and proves nothing about the code under test, only about the
// suite. Naming that plainly matters, because a reader who took it for a race check would
// believe this package's shared state had been exercised concurrently, and nothing here does
// that — the settle latch is an ordinary map write, safe only because no listener dispatches
// two terminal frames for one response at the same time.
//
// PARSED, NOT GREPPED. Searching the bytes for "t.Parallel(" also matches the string in this
// comment, a t.Log about parallelism, or a line someone commented out to fix exactly this
// failure — so the text form both false-positives and, worse, teaches that commenting the call
// out is enough. The AST sees calls.
func TestNoTestInThisPackageRunsInParallel(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	inspected := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, "_test.go") || name == "parallel_guard_test.go" {
			continue
		}
		// Comments are not requested, so a mention of the call in prose cannot reach the walk
		// below even in principle.
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		inspected++
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Parallel" {
				return true
			}
			// Any receiver, deliberately: t, subtests' own t, and a helper's *testing.T
			// parameter under whatever name all mean the same thing here.
			t.Errorf("%s calls %s.Parallel at %s, which is unsafe while this package's tests swap "+
				"implausibleWarnOnce and slog.Default(). Either drop the parallel call or make "+
				"those two injectable first — see the comment on implausibleWarnOnce.",
				name, exprText(sel.X), fset.Position(call.Pos()))
			return true
		})
	}
	// A GUARD THAT INSPECTED NOTHING PASSES, which is the failure mode of every check that
	// selects its own inputs. This one skips by FILENAME — its own file, and anything not
	// ending in _test.go — so a rename, a move, or a package split that leaves this file alone
	// would make it green over zero files and stay green forever.
	if inspected == 0 {
		t.Fatal("no test files inspected: this guard selects its inputs by filename, so it reports success on an empty set")
	}
}

// exprText names the receiver of the offending call for the failure message. Only the simple
// shapes are spelled out, because the message needs to identify the line, not to reproduce it —
// fset.Position already says exactly where to look.
func exprText(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return "<expr>"
}
