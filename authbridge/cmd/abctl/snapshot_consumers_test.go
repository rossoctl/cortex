package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// This file is the answer to a defect that shipped FOUR TIMES on this branch: a field
// populated server-side, carried on the wire, and read by no client in abctl. Every
// instance was a money disclosure that silently did nothing.
//
//   - usage.Snapshot.Degraded — the ledger's own admission that rows are MISSING from the
//     total. Caught in review.
//   - usage.Snapshot.IncompleteBy — WHICH WAY an inexact figure is inexact, so "at least
//     $12.40" could stop being rendered as "roughly $12.40". Caught in review.
//   - the per-tier token split on usage.Counts (InputTokens, CacheReadTokens,
//     CacheWriteTokens, OutputTokens) — aggregated, then shown nowhere.
//   - usage.Snapshot.UngroupedCostMicros — the spend no series row carries, which is why a
//     breakdown summed to less than the total printed above it.
//
// Reviewers caught two of the four. Review is the wrong mechanism: the failure is
// invisible in a diff that only ADDS a producer, and the cost is borne by an operator who
// reads a confident number instead of a qualified one.
//
// WHAT THESE TESTS PROVE, AND WHAT THEY DO NOT. They prove a field is READ — that some
// non-test file under cmd/abctl contains a selector expression naming it. They do NOT
// prove it is rendered, rendered correctly, or rendered on the surface that needed it. A
// field read into a variable and dropped passes. A field printed with the wrong sign
// passes. A field read only by the TUI when the CLI also needed it passes, deliberately:
// see snapshotFieldExceptions on why "consumed somewhere in cmd/abctl" is the right bar.
// Correctness is what the per-surface tests in cmd_cost_test.go and tui/cost_pane_test.go
// are for; this is the cheaper claim that nothing was forgotten entirely, made by a check
// that cannot be forgotten to update.
//
// The enumeration is by REFLECTION, so a field added tomorrow is covered without anyone
// remembering this file exists. The matching is by AST, not grep: a selector expression is
// code, so a field named only in a comment or a string literal does not count — which
// matters here, because every one of the four defects above was heavily discussed in
// comments while nothing read it.

// snapshotBaseHint is what makes the Snapshot check specific rather than vacuous.
//
// The check cannot see TYPES — resolving them would mean type-checking the module from a
// test, which is a build dependency and a startup cost this does not want — so it matches
// the selector's BASE by name: `snap.Window`, `m.spend.snap.Buckets`, `todaySnap.Totals`.
// Without that, `cs.Window` (a CostSettings field of the same name, in tui/settings.go)
// would satisfy Snapshot.Window and the check would pass on fields nothing reads.
// CostSettings really does carry both a Window and a Group, so this is the difference
// between a guard and a placebo.
//
// The cost is a FALSE MISS if a future author names the variable something else — the test
// fails, loudly, and its message says to add the name here. That direction is the safe one:
// a false miss is one line of maintenance, a false pass is the defect this file exists to
// catch. Every snapshot-typed declaration in cmd/abctl today satisfies it.
const snapshotBaseHint = "snap"

// snapshotFieldExceptions are the fields no surface in this binary can act on, each with
// the reason it cannot. NOT a blanket skip: an entry is a claim, and the test below fails
// if one names a field that no longer exists or a field that has since found a reader.
//
// The bar is "consumed SOMEWHERE in cmd/abctl", not "consumed by every surface". The
// binary has four money surfaces with different amounts of room — the Cost pane, the spend
// strip, the sessions table and `abctl cost` — and demanding all of them would fail on
// fields that are genuinely void for one of them: `abctl cost` requests resolution 0,
// session "" and group none and prints a single total, so a breakdown-shaped field has
// nothing to do there and saying so with a per-surface exception list would be a table
// nobody could keep true.
//
// Note what is NOT here. Buckets is read (tui/cost_pane.go's costSeries, the Usage pane's
// charts, the strip's per-session sum), so it needs no exception; an earlier audit listed
// it, and listing a field that has a reader is how an exception table starts covering for
// a real gap.
var snapshotFieldExceptions = map[string]string{
	"Session": "An ECHO of the request, not a disclosure. Every caller in cmd/abctl passes " +
		"the session id itself — \"\" for all-sessions on the Cost pane, the strip and `abctl " +
		"cost` — so reading it back could only confirm what this process already decided. " +
		"Contrast Window, which is NOT an echo: a proxy with no ledger answers window=today " +
		"from the ring's longest span and reports THAT, which is why every surface reads it. " +
		"A client that ever rendered a snapshot it did not request (a saved response, or a " +
		"picker fed by the server) would need this.",
	"BucketSeconds": "The NEGOTIATED resolution, and nothing here labels a time axis or " +
		"divides len(Buckets) into the window. The Cost pane and `abctl cost` ask for " +
		"resolution 0 and read Totals; the Usage pane and the spend strip walk every bucket " +
		"positionally and name no span. Even the one rate on screen avoids it: spend.go " +
		"derives BurnPerMin from the parsed Snapshot.Window, because a per-minute figure " +
		"divides by the WINDOW rather than by a bucket. It qualifies the SHAPE of an answer, " +
		"never the size of a figure, so it is not the disclosure class this file guards.",
	"Group": "The axis ECHOED back, and the Cost pane deliberately prefers the axis it " +
		"REQUESTED — see costBreakdownSection's first comment. The echo can come back as the " +
		"GroupMethod alias, which would head the model series \"BY METHOD\"; and " +
		"beginCostFetch clears the snapshot on every view change, so the requested axis and " +
		"the served one cannot disagree. Reading the echo would be strictly worse than " +
		"ignoring it. Also not a money disclosure: it says which question was answered, not " +
		"how much anything cost.",
}

