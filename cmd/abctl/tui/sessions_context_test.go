package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// THE RULE ITSELF IS TESTED IN core/pipeline/promptcontext_test.go, against
// pipeline.PromptContextOf, with its OWN copy of the fixtures below — see that package's
// promptcontext_fixtures_test.go for why they are duplicated rather than shared.
//
// What stays here is what still needs a *model: sessions_context_fold_test.go and
// sessions_context_wire_test.go drive sessionContextFor and the real handlers — caching,
// rebasing, projection — through pipeline.PromptContextOf, and sessions_context_bench_test.go
// times it. TestContextGauge and the two table tests below are rendering, not the rule. The
// fixtures stay in this file because those three other files still call them.

// toolsOf builds a manifest of n tools. Only its LENGTH matters to the rule: a request that
// carries any tools is an agentic conversation, one that carries none is a one-shot completion.
func toolsOf(n int) []pipeline.InferenceTool {
	out := make([]pipeline.InferenceTool, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, pipeline.InferenceTool{Name: fmt.Sprintf("Tool%02d", i)})
	}
	return out
}

// exchange is a request/response pair as the store records one: the manifest and the message count
// on both sides, the token counts on the response, since the provider is the only party that
// tokenizes.
func exchange(id string, at time.Time, msgs, ntools, input, cacheRead int) []pipeline.SessionEvent {
	inf := func() *pipeline.InferenceExtension {
		return &pipeline.InferenceExtension{
			Model:    "claude-opus-5",
			Messages: make([]pipeline.InferenceMessage, msgs),
			Tools:    toolsOf(ntools),
		}
	}
	respInf := inf()
	respInf.InputTokens, respInf.CacheReadTokens = input, cacheRead
	return []pipeline.SessionEvent{
		{At: at, RequestID: id, Phase: pipeline.SessionRequest,
			Direction: pipeline.Outbound, Inference: inf()},
		{At: at.Add(time.Second), RequestID: id, Phase: pipeline.SessionResponse,
			Direction: pipeline.Outbound, Inference: respInf},
	}
}

// conversation is an agentic turn: 27 tools, as every main-thread request measured carried.
func conversation(id string, at time.Time, msgs, context int) []pipeline.SessionEvent {
	return exchange(id, at, msgs, 27, 300, context-300)
}

// oneShot is a title / quota / summary call: no tools, three messages, and — the part that
// matters — a context that can be large.
func oneShot(id string, at time.Time, context int) []pipeline.SessionEvent {
	return exchange(id, at, 3, 0, 300, context-300)
}

// roled states the caller's role on every event of a turn, which is what a proxy that reads
// Claude Code's billing-header line publishes.
//
// conversation and oneShot above state NOTHING, so every test written before this field existed
// exercises the fallback rule — and that is deliberate rather than an oversight: the two rules
// have to be pinned separately, because the fallback is a PERMANENT path rather than a
// version-skew relic. An earlier version of this comment said "a proxy older than the field is the
// case the fallback is for", which reads stated as a proxy capability; it is a CLIENT property.
// inferenceparser.agentRole returns "" for every client that is not Claude Code — it requires the
// x-anthropic-billing-header: prefix on the first system line — so no proxy version retires the
// unstated arm. mainAgent and subagent below are their stated counterparts.
func roled(evs []pipeline.SessionEvent, role pipeline.AgentRole) []pipeline.SessionEvent {
	for i := range evs {
		evs[i].Inference.AgentRole = role
	}
	return evs
}

// mainAgent is a conversation turn whose request declared itself the interactive thread.
func mainAgent(id string, at time.Time, msgs, context int) []pipeline.SessionEvent {
	return roled(conversation(id, at, msgs, context), pipeline.AgentRoleMain)
}

// subagent is a Task-spawned agent's turn. It carries its OWN tool manifest, which is why the
// manifest alone cannot filter it out — measured at 11 tools against the main thread's 27.
func subagent(id string, at time.Time, msgs, context int) []pipeline.SessionEvent {
	return roled(conversation(id, at, msgs, context), pipeline.AgentRoleSubagent)
}

