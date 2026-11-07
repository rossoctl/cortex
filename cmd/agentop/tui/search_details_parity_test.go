package tui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/cmd/agentop/apiclient"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/session/archive"
	"github.com/rossoctl/cortex/core/sessionapi"
)

// THE RULE, END TO END: "the proxy's answer, or detailHas on the summary row" is "detailHas on
// the full event with its capture", for each event and each query. The proxy is the real
// session API, with a store and an archive. The queries avoid the gaps the spec names.
func TestDetailSearch_ParityWithTheDetailView(t *testing.T) {
	a, err := archive.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := session.New(0, 0, 0)
	store.AddRecorder(a)
	pad := " " + strings.Repeat("filler ", 12)
	inf := pipeline.InferenceExtension{Model: "m",
		Messages: []pipeline.InferenceMessage{
			{Role: "user", Content: "alpha prompt" + pad},
			{Role: "assistant", Content: "beta reply", ToolCallNames: "bash", ToolUseIDs: "t1"},
			{Role: "user", ToolUseIDs: "t1"},
		},
		Tools: []pipeline.InferenceTool{{Name: "bash", Description: "run gamma",
			Parameters: `{"type":"object","properties":{"deltafield":{"type":"string"}}}`}},
		ToolResults:      []pipeline.InferenceToolResult{{ToolUseID: "t1", Content: "epsilon output"}},
		HistoryToolCalls: []pipeline.InferenceToolCall{{ID: "t1", Name: "bash", Arguments: `{"cmd":"zeta"}`}},
	}
	resp := inf
	resp.Completion = "eta answer"
	resp.ToolCalls = []pipeline.InferenceToolCall{{ID: "t2", Name: "read", Arguments: `{"path":"/theta"}`}}
	now := time.Now()
	body := `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"iota raw` + pad + `"}]}]}`
	for _, ev := range []struct {
		e pipeline.SessionEvent
		c *pipeline.RequestCapture
	}{
		{pipeline.SessionEvent{At: now, Phase: pipeline.SessionRequest, Inference: &inf},
			&pipeline.RequestCapture{Original: []byte(body), Header: http.Header{"X-Kappa": {"lambda"}}}},
		{pipeline.SessionEvent{At: now, Phase: pipeline.SessionResponse, Inference: &resp},
			&pipeline.RequestCapture{Response: &pipeline.ResponseCapture{Status: 200, Body: []byte("data: mu stream" + pad)}}},
		{pipeline.SessionEvent{At: now, Phase: pipeline.SessionRequest,
			MCP: &pipeline.MCPExtension{Method: "tools/call", Params: map[string]any{"path": "/nu"}}}, nil},
		{pipeline.SessionEvent{At: now, Phase: pipeline.SessionResponse,
			MCP: &pipeline.MCPExtension{Method: "tools/call", Result: map[string]any{"text": "xi"}}}, nil},
	} {
		store.AppendCapture("s1", ev.e, ev.c)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(sessionapi.New(":0", store, sessionapi.WithArchive(a)).Server().Handler)
	t.Cleanup(func() { ts.Close(); store.Close() })

	ctx, c := context.Background(), apiclient.New(ts.URL)
	page, err := c.GetSessionPage(ctx, "s1", 0, 100)
	if err != nil || len(page.Events) != 4 {
		t.Fatalf("page: %d events, err %v", len(page.Events), err)
	}
	for _, q := range []string{"alpha", "beta reply", "gamma", "deltafield", "epsilon", "zeta", "eta answer",
		"theta", "iota", "lambda", "x-kappa", "mu stream", "/nu", "xi", "toolUseId", "role", "bash", "absent"} {
		res, err := c.SearchSession(ctx, "s1", q)
		if err != nil {
			t.Fatal(err)
		}
		for i := range page.Events {
			sum := &page.Events[i]
			full, raw, err := c.GetEventRaw(ctx, "s1", sum.Seq)
			if err != nil {
				t.Fatal(err)
			}
			got := slices.Contains(res.Seqs, sum.Seq) || detailHas(eventRow{event: sum}, q)
			if want := detailHas(eventRow{event: full, raw: raw}, q); got != want {
				t.Errorf("%q, seq %d: proxy or summary = %v, the detail view = %v", q, sum.Seq, got, want)
			}
		}
	}
}
