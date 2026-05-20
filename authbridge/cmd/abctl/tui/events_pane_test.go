package tui

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
)

// TestShortPhase_Denied locks the abctl rendering string for the
// denied phase — changing this silently would ripple into the events
// table and teatest snapshots.
func TestShortPhase_Denied(t *testing.T) {
	if got := shortPhase(pipeline.SessionDenied); got != "deny" {
		t.Errorf("shortPhase(SessionDenied) = %q, want deny", got)
	}
}

// TestInvocationRow_Cells exercises the ACTION and PLUGIN column
// renderers for each shape a row can take: an Invocation with an action,
// multiple invocations (the row is per-invocation, each carries only
// its own plugin), and the pseudo-row fallback when an event has no
// Invocations at all.
func TestInvocationRow_Cells(t *testing.T) {
	evWithInv := &pipeline.SessionEvent{
		Invocations: &pipeline.Invocations{
			Inbound: []pipeline.Invocation{
				{Plugin: "jwt-validation", Action: pipeline.ActionAllow},
				{Plugin: "a2a-parser", Action: pipeline.ActionObserve},
			},
		},
	}
	cases := []struct {
		name       string
		row        invocationRow
		wantAction string
		wantPlugin string
	}{
		{
			name:       "empty pseudo-row",
			row:        invocationRow{event: &pipeline.SessionEvent{}},
			wantAction: "—",
			wantPlugin: "—",
		},
		{
			name: "inbound allow",
			row: invocationRow{
				event:     evWithInv,
				inv:       &evWithInv.Invocations.Inbound[0],
				direction: pipeline.Inbound,
			},
			wantAction: "allow",
			wantPlugin: "jwt-validation",
		},
		{
			name: "inbound observe (parser)",
			row: invocationRow{
				event:     evWithInv,
				inv:       &evWithInv.Invocations.Inbound[1],
				direction: pipeline.Inbound,
			},
			wantAction: "observe",
			wantPlugin: "a2a-parser",
		},
		{
			name: "shadow deny (observe mode)",
			row: invocationRow{
				event: &pipeline.SessionEvent{},
				inv: &pipeline.Invocation{
					Plugin: "pii-scrubber",
					Action: pipeline.ActionDeny,
					Shadow: true,
				},
				direction: pipeline.Inbound,
			},
			// Asterisk suffix flags the would-have-blocked event so
			// operators can visually separate real denies from shadow
			// denies in the timeline.
			wantAction: "deny*",
			wantPlugin: "pii-scrubber",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.row.actionCell(); got != tc.wantAction {
				t.Errorf("actionCell = %q, want %q", got, tc.wantAction)
			}
			if got := tc.row.pluginCell(); got != tc.wantPlugin {
				t.Errorf("pluginCell = %q, want %q", got, tc.wantPlugin)
			}
		})
	}
}

// TestFlattenInvocations covers the core expansion: an event with N
// invocations should produce N rows; an event with zero invocations
// should still produce one pseudo-row so the event stays reachable.
func TestFlattenInvocations(t *testing.T) {
	events := []pipeline.SessionEvent{
		// 2 inbound invocations → 2 rows
		{
			Direction: pipeline.Inbound,
			Invocations: &pipeline.Invocations{
				Inbound: []pipeline.Invocation{
					{Plugin: "jwt-validation", Action: pipeline.ActionAllow},
					{Plugin: "a2a-parser", Action: pipeline.ActionObserve},
				},
			},
		},
		// 1 outbound invocation → 1 row
		{
			Direction: pipeline.Outbound,
			Invocations: &pipeline.Invocations{
				Outbound: []pipeline.Invocation{
					{Plugin: "token-exchange", Action: pipeline.ActionSkip},
				},
			},
		},
		// no invocations → 1 pseudo-row
		{Direction: pipeline.Inbound},
	}
	got := flattenInvocations(events)
	if len(got) != 4 {
		t.Fatalf("flattenInvocations returned %d rows, want 4", len(got))
	}
	if got[0].inv == nil || got[0].inv.Plugin != "jwt-validation" {
		t.Errorf("row 0 = %+v, want jwt-validation", got[0])
	}
	if got[1].inv == nil || got[1].inv.Plugin != "a2a-parser" {
		t.Errorf("row 1 = %+v, want a2a-parser", got[1])
	}
	if got[2].inv == nil || got[2].inv.Plugin != "token-exchange" {
		t.Errorf("row 2 = %+v, want token-exchange", got[2])
	}
	if got[3].inv != nil {
		t.Errorf("row 3 should be pseudo-row with nil inv, got %+v", got[3])
	}
}

