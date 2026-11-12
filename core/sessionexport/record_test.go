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
	secretVersion     = "SECRET-CLIENT-VERSION"
	secretThinking    = "SECRET-THINKING-TYPE"
)

// allSecrets is every planted string. A record's encoding must contain none of them.
func allSecrets() []string {
	return []string{
		secretPrompt, secretCompletion, secretToolArgs, secretToolResult, secretToolName,
		secretHistoryArgs, secretMCPParams, secretMCPResult, secretA2APart, secretA2AArtifact,
		secretA2AError, secretPath, secretInvPath, secretInvReason, secretInvDetail,
		secretPlugin, secretSPIFFE, secretSubject, secretClientID, secretAgentID, secretScope,
		secretUserAgent, secretVersion, secretThinking,
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
		// Name/Version stay recognisable so Client projects to a value at all, while Raw and
		// the parsed Version both carry planted secrets — the two places the header's own text
		// reaches an EventClient, and so the two leaks this fixture has to be able to see.
		Client: &model.EventClient{
			Name:    "claude-code",
			Version: secretVersion,
			Raw:     "claude-code/2.1.270 " + secretUserAgent,
		},
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

// TestFromEvent_ClientCarriesNoHeaderContent is the header-content regression, and it drives
// the REAL parser rather than a hand-built EventClient: model.ParseUserAgent is what
// populates the field in production, and a fixture that sets Raw without Name describes a
// header no parser produces — so it could pass while the header still reached the file.
//
// Both halves of the label come from the header and each needed its own guard:
//
//   - an UNRECOGNISED product token leaves Name empty, so Label() answers the whole raw
//     header
//   - a RECOGNISED one ("claude-cli" is in knownClients) copies the rest of the token into
//     Version verbatim, so a name-only guard still exports "claude-code/<header text>"
//
// Asserted on the ENCODED BYTES as well as the field, so it still holds if the projection
// later routes that string somewhere else.
func TestFromEvent_ClientCarriesNoHeaderContent(t *testing.T) {
	for _, tc := range []struct {
		name string
		ua   string
		want string // the exported Client
	}{{
		name: "recognised product token, secret in the version",
		ua:   "claude-cli/" + secretUserAgent,
		want: "claude-code", // the name survives; the unparseable version does not
	}, {
		name: "unrecognised product token",
		ua:   "mystery-agent/" + secretUserAgent,
		want: "",
	}, {
		name: "secret as a trailing comment on a real version",
		ua:   "claude-cli/2.1.270 (" + secretUserAgent + ")",
		want: "claude-code/2.1.270", // the comment is not part of the product token
	}, {
		name: "no slash at all",
		ua:   secretUserAgent,
		want: "",
	}, {
		name: "a path-shaped version",
		ua:   "claude-cli//etc/" + secretUserAgent,
		want: "claude-code",
	}, {
		name: "a genuine version is kept",
		ua:   "claude-cli/2.1.270",
		want: "claude-code/2.1.270",
	}, {
		// The cases a digit-led charset let through. The first secret in this test only
		// dropped because it begins with a letter; put a digit in front and the old shape
		// shipped the lot.
		name: "digit-led free text",
		ua:   "claude-cli/1-" + secretUserAgent,
		want: "claude-code",
	}, {
		name: "digit-prefixed secret, no separator",
		ua:   "claude-cli/0" + secretUserAgent,
		want: "claude-code",
	}, {
		name: "a project name that starts with a digit",
		ua:   "claude-cli/1-acme-merger-q3-plan",
		want: "claude-code",
	}, {
		name: "a UUID",
		ua:   "claude-cli/3f2a9c1e-7b4d-4e2a-9f1c-1234567890ab",
		want: "claude-code",
	}, {
		name: "prose hung off a real version",
		ua:   "claude-cli/2.1.0-my.customer.list.foo",
		want: "claude-code",
	}, {
		// trailingKnownClient's path: the agent is the LAST token, so the version comes from
		// there and the same gate has to hold.
		name: "trailing known client",
		ua:   "mystery/1.0 claude-cli/9" + secretUserAgent,
		want: "claude-code",
	}, {
		// The second door: a free character class after the `+` is the same hole as a free
		// one after the digit, one character further along.
		name: "secret in the build tag",
		ua:   "claude-cli/1.0+" + secretUserAgent,
		want: "claude-code",
	}, {
		name: "a word in the build tag",
		ua:   "claude-cli/1.0+hunter2",
		want: "claude-code",
	}, {
		name: "a customer name in the build tag",
		ua:   "claude-cli/2.1.0+acmecorp1",
		want: "claude-code",
	}, {
		name: "build tag on the trailing-client path",
		ua:   "mystery/1.0 claude-cli/1.0+hunter2",
		want: "claude-code",
	}, {
		name: "an unbounded digit payload after a pre-release word",
		ua:   "claude-cli/2.1.0-rc.4111.1111.1111.1111.5555.6666",
		want: "claude-code",
	}, {
		// The one build tag that is real, and it reaches here through a live path:
		// bob-shell parses to Name "bob" with this version.
		name: "the observed build tag survives",
		ua:   "bob-shell/1.126.0+bob2.2.1",
		want: "bob/1.126.0+bob2.2.1",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			e := loadedEvent()
			e.Client = model.ParseUserAgent(tc.ua)

			if got := FromEvent(e, "s").Client; got != tc.want {
				t.Errorf("Client = %q, want %q (parsed name %q, version %q)",
					got, tc.want, e.Client.Name, e.Client.Version)
			}
			var buf bytes.Buffer
			if err := NewWriter(&buf).WriteEvent(e, "s"); err != nil {
				t.Fatalf("WriteEvent: %v", err)
			}
			if strings.Contains(buf.String(), secretUserAgent) {
				t.Errorf("User-Agent content reached the export:\n%s", buf.String())
			}
		})
	}
}

// TestClientLabel_VersionShape pins the shape rule directly, because the version is the half
// that is copied from the header and the boundary is where a rule like this goes wrong. Every
// "keep" below is a form this proxy has actually observed.
func TestClientLabel_VersionShape(t *testing.T) {
	for _, keep := range []string{"1", "2.1", "2.1.285", "2.1.0-beta.3", "8.4.0", "1.126.0+bob2.2.1"} {
		if got := clientLabel(&model.EventClient{Name: "claude-code", Version: keep}); got != "claude-code/"+keep {
			t.Errorf("version %q: Client = %q, want it kept — this is a real version form", keep, got)
		}
	}
	for _, drop := range []string{
		secretUserAgent, "private-data", "tok_abc123", "/etc/passwd", "v2.1.0", "2.1 270",
		"a.b.c", "", "../../x", "2.1.0;rm -rf /",
		// DIGIT-LED IS NOT VERSION-SHAPED. A charset that merely required a leading digit
		// admitted every one of these, which is why the rule is a grammar with a numeric core
		// and a closed list of pre-release words.
		"1-private-data", "0" + secretUserAgent, "1-acme-merger-q3-plan",
		"3f2a9c1e-7b4d-4e2a-9f1c-1234567890ab", "2.1.0-my.customer.list.foo",
		"1.2.3-internal-codename", "9SECRET-TAIL", "1234567", "1.2.3.4.5",
		// A FREE CHARACTER CLASS AFTER THE `+` IS THE SAME HOLE AS ONE AFTER THE DIGIT.
		// `\+[a-z]{0,8}\d…` admitted eight letters of whatever the header said, so the
		// bypass simply moved past the plus. Only the literal tag is allowed now.
		"1.0+hunter2", "2.1.0+acmecorp1", "1+password1", "2.1.0+customer1.2.3",
		"1.0+" + secretUserAgent, "1.0+acme", "2.1.0+bob", "1.0+BOB2.2.1",
		// An unbounded repeat is a payload channel even when every character is a digit.
		"2.1.0-rc.4111.1111.1111.1111.5555.6666", "1.0-beta.1.2.3.4.5.6.7.8",
	} {
		if got := clientLabel(&model.EventClient{Name: "claude-code", Version: drop}); got != "claude-code" {
			t.Errorf("version %q: Client = %q, want the bare name — a non-version must not be exported",
				drop, got)
		}
	}
	// An empty Name is absence whatever the version says, since Label() would answer the raw
	// header or the reserved "unknown".
	if got := clientLabel(&model.EventClient{Version: "2.1.285", Raw: "mystery/2.1.285"}); got != "" {
		t.Errorf("Client = %q, want empty for an unrecognised agent", got)
	}
	if got := clientLabel(nil); got != "" {
		t.Errorf("Client = %q, want empty for a nil client", got)
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

// TestWriter_SkipsNilEvents: FromEvent stays total, but a LINE for a nil event is a phantom
// row. At has no omitempty — an event always has a time — so the record read
// `{"schema":1,"sessionId":"","at":"0001-01-01T00:00:00Z"}`: a consumer bucketing by At gets
// an event in year 1, one counting rows per session gets a row belonging to none, and the
// sessionID the caller passed was dropped because FromEvent's nil branch returns before the
// fallback. The gap in an archive walk is the walk's, not an event that happened.
func TestWriter_SkipsNilEvents(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)

	if err := w.WriteEvent(nil, "sess-x"); err != nil {
		t.Fatalf("WriteEvent(nil) = %v, want nil — a nil element is not an error", err)
	}
	if buf.Len() != 0 {
		t.Errorf("a nil event wrote %q, want nothing", buf.String())
	}
	if w.Count() != 0 {
		t.Errorf("Count = %d, want 0 — a skipped event is not a written record", w.Count())
	}

	// A real event still writes, and the count is of records on the wire.
	if err := w.WriteEvent(loadedEvent(), "sess-x"); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
	if w.Count() != 1 {
		t.Errorf("Count = %d, want 1 after one real event", w.Count())
	}
	if n := strings.Count(strings.TrimRight(buf.String(), "\n"), "\n"); n != 0 {
		t.Errorf("got %d extra lines, want a single record:\n%s", n, buf.String())
	}
}

// TestFromEvent_AgentRoleIsAClosedVocabulary: the live producer answers only "main",
// "subagent" or "", but FromEvent's input is an archive walk — a DECODED event, where this
// field is whatever the JSON said. It is the one string in the record that is neither capped
// nor from a closed set, so it gets the closed set.
//
// Driven through json.Unmarshal rather than by assigning the field, because the decoded event
// IS the threat model: a fixture that sets it directly proves nothing about what a file can
// contain.
func TestFromEvent_AgentRoleIsAClosedVocabulary(t *testing.T) {
	for _, tc := range []struct {
		role string
		want string
	}{
		{"main", "main"},
		{"subagent", "subagent"},
		{"", ""},
		{"Main", ""}, // the vocabulary is exact, not case-folded
		{"orchestrator", ""},
		{strings.Repeat("r", 130) + "\x1b[31m", ""},
		{secretUserAgent, ""},
	} {
		payload, err := json.Marshal(map[string]any{
			"SessionID": "s",
			"At":        "2026-11-12T00:00:00Z",
			"Inference": map[string]any{"agentRole": tc.role},
		})
		if err != nil {
			t.Fatal(err)
		}
		var e model.SessionEvent
		if err := json.Unmarshal(payload, &e); err != nil {
			t.Fatalf("unmarshal %q: %v", tc.role, err)
		}
		if got := FromEvent(&e, "s").AgentRole; got != tc.want {
			t.Errorf("agentRole %q decoded from an archive exported as %q, want %q",
				tc.role, got, tc.want)
		}
	}
}

// TestRecord_WireStringsAreBounded: "a record is ~30 small scalars" is a claim about the
// file, and Model/RequestedModel/ServedModel/FinishReason come off the wire with no cap of
// their own — a 5KB `model` in a request body exported as 5KB before this bound. Host and
// the error fields are bounded on the same grounds.
func TestRecord_WireStringsAreBounded(t *testing.T) {
	long := strings.Repeat("m", 5014)
	e := &model.SessionEvent{
		SessionID:     "s",
		At:            time.Now(),
		Host:          long,
		RequestedHost: long,
		HTTPMethod:    long,
		Error:         &model.EventError{Kind: long, Code: long},
		Inference: &model.InferenceExtension{
			Model: long, RequestedModel: long, ServedModel: long, FinishReason: long,
		},
	}
	r := FromEvent(e, "s")
	for _, f := range []struct {
		name string
		got  string
	}{
		{"Model", r.Model}, {"RequestedModel", r.RequestedModel}, {"ServedModel", r.ServedModel},
		{"FinishReason", r.FinishReason}, {"Host", r.Host}, {"RequestedHost", r.RequestedHost},
		{"HTTPMethod", r.HTTPMethod}, {"ErrorKind", r.ErrorKind}, {"ErrorCode", r.ErrorCode},
	} {
		if len(f.got) > maxLabelLen {
			t.Errorf("%s is %d bytes, want <= %d — an unbounded wire string reached the export",
				f.name, len(f.got), maxLabelLen)
		}
	}

	// Control runes are stripped before the cut, so no row can carry something a terminal
	// would act on, and the one-line property does not depend on the encoder alone.
	e.Inference.Model = "claude\x00opus\x1b[31m"
	if got := FromEvent(e, "s").Model; strings.ContainsAny(got, "\x00\x1b") {
		t.Errorf("Model = %q, want control runes removed", got)
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
