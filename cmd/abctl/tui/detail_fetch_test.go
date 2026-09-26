package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/pipeline"
)

// The predicate decides whether Enter costs a round trip. Every arm matters: not
// fetching when the bodies are missing leaves the detail pane permanently
// incomplete, and fetching when they are already here spends a request per
// keystroke for bytes abctl is holding.
func TestNeedsFullEvent(t *testing.T) {
	withInference := &pipeline.SessionEvent{Seq: 1, Inference: &pipeline.InferenceExtension{}}
	withA2A := &pipeline.SessionEvent{Seq: 2, A2A: &pipeline.A2AExtension{}}
	withMCP := &pipeline.SessionEvent{Seq: 3, MCP: &pipeline.MCPExtension{}}
	tunnel := &pipeline.SessionEvent{Seq: 4, Host: "example.com:443"}

	for _, tc := range []struct {
		name      string
		projects  bool
		fetched   bool
		event     *pipeline.SessionEvent
		want      bool
		reasoning string
	}{
		{"projected inference", true, false, withInference, true, "messages were stripped"},
		{"projected a2a", true, false, withA2A, true, "artifact was stripped"},
		{"projected mcp", true, false, withMCP, true, "params/result were stripped"},
		{"projected tunnel", true, false, tunnel, false, "no protocol extension, so nothing was stripped"},
		{"old proxy sent everything", false, false, withInference, false, "re-fetching bytes we already hold"},
		{"nil event", true, false, nil, false, "nothing to fetch"},
		// The case that was missing, and the whole reason this is tracked rather
		// than inferred: a plain text RESPONSE carries none of the dropped fields
		// even after a successful fetch, so a body-presence check would ask for it
		// again on every open.
		{"already fetched", true, true, withInference, false, "the bodies are already here"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &model{serverProjects: tc.projects, selectedSess: "s1"}
			if tc.fetched && tc.event != nil {
				m.markFullFetched("s1", tc.event.Seq)
			}
			if got := m.needsFullEvent(tc.event); got != tc.want {
				t.Errorf("needsFullEvent = %v, want %v — %s", got, tc.want, tc.reasoning)
			}
		})
	}
}

// A Seq recorded as fetched must not be trusted for a different session, or
// switching pods would show one session's row as complete on the strength of
// another's fetch.
func TestNeedsFullEvent_MarkIsPerSession(t *testing.T) {
	e := &pipeline.SessionEvent{Seq: 7, Inference: &pipeline.InferenceExtension{}}
	m := &model{serverProjects: true, selectedSess: "s1"}
	m.markFullFetched("s1", 7)

	if m.needsFullEvent(e) {
		t.Error("s1/seq7 was fetched; it should not be fetched again")
	}
	m.selectedSess = "s2"
	if !m.needsFullEvent(e) {
		t.Error("s2 has never fetched seq 7 — it must not inherit s1's mark")
	}
}

// detailModel builds a model parked on the detail pane for one event.
func detailModel(t *testing.T, sess string, ev *pipeline.SessionEvent) *model {
	t.Helper()
	m := newEventsPaneModel([]pipeline.SessionEvent{*ev})
	m.selectedSess = sess
	m.pane = paneDetail
	m.detailRow = eventRow{event: ev}
	m.detailEvent = ev
	m.detailVp.Width = 80
	m.detailVp.Height = 20
	return m
}

// The fetched bodies have to actually reach the pane, or the round trip bought
// nothing.
func TestApplyDetailEvent_InstallsTheFullEvent(t *testing.T) {
	summary := &pipeline.SessionEvent{
		Seq: 7, Host: "h", Inference: &pipeline.InferenceExtension{Model: "opus"},
	}
	m := detailModel(t, "s1", summary)

	full := &pipeline.SessionEvent{
		Seq: 7, Host: "h",
		Inference: &pipeline.InferenceExtension{
			Model:      "opus",
			Messages:   []pipeline.InferenceMessage{{Role: "user", Content: "SENTINEL-BODY"}},
			Completion: "SENTINEL-COMPLETION",
		},
	}
	m.applyDetailEvent(detailEventLoadedMsg{sessionID: "s1", seq: 7, event: full})

	if m.detailEvent == nil || len(m.detailEvent.Inference.Messages) != 1 {
		t.Fatal("the full event was not installed")
	}
	rendered := m.detailVp.View()
	if !strings.Contains(rendered, "SENTINEL-BODY") {
		t.Errorf("the message body did not reach the pane:\n%s", rendered)
	}
}

