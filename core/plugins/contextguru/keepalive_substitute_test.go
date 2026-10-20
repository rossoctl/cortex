package contextguru

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/dispatch"
	"github.com/rossoctl/context-guru/keepalive"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// fakeSummaryModel is the components.MessagesModel the registered keep-alive candidate calls
// through — a stand-in for cache_aware_summarizer's own resolved model, so this test exercises
// the keeper's substitution decision against cortexDispatcher without a real model call.
type fakeSummaryModel struct {
	calls int
	out   string
}

func (m *fakeSummaryModel) Complete(context.Context, string) (string, error) { return m.out, nil }

func (m *fakeSummaryModel) CompleteMessages(context.Context, string, []bschemas.ChatMessage) (string, error) {
	m.calls++
	return m.out, nil
}

// A due ping for a session cache_aware_summarizer has registered a keep-alive candidate for
// becomes the summarizer's own call instead of an ordinary HTTP ping — through cortexDispatcher
// exactly as through the standalone proxy's Dispatcher. Proven here by what the fake upstream
// actually received: zero requests, because the substitute never calls cortexDispatcher.Do at
// all — the engine's keepalive.Keeper (keepalive/substitute.go) commissions the candidate's own
// call directly and reaches cortexDispatcher only for Price, which this test also exercises (via
// a fixedResolver) and checks landed in Stats().SpentUSD.
//
// WHAT THIS DOES NOT PROVE: that this path is reachable through today's real Cortex traffic.
// cache_aware_summarizer needs a components.MessagesModel or a components.PrefixAsker to
// register a candidate at all (see its own doc comment), and Cortex's cgModel implements neither
// — only Complete. So in production, as wired in plugin.go today, cache_aware_summarizer never
// actually registers a candidate, and this substitution never fires. This test is the dispatcher
// contract for when that changes (cgModel grows CompleteMessages, or some other MessagesModel-
// capable component is added), exercised directly via offload.RegisterKeepAliveCandidateForTest
// rather than by driving the real pipeline. It also does not exercise Price: a registered
// candidate's usage counters come from cheapmodel's own instrumentation
// (cheapmodel.WithDetachedSink), which this test's fakeSummaryModel bypasses entirely — the same
// reason the engine's own equivalent test (keepalive/substitute_test.go) does not assert a cost
// either.
func TestCortexSubstitutesCacheAwareSummarizerForAPing(t *testing.T) {
	var upstreamHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"input_tokens":1,"cache_read_input_tokens":48576,` +
			`"output_tokens":1}}`))
	}))
	defer srv.Close()

	st := store.NewMemory(store.Options{MaxEntries: 400})
	model := &fakeSummaryModel{out: "<summary>explored the handler, 3 tests fail.</summary>"}
	const session = "cortex-sub-sess-1"
	msg := bschemas.ChatMessage{Role: bschemas.ChatMessageRoleUser}
	schema.SetMessageText(&msg, "fix the failing tests, there is a lot of context here")
	span := []bschemas.ChatMessage{msg}
	ask := append([]bschemas.ChatMessage(nil), span...)
	ask = append(ask, bschemas.ChatMessage{Role: bschemas.ChatMessageRoleUser})
	call := func(ctx context.Context) (string, error) { return model.CompleteMessages(ctx, "", ask) }
	offload.RegisterKeepAliveCandidateForTest(session, st, call, "messages", span, 1, "", 0)
	t.Cleanup(func() { offload.ClearKeepAliveCandidate(session) })

	d := newCortexDispatcher(newPingClient())
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	k := keepalive.New(keepalive.Options{Dispatcher: d, Now: func() time.Time { return now },
		Go: func(f func()) { f() }})
	t.Cleanup(k.Shutdown)

	pol := keepalive.Policy{KeepAlive: true, Idle: 280 * time.Second, MaxPings: 2,
		MaxUSDPerPing: 0.25, MinPrefixTokens: 20000}
	// A Claude-named model: relayServesClaude falls back to the model name when the endpoint
	// (here, httptest's localhost) doesn't resolve to an anthropic.com host, and only keeps a
	// relay warm for a Claude model — this fixture is Anthropic-via-relay on purpose, so it must
	// name one.
	body := []byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`)
	hdr := http.Header{"Authorization": {"Bearer sk-caller"}}
	host := stripScheme(srv.URL)
	// Two requests: a session's FIRST is not tracked (the gate skips it, matching recordFor's own
	// "has this session sent a request before" semantics), so only the second establishes the
	// entry the candidate above substitutes for.
	for i := 0; i < 2; i++ {
		k.Record(keepalive.Record{Key: keepalive.Key{Session: session},
			StartedAt: now.Add(time.Duration(i) * time.Second), Body: body,
			Provider: bschemas.Anthropic, Endpoint: host, Route: "/v1/messages",
			Status: http.StatusOK, Usage: keepalive.Usage{CacheRead: 48576}, UsageOK: true,
			Policy: pol, Header: hdr,
			Hold: func() dispatch.Ref { return d.hold("http", host, hdr) }})
	}

	if n := k.Sweep(now.Add(281 * time.Second)); n != 1 {
		t.Fatalf("the due ping did not fire (%d)", n)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && k.Stats().SummarySubstituted == 0 {
		time.Sleep(time.Millisecond)
	}
	stats := k.Stats()
	if stats.SummarySubstituted != 1 {
		t.Fatalf("SummarySubstituted = %d, want 1", stats.SummarySubstituted)
	}
	if stats.Pings != 1 {
		t.Errorf("Pings = %d, want 1 — a substituted ping must be counted once, as THE ping for "+
			"this idle span", stats.Pings)
	}
	if upstreamHits != 0 {
		t.Errorf("the fake upstream saw %d request(s); the substitute must replace the HTTP ping "+
			"through cortexDispatcher, not add to it", upstreamHits)
	}
	if model.calls != 1 {
		t.Errorf("the summarizer's model was called %d times, want 1", model.calls)
	}
	if b, ok := st.Get(store.SumPrefix + session); !ok || len(b) == 0 {
		t.Error("the substitute call produced no checkpoint — it ran but is not doing the " +
			"compaction half of its job")
	}
}
