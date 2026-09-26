package usage

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/pipeline"
)

// Aggregator's own doc says "Safe for concurrent use", and until this file nothing in the
// package exercised that: every other test here drives one goroutine, so a clean `-race`
// run over the suite proved only that a single-threaded fold does not race with itself.
// The claim is load-bearing — Record runs synchronously inside session.Store.Append, on
// every proxied response, while /v1/usage polls Snapshot from the API server's goroutines
// — so it gets a test that can actually fail.
//
// Sized so the writers and readers really do interleave: several goroutines per session
// ring, thousands of folds, and readers spinning for the whole run rather than sampling
// once.
const (
	concurrentWriters  = 8
	concurrentTurns    = 250
	concurrentSessions = 4

	// Per-response fixture constants, kept as named values because every expectation
	// below is arithmetic over them.
	turnTokens     = 1050 // 100 input + 900 cache read + 20 cache write + 30 output
	turnCostUSD    = 0.001
	turnCostMicros = 1000
	// Every 5th turn is disclosed inexact and every 25th is an error, so both counts are
	// exact rather than "roughly this many".
	incompleteEvery = 5
	errorEvery      = 25
)

// concurrentTurn builds one turn's two events: the request half carrying a request-only
// plugin (so the pending-map pairing path is exercised under contention too) and the
// response half carrying a full token split, a client, and a published cost record.
//
// The RequestID is unique across all writers, because pairing is by id and a collision
// would make the plugin attribution below nondeterministic for reasons that have nothing
// to do with concurrency.
func concurrentTurn(t *testing.T, at time.Time, writer, turn int) (request, response *pipeline.SessionEvent) {
	t.Helper()
	id := fmt.Sprintf("w%d-t%d", writer, turn)

	resp := &pipeline.SessionEvent{
		At: at, Direction: pipeline.Outbound, Phase: pipeline.SessionResponse,
		RequestID: id, StatusCode: 200, Duration: time.Second,
		Host: "gw.internal",
		Inference: &pipeline.InferenceExtension{
			Model: "claude-opus-5", InputTokens: 100, CacheReadTokens: 900,
			CacheWriteTokens: 20, OutputTokens: 30, ReasoningTokens: 5,
			TotalTokens: turnTokens, PresentKinds: 0b11111, FinishReason: "end_turn",
		},
		Invocations: &pipeline.Invocations{Outbound: []pipeline.Invocation{{
			Plugin: "inference-parser", Phase: pipeline.InvocationPhaseResponse,
		}}},
	}
	// Half the traffic names a client and half names none, so the byAgent axis folds both
	// a real label and the reserved "unknown" bucket while it is being read.
	if writer%2 == 0 {
		resp.Client = &pipeline.EventClient{Name: "claude-code", Version: "2.1.14", Raw: "claude-cli/2.1.14"}
	}
	if turn%errorEvery == errorEvery-1 {
		resp.StatusCode = 500
	}

	ev := event.Event{
		CostUSD: turnCostUSD, Source: event.SourceUsageFallback,
		Provenance: "configured", Settled: true,
	}
	if turn%incompleteEvery == 0 {
		ev.Incomplete = true
		// Both reasons, alternating, so IncompleteBy is a multi-key map under contention
		// rather than a single counter wearing a label.
		ev.IncompleteReason = pricing.ReasonSplitUnreported
		if turn%(incompleteEvery*2) == 0 {
			ev.IncompleteReason = pricing.ReasonOutputUncounted
		}
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	resp.Plugins = map[string]json.RawMessage{event.Key: raw}

	return reqEvent(at, id, "context-guru"), resp
}

// TestAggregator_ConcurrentRecordAndSnapshot runs every write path against every read path
// at once and then reconciles the books.
//
// The assertions are INVARIANTS, not merely "-race said nothing":
//
//   - Nothing is lost. After the writers are joined, every counter equals the arithmetic
//     over what was recorded — requests, tokens, dollars, coverage and the inexactness
//     disclosure alike.
//   - Nothing is torn. Every snapshot a reader saw mid-flight had CostMicros exactly
//     turnCostMicros x Requests. A fold that counted a request outside the lock that adds
//     its dollars, or a Snapshot that read the counters in two passes, would show a
//     request without its money.
//   - Nothing goes backwards. Within one reader, Requests is non-decreasing: the clock is
//     fixed so the bucket only accumulates, and any observation that dropped would mean a
//     write had been lost or a partially-reset bucket read.
//   - The axes reconcile with the total. Per-session rings, the bySession series and the
//     byAgent series each sum to the all-sessions figure, so a concurrent addLabel cannot
//     have dropped a label's worth of traffic while the bucket total kept it.
func TestAggregator_ConcurrentRecordAndSnapshot(t *testing.T) {
	now := time.Now().Truncate(BucketWidth)
	// Exactly as many session rings as sessions, so nothing is evicted mid-run and the
	// per-session reconciliation below is exact.
	a := New(WithClock(func() time.Time { return now }), WithMaxSessions(concurrentSessions))

	const total = concurrentWriters * concurrentTurns
	sessionOf := func(writer int) string { return fmt.Sprintf("s%d", writer%concurrentSessions) }

	// Built before the goroutines start: the fixture calls t.Fatal on a marshal failure,
	// which is not allowed from a non-test goroutine.
	type turn struct{ request, response *pipeline.SessionEvent }
	turns := make([][]turn, concurrentWriters)
	for w := 0; w < concurrentWriters; w++ {
		turns[w] = make([]turn, concurrentTurns)
		for i := 0; i < concurrentTurns; i++ {
			req, resp := concurrentTurn(t, now, w, i)
			turns[w][i] = turn{req, resp}
		}
	}

	done := make(chan struct{})
	var readers, writers sync.WaitGroup

	// READERS: one per read shape, each spinning for the whole run. Failures go through
	// t.Errorf, never t.Fatalf, which must only be called from the test goroutine.
	reads := []struct {
		name    string
		session string
		group   Group
	}{
		{"all sessions, ungrouped", "", GroupNone},
		{"one session ring", "s0", GroupSession},
		{"by agent", "", GroupAgent},
		{"by model", "", GroupModel},
	}
	observed := make([]int, len(reads))
	for i, r := range reads {
		readers.Add(1)
		go func(i int, name, session string, group Group) {
			defer readers.Done()
			var prev int64
			for {
				select {
				case <-done:
					return
				default:
				}
				snap := a.Snapshot(10*BucketWidth, BucketWidth, session, group)
				got := snap.Totals
				if got.Requests < prev {
					t.Errorf("%s: Requests went backwards, %d then %d; a fixed-clock bucket only accumulates, so a write was lost or a resetting bucket was read mid-fold",
						name, prev, got.Requests)
				}
				prev = got.Requests
				if got.Requests > total {
					t.Errorf("%s: Requests = %d, more than the %d turns recorded", name, got.Requests, total)
				}
				if got.CostMicros != got.Requests*turnCostMicros {
					t.Errorf("%s: CostMicros = %d for %d requests, want %d; a request and its dollars must become visible together",
						name, got.CostMicros, got.Requests, got.Requests*turnCostMicros)
				}
				if got.IncompleteRequests > got.PricedRequests {
					t.Errorf("%s: IncompleteRequests %d exceeds PricedRequests %d; it is a subset, and a client subtracting the two would go negative",
						name, got.IncompleteRequests, got.PricedRequests)
				}
				// The returned maps and series must be the reader's own copies. If Snapshot
				// handed back a bucket's internal map, iterating it here — outside the
				// aggregator's lock, while writers fold — is what the race detector is for.
				var seriesRequests int64
				for _, b := range snap.Buckets {
					for _, v := range b.Series {
						seriesRequests += v.Requests
					}
				}
				for _, n := range snap.IncompleteBy {
					seriesRequests += n
				}
				for _, n := range snap.PricedBy {
					seriesRequests += n
				}
				observed[i]++
			}
		}(i, r.name, r.session, r.group)
	}

	// WRITERS: two per session ring, each folding into the shared all-sessions ring and
	// into its session's, with the request half held in the pending map in between.
	for w := 0; w < concurrentWriters; w++ {
		writers.Add(1)
		go func(w int) {
			defer writers.Done()
			session := sessionOf(w)
			for _, tn := range turns[w] {
				a.Record(session, tn.request)
				a.Record(session, tn.response)
			}
		}(w)
	}

	writers.Wait()
	close(done)
	readers.Wait()

	for i, n := range observed {
		if n == 0 {
			t.Errorf("%s: reader completed no snapshots; nothing was read concurrently with the writes and this test proves nothing about that shape", reads[i].name)
		}
	}

	// NOTHING LOST: every counter is the arithmetic over what was recorded.
	got := a.Snapshot(10*BucketWidth, BucketWidth, "", GroupNone).Totals
	wantIncomplete := int64(total / incompleteEvery)
	for _, c := range []struct {
		field string
		got   int64
		want  int64
	}{
		{"Requests", got.Requests, total},
		{"Tokens", got.Tokens, total * turnTokens},
		{"InputTokens", got.InputTokens, total * 100},
		{"CacheReadTokens", got.CacheReadTokens, total * 900},
		{"CacheWriteTokens", got.CacheWriteTokens, total * 20},
		{"OutputTokens", got.OutputTokens, total * 30},
		{"ReasoningTokens", got.ReasoningTokens, total * 5},
		{"CostMicros", got.CostMicros, total * turnCostMicros},
		{"PricedRequests", got.PricedRequests, total},
		{"PriceableRequests", got.PriceableRequests, total},
		{"IncompleteRequests", got.IncompleteRequests, wantIncomplete},
		{"Errors", got.Errors, total / errorEvery},
	} {
		if c.got != c.want {
			t.Errorf("Totals.%s = %d, want %d: %d concurrent turns must add up exactly, and a lost update here is money that vanished",
				c.field, c.got, c.want, total)
		}
	}

	// The inexactness breakdown survives contention and still sums to its counter.
	var byReason int64
	for _, n := range a.Snapshot(10*BucketWidth, BucketWidth, "", GroupNone).IncompleteBy {
		byReason += n
	}
	if byReason != wantIncomplete {
		t.Errorf("IncompleteBy sums to %d, want %d", byReason, wantIncomplete)
	}

	// PER-SESSION RINGS reconcile with the all-sessions total, and each holds only its
	// own writers' traffic.
	var perSession int64
	for s := 0; s < concurrentSessions; s++ {
		id := fmt.Sprintf("s%d", s)
		st := a.Snapshot(10*BucketWidth, BucketWidth, id, GroupNone).Totals
		if want := int64(total / concurrentSessions); st.Requests != want {
			t.Errorf("session %s Requests = %d, want %d", id, st.Requests, want)
		}
		perSession += st.Requests
	}
	if perSession != int64(total) {
		t.Errorf("per-session rings sum to %d, want the all-sessions total %d", perSession, total)
	}

	// THE LABEL AXES reconcile too: a concurrent addLabel that dropped an entry would
	// leave a series short while the bucket total stayed right.
	for _, group := range []Group{GroupSession, GroupAgent, GroupModel, GroupStatus} {
		var sum int64
		for _, b := range a.Snapshot(10*BucketWidth, BucketWidth, "", group).Buckets {
			for _, v := range b.Series {
				sum += v.Requests
			}
		}
		if sum != int64(total) {
			t.Errorf("group=%s series sum to %d requests, want %d", group, sum, total)
		}
	}

	// The request half was paired under contention: its plugin appears with every turn's
	// traffic, not a subset of it.
	if got := pluginSeries(t, a, "")["context-guru"].Requests; got != int64(total) {
		t.Errorf("request-only plugin carries %d requests, want %d: a pending entry was lost or overwritten under concurrency", got, total)
	}
}
