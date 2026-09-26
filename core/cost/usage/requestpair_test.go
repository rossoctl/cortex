package usage

import (
	"fmt"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// reqEvent is the request half of a turn, carrying only request-phase
// invocations — which is what the listener actually appends.
func reqEvent(at time.Time, id string, plugins ...string) *pipeline.SessionEvent {
	inv := &pipeline.Invocations{}
	for _, p := range plugins {
		inv.Outbound = append(inv.Outbound, pipeline.Invocation{
			Plugin: p, Phase: pipeline.InvocationPhaseRequest,
		})
	}
	return &pipeline.SessionEvent{
		At: at, Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
		RequestID: id, Invocations: inv,
	}
}

// respEventWith is the response half, carrying only response-phase invocations.
func respEventWith(at time.Time, id string, tokens int, plugins ...string) *pipeline.SessionEvent {
	inv := &pipeline.Invocations{}
	for _, p := range plugins {
		inv.Outbound = append(inv.Outbound, pipeline.Invocation{
			Plugin: p, Phase: pipeline.InvocationPhaseResponse,
		})
	}
	return &pipeline.SessionEvent{
		At: at, Direction: pipeline.Outbound, Phase: pipeline.SessionResponse,
		RequestID: id, StatusCode: 200, Duration: time.Second,
		Inference:   &pipeline.InferenceExtension{Model: "claude-sonnet-5", TotalTokens: tokens},
		Invocations: inv,
	}
}

func pluginSeries(t *testing.T, a *Aggregator, session string) map[string]Counts {
	t.Helper()
	out := map[string]Counts{}
	for _, b := range a.Snapshot(10*time.Minute, BucketWidth, session, GroupPlugin).Buckets {
		for k, v := range b.Series {
			cur := out[k]
			cur.Requests += v.Requests
			cur.Tokens += v.Tokens
			out[k] = cur
		}
	}
	return out
}

// A plugin that only acts on the request must appear in the by-plugin breakdown.
// The listener splits invocations by phase, so context-guru and tool-prune
// (WritesRequestBody with a stub OnResponse) appear only on the request event —
// which Record skips for counting. Before pairing, `by plugin` silently meant
// "plugins that ran on the response".
func TestRecord_RequestOnlyPluginIsAttributed(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 30, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))

	a.Record("s1", reqEvent(now, "r1", "inference-parser", "context-guru"))
	a.Record("s1", respEventWith(now, "r1", 100, "inference-parser"))

	series := pluginSeries(t, a, "")
	if _, ok := series["context-guru"]; !ok {
		t.Fatalf("request-only plugin missing from by-plugin; got %v", keys(series))
	}
	// It inherits the response's tokens, the same whole-attribution the aggregator
	// already documents for multi-plugin responses.
	if got := series["context-guru"].Tokens; got != 100 {
		t.Errorf("context-guru tokens = %d, want 100 (the paired response's)", got)
	}
	if got := series["context-guru"].Requests; got != 1 {
		t.Errorf("context-guru requests = %d, want 1", got)
	}
}

// The request event must contribute labels only. Counting it as traffic would
// double every request and halve the latency mean.
func TestRecord_RequestEventAddsNoTraffic(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 30, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))

	a.Record("s1", reqEvent(now, "r1", "context-guru"))
	// No response yet: nothing counted at all.
	if got := a.Snapshot(time.Minute, BucketWidth, "", GroupNone).Totals; got.Requests != 0 {
		t.Errorf("totals after a lone request event = %+v, want zero", got)
	}

	a.Record("s1", respEventWith(now, "r1", 100, "inference-parser"))
	totals := a.Snapshot(time.Minute, BucketWidth, "", GroupNone).Totals
	if totals.Requests != 1 {
		t.Errorf("requests = %d, want 1 — one turn is one request", totals.Requests)
	}
	if totals.Tokens != 100 {
		t.Errorf("tokens = %d, want 100", totals.Tokens)
	}
	b := a.Snapshot(time.Minute, BucketWidth, "", GroupNone).Buckets[0]
	if b.LatMeanMs != 1000 {
		t.Errorf("latency mean = %v, want 1000 — the request event must not dilute it", b.LatMeanMs)
	}
}

