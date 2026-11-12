package sessionexport

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/internal/parsercommon"
	"github.com/rossoctl/cortex/core/model"
)

// secret strings planted in every content-bearing field of the event below. A record must
// contain none of them: that is the whole privacy claim of this package, and the only way to
// hold it is to look at the encoded bytes rather than to trust the projection's field list.
//
// EVERY ONE OF THESE MUST BE ASSIGNED IN loadedEvent AND CHECKED IN allSecrets, or the
// assertion it stands for can never fail. An earlier version of this file declared
// secretA2APart and checked it without ever assigning it, which left A2A, Invocations and
// the identity fields unexercised — a record that shipped Invocation.Path would have passed.
// Keep the three lists in step: declare, assign, check.
const (
	secretPrompt      = "SECRET-PROMPT-TEXT"
	secretCompletion  = "SECRET-COMPLETION-TEXT"
	secretToolArgs    = "SECRET-TOOL-ARGUMENTS"
	secretToolResult  = "SECRET-TOOL-RESULT"
	secretToolName    = "SECRET-TOOL-NAME"
	secretHistoryArgs = "SECRET-HISTORY-TOOL-ARGUMENTS"
	secretMCPParams   = "SECRET-MCP-PARAMS"
	secretMCPResult   = "SECRET-MCP-RESULT"
	secretA2APart     = "SECRET-A2A-PART"
	secretA2AArtifact = "SECRET-A2A-ARTIFACT"
	secretA2AError    = "SECRET-A2A-ERROR-MESSAGE"
	secretPath        = "SECRET-PATH-TOKEN"
	secretInvPath     = "SECRET-INVOCATION-PATH"
	secretInvReason   = "SECRET-INVOCATION-REASON"
	secretInvDetail   = "SECRET-INVOCATION-DETAIL"
	secretPlugin      = "SECRET-PLUGIN-PAYLOAD"
	secretSPIFFE      = "SECRET-SPIFFE-ID"
	secretSubject     = "SECRET-SUBJECT"
	secretClientID    = "SECRET-CLIENT-ID"
	secretAgentID     = "SECRET-AGENT-ID"
	secretScope       = "SECRET-SCOPE"
	secretUserAgent   = "SECRET-USER-AGENT-TAIL"
	secretThinking    = "SECRET-THINKING-TYPE"
)

