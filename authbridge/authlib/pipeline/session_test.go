package pipeline

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDirectionString(t *testing.T) {
	if Inbound.String() != "inbound" {
		t.Errorf("Inbound.String() = %q, want %q", Inbound.String(), "inbound")
	}
	if Outbound.String() != "outbound" {
		t.Errorf("Outbound.String() = %q, want %q", Outbound.String(), "outbound")
	}
	if got := Direction(42).String(); got != "unknown" {
		t.Errorf("unknown direction = %q, want %q", got, "unknown")
	}
}

func TestSessionEvent_MarshalJSON_ReadableEnums(t *testing.T) {
	e := SessionEvent{
		At:        time.Unix(1700000000, 0).UTC(),
		Direction: Inbound,
		Phase:     SessionResponse,
		Duration:  250 * time.Millisecond,
		Host:      "weather-tool-mcp:9090",
		A2A:       &A2AExtension{Method: "message/stream"},
	}

	data, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(data)

	// Enums must be strings, not numbers.
	if !strings.Contains(s, `"direction":"inbound"`) {
		t.Errorf("direction not serialized as string: %s", s)
	}
	if !strings.Contains(s, `"phase":"response"`) {
		t.Errorf("phase not serialized as string: %s", s)
	}
	// Duration must be emitted in ms, not the default nanosecond form.
	if !strings.Contains(s, `"durationMs":250`) {
		t.Errorf("durationMs missing or wrong: %s", s)
	}
}

// TestSessionEvent_JSONRoundTrip locks in the round-trip contract between
// MarshalJSON and UnmarshalJSON. A second Marshal of the decoded event must
// produce byte-identical JSON — that's the canary that catches fields added
// to SessionEvent + the wire struct + MarshalJSON but forgotten in
// UnmarshalJSON (the dropped field would silently vanish on the client).
func TestSessionEvent_JSONRoundTrip(t *testing.T) {
	stream := true
	maxTok := 256
	orig := SessionEvent{
		SessionID: "sess-xyz",
		At:        time.Unix(1700000000, 0).UTC(),
		Direction: Outbound,
		Phase:     SessionResponse,
		Duration:  1600 * time.Millisecond,
		Host:      "api.openai.com",
		A2A: &A2AExtension{
			Method: "message/stream", RPCID: "r-1", SessionID: "ctx-1",
			TaskID: "t-1", FinalStatus: "completed", Artifact: "hello",
			Role: "user", Parts: []A2APart{{Kind: "text", Content: "hi"}},
		},
		MCP: &MCPExtension{Method: "tools/call", RPCID: "m-2"},
		Inference: &InferenceExtension{
			Model: "gpt-4", Stream: stream, MaxTokens: &maxTok,
			Messages:   []InferenceMessage{{Role: "user", Content: "hi"}},
			Tools:      []InferenceTool{{Name: "get_weather", Description: "d"}},
			ToolCalls:  []InferenceToolCall{{ID: "c1", Name: "get_weather", Arguments: `{"city":"NYC"}`}},
			Completion: "Hello, world!", FinishReason: "stop", TotalTokens: 17,
		},
		Identity:   &EventIdentity{Subject: "alice", ClientID: "agent-1"},
		StatusCode: 200,
		Error:      &EventError{Kind: "upstream", Message: "timeout"},
		Tunnel:     true,
		HTTPMethod: "GET",
		HTTPPath:   "/v1/models",
	}

	first, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("first Marshal: %v", err)
	}
	var decoded SessionEvent
	if err := json.Unmarshal(first, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	second, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("second Marshal: %v", err)
	}
	if string(first) != string(second) {
		t.Errorf("round-trip drifted:\n  first:  %s\n  second: %s", first, second)
	}
}

func TestSessionEvent_MarshalJSON_OmitsEmpty(t *testing.T) {
	e := SessionEvent{Direction: Outbound, Phase: SessionRequest}
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(data)

	for _, field := range []string{
		"a2a", "mcp", "inference", "auth", "plugins", "identity", "statusCode",
		"error", "host", "durationMs",
		// The omitempty half of the version-skew contract: a new proxy that
		// recorded none of these must not emit the keys at all, so a client
		// predating them sees the same bytes it always did.
		"tunnel", "tunnelReason", "httpMethod", "httpPath",
	} {
		if strings.Contains(s, `"`+field+`":`) {
			t.Errorf("expected %q omitted when zero: %s", field, s)
		}
	}
}

