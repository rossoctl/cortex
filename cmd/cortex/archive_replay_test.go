package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/usage"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/session/archive"
)

// recordTraffic runs one process's worth of traffic — two sessions interleaved, each turn a
// request carrying a request-phase plugin and a priced response — through a store whose recorders
// are agg and an archive at root, then stops that process.
func recordTraffic(t *testing.T, root string, agg *usage.Aggregator, now time.Time) {
	t.Helper()
	arch, err := archive.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	store := session.New(0, 0, 0)
	if agg != nil {
		store.AddRecorder(agg)
	}
	store.AddRecorder(arch)
	for i, s := range []string{"a", "b", "a", "b"} {
		at := now.Add(time.Duration(i-5) * time.Minute)
		rid := fmt.Sprintf("r%d", i)
		raw, err := json.Marshal(event.Event{CostUSD: 0.01 * float64(i+1), Settled: true, Provenance: "configured"})
		if err != nil {
			t.Fatal(err)
		}
		store.Append(s, pipeline.SessionEvent{At: at, Phase: pipeline.SessionRequest, RequestID: rid,
			Invocations: &pipeline.Invocations{Outbound: []pipeline.Invocation{
				{Plugin: "tool-prune", Phase: pipeline.InvocationPhaseRequest}}}})
		store.Append(s, pipeline.SessionEvent{At: at.Add(time.Second), Phase: pipeline.SessionResponse,
			RequestID: rid, StatusCode: 200, Duration: time.Second, Host: "api.anthropic.com",
			Client:    &pipeline.EventClient{Name: "claude-code", Version: "2.1.0"},
			Inference: &pipeline.InferenceExtension{Model: "claude-sonnet-5", TotalTokens: 100 * (i + 1), InputTokens: 10},
			Plugins:   map[string]json.RawMessage{event.Key: raw}})
	}
	store.Close()
	if err := arch.Close(); err != nil {
		t.Fatal(err)
	}
}

// The round trip the replay exists for: traffic recorded by one process, which then stops, reads
// back into the next process's ring as the first process's ring had it — in every grouping, for
// every session, with request plugins paired to their responses across interleaved sessions.
//
// Run twice: with the default cap, and with a cap of one session ring, where every new session
// evicts the coldest — so the surviving per-session ring is only right if the replay fed events in
// the order they happened.
func TestReplayUsage_TheNextProcessHasTheRingTheLastOneHad(t *testing.T) {
	for _, maxSess := range []int{-1, 1} { // -1 keeps the default
		t.Run(fmt.Sprint("maxSessions=", maxSess), func(t *testing.T) {
			root := t.TempDir()
			now := time.Now().Truncate(time.Minute).Add(30 * time.Second)
			clock := func() time.Time { return now }
			first := usage.New(usage.WithClock(clock), usage.WithMaxSessions(maxSess))
			recordTraffic(t, root, first, now)

			reopened, err := archive.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			second := usage.New(usage.WithClock(clock), usage.WithMaxSessions(maxSess))
			priv := second.BeginReplay()
			events, sessions, skipped, err := replayUsage(context.Background(), reopened.ReplaySources(now.Add(-usage.MaxWindow)), priv)
			if err != nil || events != 8 || sessions != 2 || skipped != 0 {
				t.Fatalf("replayUsage = %d events, %d sessions, %d skipped, %v; want 8, 2, 0, nil", events, sessions, skipped, err)
			}
			second.FinishReplay(priv)
			for _, g := range []usage.Group{usage.GroupNone, usage.GroupModel, usage.GroupPlugin, usage.GroupSession,
				usage.GroupAgent, usage.GroupHost, usage.GroupStatus} {
				for _, id := range []string{"", "a", "b"} {
					w := first.Snapshot(time.Hour, time.Minute, id, g)
					got := second.Snapshot(time.Hour, time.Minute, id, g)
					if !reflect.DeepEqual(w, got) {
						t.Errorf("group %s session %q: replayed ring differs:\nwant %+v\ngot  %+v", g, id, w, got)
					}
				}
			}
		})
	}
}

// errAfter is a context whose Err is nil for its first n calls and context.DeadlineExceeded after,
// so a test can run a deadline out part-way through a read instead of before it starts. ReplaySince
// asks Err and nothing else, once per segment and once per event.
type errAfter struct {
	context.Context
	n int
}

func (c *errAfter) Err() error {
	if c.n > 0 {
		c.n--
		return nil
	}
	return context.DeadlineExceeded
}

// ALL OR NOTHING: a replay that runs out of time feeds nothing, and the ring starts empty as it
// always did, rather than holding a partial LAST 1H that reads as real.
//
// THE DEADLINE LANDS MID-READ, after ReplaySince has delivered events, not before it starts. Err
// answers nil three times: the first segment's check, then its first turn's request and response.
// A replay that fed the ring from inside the callback has fed that turn by then, so this fails it,
// where a context already done would deliver nothing and let it pass.
func TestReplayUsage_AnExhaustedBudgetFeedsNothing(t *testing.T) {
	const allowed = 3
	root := t.TempDir()
	now := time.Now()
	recordTraffic(t, root, nil, now)
	arch, err := archive.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer arch.Close()

	// The premise: this deadline interrupts a read that has started delivering.
	delivered := 0
	if _, err := arch.ReplaySince(&errAfter{context.Background(), allowed}, now.Add(-usage.MaxWindow),
		func(string, *pipeline.SessionEvent) { delivered++ }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ReplaySince err = %v, want context.DeadlineExceeded", err)
	}
	if delivered == 0 || delivered >= 8 {
		t.Fatalf("ReplaySince delivered %d of 8 events before the deadline; want some but not all", delivered)
	}

	agg := usage.New(usage.WithClock(func() time.Time { return now }))
	ctx := &errAfter{context.Background(), allowed}
	if _, _, _, err := replayUsage(ctx, arch.ReplaySources(now.Add(-usage.MaxWindow)), agg); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if n := agg.Snapshot(usage.MaxWindow, usage.MaxWindow, "", usage.GroupNone).Totals.Requests; n != 0 {
		t.Fatalf("an abandoned replay fed %d requests", n)
	}
}

