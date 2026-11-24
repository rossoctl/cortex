package usage

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/pipeline"
)

// TestRekeyed_MovesTheSessionsFiguresToTheAdoptingSession pins what session.Store.Adopt
// relies on: after a pending bucket is renamed, /v1/usage?session=<new> answers with its
// figures and the all-sessions breakdown names the new id, not the pending one.
func TestRekeyed_MovesTheSessionsFiguresToTheAdoptingSession(t *testing.T) {
	a := New()
	a.Record("pending:bob-shell", inferenceEvent("m", 13, 0, 0, 5, 0, 0b1001))
	a.Record("other", inferenceEvent("m", 29, 0, 0, 7, 0, 0b1001))

	a.Rekeyed("pending:bob-shell", "task-1")

	all := mergeSeries(a.Snapshot(10*time.Minute, BucketWidth, "", GroupSession).Buckets)
	if _, ok := all["pending:bob-shell"]; ok {
		t.Error("all-sessions breakdown still names the adopted pending bucket")
	}
	if got := all["task-1"].InputTokens; got != 13 {
		t.Errorf("task-1 InputTokens = %d, want 13", got)
	}
	if got := all["other"].InputTokens; got != 29 {
		t.Errorf("other InputTokens = %d, want 29 (untouched)", got)
	}
	if got := a.Snapshot(10*time.Minute, BucketWidth, "task-1", GroupNone).Totals.InputTokens; got != 13 {
		t.Errorf("session=task-1 InputTokens = %d, want 13 (the per-session ring must follow)", got)
	}
	if got := a.Snapshot(10*time.Minute, BucketWidth, "pending:bob-shell", GroupNone).Totals.InputTokens; got != 0 {
		t.Errorf("session=pending:bob-shell InputTokens = %d, want 0 after the move", got)
	}
}

// Each agent's own ring keeps a session breakdown too, so adoption renames the pending bucket there
// as well.
func TestRekeyed_RenamesThePendingBucketInEveryAgentsRing(t *testing.T) {
	a := New()
	for _, c := range []struct {
		name string
		in   int
	}{{"bob-shell", 13}, {"claude-code", 17}} {
		e := inferenceEvent("m", c.in, 0, 0, 5, 0, 0b1001)
		e.Client = &pipeline.EventClient{Name: c.name, Version: "1.0"}
		a.Record("pending:bob-shell", e)
	}

	a.Rekeyed("pending:bob-shell", "task-1")

	// Looked up by the FOLDED name: the ring keys Bob's ring "bob" however the
	// event's client was labelled, so the pre-convergence "bob-shell" label above
	// and today's query spell one agent.
	for agent, want := range map[string]int64{"bob": 13, "claude-code": 17} {
		series := mergeSeries(a.AgentSnapshot(10*time.Minute, BucketWidth, "", agent, GroupSession).Buckets)
		if _, ok := series["pending:bob-shell"]; ok {
			t.Errorf("%s's ring still names the adopted pending bucket: %v", agent, series)
		}
		if got := series["task-1"].InputTokens; got != want {
			t.Errorf("%s's ring: task-1 InputTokens = %d, want %d", agent, got, want)
		}
	}
}

// pricedEvent is a settled, priced response at `at`, so its labels carry a unit.
func pricedEvent(t *testing.T, at time.Time, model string) *pipeline.SessionEvent {
	t.Helper()
	e := inferenceEvent(model, 100, 0, 0, 30, 0, 0b1001)
	e.At = at
	raw, err := json.Marshal(event.Event{CostUSD: 0.005, Provenance: "configured", Settled: true})
	if err != nil {
		t.Fatal(err)
	}
	e.Plugins = map[string]json.RawMessage{event.Key: raw}
	return e
}

