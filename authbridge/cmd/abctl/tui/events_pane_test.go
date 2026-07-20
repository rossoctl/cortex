package tui

import (
	"encoding/json"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rossoctl/rossocortex/authbridge/authlib/pipeline"
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

// TestSpanLevels_Prefix locks the PHASE-column prefix: empty levels render as
// empty string; one level renders one glyph; two levels render two glyphs.
func TestSpanLevels_Prefix(t *testing.T) {
	cases := []struct {
		name string
		s    spanLevels
		want string
	}{
		{"none", spanLevels{}, ""},
		{"outer only — start", spanLevels{outer: glyphStart}, "┌"},
		{"outer only — middle", spanLevels{outer: glyphMiddle}, "│"},
		{"outer only — end", spanLevels{outer: glyphEnd}, "└"},
		{"both — outer middle, inner start", spanLevels{outer: glyphMiddle, inner: glyphStart}, "│┌"},
		{"both — outer middle, inner end", spanLevels{outer: glyphMiddle, inner: glyphEnd}, "│└"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.prefix(); got != tc.want {
				t.Errorf("prefix() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestComputeSpanGlyphs covers per-row tree-glyph assignment for the PHASE
// column. Up to two levels of (request, response) nesting are surfaced — the
// widest containing span as outer, the next-widest as inner, deeper dropped.
func TestComputeSpanGlyphs(t *testing.T) {
	none := spanLevels{}
	outer := func(g spanGlyph) spanLevels { return spanLevels{outer: g} }
	both := func(o, i spanGlyph) spanLevels { return spanLevels{outer: o, inner: i} }

	cases := []struct {
		name  string
		pairs map[int]int
		n     int
		want  []spanLevels
	}{
		{"no pairs", nil, 3, []spanLevels{none, none, none}},
		{
			name:  "adjacent pair",
			pairs: map[int]int{0: 1, 1: 0},
			n:     2,
			want:  []spanLevels{outer(glyphStart), outer(glyphEnd)},
		},
		{
			name:  "one row in between",
			pairs: map[int]int{0: 2, 2: 0},
			n:     3,
			want:  []spanLevels{outer(glyphStart), outer(glyphMiddle), outer(glyphEnd)},
		},
		{
			// The real shape: an outer a2a exchange (0,5) bracketing two inner
			// inference exchanges (1,2) and (3,4).
			name: "nested exchanges (a2a containing two inference calls)",
			pairs: map[int]int{
				0: 5, 5: 0,
				1: 2, 2: 1,
				3: 4, 4: 3,
			},
			n: 6,
			want: []spanLevels{
				outer(glyphStart),
				both(glyphMiddle, glyphStart),
				both(glyphMiddle, glyphEnd),
				both(glyphMiddle, glyphStart),
				both(glyphMiddle, glyphEnd),
				outer(glyphEnd),
			},
		},
		{
			// The #52 case: a pair (2,3) nested THREE deep — inside a middle
			// span (1,4) inside an outer span (0,5). The innermost pair's
			// endpoints must still show their ┌/└ corners (so its req/resp
			// connect) rather than the middle span's bar masking them. inner =
			// the row's narrowest containing span, not the second-widest.
			name: "triple-nested innermost pair keeps its corners",
			pairs: map[int]int{
				0: 5, 5: 0,
				1: 4, 4: 1,
				2: 3, 3: 2,
			},
			n: 6,
			want: []spanLevels{
				outer(glyphStart),             // 0: outer starts
				both(glyphMiddle, glyphStart), // 1: outer mid, middle-span starts
				both(glyphMiddle, glyphStart), // 2: outer mid, innermost STARTS (was masked to middle)
				both(glyphMiddle, glyphEnd),   // 3: outer mid, innermost ENDS (was masked to middle)
				both(glyphMiddle, glyphEnd),   // 4: outer mid, middle-span ends
				outer(glyphEnd),               // 5: outer ends
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeSpanGlyphs(tc.pairs, tc.n)
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d", len(got), len(tc.want))
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("row %d: got {outer=%q inner=%q}, want {outer=%q inner=%q}",
						i, string(rune(got[i].outer)), string(rune(got[i].inner)),
						string(rune(tc.want[i].outer)), string(rune(tc.want[i].inner)))
				}
			}
		})
	}
}

// TestComputeEventPairs_NestedExchangeGlyphs is the end-to-end #23 shape: an
// inbound a2a message/stream request, two outbound inference exchanges during
// processing, then the a2a response. The a2a request/response must pair and
// bracket (┌ … └) with the inference exchanges nested (│┌ … │└) inside.
func TestComputeEventPairs_NestedExchangeGlyphs(t *testing.T) {
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

	glyphs := computeSpanGlyphs(partner, len(rows))
	want := []string{"┌", "│┌", "│└", "│┌", "│└", "└"}
	for i, w := range want {
		if got := glyphs[i].prefix(); got != w {
			t.Errorf("row %d prefix = %q, want %q", i, got, w)
		}
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
