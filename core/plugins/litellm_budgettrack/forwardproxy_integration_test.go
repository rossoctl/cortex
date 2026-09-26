package litellm_budgettrack

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fwd "github.com/rossoctl/cortex/core/listener/forwardproxy"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/plugins/inferenceparser"
	"github.com/rossoctl/cortex/core/pricing"
	"github.com/rossoctl/cortex/core/session"
)

// TestForwardProxyStreamedSSEUpdatesLedger is the listener-level test the PR #815
// review asked for: stand up the real forward proxy with a streamed
// (text/event-stream) upstream and BudgetTrack in the outbound pipeline, drive a
// request through the proxy, and assert the ledger moved.
//
// This exercises the path the direct-call unit tests structurally cannot: whether
// the listener actually dispatches response frames to the plugin. On the
// header-only version (before this branch), a streamed response never reaches the
// plugin's cost accounting, so this test would fail — which is exactly the gap the
// reviewer flagged.
func TestForwardProxyStreamedSSEUpdatesLedger(t *testing.T) {
	// Upstream emits Anthropic-style streamed usage and NO cost header — as
	// LiteLLM does for streamed responses — so the plugin must price it from the
	// parsed token usage.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f, _ := w.(http.Flusher)
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":100,\"output_tokens\":1}}}\n\n")
		if f != nil {
			f.Flush()
		}
		io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":40}}\n\n")
		if f != nil {
			f.Flush()
		}
	}))
	t.Cleanup(upstream.Close)

	spend := filepath.Join(t.TempDir(), "spend.json")
	p := configurePriced(t, 5, map[pricing.Tier]float64{
		pricing.TierInput:  1e-6,
		pricing.TierOutput: 5e-6,
	})
	p.cfg.SpendFile = spend
	raw, _ := json.Marshal(budgetTrackConfig{SpendFile: spend, MaxBudget: 5})
	// WrapConfigured is what the real build applies; it preserves StreamingResponder.
	wrapped := pipeline.WrapConfigured(p, raw)

	// inference-parser sits AFTER budget-track, which is what RequiresLater
	// declares. The response pass walks the chain in reverse, so this order makes
	// the parser fold each frame BEFORE budget-track settles the cost on the
	// terminal one. Reversing these two silently unprices the response — which is
	// the failure this test caught and RequiresLater now prevents at build time.
	pipe, err := pipeline.New([]pipeline.Plugin{wrapped, inferenceparser.NewInferenceParser()})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	if !pipe.HasStreamingResponders() {
		t.Fatal("pipeline does not recognize BudgetTrack as a StreamingResponder")
	}
	store := session.New(5*time.Minute, 100, 0)
	t.Cleanup(store.Close)
	srv, err := fwd.NewServer(pipeline.NewHolder(pipe), store, nil)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	proxy := httptest.NewServer(srv.Handler())
	t.Cleanup(proxy.Close)

	pu, _ := url.Parse(proxy.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}
	body := `{"model":"claude-opus-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, upstream.URL+"/v1/messages", strings.NewReader(body))
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request via proxy: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	if err := resp.Body.Close(); err != nil {
		t.Errorf("close body: %v", err)
	}

	// The terminal last=true dispatch runs in the handler's defer after the
	// stream is forwarded, so poll briefly for the ledger to settle.
	want := 100*1e-6 + 40*5e-6 // 0.0003
	for i := 0; i < 200; i++ {
		p.mu.Lock()
		got, calls := p.ledger.TotalSpend, p.ledger.TotalCalls
		p.mu.Unlock()
		if calls > 0 {
			if got < want-1e-12 || got > want+1e-12 {
				t.Fatalf("ledger TotalSpend = %v, want %v", got, want)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("ledger never updated from a streamed SSE response — the forward proxy did not dispatch frames to the plugin")
}
