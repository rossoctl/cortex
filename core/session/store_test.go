package session

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/pipeline"
)

func TestStore_AppendAndView(t *testing.T) {
	s := New(5*time.Minute, 100, 0)

	ev := pipeline.SessionEvent{
		At:        time.Now(),
		Direction: pipeline.Inbound,
		Phase:     pipeline.SessionRequest,
		A2A:       &pipeline.A2AExtension{Method: "message/send"},
	}
	s.Append("sess-1", ev)

	v := s.View("sess-1")
	if v == nil {
		t.Fatal("View returned nil")
	}
	if v.ID != "sess-1" {
		t.Errorf("View.ID = %q, want %q", v.ID, "sess-1")
	}
	if len(v.Events) != 1 {
		t.Fatalf("View.Events len = %d, want 1", len(v.Events))
	}
	if v.Events[0].A2A.Method != "message/send" {
		t.Errorf("Event.A2A.Method = %q, want %q", v.Events[0].A2A.Method, "message/send")
	}
}

func TestStore_ActiveSession(t *testing.T) {
	s := New(5*time.Minute, 100, 0)

	if id := s.ActiveSession(); id != "" {
		t.Errorf("ActiveSession() = %q, want empty", id)
	}

	s.Append("sess-1", pipeline.SessionEvent{At: time.Now(), Direction: pipeline.Inbound, Phase: pipeline.SessionRequest})
	if id := s.ActiveSession(); id != "sess-1" {
		t.Errorf("ActiveSession() = %q, want %q", id, "sess-1")
	}

	s.Append("sess-2", pipeline.SessionEvent{At: time.Now(), Direction: pipeline.Inbound, Phase: pipeline.SessionRequest})
	if id := s.ActiveSession(); id != "sess-2" {
		t.Errorf("ActiveSession() = %q, want %q", id, "sess-2")
	}
}

func TestStore_MultipleSessions(t *testing.T) {
	s := New(5*time.Minute, 100, 0)

	s.Append("sess-1", pipeline.SessionEvent{At: time.Now(), Direction: pipeline.Inbound, Phase: pipeline.SessionRequest, A2A: &pipeline.A2AExtension{Method: "m1"}})
	s.Append("sess-2", pipeline.SessionEvent{At: time.Now(), Direction: pipeline.Inbound, Phase: pipeline.SessionRequest, A2A: &pipeline.A2AExtension{Method: "m2"}})

	v1 := s.View("sess-1")
	v2 := s.View("sess-2")
	if v1 == nil || v2 == nil {
		t.Fatal("expected both sessions to exist")
	}
	if len(v1.Events) != 1 || v1.Events[0].A2A.Method != "m1" {
		t.Errorf("sess-1 unexpected content: %+v", v1.Events)
	}
	if len(v2.Events) != 1 || v2.Events[0].A2A.Method != "m2" {
		t.Errorf("sess-2 unexpected content: %+v", v2.Events)
	}
}

func TestStore_TTLExpiry(t *testing.T) {
	s := New(50*time.Millisecond, 100, 0)

	s.Append("sess-1", pipeline.SessionEvent{At: time.Now(), Direction: pipeline.Inbound, Phase: pipeline.SessionRequest})

	v := s.View("sess-1")
	if v == nil {
		t.Fatal("expected View to return session before expiry")
	}

	time.Sleep(60 * time.Millisecond)

	v = s.View("sess-1")
	if v != nil {
		t.Error("expected View to return nil after TTL expiry")
	}

	if id := s.ActiveSession(); id != "" {
		t.Errorf("ActiveSession() = %q, want empty after expiry", id)
	}
}

func TestStore_MaxEvents(t *testing.T) {
	s := New(5*time.Minute, 3, 0)

	for i := 0; i < 5; i++ {
		s.Append("sess-1", pipeline.SessionEvent{
			At:        time.Now(),
			Direction: pipeline.Outbound,
			Phase:     pipeline.SessionRequest,
			MCP:       &pipeline.MCPExtension{Method: string(rune('a' + i))},
		})
	}

	v := s.View("sess-1")
	if v == nil {
		t.Fatal("View returned nil")
	}
	if len(v.Events) != 3 {
		t.Fatalf("Events len = %d, want 3 (capped at maxEvents)", len(v.Events))
	}
	if v.Events[0].MCP.Method != "c" {
		t.Errorf("Events[0].MCP.Method = %q, want %q (oldest evicted)", v.Events[0].MCP.Method, "c")
	}
}

func TestStore_ConcurrentAccess(t *testing.T) {
	s := New(5*time.Minute, 1000, 0)
	var wg sync.WaitGroup

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				s.Append("sess-concurrent", pipeline.SessionEvent{
					At:        time.Now(),
					Direction: pipeline.Outbound,
					Phase:     pipeline.SessionRequest,
					MCP:       &pipeline.MCPExtension{Method: "tools/call"},
				})
			}
		}()
	}

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				s.View("sess-concurrent")
				s.ActiveSession()
			}
		}()
	}

	wg.Wait()

	v := s.View("sess-concurrent")
	if v == nil {
		t.Fatal("expected session to exist after concurrent access")
	}
	if len(v.Events) != 1000 {
		t.Errorf("Events len = %d, want 1000", len(v.Events))
	}
}

func TestStore_Cleanup(t *testing.T) {
	s := New(50*time.Millisecond, 100, 0)

	s.Append("sess-old", pipeline.SessionEvent{At: time.Now(), Direction: pipeline.Inbound, Phase: pipeline.SessionRequest})
	time.Sleep(60 * time.Millisecond)

	s.Append("sess-new", pipeline.SessionEvent{At: time.Now(), Direction: pipeline.Inbound, Phase: pipeline.SessionRequest})

	if v := s.View("sess-old"); v != nil {
		t.Error("expected sess-old to be cleaned up")
	}
	if v := s.View("sess-new"); v == nil {
		t.Error("expected sess-new to still exist")
	}
}

func TestStore_ViewNonExistent(t *testing.T) {
	s := New(5*time.Minute, 100, 0)
	if v := s.View("does-not-exist"); v != nil {
		t.Error("expected nil for non-existent session")
	}
}