// SessionDenied must serialize as "denied" on the wire so consumers
// filtering on phase (abctl `/deny`, stats queries) see a stable string
// rather than the numeric enum.
func TestSessionPhase_Denied_SerializesAsString(t *testing.T) {
	if got := SessionDenied.String(); got != "denied" {
		t.Errorf("SessionDenied.String() = %q, want %q", got, "denied")
	}
	data, err := json.Marshal(SessionDenied)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(data) != `"denied"` {
		t.Errorf("SessionDenied Marshal = %s, want %q", data, `"denied"`)
	}
	var p SessionPhase
	if err := json.Unmarshal([]byte(`"denied"`), &p); err != nil {
		t.Fatalf("Unmarshal denied: %v", err)
	}
	if p != SessionDenied {
		t.Errorf("Unmarshal(\"denied\") = %v, want SessionDenied", p)
	}
}

// Round-trip Invocations through JSON including both directions,
// multiple entries per direction, and the optional diagnostic fields.
// Also verifies the Action field serializes as the 5-value string
// vocabulary (not the old "decision"/"action"-per-direction shape).
// Locks the wire schema so a future field addition that's missed in
// sessionEventWire fails this test.
func TestSessionEvent_Invocations_JSONRoundTrip(t *testing.T) {
	orig := SessionEvent{
		At:        time.Unix(1700000000, 0).UTC(),
		Direction: Inbound,
		Phase:     SessionDenied,
		Invocations: &Invocations{
			Inbound: []Invocation{{
				Plugin: "jwt-validation",
				Action: ActionDeny,
				Reason: "jwt_failed",
				Details: map[string]string{
					"expected_issuer":   "http://keycloak.localtest.me:8080/realms/rossoctl",
					"expected_audience": "spiffe://localtest.me/ns/team1/sa/weather-tool",
				},
			}},
			Outbound: []Invocation{{
				Plugin: "token-exchange",
				Action: ActionModify,
				Reason: "cache_hit",
				Details: map[string]string{
					"route_matched":    "true",
					"route_host":       "weather-tool-mcp",
					"target_audience":  "spiffe://localtest.me/ns/team1/sa/weather-tool",
					"requested_scopes": "openid weather-aud",
					"cache_hit":        "true",
				},
			}},
		},
	}
	first, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("first Marshal: %v", err)
	}
	if !strings.Contains(string(first), `"phase":"denied"`) {
		t.Errorf("expected phase=denied in JSON: %s", first)
	}
	if !strings.Contains(string(first), `"action":"deny"`) {
		t.Errorf("expected action=deny (5-value vocab) in JSON: %s", first)
	}
	if !strings.Contains(string(first), `"action":"modify"`) {
		t.Errorf("expected action=modify in JSON: %s", first)
	}
	var decoded SessionEvent
	if err := json.Unmarshal(first, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	second, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("second Marshal: %v", err)
	}
	if string(first) != string(second) {
		t.Errorf("Invocations round-trip drifted:\n  first:  %s\n  second: %s", first, second)
	}
}

// Plugins map should round-trip as keyed RawMessage. The listener is
// expected to have already marshaled each value (Extensions.Plugins uses
// any; SessionEvent.Plugins uses json.RawMessage — it's the wire form).
// This test verifies the consumer side: decode lands RawMessage back in
// place so abctl (or any JSON consumer) can re-decode per plugin.
func TestSessionEvent_PluginsMap_JSONRoundTrip(t *testing.T) {
	orig := SessionEvent{
		At:        time.Unix(1700000000, 0).UTC(),
		Direction: Outbound,
		Phase:     SessionRequest,
		Plugins: map[string]json.RawMessage{
			"rate-limiter": json.RawMessage(`{"allowed":true,"tokensLeft":42}`),
			"custom-audit": json.RawMessage(`{"traceId":"abc-123"}`),
		},
	}
	first, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("first Marshal: %v", err)
	}
	if !strings.Contains(string(first), `"plugins":`) {
		t.Errorf("expected plugins in JSON: %s", first)
	}
	var decoded SessionEvent
	if err := json.Unmarshal(first, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(decoded.Plugins) != 2 {
		t.Fatalf("decoded.Plugins size = %d, want 2", len(decoded.Plugins))
	}
	// RawMessage should round-trip byte-identical so downstream consumers
	// can decode each plugin's payload into its own type.
	if got := string(decoded.Plugins["rate-limiter"]); got != `{"allowed":true,"tokensLeft":42}` {
		t.Errorf("rate-limiter payload drifted: %q", got)
	}
}

