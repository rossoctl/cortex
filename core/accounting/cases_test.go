package accounting

import (
	"encoding/json"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/accountant/test/cases"
)

// differencesFile is accountant/docs/testing.md §10.4's host-side differences list: cases where
// Cortex knowingly differs from the library's cases, or does not yet cover something the cases
// test. Kept at the repo's docs/ root because it is not specific to one package.
const differencesFile = "../../docs/accounting-differences.json"

// difference is one entry of differencesFile.
type difference struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"` // "differs" | "notCovered"
	Reason string `json:"reason"`
}

type differencesDoc struct {
	Host        string       `json:"host"`
	Differences []difference `json:"differences"`
}

func loadDifferences(t *testing.T) map[string]difference {
	t.Helper()
	raw, err := os.ReadFile(differencesFile)
	if err != nil {
		t.Fatalf("read %s: %v", differencesFile, err)
	}
	var doc differencesDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal %s: %v", differencesFile, err)
	}
	if doc.Host != "cortex" {
		t.Fatalf("%s: host = %q, want cortex", differencesFile, doc.Host)
	}
	out := map[string]difference{}
	for _, d := range doc.Differences {
		if d.Kind != "differs" && d.Kind != "notCovered" {
			t.Fatalf("%s: case %q has unknown kind %q", differencesFile, d.ID, d.Kind)
		}
		if strings.TrimSpace(d.Reason) == "" {
			t.Fatalf("%s: case %q has no reason", differencesFile, d.ID)
		}
		out[d.ID] = d
	}
	return out
}

// TestAccountantCases plays every case in github.com/rossoctl/accountant/test/cases through the
// library exactly as a host does: BeginRequest, Add, EndRequest, Compute (or ComputeStandalone
// for a keep-alive-ping step), and compares the Result to the case's Expected, the way the
// library's own test/cases/cases_test.go (TestComputeMeetsEveryCase) does. This is the agreement
// test accountant/docs/testing.md §10.4 asks every host to run: a case not listed in
// docs/accounting-differences.json must agree with the library, because Task 1 adds no host logic
// that could disagree with it yet (wire and facts are not wired into Compute's inputs here) — this
// test exists so that later tasks, which do add such logic, inherit a passing baseline and a place
// to record any divergence they introduce.
func TestAccountantCases(t *testing.T) {
	diffs := loadDifferences(t)
	all := cases.All()
	if len(all) == 0 {
		t.Fatal("no cases: github.com/rossoctl/accountant/test/cases returned none")
	}
	for _, c := range all {
		c := c
		if d, ok := diffs[c.ID]; ok {
			t.Run(c.ID, func(t *testing.T) {
				t.Skipf("listed in %s: %s (%s)", differencesFile, d.Reason, d.Kind)
			})
			continue
		}
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