func TestView_Intents(t *testing.T) {
	v := &pipeline.SessionView{
		ID: "test",
		Events: []pipeline.SessionEvent{
			{Direction: pipeline.Inbound, Phase: pipeline.SessionRequest, A2A: &pipeline.A2AExtension{Method: "message/send"}},
			{Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, MCP: &pipeline.MCPExtension{Method: "tools/call"}},
			{Direction: pipeline.Inbound, Phase: pipeline.SessionRequest, A2A: &pipeline.A2AExtension{Method: "message/send"}},
			{Direction: pipeline.Outbound, Phase: pipeline.SessionResponse, MCP: &pipeline.MCPExtension{Method: "tools/call"}},
		},
	}

	intents := v.Intents()
	if len(intents) != 2 {
		t.Fatalf("Intents() len = %d, want 2", len(intents))
	}
	for _, e := range intents {
		if e.Direction != pipeline.Inbound || e.Phase != pipeline.SessionRequest || e.A2A == nil {
			t.Errorf("unexpected intent event: %+v", e)
		}
	}
}

func TestView_ToolCalls(t *testing.T) {
	v := &pipeline.SessionView{
		ID: "test",
		Events: []pipeline.SessionEvent{
			{Direction: pipeline.Inbound, Phase: pipeline.SessionRequest, A2A: &pipeline.A2AExtension{}},
			{Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, MCP: &pipeline.MCPExtension{Method: "tools/call"}},
			{Direction: pipeline.Outbound, Phase: pipeline.SessionResponse, MCP: &pipeline.MCPExtension{Method: "tools/call"}},
			{Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Inference: &pipeline.InferenceExtension{Model: "llama3.1"}},
		},
	}

	calls := v.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("ToolCalls() len = %d, want 1", len(calls))
	}
	if calls[0].MCP.Method != "tools/call" {
		t.Errorf("ToolCalls()[0].MCP.Method = %q, want %q", calls[0].MCP.Method, "tools/call")
	}
}

func TestView_ToolResponses(t *testing.T) {
	v := &pipeline.SessionView{
		ID: "test",
		Events: []pipeline.SessionEvent{
			{Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, MCP: &pipeline.MCPExtension{Method: "tools/call"}},
			{Direction: pipeline.Outbound, Phase: pipeline.SessionResponse, MCP: &pipeline.MCPExtension{Method: "tools/call"}},
			{Direction: pipeline.Outbound, Phase: pipeline.SessionResponse, Inference: &pipeline.InferenceExtension{}},
		},
	}

	responses := v.ToolResponses()
	if len(responses) != 1 {
		t.Fatalf("ToolResponses() len = %d, want 1", len(responses))
	}
}

func TestView_LastIntent(t *testing.T) {
	v := &pipeline.SessionView{
		ID: "test",
		Events: []pipeline.SessionEvent{
			{Direction: pipeline.Inbound, Phase: pipeline.SessionRequest, A2A: &pipeline.A2AExtension{Method: "first"}},
			{Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, MCP: &pipeline.MCPExtension{}},
			{Direction: pipeline.Inbound, Phase: pipeline.SessionRequest, A2A: &pipeline.A2AExtension{Method: "second"}},
		},
	}

	last := v.LastIntent()
	if last == nil {
		t.Fatal("LastIntent() returned nil")
	}
	if last.A2A.Method != "second" {
		t.Errorf("LastIntent().A2A.Method = %q, want %q", last.A2A.Method, "second")
	}
}

func TestView_LastIntent_Empty(t *testing.T) {
	v := &pipeline.SessionView{
		ID: "test",
		Events: []pipeline.SessionEvent{
			{Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, MCP: &pipeline.MCPExtension{}},
		},
	}

	if last := v.LastIntent(); last != nil {
		t.Error("LastIntent() should be nil when no A2A inbound events exist")
	}
}

func TestStore_MaxSessionsEviction(t *testing.T) {
	s := New(5*time.Minute, 100, 2)
	defer s.Close()

	s.Append("sess-1", pipeline.SessionEvent{At: time.Now(), A2A: &pipeline.A2AExtension{Method: "m1"}})
	time.Sleep(time.Millisecond)
	s.Append("sess-2", pipeline.SessionEvent{At: time.Now(), A2A: &pipeline.A2AExtension{Method: "m2"}})
	time.Sleep(time.Millisecond)
	s.Append("sess-3", pipeline.SessionEvent{At: time.Now(), A2A: &pipeline.A2AExtension{Method: "m3"}})

	if v := s.View("sess-1"); v != nil {
		t.Error("sess-1 should have been evicted (oldest)")
	}
	if v := s.View("sess-2"); v == nil {
		t.Error("sess-2 should still exist")
	}
	if v := s.View("sess-3"); v == nil {
		t.Error("sess-3 should still exist")
	}
}

func TestStore_Close(t *testing.T) {
	s := New(50*time.Millisecond, 100, 0)
	s.Close()
	// Should not panic after close
	s.Append("sess", pipeline.SessionEvent{At: time.Now(), A2A: &pipeline.A2AExtension{Method: "m"}})
	if v := s.View("sess"); v == nil {
		t.Error("store should still function after Close (only background cleanup stops)")
	}
}

func TestView_InferenceRequests(t *testing.T) {
	v := &pipeline.SessionView{
		ID: "test",
		Events: []pipeline.SessionEvent{
			{Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Inference: &pipeline.InferenceExtension{Model: "llama3.1"}},
			{Direction: pipeline.Outbound, Phase: pipeline.SessionResponse, Inference: &pipeline.InferenceExtension{}},
			{Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, MCP: &pipeline.MCPExtension{}},
		},
	}

	reqs := v.InferenceRequests()
	if len(reqs) != 1 {
		t.Fatalf("InferenceRequests() len = %d, want 1", len(reqs))
	}
	if reqs[0].Inference.Model != "llama3.1" {
		t.Errorf("InferenceRequests()[0].Model = %q, want %q", reqs[0].Inference.Model, "llama3.1")
	}
}

func TestStore_Rekey(t *testing.T) {
	s := New(5*time.Minute, 100, 0)

	ev1 := pipeline.SessionEvent{Direction: pipeline.Inbound, A2A: &pipeline.A2AExtension{Method: "message/send"}}
	ev2 := pipeline.SessionEvent{Direction: pipeline.Outbound, MCP: &pipeline.MCPExtension{Method: "tools/call"}}
	s.Append(DefaultSessionID, ev1)
	s.Append(DefaultSessionID, ev2)

	s.Rekey(DefaultSessionID, "ctx-abc")

	if v := s.View(DefaultSessionID); v != nil {
		t.Error("old session should be gone after rekey")
	}
	v := s.View("ctx-abc")
	if v == nil {
		t.Fatal("new session should hold the rekeyed events")
	}
	if len(v.Events) != 2 {
		t.Errorf("events len = %d, want 2", len(v.Events))
	}
	if id := s.ActiveSession(); id != "ctx-abc" {
		t.Errorf("ActiveSession = %q, want %q — activeID should follow rekey", id, "ctx-abc")
	}

	// Next turn appends to the real contextId and still sees the earlier events.
	s.Append("ctx-abc", pipeline.SessionEvent{Direction: pipeline.Inbound})
	if v := s.View("ctx-abc"); v == nil || len(v.Events) != 3 {
		t.Errorf("expected 3 events after second turn, got %v", v)
	}
}