// TestPairInvocationRows verifies that each plugin's request row pairs
// with its own response row independently. A pipeline with
// jwt-validation + a2a-parser on both request and response phases yields
// 4 rows (2 req + 2 resp), and pairing should connect them in-plugin:
// jwt-validation-req ↔ jwt-validation-resp; a2a-parser-req ↔
// a2a-parser-resp.
func TestPairInvocationRows(t *testing.T) {
	inv := func(plugin string, action pipeline.InvocationAction) *pipeline.Invocation {
		return &pipeline.Invocation{Plugin: plugin, Action: action}
	}
	reqEv := &pipeline.SessionEvent{Direction: pipeline.Inbound, Phase: pipeline.SessionRequest}
	respEv := &pipeline.SessionEvent{Direction: pipeline.Inbound, Phase: pipeline.SessionResponse}
	rows := []invocationRow{
		{event: reqEv, inv: inv("jwt-validation", pipeline.ActionAllow), direction: pipeline.Inbound},
		{event: reqEv, inv: inv("a2a-parser", pipeline.ActionObserve), direction: pipeline.Inbound},
		{event: respEv, inv: inv("jwt-validation", pipeline.ActionAllow), direction: pipeline.Inbound},
		{event: respEv, inv: inv("a2a-parser", pipeline.ActionObserve), direction: pipeline.Inbound},
	}
	pairs := pairInvocationRows(rows)
	if pairs[0] != 2 || pairs[2] != 0 {
		t.Errorf("expected jwt-validation pair 0↔2, got %v", pairs)
	}
	if pairs[1] != 3 || pairs[3] != 1 {
		t.Errorf("expected a2a-parser pair 1↔3, got %v", pairs)
	}
}

// TestMatchInvocationRow_DenyShortcut verifies that typing "deny" in the
// filter box surfaces both the SessionDenied phase AND any invocation
// whose Action is ActionDeny (jwt-validation or token-exchange
// denials).
func TestMatchInvocationRow_DenyShortcut(t *testing.T) {
	denied := invocationRow{
		event: &pipeline.SessionEvent{Phase: pipeline.SessionDenied},
	}
	if !matchInvocationRow(denied, "deny") {
		t.Error("SessionDenied event should match the `deny` shortcut")
	}

	inboundDeny := invocationRow{
		event: &pipeline.SessionEvent{Phase: pipeline.SessionRequest},
		inv:   &pipeline.Invocation{Action: pipeline.ActionDeny},
	}
	if !matchInvocationRow(inboundDeny, "deny") {
		t.Error("inbound-deny invocation should match the `deny` shortcut")
	}

	clean := invocationRow{
		event: &pipeline.SessionEvent{Phase: pipeline.SessionRequest},
		inv:   &pipeline.Invocation{Action: pipeline.ActionAllow},
	}
	if matchInvocationRow(clean, "deny") {
		t.Error("allow invocation should NOT match the `deny` shortcut")
	}
}

// TestMatchInvocationRow_PluginSubstring verifies that filtering by plugin
// name substring-matches against the Invocation.Plugin field so operators
// can isolate one plugin's rows.
func TestMatchInvocationRow_PluginSubstring(t *testing.T) {
	row := invocationRow{
		event: &pipeline.SessionEvent{Phase: pipeline.SessionRequest},
		inv:   &pipeline.Invocation{Plugin: "jwt-validation", Action: pipeline.ActionSkip, Reason: "path_bypass", Path: "/healthz"},
	}
	if !matchInvocationRow(row, "jwt-validation") {
		t.Error("filter jwt-validation should match")
	}
	if !matchInvocationRow(row, "path_bypass") {
		t.Error("filter by reason should match")
	}
	if !matchInvocationRow(row, "/healthz") {
		t.Error("filter by path should match")
	}
	if matchInvocationRow(row, "token-exchange") {
		t.Error("filter token-exchange should NOT match a jwt-validation row")
	}
}

