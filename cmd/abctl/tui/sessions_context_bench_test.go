package tui

import (
	"fmt"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// benchContextEvents is a session shaped like a real one: one-shot calls carrying a big context
// interleaved with a conversation whose message count grows.
func benchContextEvents(n int) []pipeline.SessionEvent {
	base := time.Now()
	out := make([]pipeline.SessionEvent, 0, n)
	for i := 0; len(out) < n; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		if i%4 == 0 {
			out = append(out, oneShot(fmt.Sprintf("o%d", i), at, 200_000)...)
		} else {
			out = append(out, conversation(fmt.Sprintf("c%d", i), at, 100+i, 400_000+i)...)
		}
	}
	return out[:n]
}

// THE SHAPE THAT MATTERS: one event arrives, and the sessions row loop asks every session for its
// gauge. rebuildSessionsTable runs on every streamed event and retention is unbounded, so a rescan
// here is O(events) per session per event — on the pane abctl opens on.
//
// This benchmark exists because that regression shipped once. The first version of the rule
// made two full passes and allocated a map per call; measured at 100k events it cost 6.10ms and
// 3.49MB per session per event, against ~14ns for the tail scan it replaced. Dropping the map and
// the dead paired-request check took the whole-slice cost to 0.51ms and no allocation, and folding
// the delta took the per-event cost off the session length entirely.
//
// Run it before changing sessionContextFor:
//
//	go test ./tui/ -run XXX -bench BenchmarkSessionContextPerEvent
func BenchmarkSessionContextPerEvent(b *testing.B) {
	for _, n := range []int{1_000, 10_000} {
		// NO SERVER FIGURE: an old proxy, and every cached-only row. TokensMergedWith returns at
		// its nil guard, so this measures the fold and nothing else — which is what it measured
		// before the server published anything, and therefore the baseline to compare against.
		b.Run(fmt.Sprintf("folded/%d", n), func(b *testing.B) {
			benchFoldedRows(b, n, false)
		})
		// AND WITH ONE, which is the branch production actually takes: every server-listed row
		// carries a published figure now, so the nil case above measures the path a CURRENT proxy
		// never reaches. Adding the merge to this loop was measured through this pair — see
		// pipeline.PromptContextFold.TokensMergedWith for the numbers it is quoted by.
		b.Run(fmt.Sprintf("folded+server/%d", n), func(b *testing.B) {
			benchFoldedRows(b, n, true)
		})
		b.Run(fmt.Sprintf("rescan/%d", n), func(b *testing.B) {
			evs := map[string][]pipeline.SessionEvent{}
			for i := 0; i < 10; i++ {
				evs[fmt.Sprintf("s%d", i)] = benchContextEvents(n)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for _, e := range evs {
					_ = pipeline.PromptContextOf(e)
				}
			}
		})
	}
}

// ALLOCATION-FREEDOM IS ASSERTED HERE, not just reported. b.ReportAllocs in the benchmarks above
// PRINTS 0 allocs/op and nothing fails when that becomes 1, and nobody reads a benchmark on a green
// CI run — so the property the second entry point exists for had no guard at all.
//
// THAT PROPERTY IS THE WHOLE REASON TokensMergedWith EXISTS. Merging by publishing the local fold put
// a 48-byte *PromptContext on the heap per row per rebuild — 356 ns/op and 10 allocs/op on
// BenchmarkSessionContextPerEvent/folded/10000 against 184 before a server figure existed — and the
// escape cannot be optimised away, because MergePromptContext is over the inline budget and its
// parameters flow to its result. See pipeline.PromptContextFold.TokensMergedWith. A change that lets
// the literal escape again would regress the row loop silently; this fails instead.
//
// A TEST RATHER THAN A BENCHMARK, so it runs on every `go test ./...`. testing.AllocsPerRun pins
// GOMAXPROCS to 1 and warms the closure itself, and the steady state it measures is the one the row
// loop is in: the run is already folded and the slice has not grown, so localContextFor returns on
// its length check and writes no map entry.
func TestSessionContextFor_MergesWithoutAllocating(t *testing.T) {
	const id = "s"
	m := &model{events: map[string][]pipeline.SessionEvent{
		id: conversation("c1", time.Now(), 600, 500_000),
	}}
	// Warm, as a running TUI is.
	want := m.sessionContextFor(id, nil)
	if want != 500_000 {
		t.Fatalf("local figure is %d, want 500000 — the fixture is not exercising the fold", want)
	}

	// A FIGURE THAT TIES ON Msgs AND At AND LOSES ON Tokens, which is the DEEPEST path through
	// better(): one disagreeing earlier returns after a single int compare and would measure less of
	// the merge than production does. Derived from the fold so it cannot drift from the fixture, and
	// one token short of it so the local figure still wins and the assertion below holds.
	p := m.contextRun[id].Publish()
	if p == nil {
		t.Fatal("the warm fold published nothing, so this test is not exercising the merge")
	}
	server := &pipeline.PromptContext{Tokens: p.Tokens - 1, Msgs: p.Msgs, At: p.At}

	// Captured rather than asserted inside the closure: a t.Fatalf in there would Goexit mid-run and
	// abandon AllocsPerRun's own GOMAXPROCS restore.
	var got int
	allocs := testing.AllocsPerRun(100, func() {
		got = m.sessionContextFor(id, server)
	})
	if got != want {
		t.Errorf("merged figure is %d, want %d — the server figure must lose to the fold here, or "+
			"this is measuring a different branch", got, want)
	}
	if allocs != 0 {
		t.Errorf("sessionContextFor allocated %v times per call with a server figure, want 0 — the "+
			"48-byte *PromptContext is escaping again; see "+
			"pipeline.PromptContextFold.TokensMergedWith for why that method exists", allocs)
	}
}