func TestStore_Rekey_NoOpCases(t *testing.T) {
	s := New(5*time.Minute, 100, 0)
	s.Append("sess-a", pipeline.SessionEvent{Direction: pipeline.Inbound})

	// oldID absent: no-op.
	s.Rekey("missing", "sess-b")
	if v := s.View("sess-b"); v != nil {
		t.Error("rekey of missing oldID should not create newID")
	}

	// newID already exists: preserve it, do not clobber.
	s.Append("sess-b", pipeline.SessionEvent{Direction: pipeline.Outbound})
	s.Rekey("sess-a", "sess-b")
	if v := s.View("sess-a"); v == nil {
		t.Error("sess-a should still exist because sess-b was already taken")
	}
	vb := s.View("sess-b")
	if vb == nil || len(vb.Events) != 1 {
		t.Errorf("sess-b should be preserved with its own event, got %v", vb)
	}

	// Empty IDs: no-op, no panic.
	s.Rekey("", "x")
	s.Rekey("x", "")
	s.Rekey("sess-a", "sess-a")
}

func TestStore_Rekey_ActiveIDUntouchedWhenNotOldID(t *testing.T) {
	s := New(5*time.Minute, 100, 0)
	s.Append("sess-a", pipeline.SessionEvent{})
	s.Append("sess-b", pipeline.SessionEvent{}) // activeID is now sess-b

	s.Rekey("sess-a", "sess-a-renamed")

	if id := s.ActiveSession(); id != "sess-b" {
		t.Errorf("ActiveSession = %q, want sess-b (unchanged — rekey did not touch active)", id)
	}
	if v := s.View("sess-a-renamed"); v == nil {
		t.Error("sess-a-renamed should exist")
	}
}

func TestView_FailedEvents(t *testing.T) {
	s := New(5*time.Minute, 100, 0)
	defer s.Close()

	s.Append("sess", pipeline.SessionEvent{Phase: pipeline.SessionRequest, A2A: &pipeline.A2AExtension{}})
	s.Append("sess", pipeline.SessionEvent{Phase: pipeline.SessionResponse, StatusCode: 200})
	s.Append("sess", pipeline.SessionEvent{
		Phase:      pipeline.SessionResponse,
		StatusCode: 503,
		Error:      &pipeline.EventError{Kind: "backend_error", Code: "503"},
	})
	s.Append("sess", pipeline.SessionEvent{
		Phase: pipeline.SessionResponse,
		Error: &pipeline.EventError{Kind: "blocked", Message: "pii"},
	})

	v := s.View("sess")
	if v == nil {
		t.Fatal("View returned nil")
	}

	failed := v.FailedEvents()
	if len(failed) != 2 {
		t.Fatalf("FailedEvents len = %d, want 2", len(failed))
	}
	if failed[0].Error.Kind != "backend_error" {
		t.Errorf("first failed Kind = %q", failed[0].Error.Kind)
	}

	last := v.LastError()
	if last == nil || last.Error.Kind != "blocked" {
		t.Errorf("LastError = %+v, want blocked", last)
	}
}

func TestView_LastError_NoErrors(t *testing.T) {
	s := New(5*time.Minute, 100, 0)
	defer s.Close()
	s.Append("sess", pipeline.SessionEvent{Phase: pipeline.SessionResponse, StatusCode: 200})
	v := s.View("sess")
	if v.LastError() != nil {
		t.Error("LastError should be nil when no errors present")
	}
	if v.FailedEvents() != nil {
		t.Error("FailedEvents should be nil when no errors present")
	}
}

