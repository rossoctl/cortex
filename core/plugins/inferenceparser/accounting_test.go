package inferenceparser

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/cortex/core/pipeline"
)

// countingAccountant stands in for *accounting.Host.
type countingAccountant struct {
	calls int
	res   accountant.Result
}

func (c *countingAccountant) Compute(*pipeline.Context) (accountant.Result, bool) {
	c.calls++
	return c.res, true
}

// settleCost runs on every finalization path, and a listener can repeat a terminal frame, so it
// can run more than once for one request. The accountant is asked once, at the first pass with
// usage, and its figure is only logged at DEBUG for now: nothing is written.
func TestSettleCost_CallsAccountingOnce(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	acc := &countingAccountant{res: accountant.Result{Ops: []accountant.PricedOp{
		{Saved: accountant.TierTokens{CacheWrite5m: 120, CacheRead: 7}},
	}}}
	p := NewInferenceParser()
	p.acct = acc

	// No usage yet: nothing to account.
	pctx := &pipeline.Context{Path: "/v1/messages", ResponseHeaders: http.Header{}}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: "claude-sonnet-4-5"}
	p.settleCost(pctx)
	if acc.calls != 0 {
		t.Fatalf("Compute called %d times before the usage arrived", acc.calls)
	}

	pctx.Extensions.Inference.InputTokens = 10
	pctx.Extensions.Inference.CacheWriteTokens = 900
	p.settleCost(pctx)
	p.settleCost(pctx)
	if acc.calls != 1 {
		t.Errorf("Compute called %d times, want 1", acc.calls)
	}
	out := logs.String()
	if strings.Count(out, "accounting: computed") != 1 || !strings.Contains(out, "ops=1") ||
		!strings.Contains(out, "savedCacheWrite5m=120") || !strings.Contains(out, "savedCacheRead=7") {
		t.Errorf("want one DEBUG line with the op count and saved tokens per tier:\n%s", out)
	}
}

func TestSettleCost_NoAccountantIsFine(t *testing.T) {
	p := NewInferenceParser()
	pctx := &pipeline.Context{Path: "/v1/messages", ResponseHeaders: http.Header{}}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: "m", InputTokens: 10}
	p.settleCost(pctx) // must not panic
	p.SetAccounting(nil)
	if p.acct != nil {
		t.Error("SetAccounting(nil) stored a typed nil, which would read as set")
	}
}
