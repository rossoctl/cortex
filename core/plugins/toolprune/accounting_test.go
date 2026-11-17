package toolprune

import (
	"context"
	"strings"
	"testing"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/cortex/core/accounting"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/tidwall/gjson"
)

// TestPrune_MovedCacheMarkKeepsTheRequestPriced: when the pruned tool is the one that carried the
// cache_control mark, the mark moves to the last survivor (TestPrune_PreservesCacheControlBreakpoint),
// and that survivor's wire bytes grow by the mark. The library's sum check compares the bytes the
// reported ops remove with the bytes the bodies differ by, section by section; with the growth
// unreported it fails, and the whole request is recorded with no saving. tool-prune reports the
// growth as a rewrite in place of the survivor — the form the context-guru proxy's envelope
// reporter gives a moved mark — so the sum holds and the removal is priced.
func TestPrune_MovedCacheMarkKeepsTheRequestPriced(t *testing.T) {
	notebook := `{"name":"NotebookEdit","description":"` + strings.Repeat("edit a notebook ", 200) +
		`","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}`
	body := `{"model":"claude-sonnet-4-5","max_tokens":10,"tools":[` +
		`{"name":"Read","description":"read a file","input_schema":{"type":"object"}},` +
		`{"name":"Bash","description":"run a command","input_schema":{"type":"object"}},` + notebook +
		`],"messages":[{"role":"user","content":[{"type":"text","text":"hi` + strings.Repeat(" lorem ipsum dolor", 400) +
		`","cache_control":{"type":"ephemeral"}}]}]}`

	h, err := accounting.New(accounting.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	pipe, err := pipeline.New([]pipeline.Plugin{configured(t, "NotebookEdit")}, pipeline.WithAccountant(h))
	if err != nil {
		t.Fatal(err)
	}
	pctx := inferenceCtx("/v1/messages", body, "Read", "Bash", "NotebookEdit")
	pctx.Host, pctx.Method, pctx.SessionID = "api.anthropic.com", "POST", "s1"
	pctx.Extensions.Inference.Model = "claude-sonnet-4-5"
	if act := pipe.Run(context.Background(), pctx); act.Type != pipeline.Continue {
		t.Fatalf("action = %v", act.Type)
	}
	if got := gjson.GetBytes(pctx.Body, "tools.1.cache_control").Exists(); !got {
		t.Fatalf("the mark did not move to the last survivor: %s", gjson.GetBytes(pctx.Body, "tools").Raw)
	}
	pctx.StatusCode = 200
	// A cold first request: the forwarded prompt written to the cache.
	pctx.Extensions.Inference.InputTokens = 3
	pctx.Extensions.Inference.CacheWriteTokens = len(pctx.Body) / 4

	res, ok := h.Compute(pctx)
	if !ok {
		t.Fatal("Compute returned no Result")
	}
	for _, c := range res.Checks {
		if c.Check == accountant.CheckSum {
			t.Errorf("sum check failed: %s — the moved mark's growth is not covered by an op", c.Detail)
		}
	}
	var removed *accountant.PricedOp
	for i := range res.Ops {
		if strings.HasPrefix(res.Ops[i].ChangeID, "tool-prune:NotebookEdit:") {
			removed = &res.Ops[i]
		}
	}
	if removed == nil {
		t.Fatalf("no op for the removed tool: %+v", res.Ops)
	}
	if removed.Saved.IsZero() {
		t.Errorf("the removal saved nothing: %+v (checks %+v, unpriced %+v)", removed, res.Checks, res.Unpriced)
	}
}
