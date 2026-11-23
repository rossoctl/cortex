package sessionapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/session/archive"
	"github.com/rossoctl/cortex/core/sessionexport"
)

// tcSecret is planted in every content field the PAIRING reads, not only in tool arguments:
// toolpair's closing cases scan message text for task notifications and result text for
// background-job acknowledgements, so a fixture that planted only in arguments could pass while
// a closing case leaked the text it matched on (spec T9).
const (
	tcSecretPrompt = "SECRET-HANDLER-PROMPT"
	tcSecretArgs   = "SECRET-HANDLER-ARGS"
	tcSecretResult = "SECRET-HANDLER-RESULT"
	tcToolUseID    = "toolu_SECRET_HANDLER_USE_ID"
)

// toolCallFixture serves an archive whose session pairs into tool calls: two turns, the second
// carrying the first's result, plus one MCP-named call so the hashing path is exercised at the
// HTTP surface rather than only in the projection's own tests.
func toolCallFixture(t *testing.T) *httptest.Server {
	t.Helper()
	root := t.TempDir()
	write, err := archive.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	now := exportBase
	store := session.New(0, 0, 0, session.WithClock(func() time.Time { return now }))
	store.AddRecorder(write)
	client := &model.EventClient{Name: "claude-code", Version: "2.1.270"}
	add := func(e model.SessionEvent) {
		now = e.At
		store.Append("tc", e)
	}
	for turn := range 2 {
		at := exportBase.Add(time.Duration(turn) * time.Minute)
		var results []model.InferenceToolResult
		if turn > 0 {
			results = []model.InferenceToolResult{
				{ToolUseID: tcToolUseID, Content: tcSecretResult},
				{ToolUseID: tcToolUseID + "-mcp", Content: tcSecretResult},
			}
		}
		add(model.SessionEvent{
			At: at, Direction: model.Outbound, Phase: model.SessionRequest,
			RequestID: "r" + string(rune('0'+turn)), Client: client, HTTPMethod: "POST",
			Inference: &model.InferenceExtension{
				Model: "m", AgentRole: model.AgentRoleMain,
				Messages:    []model.InferenceMessage{{Role: "user", Content: tcSecretPrompt}},
				Tools:       []model.InferenceTool{{Name: "Bash"}},
				ToolResults: results,
			},
		})
		if turn == 0 {
			add(model.SessionEvent{
				At: at.Add(time.Second), Direction: model.Outbound, Phase: model.SessionResponse,
				RequestID: "r0", Client: client, Duration: 2 * time.Second,
				Inference: &model.InferenceExtension{
					Model: "m", AgentRole: model.AgentRoleMain,
					ToolCalls: []model.InferenceToolCall{
						{ID: tcToolUseID, Name: "Bash", Arguments: tcSecretArgs},
						{ID: tcToolUseID + "-mcp", Name: "mcp__acme-billing-prod__refund",
							Arguments: tcSecretArgs},
					},
				},
			})
		}
	}
	store.Close()
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}
	a, err := archive.Open(root, archive.WithClock(func() time.Time { return exportNow }))
	if err != nil {
		t.Fatal(err)
	}
	srv := New(":0", session.New(0, 0, 0), WithArchive(a),
		WithClock(func() time.Time { return exportNow }))
	ts := httptest.NewServer(srv.server.Handler)
	t.Cleanup(func() {
		ts.Close()
		a.Close()
	})
	return ts
}

// kindsOf counts the record kinds in an NDJSON body.
func kindsOf(t *testing.T, body string) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line is not JSON: %q", line)
		}
		k, _ := m["kind"].(string)
		out[k]++
	}
	return out
}

// TestHandleExport_PlainStreamHasNoKind is T20 at the HTTP surface: without the opt-in this
// endpoint's output is byte-identical to the one that shipped before toolCall records existed.
//
// The projection's own TestWriter_PlainStreamUnchanged covers the Writer; this covers the
// HANDLER, which is where a default could flip without the Writer changing at all.
func TestHandleExport_PlainStreamHasNoKind(t *testing.T) {
	ts := toolCallFixture(t)
	code, body, _ := exportRecords(t, ts, "/v1/export")
	if code != http.StatusOK {
		t.Fatalf("GET /v1/export = %d %s", code, body)
	}
	if strings.Contains(body, `"kind"`) {
		t.Errorf("a plain export carries a kind key:\n%s", body)
	}
	// Matched as the KIND VALUE, not as a bare substring: the event record's own
	// `toolCallCount` field contains "toolCall", so a substring test passes only by accident of
	// which fields the fixture happens to populate.
	if strings.Contains(body, `"kind":"`+sessionexport.ToolCallKind+`"`) {
		t.Errorf("a plain export carries toolCall records:\n%s", body)
	}
	if n := kindsOf(t, body)[sessionexport.ToolCallKind]; n != 0 {
		t.Errorf("%d toolCall records on a plain export, want 0", n)
	}
}

