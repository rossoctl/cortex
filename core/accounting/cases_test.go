package accounting

import (
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/accountant/test/cases"
)

// TestAccountantCases plays every case in github.com/rossoctl/accountant/test/cases directly
// against the library: BeginRequest, Add, EndRequest, Compute (or ComputeStandalone for a
// keep-alive-ping step), and compares the Result to the case's Expected, the way the library's
// own test/cases/cases_test.go (TestComputeMeetsEveryCase) does. The cases carry already-parsed
// accountant.Body values, not raw request bodies, so this does not exercise Cortex's own
// wire.Parse/facts.For/Classify or Host.RequestEnd/Compute path — it shows the library at the
// pinned version meets its own cases, not that Host agrees with them. This is the agreement test
// accountant/docs/testing.md §10.4 asks every host to run. Every case must pass: none is skipped
// or deferred, so a case Cortex cannot meet fails here and is fixed, not listed away.
func TestAccountantCases(t *testing.T) {
	all := cases.All()
	if len(all) == 0 {
		t.Fatal("no cases: github.com/rossoctl/accountant/test/cases returned none")
	}
	for _, c := range all {
		c := c
		t.Run(c.ID, func(t *testing.T) {
			ts := accountant.NewTrackers()
			for i, s := range c.Steps {
				var res accountant.Result
				if s.IsStandalone() {
					res = accountant.ComputeStandalone(*s.Standalone)
				} else {
					r := ts.BeginRequest(s.Begin.Meta, s.Begin.Client, s.Begin.ClientBreakpoints)
					for _, op := range s.Add {
						r.Add(op)
					}
					r.EndRequest(s.End.Thread, s.End.Forwarded, s.End.Breakpoints, s.End.Facts.EntryFacts())
					res = r.Compute(s.Usage)
				}
				compareResult(t, i, s.Expected, res)
			}
		})
	}
}

// compareResult is cases_test.go's compare, ported: it is the one place that knows how an
// accountant.Result matches a cases.Expected, so a divergence found while wiring Cortex's own
// host code (later tasks) is caught here in the same terms the library's own cases_test.go
// checks it in.
func compareResult(t *testing.T, step int, want cases.Expected, got accountant.Result) {
	t.Helper()
	if want.Phase != "" && got.Phase != want.Phase {
		t.Errorf("step %d: phase %q, want %q", step, got.Phase, want.Phase)
	}
	if want.Boundary != nil && (got.Boundary == nil || *got.Boundary != *want.Boundary) {
		t.Errorf("step %d: boundary %v, want %v", step, got.Boundary, *want.Boundary)
	}
	compareCompaction(t, step, want.Compaction, got.Compaction)

	var priced []accountant.PricedOp
	for _, p := range got.Ops {
		if p.Saved != (accountant.TierTokens{}) || p.Caused != (accountant.TierTokens{}) || p.Call != nil {
			priced = append(priced, p)
		}
	}
	if len(priced) != len(want.Ops) {
		t.Fatalf("step %d: %d priced ops, want %d", step, len(priced), len(want.Ops))
	}
	for j, w := range want.Ops {
		p := priced[j]
		if p.ChangeID != w.ChangeID || p.Saved != w.Saved || p.Caused != w.Caused {
			t.Errorf("step %d op %d: got %s saved=%+v caused=%+v, want %s saved=%+v caused=%+v",
				step, j, p.ChangeID, p.Saved, p.Caused, w.ChangeID, w.Saved, w.Caused)
		}
		if (w.Call == nil) != (p.Call == nil) {
			t.Errorf("step %d op %d: call presence differs", step, j)
		}
		if p.Estimated != w.Estimated {
			t.Errorf("step %d op %d: estimated %v, want %v", step, j, p.Estimated, w.Estimated)
		}
	}

	if len(got.CacheEffects) != len(want.Effects) {
		t.Fatalf("step %d: %d cache effects, want %d: %+v", step, len(got.CacheEffects), len(want.Effects), got.CacheEffects)
	}
	for j, w := range want.Effects {
		g := got.CacheEffects[j]
		if g.Kind != w.Kind || g.ChangeID != w.ChangeID || g.Saved != w.Saved || g.Caused != w.Caused ||
			(w.TTL != "" && g.TTL != w.TTL) || g.Cause != w.Cause || g.InBaseline != w.InBaseline {
			t.Errorf("step %d effect %d: got %s %s saved=%+v caused=%+v inBaseline=%v, want %s %s saved=%+v caused=%+v inBaseline=%v",
				step, j, g.Kind, g.ChangeID, g.Saved, g.Caused, g.InBaseline, w.Kind, w.ChangeID, w.Saved, w.Caused, w.InBaseline)
		}
		if w.FromNil && g.From != nil {
			t.Errorf("step %d effect %d: from %v, want nil (the start of the prompt)", step, j, *g.From)
		}
		if w.From != nil && (g.From == nil || *g.From != *w.From) {
			t.Errorf("step %d effect %d: from %v, want %v", step, j, g.From, *w.From)
		}
		if w.To != nil && (g.To == nil || *g.To != *w.To) {
			t.Errorf("step %d effect %d: to %v, want %v", step, j, g.To, *w.To)
		}
	}

	var failed []string
	for _, c := range got.Checks {
		failed = append(failed, c.Check)
	}
	sort.Strings(failed)
	wantChecks := append([]string(nil), want.Checks...)
	sort.Strings(wantChecks)
	if strings.Join(failed, ",") != strings.Join(wantChecks, ",") {
		t.Errorf("step %d: failed checks %v, want %v", step, failed, wantChecks)
	}

	reasons := map[string]bool{}
	for _, u := range got.Unpriced {
		reasons[u.OpID+"|"+u.Reason] = true
	}
	for _, w := range want.Unpriced {
		if !reasons[w.OpID+"|"+w.Reason] {
			t.Errorf("step %d: missing unpriced %q for op %q", step, w.Reason, w.OpID)
		}
	}
}

func compareCompaction(t *testing.T, step int, want *cases.ExpectedCompaction, got *accountant.CompactionRecord) {
	t.Helper()
	switch {
	case want == nil && got != nil:
		t.Errorf("step %d: compaction record %+v, want none", step, *got)
	case want != nil && got == nil:
		t.Errorf("step %d: no compaction record, want %+v", step, *want)
	case want != nil && (got.Trigger != want.Trigger || !slices.Equal(got.Ended, want.Ended)):
		t.Errorf("step %d: compaction %+v, want %+v", step, *got, *want)
	}
}