// richTurn is two responses at `at` that between them set every field of a bucket: a priced
// 5xx carrying a latency, a client, a plugin, a saving and an inexact figure, and an unpriced
// one. TestRekeyed_TheMergeFixtureSetsEveryBucketField holds it to that. Give each side of a
// merge its own model, agent and host, or a label's units already on the target hide a merge
// that dropped the source's.
func richTurn(t *testing.T, at time.Time, model, agent, host string, ms int) []*pipeline.SessionEvent {
	t.Helper()
	priced := inferenceEvent(model, 100, 2000, 50, 30, 12, 0b11111)
	priced.At = at
	priced.StatusCode = 500
	priced.Host = host
	priced.Duration = time.Duration(ms) * time.Millisecond
	priced.Client = &pipeline.EventClient{Name: agent, Version: "1.0"}
	priced.Invocations = &pipeline.Invocations{Outbound: []pipeline.Invocation{{
		Plugin: "inference-parser", Phase: pipeline.InvocationPhaseResponse,
	}}}
	raw, err := json.Marshal(event.Event{
		CostUSD: 0.005, Source: event.SourceUsageFallback, Provenance: "configured",
		Settled: true, Incomplete: true, IncompleteReason: pricing.ReasonOutputUncounted,
		Avoided: []event.Saving{{
			Basis: event.BasisUSD, Component: "tool-prune", TokensAvoided: 400, USD: 0.0012,
			Tier: "input", Provenance: "configured", Estimated: true,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	priced.Plugins = map[string]json.RawMessage{event.Key: raw}

	unpriced := inferenceEvent(model+"-unpriced", 10, 0, 0, 5, 0, 0b1001)
	unpriced.At = at
	unpriced.Host = host
	return []*pipeline.SessionEvent{priced, unpriced}
}

// The equality below compares buckets whole, so it only covers a field the fixture sets.
func TestRekeyed_TheMergeFixtureSetsEveryBucketField(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := New()
	for _, e := range richTurn(t, now, "m", "claude-code", "gw.example.com", 40) {
		a.Record("s1", e)
	}
	b := reflect.ValueOf(a.sessions["s1"].buckets[slot(now)])
	for i := 0; i < b.NumField(); i++ {
		if b.Field(i).IsZero() {
			t.Errorf("bucket.%s is zero after richTurn: extend richTurn to set it", b.Type().Field(i).Name)
		}
	}
}

// TestRekeyed_MergesIntoATargetThatAlreadyHasARing pins the rename into an id that already has a
// ring. The store reaches it: it evicts sessions without telling the aggregator, so a ring outlives
// its session there, and a later Rekey or Adopt into that id is permitted. After the rename the
// aggregator must be indistinguishable from one that recorded every event under the new id, in
// the lap arrangements below.
func TestRekeyed_MergesIntoATargetThatAlreadyHasARing(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	lap := NumBuckets * BucketWidth
	for _, c := range []struct {
		name           string
		target, source time.Time
	}{
		{"same minute", now, now},
		{"different minutes", now.Add(-3 * BucketWidth), now},
		{"source a lap newer in the slot", now.Add(-lap), now},
		{"target a lap newer in the slot", now, now.Add(-lap)},
	} {
		t.Run(c.name, func(t *testing.T) {
			type rec struct {
				id string
				e  *pipeline.SessionEvent
			}
			var target, source []rec
			for _, e := range richTurn(t, c.target, "target-model", "claude-code", "gw.example.com", 40) {
				target = append(target, rec{"ctx-1", e})
			}
			for _, e := range richTurn(t, c.source, "source-model", "opencode", "api.example.org", 70) {
				source = append(source, rec{"default", e})
			}
			// Recorded oldest first, as the listener would have.
			recs := slices.Concat(target, source)
			if c.source.Before(c.target) {
				recs = slices.Concat(source, target)
			}

			got, want := New(), New()
			for _, r := range recs {
				got.Record(r.id, r.e)
				want.Record("ctx-1", r.e)
			}
			got.Rekeyed("default", "ctx-1")

			if _, ok := got.sessions["default"]; ok {
				t.Error("the renamed session still has a ring of its own")
			}
			for name, pair := range map[string][2]any{
				"per-session rings": {got.sessions, want.sessions},
				"all-sessions ring": {got.all, want.all},
				"per-agent rings":   {got.agents, want.agents},
			} {
				if !reflect.DeepEqual(pair[0], pair[1]) {
					t.Errorf("%s differ from recording every event under ctx-1", name)
				}
			}
			one := got.Snapshot(2*lap, BucketWidth, "ctx-1", GroupNone).Totals.Requests
			all := mergeSeries(got.Snapshot(2*lap, BucketWidth, "", GroupSession).Buckets)["ctx-1"].Requests
			if one != all {
				t.Errorf("session=ctx-1 Requests = %d, all-sessions row = %d", one, all)
			}
		})
	}
}

// The merge goes through the label cap a fold does, and a label's units follow it to the key it
// was filed under, overflow included.
func TestRekeyed_MergeKeepsTheLabelCapAndItsUnits(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	a := New()
	for i := 0; i < 40; i++ {
		a.Record("ctx-1", pricedEvent(t, now, fmt.Sprintf("target-%d", i)))
		a.Record("default", pricedEvent(t, now, fmt.Sprintf("source-%d", i)))
	}

	a.Rekeyed("default", "ctx-1")

	b := a.sessions["ctx-1"].buckets[slot(now)]
	if b.Requests != 80 {
		t.Errorf("Requests = %d, want 80 from both sessions", b.Requests)
	}
	if len(b.byMethod) > maxLabelsPerBucket {
		t.Errorf("byMethod holds %d labels, over the cap of %d", len(b.byMethod), maxLabelsPerBucket)
	}
	var sum int64
	for _, c := range b.byMethod {
		sum += c.Requests
	}
	if sum != b.Requests {
		t.Errorf("byMethod sums to %d requests, the bucket holds %d", sum, b.Requests)
	}
	for label := range b.labelUnits[GroupModel] {
		if _, ok := b.byMethod[label]; !ok {
			t.Errorf("a unit is noted against %q, which byMethod does not hold", label)
		}
	}
	if _, ok := b.labelUnits[GroupModel][overflowLabel]; !ok {
		t.Errorf("the overflow label carries no unit, though every request folded into it was priced")
	}
}