// TestMatchInvocationRow_PluginPrefix tests the `plugin:<name>` escape-
// hatch filter — matches when the event's Plugins map contains <name>.
func TestMatchInvocationRow_PluginPrefix(t *testing.T) {
	row := invocationRow{
		event: &pipeline.SessionEvent{
			Plugins: map[string]json.RawMessage{
				"rate-limiter": json.RawMessage(`{"allowed":true}`),
			},
		},
	}
	if !matchInvocationRow(row, "plugin:rate-limiter") {
		t.Error("expected match on plugin:rate-limiter")
	}
	if matchInvocationRow(row, "plugin:nonexistent") {
		t.Error("expected no match for a plugin not in the map")
	}
}

// TestComputeEventPairIDs_BypassResponseWithEmptyInvocations locks the
// event-level fallback pairing: when a response event has no plugin
// invocations at all (e.g. jwt-validation bypass response), it should
// still pair with its preceding request event via direction+host match
// so the # column shows the same ID on both rows.
func TestComputeEventPairIDs_BypassResponseWithEmptyInvocations(t *testing.T) {
	events := []pipeline.SessionEvent{
		// Event 0: bypass req — jwt-validation skip invocation
		{
			Direction: pipeline.Inbound,
			Phase:     pipeline.SessionRequest,
			Invocations: &pipeline.Invocations{Inbound: []pipeline.Invocation{{
				Plugin: "jwt-validation",
				Phase:  pipeline.InvocationPhaseRequest,
				Action: pipeline.ActionSkip,
			}}},
		},
		// Event 1: bypass resp — no invocations (response-phase filter returns empty)
		{Direction: pipeline.Inbound, Phase: pipeline.SessionResponse, StatusCode: 200},
		// Event 2: bypass req (different bypass path, same direction+host="")
		{
			Direction: pipeline.Inbound,
			Phase:     pipeline.SessionRequest,
			Invocations: &pipeline.Invocations{Inbound: []pipeline.Invocation{{
				Plugin: "jwt-validation",
				Phase:  pipeline.InvocationPhaseRequest,
				Action: pipeline.ActionSkip,
			}}},
		},
		// Event 3: bypass resp
		{Direction: pipeline.Inbound, Phase: pipeline.SessionResponse, StatusCode: 200},
	}

	rows := flattenInvocations(events)
	pairs := pairInvocationRows(rows)
	ids := computeEventPairIDs(rows, pairs)

	id0, id1 := ids[&events[0]], ids[&events[1]]
	id2, id3 := ids[&events[2]], ids[&events[3]]

	if id0 != id1 {
		t.Errorf("bypass req/resp #1: got ids (%d,%d), want equal", id0, id1)
	}
	if id2 != id3 {
		t.Errorf("bypass req/resp #2: got ids (%d,%d), want equal", id2, id3)
	}
	if id0 == id2 {
		t.Errorf("different bypass pairs should have different ids, both got %d", id0)
	}
}

// TestPairInvocationRows_MethodDiscrimination locks the method-aware
// pairing. Fire-and-forget MCP methods (notifications/initialized) have
// no response; a subsequent tools/list req+resp pair must not be
// disrupted by the notification's mcp-parser row greedily claiming the
// tools/list response row.
func TestPairInvocationRows_MethodDiscrimination(t *testing.T) {
	mk := func(phase pipeline.SessionPhase, method string) pipeline.SessionEvent {
		return pipeline.SessionEvent{
			Direction: pipeline.Outbound,
			Phase:     phase,
			MCP:       &pipeline.MCPExtension{Method: method},
			Invocations: &pipeline.Invocations{Outbound: []pipeline.Invocation{{
				Plugin: "mcp-parser",
				Phase:  invocationPhaseFor(phase),
				Action: pipeline.ActionObserve,
			}}},
		}
	}
	events := []pipeline.SessionEvent{
		mk(pipeline.SessionRequest, "notifications/initialized"), // no resp (fire and forget)
		mk(pipeline.SessionRequest, "tools/list"),
		mk(pipeline.SessionResponse, "tools/list"),
	}
	rows := flattenInvocations(events)
	pairs := pairInvocationRows(rows)
	ids := computeEventPairIDs(rows, pairs)

	if ids[&events[1]] != ids[&events[2]] {
		t.Errorf("tools/list req and resp must share ID, got %d vs %d",
			ids[&events[1]], ids[&events[2]])
	}
	if ids[&events[0]] == ids[&events[1]] {
		t.Errorf("notifications/initialized (orphan) must not share ID with tools/list, both got %d",
			ids[&events[0]])
	}
}