// THE REPLAY RUNS BESIDE LIVE TRAFFIC and still counts every event once. The set is taken before
// the store records anything; then live traffic is recorded through the same archive while the
// replay is pending. The merged ring holds the earlier process's traffic plus the live traffic —
// not the live traffic twice, which is what reading the archive after it had been written would do.
func TestStartUsageReplay_LiveTrafficIsNotCountedTwice(t *testing.T) {
	root := t.TempDir()
	now := time.Now().Truncate(time.Minute).Add(30 * time.Second)
	clock := func() time.Time { return now }
	want := usage.New(usage.WithClock(clock))
	recordTraffic(t, root, want, now)

	arch, err := archive.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	live := usage.New(usage.WithClock(clock))
	set := arch.ReplaySources(now.Add(-usage.MaxWindow))
	priv := live.BeginReplay()

	store := session.New(0, 0, 0)
	store.AddRecorder(live)
	store.AddRecorder(arch)
	liveTurn := pipeline.SessionEvent{At: now, Phase: pipeline.SessionResponse, StatusCode: 200, Duration: time.Second,
		RequestID: "live", Inference: &pipeline.InferenceExtension{Model: "claude-sonnet-5", TotalTokens: 7}}
	store.Append("a", liveTurn)
	want.Record("a", &liveTurn)
	store.Close()
	// Closed, so the live turn is on disk where a read could find it: the set must not.
	if err := arch.Close(); err != nil {
		t.Fatal(err)
	}

	if _, _, _, err := replayUsage(context.Background(), set, priv); err != nil {
		t.Fatal(err)
	}
	live.FinishReplay(priv)
	for _, id := range []string{"", "a", "b"} {
		w := want.Snapshot(time.Hour, time.Minute, id, usage.GroupNone).Totals
		got := live.Snapshot(time.Hour, time.Minute, id, usage.GroupNone).Totals
		if !reflect.DeepEqual(w, got) {
			t.Errorf("session %q: totals differ:\nwant %+v\ngot  %+v", id, w, got)
		}
	}
}

// A session erased while the replay is pending stays erased once it merges, and one renamed keeps
// its new id: the merge applies what the live ring did meanwhile before it adds the replay.
func TestStartUsageReplay_AnEraseOrRenameMeanwhileHolds(t *testing.T) {
	for _, c := range []struct {
		name string
		do   func(*usage.Aggregator)
		gone []string // sessions with no figures after the merge
		kept []string // sessions with figures after the merge
	}{
		{"delete", func(a *usage.Aggregator) { a.Deleted("a") }, []string{"a"}, []string{"b"}},
		{"clear", func(a *usage.Aggregator) { a.Cleared() }, []string{"a", "b"}, nil},
		{"rename", func(a *usage.Aggregator) { a.Rekeyed("a", "z") }, []string{"a"}, []string{"b", "z"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			now := time.Now().Truncate(time.Minute).Add(30 * time.Second)
			clock := func() time.Time { return now }
			recordTraffic(t, root, nil, now)
			arch, err := archive.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer arch.Close()
			live := usage.New(usage.WithClock(clock))
			set := arch.ReplaySources(now.Add(-usage.MaxWindow))
			priv := live.BeginReplay()
			c.do(live)
			if _, _, _, err := replayUsage(context.Background(), set, priv); err != nil {
				t.Fatal(err)
			}
			live.FinishReplay(priv)
			for _, id := range c.gone {
				if n := live.Snapshot(time.Hour, time.Minute, id, usage.GroupNone).Totals.Requests; n != 0 {
					t.Errorf("session %q has %d requests after the merge, want none", id, n)
				}
				if _, ok := sessionSeries(live)[id]; ok {
					t.Errorf("the all-sessions breakdown still names %q", id)
				}
			}
			for _, id := range c.kept {
				if n := live.Snapshot(time.Hour, time.Minute, id, usage.GroupNone).Totals.Requests; n != 2 {
					t.Errorf("session %q has %d requests after the merge, want 2", id, n)
				}
			}
			if n := live.Snapshot(time.Hour, time.Minute, "", usage.GroupNone).Totals.Requests; n != 4 {
				t.Errorf("all-sessions total = %d requests, want 4: an erase keeps totals", n)
			}
		})
	}
}

// sessionSeries is every session id the all-sessions ring's session breakdown names.
func sessionSeries(a *usage.Aggregator) map[string]bool {
	out := map[string]bool{}
	for _, b := range a.Snapshot(time.Hour, time.Minute, "", usage.GroupSession).Buckets {
		for k := range b.Series {
			out[k] = true
		}
	}
	return out
}

// startUsageReplay does not hold up startup: it returns before the replay has read anything, and
// the live ring holds the replayed traffic once its channel closes.
func TestStartUsageReplay_ReturnsAtOnceAndMergesLater(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	recordTraffic(t, root, nil, now)
	arch, err := archive.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer arch.Close()
	live := usage.New()
	done := startUsageReplay(arch, live)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the replay did not finish")
	}
	if n := live.Snapshot(usage.MaxWindow, usage.MaxWindow, "", usage.GroupNone).Totals.Requests; n != 4 {
		t.Fatalf("live ring holds %d requests after the replay, want 4", n)
	}
}
