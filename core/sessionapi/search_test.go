package sessionapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/session/archive"
)

// search asks base for id's events that match q, and returns the answer, the status and the
// body.
func search(t *testing.T, base, id, q string) (searchResponse, int, string) {
	t.Helper()
	resp, err := http.Get(base + "/v1/sessions/" + url.PathEscape(id) + "/search?q=" + url.QueryEscape(q))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out searchResponse
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
	}
	return out, resp.StatusCode, string(body)
}

// seqsFor is the seqs search answers, failing the test on any status but 200.
func seqsFor(t *testing.T, base, id, q string) []uint64 {
	t.Helper()
	out, code, body := search(t, base, id, q)
	if code != http.StatusOK {
		t.Fatalf("%q: status %d, body %s", q, code, body)
	}
	return out.Seqs
}

func req(inf *pipeline.InferenceExtension) pipeline.SessionEvent {
	return pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest, Inference: inf}
}

func resp(inf *pipeline.InferenceExtension) pipeline.SessionEvent {
	return pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionResponse, Inference: inf}
}

// A response event carries its request's messages, and the detail view hides them there.
func TestSearch_PromptMatchesTheRequestNotTheResponse(t *testing.T) {
	ts, store := newTestServer(t)
	inf := &pipeline.InferenceExtension{Model: "m",
		Messages: []pipeline.InferenceMessage{{Role: "user", Content: "find the NEEDLE-PROMPT"}}}
	store.Append("s1", req(inf))
	store.Append("s1", resp(inf))
	if got := seqsFor(t, ts.URL, "s1", "needle-prompt"); !slices.Equal(got, []uint64{1}) {
		t.Errorf("seqs %v, want [1]", got)
	}
}

func TestSearch_ToolCallsMatchTheResponseNotTheRequest(t *testing.T) {
	ts, store := newTestServer(t)
	inf := &pipeline.InferenceExtension{Model: "m",
		ToolCalls: []pipeline.InferenceToolCall{{Name: "bash", Arguments: `{"cmd":"NEEDLE-ARGS"}`}}}
	store.Append("s1", req(inf))
	store.Append("s1", resp(inf))
	if got := seqsFor(t, ts.URL, "s1", "needle-args"); !slices.Equal(got, []uint64{2}) {
		t.Errorf("seqs %v, want [2]", got)
	}
}

// The detail tree shows key names, so they match where the field is present.
func TestSearch_KeyNames(t *testing.T) {
	ts, store := newTestServer(t)
	store.Append("s1", req(&pipeline.InferenceExtension{Model: "m",
		Messages: []pipeline.InferenceMessage{{Role: "user", Content: "a", ToolUseID: "t1"}}}))
	store.Append("s1", req(&pipeline.InferenceExtension{Model: "m",
		Messages: []pipeline.InferenceMessage{{Role: "user", Content: "b"}}}))
	if got := seqsFor(t, ts.URL, "s1", "toolUseId"); !slices.Equal(got, []uint64{1}) {
		t.Errorf("seqs %v, want [1]", got)
	}
}

func TestSearch_ToolParameters(t *testing.T) {
	ts, store := newTestServer(t)
	store.Append("s1", req(&pipeline.InferenceExtension{Model: "m", Tools: []pipeline.InferenceTool{{
		Name: "t", Parameters: `{"type":"object","properties":{"needlefield":{"type":"string"}}}`}}}))
	if got := seqsFor(t, ts.URL, "s1", "needlefield"); !slices.Equal(got, []uint64{1}) {
		t.Errorf("seqs %v, want [1]", got)
	}
}

func TestSearch_MCPAndA2A(t *testing.T) {
	ts, store := newTestServer(t)
	mcp := &pipeline.MCPExtension{Method: "tools/call",
		Params: map[string]any{"path": "/needle-p"}, Result: map[string]any{"text": "needle-r"}}
	store.Append("s1", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionRequest, MCP: mcp})
	store.Append("s1", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionResponse, MCP: mcp})
	store.Append("s1", pipeline.SessionEvent{At: time.Now(), Phase: pipeline.SessionResponse,
		A2A: &pipeline.A2AExtension{Artifact: "the needle-a artifact"}})
	for q, want := range map[string][]uint64{"needle-p": {1}, "needle-r": {2}, "needle-a": {3}} {
		if got := seqsFor(t, ts.URL, "s1", q); !slices.Equal(got, want) {
			t.Errorf("%q: seqs %v, want %v", q, got, want)
		}
	}
}