// TestSessionEventWire_HasEveryDomainField is the tripwire the round-trip test
// cannot be.
//
// SessionEvent has no struct tags: it marshals through the hand-maintained
// sessionEventWire DTO with field-by-field copies. Add a field to SessionEvent and
// forget the DTO and the field never crosses the wire — which is exactly what
// happened to TunnelReason. Every existing test stayed green, because
// TestSessionEvent_JSONRoundTrip compares Marshal→Unmarshal→Marshal for byte
// identity and a field absent from the DTO on BOTH sides round-trips identically.
// Symmetric loss is invisible to a symmetric test.
//
// This asserts structurally instead: for every exported field on SessionEvent there
// must be one of the same name on sessionEventWire. It cannot be satisfied by
// accident, and it fails at the moment the two drift rather than in the field.
func TestSessionEventWire_HasEveryDomainField(t *testing.T) {
	domain := reflect.TypeOf(SessionEvent{})
	wire := reflect.TypeOf(sessionEventWire{})

	// Deliberate renames, kept explicit so the check stays strict. Duration is
	// milliseconds on the wire, and the DTO field is named for the unit rather than
	// silently changing what a same-named field means.
	renamed := map[string]string{"Duration": "DurationMs"}

	for i := 0; i < domain.NumField(); i++ {
		f := domain.Field(i)
		if !f.IsExported() {
			continue
		}
		want := f.Name
		if alias, ok := renamed[want]; ok {
			want = alias
		}
		if _, ok := wire.FieldByName(want); !ok {
			t.Errorf("SessionEvent.%s has no sessionEventWire counterpart (looked for %q): "+
				"it will never reach the wire, and the round-trip test cannot see that",
				f.Name, want)
		}
	}
}

// TestSessionEventWire_EveryFieldSerializes is the value-level half: a populated
// event must produce a JSON key per wire field. Catches a field present in the DTO
// but missing from MarshalJSON's literal — the other way the two halves can drift.
func TestSessionEventWire_EveryFieldSerializes(t *testing.T) {
	ev := SessionEvent{
		SessionID: "s", At: time.Now(), Direction: Outbound, Phase: SessionRequest,
		RequestID: "r", Host: "h:443", StatusCode: 200, Duration: time.Second,
		Tunnel: true, TunnelReason: TunnelClientRejectedCA,
		HTTPMethod: "GET", HTTPPath: "/v1/models",
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal to map: %v", err)
	}
	for _, key := range []string{
		"sessionId", "at", "direction", "phase", "requestId",
		"host", "statusCode", "durationMs", "tunnel", "tunnelReason",
		"httpMethod", "httpPath",
	} {
		if _, ok := got[key]; !ok {
			t.Errorf("populated event produced no %q key; got %s", key, b)
		}
	}
}

// TestTunnelReasonsAreDocumented pins every reason against the operator docs.
//
// The doc previously said "two other reasons" when there were four, and omitted
// origin-unverified — the one that points at the destination rather than the client,
// and so the one most likely to send someone looking in the wrong place. A reason
// nobody can look up is barely better than the em dash it replaced.
func TestTunnelReasonsAreDocumented(t *testing.T) {
	doc, err := os.ReadFile("../../docs/laptop-service.md")
	if err != nil {
		t.Skipf("docs not readable from here: %v", err)
	}
	for _, reason := range []TunnelReason{
		TunnelClientRejectedCA, TunnelClientHungUp, TunnelHandshakeFailed,
		TunnelOriginUnverified, TunnelSkipCached, TunnelBridgeDisabled,
		TunnelPassthroughPort, TunnelPassthroughNonTLS, TunnelPassthroughHost,
	} {
		if !strings.Contains(string(doc), string(reason)) {
			t.Errorf("tunnel reason %q is not in laptop-service.md; an operator who sees "+
				"it in the timeline has nowhere to look it up", reason)
		}
	}
}
