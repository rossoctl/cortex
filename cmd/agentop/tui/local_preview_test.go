package tui

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
	"github.com/rossoctl/cortex/core/sessionapi"
)

// seedAgentTurns appends n request/response pairs shaped like a long-running
// agent's traffic: a cache-heavy prompt that tool-prune trimmed, and a priced
// response. Shared by the rendering check and the manual preview so what CI
// asserts and what an operator eyeballs cannot drift.
func seedAgentTurns(store *session.Store, sid string, n int) {
	durations := []time.Duration{
		23_970 * time.Millisecond, 9_460 * time.Millisecond,
		53_660 * time.Millisecond, 25_500 * time.Millisecond,
	}
	for i := range n {
		rid := fmt.Sprintf("turn-%02d", i)
		// Prompt grows a little each turn, as a conversation does.
		cacheRead := 680_000 + i*1_000
		output := 1_850 + i*600
		store.Append(sid, pipeline.SessionEvent{
			At:        time.Now().Add(time.Duration(i) * 2 * time.Minute),
			Direction: pipeline.Outbound,
			Phase:     pipeline.SessionRequest,
			RequestID: rid,
			Host:      "ete-litellm.ai-mode.svc.cluster.local",
			Inference: &pipeline.InferenceExtension{Model: "claude-opus-5"},
			Plugins: map[string]json.RawMessage{
				"tool-prune": json.RawMessage(bigPromptWire),
			},
			Invocations: &pipeline.Invocations{Outbound: []pipeline.Invocation{
				{Plugin: "tool-prune", Action: pipeline.ActionModify, Reason: "tools_pruned"},
			}},
		})
		cost := 0.2824 + float64(i)*0.03
		store.Append(sid, pipeline.SessionEvent{
			At:         time.Now().Add(time.Duration(i)*2*time.Minute + durations[i%len(durations)]),
			Direction:  pipeline.Outbound,
			Phase:      pipeline.SessionResponse,
			RequestID:  rid,
			Host:       "ete-litellm.ai-mode.svc.cluster.local",
			StatusCode: 200,
			Duration:   durations[i%len(durations)],
			Inference: &pipeline.InferenceExtension{
				Model: "claude-opus-5", InputTokens: 1_300, CacheReadTokens: cacheRead,
				OutputTokens: output, TotalTokens: 1_300 + cacheRead + output,
			},
			Plugins: map[string]json.RawMessage{
				// The cost record, as the proxy publishes it on the RESPONSE: the
				// prompt-only and output-only figures (1.9e-5 per output token,
				// 5x the input rate) and the saving now travel here rather than
				// being derived in the UI from rates on the request event.
				event.Key: json.RawMessage(fmt.Sprintf(
					`{"cost_usd":%.4f,"source":"gateway-header","daily_total_usd":%.4f,"daily_max_usd":5,`+
						`"prompt_usd":%.5f,"output_usd":%.5f,`+
						`"avoided":[{"component":"tool-prune","tokensAvoided":9899,`+
						`"usd":0.0038,"tier":"cache_read","estimated":true}]}`,
					cost, cost*float64(i+1), 1_300*3.8e-6+float64(cacheRead)*3.8e-7, float64(output)*1.9e-5)),
			},
			Invocations: &pipeline.Invocations{Outbound: []pipeline.Invocation{
				{Plugin: "inference-parser", Action: pipeline.ActionObserve, Reason: "parsed"},
			}},
		})
	}
}

// TestSplitColumnsRender drives the real row builder over seeded traffic and
// asserts the TOKENS and COST cells of the exchange's one row (#1681): the prompt
// with its saving and the generated count in TOKENS, and the exchange total with
// its saving in COST.
func TestSplitColumnsRender(t *testing.T) {
	store := session.New(5*time.Minute, 100, 0)
	defer store.Close()
	seedAgentTurns(store, "preview", 1)

	view := store.View("preview")
	if view == nil || len(view.Events) != 2 {
		t.Fatalf("seeded view = %+v, want 2 events", view)
	}
	built := buildEventRows(view.Events)
	_, partner := computeEventPairs(built)
	rows, _ := mergeExchanges(built, partner)
	if len(rows) != 1 {
		t.Fatalf("%d rows, want 1: the request and its response are one exchange", len(rows))
	}

	// Prompt total (1,300 + 680,000) with the saving in parentheses, then the 1,850
	// generated.
	if got := exchangeTokensCell(rows[0]); got != "681.3k(−9.9k)→1.9k" {
		t.Errorf("TOKENS = %q, want %q", got, "681.3k(−9.9k)→1.9k")
	}
	// The 0.2824 exchange total on the record, with the saving.
	if got := exchangeCostCell(rows[0]); got != "$0.2824(−$0.0038)" {
		t.Errorf("COST = %q, want %q", got, "$0.2824(−$0.0038)")
	}
}

// TestLocalPreview is a manual affordance, not a CI test: it serves seeded
// traffic on a fixed port so `agentop observe --endpoint` can render it without a
// cluster, Keycloak, or a live LLM. Skipped unless AGENTOP_PREVIEW is set.
//
//	AGENTOP_PREVIEW=1 go test ./tui/ -run TestLocalPreview -v -timeout 0
//	go run . observe --endpoint http://127.0.0.1:47699
func TestLocalPreview(t *testing.T) {
	if os.Getenv("AGENTOP_PREVIEW") == "" {
		t.Skip("manual preview; set AGENTOP_PREVIEW=1 to serve seeded traffic")
	}
	const addr = "127.0.0.1:47699"
	store := session.New(30*time.Minute, 100, 0)
	defer store.Close()
	seedAgentTurns(store, "preview", 4)

	// Bind synchronously rather than inside the serving goroutine. Handing the
	// address to ListenAndServe and only logging its error makes a bind failure
	// — a stale server still holding the port is the common one — look identical
	// to a healthy start: the test blocks below either way, and the operator
	// sees "connection refused" from agentop with no hint why.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("cannot bind %s: %v\n"+
			"Something else holds the port. Find it with: lsof -nP -iTCP:47699", addr, err)
	}
	srv := sessionapi.New(addr, store)
	go func() {
		if err := srv.Server().Serve(ln); err != nil {
			t.Logf("preview server stopped: %v", err)
		}
	}()
	t.Logf("READY: seeded 4 turns, serving http://%s", addr)
	t.Logf("Now run:  GOWORK=off go run . observe --endpoint http://%s", addr)
	t.Log("Ctrl-C to stop.")
	select {} //nolint:staticcheck // blocks until the operator interrupts
}