// benchFoldedRows is one streamed event's worth of work: ten warm sessions, one of which has a turn
// to fold, asked for their gauges exactly as the row loop asks.
//
// withServer decides whether each row carries a published figure, which is the only difference
// between the two sub-benchmarks above — same fixture, same loop, same slice indexing — so the delta
// between them is the merge and nothing else.
func benchFoldedRows(b *testing.B, n int, withServer bool) {
	m := &model{events: map[string][]pipeline.SessionEvent{}}
	ids := make([]string, 10)
	for i := range ids {
		ids[i] = fmt.Sprintf("s%d", i)
		m.events[ids[i]] = benchContextEvents(n)
		_ = m.sessionContextFor(ids[i], nil) // warm, as a running TUI is
	}
	// THE SLICE IS BUILT ONCE, OUTSIDE THE LOOP, and the run is rewound instead.
	//
	// An earlier version appended the delta inside the b.N loop, which made the measurement a
	// function of how many iterations the sweep chose: the slice grew by a turn every iteration, so
	// a default -benchtime spent most of its time in append and realloc — 152KB/op of it — and the
	// per-event fold this benchmark exists to protect was the small term. It only read correctly
	// under -benchtime 20x, which is a measurement you have to remember to ask for.
	//
	// Rewinding the winner's run to its pre-delta state is the same work with none of the growth:
	// every iteration folds exactly the arriving events for ids[0] and takes the length-check hit
	// for the other nine, which is the shape one streamed event has. What it adds is one map store
	// per iteration, constant and tens of nanoseconds.
	arriving := conversation("new", time.Now(), 999, 900_000)
	head := m.events[ids[0]]
	full := make([]pipeline.SessionEvent, 0, len(head)+len(arriving))
	full = append(append(full, head...), arriving...)
	warm := m.contextRun[ids[0]]
	m.events[ids[0]] = full

	// A SLICE IN LOCKSTEP WITH ids, ALL nil WHEN withServer IS FALSE, so both sub-benchmarks run
	// the identical loop body and index it the identical way. A map would have put a mapaccess on
	// the measured path of one case and not the other, and production reads this off a struct field.
	servers := make([]*pipeline.PromptContext, len(ids))
	if withServer {
		// ONE FIGURE PER SESSION, TIED TO THAT SESSION'S OWN FOLD on Msgs and At, so better()
		// runs past both of the unstated arm's leading comparators and settles on Tokens. That is
		// the DEEPEST path through the comparison and therefore an upper bound on what the merge
		// costs a row: a figure disagreeing on Msgs returns after one int compare, and a STATED
		// one against these unstated fixtures returns on the dominance check before that.
		//
		// Derived from the folds rather than written out, so it cannot drift from the fixture.
		// Tokens is one below the fold's, so the fold wins and both sub-benchmarks return the same
		// figure — the comparison is the only thing that differs.
		//
		// ids[0] is published AFTER its delta is folded, because that is the state the loop below
		// compares against; its run is then rewound to warm like every iteration does.
		_ = m.sessionContextFor(ids[0], nil)
		for i, id := range ids {
			p := m.contextRun[id].Publish()
			if p == nil {
				b.Fatalf("%s published nothing, so this case is not exercising the merge", id)
			}
			servers[i] = &pipeline.PromptContext{Tokens: p.Tokens - 1, Msgs: p.Msgs, At: p.At}
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.contextRun[ids[0]] = warm // as if the delta had only just landed
		for j, id := range ids {
			_ = m.sessionContextFor(id, servers[j])
		}
	}
}
