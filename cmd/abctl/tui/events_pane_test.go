package tui

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/cortex/core/pipeline"
)

// TestPageActivePane_EventsTable verifies PgDn/PgUp page the events table by a
// near-full screen (one row of overlap), clamped to the row range — the lever
// for sessions that now hold up to session.max_events (500) rows.
func TestPageActivePane_EventsTable(t *testing.T) {
	events := make([]pipeline.SessionEvent, 40)
	for i := range events {
		events[i] = pipeline.SessionEvent{
			Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
			Host: "h", Inference: &pipeline.InferenceExtension{Model: "m"},
		}
	}
	m := &model{
		pane: paneEvents, selectedSess: "s", bodyHeight: 12,
		events: map[string][]pipeline.SessionEvent{"s": events},
	}
	m.eventsTbl = newEventsTable()
	m.rebuildEventsTable()
	m.eventsTbl.SetCursor(0)

	h := m.eventsTbl.Height()
	if h < 2 {
		t.Fatalf("table height too small to page: %d", h)
	}
	m.pageActivePane(tea.KeyMsg{Type: tea.KeyPgDown})
	if got := m.eventsTbl.Cursor(); got != h-1 {
		t.Errorf("PgDn from top: cursor=%d, want %d", got, h-1)
	}
	m.pageActivePane(tea.KeyMsg{Type: tea.KeyPgUp})
	if got := m.eventsTbl.Cursor(); got != 0 {
		t.Errorf("PgUp back to top: cursor=%d, want 0", got)
	}

	// b/f (the keys shown in the footer, work on any keyboard) page the same way.
	m.pageActivePane(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")})
	if got := m.eventsTbl.Cursor(); got != h-1 {
		t.Errorf("f (page down) from top: cursor=%d, want %d", got, h-1)
	}
	m.pageActivePane(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b")})
	if got := m.eventsTbl.Cursor(); got != 0 {
		t.Errorf("b (page up) back to top: cursor=%d, want 0", got)
	}
}

// TestShortPhase covers the rendered string for every SessionPhase.
// SessionDenied renders as "req" (not "deny") because the deny
// outcome is already on the row in the ACTION + STATUS columns —
// duplicating it in PHASE was redundant. PHASE communicates
// lifecycle position; ACTION communicates outcome.
func TestShortPhase(t *testing.T) {
	cases := []struct {
		phase pipeline.SessionPhase
		want  string
	}{
		{pipeline.SessionRequest, "req"},
		{pipeline.SessionResponse, "resp"},
		{pipeline.SessionDenied, "req"},
	}
	for _, tc := range cases {
		if got := shortPhase(tc.phase); got != tc.want {
			t.Errorf("shortPhase(%v) = %q, want %q", tc.phase, got, tc.want)
		}
	}
}

// TestEventAction_Precedence locks the per-event ACTION/PLUGIN aggregation.
// One row per message: the winning action is the highest-ranked across all
// invocations (deny > modify > observe > allow > skip), the PLUGIN cell names
// the single responsible plugin, and a shadow deny gets the "*" suffix.
func TestEventAction_Precedence(t *testing.T) {
	ev := func(invs ...pipeline.Invocation) *pipeline.SessionEvent {
		return &pipeline.SessionEvent{Invocations: &pipeline.Invocations{Inbound: invs}}
	}
	cases := []struct {
		name       string
		event      *pipeline.SessionEvent
		wantAction string
		wantPlugin string
	}{
		{
			name:       "no invocations → passthrough markers",
			event:      &pipeline.SessionEvent{},
			wantAction: "—",
			wantPlugin: "—",
		},
		{
			name: "deny dominates observes",
			event: ev(
				pipeline.Invocation{Plugin: "a2a-parser", Action: pipeline.ActionObserve},
				pipeline.Invocation{Plugin: "jwt-validation", Action: pipeline.ActionDeny},
				pipeline.Invocation{Plugin: "mcp-parser", Action: pipeline.ActionObserve},
			),
			wantAction: "deny",
			wantPlugin: "jwt-validation",
		},
		{
			name: "modify beats allow",
			event: ev(
				pipeline.Invocation{Plugin: "jwt-validation", Action: pipeline.ActionAllow},
				pipeline.Invocation{Plugin: "token-exchange", Action: pipeline.ActionModify},
			),
			wantAction: "modify",
			wantPlugin: "token-exchange",
		},
		{
			// The real #23 case: a gate allows AND a parser observes. The
			// parser (which supplied METHOD) is the informative headline, so
			// observe must outrank allow.
			name: "observe beats allow (parser over gate)",
			event: ev(
				pipeline.Invocation{Plugin: "jwt-validation", Action: pipeline.ActionAllow},
				pipeline.Invocation{Plugin: "a2a-parser", Action: pipeline.ActionObserve},
			),
			wantAction: "observe",
			wantPlugin: "a2a-parser",
		},
		{
			// skip-only (plugins ran but none applied) reads as a passthrough:
			// no plugin is credited, because naming a skipper (e.g.
			// token-exchange on an unrelated host) implies it processed the
			// message.
			name: "all skip → passthrough markers (no plugin credited)",
			event: ev(
				pipeline.Invocation{Plugin: "jwt-validation", Action: pipeline.ActionSkip},
				pipeline.Invocation{Plugin: "token-exchange", Action: pipeline.ActionSkip},
			),
			wantAction: "—",
			wantPlugin: "—",
		},
		{
			// A single skip (the row-30 www.example.org case) is also a
			// passthrough — token-exchange skipped, nothing was done.
			name: "single skip → passthrough markers",
			event: ev(
				pipeline.Invocation{Plugin: "token-exchange", Action: pipeline.ActionSkip},
			),
			wantAction: "—",
			wantPlugin: "—",
		},
		{
			name: "shadow deny gets asterisk",
			event: ev(
				pipeline.Invocation{Plugin: "pii-scrubber", Action: pipeline.ActionDeny, Shadow: true},
			),
			wantAction: "deny*",
			wantPlugin: "pii-scrubber",
		},
		{
			name: "single observe (parser-only)",
			event: ev(
				pipeline.Invocation{Plugin: "inference-parser", Action: pipeline.ActionObserve},
			),
			wantAction: "observe",
			wantPlugin: "inference-parser",
		},
		{
			// Shadow deny + a real allow: the deny ran under on_error: observe
			// and did NOT block, so the headline must reflect the enforced
			// allow (not "deny*"), with "*" flagging that a shadow policy
			// would have blocked. The would-have-blocked decision is surfaced
			// without claiming a block that never happened.
			name: "shadow deny with enforced allow → allow*",
			event: ev(
				pipeline.Invocation{Plugin: "pii-scrubber", Action: pipeline.ActionDeny, Shadow: true},
				pipeline.Invocation{Plugin: "jwt-validation", Action: pipeline.ActionAllow},
			),
			wantAction: "allow*",
			wantPlugin: "jwt-validation",
		},
		{
			// Shadow deny + a parser observe: enforced winner is observe; the
			// shadow is flagged with "*".
			name: "shadow deny with enforced observe → observe*",
			event: ev(
				pipeline.Invocation{Plugin: "pii-scrubber", Action: pipeline.ActionDeny, Shadow: true},
				pipeline.Invocation{Plugin: "a2a-parser", Action: pipeline.ActionObserve},
			),
			wantAction: "observe*",
			wantPlugin: "a2a-parser",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotAction, gotPlugin := eventAction(allInvocations(tc.event))
			if gotAction != tc.wantAction {
				t.Errorf("action = %q, want %q", gotAction, tc.wantAction)
			}
			if gotPlugin != tc.wantPlugin {
				t.Errorf("plugin = %q, want %q", gotPlugin, tc.wantPlugin)
			}
		})
	}
}

// TestEventAction_OutboundInvocations confirms outbound-direction invocations
// participate in aggregation (forward-proxy events record on the Outbound
// slot).
func TestEventAction_OutboundInvocations(t *testing.T) {
	ev := &pipeline.SessionEvent{
		Direction: pipeline.Outbound,
		Invocations: &pipeline.Invocations{
			Outbound: []pipeline.Invocation{
				{Plugin: "token-exchange", Action: pipeline.ActionModify},
			},
		},
	}
	action, plugin := eventAction(allInvocations(ev))
	if action != "modify" || plugin != "token-exchange" {
		t.Errorf("eventAction = (%q, %q), want (modify, token-exchange)", action, plugin)
	}
}

// TestEventInactive covers the predicate the `s` toggle uses to hide
// passthrough / skip-only messages. A message with no invocations (a
// passthrough the operator can now see) and a message where every plugin
// skipped are both inactive; any non-skip action makes it active.
func TestEventInactive(t *testing.T) {
	ev := func(invs ...pipeline.Invocation) *pipeline.SessionEvent {
		return &pipeline.SessionEvent{Invocations: &pipeline.Invocations{Inbound: invs}}
	}
	cases := []struct {
		name  string
		event *pipeline.SessionEvent
		want  bool
	}{
		{"no invocations (passthrough)", &pipeline.SessionEvent{}, true},
		{"all skip", ev(
			pipeline.Invocation{Plugin: "jwt-validation", Action: pipeline.ActionSkip},
			pipeline.Invocation{Plugin: "token-exchange", Action: pipeline.ActionSkip},
		), true},
		{"one observe among skips", ev(
			pipeline.Invocation{Plugin: "jwt-validation", Action: pipeline.ActionSkip},
			pipeline.Invocation{Plugin: "a2a-parser", Action: pipeline.ActionObserve},
		), false},
		{"allow", ev(pipeline.Invocation{Plugin: "jwt-validation", Action: pipeline.ActionAllow}), false},
		{"deny", ev(pipeline.Invocation{Plugin: "jwt-validation", Action: pipeline.ActionDeny}), false},
		{"shadow deny counts as activity", ev(
			pipeline.Invocation{Plugin: "pii-scrubber", Action: pipeline.ActionDeny, Shadow: true},
		), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := eventInactive(allInvocations(tc.event)); got != tc.want {
				t.Errorf("eventInactive = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestBuildEventRows_MultiPluginIsOneRow locks the core fix: a single message
// touched by three plugins is exactly ONE display row (not three), and its
// aggregate headlines the plugin that did the meaningful work (a2a-parser
// observing) rather than the gate that allowed or the parser that skipped.
func TestBuildEventRows_MultiPluginIsOneRow(t *testing.T) {
	events := []pipeline.SessionEvent{
		{
			Direction: pipeline.Inbound,
			Phase:     pipeline.SessionRequest,
			Host:      "weather-agent",
			Invocations: &pipeline.Invocations{
				Inbound: []pipeline.Invocation{
					{Plugin: "jwt-validation", Action: pipeline.ActionAllow},
					{Plugin: "a2a-parser", Action: pipeline.ActionObserve},
					{Plugin: "mcp-parser", Action: pipeline.ActionSkip},
				},
			},
		},
	}
	rows := buildEventRows(events)
	if len(rows) != 1 {
		t.Fatalf("3-plugin message produced %d rows, want 1", len(rows))
	}
	if rows[0].tunnel != nil {
		t.Errorf("row should have no folded tunnel")
	}
	action, plugin := eventAction(rows[0].invocations())
	if action != "observe" {
		t.Errorf("aggregate action = %q, want observe", action)
	}
	if plugin != "a2a-parser" {
		t.Errorf("aggregate plugin = %q, want a2a-parser", plugin)
	}
}

// TestBuildEventRows_EmptyInvocationsResponse confirms a status-only response
// that no plugin acted on (now recorded server-side) renders as one row
// carrying its status.
func TestBuildEventRows_EmptyInvocationsResponse(t *testing.T) {
	events := []pipeline.SessionEvent{
		{Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: "example.com"},
		{Direction: pipeline.Outbound, Phase: pipeline.SessionResponse, Host: "example.com", StatusCode: 404},
	}
	rows := buildEventRows(events)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	resp := rows[1].event
	if got := statusCell(*resp); got != "404" {
		t.Errorf("response statusCell = %q, want 404", got)
	}
	action, plugin := eventAction(allInvocations(resp))
	if action != "—" || plugin != "—" {
		t.Errorf("no-plugin response aggregate = (%q, %q), want (—, —)", action, plugin)
	}
}

// TestBuildEventRows_CollapsesBridgedConnect locks the CONNECT-fold (Part C):
// a tunnel-open (host:port, opaque) immediately followed by the decrypted
// inner request (same host, real method) is ONE row keyed on the inner
// request, with the tunnel attached. The inner response stays a separate row.
func TestBuildEventRows_CollapsesBridgedConnect(t *testing.T) {
	events := []pipeline.SessionEvent{
		// CONNECT tunnel-open — opaque, host:port, gate invocation only.
		{
			Direction: pipeline.Outbound,
			Phase:     pipeline.SessionRequest,
			Host:      "api.anthropic.com:443",
			Tunnel:    true,
			Invocations: &pipeline.Invocations{
				Outbound: []pipeline.Invocation{{Plugin: "jwt-validation", Action: pipeline.ActionSkip}},
			},
		},
		// Decrypted inner request — real model, host without port.
		{
			Direction: pipeline.Outbound,
			Phase:     pipeline.SessionRequest,
			Host:      "api.anthropic.com",
			Inference: &pipeline.InferenceExtension{Model: "claude-3-5-sonnet"},
			Invocations: &pipeline.Invocations{
				Outbound: []pipeline.Invocation{{Plugin: "inference-parser", Action: pipeline.ActionObserve}},
			},
		},
		// Inner response.
		{
			Direction:  pipeline.Outbound,
			Phase:      pipeline.SessionResponse,
			Host:       "api.anthropic.com",
			Inference:  &pipeline.InferenceExtension{Model: "claude-3-5-sonnet", TotalTokens: 1200},
			StatusCode: 200,
		},
	}
	rows := buildEventRows(events)
	if len(rows) != 2 {
		t.Fatalf("bridged call produced %d rows, want 2 (collapsed req + resp)", len(rows))
	}
	// Row 0: the inner request, with the CONNECT folded as tunnel.
	if rows[0].event.Inference == nil || rows[0].event.Inference.Model != "claude-3-5-sonnet" {
		t.Errorf("row 0 should be the decrypted inner request")
	}
	if rows[0].tunnel == nil {
		t.Fatalf("row 0 should carry the folded CONNECT tunnel")
	}
	if rows[0].tunnel.Host != "api.anthropic.com:443" {
		t.Errorf("folded tunnel host = %q, want api.anthropic.com:443", rows[0].tunnel.Host)
	}
	// Row 0 host should be the clean inner host (no :443).
	if rows[0].event.Host != "api.anthropic.com" {
		t.Errorf("collapsed row host = %q, want api.anthropic.com", rows[0].event.Host)
	}
	// Row 1: the response, no tunnel.
	if rows[1].event.Phase != pipeline.SessionResponse || rows[1].tunnel != nil {
		t.Errorf("row 1 should be the standalone inner response")
	}
}

// TestBuildEventRows_PassthroughTunnelStandsAlone covers every shape in which a
// non-bridged CONNECT tunnel-open must remain its own row rather than fold:
// a lone trailing CONNECT, two back-to-back CONNECTs to the SAME host
// (connection pooling — must NOT fold into each other), and a CONNECT followed
// by a different-host request (host-mismatch guard).
func TestBuildEventRows_PassthroughTunnelStandsAlone(t *testing.T) {
	connect := func(host string) pipeline.SessionEvent {
		return pipeline.SessionEvent{Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: host, Tunnel: true}
	}
	cases := []struct {
		name   string
		events []pipeline.SessionEvent
	}{
		{
			name:   "lone trailing CONNECT",
			events: []pipeline.SessionEvent{connect("passthrough.example:443")},
		},
		{
			name: "two CONNECTs to the same host (pooling) must not fold",
			events: []pipeline.SessionEvent{
				connect("api.example.com:443"),
				connect("api.example.com:443"),
			},
		},
		{
			name: "CONNECT then different-host request",
			events: []pipeline.SessionEvent{
				connect("passthrough.example:443"),
				{Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: "other.example",
					Inference: &pipeline.InferenceExtension{Model: "x"}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows := buildEventRows(tc.events)
			if len(rows) != len(tc.events) {
				t.Fatalf("got %d rows, want %d (nothing should fold)", len(rows), len(tc.events))
			}
			for i, r := range rows {
				if r.tunnel != nil {
					t.Errorf("row %d unexpectedly folded a tunnel", i)
				}
			}
		})
	}
}

// TestBuildEventRows_TunnelInvocationsFoldIntoRow locks #3: a bridged row's
// ACTION/inactive view must include the folded CONNECT tunnel's gate
// invocations, so an egress gate that ALLOWED the CONNECT is reflected even
// when the decrypted inner request itself saw no plugin activity.
func TestBuildEventRows_TunnelInvocationsFoldIntoRow(t *testing.T) {
	events := []pipeline.SessionEvent{
		// CONNECT tunnel-open — an egress gate explicitly ALLOWED it.
		{
			Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: "api.example.com:443", Tunnel: true,
			Invocations: &pipeline.Invocations{Outbound: []pipeline.Invocation{
				{Plugin: "egress-policy", Action: pipeline.ActionAllow},
			}},
		},
		// Decrypted inner request — no plugin matched (opaque passthrough body).
		{Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: "api.example.com"},
	}
	rows := buildEventRows(events)
	if len(rows) != 1 || rows[0].tunnel == nil {
		t.Fatalf("expected 1 folded row, got %d (tunnel=%v)", len(rows), rows[0].tunnel != nil)
	}
	// The inner event alone has no invocations; the row must surface the
	// tunnel's allow.
	if eventInactive(rows[0].invocations()) {
		t.Error("row with a tunnel-level allow should not be inactive")
	}
	action, plugin := eventAction(rows[0].invocations())
	if action != "allow" || plugin != "egress-policy" {
		t.Errorf("folded ACTION = (%q, %q), want (allow, egress-policy)", action, plugin)
	}
}

// TestRebuildEventsTable_HideInactive is the integration check for #5: the
// hideInactive toggle (predicate → row build → footer count) suppresses
// passthrough/skip-only messages while keeping partially-active ones.
func TestRebuildEventsTable_HideInactive(t *testing.T) {
	events := []pipeline.SessionEvent{
		// active — a2a observe
		{Direction: pipeline.Inbound, Phase: pipeline.SessionRequest, Host: "agent",
			Invocations: &pipeline.Invocations{Inbound: []pipeline.Invocation{
				{Plugin: "jwt-validation", Action: pipeline.ActionSkip}, // partially-active: skip + observe
				{Plugin: "a2a-parser", Action: pipeline.ActionObserve},
			}}},
		// inactive — no invocations (passthrough response)
		{Direction: pipeline.Outbound, Phase: pipeline.SessionResponse, Host: "x", StatusCode: 200},
		// inactive — skip-only
		{Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: "y",
			Invocations: &pipeline.Invocations{Outbound: []pipeline.Invocation{
				{Plugin: "token-exchange", Action: pipeline.ActionSkip},
			}}},
	}
	m := &model{selectedSess: "s", events: map[string][]pipeline.SessionEvent{"s": events}}
	m.eventsTbl = newEventsTable()

	m.hideInactive = false
	m.rebuildEventsTable()
	if len(m.visibleRows) != 3 || m.hiddenInactive != 0 {
		t.Fatalf("show-all: rows=%d hidden=%d, want 3/0", len(m.visibleRows), m.hiddenInactive)
	}

	m.hideInactive = true
	m.rebuildEventsTable()
	if len(m.visibleRows) != 1 || m.hiddenInactive != 2 {
		t.Fatalf("hide: rows=%d hidden=%d, want 1/2", len(m.visibleRows), m.hiddenInactive)
	}
	// The surviving row is the partially-active a2a message.
	if action, _ := eventAction(m.visibleRows[0].invocations()); action != "observe" {
		t.Errorf("surviving row action = %q, want observe", action)
	}
}

// TestComputeEventPairIDs pairs each response row with its preceding request
// row by direction + host + method, sharing one # across the exchange and
// minting fresh integers for unpaired rows.
func TestComputeEventPairIDs(t *testing.T) {
	events := []pipeline.SessionEvent{
		{Direction: pipeline.Inbound, Phase: pipeline.SessionRequest, Host: "weather-agent"},
		{Direction: pipeline.Inbound, Phase: pipeline.SessionResponse, Host: "weather-agent", StatusCode: 200},
		{Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: "tool"},
		{Direction: pipeline.Outbound, Phase: pipeline.SessionResponse, Host: "tool", StatusCode: 200},
	}
	rows := buildEventRows(events)
	ids, _ := computeEventPairs(rows)

	if ids[&events[0]] != ids[&events[1]] {
		t.Errorf("inbound req/resp should share id, got %d vs %d", ids[&events[0]], ids[&events[1]])
	}
	if ids[&events[2]] != ids[&events[3]] {
		t.Errorf("outbound req/resp should share id, got %d vs %d", ids[&events[2]], ids[&events[3]])
	}
	if ids[&events[0]] == ids[&events[2]] {
		t.Errorf("distinct exchanges should have distinct ids, both got %d", ids[&events[0]])
	}
}

// TestComputeEventPairIDs_MethodDiscrimination locks method-aware pairing: a
// fire-and-forget request (MCP notifications/initialized, no response) must
// not steal the response that belongs to a later tools/list request.
func TestComputeEventPairIDs_MethodDiscrimination(t *testing.T) {
	mk := func(phase pipeline.SessionPhase, method string) pipeline.SessionEvent {
		return pipeline.SessionEvent{
			Direction: pipeline.Outbound,
			Phase:     phase,
			Host:      "tool",
			MCP:       &pipeline.MCPExtension{Method: method},
		}
	}
	events := []pipeline.SessionEvent{
		mk(pipeline.SessionRequest, "notifications/initialized"), // no response (fire and forget)
		mk(pipeline.SessionRequest, "tools/list"),
		mk(pipeline.SessionResponse, "tools/list"),
	}
	rows := buildEventRows(events)
	ids, _ := computeEventPairs(rows)

	if ids[&events[1]] != ids[&events[2]] {
		t.Errorf("tools/list req and resp must share id, got %d vs %d", ids[&events[1]], ids[&events[2]])
	}
	if ids[&events[0]] == ids[&events[1]] {
		t.Errorf("notifications/initialized must not share id with tools/list, both got %d", ids[&events[0]])
	}
}

// TestMatchEventRow_DenyShortcut verifies that typing "deny" surfaces both the
// SessionDenied phase AND any invocation whose Action is ActionDeny.
func TestMatchEventRow_DenyShortcut(t *testing.T) {
	denied := eventRow{event: &pipeline.SessionEvent{Phase: pipeline.SessionDenied}}
	if !matchEventRow(denied, "deny") {
		t.Error("SessionDenied event should match the `deny` shortcut")
	}

	inboundDeny := eventRow{event: &pipeline.SessionEvent{
		Phase:       pipeline.SessionRequest,
		Invocations: &pipeline.Invocations{Inbound: []pipeline.Invocation{{Action: pipeline.ActionDeny}}},
	}}
	if !matchEventRow(inboundDeny, "deny") {
		t.Error("event with a deny invocation should match the `deny` shortcut")
	}

	clean := eventRow{event: &pipeline.SessionEvent{
		Phase:       pipeline.SessionRequest,
		Invocations: &pipeline.Invocations{Inbound: []pipeline.Invocation{{Action: pipeline.ActionAllow}}},
	}}
	if matchEventRow(clean, "deny") {
		t.Error("allow-only event should NOT match the `deny` shortcut")
	}
}

// TestMatchEventRow_PluginSubstring verifies substring matching across an
// event's invocation fields (plugin name, reason, path).
func TestMatchEventRow_PluginSubstring(t *testing.T) {
	row := eventRow{event: &pipeline.SessionEvent{
		Phase: pipeline.SessionRequest,
		Invocations: &pipeline.Invocations{Inbound: []pipeline.Invocation{
			{Plugin: "jwt-validation", Action: pipeline.ActionSkip, Reason: "path_bypass", Path: "/healthz"},
		}},
	}}
	if !matchEventRow(row, "jwt-validation") {
		t.Error("filter jwt-validation should match")
	}
	if !matchEventRow(row, "path_bypass") {
		t.Error("filter by reason should match")
	}
	if !matchEventRow(row, "/healthz") {
		t.Error("filter by path should match")
	}
	if matchEventRow(row, "token-exchange") {
		t.Error("filter token-exchange should NOT match a jwt-validation-only event")
	}
}

// TestMatchEventRow_TunnelFields confirms a folded tunnel's fields are
// searchable on the collapsed row — filtering by the bridged origin's
// host:port still surfaces the row even though the row's own host is
// port-stripped.
func TestMatchEventRow_TunnelFields(t *testing.T) {
	row := eventRow{
		event:  &pipeline.SessionEvent{Host: "api.anthropic.com"},
		tunnel: &pipeline.SessionEvent{Host: "api.anthropic.com:443"},
	}
	if !matchEventRow(row, "anthropic.com:443") {
		t.Error("filter on the tunnel host:port should match the collapsed row")
	}
}

// TestMatchEventRow_PluginPrefix tests the `plugin:<name>` escape-hatch filter
// against the event's Plugins map.
func TestMatchEventRow_PluginPrefix(t *testing.T) {
	row := eventRow{event: &pipeline.SessionEvent{
		Plugins: map[string]json.RawMessage{
			"rate-limiter": json.RawMessage(`{"allowed":true}`),
		},
	}}
	if !matchEventRow(row, "plugin:rate-limiter") {
		t.Error("expected match on plugin:rate-limiter")
	}
	if matchEventRow(row, "plugin:nonexistent") {
		t.Error("expected no match for a plugin not in the map")
	}
}

// TestStatusCell exercises the realistic auth-only request/response shape:
// a response carrying a 200 renders that status; a request renders blank.
func TestStatusCell(t *testing.T) {
	now := time.Date(2026, 5, 8, 14, 22, 5, 0, time.UTC)
	req := pipeline.SessionEvent{At: now, Phase: pipeline.SessionRequest, Host: "weather-agent"}
	resp := pipeline.SessionEvent{At: now.Add(12 * time.Millisecond), Phase: pipeline.SessionResponse,
		Host: "weather-agent", StatusCode: 200, Duration: 12 * time.Millisecond}
	if got := statusCell(req); got != "" {
		t.Errorf("request statusCell = %q, want empty", got)
	}
	if got := statusCell(resp); got != "200" {
		t.Errorf("response statusCell = %q, want 200", got)
	}
	if got := durationCell(resp); got != "12ms" {
		t.Errorf("durationCell = %q, want 12ms", got)
	}
}

// TestHostOnly covers port stripping (used by collapse + pairing), including
// the no-port and IPv6 cases.
func TestHostOnly(t *testing.T) {
	cases := []struct{ in, want string }{
		{"example.com:443", "example.com"},
		{"example.com", "example.com"},
		{"[::1]:8443", "::1"},
	}
	for _, tc := range cases {
		if got := hostOnly(tc.in); got != tc.want {
			t.Errorf("hostOnly(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestComputeEventPairs_NestedExchanges is the end-to-end #23 shape: an
// inbound a2a message/stream request, two outbound inference exchanges during
// processing, then the a2a response. The a2a request/response must pair and
// exchange, with the inference exchanges falling inside its window.
func TestComputeEventPairs_NestedExchanges(t *testing.T) {
	a2aReq := pipeline.SessionEvent{Direction: pipeline.Inbound, Phase: pipeline.SessionRequest,
		Host: "claude-agent", A2A: &pipeline.A2AExtension{Method: "message/stream"}}
	infReq1 := pipeline.SessionEvent{Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
		Host: "litellm", Inference: &pipeline.InferenceExtension{Model: "claude"}}
	infResp1 := pipeline.SessionEvent{Direction: pipeline.Outbound, Phase: pipeline.SessionResponse,
		Host: "litellm", Inference: &pipeline.InferenceExtension{Model: "claude"}, StatusCode: 200}
	infReq2 := pipeline.SessionEvent{Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
		Host: "litellm", Inference: &pipeline.InferenceExtension{Model: "claude"}}
	infResp2 := pipeline.SessionEvent{Direction: pipeline.Outbound, Phase: pipeline.SessionResponse,
		Host: "litellm", Inference: &pipeline.InferenceExtension{Model: "claude"}, StatusCode: 200}
	a2aResp := pipeline.SessionEvent{Direction: pipeline.Inbound, Phase: pipeline.SessionResponse,
		Host: "claude-agent", A2A: &pipeline.A2AExtension{Method: "message/stream"}, StatusCode: 200}
	events := []pipeline.SessionEvent{a2aReq, infReq1, infResp1, infReq2, infResp2, a2aResp}

	rows := buildEventRows(events)
	ids, partner := computeEventPairs(rows)

	// a2a request (row 0) pairs with a2a response (row 5), spanning everything.
	if partner[0] != 5 || partner[5] != 0 {
		t.Errorf("a2a req/resp should pair 0↔5, got partner=%v", partner)
	}
	if ids[&events[0]] != ids[&events[5]] {
		t.Errorf("a2a req/resp should share #, got %d vs %d", ids[&events[0]], ids[&events[5]])
	}

	// The inner inference exchanges pair with each other, not across.
	if partner[1] != 2 || partner[3] != 4 {
		t.Errorf("inner exchanges should pair 1↔2 and 3↔4, got partner=%v", partner)
	}
}

// TestPlural matches the helper used by the events footer hint:
// "1 skip hidden" vs "2 skips hidden".
func TestPlural(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "s"},
		{1, ""},
		{2, "s"},
		{17, "s"},
	}
	for _, tc := range cases {
		if got := plural(tc.n); got != tc.want {
			t.Errorf("plural(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

// TestComputeEventPairs_RequestIDBeatsAdjacency reproduces a real
// misdiagnosis. Claude Code fires its session-title request concurrently with
// the main one; both are POSTs to the same host. Interleaved as
// req(main) req(title) resp(title,400) resp(main,200), the closest-preceding
// heuristic pairs req(title) with resp(title) — but pairs req(main) with
// resp(main) only by luck of ordering, and with a different interleaving it
// draws the title request's 400 under the main request's row.
//
// That is exactly what happened: a 400 belonging to a request tool-prune never
// touched was rendered beneath the row where tool-prune reported a body
// rewrite, which read as the plugin having broken the request. RequestID makes
// the pairing exact.
func TestComputeEventPairs_RequestIDBeatsAdjacency(t *testing.T) {
	ev := func(phase pipeline.SessionPhase, id string, code int) *pipeline.SessionEvent {
		return &pipeline.SessionEvent{
			Direction:  pipeline.Outbound,
			Phase:      phase,
			Host:       "litellm.example",
			RequestID:  id,
			StatusCode: code,
		}
	}
	// The real interleaving observed in the session store: the title request
	// is issued first, the main request second, and the title's 400 arrives
	// before the main response. The heuristic then walks back from the 400 to
	// the nearest unpaired request — the MAIN one — and brackets them together.
	title := ev(pipeline.SessionRequest, "bbb", 0)
	mainReq := ev(pipeline.SessionRequest, "aaa", 0)
	titleResp := ev(pipeline.SessionResponse, "bbb", 400)
	mainResp := ev(pipeline.SessionResponse, "aaa", 200)
	rows := []eventRow{{event: title}, {event: mainReq}, {event: titleResp}, {event: mainResp}}

	_, partner := computeEventPairs(rows)

	if partner[0] != 2 {
		t.Errorf("title request (row 0) paired with row %d, want 2 (its own 400)", partner[0])
	}
	if partner[1] != 3 {
		t.Errorf("main request (row 1) paired with row %d, want 3 (its own 200)", partner[1])
	}
	// The specific failure this fixes: the main request owning the title's 400.
	if partner[1] == 2 {
		t.Error("main request paired with the title request's 400 — the misdiagnosis this fixes")
	}
}

// TestComputeEventPairs_FallsBackWithoutRequestID keeps the heuristic working
// for events from a proxy that does not stamp an id, so an older data plane
// still renders brackets.
func TestComputeEventPairs_FallsBackWithoutRequestID(t *testing.T) {
	req := &pipeline.SessionEvent{Direction: pipeline.Outbound, Phase: pipeline.SessionRequest, Host: "h"}
	resp := &pipeline.SessionEvent{Direction: pipeline.Outbound, Phase: pipeline.SessionResponse, Host: "h", StatusCode: 200}
	rows := []eventRow{{event: req}, {event: resp}}
	_, partner := computeEventPairs(rows)
	if partner[0] != 1 || partner[1] != 0 {
		t.Errorf("heuristic pairing broke for id-less events: partner=%v", partner)
	}
}

// TestComputeEventPairs_FieldTrace replays a real interleaving captured from a
// Claude Code session, where the adjacency heuristic mispaired 6 of 15
// responses — a 3-way rotation (rows 10/11/12) and a straight swap (20/21).
//
// The mispairing was not cosmetic. It rendered a 400 beneath every row where
// tool-prune reported rewriting a body, when each of those 400s belonged to a
// different concurrent request and every request tool-prune touched returned
// 200. Ownership here is not guesswork: each response's duration is measured
// from its own request's start, so subtracting it identifies the true owner
// independently of the id being tested.
func TestComputeEventPairs_FieldTrace(t *testing.T) {
	type spec struct {
		id    string // true owning request id
		phase pipeline.SessionPhase
		host  string
		code  int
	}
	// Order is wall-clock order as observed; ids are the true owners.
	trace := []spec{
		{"r07", pipeline.SessionRequest, "mcp.ete", 0},
		{"r08", pipeline.SessionRequest, "mcp.ete", 0},
		{"r08", pipeline.SessionResponse, "mcp.ete", 200},
		{"r09", pipeline.SessionRequest, "litellm", 0},
		{"r09", pipeline.SessionResponse, "litellm", 200},
		{"r10", pipeline.SessionRequest, "litellm", 0},
		{"r11", pipeline.SessionRequest, "litellm", 0}, // tool-prune modified this one
		{"r10", pipeline.SessionResponse, "litellm", 400},
		{"r12", pipeline.SessionRequest, "litellm", 0},
		{"r11", pipeline.SessionResponse, "litellm", 200}, // the modify's real outcome
		{"r12", pipeline.SessionResponse, "litellm", 400},
		{"r13", pipeline.SessionRequest, "litellm", 0},
		{"r14", pipeline.SessionRequest, "litellm", 0}, // tool-prune modified this one
		{"r07", pipeline.SessionResponse, "mcp.ete", 200},
		{"r13", pipeline.SessionResponse, "litellm", 400},
		{"r15", pipeline.SessionRequest, "litellm", 0},
		{"r16", pipeline.SessionRequest, "mcp.ete", 0},
		{"r16", pipeline.SessionResponse, "mcp.ete", 400},
		{"r15", pipeline.SessionResponse, "litellm", 400},
		{"r14", pipeline.SessionResponse, "litellm", 200}, // the modify's real outcome
	}

	rows := make([]eventRow, 0, len(trace))
	for _, s := range trace {
		rows = append(rows, eventRow{event: &pipeline.SessionEvent{
			Direction: pipeline.Outbound, Phase: s.phase,
			Host: s.host, RequestID: s.id, StatusCode: s.code,
		}})
	}

	_, partner := computeEventPairs(rows)

	for i, s := range trace {
		j, ok := partner[i]
		if !ok {
			if s.id == "r07" || s.phase == pipeline.SessionRequest {
				// every request in this trace does get a response
				t.Errorf("row %d (%s %s) unpaired", i, s.id, s.phase)
			}
			continue
		}
		if got := rows[j].event.RequestID; got != s.id {
			t.Errorf("row %d (%s) paired with %s — pairing crossed requests", i, s.id, got)
		}
	}

	// The specific regression: no tool-prune-modified request may own a 400.
	for _, modified := range []string{"r11", "r14"} {
		for i, s := range trace {
			if s.phase != pipeline.SessionRequest || s.id != modified {
				continue
			}
			j := partner[i]
			if code := rows[j].event.StatusCode; code != 200 {
				t.Errorf("%s (tool-prune modified) paired with a %d; its real response was 200", modified, code)
			}
		}
	}
}

// An unbridged CONNECT must name itself in the ACTION column. It carries TLS
// bytes, so no plugin ran, no protocol was parsed and there is no status — left
// blank the row reads as a request that failed or that the pipeline ignored, which
// is how a routine egress tunnel (git, gh, an SDK that does not trust the bridge
// CA) came to look like a bug.
func TestRowAction_UnbridgedTunnelIsNamed(t *testing.T) {
	er := eventRow{event: &pipeline.SessionEvent{
		Direction: pipeline.Outbound,
		Phase:     pipeline.SessionRequest,
		Host:      "api.github.com:443",
		Tunnel:    true,
	}}
	action, plugin := rowAction(er, er.invocations())
	if action != tunnelAction {
		t.Errorf("ACTION = %q, want %q", action, tunnelAction)
	}
	if plugin != "—" {
		t.Errorf("PLUGIN = %q, want an em dash: no plugin ran", plugin)
	}
}

// A gate can deny a CONNECT on the tunnel-open itself. That deny is the whole
// point of the row and must keep the headline.
func TestRowAction_DeniedTunnelKeepsTheDeny(t *testing.T) {
	er := eventRow{event: &pipeline.SessionEvent{
		Direction: pipeline.Outbound,
		Phase:     pipeline.SessionRequest,
		Host:      "blocked.example:443",
		Tunnel:    true,
		Invocations: &pipeline.Invocations{Outbound: []pipeline.Invocation{
			{Plugin: "egress-gate", Action: pipeline.ActionDeny},
		}},
	}}
	action, plugin := rowAction(er, er.invocations())
	if action != string(pipeline.ActionDeny) {
		t.Errorf("ACTION = %q, want deny — the label must not mask a gate decision", action)
	}
	if plugin != "egress-gate" {
		t.Errorf("PLUGIN = %q, want egress-gate", plugin)
	}
}

// A bridged tunnel is folded into its decrypted inner request, so the row shows
// the inner request's action. The tunnel label must not override it.
func TestRowAction_BridgedTunnelShowsInnerAction(t *testing.T) {
	tunnel := &pipeline.SessionEvent{
		Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
		Host: "ete-litellm.example:443", Tunnel: true,
	}
	inner := &pipeline.SessionEvent{
		Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
		Host: "ete-litellm.example",
		Invocations: &pipeline.Invocations{Outbound: []pipeline.Invocation{
			{Plugin: "inference-parser", Action: pipeline.ActionObserve},
		}},
	}
	er := eventRow{event: inner, tunnel: tunnel}
	action, plugin := rowAction(er, er.invocations())
	if action != string(pipeline.ActionObserve) {
		t.Errorf("ACTION = %q, want observe from the decrypted inner request", action)
	}
	if plugin != "inference-parser" {
		t.Errorf("PLUGIN = %q, want inference-parser", plugin)
	}
}

// An ordinary request that no plugin touched keeps its em dash: only a tunnel
// earns the label, or every passthrough row would claim to be one.
func TestRowAction_PlainPassthroughIsUnchanged(t *testing.T) {
	er := eventRow{event: &pipeline.SessionEvent{
		Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
		Host: "example.com",
	}}
	if action, _ := rowAction(er, er.invocations()); action != "—" {
		t.Errorf("ACTION = %q for a non-tunnel passthrough, want an em dash", action)
	}
}

// The label must fit the column, or the table shifts.
func TestTunnelAction_FitsTheActionColumn(t *testing.T) {
	if got := len([]rune(tunnelAction)); got > actionColWidth {
		t.Errorf("%q is %d columns, ACTION is %d wide", tunnelAction, got, actionColWidth)
	}
}

// End to end through buildEventRows, using the event shapes a live server records:
// an unbridged CONNECT stands alone and is named; a bridged pair folds to one row
// showing the inner action.
func TestBuildEventRows_TunnelRowsAreLabelled(t *testing.T) {
	base := time.Now()
	events := []pipeline.SessionEvent{
		{At: base, Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
			RequestID: "40", Host: "api.github.com:443", Tunnel: true},
		{At: base.Add(time.Second), Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
			RequestID: "41", Host: "ete-litellm.example:443", Tunnel: true},
		{At: base.Add(time.Second), Direction: pipeline.Outbound, Phase: pipeline.SessionRequest,
			RequestID: "41", Host: "ete-litellm.example",
			Invocations: &pipeline.Invocations{Outbound: []pipeline.Invocation{
				{Plugin: "inference-parser", Action: pipeline.ActionObserve},
			}}},
	}
	rows := buildEventRows(events)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (the bridged pair folds)", len(rows))
	}
	if a, _ := rowAction(rows[0], rows[0].invocations()); a != tunnelAction {
		t.Errorf("unbridged tunnel row ACTION = %q, want %q", a, tunnelAction)
	}
	if a, _ := rowAction(rows[1], rows[1].invocations()); a != string(pipeline.ActionObserve) {
		t.Errorf("bridged row ACTION = %q, want observe", a)
	}
}

// TestTunnelReasonCell: the PLUGIN cell carries the reason on a tunnel row,
// because no plugin ran there and a second em dash beside the first is what made
// a routine passthrough and a blind-to-everything CA rejection look identical.
func TestTunnelReasonCell(t *testing.T) {
	for _, tc := range []struct {
		reason pipeline.TunnelReason
		want   string
	}{
		{"", "—"}, // bridged: folded into the inner request
		{pipeline.TunnelClientRejectedCA, string(pipeline.TunnelClientRejectedCA)},
		{pipeline.TunnelSkipCached, string(pipeline.TunnelSkipCached)},
		{"some-future-reason", "some-future-reason"}, // newer proxy, older abctl
	} {
		if got := tunnelReasonCell(tc.reason); got != tc.want {
			t.Errorf("tunnelReasonCell(%q) = %q, want %q", tc.reason, got, tc.want)
		}
	}
}

// TestRowActionSurfacesTunnelReason: the reason has to reach the row, not just
// the event. A tunnel row shows "tunnel" plus WHY; a row where a plugin acted
// keeps that plugin's headline, because a gate CAN deny a CONNECT and that deny
// must not be replaced by a tunnel label.
func TestRowActionSurfacesTunnelReason(t *testing.T) {
	ev := &pipeline.SessionEvent{Tunnel: true, TunnelReason: pipeline.TunnelClientRejectedCA}
	action, plugin := rowAction(eventRow{event: ev}, nil)
	if action != tunnelAction {
		t.Errorf("action = %q, want %q", action, tunnelAction)
	}
	if plugin != string(pipeline.TunnelClientRejectedCA) {
		t.Errorf("plugin cell = %q, want the reason %q", plugin, pipeline.TunnelClientRejectedCA)
	}

	// A denied CONNECT keeps its own headline.
	denied := &pipeline.SessionEvent{Tunnel: true, TunnelReason: pipeline.TunnelSkipCached}
	invs := []pipeline.Invocation{{Plugin: "ibac", Action: "deny"}}
	action, _ = rowAction(eventRow{event: denied}, invs)
	if action == tunnelAction {
		t.Error("a denied CONNECT was relabelled 'tunnel'; the deny must headline")
	}
}

// eventSeq builds n events with distinct keys — At and RequestID vary
// per index so keyOf discriminates one from another.
func eventSeq(n int, prefix string) []pipeline.SessionEvent {
	events := make([]pipeline.SessionEvent, n)
	for i := range events {
		events[i] = pipeline.SessionEvent{
			At:        time.Time{}.Add(time.Duration(i) * time.Millisecond),
			RequestID: fmt.Sprintf("%s%d-req", prefix, i),
			Direction: pipeline.Outbound,
			Phase:     pipeline.SessionRequest,
			Host:      fmt.Sprintf("%s%d", prefix, i),
			Inference: &pipeline.InferenceExtension{Model: "m"},
		}
	}
	return events
}

func newEventsPaneModel(events []pipeline.SessionEvent) *model {
	m := &model{
		pane: paneEvents, selectedSess: "s", bodyHeight: 12,
		events: map[string][]pipeline.SessionEvent{"s": events},
	}
	m.eventsTbl = newEventsTable()
	m.rebuildEventsTable()
	return m
}

// TestSelectedEventKey_SurvivesEviction locks in the identity restore:
// after FIFO eviction the cursor follows the pinned event by key.
func TestSelectedEventKey_SurvivesEviction(t *testing.T) {
	events := eventSeq(10, "e")
	m := newEventsPaneModel(events)
	m.eventsTbl.SetCursor(5)
	m.selectedEventKey = keyOf(m.selectedEvent())
	want := m.selectedEvent().Host

	m.events["s"] = append(events[3:], eventSeq(5, "n")...)
	m.rebuildEventsTable()

	if got := m.selectedEvent().Host; got != want {
		t.Errorf("selection lost: got %s, want %s", got, want)
	}
}

// TestSelectedEventKey_TailWinsOverPin — a cursor at the last row keeps
// tailing on append, so live sessions keep scrolling.
func TestSelectedEventKey_TailWinsOverPin(t *testing.T) {
	events := eventSeq(5, "e")
	m := newEventsPaneModel(events)
	m.eventsTbl.SetCursor(4)
	m.selectedEventKey = keyOf(m.selectedEvent())

	m.events["s"] = append(events, eventSeq(2, "n")...)
	m.rebuildEventsTable()

	if got, want := m.eventsTbl.Cursor(), len(m.eventsTbl.Rows())-1; got != want {
		t.Errorf("tail didn't follow: cursor=%d, want %d", got, want)
	}
}

// TestSelectedEventKey_EvictedPinClampsToOldestSurvivor — when the
// pinned event is gone, the cursor lands on the oldest surviving row
// (the nearest edge to where the pin was) and the stale key clears.
func TestSelectedEventKey_EvictedPinClampsToOldestSurvivor(t *testing.T) {
	events := eventSeq(10, "e")
	m := newEventsPaneModel(events)
	m.eventsTbl.SetCursor(2)
	m.selectedEventKey = keyOf(m.selectedEvent())

	m.events["s"] = events[5:] // pinned e2 evicted; e5 is now oldest
	m.rebuildEventsTable()

	if got := m.eventsTbl.Cursor(); got != 0 {
		t.Errorf("cursor=%d, want 0 (oldest survivor)", got)
	}
	if got := m.selectedEvent().Host; got != "e5" {
		t.Errorf("cursor's event = %s, want e5 (oldest survivor)", got)
	}
	if m.selectedEventKey != (eventKey{}) {
		t.Errorf("stale pin retained: %+v", m.selectedEventKey)
	}
}

// TestSelectedEventKey_UpdatedOnCursorMotion — every cursor-motion
// keypress refreshes the pin to the new row. Arrow keys and j/k route
// through handleKey's paneEvents fallback; page keys via pageActivePane.
func TestSelectedEventKey_UpdatedOnCursorMotion(t *testing.T) {
	cases := []struct {
		name string
		msg  tea.KeyMsg
	}{
		{"arrow-down", tea.KeyMsg{Type: tea.KeyDown}},
		{"j", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")}},
		{"page-down", tea.KeyMsg{Type: tea.KeyPgDown}},
		{"f", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")}},
		{"G", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("G")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newEventsPaneModel(eventSeq(10, "e"))
			// newEventsPaneModel tails to the last row; start at 0 so
			// KeyDown / PgDn have room to move.
			m.eventsTbl.SetCursor(0)
			before := m.eventsTbl.Cursor()

			m2, _ := m.Update(tc.msg)
			m = m2.(*model)

			if m.eventsTbl.Cursor() == before {
				t.Fatalf("cursor did not move for %q; test would pass vacuously", tc.name)
			}
			if got, want := m.selectedEventKey, keyOf(m.selectedEvent()); got != want {
				t.Errorf("pin=%+v, want cursor's event %+v", got, want)
			}
		})
	}
}

// TestKeyOf_SameInstantSameKey — a time.Now() At and its JSON
// round-tripped form (stripped monotonic + different Location)
// produce the same eventKey, so findByKey matches under struct ==.
func TestKeyOf_SameInstantSameKey(t *testing.T) {
	now := time.Now()
	roundTripped := now.UTC().Round(0)
	a := keyOf(&pipeline.SessionEvent{At: now, RequestID: "r"})
	b := keyOf(&pipeline.SessionEvent{At: roundTripped, RequestID: "r"})
	if a != b {
		t.Errorf("keys differ for same instant:\n  a: %+v\n  b: %+v", a, b)
	}
}

// TestPadLeft — right-aligns numeric cells so decimal points and digit
// magnitudes line up under each other. Blank stays blank so the eye can
// still scan for missing values.
func TestPadLeft(t *testing.T) {
	cases := []struct {
		in    string
		width int
		want  string
	}{
		{"1.23s", 10, "     1.23s"},
		{"", 10, ""},
		{"exactly10c", 10, "exactly10c"},
		{"toolongforthisfield", 10, "toolongforthisfield"},
		// Display-width padding: "1,048,576(−12.3k)" is 19 bytes but 17
		// display columns (U+2212 is 3 bytes, 1 column). A byte-length guard
		// would skip padding at width 19 and leave the cell left-shifted.
		{"1,048,576(−12.3k)", 19, "  1,048,576(−12.3k)"},
	}
	for _, tc := range cases {
		if got := padLeft(tc.in, tc.width); got != tc.want {
			t.Errorf("padLeft(%q, %d) = %q, want %q", tc.in, tc.width, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Sorting (#865)
// ---------------------------------------------------------------------------

// sortTestModel builds a model over events with the default columns and an
// events table, matching what rebuildEventsTable expects.
func sortTestModel(events []pipeline.SessionEvent) *model {
	m := &model{
		selectedSess: "s",
		events:       map[string][]pipeline.SessionEvent{"s": events},
		eventColumns: defaultColumnSelection(),
		width:        200,
	}
	m.eventsTbl = newEventsTable()
	return m
}

// cellAt reads one rendered cell out of the table by column id, so a test asserts
// on what the operator sees rather than on an internal index.
func cellAt(t *testing.T, m *model, row int, id eventColumnID) string {
	t.Helper()
	cols, _ := fitColumns(selectedColumns(m.eventColumns), m.width)
	for i, c := range cols {
		if c.id == id {
			rows := m.eventsTbl.Rows()
			if row >= len(rows) {
				t.Fatalf("row %d out of range (%d rows)", row, len(rows))
			}
			return rows[row][i]
		}
	}
	t.Fatalf("column %s is not visible", id)
	return ""
}

func hostsInOrder(m *model) []string {
	out := make([]string, 0, len(m.visibleRows))
	for _, r := range m.visibleRows {
		out = append(out, r.event.Host)
	}
	return out
}

// DURATION must sort NUMERICALLY. This is the column #865 names, and the one where
// sorting the rendered cell is visibly wrong: durationCell emits "340ms" and
// "1.20s", which compare lexically as 340ms > 1.20s.
func TestSort_DurationIsNumericNotLexical(t *testing.T) {
	events := []pipeline.SessionEvent{
		{Host: "mid", Phase: pipeline.SessionResponse, Duration: 340 * time.Millisecond},
		{Host: "slow", Phase: pipeline.SessionResponse, Duration: 1200 * time.Millisecond},
		{Host: "fast", Phase: pipeline.SessionResponse, Duration: 90 * time.Millisecond},
	}
	m := sortTestModel(events)

	m.sortCol, m.sortDesc = colDuration, true
	m.rebuildEventsTable()
	if got, want := hostsInOrder(m), []string{"slow", "mid", "fast"}; !slices.Equal(got, want) {
		t.Errorf("descending = %v, want %v (a string sort would give mid, fast, slow)", got, want)
	}

	m.sortDesc = false
	m.rebuildEventsTable()
	if got, want := hostsInOrder(m), []string{"fast", "mid", "slow"}; !slices.Equal(got, want) {
		t.Errorf("ascending = %v, want %v", got, want)
	}

	// The rendered cells confirm the values really are the ones being compared.
	// TrimSpace strips the right-align padding — asserting on the value, not the
	// display format.
	if got := strings.TrimSpace(cellAt(t, m, 0, colDuration)); got != "90ms" {
		t.Errorf("first ascending DURATION cell = %q, want 90ms", got)
	}
	if got := strings.TrimSpace(cellAt(t, m, 2, colDuration)); got != "1.20s" {
		t.Errorf("last ascending DURATION cell = %q, want 1.20s", got)
	}
}

// STATUS sorts by the integer, so the 5xx rows group at one end rather than
// collating "200" against "503" as text (which happens to agree here, but does not
// once statusCell decorates the cell).
func TestSort_StatusIsNumeric(t *testing.T) {
	events := []pipeline.SessionEvent{
		{Host: "a", Phase: pipeline.SessionResponse, StatusCode: 200},
		{Host: "b", Phase: pipeline.SessionResponse, StatusCode: 503},
		{Host: "c", Phase: pipeline.SessionResponse, StatusCode: 404},
	}
	m := sortTestModel(events)
	m.sortCol, m.sortDesc = colStatus, true
	m.rebuildEventsTable()
	if got, want := hostsInOrder(m), []string{"b", "c", "a"}; !slices.Equal(got, want) {
		t.Errorf("descending by status = %v, want %v", got, want)
	}
}

// HOST sorts lexically, and on the port-stripped host so that "h:443" and "h" land
// together rather than the port deciding.
func TestSort_HostIsLexicalAndPortStripped(t *testing.T) {
	events := []pipeline.SessionEvent{
		{Host: "zulu", Phase: pipeline.SessionRequest},
		{Host: "alpha:443", Phase: pipeline.SessionRequest},
		{Host: "mike", Phase: pipeline.SessionRequest},
	}
	m := sortTestModel(events)
	m.sortCol, m.sortDesc = colHost, false
	m.rebuildEventsTable()
	if got, want := hostsInOrder(m), []string{"alpha:443", "mike", "zulu"}; !slices.Equal(got, want) {
		t.Errorf("ascending by host = %v, want %v", got, want)
	}
}

// Ties keep arrival order. Most rows in a real session have no duration at all, and
// an unstable sort would reshuffle them on every streamed event.
func TestSort_TiesKeepChronologicalOrder(t *testing.T) {
	var events []pipeline.SessionEvent
	for _, h := range []string{"a", "b", "c", "d", "e"} {
		events = append(events, pipeline.SessionEvent{Host: h, Phase: pipeline.SessionRequest})
	}
	m := sortTestModel(events)
	m.sortCol, m.sortDesc = colDuration, true
	m.rebuildEventsTable()
	if got, want := hostsInOrder(m), []string{"a", "b", "c", "d", "e"}; !slices.Equal(got, want) {
		t.Errorf("all-equal keys reordered: %v, want arrival order %v", got, want)
	}
}

// Chronological is the default, and it must be byte-identical to what the table
// rendered before sorting existed.
func TestSort_ChronologicalIsUnchanged(t *testing.T) {
	events := []pipeline.SessionEvent{
		{Host: "one", Phase: pipeline.SessionResponse, Duration: 5 * time.Second, StatusCode: 200},
		{Host: "two", Phase: pipeline.SessionResponse, Duration: time.Millisecond, StatusCode: 500},
	}
	m := sortTestModel(events)
	m.rebuildEventsTable()
	if m.sortCol != "" {
		t.Fatalf("default sortCol = %q, want empty (chronological)", m.sortCol)
	}
	before := append([]table.Row(nil), m.eventsTbl.Rows()...)
	hostsBefore := hostsInOrder(m)

	// Sorting and then returning to chronological restores exactly the same rows.
	m.sortCol, m.sortDesc = colDuration, true
	m.rebuildEventsTable()
	m.sortCol, m.sortDesc = "", false
	m.rebuildEventsTable()

	after := m.eventsTbl.Rows()
	if len(after) != len(before) {
		t.Fatalf("row count changed: %d then %d", len(before), len(after))
	}
	for i := range before {
		if !slices.Equal(before[i], after[i]) {
			t.Errorf("row %d differs after a round trip:\n before %q\n after  %q", i, before[i], after[i])
		}
	}
	if got := hostsInOrder(m); !slices.Equal(got, hostsBefore) {
		t.Errorf("order after round trip = %v, want %v", got, hostsBefore)
	}
}

// The regression that matters most: sorting must not disturb the request/response
// pairing. computeEventPairs walks the CHRONOLOGICAL slice — its fallback pass
// searches backward from a response, and it mints the # exchange numbers in
// first-seen order — and the TOKENS/COST cells reach a request's figures through a
// partner map keyed by that slice's indices. Sorting reorders only the finished
// rows, so every one of those values must come out identical.
func TestSort_PairingAndExchangeFiguresSurvive(t *testing.T) {
	// Two exchanges, interleaved, with the SLOWER one first so a DURATION sort has
	// to move rows. Only the responses carry ids/figures, as in production.
	events := []pipeline.SessionEvent{
		{Host: "slow", Phase: pipeline.SessionRequest, RequestID: "r1", Direction: pipeline.Outbound},
		{Host: "fast", Phase: pipeline.SessionRequest, RequestID: "r2", Direction: pipeline.Outbound},
		{Host: "slow", Phase: pipeline.SessionResponse, RequestID: "r1", Direction: pipeline.Outbound,
			StatusCode: 200, Duration: 9 * time.Second,
			Inference: &pipeline.InferenceExtension{InputTokens: 5_000, OutputTokens: 100}},
		{Host: "fast", Phase: pipeline.SessionResponse, RequestID: "r2", Direction: pipeline.Outbound,
			StatusCode: 200, Duration: 10 * time.Millisecond,
			Inference: &pipeline.InferenceExtension{InputTokens: 20, OutputTokens: 7}},
	}

	// Baseline: chronological. Record each event's # and TOKENS keyed by identity, so
	// the comparison survives the rows moving.
	m := sortTestModel(events)
	m.rebuildEventsTable()
	type figures struct{ index, tokens string }
	want := map[eventKey]figures{}
	for row, er := range m.visibleRows {
		want[keyOf(er.event)] = figures{
			index:  cellAt(t, m, row, colIndex),
			tokens: cellAt(t, m, row, colTokens),
		}
	}
	if len(want) != 4 {
		t.Fatalf("baseline captured %d rows, want 4", len(want))
	}
	// The request row must genuinely have borrowed its paired response's prompt
	// count, or this test would be asserting that blanks stay blank.
	reqKey := keyOf(&events[0])
	if want[reqKey].tokens == "" {
		t.Fatalf("baseline: slow REQUEST row shows no TOKENS, so pairing was not exercised")
	}

	for _, tc := range []struct {
		name string
		col  eventColumnID
		desc bool
	}{
		{"duration desc", colDuration, true},
		{"duration asc", colDuration, false},
		{"tokens desc", colTokens, true},
		{"host asc", colHost, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m.sortCol, m.sortDesc = tc.col, tc.desc
			m.rebuildEventsTable()
			if len(m.visibleRows) != 4 {
				t.Fatalf("rows = %d, want 4", len(m.visibleRows))
			}
			for row, er := range m.visibleRows {
				k := keyOf(er.event)
				got := figures{
					index:  cellAt(t, m, row, colIndex),
					tokens: cellAt(t, m, row, colTokens),
				}
				if got != want[k] {
					t.Errorf("%s row (now at %d): # = %q tokens = %q, want # = %q tokens = %q",
						er.event.Host, row, got.index, got.tokens, want[k].index, want[k].tokens)
				}
			}
		})
	}
}

// TOKENS sorts on the numeric count, including on a REQUEST row whose figure comes
// from its paired response — so the sort key has to make the same pairedResponse
// call the cell does.
func TestSort_TokensIsNumericAcrossThePair(t *testing.T) {
	events := []pipeline.SessionEvent{
		{Host: "small", Phase: pipeline.SessionRequest, RequestID: "a", Direction: pipeline.Outbound},
		{Host: "small", Phase: pipeline.SessionResponse, RequestID: "a", Direction: pipeline.Outbound,
			Inference: &pipeline.InferenceExtension{InputTokens: 900}},
		{Host: "big", Phase: pipeline.SessionRequest, RequestID: "b", Direction: pipeline.Outbound},
		{Host: "big", Phase: pipeline.SessionResponse, RequestID: "b", Direction: pipeline.Outbound,
			Inference: &pipeline.InferenceExtension{InputTokens: 1_048_576}},
	}
	m := sortTestModel(events)
	m.sortCol, m.sortDesc = colTokens, true
	m.rebuildEventsTable()

	// The big REQUEST row leads: 1,048,576 formats with separators, so a string sort
	// would rank "900" above "1,048,576".
	first := m.visibleRows[0]
	if first.event.Host != "big" || first.event.Phase != pipeline.SessionRequest {
		t.Errorf("first row = %s/%s, want big/request", first.event.Host, first.event.Phase)
	}
	if got := strings.TrimSpace(cellAt(t, m, 0, colTokens)); got != "1,048,576" {
		t.Errorf("first TOKENS cell = %q, want 1,048,576", got)
	}
}

// A row with no figure sorts to the BOTTOM descending, keeping the end the operator
// sorted toward clear of rows the proxy could not measure.
func TestSort_BlankFiguresSortLastDescending(t *testing.T) {
	events := []pipeline.SessionEvent{
		{Host: "unmeasured", Phase: pipeline.SessionRequest},
		{Host: "measured", Phase: pipeline.SessionResponse, Duration: 2 * time.Second},
	}
	m := sortTestModel(events)
	m.sortCol, m.sortDesc = colDuration, true
	m.rebuildEventsTable()
	if got := hostsInOrder(m); !slices.Equal(got, []string{"measured", "unmeasured"}) {
		t.Errorf("descending = %v, want [measured unmeasured]", got)
	}
	if got := cellAt(t, m, 1, colDuration); got != "" {
		t.Errorf("unmeasured DURATION cell = %q, want blank", got)
	}
}

// visibleRows must be permuted with the rendered rows, since the table cursor
// indexes both: selectedEventRow answers what the detail pane and yank act on, so
// a drift would show one row and operate on another.
func TestSort_VisibleRowsStayParallelToTheTable(t *testing.T) {
	events := []pipeline.SessionEvent{
		{Host: "a", Phase: pipeline.SessionResponse, Duration: time.Millisecond},
		{Host: "b", Phase: pipeline.SessionResponse, Duration: 3 * time.Second},
		{Host: "c", Phase: pipeline.SessionResponse, Duration: 2 * time.Second},
	}
	m := sortTestModel(events)
	m.sortCol, m.sortDesc = colDuration, true
	m.rebuildEventsTable()

	for row := range m.visibleRows {
		setCursorVisible(&m.eventsTbl, row)
		er, ok := m.selectedEventRow()
		if !ok {
			t.Fatalf("row %d: no selected row", row)
		}
		// The HOST cell rendered at this row must belong to the event
		// selectedEventRow hands back.
		if got := cellAt(t, m, row, colHost); got != er.event.Host {
			t.Errorf("row %d: table shows host %q, selectedEventRow gives %q", row, got, er.event.Host)
		}
	}
}
