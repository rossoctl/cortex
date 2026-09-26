package parity

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// THE SAME DEFECT WAS FOUND AND FIXED FIVE TIMES, IN FIVE FILES, ACROSS FOUR ROUNDS OF REVIEW.
//
// Every time, the shape was identical: a dispatch that runs after a response is complete, handed
// the REQUEST's context. pipeline.RunResponse and RunResponseFrame refuse a done context before
// calling any plugin and return Deny("pipeline.cancelled"), which no call site can tell from a
// policy reject — so a client hanging up turned a complete response into a rejection, the cost
// never settled, and no session row was written. Each round fixed the instance it was shown and
// left the others, because nobody had the list.
//
// This is the list. Every dispatch site in every listener appears below, classified, and the test
// fails if a site exists that is not here — so a new one cannot be added without someone deciding
// which kind it is. Writing the sixth instance of the bug now requires writing down that it is
// finalization AND passing a context that is not, which is a harder thing to do by accident.
//
// WHAT THE TWO CLASSES MEAN:
//
//	finalization  The response is already complete or gone. The work is settling a cost,
//	              recording a row, finishing plugins. It MUST run on a context detached from the
//	              request and bounded — httpx.TeardownContext — because the request's own context
//	              is frequently already done at that point, and a plugin holding a detached
//	              context with no deadline holds it forever.
//	finalizationDelivered
//	              The same, AND the response has already gone downstream, so a plugin rejecting
//	              here cannot take effect. It must also call pipeline.Context.MarkResponseDelivered
//	              — otherwise OutcomeFromContext reports OutcomeDeny beside a 200 and every
//	              Finisher, audit row and dashboard reads a delivered response as a denial. This is
//	              the distinction the classes exist to force: "finalizing" and "already answered"
//	              are not the same moment, and the buffered arms are finalizing BEFORE the write.
//	inFlight      The caller is still waiting for this response. A reject can still change what
//	              they receive, and a cancelled context genuinely means stop. The live context is
//	              correct, and the teardown flush is the backstop for anything left unsettled.
//	inherited     A helper that takes its context as a parameter. Its class is its caller's — and the
//	              CALL is audited as a dispatch site in the caller's own function, because otherwise
//	              a helper hides its context's provenance one hop away. That hole was real: passing
//	              the response phase's context to dispatchTerminalFrame instead of a fresh one
//	              survived this test, in the very listener the rule was written for.
//
// CHECKED, NOT MERELY LISTED. For a finalization site the test walks the enclosing function and
// requires the context argument to be assigned from httpx.TeardownContext — the property that
// actually matters, not a promise in a comment.
type dispatchClass string

const (
	finalization          dispatchClass = "finalization"
	finalizationDelivered dispatchClass = "finalizationDelivered"
	inFlight              dispatchClass = "inFlight"
	inherited             dispatchClass = "inherited"
)

// dispatchSites is keyed by "<file>:<function>:<context argument>", which is stable under edits
// that move code around and unstable exactly when someone changes which context a dispatch uses —
// which is the change worth noticing.
var dispatchSites = map[string]dispatchClass{
	// The reverse proxy's buffered arm: the body is read whole before either dispatch, so both
	// are finalization. This was must-fix 2 of round 7 — the third instance of the bug.
	"reverseproxy/server.go:modifyResponse:phaseCtx": finalization,
	"reverseproxy/server.go:modifyResponse:finalCtx": finalization,
	// The streaming body's terminal dispatch, built lazily inside finalize() so the deadline
	// starts when the work does. Round 6's must-fix was that it did not.
	"reverseproxy/server.go:finalize:ctx": finalizationDelivered,
	// Mid-stream frames, while bytes are still going to a client who is there.
	"reverseproxy/server.go:Read:b.ctx": inFlight,

	// The forward proxy's PRIMARY buffered outbound path, where most non-streamed inference
	// responses go. Still on the request context after three rounds of fixing its siblings; found
	// by building this table, which is the argument for having one.
	"forwardproxy/server.go:serveOutbound:phaseCtx": finalization,
	"forwardproxy/server.go:serveOutbound:finalCtx": finalization,
	// The real streaming path's finish defer — the first instance ever fixed.
	"forwardproxy/server.go:handleStreamingResponse:finalCtx": finalizationDelivered,
	// Mid-stream frames on that same path.
	"forwardproxy/server.go:handleStreamingResponse:r.Context()": inFlight,
	// Passthrough runs the response phase BEFORE any byte is written, so a reject still reaches
	// the client and cancellation still means stop. No cost owner is on this path: it is taken
	// only when no StreamingResponder is configured.
	"forwardproxy/server.go:streamPassthrough:r.Context()": inFlight,
	// The buffered fallback: phase, fold and settle, each after io.ReadAll. Three separate
	// contexts because each is its own bounded piece of work.
	"forwardproxy/server.go:streamFallbackBuffered:phaseCtx": finalization,
	"forwardproxy/server.go:streamFallbackBuffered:foldCtx":  finalization,
	"forwardproxy/server.go:streamFallbackBuffered:finalCtx": finalization,

	// ext_proc's teardown flush: the stream is gone by definition here, and its two dispatches take
	// separate budgets. The second goes through dispatchTerminalFrame, and the CALL is audited here
	// — the helper's own entry below covers what it does with whatever it is handed.
	"extproc/server.go:Process:phaseCtx": finalizationDelivered,
	"extproc/server.go:Process:finalCtx": finalizationDelivered,
	// Envoy is holding the response and waiting for our reply on a live stream: a reject becomes
	// an ImmediateResponse, so these are in-flight even though one of them is a terminal frame.
	// Anything left unsettled when the stream dies is picked up by the flush above.
	"extproc/server.go:handleResponseHeaders:ctx": inFlight,
	"extproc/server.go:handleResponseBody:ctx":    inFlight,
	// Helpers. Called from the body phase with the live context and from the flush with the
	// detached one, so their class is whichever caller reached them.
	"extproc/server.go:dispatchTerminalFrame:ctx":  inherited,
	"extproc/server.go:dispatchBufferedFrames:ctx": inherited,
}