// TestUsageSnapshot_EveryFieldHasAConsumerInAbctl is the guard.
func TestUsageSnapshot_EveryFieldHasAConsumerInAbctl(t *testing.T) {
	read := snapshotScopedReads(t)
	typ := reflect.TypeOf(usage.Snapshot{})
	if typ.NumField() == 0 {
		t.Fatal("usage.Snapshot has no fields; this check would pass vacuously")
	}
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if reason, isException := snapshotFieldExceptions[name]; isException {
			// An exception that has come true is an exception to delete. Left behind, it would
			// go on excusing a field that no longer needs excusing, and the next reader would
			// trust the reason instead of the code.
			if read[name] {
				t.Errorf("usage.Snapshot.%s now has a reader in cmd/abctl, so its entry in "+
					"snapshotFieldExceptions is stale — delete it. The reason it recorded was: %s",
					name, reason)
			}
			continue
		}
		if read[name] {
			continue
		}
		t.Errorf(`usage.Snapshot.%s is populated by the server and read by nothing in cmd/abctl.

A SERVER-POPULATED DISCLOSURE WITH NO CLIENT IS THE DEFECT — not a missing nicety. The
producer computes it, the wire carries it, and the operator sees a figure that looks
complete while the thing qualifying it is thrown away at the last step. Three shipped that
way on this branch before this test existed, and each fix was the same shape:
Snapshot.Degraded (rows missing from the total), Snapshot.IncompleteBy (which way a figure
is inexact) and the per-tier token split on usage.Counts.

Do one of these:
  - read it where a reader can act on it — tui/cost_pane.go for the pane (costTotalBody for
    a caveat, costBreakdownSection for a row), cmd_cost.go for the CLI (writeCostSummary for
    a human, costJSON for a script);
  - or, if no surface in this binary can ever act on it, add it to snapshotFieldExceptions
    with the reason, in the form the entries there use. A reason, not a skip.

If it IS read and this still fails, the read goes through a variable whose name does not
contain %q. This check matches selector bases by name because it cannot see types — add the
name to snapshotBaseHint's rule and say why.`, name, snapshotBaseHint)
	}
	for name, reason := range snapshotFieldExceptions {
		if _, ok := typ.FieldByName(name); !ok {
			t.Errorf("snapshotFieldExceptions names %s, which usage.Snapshot no longer has — "+
				"delete the entry rather than leaving a reason for a field that is gone: %s",
				name, reason)
		}
	}
}

// TestUsageCounts_EveryFieldHasAConsumerInAbctl covers the shape the THIRD instance lived
// in: the per-tier token split is on usage.Counts, not on Snapshot, so the check above
// would not have caught it.
//
// WEAKER MATCHING, stated plainly. A Counts is read through short-lived locals with no
// naming convention — `t := snap.Totals`, `c := series[label]`, `cur.Add(v)` — so there is
// no base-name hint to key on and this accepts a selector of the right name ANYWHERE in
// cmd/abctl. A same-named field on an unrelated type would therefore satisfy it. That risk
// is real but small: these names are distinctive (CacheWriteTokens, PriceableRequests,
// IncompleteRequests), where Snapshot's collide with CostSettings' on the two short ones.
//
// No exception table, because none is needed: all thirteen fields have readers today. When
// a field arrives that genuinely cannot be shown here, add a table in
// snapshotFieldExceptions' form rather than loosening the test.
func TestUsageCounts_EveryFieldHasAConsumerInAbctl(t *testing.T) {
	read := anySelectorReads(t)
	typ := reflect.TypeOf(usage.Counts{})
	if typ.NumField() == 0 {
		t.Fatal("usage.Counts has no fields; this check would pass vacuously")
	}
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if read[name] {
			continue
		}
		t.Errorf(`usage.Counts.%s is aggregated by the server and read by nothing in cmd/abctl.

This is the shape the per-tier token split shipped in: the counters were summed all the way
into Counts and no surface showed them, so a reader could not tell an output-heavy window
from a cache-heavy one at ten times the price. Show it (tui/cost_pane.go's
costTokenSection, cmd_cost.go's tokenSplit) or, if it cannot be shown here, add an
exception table in snapshotFieldExceptions' form — a reason per field, not a skip.`, name)
	}
}

