package session

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/pipeline"
)

// priorKeeper is a session archive reduced to what ListSessions asks of one: the last seq it holds
// under each id, and the fold of that history. refuse makes it a keeper that cannot say.
type priorKeeper struct {
	last   map[string]uint64
	prior  map[string]Prior
	refuse bool
}

func (k *priorKeeper) Record(string, *pipeline.SessionEvent) {}
func (k *priorKeeper) LastSeq(id string) uint64              { return k.last[id] }
func (k *priorKeeper) Prior(id string, after uint64) (Prior, bool) {
	if k.refuse || after != k.last[id] {
		return Prior{}, false
	}
	p, ok := k.prior[id]
	return p, ok
}

// continuityFixture is foldFixture with a /rename and then a later prose turn, so a split can fall
// on either side of every change in the title's rank.
func continuityFixture(t *testing.T) []pipeline.SessionEvent {
	t.Helper()
	evs := foldFixture(t)
	rename := titleEvent("<command-name>/rename</command-name><command-args>flaky store test</command-args>")
	rename.Phase = pipeline.SessionRequest
	later := titleEvent("one more question")
	later.Phase = pipeline.SessionRequest
	return append(evs, rename, later)
}

// THE REASON FOR PriorKeeper: a store that lost a session — to a restart, a ttl, max_sessions — and
// is handed its earlier history lists the session exactly as a store that never lost it. Split at
// every event, so every figure's merge is tried on both sides of every change in it.
func TestListSessions_ContinuesAnEarlierHistory(t *testing.T) {
	evs := continuityFixture(t)
	whole := New(0, 0, 0)
	defer whole.Close()
	for _, e := range evs {
		whole.Append("s1", e)
	}
	want := summaryOf(t, whole, "s1")
	if want.Title != "flaky store test" || want.PromptContext == nil || len(want.Currencies) != 2 ||
		want.CostMicros == 0 || want.AvoidedMicros == 0 || want.Agent == "" {
		t.Fatalf("fixture no longer exercises every figure: %+v", want)
	}
	created := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	for split := 1; split < len(evs); split++ {
		earlier := NewSummaryFold()
		for i := range evs[:split] {
			earlier.Add("s1", &evs[i])
		}
		k := &priorKeeper{last: map[string]uint64{"s1": uint64(split)},
			prior: map[string]Prior{"s1": {Fold: earlier, CreatedAt: created}}}
		st := New(0, 0, 0)
		st.AddRecorder(k)
		for _, e := range evs[split:] {
			st.Append("s1", e)
		}
		got := summaryOf(t, st, "s1")
		st.Close()
		if got.EventCount != want.EventCount || got.Title != want.Title || got.Agent != want.Agent ||
			got.TotalTokens != want.TotalTokens || got.CostMicros != want.CostMicros ||
			got.AvoidedMicros != want.AvoidedMicros || got.Saturated != want.Saturated ||
			!slices.Equal(got.Currencies, want.Currencies) || !sameContext(got.PromptContext, want.PromptContext) {
			t.Errorf("split %d:\n got  %+v\n want %+v", split, got, want)
		}
		if !got.CreatedAt.Equal(created) {
			t.Errorf("split %d: CreatedAt = %v, want the earlier history's %v", split, got.CreatedAt, created)
		}
	}
}

// A keeper that cannot say exactly what came before leaves the row as the entry's own figures: an
// undercount, never the entry counted twice.
func TestListSessions_AnUnknownEarlierHistoryLeavesTheEntrysOwnFigures(t *testing.T) {
	st := New(0, 0, 0)
	defer st.Close()
	st.AddRecorder(&priorKeeper{last: map[string]uint64{"s1": 5}, refuse: true})
	st.Append("s1", pipeline.SessionEvent{Phase: pipeline.SessionRequest})
	if got := summaryOf(t, st, "s1"); got.EventCount != 1 {
		t.Fatalf("EventCount = %d, want the entry's own 1", got.EventCount)
	}
}

// The claim owner's label wins as it always has, even when only the earlier history carries it:
// the owner's first event came before the restart, and nothing since named a client.
func TestListSessions_TheOwnersLabelMayComeFromTheEarlierHistory(t *testing.T) {
	claude := &pipeline.EventClient{Name: "claude-code", Version: "2.1.284"}
	earlier := NewSummaryFold()
	earlier.Add("s1", &pipeline.SessionEvent{Phase: pipeline.SessionRequest, Client: claude})
	st := New(0, 0, 0)
	defer st.Close()
	st.AddRecorder(&priorKeeper{last: map[string]uint64{"s1": 1}, prior: map[string]Prior{"s1": {Fold: earlier}}})
	st.Append("s1", claimedBy("claude-code"))
	if got, want := summaryOf(t, st, "s1").Agent, earlier.Summary("s1").Agent; got != want || want == "" {
		t.Fatalf("Agent = %q, want the owner's label from the earlier history, %q", got, want)
	}
}