func TestContextGauge(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tokens int
		width  int
		want   string
	}{
		// The track is always drawn, so a scale is visible on every row.
		{"half full", 500_000, 10, "▕████    ▏"},
		{"nearly full", 991_200, 10, "▕███████▉▏"},
		{"full", 1_000_000, 10, "▕████████▏"},
		// A NON-ZERO CONTEXT NEVER RENDERS AS AN EMPTY TRACK. tierBar's rule, inherited: 8,200
		// of a million is 0.8%, which rounds to no block at all, so it gets the sliver instead.
		// An empty track beside a live session reads as a rendering fault.
		{"a sliver rather than nothing", 8_200, 10, "▕▏       ▏"},
		// Unknown is the em dash COST and SAVED use, and it must not be confusable with the
		// sliver above — which is the whole reason the brackets are drawn.
		{"unknown", 0, 10, emptyCell},
		// Over the window: capped rather than overflowing its column.
		{"past the window", 1_400_000, 10, "▕████████▏"},
		// Narrow terminals shrink the track with the column.
		{"the narrowest useful gauge", 500_000, 3, "▕▌▏"},
		// Below that there is nothing to draw; a bracket pair alone would claim a scale it
		// cannot show.
		{"too narrow to say anything", 500_000, 2, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := contextGauge(tc.tokens, tc.width)
			if got != tc.want {
				t.Errorf("contextGauge(%d, %d) = %q, want %q", tc.tokens, tc.width, got, tc.want)
			}
			// Exactly the column's width, so every row's gauge starts and ends in the same
			// place and the fills can be compared down the column by eye.
			if tc.want != "" && tc.want != emptyCell {
				if w := lipgloss.Width(got); w != tc.width {
					t.Errorf("gauge is %d columns, want %d: %q", w, tc.width, got)
				}
			}
		})
	}
}

// THE COLUMN REPLACED ACTIVE, and the row arity follows the header — which is the invariant
// rebuildSessionsTable's own comment calls a crash rather than a cosmetic bug.
func TestSessionsTable_ContextColumnReplacesActive(t *testing.T) {
	m := &model{width: 200}
	m.sessionsTbl = newSessionsTable()
	m.sessions = []session.SessionSummary{{
		ID: "ctx", UpdatedAt: time.Now(), EventCount: 3, TotalTokens: 500_000,
	}}
	m.events = map[string][]pipeline.SessionEvent{
		"ctx": conversation("c1", time.Now(), 600, 500_000),
	}
	m.rebuildSessionsTable()

	cols := m.sessionsTbl.Columns()
	for _, c := range cols {
		if headerTitle(c) == "ACTIVE" {
			t.Error("the ACTIVE column is still here")
		}
	}
	last := headerTitle(cols[len(cols)-1])
	if last != contextColumnTitle {
		t.Fatalf("last column is %q, want %s", last, contextColumnTitle)
	}
	// The heading states the denominator, because a gauge with no scale is a decoration.
	if !strings.Contains(contextColumnTitle, "1M") {
		t.Errorf("the heading does not name its denominator: %q", contextColumnTitle)
	}

	row := m.sessionsTbl.Rows()[0]
	if len(row) != len(cols) {
		t.Fatalf("row has %d cells against %d columns", len(row), len(cols))
	}
	// 500k of 1M: a gauge half full, drawn to the fitted width.
	cell := row[len(row)-1]
	if !strings.Contains(cell, "▕") || !strings.Contains(cell, "█") {
		t.Errorf("last cell is not a gauge: %q", cell)
	}
	if got, want := lipgloss.Width(cell), cols[len(cols)-1].Width; got != want {
		t.Errorf("gauge cell is %d columns in a %d-wide column: %q", got, want, cell)
	}
}

// A session abctl has no events for shows the dash, not an empty track. On a fresh attach that is
// every session idle since before the connection, so it is the common case rather than an edge.
func TestSessionsTable_UnknownContextIsADash(t *testing.T) {
	m := &model{width: 200}
	m.sessionsTbl = newSessionsTable()
	m.sessions = []session.SessionSummary{{ID: "idle", UpdatedAt: time.Now(), EventCount: 9}}
	m.rebuildSessionsTable()

	cell := m.sessionsTbl.Rows()[0]
	if got := strings.TrimSpace(cell[len(cell)-1]); got != emptyCell {
		t.Errorf("unknown context rendered %q, want %q", got, emptyCell)
	}
}
