package usage

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// The guard: an event NumBuckets minutes old shares the current minute's slot, and replaying it
// must not erase that minute; nor may the margin minute or a future event land.
func TestReplay_DropsWhatWouldResetAVisibleBucket(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 30, 0, time.UTC)
	a := New(WithClock(func() time.Time { return now }))
	a.Record("s1", respEvent(now, 200, time.Second, "m", 100))
	for _, at := range []time.Time{
		now.Add(-NumBuckets * BucketWidth), now.Add(-(NumBuckets - 1) * BucketWidth), now.Add(BucketWidth),
	} {
		if a.Replay("s1", respEvent(at, 200, time.Second, "m", 7)) {
			t.Errorf("replayed an event at %v", at)
		}
	}
	if got := a.Snapshot(MaxWindow, MaxWindow, "", GroupNone).Totals.Tokens; got != 100 {
		t.Fatalf("tokens = %d, want only the live event's 100", got)
	}
	if !a.Replay("s1", respEvent(now.Add(-(NumBuckets-2)*BucketWidth), 200, time.Second, "m", 7)) {
		t.Fatal("an event inside the window was dropped")
	}
	if a.Replay("s1", &pipeline.SessionEvent{Phase: pipeline.SessionResponse}) {
		t.Fatal("replayed an event with no time")
	}
}

// ReplayCopy keeps everything Record reads: a ring fed the copies of a fixture that sets every
// bucket field — with a conversation and a stray plugin entry added, which it must drop — is the
// ring fed the events, in every grouping.
func TestReplayCopy_RecordsWhatTheEventDoes(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	full, slim := New(WithClock(clock)), New(WithClock(clock))
	evs := append(richTurn(t, now.Add(-2*time.Minute), "m1", "claude-code", "api.anthropic.com", 40),
		richTurn(t, now.Add(-time.Minute), "m2", "opencode", "api.openai.com", 70)...)
	for _, e := range evs {
		if e.Inference != nil {
			e.Inference.Messages = []pipeline.InferenceMessage{{Role: "user", Content: "a conversation"}}
		}
		if e.Plugins == nil {
			e.Plugins = map[string]json.RawMessage{}
		}
		e.Plugins["mcp-parser"] = json.RawMessage(`{"x":1}`)
		c := ReplayCopy(e)
		if c.Inference != nil && c.Inference.Messages != nil {
			t.Fatal("ReplayCopy kept the conversation")
		}
		if _, ok := c.Plugins["mcp-parser"]; ok {
			t.Fatal("ReplayCopy kept a plugin entry Record does not read")
		}
		full.Record("s1", e)
		slim.Record("s1", &c)
	}
	for _, g := range []Group{GroupNone, GroupModel, GroupEndpoint, GroupSession, GroupAgent,
		GroupCurrency, GroupStatus, GroupPlugin, GroupHost} {
		f := full.Snapshot(10*time.Minute, BucketWidth, "", g)
		s := slim.Snapshot(10*time.Minute, BucketWidth, "", g)
		if !reflect.DeepEqual(f, s) {
			t.Errorf("group %s: the copies record differently:\nfull %+v\nslim %+v", g, f, s)
		}
	}
}

// A copy is one size however long the conversation was: two events that differ only in how much
// conversation they carry, in every field that grows with it, leave copies of the same length.
func TestReplayCopy_DoesNotGrowWithTheConversation(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	size := func(turns int) int {
		e := richTurn(t, now, "m1", "claude-code", "api.anthropic.com", 40)[0]
		text := strings.Repeat("x", 1024*turns)
		for range turns {
			e.Inference.Messages = append(e.Inference.Messages, pipeline.InferenceMessage{Role: "user", Content: text})
			e.Inference.Tools = append(e.Inference.Tools, pipeline.InferenceTool{Name: "Read", Description: text})
			e.Inference.ToolCalls = append(e.Inference.ToolCalls, pipeline.InferenceToolCall{ID: "toolu_1", Name: "Read", Arguments: text})
			e.Inference.ToolResults = append(e.Inference.ToolResults, pipeline.InferenceToolResult{ToolUseID: "toolu_1", Content: text})
		}
		e.Inference.ToolChoice = map[string]any{"type": "tool", "name": text}
		e.Inference.Completion = text
		c := ReplayCopy(e)
		raw, err := json.Marshal(&c)
		if err != nil {
			t.Fatal(err)
		}
		return len(raw)
	}
	if one, many := size(1), size(50); many != one {
		t.Fatalf("a copy grew with the conversation: %d bytes after one turn, %d after fifty", one, many)
	}
}

