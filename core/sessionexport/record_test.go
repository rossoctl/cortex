package sessionexport

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/model"
)

// secret strings planted in every content-bearing field of the event below. A record must
// contain none of them: that is the whole privacy claim of this package, and the only way to
// hold it is to look at the encoded bytes rather than to trust the projection's field list.
const (
	secretPrompt     = "SECRET-PROMPT-TEXT"
	secretCompletion = "SECRET-COMPLETION-TEXT"
	secretToolArgs   = "SECRET-TOOL-ARGUMENTS"
	secretToolResult = "SECRET-TOOL-RESULT"
	secretToolName   = "SECRET-TOOL-NAME"
	secretMCPParams  = "SECRET-MCP-PARAMS"
	secretA2APart    = "SECRET-A2A-PART"
	secretPath       = "SECRET-PATH-TOKEN"
	secretPlugin     = "SECRET-PLUGIN-PAYLOAD"
	secretSPIFFE     = "SECRET-SPIFFE-ID"
	secretSubject    = "SECRET-SUBJECT"
)

// loadedEvent is an event with every field this package reads populated AND every
// content-bearing field it must not read populated with a planted secret.
func loadedEvent() *model.SessionEvent {
	return &model.SessionEvent{
		SessionID:     "sess-1",
		Seq:           42,
		RequestID:     "req-1",
		At:            time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC),
		Direction:     model.Outbound,
		Phase:         model.SessionResponse,
		Host:          "api.example.com",
		RequestedHost: "asked.example.com",
		HTTPMethod:    "POST",
		HTTPPath:      "/v1/messages/" + secretPath,
		StatusCode:    200,
		Duration:      1500 * time.Millisecond,
		Ttfb:          400 * time.Millisecond,
		BytesUp:       12345,
		BytesDown:     678,
		Tunnel:        true,
		TunnelReason:  model.TunnelReason("dial-failed"),
		Client:        &model.EventClient{Name: "claude-code", Version: "2.1.270"},
		Error:         &model.EventError{Kind: "upstream", Code: "429", Message: secretPrompt},
		Identity:      &model.EventIdentity{Subject: secretSubject},
		TLS:           &model.EventTLS{PeerSPIFFEID: secretSPIFFE},
		Plugins:       map[string]json.RawMessage{"p": json.RawMessage(`{"k":"` + secretPlugin + `"}`)},
		MCP:           &model.MCPExtension{Method: "tools/call", Params: map[string]any{"a": secretMCPParams}},
		Inference: &model.InferenceExtension{
			Model:            "claude-opus-5",
			RequestedModel:   "claude-sonnet-5",
			ServedModel:      "claude-opus-5-20260101",
			Stream:           true,
			StreamedResponse: true,
			FinishReason:     "tool_use",
			AgentRole:        model.AgentRoleSubagent,
			PromptTokens:     1000,
			CompletionTokens: 200,
			CacheReadTokens:  300,
			CacheWriteTokens: 400,
			Messages:         []model.InferenceMessage{{Role: "user", Content: secretPrompt}},
			Tools:            []model.InferenceTool{{Name: secretToolName, Description: secretPrompt}},
			ToolCalls:        []model.InferenceToolCall{{Name: secretToolName, Arguments: secretToolArgs}},
			ToolResults:      []model.InferenceToolResult{{Content: secretToolResult}},
			Completion:       secretCompletion,
		},
	}
}

// TestRecord_CarriesNoContent is the privacy contract. It encodes a record projected from an
// event whose every content field holds a planted secret and fails if any secret survives.
//
// Checking the ENCODED BYTES rather than the struct's fields is the point: a field added to
// Record later that happens to carry content fails here without anyone remembering this test
// exists.
func TestRecord_CarriesNoContent(t *testing.T) {
	var buf bytes.Buffer
	if err := NewWriter(&buf).WriteEvent(loadedEvent(), "sess-1"); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
	got := buf.String()
	for _, secret := range []string{
		secretPrompt, secretCompletion, secretToolArgs, secretToolResult, secretToolName,
		secretMCPParams, secretA2APart, secretPath, secretPlugin, secretSPIFFE, secretSubject,
	} {
		if strings.Contains(got, secret) {
			t.Errorf("record leaked %q — a content-bearing field reached the export:\n%s", secret, got)
		}
	}
}

