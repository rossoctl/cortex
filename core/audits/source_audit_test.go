// Package audits holds source-scanning tests: they parse the tree's non-test
// Go files and fail on patterns that have produced bugs more than once, so the
// next copy cannot arrive silently. Parser-only — no type information — which
// is the honest limit of each check and the reason every rule is textual.
package audits

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// scannedRoots are the subtrees these audits walk. The whole of core and cmd,
// modules and all, because the bug these guard against arrives in new code and
// does not respect module boundaries — parsing is path-based and needs none of
// them. Skipped: _test.go files, and anything under a directory starting with
// "." (worktrees).
var scannedRoots = []string{"../../core", "../../cmd"}

// sourceFiles walks the roots and returns every non-test .go file, relative to
// this package. An empty walk fails loudly: it is how a path change turns an
// audit into a no-op that reports success.
func sourceFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, root := range scannedRoots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if strings.HasPrefix(d.Name(), ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
				out = append(out, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no source files under %v: these audits would pass over nothing", scannedRoots)
	}
	sort.Strings(out)
	return out
}

// parseAll parses every file from sourceFiles, once, for the tests that share
// it, and returns the fileset their positions refer to.
func parseAll(t *testing.T) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for _, path := range sourceFiles(t) {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		files[path] = file
	}
	return fset, files
}

// relPath renders a path relative to the repo root, the form the allowlists key
// on — stable no matter which module the test runs from. ToSlash first, so the
// "../../" prefix also matches the separator WalkDir uses on Windows.
func relPath(path string) string {
	return strings.TrimPrefix(filepath.ToSlash(path), "../../")
}

// ---------------------------------------------------------------- transport

// Go's http.Transport keeps 2 idle connections per host unless told otherwise,
// so a concurrent caller over HTTP/1.1 pays a fresh TCP+TLS handshake for every
// request beyond the second — 10 concurrent callers to one gateway opened 36–45
// connections (#1546). Every transport that serves ongoing traffic must raise
// the pool; the exceptions are one-shot fetches, and each is listed here with
// its reason so an exemption is a decision someone wrote down.
//
// The audit sees Transport composite literals only. The same default reaches
// the wire through two shapes it does not: an http.Client built without a
// Transport (it uses DefaultTransport as-is) and a DefaultTransport Clone
// without MaxIdleConnsPerHost set — today the loopback-only clients in
// apiclient and local_endpoint, where 2 per host is harmless.
var transportsWithoutIdlePool = map[string]string{
	// A single model-list fetch per refresh; no request ever reuses the pool.
	"core/plugins/inferencerouter/routerconfig/models.go:NewModelsClient": "one-shot fetch; the pool is never reused",
}

func TestTransportsSetIdleConnectionPool(t *testing.T) {
	seen := map[string]bool{}
	fset, files := parseAll(t)
	for path, parsed := range files {
		// The whole file, not just function bodies: a transport built in a
		// package-level var is as live as one built in a function, and looking
		// only at FuncDecls would not see it.
		for _, scope := range scopes(parsed) {
			ast.Inspect(scope.node, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok || !isTransport(lit.Type) {
					return true
				}
				if compositeKeySet(lit, "MaxIdleConnsPerHost") {
					return true
				}
				key := fmt.Sprintf("%s:%s", relPath(path), scope.name)
				if _, ok := transportsWithoutIdlePool[key]; ok {
					seen[key] = true
					return true
				}
				t.Errorf(`%s builds an http.Transport without MaxIdleConnsPerHost

  Go keeps 2 idle connections per host by default, so concurrent requests over
  HTTP/1.1 pay a new TCP+TLS handshake each — the pool churn #1546 measured.
  Set MaxIdleConnsPerHost on this transport, or add it to
  transportsWithoutIdlePool with the reason it is exempt.`,
					fset.Position(lit.Pos()))
				return true
			})
		}
	}
	assertNoStaleEntries(t, "transportsWithoutIdlePool", transportsWithoutIdlePool, seen)
}

// isTransport reports whether e is an http.Transport composite literal type.
// The package is checked too, so an http2.Transport literal — a type without
// MaxIdleConnsPerHost — stays outside the rule.
func isTransport(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "http" && sel.Sel.Name == "Transport"
}

// ------------------------------------------------------------------ rename