// TestHandleExport_IncludeToolCallsAddsRecords is the opt-in's happy path: both kinds on one
// stream, each labelled, with the event records still present — the opt-in ADDS, it does not
// replace.
func TestHandleExport_IncludeToolCallsAddsRecords(t *testing.T) {
	ts := toolCallFixture(t)
	resp, body := getPath(t, ts.URL, "/v1/export?include=toolCalls")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d %s", resp.StatusCode, body)
	}
	kinds := kindsOf(t, body)
	if kinds[sessionexport.EventKind] == 0 {
		t.Error("no event records: the opt-in replaced the stream instead of adding to it")
	}
	if kinds[sessionexport.ToolCallKind] != 2 {
		t.Errorf("%d toolCall records, want the fixture's 2: %v", kinds[sessionexport.ToolCallKind], kinds)
	}
	if kinds[""] != 0 {
		t.Errorf("%d records carry no kind on an opted-in stream (T28)", kinds[""])
	}
}

// TestHandleExport_ToolCallsCarryNoContent is the privacy contract at the HTTP surface — the
// layer that actually leaves the machine. Every planted secret must be absent, and so must the
// tool-use id (T8) and the MCP server name (T11).
func TestHandleExport_ToolCallsCarryNoContent(t *testing.T) {
	ts := toolCallFixture(t)
	resp, body := getPath(t, ts.URL, "/v1/export?include=toolCalls")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d %s", resp.StatusCode, body)
	}
	for _, secret := range []string{tcSecretPrompt, tcSecretArgs, tcSecretResult} {
		if strings.Contains(body, secret) {
			t.Errorf("export leaked %q:\n%s", secret, body)
		}
	}
	// T8: the pairing key must not reach the wire.
	if strings.Contains(body, tcToolUseID) {
		t.Errorf("export leaked the tool-use id — records are joinable to a conversation:\n%s", body)
	}
	// T11: the MCP namespace carries a vendor relationship.
	for _, frag := range []string{"acme", "billing", "refund"} {
		if strings.Contains(body, frag) {
			t.Errorf("export leaked MCP name fragment %q in clear:\n%s", frag, body)
		}
	}
	// And the hashed label IS present, so the above is not passing because nothing was exported.
	if !strings.Contains(body, "hmac-sha256:") {
		t.Error("no hashed tool label in the body: the MCP call was not exported at all")
	}
}

// TestHandleExport_IncludeIsAClosedVocabulary: an unrecognised value is a 400, not a silent
// ignore. Answering a typo with the plain stream makes the feature look broken, and the caller
// never learns why.
func TestHandleExport_IncludeIsAClosedVocabulary(t *testing.T) {
	ts := toolCallFixture(t)
	for _, tc := range []struct {
		query string
		want  int
	}{
		{"include=toolCalls", http.StatusOK},
		{"include=", http.StatusOK},                  // a trailing separator reads as absent
		{"include=toolcalls", http.StatusBadRequest}, // the case typo
		{"include=toolCall", http.StatusBadRequest},  // the singular typo
		{"include=title", http.StatusBadRequest},     // #1906 §1, not implemented here
		{"include=toolCalls&include=toolCalls", http.StatusOK},
		{"include=toolCalls&include=bogus", http.StatusBadRequest},
	} {
		resp, body := getPath(t, ts.URL, "/v1/export?"+tc.query)
		if resp.StatusCode != tc.want {
			t.Errorf("?%s = %d, want %d: %s", tc.query, resp.StatusCode, tc.want, body)
		}
		if tc.want == http.StatusBadRequest {
			if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
				t.Errorf("?%s refusal labelled %q, want JSON", tc.query, ct)
			}
		}
	}
}

// TestHandleExport_IncludeComposesWithTheOtherFilters: the opt-in is orthogonal to session and
// since, so a caller can narrow and opt in at once.
func TestHandleExport_IncludeComposesWithTheOtherFilters(t *testing.T) {
	ts := toolCallFixture(t)
	resp, body := getPath(t, ts.URL, "/v1/export?include=toolCalls&session=tc")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d %s", resp.StatusCode, body)
	}
	if n := kindsOf(t, body)[sessionexport.ToolCallKind]; n != 2 {
		t.Errorf("%d toolCall records with ?session=tc, want 2", n)
	}
	// An unknown session is still a 400, and still before the first byte.
	resp, body = getPath(t, ts.URL, "/v1/export?include=toolCalls&session=nope")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown session with include = %d, want 400: %s", resp.StatusCode, body)
	}
}
