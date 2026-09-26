package tui

import (
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/cost/usage"
)

// The three coverage states a cost total can be in. Conflating any two of them is
// how a partial figure gets read as a complete one.
func TestRenderCostSummary_ThreeCoverageStates(t *testing.T) {
	for _, tc := range []struct {
		name string
		snap usage.Snapshot
		want []string
		deny []string
	}{{
		name: "nothing priced says so instead of showing zero",
		snap: usage.Snapshot{Totals: usage.Counts{Requests: 5}},
		want: []string{"unavailable"},
		// $0.00 would read as "this traffic was free", which is a different claim.
		deny: []string{"$0.00"},
	}, {
		name: "fully priced shows a bare total",
		snap: usage.Snapshot{
			Priced: true,
			Totals: usage.Counts{Requests: 5, PricedRequests: 5, PriceableRequests: 5, CostMicros: 1_250_000},
		},
		want: []string{"$1.25"},
		deny: []string{"priced", "unpriced"},
	}, {
		name: "partially priced discloses the gap and names it",
		snap: usage.Snapshot{
			Priced:     true,
			Totals:     usage.Counts{Requests: 10, PricedRequests: 4, PriceableRequests: 10, CostMicros: 500_000},
			UnpricedBy: map[string]int64{"api.openai.com gpt-5": 6},
		},
		want: []string{"$0.50", "4/10", "api.openai.com gpt-5"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got := renderCostSummary(&tc.snap)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("rendered %q, missing %q", got, w)
				}
			}
			for _, d := range tc.deny {
				if strings.Contains(got, d) {
					t.Errorf("rendered %q, should not contain %q", got, d)
				}
			}
		})
	}
}

// TestRenderCostSummary_NamesTheBiggestGapFirst: an operator fixing coverage wants
// the entry that buys the most, not an alphabetical list.
func TestRenderCostSummary_NamesTheBiggestGapFirst(t *testing.T) {
	snap := usage.Snapshot{
		Priced: true,
		Totals: usage.Counts{Requests: 100, PricedRequests: 10, PriceableRequests: 100, CostMicros: 1},
		UnpricedBy: map[string]int64{
			"a.example small": 2,
			"z.example huge":  87,
			"m.example mid":   1,
		},
	}
	got := renderCostSummary(&snap)
	hi, lo := strings.Index(got, "z.example huge"), strings.Index(got, "a.example small")
	if hi < 0 {
		t.Fatalf("the largest gap was not named: %q", got)
	}
	if lo >= 0 && lo < hi {
		t.Errorf("smaller gap listed before the largest: %q", got)
	}
}

// TestRenderCostSummary_CapsTheGapList keeps one pathological deployment from
// pushing everything else off the line.
func TestRenderCostSummary_CapsTheGapList(t *testing.T) {
	by := map[string]int64{}
	for i := 0; i < 20; i++ {
		by[string(rune('a'+i))+".example m"] = int64(20 - i)
	}
	snap := usage.Snapshot{
		Priced:     true,
		Totals:     usage.Counts{Requests: 300, PricedRequests: 1, PriceableRequests: 300, CostMicros: 1},
		UnpricedBy: by,
	}
	got := renderCostSummary(&snap)
	if n := strings.Count(got, ".example"); n > 3 {
		t.Errorf("named %d gaps, want at most 3: %q", n, got)
	}
	if !strings.Contains(got, "+17 more") {
		t.Errorf("did not disclose the elided gaps: %q", got)
	}
}

// TestRenderCostSummary_NonInferenceTrafficDoesNotLookLikeAGap is the case the
// old denominator got wrong.
//
// A sidecar handling nine MCP tool calls and one priced inference request is
// CORRECTLY configured with complete cost coverage, but comparing PricedRequests
// against all Requests reported "1/10 priced" forever with an empty gap list. A
// permanent warning with nothing to act on is worse than no warning.
func TestRenderCostSummary_NonInferenceTrafficDoesNotLookLikeAGap(t *testing.T) {
	snap := usage.Snapshot{
		Priced: true,
		Totals: usage.Counts{
			Requests:          10, // 9 of them non-inference
			PriceableRequests: 1,
			PricedRequests:    1,
			CostMicros:        250_000,
		},
	}
	got := renderCostSummary(&snap)
	if strings.Contains(got, "priced") {
		t.Errorf("rendered %q — full coverage must not disclose a gap", got)
	}
	if !strings.Contains(got, "$0.25") {
		t.Errorf("rendered %q, want the total", got)
	}
}