// A claim owner's label is the first label that agent sent across the session's whole history, so
// where the earlier history and the entry carry the owner under two different labels, the earlier
// one wins — as it does in a store that never lost the session. Two versions alone cannot show it:
// AgentName folds a name/version label to the bare name, so the entry's label here is a WebFetch's
// raw User-Agent, which names claude-code only inside a comment.
func TestListSessions_TheOwnersEarlierLabelWinsOverTheEntrys(t *testing.T) {
	first := pipeline.SessionEvent{Phase: pipeline.SessionRequest,
		Client: &pipeline.EventClient{Name: "claude-code", Version: "2.1.284"}}
	later := pipeline.SessionEvent{Phase: pipeline.SessionRequest,
		Client: &pipeline.EventClient{Raw: "Claude-User (claude-code/2.1.290; +https://support.anthropic.com/)"}}

	whole := New(0, 0, 0)
	defer whole.Close()
	whole.Append("s1", first)
	whole.Append("s1", later)
	whole.Append("s1", claimedBy("claude-code"))
	want := summaryOf(t, whole, "s1").Agent

	earlier := NewSummaryFold()
	earlier.Add("s1", &first)
	entryOnly := NewSummaryFold()
	entryOnly.Add("s1", &later)
	if want == "" || want != earlier.Summary("s1").Agent || want == entryOnly.Summary("s1").Agent {
		t.Fatalf("fixture: the owner's two labels do not differ, or the whole store does not pick the earlier "+
			"(whole %q, earlier %q, entry %q)", want, earlier.Summary("s1").Agent, entryOnly.Summary("s1").Agent)
	}

	st := New(0, 0, 0)
	defer st.Close()
	st.AddRecorder(&priorKeeper{last: map[string]uint64{"s1": 1}, prior: map[string]Prior{"s1": {Fold: earlier}}})
	st.Append("s1", later)
	st.Append("s1", claimedBy("claude-code"))
	if got := summaryOf(t, st, "s1").Agent; got != want {
		t.Fatalf("Agent = %q, want the owner's earlier label %q, as a store that never lost the session reports",
			got, want)
	}
}

// A total that clamped on either side is a bound, so the merged row says so: one saturated figure
// before the restart marks the row after it.
func TestListSessions_ASaturatedEarlierHistoryMarksTheRow(t *testing.T) {
	earlier := NewSummaryFold()
	// Enough near-maximal costs that their sum overflows int64 micros: saturation is the ADDITION
	// overflowing. pricing.MicrosFromUSD refuses any one figure at or past MaxCostMicros (2^53)
	// rather than clamping it, so it takes over a thousand of them, as in
	// TestSumCost_SaturatesRatherThanWrapping.
	const n = 2048
	huge := pipeline.SessionEvent{Phase: pipeline.SessionResponse,
		Plugins: costRecord(t, event.Event{CostUSD: 9e9, Settled: true, Provenance: "configured"})}
	for range n {
		earlier.Add("s1", &huge)
	}
	if !earlier.Summary("s1").Saturated {
		t.Fatal("fixture: the earlier fold did not saturate")
	}
	st := New(0, 0, 0)
	defer st.Close()
	st.AddRecorder(&priorKeeper{last: map[string]uint64{"s1": n}, prior: map[string]Prior{"s1": {Fold: earlier}}})
	st.Append("s1", pipeline.SessionEvent{Phase: pipeline.SessionResponse,
		Plugins: costRecord(t, event.Event{CostUSD: 0.01, Settled: true, Provenance: "configured"})})
	if got := summaryOf(t, st, "s1"); !got.Saturated {
		t.Fatalf("Saturated = false after a saturated earlier history: %+v", got)
	}
}

// BenchmarkListSessions_WithPrior is agentop's two-second poll over a full store of resumed sessions:
// the default 100, each with an earlier history to merge. Against the same store with none, it
// prices the lookup and merge.
func BenchmarkListSessions_WithPrior(b *testing.B) {
	t := &testing.T{}
	for _, withPrior := range []bool{false, true} {
		b.Run(fmt.Sprintf("prior=%v", withPrior), func(b *testing.B) {
			k := &priorKeeper{last: map[string]uint64{}, prior: map[string]Prior{}}
			st := New(0, 0, 0)
			defer st.Close()
			if withPrior {
				st.AddRecorder(k)
			}
			for i := range 100 {
				id := fmt.Sprintf("s%03d", i)
				earlier := NewSummaryFold()
				for _, e := range foldFixture(t) {
					earlier.Add(id, &e)
				}
				k.last[id], k.prior[id] = uint64(earlier.events), Prior{Fold: earlier}
				for _, e := range foldFixture(t) {
					st.Append(id, e)
				}
			}
			b.ReportAllocs()
			for b.Loop() {
				_ = st.ListSessions()
			}
		})
	}
}