// allSecrets is every planted string. A record's encoding must contain none of them.
func allSecrets() []string {
	return []string{
		secretPrompt, secretCompletion, secretToolArgs, secretToolResult, secretToolName,
		secretHistoryArgs, secretMCPParams, secretMCPResult, secretA2APart, secretA2AArtifact,
		secretA2AError, secretPath, secretInvPath, secretInvReason, secretInvDetail,
		secretPlugin, secretSPIFFE, secretSubject, secretClientID, secretAgentID, secretScope,
		secretUserAgent, secretThinking,
	}
}

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
		// Name/Version stay recognisable so Client projects to a value at all; Raw carries the
		// planted secret, because Label() falls back to it when Name is empty and that is the
		// header-content leak this fixture has to be able to see.
		Client:   &model.EventClient{Name: "claude-code", Version: "2.1.270", Raw: "claude-code/2.1.270 " + secretUserAgent},
		Error:    &model.EventError{Kind: "upstream", Code: "429", Message: secretPrompt},
		Identity: &model.EventIdentity{Subject: secretSubject, ClientID: secretClientID, AgentID: secretAgentID, Scopes: []string{secretScope}},
		TLS:      &model.EventTLS{PeerSPIFFEID: secretSPIFFE},
		Plugins:  map[string]json.RawMessage{"p": json.RawMessage(`{"k":"` + secretPlugin + `"}`)},
		MCP: &model.MCPExtension{
			Method: "tools/call",
			Params: map[string]any{"a": secretMCPParams},
			Result: map[string]any{"b": secretMCPResult},
		},
		A2A: &model.A2AExtension{
			Method:       "message/stream",
			FinalStatus:  "failed",
			Parts:        []model.A2APart{{Kind: "text", Content: secretA2APart}},
			Artifact:     secretA2AArtifact,
			ErrorMessage: secretA2AError,
		},
		// Invocation.Path is the leak that matters most: it is back-filled from the same
		// Context.Path as HTTPPath, whose own doc warns that a path segment can hold a bot
		// token or a webhook secret. A record must carry neither copy.
		Invocations: &model.Invocations{
			Outbound: []model.Invocation{{
				Plugin:  "inference-parser",
				Action:  model.InvocationAction("observe"),
				Phase:   model.InvocationPhaseResponse,
				Path:    "/v1/messages/" + secretInvPath,
				Reason:  secretInvReason,
				Details: map[string]string{"k": secretInvDetail},
			}},
		},
		Inference: &model.InferenceExtension{
			Model:            "claude-opus-5",
			RequestedModel:   "claude-sonnet-5",
			ServedModel:      "claude-opus-5-20260101",
			Stream:           true,
			StreamedResponse: true,
			FinishReason:     "tool_use",
			AgentRole:        model.AgentRoleSubagent,
			InputTokens:      1000,
			CacheReadTokens:  300,
			CacheWriteTokens: 400,
			OutputTokens:     200,
			ReasoningTokens:  50,
			PresentKinds:     0b11111,
			Messages:         []model.InferenceMessage{{Role: "user", Content: secretPrompt}},
			Tools:            []model.InferenceTool{{Name: secretToolName, Description: secretPrompt}},
			ToolCalls:        []model.InferenceToolCall{{Name: secretToolName, Arguments: secretToolArgs}},
			ToolResults:      []model.InferenceToolResult{{Content: secretToolResult}},
			HistoryToolCalls: []model.InferenceToolCall{{Name: secretToolName, Arguments: secretHistoryArgs}},
			ThinkingType:     secretThinking,
			ThinkingDisplay:  secretThinking,
			HistoryThinking:  &model.InferenceThinking{Blocks: 3, TextBytes: 900},
			ResponseThinking: &model.InferenceThinking{Blocks: 1, TextBytes: 120},
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
	for _, secret := range allSecrets() {
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

// TestFromEvent_UnrecognisedClientCarriesNoHeader is the header-content regression. Label()
// falls back to the RAW User-Agent when Name is empty, so filtering only the literal
// "unknown" let an arbitrary header reach the export: a User-Agent of
// "claude-cli/private-data" became the label "claude-cli/private-data".
//
// Asserted on the ENCODED BYTES, not on r.Client, so it still holds if the projection later
// routes that string to some other field.
func TestFromEvent_UnrecognisedClientCarriesNoHeader(t *testing.T) {
	e := loadedEvent()
	e.Client = &model.EventClient{Raw: "claude-cli/" + secretUserAgent}

	if got := FromEvent(e, "s").Client; got != "" {
		t.Errorf("Client = %q, want empty — an unparsed User-Agent is not an agent name", got)
	}
	var buf bytes.Buffer
	if err := NewWriter(&buf).WriteEvent(e, "s"); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
	if strings.Contains(buf.String(), secretUserAgent) {
		t.Errorf("the raw User-Agent reached the export:\n%s", buf.String())
	}
}

// TestFromEvent_CountsFallBackToTheEventsOwn covers a projected copy: sessionapi's
// view=summary drops Messages, Tools and ToolCalls but keeps a count of each, so reading
// only len() reports zero for every row such a reader delivers.
func TestFromEvent_CountsFallBackToTheEventsOwn(t *testing.T) {
	e := loadedEvent()
	e.Inference.Messages, e.Inference.Tools, e.Inference.ToolCalls = nil, nil, nil
	e.Inference.MessageCount, e.Inference.ToolCount, e.Inference.ToolCallCount = 177, 11, 2

	r := FromEvent(e, "s")
	if r.MessageCount != 177 {
		t.Errorf("MessageCount = %d, want 177 from the event's own count", r.MessageCount)
	}
	if r.ToolCount != 11 {
		t.Errorf("ToolCount = %d, want 11 from the event's own count", r.ToolCount)
	}
	if r.ToolCallCount != 2 {
		t.Errorf("ToolCallCount = %d, want 2 from the event's own count", r.ToolCallCount)
	}
}

// TestFromEvent_SlicesWinOverTheCounts is the other direction of the same rule: when both
// are present the slice is the truth, since zero on a count means "not stated".
func TestFromEvent_SlicesWinOverTheCounts(t *testing.T) {
	e := loadedEvent()
	e.Inference.MessageCount, e.Inference.ToolCount, e.Inference.ToolCallCount = 999, 999, 999

	r := FromEvent(e, "s")
	if r.MessageCount != 1 || r.ToolCount != 1 || r.ToolCallCount != 1 {
		t.Errorf("counts = %d/%d/%d, want 1/1/1 from the slices the event still carries",
			r.MessageCount, r.ToolCount, r.ToolCallCount)
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

// TestRecord_TokensAreTheSplitNeverAnAggregate is the cost-correctness contract, run through
// the REAL producer: parsercommon.TokenUsage.Fill is what every inference parser publishes
// through, and it sets the legacy InferenceExtension.PromptTokens to Input+CacheRead+
// CacheWrite. Exporting that beside cacheReadTokens shipped a total next to its own
// components, so a consumer pricing input + cache counted the cached tokens twice.
//
// The figures are from a real archive row: 196,096 cached tokens is what the double-count
// was worth on one response.
func TestRecord_TokensAreTheSplitNeverAnAggregate(t *testing.T) {
	var inf model.InferenceExtension
	parsercommon.TokenUsage{
		Input:     1249,
		CacheRead: 196096,
		Output:    49,
		Present:   parsercommon.KindInput | parsercommon.KindCacheRead | parsercommon.KindOutput,
	}.Fill(&inf)

	// The aggregate the export must not carry, confirmed present on the event itself — so
	// this test fails if Fill ever stops deriving it and the premise goes stale.
	if want := 1249 + 196096; inf.PromptTokens != want {
		t.Fatalf("premise broken: inf.PromptTokens = %d, want the aggregate %d", inf.PromptTokens, want)
	}

	r := FromEvent(&model.SessionEvent{SessionID: "s", At: time.Now(), Inference: &inf}, "s")
	if r.InputTokens != 1249 {
		t.Errorf("InputTokens = %d, want 1249 — the UNCACHED prompt, which the ledger prices", r.InputTokens)
	}
	if r.CacheReadTokens != 196096 {
		t.Errorf("CacheReadTokens = %d, want 196096", r.CacheReadTokens)
	}
	if r.OutputTokens != 49 {
		t.Errorf("OutputTokens = %d, want 49", r.OutputTokens)
	}

	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"promptTokens", "completionTokens", "totalTokens"} {
		if bytes.Contains(b, []byte(`"`+key+`"`)) {
			t.Errorf("%s is an aggregate of fields this record already carries; exporting both "+
				"double-counts:\n%s", key, b)
		}
	}
	// The cached figure must appear exactly once across the whole record.
	if n := bytes.Count(b, []byte("196096")); n != 1 {
		t.Errorf("196096 appears %d times, want exactly 1 — a cached count on two keys is the "+
			"double-count:\n%s", n, b)
	}
}

// TestRecord_PresentKindsSurvives: a set bit with a zero count is "the provider reported
// zero", an unset bit is "not exposed". Collapsing them is what this field exists to stop,
// so it must reach the wire even when every count it describes is absent.
func TestRecord_PresentKindsSurvives(t *testing.T) {
	var inf model.InferenceExtension
	parsercommon.TokenUsage{Present: parsercommon.KindCacheWrite}.Fill(&inf)

	r := FromEvent(&model.SessionEvent{SessionID: "s", At: time.Now(), Inference: &inf}, "s")
	if r.PresentKinds != uint8(parsercommon.KindCacheWrite) {
		t.Fatalf("PresentKinds = %d, want %d", r.PresentKinds, uint8(parsercommon.KindCacheWrite))
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"presentKinds"`)) {
		t.Errorf("presentKinds absent, so a reported zero cannot be told from an unexposed "+
			"kind:\n%s", b)
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
	for _, key := range []string{
		"durationMs", "ttfbMs", "bytesUp", "bytesDown", "statusCode",
		"inputTokens", "cacheReadTokens", "outputTokens", "presentKinds",
	} {
		if bytes.Contains(b, []byte(`"`+key+`"`)) {
			t.Errorf("%s present on an unmeasured event; an absent figure must not render as 0:\n%s", key, b)
		}
	}
}