// TestRecord_EveryFieldIsPopulated holds the other half: that FromEvent actually fills every
// field Record declares. A field added to the struct but not to FromEvent is silently absent
// from every export, which is the quiet failure this catches.
//
// It reflects over the struct rather than listing fields, so it covers additions by default.
func TestRecord_EveryFieldIsPopulated(t *testing.T) {
	r := FromEvent(loadedEvent(), "sess-1")
	v := reflect.ValueOf(r)
	for i := range v.NumField() {
		f := v.Type().Field(i)
		if v.Field(i).IsZero() {
			t.Errorf("Record.%s is zero after projecting a fully-populated event — "+
				"either FromEvent does not set it, or loadedEvent does not exercise it", f.Name)
		}
	}
}

// TestFromEvent_UnknownClientStaysAbsent: Label() answers "unknown" for a nil client, and an
// aggregate must not report a real agent by that name.
func TestFromEvent_UnknownClientStaysAbsent(t *testing.T) {
	e := loadedEvent()
	e.Client = nil
	if got := FromEvent(e, "s").Client; got != "" {
		t.Errorf("Client = %q, want empty — absence must not become a display string", got)
	}
}

// TestFromEvent_CountsFallBackToTheEventsOwn covers a projected copy: sessionapi's
// view=summary drops Messages and Tools but keeps MessageCount and ToolCount, so reading
// only len() reports zero for every row such a reader delivers.
func TestFromEvent_CountsFallBackToTheEventsOwn(t *testing.T) {
	e := loadedEvent()
	e.Inference.Messages, e.Inference.Tools = nil, nil
	e.Inference.MessageCount, e.Inference.ToolCount = 177, 11

	r := FromEvent(e, "s")
	if r.MessageCount != 177 {
		t.Errorf("MessageCount = %d, want 177 from the event's own count", r.MessageCount)
	}
	if r.ToolCount != 11 {
		t.Errorf("ToolCount = %d, want 11 from the event's own count", r.ToolCount)
	}
}

// TestFromEvent_SessionIDFallback: an archive reader stamps the id, since an event's own
// field may be unset; the event's own id wins when it has one.
func TestFromEvent_SessionIDFallback(t *testing.T) {
	e := loadedEvent()
	e.SessionID = ""
	if got := FromEvent(e, "from-archive").SessionID; got != "from-archive" {
		t.Errorf("SessionID = %q, want the caller's id when the event carries none", got)
	}
	e.SessionID = "own"
	if got := FromEvent(e, "from-archive").SessionID; got != "own" {
		t.Errorf("SessionID = %q, want the event's own id to win", got)
	}
}

// TestFromEvent_NilIsSafe: an export walks whatever the archive hands it.
func TestFromEvent_NilIsSafe(t *testing.T) {
	if got := FromEvent(nil, "s"); got.Schema != SchemaVersion {
		t.Errorf("Schema = %d, want %d even for a nil event", got.Schema, SchemaVersion)
	}
}

// TestWriter_IsOneLinePerRecord: NDJSON's only structural promise.
func TestWriter_IsOneLinePerRecord(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	for range 3 {
		if err := w.WriteEvent(loadedEvent(), "s"); err != nil {
			t.Fatalf("WriteEvent: %v", err)
		}
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3:\n%s", len(lines), buf.String())
	}
	for i, ln := range lines {
		var r Record
		if err := json.Unmarshal([]byte(ln), &r); err != nil {
			t.Errorf("line %d is not valid JSON: %v", i, err)
		}
	}
	if w.Count() != 3 {
		t.Errorf("Count = %d, want 3", w.Count())
	}
}

// TestRecord_DurationsAreMilliseconds pins the unit, since the field names are the only
// place the unit is stated and a consumer reading ns as ms is off by a million.
func TestRecord_DurationsAreMilliseconds(t *testing.T) {
	r := FromEvent(loadedEvent(), "s")
	if r.DurationMs != 1500 {
		t.Errorf("DurationMs = %d, want 1500", r.DurationMs)
	}
	if r.TtfbMs != 400 {
		t.Errorf("TtfbMs = %d, want 400", r.TtfbMs)
	}
}

// TestRecord_AbsentNumbersAreOmitted: zero means "not recorded", so it must not appear on
// the wire as a real 0 — the event's own convention for BytesUp/BytesDown and Duration.
func TestRecord_AbsentNumbersAreOmitted(t *testing.T) {
	e := &model.SessionEvent{SessionID: "s", At: time.Now(), Phase: model.SessionRequest}
	b, err := json.Marshal(FromEvent(e, "s"))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"durationMs", "ttfbMs", "bytesUp", "bytesDown", "promptTokens", "statusCode"} {
		if bytes.Contains(b, []byte(`"`+key+`"`)) {
			t.Errorf("%s present on an unmeasured event; an absent figure must not render as 0:\n%s", key, b)
		}
	}
}
