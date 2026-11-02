package tui

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/core/pipeline"
)

// TestStreamRetentionOffScreen: streamed events for sessions the user is not
// watching must not be retained (#1542). handleStreamEvent used to append
// every /v1/events arrival to m.events[sessionID] for EVERY session — never
// interned, never released — so an idle agentop grew at the wire rate
// (~18 MB/min measured live; 1.5 GB after 13h). This is the issue's fixture:
// 3 background sessions x 50 turns, each turn re-sending the whole
// conversation, into a model sitting on the Sessions pane.
func TestStreamRetentionOffScreen(t *testing.T) {
	m := newRetentionModel(t, "on-screen", 0)
	m.pane = paneSessions
	hp := func() uint64 {
		// Enough GCs to settle: a bare double-GC left suite garbage above the
		// 0.6 MB distinct-content bound once in a full-suite run.
		for i := 0; i < 5; i++ {
			runtime.GC()
		}
		var s runtime.MemStats
		runtime.ReadMemStats(&s)
		return s.HeapAlloc
	}
	base := hp()
	for n := 1; n <= 50; n++ {
		for k := 0; k < 3; k++ {
			msgs := make([]pipeline.InferenceMessage, n)
			for i := range msgs {
				msgs[i] = pipeline.InferenceMessage{Role: "user", Content: strings.Clone(fmt.Sprintf("s%d-m%d-", k, i) + strings.Repeat("x", 4096))}
			}
			m.handleStreamEvent(apiclient.StreamEvent{Event: &pipeline.SessionEvent{
				At: time.Now(), SessionID: fmt.Sprintf("bg-%d", k), Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
				Inference: &pipeline.InferenceExtension{Messages: msgs},
			}})
		}
	}
	// SIGNED, because HeapAlloc can go DOWN between the two reads — the base
	// includes garbage from earlier tests and five GCs can return it, which a
	// uint64 subtraction wraps to ~2^64 (the first CI run of this test failed
	// printing "18446744073709.5 MB retained"). A negative delta passes: "heap
	// shrank" is not retention, and the deterministic assertion below is the
	// real guard.
	retained := int64(hp()) - int64(base)
	distinct := uint64(3 * 50 * 4110)
	t.Logf("heap +%.1f MB retained; distinct content %.1f MB", float64(retained)/1e6, float64(distinct)/1e6)
	// Before the fix this held 18.9 MB — 31x the distinct content. After it,
	// nothing is retained for sessions nobody is watching. The bound is a
	// fixed 2 MB rather than the distinct content (0.6 MB): the deterministic
	// assertion is held-events == 0 below, this one guards the heap, and a
	// full-suite run leaves more than 0.6 MB of uncollected garbage after
	// even five GCs. The bug's signature is 18.9 MB — an order above.
	if retained > 2<<20 {
		t.Errorf("off-screen streamed events retained %.1f MB; only the watched session may retain events (bug signature was 18.9 MB)",
			float64(retained)/1e6)
	}
	for _, id := range []string{"bg-0", "bg-1", "bg-2"} {
		if len(m.events[id]) != 0 {
			t.Errorf("session %s (never opened) holds %d streamed events", id, len(m.events[id]))
		}
	}
	// The summary the handler owes every session regardless of retention.
	if len(m.sessions) != 4 {
		t.Errorf("sessions = %d, want 4 (on-screen stub + 3 streamed)", len(m.sessions))
	}
}

// The same events for the WATCHED session are still retained — the
// pane-agnostic selectedSess, so walking events → sessions → events does not
// punch a gap in the live tail.
func TestStreamRetentionWatchedSessionStillAppends(t *testing.T) {
	m := newRetentionModel(t, "watched", 0)
	m.pane = paneSessions // not even the events pane — selectedSess is what counts
	for i := 0; i < 10; i++ {
		m.handleStreamEvent(apiclient.StreamEvent{Event: &pipeline.SessionEvent{
			At: time.Now(), SessionID: "watched", Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
		}})
	}
	if len(m.events["watched"]) != 10 {
		t.Errorf("watched session holds %d events, want 10", len(m.events["watched"]))
	}
}