// A reply for a row the operator has left must be dropped. Otherwise moving the
// cursor while a fetch is in flight redraws the detail of the previous row —
// content that looks authoritative and describes something else.
func TestApplyDetailEvent_IgnoresStaleReplies(t *testing.T) {
	onScreen := &pipeline.SessionEvent{Seq: 7, Host: "h", Inference: &pipeline.InferenceExtension{}}

	for _, tc := range []struct {
		name string
		msg  detailEventLoadedMsg
	}{
		{"different seq", detailEventLoadedMsg{sessionID: "s1", seq: 99,
			event: &pipeline.SessionEvent{Seq: 99, Host: "OTHER"}}},
		{"different session", detailEventLoadedMsg{sessionID: "s2", seq: 7,
			event: &pipeline.SessionEvent{Seq: 7, Host: "OTHER"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := detailModel(t, "s1", onScreen)
			m.applyDetailEvent(tc.msg)
			if m.detailEvent.Host != "h" {
				t.Errorf("a stale reply replaced the pane: host = %q", m.detailEvent.Host)
			}
		})
	}

	t.Run("left the pane", func(t *testing.T) {
		m := detailModel(t, "s1", onScreen)
		m.pane = paneEvents
		m.applyDetailEvent(detailEventLoadedMsg{sessionID: "s1", seq: 7,
			event: &pipeline.SessionEvent{Seq: 7, Host: "OTHER"}})
		if m.detailEvent.Host != "h" {
			t.Error("a reply was applied after the operator left the detail pane")
		}
	})
}

// A failed fetch must not destroy what is already rendered: the summary is correct
// and useful, it is only missing the bodies.
func TestApplyDetailEvent_FailureKeepsTheRenderedSummary(t *testing.T) {
	summary := &pipeline.SessionEvent{
		Seq: 7, Host: "SUMMARY-HOST", Inference: &pipeline.InferenceExtension{Model: "opus"},
	}
	m := detailModel(t, "s1", summary)
	m.showDetail(m.detailRow, true)
	before := m.detailVp.View()

	m.applyDetailEvent(detailEventLoadedMsg{
		sessionID: "s1", seq: 7, err: errors.New("boom"),
	})

	if m.detailEvent.Host != "SUMMARY-HOST" {
		t.Error("the rendered summary was replaced on failure")
	}
	if m.detailVp.View() != before {
		t.Error("the pane was redrawn on failure — the summary is still the best thing to show")
	}
	if !strings.Contains(m.flash, "boom") {
		t.Errorf("the failure was not reported to the operator: flash = %q", m.flash)
	}
}

// serverProjects is learned from the echo, never assumed — it is what stops abctl
// fetching against a proxy that already sent whole events.
func TestSnapshotLoaded_LearnsWhetherTheServerProjects(t *testing.T) {
	for _, projected := range []bool{true, false} {
		m := newEventsPaneModel(nil)
		m.selectedSess = "s1"
		updated, _ := m.Update(snapshotLoadedMsg{id: "s1", projected: projected})
		if got := updated.(*model).serverProjects; got != projected {
			t.Errorf("serverProjects = %v, want %v", got, projected)
		}
	}
}

// The full event has to land in the slice abctl holds, not just on detailRow, or
// ↵ → Esc → ↵ on the same row pays the round trip every time.
func TestApplyDetailEvent_WritesBackToTheHeldSlice(t *testing.T) {
	summary := &pipeline.SessionEvent{
		Seq: 7, Host: "h", Inference: &pipeline.InferenceExtension{Model: "opus"},
	}
	m := detailModel(t, "s1", summary)
	m.events = map[string][]pipeline.SessionEvent{
		"s1": {{Seq: 6, Host: "other"}, *summary, {Seq: 8, Host: "other"}},
	}

	full := &pipeline.SessionEvent{
		Seq: 7, Host: "h",
		Inference: &pipeline.InferenceExtension{
			Model:    "opus",
			Messages: []pipeline.InferenceMessage{{Role: "user", Content: "BODY"}},
		},
	}
	m.applyDetailEvent(detailEventLoadedMsg{sessionID: "s1", seq: 7, event: full})

	held := m.events["s1"]
	if len(held[1].Inference.Messages) != 1 {
		t.Error("the held event was not upgraded; the next open would re-fetch")
	}
	// Matched by Seq, so its neighbours are untouched.
	if held[0].Seq != 6 || held[2].Seq != 8 {
		t.Error("the write-back disturbed neighbouring rows")
	}
	// A Seq the session does not hold is a no-op, not a panic: eviction and pod
	// switches both make this normal.
	m.applyDetailEvent(detailEventLoadedMsg{sessionID: "s1", seq: 7,
		event: &pipeline.SessionEvent{Seq: 999}})
}

// `y` writes the event's JSON out for debugging, and handing somebody a body-less
// event that looks complete is a bad surprise — so the flash says when the bodies
// are not in yet. Now exactly needsFullEvent, which is the point: two lists that
// could disagree is what produced the bug below.
func TestDetailIsProjected(t *testing.T) {
	inf := func(seq uint64) *pipeline.SessionEvent {
		return &pipeline.SessionEvent{Seq: seq, Inference: &pipeline.InferenceExtension{Model: "opus"}}
	}
	for _, tc := range []struct {
		name     string
		projects bool
		fetched  bool
		event    *pipeline.SessionEvent
		want     bool
	}{
		{"summary awaiting its bodies", true, false, inf(1), true},
		{"bodies have landed", true, true, inf(2), false},
		{"old proxy sent everything", false, false, inf(3), false},
		{"no protocol extension at all", true, false, &pipeline.SessionEvent{Seq: 4, Host: "h"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &model{serverProjects: tc.projects, selectedSess: "s1", detailEvent: tc.event}
			if tc.fetched {
				m.markFullFetched("s1", tc.event.Seq)
			}
			if got := m.detailIsProjected(); got != tc.want {
				t.Errorf("detailIsProjected = %v, want %v", got, tc.want)
			}
		})
	}
}

// THE SHAPE THAT BROKE IT: a tool-only response turn — tool_use blocks and no text
// block. ToolCalls is set, and Messages/Tools are request-side so they are empty on
// a response, and Completion is empty because there was no text. The old
// body-presence check tested Messages and Tools only, so this event stayed
// "projected" after a successful fetch and `y` told the operator to re-yank
// something already complete — advice that could never come true.
//
// Equally broken for a plain TEXT response, which has no dropped field populated at
// all. That is why the mark is tracked rather than inferred.
func TestDetailIsProjected_ToolOnlyAndTextOnlyResponses(t *testing.T) {
	for _, tc := range []struct {
		name  string
		infer *pipeline.InferenceExtension
	}{
		{"tool-only response", &pipeline.InferenceExtension{
			ToolCalls: []pipeline.InferenceToolCall{{Name: "search"}},
		}},
		{"text-only response", &pipeline.InferenceExtension{
			Completion: "hello", FinishReason: "stop",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &pipeline.SessionEvent{Seq: 9, Inference: tc.infer}
			m := &model{serverProjects: true, selectedSess: "s1", detailEvent: e}

			if !m.detailIsProjected() {
				t.Fatal("before the fetch this is a summary")
			}
			m.markFullFetched("s1", 9)
			if m.detailIsProjected() {
				t.Error("after a successful fetch the event is complete; the yank note must stop")
			}
			if m.needsFullEvent(e) {
				t.Error("after a successful fetch re-opening must not fetch again")
			}
		})
	}
}