// listenerPackages are the packages this table covers. Named rather than discovered, so adding a
// listener is a deliberate addition here — but every .go file INSIDE them is globbed, which is the
// difference between an audit and a list.
//
// Naming files was a blind spot with a proof: a new forwardproxy/zz_blindspot.go containing exactly
// the defect this table exists for — a finalization dispatch on r.Context() — left the audit green,
// because the file was not one of the three it looked at. The bug it guards against arrives in new
// code, so the check has to look at code that is new.
var listenerPackages = []string{"reverseproxy", "forwardproxy", "extproc"}

func TestEveryResponseDispatchSiteIsClassified(t *testing.T) {
	seen := map[string]bool{}
	for _, rel := range sourceFiles(t) {
		path := filepath.Join("..", rel)
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		for _, site := range dispatchesIn(fset, file) {
			key := fmt.Sprintf("%s:%s:%s", rel, site.function, site.ctxArg)
			class, ok := dispatchSites[key]
			if !ok {
				t.Errorf(`unclassified response dispatch at %s (%s)

  add it to dispatchSites as:
      %q: finalization,   // or finalizationDelivered, inFlight, inherited

  finalization  the response is complete or gone; settle, record, finish. MUST take a context
                from httpx.TeardownContext — the request's own is usually already done, and
                RunResponse refuses a done context before calling any plugin, so the charge is
                silently dropped and no row is written.
  inFlight      the caller is still waiting; a reject can still change what they get.
  inherited     a helper taking its context as a parameter; its callers carry the class.

  Getting this wrong is the bug this table exists for: it was found and fixed five times, in
  five files, before anyone wrote the list.`, fset.Position(site.pos), site.call, key)
				continue
			}
			seen[key] = true
			if class == finalizationDelivered && !marksDelivered(site.fn) {
				t.Errorf(`%s at %s is classified %s but its function never calls MarkResponseDelivered

  The response has already gone downstream at this dispatch, so a plugin rejecting here cannot
  take effect — and OutcomeFromContext maps any deny to OutcomeDeny, so every Finisher would read
  a request answered with a 200 as denied. Either call pctx.MarkResponseDelivered() before the
  dispatch, or reclassify: a site finalizing BEFORE the write is plain finalization, because a
  reject there still changes what the client receives.`,
					key, fset.Position(site.pos), class)
			}
			if (class == finalization || class == finalizationDelivered) && !fromTeardownContext(site.fn, site.ctxArg) {
				t.Errorf(`%s at %s is classified %s but its context is not from httpx.TeardownContext

  got: %s

  A finalization dispatch on the request's context is a no-op whenever the client has gone:
  RunResponse returns Deny("pipeline.cancelled") before any plugin runs, the cost does not
  settle, and the call site reads that Deny as a policy reject. Either take the context from
  httpx.TeardownContext, or reclassify this site as inFlight and say why.`,
					key, fset.Position(site.pos), class, site.call)
			}
		}
	}
	// NO TWO DISPATCHES MAY SHARE ONE FINALIZATION CONTEXT, which is the rule the sites disagreed
	// about until #1037's review: shared at two of them, split at a third, each written as a general
	// rule, and nothing failed when the split was collapsed. Per-dispatch is the answer, because the
	// dispatches run in order and a shared budget lets the response phase spend what the terminal
	// frame — the one that settles a figure — needs. Checked here because it is a property of the
	// SET of sites, which no single site's test can see.
	byCtx := map[string][]string{}
	for key, class := range dispatchSites {
		if class != finalization && class != finalizationDelivered {
			continue
		}
		parts := strings.Split(key, ":")
		if len(parts) != 3 {
			continue
		}
		fnCtx := parts[0] + ":" + parts[1] + ":" + parts[2]
		byCtx[fnCtx] = append(byCtx[fnCtx], key)
	}
	// The table keys already include the context expression, so two dispatches sharing one context in
	// one function collapse to a single key — which is why the count of finalization KEYS per
	// (file, function) is what reveals sharing. Counted from the source instead:
	perCtx := map[string]int{}
	for _, rel := range sourceFiles(t) {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, filepath.Join("..", rel), nil, 0)
		if err != nil {
			continue
		}
		for _, site := range dispatchesIn(fset, file) {
			key := fmt.Sprintf("%s:%s:%s", rel, site.function, site.ctxArg)
			if class := dispatchSites[key]; class == finalization || class == finalizationDelivered {
				perCtx[key]++
			}
		}
	}
	for key, n := range perCtx {
		if n > 1 {
			t.Errorf(`%d finalization dispatches share the context %q

  They run in order, so whatever the earlier one spends is taken from the later — and the later one is
  the terminal frame, which is what turns folded state into a settled figure. Give each dispatch its
  own httpx.TeardownContext; the total stays bounded because the number per response is fixed.`,
				n, key)
		}
	}

	// A TABLE ENTRY THAT MATCHES NOTHING IS AS BAD AS A MISSING ONE: it reads as coverage that
	// does not exist, and it is what a table looks like after the code it described moved.
	var stale []string
	for key := range dispatchSites {
		if !seen[key] {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale)
	for _, key := range stale {
		t.Errorf("dispatchSites has %q, which matches no dispatch in the source: remove it, or fix the key — a table entry for code that no longer exists is a claim nothing checks", key)
	}
}

