package sessionapi

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
)

// fullEvent is an event carrying every content-bearing field the projection is
// meant to drop, alongside every field the timeline renders.
func fullEvent() pipeline.SessionEvent {
	temp := 0.7
	return pipeline.SessionEvent{
		SessionID:  "s1",
		Seq:        42,
		At:         time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC),
		Direction:  pipeline.Outbound,
		Phase:      pipeline.SessionResponse,
		RequestID:  "req-1",
		Host:       "api.example.com",
		StatusCode: 200,
		Duration:   1200 * time.Millisecond,
		Identity:   &pipeline.EventIdentity{},
		Invocations: &pipeline.Invocations{
			Outbound: []pipeline.Invocation{{Plugin: "token-exchange", Action: "modify", Reason: "exchanged"}},
		},
		Plugins: map[string]json.RawMessage{
			"cost": json.RawMessage(`{"cost_usd":0.1}`),
		},
		Inference: &pipeline.InferenceExtension{
			Model:       "opus",
			Temperature: &temp,
			Stream:      true,
			// Proportioned like real traffic, which is what the whole design rests
			// on: an inference request re-sends the WHOLE conversation, so Messages
			// is ~190KB while the completion it produced is a couple of KB. A
			// fixture with those two the same size makes the projection look 3x
			// rather than the ~163x it measures against a live proxy.
			Messages:         bigConversation(24, 8192),
			Tools:            []pipeline.InferenceTool{{Name: "search"}},
			AgentRole:        pipeline.AgentRoleMain,
			Completion:       strings.Repeat("y", 2048),
			ToolCalls:        []pipeline.InferenceToolCall{{Name: "search"}},
			PromptTokens:     100,
			CompletionTokens: 50,
			TotalTokens:      150,
			InputTokens:      90,
			CacheReadTokens:  10,
			OutputTokens:     50,
			PresentKinds:     9,
			FinishReason:     "stop",
		},
		A2A: &pipeline.A2AExtension{
			Method:       "message/send",
			Role:         "user",
			IsAction:     true,
			Parts:        []pipeline.A2APart{{Kind: "text", Content: strings.Repeat("z", 1024)}},
			Artifact:     strings.Repeat("a", 512),
			FinalStatus:  "completed",
			ErrorMessage: "none",
		},
		MCP: &pipeline.MCPExtension{
			Method:   "tools/call",
			IsAction: true,
			Params:   map[string]any{"big": strings.Repeat("p", 1024)},
			Result:   map[string]any{"big": strings.Repeat("r", 1024)},
			Err:      &pipeline.MCPError{Code: -1, Message: "boom"},
		},
	}
}

