package pipeline

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"reflect"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite testdata/session-event.golden.json")

// goldenEvent sets every field of SessionEvent, so that every key of sessionEventWire is
// in the golden file. The nested extension types set some fields only.
func goldenEvent() SessionEvent {
	at := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	return SessionEvent{
		SessionID: "s1", Seq: 7, At: at, Direction: Outbound, Phase: SessionResponse, RequestID: "req-7",
		A2A:       &A2AExtension{Method: "message/send", Parts: []A2APart{{Kind: "text", Content: "hi"}}},
		MCP:       &MCPExtension{Method: "tools/call", RPCID: "7", Params: map[string]any{"name": "get_weather"}},
		Inference: &InferenceExtension{Model: "claude-opus-5", Messages: []InferenceMessage{{Role: "user", Content: "q"}}},
		Invocations: &Invocations{Outbound: []Invocation{{
			Plugin: "token-exchange", Action: ActionModify, Reason: "exchanged", Details: map[string]string{"aud": "x"},
		}}},
		Plugins:         map[string]json.RawMessage{"cost": json.RawMessage(`{"usd":0.01}`)},
		Identity:        &EventIdentity{Subject: "alice", Scopes: []string{"openid"}, ClientID: "agent", AgentID: "a1"},
		StatusCode:      503,
		Error:           &EventError{Kind: "upstream", Code: "503", Message: "unavailable"},
		Host:            "api.example:443",
		RequestedHost:   "asked.example:443",
		Duration:        1500 * time.Millisecond,
		Ttfb:            200 * time.Millisecond,
		TLS:             &EventTLS{Version: "TLS 1.3", CipherSuite: "TLS_AES_128_GCM_SHA256", PeerSPIFFEID: "spiffe://example/agent"},
		Tunnel:          true,
		TunnelReason:    TunnelClientRejectedCA,
		BytesUp:         100,
		BytesDown:       2000,
		HTTPMethod:      "POST",
		HTTPPath:        "/v1/messages",
		UpgradeStripped: "websocket",
		Client:          &EventClient{Name: "claude-code", Version: "2.1.14", Raw: "claude-cli/2.1.14"},
		Subagent:        "a1a2e36cb5946f31c",
		ParentSubagent:  "a0974b0b8591dc76b",
		Proc:            "/bin/claude",
		ClientProc:      "/bin/zsh",
		SessionHeader:   "x-session-id",
		DialAddr:        "203.0.113.7:443",
	}
}

// The event wire form is a contract with agentop and the session archive. A change to
// it changes the golden file, and so it shows in review. Run with -update to rewrite it.
func TestSessionEventWire_Golden(t *testing.T) {
	ev := goldenEvent()
	v := reflect.ValueOf(ev)
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).IsZero() {
			t.Errorf("goldenEvent leaves SessionEvent.%s unset: set it, then run with -update", v.Type().Field(i).Name)
		}
	}
	b, err := json.MarshalIndent(ev, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, '\n')
	const path = "testdata/session-event.golden.json"
	if *update {
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	golden, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update to write it): %v", err)
	}
	if !bytes.Equal(b, golden) {
		t.Errorf("wire form differs from %s (run with -update if the change is deliberate)\n got: %s\nwant: %s", path, b, golden)
	}
	// Decode the compact form: RawJSON and json.RawMessage keep their bytes as written.
	var compact bytes.Buffer
	if err := json.Compact(&compact, golden); err != nil {
		t.Fatal(err)
	}
	var back SessionEvent
	if err := json.Unmarshal(compact.Bytes(), &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, ev) {
		t.Errorf("golden decodes to a different event\n got: %+v\nwant: %+v", back, ev)
	}
}