// snapshotScopedReads returns the field names read off a SNAPSHOT-shaped expression
// somewhere in cmd/abctl's non-test code. See snapshotBaseHint for why the base matters.
func snapshotScopedReads(t *testing.T) map[string]bool {
	t.Helper()
	scoped := map[string]bool{}
	forEachSelector(t, func(sel *ast.SelectorExpr) {
		if selectorBaseNamesASnapshot(sel.X) {
			scoped[sel.Sel.Name] = true
		}
	})
	return scoped
}

// anySelectorReads returns every field name read by any selector in cmd/abctl's non-test
// code, whatever the base. The looser half; see TestUsageCounts_EveryFieldHasAConsumerInAbctl.
func anySelectorReads(t *testing.T) map[string]bool {
	t.Helper()
	all := map[string]bool{}
	forEachSelector(t, func(sel *ast.SelectorExpr) { all[sel.Sel.Name] = true })
	return all
}

// forEachSelector parses every non-test .go file under cmd/abctl and visits each selector
// expression in it.
//
// The whole tree rather than this package alone: the clients are spread across `main`
// (`abctl cost`) and `tui` (the Cost pane, the spend strip, the sessions table), and a
// check scoped to one package would report a field the other renders as unconsumed. The
// walk starts at "." because `go test` runs a package in its own directory, which for this
// file is cmd/abctl.
//
// TEST FILES ARE EXCLUDED, and that is the point rather than an optimisation: a field a
// test reads and no renderer does is exactly the defect — three of the four instances had
// producer-side tests asserting the value the whole time.
//
// Comments and strings are excluded for free by parsing to an AST: neither becomes a
// selector expression. Every one of the four defects was discussed at length in comments
// while nothing read the field, so a grep-based version of this check would have passed on
// all four.
func forEachSelector(t *testing.T, visit func(*ast.SelectorExpr)) {
	t.Helper()
	fset := token.NewFileSet()
	files := 0
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Fixtures are not clients. Nor is a vendored tree, if one ever appears here.
			switch d.Name() {
			case "testdata", "vendor":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parsing %s: %v", path, perr)
		}
		files++
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				visit(sel)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking cmd/abctl: %v", err)
	}
	// A walk that found nothing would make every check above pass while proving nothing —
	// the exact vacuous-guard failure this file is meant not to be. The floor is
	// deliberately low and merely non-zero: the point is that the walk RAN.
	if files < 5 {
		t.Fatalf("parsed only %d non-test files under cmd/abctl; the walk found nothing to "+
			"check and every assertion above would pass vacuously", files)
	}
}

// selectorBaseNamesASnapshot reports whether the expression a selector reads FROM looks
// like a snapshot: `snap`, `snapshot`, `todaySnap`, or a field of one (`m.spend.snap`).
//
// A substring match on snapshotBaseHint rather than an exact name, so a qualified or
// prefixed spelling still counts — `todaySnap` is live in tui/spend.go today. A CALL is
// deliberately not followed: `client.GetUsage(ctx, …).Window` would be a read, but nothing
// in this tree is written that way and accepting it would mean guessing at return types
// this check cannot see.
func selectorBaseNamesASnapshot(x ast.Expr) bool {
	switch e := x.(type) {
	case *ast.Ident:
		return strings.Contains(strings.ToLower(e.Name), snapshotBaseHint)
	case *ast.SelectorExpr:
		return strings.Contains(strings.ToLower(e.Sel.Name), snapshotBaseHint)
	case *ast.StarExpr:
		return selectorBaseNamesASnapshot(e.X)
	case *ast.ParenExpr:
		return selectorBaseNamesASnapshot(e.X)
	case *ast.IndexExpr:
		return selectorBaseNamesASnapshot(e.X)
	}
	return false
}