// The projection has one job: drop the payloads the timeline never renders while
// keeping every field it does. Asserted field by field, because "it got smaller"
// would also be true of a projection that dropped something the table needs.
func TestSummarizeEvent_DropsPayloadsKeepsTimelineFields(t *testing.T) {
	full := fullEvent()
	got := summarizeEvent(&full)

	// Dropped: the content-bearing fields, which live only in the detail pane.
	if got.Inference.Messages != nil {
		t.Error("Inference.Messages survived")
	}
	if got.Inference.Tools != nil {
		t.Error("Inference.Tools survived")
	}
	if got.Inference.ToolCalls != nil {
		t.Error("Inference.ToolCalls survived")
	}
	// …but their LENGTHS are stated in their place. A third category beside dropped and kept:
	// derived from a payload that goes, and the only thing left that says what it was.
	if got.Inference.MessageCount != len(full.Inference.Messages) {
		t.Errorf("messageCount = %d, want %d", got.Inference.MessageCount,
			len(full.Inference.Messages))
	}
	if got.Inference.ToolCount != len(full.Inference.Tools) {
		t.Errorf("toolCount = %d, want %d", got.Inference.ToolCount, len(full.Inference.Tools))
	}
	// agentRole needs no such rescue — it is a scalar the struct copy carries — but it is read by
	// the same consumer for the same question, so a projection that lost it would break the
	// CONTEXT gauge exactly as dropping the counts did.
	if got.Inference.AgentRole != full.Inference.AgentRole {
		t.Errorf("agentRole = %q, want %q", got.Inference.AgentRole, full.Inference.AgentRole)
	}
	if got.A2A.Artifact != "" {
		t.Error("A2A.Artifact survived")
	}
	if got.MCP.Params != nil {
		t.Error("MCP.Params survived")
	}
	if got.MCP.Result != nil {
		t.Error("MCP.Result survived")
	}

	// Kept: everything the events table and its sort keys read.
	if got.Seq != full.Seq || got.At != full.At || got.Host != full.Host {
		t.Error("identity/time/host fields did not survive")
	}
	if got.Direction != full.Direction || got.Phase != full.Phase {
		t.Error("direction/phase did not survive")
	}
	if got.StatusCode != 200 || got.Duration != 1200*time.Millisecond {
		t.Error("status/duration did not survive — STATUS and DURATION columns")
	}
	// Token counts drive the TOKENS and COST columns, and cost is derived from the
	// cache-read/write split rather than the total — so each one is checked.
	for _, tc := range []struct {
		name      string
		got, want int
	}{
		{"promptTokens", got.Inference.PromptTokens, full.Inference.PromptTokens},
		{"completionTokens", got.Inference.CompletionTokens, full.Inference.CompletionTokens},
		{"totalTokens", got.Inference.TotalTokens, full.Inference.TotalTokens},
		{"inputTokens", got.Inference.InputTokens, full.Inference.InputTokens},
		{"cacheReadTokens", got.Inference.CacheReadTokens, full.Inference.CacheReadTokens},
		{"outputTokens", got.Inference.OutputTokens, full.Inference.OutputTokens},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d — TOKENS/COST columns read it", tc.name, tc.got, tc.want)
		}
	}
	if got.Inference.PresentKinds != full.Inference.PresentKinds {
		t.Error("presentKinds did not survive — it says which counts are real")
	}
	if got.Inference.Model != "opus" || got.Inference.FinishReason != "stop" {
		t.Error("model/finishReason did not survive")
	}
	// Invocations is timeline data, not detail data: abctl renders one row per
	// invocation, and the ACTION column comes from it.
	if got.Invocations == nil || len(got.Invocations.Outbound) != 1 {
		t.Fatal("Invocations did not survive — the timeline renders a row per invocation")
	}
	if got.Invocations.Outbound[0].Plugin != "token-exchange" {
		t.Error("invocation contents did not survive")
	}
	// Protocol identity and the action/bypass classification drive what the row
	// says it is; errors are the thing you scan a timeline for.
	if got.A2A.Method != "message/send" || !got.A2A.IsAction || got.A2A.FinalStatus != "completed" {
		t.Error("A2A timeline fields did not survive")
	}
	if got.A2A.ErrorMessage != "none" {
		t.Error("A2A.ErrorMessage did not survive")
	}
	if got.MCP.Method != "tools/call" || !got.MCP.IsAction {
		t.Error("MCP timeline fields did not survive")
	}
	if got.MCP.Err == nil || got.MCP.Err.Message != "boom" {
		t.Error("MCP error did not survive")
	}
}

// The store hands out pointers to events it keeps. A projection that cleared
// fields in place would delete the operator's conversation from the store —
// permanently, and only for sessions someone happened to open.
func TestSummarizeEvent_DoesNotMutateTheStoredEvent(t *testing.T) {
	full := fullEvent()
	_ = summarizeEvent(&full)

	if len(full.Inference.Messages) != 24 {
		t.Error("Inference.Messages was cleared on the source event")
	}
	if full.Inference.Completion == "" {
		t.Error("Inference.Completion was cleared on the source event")
	}
	if len(full.A2A.Parts) != 1 || full.A2A.Artifact == "" {
		t.Error("A2A payload was cleared on the source event")
	}
	if full.MCP.Params == nil || full.MCP.Result == nil {
		t.Error("MCP payload was cleared on the source event")
	}
	if full.Plugins == nil {
		t.Error("Plugins was cleared on the source event")
	}
}