// FinishReplay adds the replay to what live traffic recorded meanwhile, in every ring: the
// all-sessions ring, an agent's, and a session's — one both sides wrote, and ones only one did.
func TestFinishReplay_AddsTheReplayToLiveTraffic(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 30, 0, time.UTC)
	clock := func() time.Time { return now }
	live := New(WithClock(clock))
	priv := live.BeginReplay()
	cc := func(e *pipeline.SessionEvent) *pipeline.SessionEvent {
		e.Client = &pipeline.EventClient{Name: "claude-code", Version: "1.0"}
		return e
	}
	live.Record("both", cc(respEvent(now, 200, time.Second, "m", 10)))
	live.Record("live", respEvent(now, 200, time.Second, "m", 20))
	priv.Replay("both", cc(respEvent(now.Add(-time.Hour), 200, time.Second, "m", 100)))
	priv.Replay("old", respEvent(now.Add(-2*time.Hour), 200, time.Second, "m", 200))
	live.FinishReplay(priv)

	for id, want := range map[string]int64{"": 330, "both": 110, "live": 20, "old": 200} {
		if got := live.Snapshot(MaxWindow, MaxWindow, id, GroupNone).Totals.Tokens; got != want {
			t.Errorf("session %q: tokens = %d, want %d", id, got, want)
		}
	}
	if got := live.AgentSnapshot(MaxWindow, MaxWindow, "", "claude-code", GroupNone).Totals.Tokens; got != 110 {
		t.Errorf("claude-code's ring: tokens = %d, want 110", got)
	}
}

// The merge keeps the session cap, and what it gives up is the coldest ring: a replayed session
// before one live traffic is writing.
func TestFinishReplay_KeepsTheSessionCap(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 30, 0, time.UTC)
	live := New(WithClock(func() time.Time { return now }), WithMaxSessions(2))
	priv := live.BeginReplay()
	live.Record("live", respEvent(now, 200, time.Second, "m", 1))
	priv.Replay("old", respEvent(now.Add(-2*time.Hour), 200, time.Second, "m", 1))
	priv.Replay("newer", respEvent(now.Add(-time.Hour), 200, time.Second, "m", 1))
	live.FinishReplay(priv)
	for id, want := range map[string]int64{"live": 1, "newer": 1, "old": 0} {
		if got := live.Snapshot(MaxWindow, MaxWindow, id, GroupNone).Totals.Tokens; got != want {
			t.Errorf("session %q: tokens = %d, want %d", id, got, want)
		}
	}
}

// Erasures stop being logged once the replay ends, whichever way: a clear after it must not reach
// the next replay's ring.
func TestAbandonReplay_StopsLogging(t *testing.T) {
	live := New()
	live.BeginReplay()
	live.Cleared()
	live.AbandonReplay()
	live.Cleared()
	if live.replaying || live.replayOps != nil {
		t.Fatalf("after AbandonReplay: replaying=%v, %d ops logged", live.replaying, len(live.replayOps))
	}
}

// A 101 row is excluded on the REPLAY path too, which only holds because
// ReplayCopy is a whole-struct copy (`c := *e`) and so carries every field the
// exclusion reads. There are THREE: StatusCode and Tunnel, and — since the
// exclusion was scoped to outbound — Direction. If ReplayCopy ever became a
// field-by-field copy that dropped or mangled any of them, the exclusion would
// silently stop applying to archived WebSocket sessions and their lifetimes
// would reappear in the latency mean on every restart — see
// TestRecord_WebSocket101IsNotARequestLatency for what that costs.
//
// The first two are asserted directly below; Direction is covered by the
// end-to-end half, which is why that half is not redundant: mangling Direction
// to Inbound moves the mean from 500ms to 300s and fails there.
func TestReplayCopy_KeepsWhatExcludesA101Row(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	ws := respEvent(now, http.StatusSwitchingProtocols, 10*time.Minute, "", 0)
	if c := ReplayCopy(ws); c.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("ReplayCopy dropped StatusCode (%d); the 101 exclusion in Record keys on it",
			c.StatusCode)
	}
	tun := respEvent(now, 200, 4*time.Minute, "", 0)
	tun.Tunnel = true
	if c := ReplayCopy(tun); !c.Tunnel {
		t.Fatal("ReplayCopy dropped Tunnel; the tunnel exclusion in Record keys on it")
	}

	// And end to end through the ring: a replayed 101 changes nothing.
	a := New(WithClock(func() time.Time { return now }))
	a.Record("s1", respEvent(now, 200, 500*time.Millisecond, "", 0))
	before := a.Snapshot(time.Minute, BucketWidth, "", GroupNone)
	c := ReplayCopy(ws)
	a.Record("s1", &c)
	after := a.Snapshot(time.Minute, BucketWidth, "", GroupNone)
	if after.Totals.Requests != before.Totals.Requests {
		t.Errorf("requests = %d, want %d — a replayed 101 must not count either",
			after.Totals.Requests, before.Totals.Requests)
	}
	if weightedLatMean(after) != weightedLatMean(before) {
		t.Errorf("latency mean moved %.1f -> %.1f on replay",
			weightedLatMean(before), weightedLatMean(after))
	}
}