// The timeline's session is watched too, even though openTimeline does not
// set selectedSess.
func TestStreamRetentionTimelineSessionStillAppends(t *testing.T) {
	m := newRetentionModel(t, "other", 0)
	m.pane = paneTimeline
	m.timeline.session = "tl-sess"
	for i := 0; i < 5; i++ {
		m.handleStreamEvent(apiclient.StreamEvent{Event: &pipeline.SessionEvent{
			At: time.Now(), SessionID: "tl-sess", Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
		}})
	}
	if len(m.events["tl-sess"]) != 5 {
		t.Errorf("timeline session holds %d events, want 5", len(m.events["tl-sess"]))
	}
}

// Leaving the timeline pane stops the watch: timeline.session is a #1542
// retention key, so Esc clears it — otherwise a session glanced at once in the
// timeline kept streaming its events for the rest of the run.
func TestStreamRetentionTimelineStopsAfterEsc(t *testing.T) {
	m := newRetentionModel(t, "other", 0)
	m.pane = paneTimeline
	m.timeline.session = "tl-sess"
	m.timeline.returnPane = paneSessions // openTimeline always records one; the zero value is paneNamespaces
	m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.pane != paneSessions || m.timeline.session != "" {
		t.Fatalf("esc left pane=%v timeline.session=%q, want sessions pane and no session", m.pane, m.timeline.session)
	}
	m.handleStreamEvent(apiclient.StreamEvent{Event: &pipeline.SessionEvent{
		At: time.Now(), SessionID: "tl-sess", Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
	}})
	if len(m.events["tl-sess"]) != 0 {
		t.Errorf("session left with the timeline still holds %d streamed events", len(m.events["tl-sess"]))
	}
}

// The Enter drill-down is an exit from the timeline too: openEventsAtSeq moves
// the pane to Events while the session stays watched through selectedSess, so
// timeline.session must be cleared there as Esc clears it — otherwise, once the
// user opens another session, the timeline's old session keeps streaming into
// m.events for the rest of the run.
func TestStreamRetentionTimelineStopsAfterDrillDown(t *testing.T) {
	m := newRetentionModel(t, "other", 0)
	m.pane = paneTimeline
	m.timeline.session = "tl-sess"
	m.openEventsAtSeq("tl-sess", 8)
	if m.pane != paneEvents || m.selectedSess != "tl-sess" {
		t.Fatalf("drill-down left pane=%v selectedSess=%q, want events pane on tl-sess", m.pane, m.selectedSess)
	}
	// Drilled in, the session is still watched — through selectedSess now.
	m.handleStreamEvent(apiclient.StreamEvent{Event: &pipeline.SessionEvent{
		At: time.Now(), SessionID: "tl-sess", Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
	}})
	if len(m.events["tl-sess"]) != 1 {
		t.Fatalf("drilled-in session holds %d events, want 1 (watched via selectedSess)", len(m.events["tl-sess"]))
	}
	// The user opens session C — the state Enter on Sessions leaves.
	m.selectedSess = "other"
	m.handleStreamEvent(apiclient.StreamEvent{Event: &pipeline.SessionEvent{
		At: time.Now(), SessionID: "tl-sess", Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
	}})
	if len(m.events["tl-sess"]) != 1 {
		t.Errorf("session the timeline held before the drill-down gained %d streamed events after the user moved on",
			len(m.events["tl-sess"])-1)
	}
}

// Dropped events still count in the pipeline pane: model.pluginRuns is folded
// at stream arrival for EVERY session, so the EVENTS column stays alive with
// nothing open instead of describing only the watched session.
func TestPluginRunsCountUnwatchedStream(t *testing.T) {
	m := newRetentionModel(t, "on-screen", 0)
	m.pane = paneSessions
	m.handleStreamEvent(apiclient.StreamEvent{Event: &pipeline.SessionEvent{
		At: time.Now(), SessionID: "bg", Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
		Invocations: &pipeline.Invocations{Outbound: []pipeline.Invocation{{Plugin: "token-exchange"}, {Plugin: "token-exchange"}}},
	}})
	if got := m.pluginRuns["token-exchange"]; got != 2 {
		t.Errorf("pluginRuns[token-exchange] = %d, want 2 (counted at arrival, not retention)", got)
	}
	if len(m.events["bg"]) != 0 {
		t.Errorf("un-watched session retained %d events", len(m.events["bg"]))
	}
}