func TestStore_Subscribe_ReceivesAppendedEvents(t *testing.T) {
	s := New(5*time.Minute, 100, 0)
	defer s.Close()

	sub, cancel := s.Subscribe()
	defer cancel()

	s.Append("sess", pipeline.SessionEvent{
		A2A: &pipeline.A2AExtension{Method: "message/send"},
	})

	select {
	case e := <-sub.Events():
		if e.A2A == nil || e.A2A.Method != "message/send" {
			t.Errorf("received wrong event: %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("no event received within 1s")
	}
}

func TestStore_Subscribe_CancelStopsDelivery(t *testing.T) {
	s := New(5*time.Minute, 100, 0)
	defer s.Close()

	sub, cancel := s.Subscribe()
	cancel()

	s.Append("sess", pipeline.SessionEvent{A2A: &pipeline.A2AExtension{}})

	// Channel should be closed; receive returns zero value with ok=false.
	select {
	case _, ok := <-sub.Events():
		if ok {
			t.Error("received event after cancel")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("channel never closed after cancel")
	}
}

func TestStore_Subscribe_SlowConsumerDropsWithoutBlocking(t *testing.T) {
	s := New(5*time.Minute, 10000, 0)
	defer s.Close()

	sub, cancel := s.Subscribe()
	defer cancel()

	// Flood well beyond buffer capacity without consuming — must not block Append.
	total := subscriberChanBuf + 50
	done := make(chan struct{})
	go func() {
		for i := 0; i < total; i++ {
			s.Append("sess", pipeline.SessionEvent{})
		}
		close(done)
	}()

	select {
	case <-done:
		// Good — Append did not block.
	case <-time.After(time.Second):
		t.Fatal("Append blocked on slow consumer")
	}

	if drops := sub.Drops(); drops == 0 {
		t.Errorf("expected some drops, got 0")
	}
}

func TestStore_Subscribe_FanOutToMultipleSubscribers(t *testing.T) {
	s := New(5*time.Minute, 100, 0)
	defer s.Close()

	a, cancelA := s.Subscribe()
	defer cancelA()
	b, cancelB := s.Subscribe()
	defer cancelB()

	s.Append("sess", pipeline.SessionEvent{A2A: &pipeline.A2AExtension{Method: "ping"}})

	gotA := waitEvent(t, a.Events(), time.Second)
	gotB := waitEvent(t, b.Events(), time.Second)
	if gotA.A2A == nil || gotA.A2A.Method != "ping" {
		t.Errorf("A received wrong event: %+v", gotA)
	}
	if gotB.A2A == nil || gotB.A2A.Method != "ping" {
		t.Errorf("B received wrong event: %+v", gotB)
	}
}

func TestStore_Subscribe_CloseStoreUnblocksSubscribers(t *testing.T) {
	s := New(5*time.Minute, 100, 0)
	sub, cancel := s.Subscribe()
	defer cancel()

	s.Close() // must close subscriber channels, not just the cleanup goroutine

	select {
	case _, ok := <-sub.Events():
		if ok {
			t.Error("expected closed channel after Store.Close")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Store.Close did not close subscriber channel")
	}
}

func TestStore_Append_StampsSessionID(t *testing.T) {
	// Consumers (snapshot or stream) need to attribute events — especially
	// outbound ones with no protocol-native session field — to a bucket.
	s := New(5*time.Minute, 100, 0)
	defer s.Close()

	s.Append("bucket-42", pipeline.SessionEvent{MCP: &pipeline.MCPExtension{Method: "tools/call"}})

	v := s.View("bucket-42")
	if v == nil || len(v.Events) != 1 {
		t.Fatalf("expected 1 event, got %+v", v)
	}
	if v.Events[0].SessionID != "bucket-42" {
		t.Errorf("SessionID = %q, want %q", v.Events[0].SessionID, "bucket-42")
	}
}

func TestStore_Rekey_RewritesExistingEventSessionIDs(t *testing.T) {
	// After Rekey, snapshot reads of the new key must show the original
	// events with SessionID updated to the new bucket ID — otherwise
	// downstream consumers see a mismatch between the session's outer ID
	// and each event's inner SessionID.
	s := New(5*time.Minute, 100, 0)
	defer s.Close()

	s.Append(DefaultSessionID, pipeline.SessionEvent{A2A: &pipeline.A2AExtension{}})
	s.Append(DefaultSessionID, pipeline.SessionEvent{MCP: &pipeline.MCPExtension{}})
	s.Rekey(DefaultSessionID, "ctx-abc")

	v := s.View("ctx-abc")
	if v == nil || len(v.Events) != 2 {
		t.Fatalf("expected 2 events under new key, got %+v", v)
	}
	for i, e := range v.Events {
		if e.SessionID != "ctx-abc" {
			t.Errorf("events[%d].SessionID = %q, want %q", i, e.SessionID, "ctx-abc")
		}
	}
}

// TestSumTokens verifies that SessionSummary.TotalTokens aggregates only
// over inference response events. Request events, non-inference responses,
// and events without Inference populated must not contribute.
func TestSumTokens(t *testing.T) {
	evs := []pipeline.SessionEvent{
		// Inference response — counted.
		{
			Phase:     pipeline.SessionResponse,
			Inference: &pipeline.InferenceExtension{TotalTokens: 100},
		},
		// Inference request (has tokens field set, unusual, but must be skipped
		// because request-phase token counts aren't usage).
		{
			Phase:     pipeline.SessionRequest,
			Inference: &pipeline.InferenceExtension{TotalTokens: 99},
		},
		// MCP response — no inference, skipped.
		{
			Phase: pipeline.SessionResponse,
			MCP:   &pipeline.MCPExtension{Method: "tools/call"},
		},
		// Inference response with zero tokens — contributes zero.
		{
			Phase:     pipeline.SessionResponse,
			Inference: &pipeline.InferenceExtension{TotalTokens: 0},
		},
		// Second inference response — counted.
		{
			Phase:     pipeline.SessionResponse,
			Inference: &pipeline.InferenceExtension{TotalTokens: 250},
		},
	}
	got := sumTokens(evs)
	if got != 350 {
		t.Errorf("sumTokens = %d, want 350", got)
	}
	if sumTokens(nil) != 0 {
		t.Error("sumTokens(nil) should be 0")
	}
	if sumTokens([]pipeline.SessionEvent{}) != 0 {
		t.Error("sumTokens([]) should be 0")
	}
}

// costRecord builds the plugin map one session event carries its cost record in.
func costRecord(t *testing.T, ev event.Event) map[string]json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]json.RawMessage{event.Key: raw}
}

// TestSumCost covers the four rules sumCost applies, which sumTokens beside it does not have
// to: pricedness gates the money and not the saving, and a denial can carry either.
//
// One event list rather than four, because the rules interact — the point is what a session
// holding a realistic mixture reports, and a per-rule fixture would pass while the sum over
// all of them was wrong.
func TestSumCost(t *testing.T) {
	evs := []pipeline.SessionEvent{
		// An ordinary priced response with a saving on it. Both figures counted.
		{
			Phase: pipeline.SessionResponse,
			Plugins: costRecord(t, event.Event{CostUSD: 0.25, Settled: true, Provenance: "configured",
				Avoided: []event.Saving{{Component: "tool-prune", TokensAvoided: 100, USD: 0.01, Tier: "input"}}}),
		},
		// UNPRICED, and carrying a saving. No dollars, and the saving still counts: the
		// prompt was pruned whether or not anything managed to price the response.
		{
			Phase: pipeline.SessionResponse,
			Plugins: costRecord(t, event.Event{Source: event.SourceUsageFallback,
				Avoided: []event.Saving{{Component: "tool-prune", TokensAvoided: 200, USD: 0.02, Tier: "input"}}}),
		},
		// A DENIAL carrying a settled figure — the proxy charges for a response whose body
		// never arrived. Counted, which is why this is not restricted to SessionResponse.
		{
			Phase:   pipeline.SessionDenied,
			Plugins: costRecord(t, event.Event{CostUSD: 0.05, Settled: true, Provenance: "authoritative"}),
		},
		// A REQUEST-phase record. Skipped: the cost is settled on the response pass, and
		// counting both halves would double-charge every request that has one.
		{
			Phase:   pipeline.SessionRequest,
			Plugins: costRecord(t, event.Event{CostUSD: 99, Settled: true, Provenance: "configured"}),
		},
		// A PROJECTED saving: observe mode left every byte on the wire, so this is money
		// that WAS spent and must not be reported as avoided.
		{
			Phase: pipeline.SessionResponse,
			Plugins: costRecord(t, event.Event{Source: event.SourceUsageFallback,
				Avoided: []event.Saving{{Component: "tool-prune", TokensAvoided: 5000, USD: 5, Tier: "input", Projected: true}}}),
		},
		// A response with no cost record at all — the common case for non-inference traffic.
		{Phase: pipeline.SessionResponse, MCP: &pipeline.MCPExtension{Method: "tools/call"}},
	}

	cost, avoided := sumCost(evs)
	// 0.25 + 0.05. The request event's $99 is the discriminator: 99_300_000 means the
	// request half was counted too.
	if want := int64(300_000); cost.Micros != want {
		t.Errorf("cost = %d, want %d", cost.Micros, want)
	}
	// 0.01 + 0.02, the priced and the unpriced request alike. 30_000 with the $5 projected
	// entry excluded — 5_030_000 would mean observe-mode figures are being reported as
	// savings.
	if want := int64(30_000); avoided.Micros != want {
		t.Errorf("avoided = %d, want %d", avoided.Micros, want)
	}
	// Nothing here is anywhere near the ceiling, so neither figure may claim to be a bound.
	if cost.Saturated || avoided.Saturated {
		t.Errorf("Saturated set on ordinary figures (%d, %d): every row would render as a floor",
			cost.Micros, avoided.Micros)
	}

	if c, a := sumCost(nil); c.Micros != 0 || a.Micros != 0 {
		t.Errorf("sumCost(nil) = %d, %d; want 0, 0", c.Micros, a.Micros)
	}
}

// A lifetime total must not WRAP, which is the one failure mode that turns a cost column into
// a negative number an operator cannot explain. A gateway's own figure is bounded per request
// and nothing bounds how many requests a session holds, so the ceiling is reachable in
// principle and the clamp is what makes the column safe.
//
// Two maximal figures, which is the smallest case that overflows.
func TestSumCost_SaturatesRatherThanWrapping(t *testing.T) {
	// Just under pricing.MaxCostMicros in dollars, so each record prices at close to the
	// largest figure cost/event will represent. Two of them exceed int64 nowhere near, so the
	// list is padded to reach the ceiling.
	big := event.Event{CostUSD: 9e9, Settled: true, Provenance: "authoritative"}
	one := costRecord(t, big)
	evs := make([]pipeline.SessionEvent, 4096)
	for i := range evs {
		evs[i] = pipeline.SessionEvent{Phase: pipeline.SessionResponse, Plugins: one}
	}

	cost, _ := sumCost(evs)
	if cost.Micros < 0 {
		t.Errorf("cost = %d: a lifetime total wrapped negative, which is the one answer a "+
			"cost column must never print", cost.Micros)
	}
	if cost.Micros != math.MaxInt64 {
		t.Errorf("cost = %d, want the int64 ceiling: the sum should clamp there", cost.Micros)
	}
	// THE CLAMP IS NOT ENOUGH ON ITS OWN, and this is the assertion that says so. MaxInt64
	// micros is about $9.2 trillion — a well-formed dollar amount no consumer can tell apart
	// from a measured one. pricing.MicrosFromUSD refuses an out-of-range figure rather than
	// clamping it for exactly this reason; a sum cannot refuse (the money was really spent),
	// so it clamps AND says it clamped.
	if !cost.Saturated {
		t.Error("the clamped total is not labelled: $9.2 trillion would render as a measured " +
			"figure, which is a wrong number wearing a right label")
	}
}

// The running totals Append maintains must equal what a full walk computes, after appends,
// after a trim, and after a trim that pins an intent event.
//
// THIS IS THE INVARIANT THAT MAKES THE INCREMENT SAFE. ListSessions stopped decoding every
// event on every poll — that was an unbounded json.Unmarshal loop under the read lock, on
// abctl's two-second timer — and reads two integers instead. An incremental total is only as
// good as the recomputation it claims to equal, and the trim is where they can part company:
// trimEventsPinIntent does not drop a plain prefix when it pins an intent, so a caller
// re-deriving the dropped set would subtract the wrong events.
func TestAppend_RunningTotalsMatchAFullRecomputation(t *testing.T) {
	priced := costRecord(t, event.Event{CostUSD: 0.25, Settled: true, Provenance: "configured",
		Avoided: []event.Saving{{Component: "tool-prune", TokensAvoided: 100, USD: 0.01, Tier: "input"}}})

	for _, tc := range []struct {
		name      string
		maxEvents int
		appends   int
		// intentFirst puts an inbound A2A request at the front, which trimEventsPinIntent
		// pins in place rather than evicting — the branch that does not drop a prefix.
		intentFirst bool
	}{
		{name: "uncapped", maxEvents: 0, appends: 40},
		{name: "trimmed", maxEvents: 5, appends: 40},
		{name: "trimmed with a pinned intent", maxEvents: 5, appends: 40, intentFirst: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := New(5*time.Minute, tc.maxEvents, 0)
			defer st.Close()
			if tc.intentFirst {
				st.Append("s1", pipeline.SessionEvent{
					Direction: pipeline.Inbound, Phase: pipeline.SessionRequest,
					A2A: &pipeline.A2AExtension{Method: "message/send"},
				})
			}
			for i := 0; i < tc.appends; i++ {
				st.Append("s1", pipeline.SessionEvent{Phase: pipeline.SessionResponse, Plugins: priced})
			}

			st.mu.RLock()
			sess := st.sessions["s1"]
			wantCost, wantAvoided := sumCost(sess.Events)
			gotCost, gotAvoided := sess.cost, sess.avoided
			held := len(sess.Events)
			moneyLen := len(sess.money)
			// Per-position, not just per-total: a money slice reshaped by a different rule
			// from Events could still sum correctly while attributing every figure to the
			// wrong event, and the next trim would then subtract the wrong ones.
			perEvent := make([]eventMoney, 0, held)
			for i := range sess.Events {
				perEvent = append(perEvent, moneyOf(&sess.Events[i]))
			}
			st.mu.RUnlock()

			// THE PARALLEL-SLICE INVARIANT. entry.money exists so a trim needs no decode, and
			// it is only safe while it stays in lockstep with Events — same length, same
			// order. Nothing but Append and applyTrim may touch either, and this is what
			// fails if something else ever does.
			if moneyLen != held {
				t.Fatalf("entry.money holds %d figures for %d events: the parallel slice has "+
					"drifted, so every subtraction after this trim is attributed wrongly",
					moneyLen, held)
			}
			if len(perEvent) == moneyLen {
				st.mu.RLock()
				for i := range perEvent {
					if sess.money[i] != perEvent[i] {
						t.Errorf("entry.money[%d] = %+v, but event %d decodes to %+v — the two "+
							"slices are the same length in a different order",
							i, sess.money[i], i, perEvent[i])
					}
				}
				st.mu.RUnlock()
			}

			if gotCost.Micros != wantCost.Micros {
				t.Errorf("running cost = %d over %d held events, full walk says %d — the "+
					"increment and the events have parted company",
					gotCost.Micros, held, wantCost.Micros)
			}
			if gotAvoided.Micros != wantAvoided.Micros {
				t.Errorf("running avoided = %d, full walk says %d",
					gotAvoided.Micros, wantAvoided.Micros)
			}
			// And the fixture really did trim, or the trimming cases prove nothing.
			if tc.maxEvents > 0 && held != tc.maxEvents {
				t.Fatalf("session holds %d events with maxEvents %d; no trim happened, so the "+
					"subtraction path was never exercised", held, tc.maxEvents)
			}
		})
	}
}

// promptContextTurn is a request/response pair as the store records one: manifest and message
// count on both sides, token counts on the response, since the provider is the only party that
// tokenizes. ntools == 0 makes it a one-shot, which the rule excludes.
//
// STATED AS THE MAIN AGENT, which is what a current proxy publishes for the interactive thread.
// promptContextTurnAs is the same turn under any other role — and it exists because hard-coding the
// role here meant no store test ever appended an event the rule has to REJECT on it.
func promptContextTurn(id string, at time.Time, msgs, ntools, context int) []pipeline.SessionEvent {
	return promptContextTurnAs(id, at, msgs, ntools, context, pipeline.AgentRoleMain)
}

// promptContextTurnAs is promptContextTurn under a stated role: AgentRoleSubagent for a Task-spawned
// thread, "" for a client that states nothing at all, which is every client that is not Claude Code.
func promptContextTurnAs(id string, at time.Time, msgs, ntools, context int,
	role pipeline.AgentRole) []pipeline.SessionEvent {
	inf := func() *pipeline.InferenceExtension {
		tools := make([]pipeline.InferenceTool, ntools)
		return &pipeline.InferenceExtension{
			Model:     "claude-opus-5",
			Messages:  make([]pipeline.InferenceMessage, msgs),
			Tools:     tools,
			AgentRole: role,
		}
	}
	resp := inf()
	resp.InputTokens, resp.CacheReadTokens = 300, context-300
	return []pipeline.SessionEvent{
		{At: at, RequestID: id, Phase: pipeline.SessionRequest,
			Direction: pipeline.Outbound, Inference: inf()},
		{At: at.Add(time.Second), RequestID: id, Phase: pipeline.SessionResponse,
			Direction: pipeline.Outbound, Inference: resp},
	}
}

// THE FIGURE OUTLIVES THE EVENTS IT WAS READ FROM, which is the opposite of the invariant
// TestAppend_RunningTotalsMatchAFullRecomputation holds for cost.
//
// cost is a SUM and stays equal to sumCost(Events), shedding whatever a trim evicts — that is
// what scopes the COST column exactly like the TOKENS column beside it. This is an EXTREMUM under
// a total order, so a trim has nothing to subtract from it; recomputing it over the survivors
// would report a small conversation when the conversation is large and merely aged out.
//
// THE ASSERTIONS DIVIDE THE WORK, and it is worth saying which does which because the PR
// description credited the wrong one. Making the figure recomputable from Events — the instinct, by
// analogy to cost — reintroduces exactly the bug this field exists to remove, and it is caught by
// `got == nil` BELOW THE LIST: the winning turn has been evicted and every survivor is a one-shot,
// so a recomputing ListSessions reports nothing at all and the Fatal fires first. The negative
// assertion at the end never runs under that regression.
//
// WHAT THE NEGATIVE ASSERTION GUARDS IS THE FIXTURE, which is what its own message says: it fires if
// maxEvents is ever raised far enough for the winning turn to survive, leaving a test that passes
// while exercising no trim at all. That is a real job — this test is otherwise indistinguishable
// from TestAppend_MaintainsThePromptContextFigure — but it is not the regression guard.
func TestAppend_PromptContextSurvivesATrim(t *testing.T) {
	const maxEvents = 4
	s := New(time.Hour, maxEvents, 0)
	defer s.Close()
	base := time.Now()

	// The winning agentic turn, then enough one-shots to evict it. Nothing here is an intent
	// event — planTrim's pin needs an INBOUND A2A request and these are outbound inference —
	// so the trim is a plain FIFO drop and the winning turn really does leave the slice.
	for _, e := range promptContextTurn("win", base, 600, 27, 500_000) {
		s.Append("sess", e)
	}
	for i := 0; i < maxEvents*2; i++ {
		for _, e := range promptContextTurn(fmt.Sprintf("o%d", i),
			base.Add(time.Duration(i+1)*time.Minute), 3, 0, 7_000) {
			s.Append("sess", e)
		}
	}

	var got *pipeline.PromptContext
	for _, sum := range s.ListSessions() {
		if sum.ID == "sess" {
			got = sum.PromptContext
		}
	}
	if got == nil {
		t.Fatal("no figure after the trim — the gauge went blank for a session that reached 500k")
	}
	if got.Tokens != 500_000 {
		t.Errorf("reported %d, want 500000", got.Tokens)
	}

	// THE TRIM REALLY RAN, asserted directly rather than inferred from a figure comparison. View is
	// the accessor for the events an entry still holds — this package has no Snapshot method — and it
	// copies the whole slice.
	live := s.View("sess").Events
	if len(live) != maxEvents {
		t.Fatalf("the entry holds %d events, want the cap of %d — this test's subject is what "+
			"survives a trim, and nothing was trimmed", len(live), maxEvents)
	}
	// AND THE WINNING TURN IS NOT AMONG THE SURVIVORS, which is the precise thing the figure has to
	// outlive. Named by request id, so the check does not depend on how many one-shots it took to
	// push it out.
	for _, e := range live {
		if e.RequestID == "win" {
			t.Fatalf("the 500k turn is still held (seq %d), so the stored figure could have been "+
				"recomputed and this test proves nothing about the trim", e.Seq)
		}
	}

	// And it is NOT recomputable from what survived — the fixture guard described above, kept
	// because it also catches a survivor that happens to carry a 500k figure of its own.
	var fold pipeline.PromptContextFold
	fold.AddAll(live)
	if fold.Tokens() == got.Tokens {
		t.Error("a recomputation over surviving events matched the stored figure, so this test " +
			"is not exercising the trim — raise the one-shot count")
	}
}

// The ordinary path: Append maintains the figure, ListSessions reports it, and a session with
// nothing to say reports nil rather than zero.
func TestAppend_MaintainsThePromptContextFigure(t *testing.T) {
	s := New(time.Hour, 0, 0)
	defer s.Close()
	base := time.Now()
	for _, e := range promptContextTurn("c1", base, 600, 27, 500_000) {
		s.Append("agentic", e)
	}
	for _, e := range promptContextTurn("o1", base, 3, 0, 282_000) {
		s.Append("oneshot", e)
	}

	// Both flagged so an absent session fails loudly instead of the switch below silently
	// asserting nothing for it — a missing case here would otherwise pass.
	var sawAgentic, sawOneshot bool
	for _, sum := range s.ListSessions() {
		switch sum.ID {
		case "agentic":
			sawAgentic = true
			if sum.PromptContext == nil || sum.PromptContext.Tokens != 500_000 {
				t.Errorf("agentic: %+v, want tokens=500000", sum.PromptContext)
			}
			if sum.PromptContext != nil && !sum.PromptContext.Stated {
				t.Error("agentic: Stated=false, but the fixture declares AgentRoleMain")
			}
		case "oneshot":
			sawOneshot = true
			if sum.PromptContext != nil {
				t.Errorf("oneshot: %+v, want nil — no manifest means no conversation to measure",
					sum.PromptContext)
			}
		}
	}
	if !sawAgentic {
		t.Error("\"agentic\" session missing from ListSessions — the assertions above never ran")
	}
	if !sawOneshot {
		t.Error("\"oneshot\" session missing from ListSessions — the assertions above never ran")
	}
}

// summaryFor is the saw-flag dance the two tests above write out by hand: a session missing from
// ListSessions has to fail LOUDLY rather than leave the assertions on it silently unrun.
func summaryFor(t *testing.T, s *Store, id string) SessionSummary {
	t.Helper()
	for _, sum := range s.ListSessions() {
		if sum.ID == id {
			return sum
		}
	}
	t.Fatalf("session %q missing from ListSessions — every assertion on it went unrun", id)
	return SessionSummary{}
}

// A SUBAGENT'S TURN MUST NOT REACH THE WIRE, asserted at the Append boundary and not only one
// package down.
//
// Until promptContextTurnAs existed every store fixture stated AgentRoleMain, so no test here ever
// appended an event the rule has to REJECT on its role: the exclusion rested entirely on the pipeline
// suite, and a store that folded the wrong half of a session would have passed this package.
//
// THE SUBAGENT WINS ON EVERY COMPARATOR better() RANKS BY — later, more messages, larger context — so
// it takes the column under any rule except the one that excludes it outright, and 900,000 against a
// one-million window is the bar an operator would act on.
func TestAppend_PromptContextExcludesASubagent(t *testing.T) {
	s := New(time.Hour, 0, 0)
	defer s.Close()
	base := time.Now()

	for _, e := range promptContextTurn("main", base, 600, 27, 500_000) {
		s.Append("mixed", e)
	}
	for _, e := range promptContextTurnAs("sub", base.Add(time.Minute), 900, 11, 900_000,
		pipeline.AgentRoleSubagent) {
		s.Append("mixed", e)
	}
	// A subagent carries its OWN manifest — 11 tools against the main thread's 27 — which is why the
	// manifest filter cannot exclude it and the role has to.
	for _, e := range promptContextTurnAs("only", base, 900, 11, 900_000,
		pipeline.AgentRoleSubagent) {
		s.Append("subagent-only", e)
	}

	got := summaryFor(t, s, "mixed").PromptContext
	if got == nil {
		t.Fatal("no figure for a session with a main-agent turn in it")
	}
	if got.Tokens != 500_000 {
		t.Errorf("reported %d, want 500000 — the subagent's turn took the column", got.Tokens)
	}
	// A session that is nothing but subagent traffic has no conversation to measure, so the field is
	// absent rather than zero and the gauge draws an em dash.
	if got := summaryFor(t, s, "subagent-only").PromptContext; got != nil {
		t.Errorf("subagent-only session published %+v, want nil", got)
	}
}

// THE ORDERING IS APPLIED AT THIS BOUNDARY, on figures chosen so that LATEST and LARGEST disagree.
//
// Both turns state the role, so better() takes the stated arm and leads with At: the 12,000-token
// turn after a compaction REPLACES the 830,000-token turn before it. Picking figures that disagree is
// the point — a store that took a plain max over token counts would pass an "assert the big one
// survives" test, and that reading is exactly what SessionSummary.PromptContext's doc used to invite
// by calling itself the largest request seen.
//
// SO THIS FIELD IS NOT MONOTONIC and nothing may treat it as a high-water mark. The reported case is
// this one: 999,623 held against a one-million window for ten hours after a compaction left the
// conversation at 400,249 — a bar at 98.6% for a session with 600k of headroom.
//
// APPENDED IN BOTH ORDERS, in two sessions, because the rule is a max over a total order rather than
// a sequential latch: the answer is the ordering's and not the arrival's. Fresh fixtures per session,
// since Append interns strings in place through the shared InferenceExtension pointer.
func TestAppend_PromptContextAppliesTheOrderingNotAMax(t *testing.T) {
	s := New(time.Hour, 0, 0)
	defer s.Close()
	base := time.Now()
	before := func() []pipeline.SessionEvent {
		return promptContextTurn("before", base, 2468, 27, 830_000)
	}
	after := func() []pipeline.SessionEvent {
		return promptContextTurn("after", base.Add(time.Hour), 40, 27, 12_000)
	}
	appendAll := func(id string, turns ...[]pipeline.SessionEvent) {
		for _, turn := range turns {
			for _, e := range turn {
				s.Append(id, e)
			}
		}
	}
	appendAll("in-order", before(), after())
	appendAll("reversed", after(), before())

	for _, id := range []string{"in-order", "reversed"} {
		got := summaryFor(t, s, id).PromptContext
		if got == nil {
			t.Fatalf("%s: no figure for a session with two stated turns", id)
		}
		if got.Tokens != 12_000 {
			t.Errorf("%s: reported %d, want 12000 — the stated arm leads with At, so the turn "+
				"AFTER the compaction holds the column; 830000 would mean a max over tokens",
				id, got.Tokens)
		}
		if !got.Stated {
			t.Errorf("%s: Stated=false, but both fixtures declare AgentRoleMain", id)
		}
		if !got.At.Equal(base.Add(time.Hour).Add(time.Second)) {
			t.Errorf("%s: At=%v, want the later turn's response timestamp", id, got.At)
		}
	}
}

// THE FALLBACK ARM AT THE Append BOUNDARY, which is the third role promptContextTurnAs was written
// for and the one nothing used. Every store fixture stated a role, so the unstated rule — the one
// that runs for every client that is not Claude Code, since agentRole is empty whenever the request
// says nothing — was exercised only in the pipeline tests.
//
// THE SAME TWO TURNS AS TestAppend_PromptContextAppliesTheOrderingNotAMax WITH THE ROLE WITHHELD, so
// the two tests are one comparison rather than two fixtures: 830,000 at 2,468 messages, then 12,000
// at 40 messages an hour later. Stated, better() leads with At and the answer is 12,000. Unstated it
// leads with the message count and the answer is 830,000. The arms DISAGREE on this input, which is
// what makes the assertion below evidence that the fallback ran rather than evidence that something
// ran.
//
// AND HOLDING THE PRE-COMPACTION FIGURE IS THE FALLBACK'S DOCUMENTED COST, not a defect to fix here:
// with no role to read, how long the conversation is separates the main thread from a subagent, and
// a stale figure beats one that flips to a subagent's. See pipeline.PromptContextOf, which also says
// why no window fixes it.
func TestAppend_PromptContextFallsBackToTheMessageCount(t *testing.T) {
	s := New(time.Hour, 0, 0)
	defer s.Close()
	base := time.Now()
	for _, e := range promptContextTurnAs("before", base, 2468, 27, 830_000, "") {
		s.Append("unstated", e)
	}
	for _, e := range promptContextTurnAs("after", base.Add(time.Hour), 40, 27, 12_000, "") {
		s.Append("unstated", e)
	}

	got := summaryFor(t, s, "unstated").PromptContext
	if got == nil {
		t.Fatal("no figure for a session whose turns carry a manifest but state no role")
	}
	if got.Tokens != 830_000 {
		t.Errorf("reported %d, want 830000 — with no role stated the most messages wins; 12000 is "+
			"the STATED arm's answer for these same two turns, so the wrong arm ranked them",
			got.Tokens)
	}
	if got.Stated {
		t.Error("Stated=true for turns that declare no role")
	}
	// The comparator that won has to reach the wire, or a client cannot merge against this figure by
	// the same rule the server ranked it with.
	if got.Msgs != 2468 {
		t.Errorf("Msgs=%d, want 2468 — the published figure must carry the count that won it",
			got.Msgs)
	}
}

// A session the store forgets entirely has no figure: the entry goes, and the fold with it. A
// session recreated under the same id starts fresh, consistent with nextSeq restarting at 1.
func TestPromptContext_WholeEntryEvictionDropsTheFigure(t *testing.T) {
	s := New(time.Hour, 0, 1) // maxSessions=1
	defer s.Close()
	base := time.Now()
	for _, e := range promptContextTurn("c1", base, 600, 27, 500_000) {
		s.Append("first", e)
	}
	for _, e := range promptContextTurn("c2", base.Add(time.Minute), 600, 27, 100_000) {
		s.Append("second", e)
	}
	for _, sum := range s.ListSessions() {
		if sum.ID == "first" {
			t.Error("the evicted session is still listed; this test is not exercising eviction")
		}
	}
	for _, e := range promptContextTurn("c3", base.Add(2*time.Minute), 3, 0, 9_000) {
		s.Append("first", e)
	}
	// Flagged so a "first" absent from the list (the recreate silently failing) fails loudly
	// instead of the check below asserting nothing.
	var sawRecreated bool
	for _, sum := range s.ListSessions() {
		if sum.ID == "first" {
			sawRecreated = true
			if sum.PromptContext != nil {
				t.Errorf("recreated session carried a figure forward: %+v", sum.PromptContext)
			}
		}
	}
	if !sawRecreated {
		t.Error("recreated \"first\" session missing from ListSessions — the assertion above never ran")
	}
}

// NIL MUST SERIALIZE AS AN ABSENT FIELD, not as {"tokens":0}. A client reading zero would draw
// an empty track where the column's contract is an em dash, and "barely used" and "not known"
// are different answers this column has to keep apart.
func TestSessionSummary_PromptContextOmittedWhenNil(t *testing.T) {
	b, err := json.Marshal(SessionSummary{ID: "s"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Case-insensitive: an untagged field marshals under its exact Go name, "PromptContext"
	// (capital P), not "promptContext" — a case-sensitive check here would never see it and
	// would pass whether or not the tag exists, pinning nothing.
	if strings.Contains(strings.ToLower(string(b)), "promptcontext") {
		t.Errorf("a nil figure serialized as %s, want the field absent", b)
	}

	at := time.Now().UTC().Truncate(time.Second)
	b, err = json.Marshal(SessionSummary{
		ID:            "s",
		PromptContext: &pipeline.PromptContext{Tokens: 851_000, Stated: true, At: at, Msgs: 1509},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back SessionSummary
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.PromptContext == nil {
		t.Fatal("the figure did not survive the round trip")
	}
	// All four fields, not the three PromptContext held before Msgs joined it — the fold's
	// current() and better() both compare Msgs, so dropping it here would silently pin a
	// round trip that loses part of the total order.
	if back.PromptContext.Tokens != 851_000 || !back.PromptContext.Stated ||
		!back.PromptContext.At.Equal(at) || back.PromptContext.Msgs != 1509 {
		t.Errorf("round-tripped to %+v", back.PromptContext)
	}
}

// TestListSessions_TotalTokens verifies the aggregate lands on the summary.
func TestListSessions_TotalTokens(t *testing.T) {
	s := New(5*time.Minute, 100, 0)
	s.Append("sess-a", pipeline.SessionEvent{
		Phase:     pipeline.SessionResponse,
		Inference: &pipeline.InferenceExtension{TotalTokens: 42},
	})
	s.Append("sess-a", pipeline.SessionEvent{
		Phase: pipeline.SessionResponse,
		MCP:   &pipeline.MCPExtension{Method: "tools/call"},
	})
	s.Append("sess-b", pipeline.SessionEvent{
		Phase:     pipeline.SessionResponse,
		Inference: &pipeline.InferenceExtension{TotalTokens: 17},
	})
	sums := s.ListSessions()
	if len(sums) != 2 {
		t.Fatalf("expected 2 summaries, got %d", len(sums))
	}
	byID := map[string]int{}
	for _, sum := range sums {
		byID[sum.ID] = sum.TotalTokens
	}
	if byID["sess-a"] != 42 {
		t.Errorf("sess-a TotalTokens = %d, want 42", byID["sess-a"])
	}
	if byID["sess-b"] != 17 {
		t.Errorf("sess-b TotalTokens = %d, want 17", byID["sess-b"])
	}
}

func waitEvent(t *testing.T, ch <-chan pipeline.SessionEvent, d time.Duration) pipeline.SessionEvent {
	t.Helper()
	select {
	case e := <-ch:
		return e
	case <-time.After(d):
		t.Fatalf("timeout waiting for event after %v", d)
		return pipeline.SessionEvent{}
	}
}
