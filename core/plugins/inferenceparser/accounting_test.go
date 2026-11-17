package inferenceparser

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/accounting"
	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/settle"
	"github.com/rossoctl/cortex/core/pipeline"
)

// countingAccountant stands in for *accounting.Host.
type countingAccountant struct {
	calls  int
	saving []event.Saving
	extras accounting.Extras
}

func (c *countingAccountant) Savings(*pipeline.Context) ([]event.Saving, accounting.Extras, bool) {
	c.calls++
	return c.saving, c.extras, true
}

func captureLogs(t *testing.T, level slog.Level) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: level})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &logs
}

func shadowMetrics(p *InferenceParser) map[string]float64 {
	m := map[string]float64{}
	for _, mm := range p.Metrics() {
		m[mm.Name] = mm.Value
	}
	return m
}

// shadowCtx is a priced response whose request tool-prune shrank, so settle.Avoided has a figure.
func shadowCtx() *pipeline.Context {
	pctx := &pipeline.Context{Host: "api.anthropic.com", Path: "/v1/messages", SessionID: "s1", ResponseHeaders: http.Header{}}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: "claude-sonnet-4-5", InputTokens: 100, CacheReadTokens: 9000, OutputTokens: 5}
	pctx.Extensions.Custom = map[string]any{
		"tool-prune" + pipeline.PluginEventSuffix: map[string]any{"bytesRemoved": 4000, "bodyBytesAfter": 36000},
	}
	return pctx
}

// settleCost runs on every finalization path, and a listener can repeat a terminal frame, so it
// can run more than once for one request. The accountant is asked once, at the first pass with
// usage.
func TestSettleCost_CallsAccountingOnce(t *testing.T) {
	acc := &countingAccountant{}
	p := NewInferenceParser()
	p.acct = acc

	// No usage yet: nothing to account.
	pctx := &pipeline.Context{Path: "/v1/messages", ResponseHeaders: http.Header{}}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: "claude-sonnet-4-5"}
	p.settleCost(pctx)
	if acc.calls != 0 {
		t.Fatalf("Savings called %d times before the usage arrived", acc.calls)
	}

	pctx.Extensions.Inference.InputTokens = 10
	pctx.Extensions.Inference.CacheWriteTokens = 900
	p.settleCost(pctx)
	p.settleCost(pctx)
	if acc.calls != 1 {
		t.Errorf("Savings called %d times, want 1", acc.calls)
	}
}

// The shadow step (design §6): the record still publishes settle.Avoided's figure, and the
// accountant's figure is only compared with it. A difference is logged at INFO with both
// figures and counted in the plugin's metrics.
func TestSettleCost_ShadowWritesOldFigure(t *testing.T) {
	logs := captureLogs(t, slog.LevelDebug)
	acc := &countingAccountant{
		saving: []event.Saving{
			{Component: "tool-prune", Operator: "tool-prune", USD: 0.5, PenaltyUSD: 0.1, Basis: accounting.BasisUSD},
			{Component: "context-guru.mask", Operator: "context-guru.mask", USD: 0.2, Basis: accounting.BasisUSD},
		},
		extras: accounting.Extras{Thread: "main", OpsVersion: 1},
	}
	p := NewInferenceParser()
	p.rates = bodylessRates(t)
	p.acct = acc

	pctx := shadowCtx()
	old := settle.Avoided(pctx, p.rates)
	if len(old) != 1 || old[0].USD <= 0 {
		t.Fatalf("fixture: settle.Avoided = %+v, want one priced tool-prune saving", old)
	}

	p.settleCost(pctx)

	ev, ok := pctx.Extensions.Custom[event.Key+pipeline.PluginEventSuffix].(event.Event)
	if !ok {
		t.Fatalf("no cost record published: %+v", pctx.Extensions.Custom)
	}
	if len(ev.Avoided) != 1 || ev.Avoided[0] != old[0] {
		t.Errorf("published Avoided = %+v, want settle.Avoided's figure %+v unchanged", ev.Avoided, old)
	}
	out := logs.String()
	// tool-prune differs for real: INFO. context-guru.mask is only in the new figure: DEBUG.
	if n := strings.Count(out, "accounting: shadow"); n != 2 {
		t.Fatalf("want one shadow line per differing component (2), got %d:\n%s", n, out)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		switch {
		case strings.Contains(line, "component=tool-prune"):
			for _, want := range []string{"level=INFO", "session=s1", "newUSD=0.4", "newPenaltyUSD=0.1", "thread=main", "diffUSD=", "inOld=true"} {
				if !strings.Contains(line, want) {
					t.Errorf("tool-prune shadow line lacks %q: %s", want, line)
				}
			}
		case strings.Contains(line, "component=context-guru.mask"):
			if !strings.Contains(line, "level=DEBUG") || !strings.Contains(line, "inOld=false") {
				t.Errorf("an operator only the accountant prices is not a DEBUG line: %s", line)
			}
		}
	}
	m := shadowMetrics(p)
	if m["accounting shadow compared"] != 1 || m["accounting shadow differed"] != 1 || m["accounting shadow new-only"] != 1 {
		t.Errorf("metrics = %+v, want one compared, one differed, one new-only", m)
	}
}

// Two figures within a micro-dollar agree: no line.
func TestSettleCost_ShadowAgreementLogsNothing(t *testing.T) {
	logs := captureLogs(t, slog.LevelInfo)
	p := NewInferenceParser()
	p.rates = bodylessRates(t)
	pctx := shadowCtx()
	same := settle.Avoided(pctx, p.rates)[0]
	same.USD += 0.0000004
	p.acct = &countingAccountant{saving: []event.Saving{same}}
	p.settleCost(pctx)
	if strings.Contains(logs.String(), "accounting: shadow") {
		t.Errorf("figures within $0.000001 were logged as different:\n%s", logs.String())
	}
	if m := shadowMetrics(p); m["accounting shadow compared"] != 1 || m["accounting shadow differed"] != 0 {
		t.Errorf("metrics = %+v, want one compared, none differed", m)
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
	if m := p.Metrics(); m != nil {
		t.Errorf("Metrics = %+v with accounting off, want none", m)
	}
}

// An operator only the accountant prices is an expected difference: it does not count as
// "differed" and writes no INFO line.
func TestSettleCost_ShadowNewOnlyIsNotADifference(t *testing.T) {
	logs := captureLogs(t, slog.LevelInfo)
	p := NewInferenceParser()
	p.rates = bodylessRates(t)
	pctx := shadowCtx()
	same := settle.Avoided(pctx, p.rates)[0]
	p.acct = &countingAccountant{saving: []event.Saving{same,
		{Component: "context-guru.mask", Operator: "context-guru.mask", USD: 0.2},
		{Component: accounting.KeeperOperator, Operator: accounting.KeeperOperator, USD: 0.1}}}
	p.settleCost(pctx)
	if strings.Contains(logs.String(), "accounting: shadow") {
		t.Errorf("an INFO shadow line for operators only the accountant prices:\n%s", logs.String())
	}
	if m := shadowMetrics(p); m["accounting shadow differed"] != 0 || m["accounting shadow new-only"] != 1 {
		t.Errorf("metrics = %+v, want none differed, one new-only", m)
	}
}