func invocationPhaseFor(p pipeline.SessionPhase) pipeline.InvocationPhase {
	if p == pipeline.SessionResponse {
		return pipeline.InvocationPhaseResponse
	}
	return pipeline.InvocationPhaseRequest
}

// Build a realistic auth-only request/response pair and assert that the
// flatten → pair pipeline connects them end-to-end. Regression-protects
// the chart-default case (jwt-validation only, no parsers).
func TestFlattenPair_AuthOnlyEndToEnd(t *testing.T) {
	now := time.Date(2026, 5, 8, 14, 22, 5, 0, time.UTC)
	invs := &pipeline.Invocations{Inbound: []pipeline.Invocation{{Plugin: "jwt-validation", Action: pipeline.ActionAllow}}}
	events := []pipeline.SessionEvent{
		{At: now, Direction: pipeline.Inbound, Phase: pipeline.SessionRequest, Invocations: invs, Host: "weather-agent"},
		{At: now.Add(12 * time.Millisecond), Direction: pipeline.Inbound, Phase: pipeline.SessionResponse, Invocations: invs, Host: "weather-agent", StatusCode: 200, Duration: 12 * time.Millisecond},
	}

	rows := flattenInvocations(events)
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	pairs := pairInvocationRows(rows)
	if pairs[0] != 1 || pairs[1] != 0 {
		t.Errorf("expected auth-only req/resp to pair: got %v", pairs)
	}
	if got := rows[0].actionCell(); got != "allow" {
		t.Errorf("req actionCell = %q, want allow", got)
	}
	if got := rows[1].pluginCell(); got != "jwt-validation" {
		t.Errorf("resp pluginCell = %q, want jwt-validation", got)
	}
	if got := statusCell(*rows[1].event); got != "200" {
		t.Errorf("statusCell = %q, want 200", got)
	}
}

// TestIsSkipRow checks the predicate the events table uses to decide
// whether a row should be hidden under the default skip-hiding mode.
// Only Action=skip rows return true; the pseudo-row with no
// Invocation, and rows for any other action, are kept.
func TestIsSkipRow(t *testing.T) {
	cases := []struct {
		name string
		row  invocationRow
		want bool
	}{
		{
			name: "skip action",
			row:  invocationRow{inv: &pipeline.Invocation{Action: pipeline.ActionSkip}},
			want: true,
		},
		{
			name: "allow action",
			row:  invocationRow{inv: &pipeline.Invocation{Action: pipeline.ActionAllow}},
			want: false,
		},
		{
			name: "deny action",
			row:  invocationRow{inv: &pipeline.Invocation{Action: pipeline.ActionDeny}},
			want: false,
		},
		{
			name: "observe action",
			row:  invocationRow{inv: &pipeline.Invocation{Action: pipeline.ActionObserve}},
			want: false,
		},
		{
			name: "modify action",
			row:  invocationRow{inv: &pipeline.Invocation{Action: pipeline.ActionModify}},
			want: false,
		},
		{
			name: "pseudo-row with no invocation",
			// Parser-only events emit a pseudo-row so they remain
			// reachable. These should never be classified as skips —
			// hiding them would lose protocol-only events from the
			// timeline.
			row:  invocationRow{event: &pipeline.SessionEvent{}, inv: nil},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSkipRow(tc.row); got != tc.want {
				t.Errorf("isSkipRow = %v, want %v", got, tc.want)
			}
		})
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