// And a real gap is still disclosed, measured against priceable traffic.
func TestRenderCostSummary_RealGapMeasuredAgainstPriceable(t *testing.T) {
	snap := usage.Snapshot{
		Priced: true,
		Totals: usage.Counts{
			Requests:          50, // mostly non-inference
			PriceableRequests: 10,
			PricedRequests:    4,
			CostMicros:        250_000,
		},
		UnpricedBy: map[string]int64{"api.openai.com gpt-5": 6},
	}
	got := renderCostSummary(&snap)
	if !strings.Contains(got, "4/10 priced") {
		t.Errorf("rendered %q, want the ratio against priceable traffic (4/10), not 4/50", got)
	}
	if !strings.Contains(got, "api.openai.com gpt-5") {
		t.Errorf("rendered %q, want the gap named", got)
	}
}

// The spec's success criterion asks for cost "labelled with provenance". A total
// assembled from a gateway's own numbers and one modelled from a shipped vendor-list
// table are not equally trustworthy, and nothing in the column distinguished them.
func TestRenderCostSummary_LabelsProvenance(t *testing.T) {
	base := usage.Counts{Requests: 10, PriceableRequests: 10, PricedRequests: 10, CostMicros: 1_240_000}
	for _, tc := range []struct {
		name string
		by   map[string]int64
		want string
		deny string
	}{{
		// The baseline a reader already assumes; annotating it is noise.
		name: "wholly authoritative is silent",
		by:   map[string]int64{"authoritative": 10},
		deny: "[",
	}, {
		name: "wholly bundled says so",
		by:   map[string]int64{"bundled": 10},
		want: "[bundled]",
	}, {
		name: "configured says so",
		by:   map[string]int64{"configured": 10},
		want: "[configured]",
	}, {
		// The case a reader most needs: part of this total is modelled.
		name: "mixed lists the dominant source first",
		by:   map[string]int64{"bundled": 3, "authoritative": 7},
		want: "[authoritative 7, bundled 3]",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got := renderCostSummary(&usage.Snapshot{Priced: true, Totals: base, PricedBy: tc.by})
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Errorf("rendered %q, missing %q", got, tc.want)
			}
			if tc.deny != "" && strings.Contains(got, tc.deny) {
				t.Errorf("rendered %q, should not contain %q", got, tc.deny)
			}
		})
	}
}

// TestRenderCostSummary_SanitizesWireDerivedLabels: the model half of an UnpricedBy key
// comes from the request body's `model` field, chosen by the workload and recorded
// verbatim by the parser. Rendering it raw to a TTY lets an escape sequence reposition
// the cursor, recolour the pane, or erase the very gap being reported. CWE-150.
func TestRenderCostSummary_SanitizesWireDerivedLabels(t *testing.T) {
	for name, hostile := range map[string]string{
		"ANSI colour":     "gw \x1b[31mclaude-opus-5",
		"cursor move":     "gw \x1b[2Aclaude",
		"newline":         "gw claude\nFAKE TOTAL: $0.00",
		"carriage return": "gw claude\rerased",
		"NUL":             "gw claude\x00",
		"DEL":             "gw claude\x7f",
	} {
		t.Run(name, func(t *testing.T) {
			got := renderCostSummary(&usage.Snapshot{
				Priced:     true,
				Totals:     usage.Counts{Requests: 10, PriceableRequests: 10, PricedRequests: 4, CostMicros: 1},
				UnpricedBy: map[string]int64{hostile: 6},
			})
			for _, bad := range []string{"\x1b", "\n", "\r", "\x00", "\x7f"} {
				if strings.Contains(got, bad) {
					t.Errorf("rendered output still carries %q: %q", bad, got)
				}
			}
			// The label is still recognisable, so a real gap remains actionable.
			if !strings.Contains(got, "claude") {
				t.Errorf("sanitizing destroyed the label: %q", got)
			}
		})
	}
}