// Renaming a temp file over its destination without f.Sync() first can leave
// the destination EMPTY after a power loss — the very outcome the rename was
// meant to rule out (#1547). The rule covers a function that creates or writes
// the file it renames; pure renames (rotations, trash moves) are outside it,
// because the bytes they move were synced where they were written. The
// exemptions are files that are rebuilt from another source on next use, where
// a lost write costs a rebuild and nothing else.
var renamesWithoutSync = map[string]string{
	// A restart re-downloads the list when the cache is unreadable.
	"core/cost/pricing/pricelist/pricelist.go:save": "cache; re-downloaded on a bad read",
	// Rewritten from the embedded source on the next start that needs it.
	"core/plugins/script/sdk.go:installSDK": "embedded source; rewritten on next start",
	// The rename moves a day file the ledger's writer already synced; the
	// append path below it syncs its own output.
	"core/cost/ledger/store.go:mergeDayFiles": "moves a file synced where it was written",
}

func TestRenamesSyncFirst(t *testing.T) {
	seen := map[string]bool{}
	fset, files := parseAll(t)
	for path, parsed := range files {
		for _, fn := range functions(parsed) {
			if !callsSelector(fn, "os", "Rename") || !writesTempFile(fn) || renamesSynced(fset, fn) {
				continue
			}
			key := fmt.Sprintf("%s:%s", relPath(path), fn.Name.Name)
			if _, ok := renamesWithoutSync[key]; ok {
				seen[key] = true
				continue
			}
			t.Errorf(`%s renames a file it did not Sync

  Without f.Sync() between the write and os.Rename, a power loss can leave the
  renamed file empty on disk (#1547). Sync the temp file before the rename, or
  route the write through a helper that does (writeFileAtomic), or add the
  function to renamesWithoutSync with the reason a lost write costs nothing.`,
				fmt.Sprintf("%s:%s", fset.Position(fn.Pos()), fn.Name.Name))
		}
	}
	assertNoStaleEntries(t, "renamesWithoutSync", renamesWithoutSync, seen)
}

// ---------------------------------------------------------------- helpers

// functions returns every function declaration in file, methods included.
func functions(file *ast.File) []*ast.FuncDecl {
	var out []*ast.FuncDecl
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			out = append(out, fn)
		}
	}
	return out
}

// scope is one naming unit for an allowlist key: a function, or the package
// scope for declarations outside any function.
type scope struct {
	name string
	node ast.Node
}

// scopes returns every place a declaration can live: each function under its
// own name, and each non-function declaration (a package-level var, const or
// type) under "package-scope". Walking the declarations rather than the file
// keeps the two from overlapping — Inspect on the file would descend into
// function bodies and key their literals as package-scope too.
func scopes(file *ast.File) []scope {
	var out []scope
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			out = append(out, scope{name: fn.Name.Name, node: fn})
			continue
		}
		out = append(out, scope{name: "package-scope", node: decl})
	}
	return out
}

// callsSelector reports whether fn contains a <pkg>.<name> call; "*" matches
// any package.
func callsSelector(fn *ast.FuncDecl, pkg, name string) bool {
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != name {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && (pkg == "*" || id.Name == pkg) {
			found = true
		}
		return true
	})
	return found
}

// writesTempFile reports whether fn creates or writes the file it goes on to
// rename — the shape the rule is about: every spelling the tree uses for
// making the temp file.
func writesTempFile(fn *ast.FuncDecl) bool {
	for _, name := range []string{"CreateTemp", "WriteFile", "Create", "OpenFile"} {
		if callsSelector(fn, "os", name) {
			return true
		}
	}
	return false
}