// sourceFiles lists every non-test .go file in the covered packages, and fails when a package
// contributes none — an empty glob is how a path change turns this audit into a no-op that reports
// success.
func sourceFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, pkg := range listenerPackages {
		matches, err := filepath.Glob(filepath.Join("..", pkg, "*.go"))
		if err != nil {
			t.Fatalf("glob %s: %v", pkg, err)
		}
		found := 0
		for _, m := range matches {
			if strings.HasSuffix(m, "_test.go") {
				continue
			}
			out = append(out, filepath.Join(pkg, filepath.Base(m)))
			found++
		}
		if found == 0 {
			t.Fatalf("package %q contributed no source files: this audit would then pass over nothing", pkg)
		}
	}
	sort.Strings(out)
	return out
}

// inheritedHelpers are the functions classified inherited, derived from the table so the two cannot
// drift. A CALL to one of these is audited exactly like a direct dispatch: it takes a context and
// hands it to a dispatch one hop down, so its provenance is the caller's business.
func inheritedHelpers() map[string]bool {
	out := map[string]bool{}
	for key, class := range dispatchSites {
		if class != inherited {
			continue
		}
		if parts := strings.Split(key, ":"); len(parts) == 3 {
			out[parts[1]] = true
		}
	}
	return out
}

// dispatchName returns the name to audit this call under — the pipeline method for a direct
// dispatch, or the helper's own name for a call to an inherited one — and "" for anything else.
func dispatchName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		if fun.Sel.Name == "RunResponse" || fun.Sel.Name == "RunResponseFrame" {
			return fun.Sel.Name
		}
	case *ast.Ident:
		if inheritedHelpers()[fun.Name] {
			return fun.Name
		}
	}
	return ""
}

type dispatchSite struct {
	function string
	ctxArg   string
	call     string
	pos      token.Pos
	fn       *ast.FuncDecl
}

// dispatchesIn finds every call to RunResponse or RunResponseFrame, whatever the receiver, with
// the enclosing function and the source text of the context argument.
func dispatchesIn(fset *token.FileSet, file *ast.File) []dispatchSite {
	var out []dispatchSite
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := dispatchName(call)
			if name == "" || len(call.Args) == 0 {
				return true
			}
			out = append(out, dispatchSite{
				function: fn.Name.Name,
				ctxArg:   exprSource(fset, call.Args[0]),
				call:     exprSource(fset, call),
				pos:      call.Pos(),
				fn:       fn,
			})
			return true
		})
	}
	return out
}

// marksDelivered reports whether fn calls MarkResponseDelivered on anything.
//
// Any receiver, deliberately: the pctx reaches these functions as a field, a parameter or a local
// depending on the listener, and what matters is that the statement is there before the dispatch.
func marksDelivered(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "MarkResponseDelivered" {
			found = true
		}
		return true
	})
	return found
}

// fromTeardownContext reports whether name is assigned from httpx.TeardownContext anywhere in fn.
//
// An assignment check rather than dataflow, which is the honest limit of this test: it proves the
// function builds a teardown context and names it this, not that no later statement reassigns it.
// That is enough for the mistake it guards — passing the request's context to finalization — and
// its failure mode is a false PASS on deliberately convoluted code, never a false alarm.
func fromTeardownContext(fn *ast.FuncDecl, name string) bool {
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) == 0 {
			return true
		}
		if id, ok := assign.Lhs[0].(*ast.Ident); !ok || id.Name != name {
			return true
		}
		for _, rhs := range assign.Rhs {
			call, ok := rhs.(*ast.CallExpr)
			if !ok {
				continue
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "TeardownContext" {
				found = true
			}
		}
		return true
	})
	return found
}

func exprSource(fset *token.FileSet, e ast.Expr) string {
	var buf strings.Builder
	if err := printer.Fprint(&buf, fset, e); err != nil {
		return "<unprintable>"
	}
	return strings.Join(strings.Fields(buf.String()), " ")
}