// Pairing is by id, not by arrival order: a client can have several requests in
// flight, and positional pairing would attribute one turn's plugins to another's
// tokens.
func TestRecord_PairsByIDNotPosition(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 30, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))

	// Two turns open; responses arrive in the opposite order.
	a.Record("s1", reqEvent(now, "r1", "context-guru"))
	a.Record("s1", reqEvent(now, "r2", "tool-prune"))
	a.Record("s1", respEventWith(now, "r2", 20, "inference-parser"))
	a.Record("s1", respEventWith(now, "r1", 700, "inference-parser"))

	series := pluginSeries(t, a, "")
	if got := series["context-guru"].Tokens; got != 700 {
		t.Errorf("context-guru tokens = %d, want 700 (r1's response)", got)
	}
	if got := series["tool-prune"].Tokens; got != 20 {
		t.Errorf("tool-prune tokens = %d, want 20 (r2's response)", got)
	}
}

// A plugin that ran in both phases is one plugin that touched one turn, so it must
// not be counted twice.
func TestRecord_PluginInBothPhasesCountsOnce(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 30, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))

	a.Record("s1", reqEvent(now, "r1", "inference-parser"))
	a.Record("s1", respEventWith(now, "r1", 100, "inference-parser"))

	if got := pluginSeries(t, a, "")["inference-parser"].Requests; got != 1 {
		t.Errorf("inference-parser requests = %d, want 1 — it ran in both phases", got)
	}
}

// Several invocations from one plugin on a single pass are still one plugin.
func TestInvocationPlugins_Dedupes(t *testing.T) {
	inv := &pipeline.Invocations{
		Outbound: []pipeline.Invocation{{Plugin: "p"}, {Plugin: "p"}, {Plugin: "q"}},
		Inbound:  []pipeline.Invocation{{Plugin: "q"}, {Plugin: ""}},
	}
	got := invocationPlugins(inv)
	if len(got) != 2 {
		t.Errorf("invocationPlugins = %v, want two distinct names", got)
	}
	if invocationPlugins(nil) != nil {
		t.Error("invocationPlugins(nil) should be nil")
	}
}

// A response whose request never arrived must still be counted — just without
// request-phase plugins. A proxy that starts mid-turn is the normal case for this.
func TestRecord_ResponseWithoutItsRequest(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 30, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))

	a.Record("s1", respEventWith(now, "orphan", 42, "inference-parser"))
	totals := a.Snapshot(time.Minute, BucketWidth, "", GroupNone).Totals
	if totals.Requests != 1 || totals.Tokens != 42 {
		t.Errorf("orphan response not counted: %+v", totals)
	}
}

// An event with no RequestID cannot be paired, and must not be paired
// positionally — that misattributes as soon as two requests are in flight.
func TestRecord_NoRequestIDIsNotPaired(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 30, 0, time.UTC)
	a := New(WithClock(fixedClock(now)))

	req := reqEvent(now, "", "context-guru")
	a.Record("s1", req)
	if len(a.pending) != 0 {
		t.Error("held a request half with no id to pair it against")
	}
	a.Record("s1", respEventWith(now, "", 100, "inference-parser"))
	if _, ok := pluginSeries(t, a, "")["context-guru"]; ok {
		t.Error("attributed an unpairable request half to a response")
	}
}

// Requests whose responses never arrive must not accumulate forever: that is one
// leaked entry per abandoned turn, on the synchronous append path.
func TestRecord_PendingIsBounded(t *testing.T) {
	base := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	now := base
	a := New(WithClock(func() time.Time { return now }))

	for i := 0; i < maxPendingRequests*2; i++ {
		now = base.Add(time.Duration(i) * time.Second)
		a.Record("s1", reqEvent(now, fmt.Sprintf("req-%d", i), "context-guru"))
	}
	a.mu.RLock()
	n := len(a.pending)
	a.mu.RUnlock()
	if n > maxPendingRequests {
		t.Errorf("pending holds %d entries, cap is %d", n, maxPendingRequests)
	}
}

// A request half older than pendingTTL is swept: the listener has abandoned that
// turn too, so its plugins will never be paired.
func TestRecord_StalePendingIsExpired(t *testing.T) {
	base := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	now := base
	a := New(WithClock(func() time.Time { return now }))

	a.Record("s1", reqEvent(now, "old", "context-guru"))

	// Fill to the cap far in the future, which triggers the sweep.
	now = base.Add(pendingTTL + time.Minute)
	for i := 0; i < maxPendingRequests; i++ {
		a.Record("s1", reqEvent(now, fmt.Sprintf("new-%d", i), "tool-prune"))
	}

	a.mu.RLock()
	_, stillThere := a.pending["old"]
	a.mu.RUnlock()
	if stillThere {
		t.Error("a request half older than pendingTTL was not swept")
	}
}