// The fold is agentop's find.Contains fold: rune by rune, with unicode.ToLower.
func TestSearch_CaseFold(t *testing.T) {
	ts, store := newTestServer(t)
	store.Append("s1", req(&pipeline.InferenceExtension{Model: "m",
		Messages: []pipeline.InferenceMessage{{Role: "user", Content: "İSTANBUL and straße"}}}))
	for _, q := range []string{"istanbul", "STRAßE"} {
		if got := seqsFor(t, ts.URL, "s1", q); !slices.Equal(got, []uint64{1}) {
			t.Errorf("%q: seqs %v, want [1]", q, got)
		}
	}
}

// archived starts a server whose store records into an archive, appends each event with its
// capture, and flushes the archive by closing it. The store keeps what it holds.
func archived(t *testing.T, events []pipeline.SessionEvent, caps []*pipeline.RequestCapture) string {
	t.Helper()
	a, err := archive.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := session.New(0, 0, 0)
	store.AddRecorder(a)
	for i, e := range events {
		store.AppendCapture("s1", e, caps[i])
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(New(":0", store, WithArchive(a)).server.Handler)
	t.Cleanup(func() {
		ts.Close()
		store.Close()
	})
	return ts.URL
}

func bodyCapture(texts ...string) *pipeline.RequestCapture {
	var msgs []string
	for _, s := range texts {
		msgs = append(msgs, `{"role":"user","content":[{"type":"text","text":"`+s+` `+strings.Repeat("filler ", 12)+`"}]}`)
	}
	return &pipeline.RequestCapture{Original: []byte(`{"model":"m","messages":[` + strings.Join(msgs, ",") + `]}`)}
}

// The capture is searched where the archive holds it, and only there.
func TestSearch_ArchiveCapture(t *testing.T) {
	ev := req(&pipeline.InferenceExtension{Model: "m"})
	base := archived(t, []pipeline.SessionEvent{ev, ev, ev},
		[]*pipeline.RequestCapture{bodyCapture("NEEDLE-RAW"), bodyCapture("NEEDLE-RAW", "next"), bodyCapture("other")})
	if got := seqsFor(t, base, "s1", "needle-raw"); !slices.Equal(got, []uint64{1, 2}) {
		t.Errorf("seqs %v, want [1 2]", got)
	}
}

// Memory and the archive both hold the event: it is in the answer one time.
func TestSearch_EachSeqOneTime(t *testing.T) {
	ev := req(&pipeline.InferenceExtension{Model: "m",
		Messages: []pipeline.InferenceMessage{{Role: "user", Content: "needle-both"}}})
	base := archived(t, []pipeline.SessionEvent{ev}, []*pipeline.RequestCapture{bodyCapture("needle-both")})
	out, _, _ := search(t, base, "s1", "needle-both")
	if !slices.Equal(out.Seqs, []uint64{1}) || out.LastSeq != 1 {
		t.Errorf("answer %+v, want seqs [1] and lastSeq 1", out)
	}
}

func TestSearch_EveryEventMatches(t *testing.T) {
	ts, store := newTestServer(t)
	for range 50 {
		store.Append("s1", req(&pipeline.InferenceExtension{Model: "m",
			Messages: []pipeline.InferenceMessage{{Role: "user", Content: "common text"}}}))
	}
	out, _, _ := search(t, ts.URL, "s1", "common")
	if len(out.Seqs) != 50 || out.LastSeq != 50 || out.SchemaVersion != SchemaVersion {
		t.Errorf("answer has %d seqs, lastSeq %d, schemaVersion %d", len(out.Seqs), out.LastSeq, out.SchemaVersion)
	}
}

func TestSearch_AnswerAndErrors(t *testing.T) {
	ts, store := newTestServer(t)
	store.Append("s1", req(&pipeline.InferenceExtension{Model: "m"}))

	if _, _, body := search(t, ts.URL, "s1", "absent"); !strings.Contains(body, `"seqs":[]`) {
		t.Errorf("no match: body %s, want an empty seqs list, not null", body)
	}
	if _, code, body := search(t, ts.URL, "s1", ""); code != http.StatusBadRequest || !strings.Contains(body, `"q is required"`) {
		t.Errorf("empty q: %d %s", code, body)
	}
	if _, code, body := search(t, ts.URL, "nope", "x"); code != http.StatusNotFound || !strings.Contains(body, `"session not found"`) {
		t.Errorf("unknown session: %d %s", code, body)
	}
}

// A closed connection stops the pass, and nothing is written.
func TestSearch_StopsWhenTheRequestEnds(t *testing.T) {
	store := session.New(0, 0, 0)
	defer store.Close()
	store.Append("s1", req(&pipeline.InferenceExtension{Model: "m",
		Messages: []pipeline.InferenceMessage{{Role: "user", Content: "needle"}}}))
	srv := New(":0", store)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest("GET", "/v1/sessions/s1/search?q=needle", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	srv.server.Handler.ServeHTTP(w, r)
	if w.Body.Len() != 0 {
		t.Errorf("body %q, want nothing after the request ended", w.Body.String())
	}
}