// renamesSynced reports whether every os.Rename in fn is preceded by a sync of
// the file it renames. A sync counts when it appears before the rename and its
// subject — the receiver of .Sync(), or the argument of a seam like syncFile —
// denotes the renamed path in one of the shapes the tree uses: the path
// itself, <file>.Name(), an ident assigned from <file>.Name(), or a file
// variable opened from the path. That is the honest limit of a parser-only
// audit: it cannot prove the synced file is the temp file, but it refuses the
// two easy mistakes — a sync of some other file, and a sync placed after the
// rename.
func renamesSynced(fset *token.FileSet, fn *ast.FuncDecl) bool {
	renames := selectorCalls(fn, "os", "Rename")
	if len(renames) == 0 {
		return true
	}
	syncs := syncSubjects(fset, fn)
	if len(syncs) == 0 {
		return false
	}
	nameOf, openedFrom := fileVars(fset, fn)
	for _, r := range renames {
		if len(r.args) == 0 {
			continue
		}
		src := exprSource(fset, r.args[0])
		ok := false
		for _, s := range syncs {
			if s.pos >= r.pos {
				continue
			}
			if src == s.subject || src == s.subject+".Name()" ||
				nameOf[src] == s.subject || openedFrom[s.subject] == src {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// callSite is a call with its position and arguments.
type callSite struct {
	pos  token.Pos
	args []ast.Expr
}

// selectorCalls returns every <pkg>.<name> call in fn.
func selectorCalls(fn *ast.FuncDecl, pkg, name string) []callSite {
	var out []callSite
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != name {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == pkg {
			out = append(out, callSite{pos: call.Pos(), args: call.Args})
		}
		return true
	})
	return out
}

// syncSubject is one sync in a function: where it sits, and the source text of
// what it syncs.
type syncSubject struct {
	pos     token.Pos
	subject string
}

// syncSubjects returns every sync in fn — a .Sync() method call or a call to a
// seam like syncFile — as the expression it syncs.
func syncSubjects(fset *token.FileSet, fn *ast.FuncDecl) []syncSubject {
	var out []syncSubject
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.SelectorExpr:
			if fun.Sel.Name == "Sync" {
				out = append(out, syncSubject{pos: call.Pos(), subject: exprSource(fset, fun.X)})
			}
		case *ast.Ident:
			if strings.Contains(strings.ToLower(fun.Name), "sync") && len(call.Args) > 0 {
				out = append(out, syncSubject{pos: call.Pos(), subject: exprSource(fset, call.Args[0])})
			}
		}
		return true
	})
	return out
}

// fileVars maps identifiers to the paths they denote, for matching a sync's
// subject against a rename's source: nameOf[X] = F when X was assigned
// F.Name(); openedFrom[F] = P when F was assigned from a call whose first
// argument is the path P (os.OpenFile, os.Create, or a local seam like the
// harvest tests' atPath).
func fileVars(fset *token.FileSet, fn *ast.FuncDecl) (nameOf, openedFrom map[string]string) {
	nameOf, openedFrom = map[string]string{}, map[string]string{}
	ast.Inspect(fn, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) == 0 || len(assign.Rhs) == 0 {
			return true
		}
		id, ok := assign.Lhs[0].(*ast.Ident)
		if !ok {
			return true
		}
		switch rhs := assign.Rhs[0].(type) {
		case *ast.CallExpr: // X := F.Name(), or F := open(P)
			if sel, ok := rhs.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Name" {
				nameOf[id.Name] = exprSource(fset, sel.X)
				break
			}
			if len(rhs.Args) > 0 {
				openedFrom[id.Name] = exprSource(fset, rhs.Args[0])
			}
		}
		return true
	})
	return nameOf, openedFrom
}

// compositeKeySet reports whether lit carries a keyed field named key.
func compositeKeySet(lit *ast.CompositeLit, key string) bool {
	for _, elt := range lit.Elts {
		if kv, ok := elt.(*ast.KeyValueExpr); ok {
			if id, ok := kv.Key.(*ast.Ident); ok && id.Name == key {
				return true
			}
		}
	}
	return false
}

// exprSource renders e as normalized source text, for comparing expressions by
// shape.
func exprSource(fset *token.FileSet, e ast.Expr) string {
	var buf strings.Builder
	if err := printer.Fprint(&buf, fset, e); err != nil {
		return "<unprintable>"
	}
	return strings.Join(strings.Fields(buf.String()), " ")
}

// assertNoStaleEntries fails for allowlist entries that matched nothing: an
// entry for code that no longer exists reads as coverage nothing checks.
func assertNoStaleEntries(t *testing.T, table string, allowlist map[string]string, seen map[string]bool) {
	t.Helper()
	var stale []string
	for key := range allowlist {
		if !seen[key] {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale)
	for _, key := range stale {
		t.Errorf("%s has %q, which matches nothing in the source: remove it, or fix the key", table, key)
	}
}