// A nil extension must stay nil rather than become an empty object, or every row
// would claim to carry a protocol it does not.
func TestSummarizeEvent_LeavesAbsentExtensionsAbsent(t *testing.T) {
	bare := pipeline.SessionEvent{Seq: 1, Host: "h"}
	got := summarizeEvent(&bare)
	if got.Inference != nil || got.A2A != nil || got.MCP != nil {
		t.Error("an absent extension was materialised by the projection")
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, absent := range []string{`"inference"`, `"a2a"`, `"mcp"`} {
		if strings.Contains(string(b), absent) {
			t.Errorf("%s appears in the encoding of an event that had none: %s", absent, b)
		}
	}
}

// The size claim this whole change rests on. Measured at 163x against a live
// proxy's real events, with the filter's fields kept (299x if they were dropped,
// which broke the filter — see TestSummarizeEvent_KeepsEverythingTheFilterSearches).
// Asserted well under that, so this pins the order of magnitude rather than one
// machine's sample.
func TestSummarizeEvent_IsOrdersOfMagnitudeSmaller(t *testing.T) {
	full := fullEvent()
	fb, err := json.Marshal(&full)
	if err != nil {
		t.Fatalf("marshal full: %v", err)
	}
	sb, err := json.Marshal(summarizeEvent(&full))
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	ratio := float64(len(fb)) / float64(len(sb))
	t.Logf("full=%d summary=%d ratio=%.1fx", len(fb), len(sb), ratio)
	// 40x against 52.4x on this fixture (163x on a live proxy's real events, whose conversations
	// are far longer than 24 messages). The floor was 10, which would have passed a 5x
	// regression — and this projection just grew two fields, so the margin is where a floor
	// earns its keep. Deterministic: one fixture, one encoder, no sampling.
	if ratio < 40 {
		t.Errorf("summary is only %.1fx smaller (full=%d summary=%d); the payloads are the point",
			ratio, len(fb), len(sb))
	}
}

// The half that was missed the first time, and silently: the events table has a
// FILTER, and it searches fields the projection had been dropping. `plugin:foo`
// matched nothing because it is a lookup into Plugins, and `/some completion text`
// stopped matching because eventHaystack reads Inference.Completion and
// A2A.Parts[].Content.
//
// Worse than losing a feature, it was INCONSISTENT: SSE-streamed events are not
// projected, so the same query matched newly arrived rows and missed everything a
// snapshot had replaced.
//
// Each field below is named by abctl's eventHaystack / matchEventRow
// (cmd/abctl/tui/events_pane.go). Keeping them costs 299x → 163x, which is 1.12 MiB
// against 0.61 for a 1000-event session — both instant.
func TestSummarizeEvent_KeepsEverythingTheFilterSearches(t *testing.T) {
	full := fullEvent()
	got := summarizeEvent(&full)

	// `plugin:<name>` is a lookup into this map.
	if _, ok := got.Plugins["cost"]; !ok {
		t.Error("Plugins was dropped — the plugin:<name> filter is a lookup into it")
	}
	// Free-text: eventHaystack appends both of these.
	if got.Inference.Completion != full.Inference.Completion {
		t.Error("Inference.Completion was dropped — the / filter searches it")
	}
	if len(got.A2A.Parts) != 1 || got.A2A.Parts[0].Content != full.A2A.Parts[0].Content {
		t.Error("A2A.Parts was dropped — the / filter searches Parts[].Content")
	}
	// Also in the haystack, and already kept for other reasons — asserted here so
	// this test is the one place the filter's whole surface is listed.
	if got.MCP.Err == nil || got.MCP.Err.Message != "boom" {
		t.Error("MCP.Err.Message was dropped — the / filter searches it")
	}
	if got.Inference.FinishReason != "stop" {
		t.Error("Inference.FinishReason was dropped — the / filter searches it")
	}
	if got.Identity == nil {
		t.Error("Identity was dropped — the / filter searches Subject and ClientID")
	}
	if got.Invocations == nil {
		t.Error("Invocations was dropped — the / filter searches plugin/action/reason/path/Details")
	}
}

// An exhaustiveness guard, because every check above is hand-enumerated: a large
// field added to any of these structs later would pass straight through and
// re-inflate the timeline with every other test still green.
//
// Counting fields rather than naming them is deliberate. The point is to FAIL when
// the shape changes, so that whoever adds a field has to decide whether it is
// timeline data or detail data and record the answer here. The numbers carry no
// meaning on their own; the failure message carries the instruction.
func TestSummarizeEvent_ShapeIsGuarded(t *testing.T) {
	for _, g := range []struct {
		name string
		typ  reflect.Type
		want int
	}{
		{"SessionEvent", reflect.TypeOf(pipeline.SessionEvent{}), 22},
		// 25 since AgentRole, which is TIMELINE data: abctl's CONTEXT gauge reads it on every
		// row the timeline serves. It needs no assertion of its own in the projection test
		// beyond the equality one there — a scalar survives the struct copy, unlike the two
		// slices whose lengths had to be recorded before they were dropped.
		{"InferenceExtension", reflect.TypeOf(pipeline.InferenceExtension{}), 25},
		{"A2AExtension", reflect.TypeOf(pipeline.A2AExtension{}), 11},
		{"MCPExtension", reflect.TypeOf(pipeline.MCPExtension{}), 6},
	} {
		t.Run(g.name, func(t *testing.T) {
			if got := g.typ.NumField(); got != g.want {
				t.Errorf("%s has %d fields, this guard was written against %d.\n"+
					"A field was added or removed. Decide which it is:\n"+
					"  TIMELINE data — rendered in the events table, or searched by abctl's\n"+
					"    eventHaystack/matchEventRow — then it MUST survive summarizeEvent, and\n"+
					"    belongs in one of the assertions above.\n"+
					"  DETAIL data — read only by the detail pane — then drop it in summarizeEvent.\n"+
					"Then update this count.", g.name, got, g.want)
			}
		})
	}
}

// bigConversation builds n messages of size bytes each — the shape of a real
// inference request, which carries every earlier turn.
func bigConversation(n, size int) []pipeline.InferenceMessage {
	msgs := make([]pipeline.InferenceMessage, 0, n)
	for i := 0; i < n; i++ {
		msgs = append(msgs, pipeline.InferenceMessage{Role: "user", Content: strings.Repeat("x", size)})
	}
	return msgs
}

// THE COUNTS ARE THE ONLY THING LEFT THAT DESCRIBES THE CONVERSATION, so they are pinned on their
// own rather than only inside the big projection test.
//
// abctl's CONTEXT gauge asks two questions of a response — did the request carry a tool manifest,
// and how long is the conversation — to tell an agentic turn from the one-shot completions Claude
// Code interleaves under the same session id. It asked them of Messages and Tools, which this
// projection drops, so the column read a dash for every row the timeline served while the
// unprojected SSE stream kept working. Measured on one live session: 41 of 62 inference responses
// carry a manifest unprojected, 0 of 62 projected.
//
// ZERO MEANS "NOT STATED" for a reader, which is why the no-inference and empty cases below assert
// zero and the populated one asserts an exact count: a consumer prefers len() and falls back to
// these, so a wrong non-zero here would be believed.
func TestSummarizeEvent_CountsTheConversationItDrops(t *testing.T) {
	t.Run("a conversation with a manifest", func(t *testing.T) {
		full := fullEvent()
		full.Inference.Messages = bigConversation(31, 8)
		full.Inference.Tools = make([]pipeline.InferenceTool, 27)
		got := summarizeEvent(&full)
		if got.Inference.MessageCount != 31 || got.Inference.ToolCount != 27 {
			t.Errorf("counts = %d msgs / %d tools, want 31 / 27",
				got.Inference.MessageCount, got.Inference.ToolCount)
		}
		if got.Inference.Messages != nil || got.Inference.Tools != nil {
			t.Error("the payloads survived; counting them is not a reason to keep them")
		}
	})

	// A one-shot completion: messages but no manifest — the distinction the gauge turns on.
	//
	// IN PROCESS the zero is stated; ON THE WIRE it is indistinguishable from an old proxy that
	// said nothing, because omitempty drops it (the subtest below asserts that, deliberately).
	// That costs nothing here and the reason is worth knowing: a reader treats "not stated" as
	// "no manifest I can see" and skips the event either way. Only a NON-ZERO count changes an
	// answer, so only a non-zero count has to survive the encoder.
	t.Run("a one-shot with no manifest", func(t *testing.T) {
		full := fullEvent()
		full.Inference.Messages = bigConversation(3, 8)
		full.Inference.Tools = nil
		got := summarizeEvent(&full)
		if got.Inference.MessageCount != 3 || got.Inference.ToolCount != 0 {
			t.Errorf("counts = %d msgs / %d tools, want 3 / 0",
				got.Inference.MessageCount, got.Inference.ToolCount)
		}
	})

	// omitempty on both, so a projected event that carried neither costs no bytes — the whole
	// point of the projection.
	t.Run("nothing to count is nothing on the wire", func(t *testing.T) {
		full := fullEvent()
		full.Inference.Messages, full.Inference.Tools = nil, nil
		b, err := json.Marshal(summarizeEvent(&full).Inference)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "messageCount") || strings.Contains(string(b), "toolCount") {
			t.Errorf("zero counts reached the wire: %s", b)
		}
	})

	// The source event must not gain them either: the store hands out pointers to what it keeps.
	t.Run("the stored event is untouched", func(t *testing.T) {
		full := fullEvent()
		_ = summarizeEvent(&full)
		if full.Inference.MessageCount != 0 || full.Inference.ToolCount != 0 {
			t.Error("summarizeEvent wrote the counts back onto the stored event")
		}
	})
}
