package accounting

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// manual is a MeterProvider read on demand, and the reader that reads it.
func manual(t *testing.T) (*sdkmetric.MeterProvider, *sdkmetric.ManualReader) {
	t.Helper()
	rd := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(rd))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	return mp, rd
}

// sum adds every data point of the counter name whose attributes include want.
func sum(t *testing.T, rd *sdkmetric.ManualReader, name string, want map[string]string) float64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := rd.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	match := func(set attribute.Set) bool {
		for k, v := range want {
			if got, ok := set.Value(attribute.Key(k)); !ok || got.AsString() != v {
				return false
			}
		}
		return true
	}
	var total float64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, p := range d.DataPoints {
					if match(p.Attributes) {
						total += float64(p.Value)
					}
				}
			case metricdata.Sum[float64]:
				for _, p := range d.DataPoints {
					if match(p.Attributes) {
						total += p.Value
					}
				}
			}
		}
	}
	return total
}

// micro says whether two dollar figures agree to the micro-dollar.
func micro(a, b float64) bool { return math.Round(a*1e6) == math.Round(b*1e6) }

var apiBilling = accountant.Billing{Mode: accountant.BillingAPI}

// maskResult is one applied mask rewrite that saved input and cache-read tokens.
func maskResult() accountant.Result {
	return accountant.Result{Ops: []accountant.PricedOp{{
		Kind: accountant.KindRewrite, Operator: "context-guru.mask",
		Saved: accountant.TierTokens{Input: 1200, CacheRead: 34000, CacheWrite1h: 500},
	}}}
}

// The sink books a saved op by tier with the operator's component, and prices it with the same
// TierUSD the ledger uses, at the request's endpoint, model and prompt total.
func TestSink_SavedMatchesTheLedgerToTheMicro(t *testing.T) {
	mp, rd := manual(t)
	rates := testRates(pricing.ProvConfigured)
	s := NewSink(mp, rates)
	meta := accountant.RequestMeta{RequestID: "r1", Session: "s1", Model: "claude-sonnet-4-5", At: time.Now(), Billing: apiBilling}
	res := maskResult()
	s.Observe(meta, accountant.Usage{Input: 3, CacheRead: 34000}, res, "api.anthropic.com", 34003)

	if got := sum(t, rd, "llm.savings.saved.tokens", map[string]string{"kind": "rewrite", "tier": "input", "component": "mask"}); got != 1200 {
		t.Errorf("saved input tokens = %v, want 1200", got)
	}
	if got := sum(t, rd, "llm.savings.saved.tokens", map[string]string{"tier": "cache_read", "component": "mask"}); got != 34000 {
		t.Errorf("saved cache_read tokens = %v, want 34000", got)
	}
	if rates.endpoint != "api.anthropic.com" || rates.promptTotal != 34003 {
		t.Errorf("priced at %q %d, want the request's endpoint and prompt total", rates.endpoint, rates.promptTotal)
	}
	want, _ := TierUSD(rates, "api.anthropic.com", "claude-sonnet-4-5", 34003, res.Ops[0].Saved)
	got := sum(t, rd, "llm.savings.saved.usd", map[string]string{"component": "mask"})
	if want <= 0 || !micro(got, want) {
		t.Errorf("saved usd = %.9f, want the ledger's TierUSD %.9f", got, want)
	}
}

// endpointRates prices by endpoint, and fails the test when a prompt total reaches it paired with
// another endpoint's: that is the binding Sink.mu exists to keep.
type endpointRates struct {
	t      *testing.T
	input  map[string]float64
	totals map[string]int
}

func (e *endpointRates) Resolve(endpoint, _ string, promptTotal int) (pricing.Rates, pricing.Provenance) {
	if e.totals[endpoint] != promptTotal {
		e.t.Errorf("endpoint %q priced with prompt total %d, want %d", endpoint, promptTotal, e.totals[endpoint])
	}
	var r pricing.Rates
	r.Base[pricing.TierInput], r.Set[pricing.TierInput] = e.input[endpoint], true
	return r, pricing.ProvConfigured
}
func (e *endpointRates) CurrencyFor(string, string) string { return "USD" }

// Two requests on different endpoints, observed at once, are each priced with their own endpoint
// and prompt total.
func TestSink_ConcurrentObservationsKeepTheirOwnEndpoint(t *testing.T) {
	mp, rd := manual(t)
	r := &endpointRates{t: t,
		input:  map[string]float64{"a.example": 1e-6, "b.example": 1e-3},
		totals: map[string]int{"a.example": 100, "b.example": 200}}
	s := NewSink(mp, r)
	res := accountant.Result{Ops: []accountant.PricedOp{{Kind: accountant.KindRewrite, Operator: "tool-prune",
		Saved: accountant.TierTokens{Input: 10}}}}
	const n = 200
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		for ep, total := range r.totals {
			wg.Add(1)
			go func(ep string, total int) {
				defer wg.Done()
				meta := accountant.RequestMeta{RequestID: fmt.Sprint(ep, i), Model: "m", At: time.Now(), Billing: apiBilling}
				s.Observe(meta, accountant.Usage{Input: total}, res, ep, total)
			}(ep, total)
		}
	}
	wg.Wait()
	want := n * 10 * (1e-6 + 1e-3)
	if got := sum(t, rd, "llm.savings.saved.usd", map[string]string{"component": "tool-prune"}); !micro(got, want) {
		t.Errorf("saved usd = %.9f, want %.9f", got, want)
	}
}

type panicRates struct{}

func (panicRates) Resolve(string, string, int) (pricing.Rates, pricing.Provenance) { panic("boom") }
func (panicRates) CurrencyFor(string, string) string                               { return "USD" }

// A panic in the pricer costs that observation and nothing else: it does not escape Observe, and
// the sink's lock is released for the next one.
func TestSink_PricerPanicDoesNotEscape(t *testing.T) {
	mp, _ := manual(t)
	s := NewSink(mp, panicRates{})
	meta := accountant.RequestMeta{Model: "m", At: time.Now(), Billing: apiBilling}
	for i := 0; i < 2; i++ {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("Observe let a panic out: %v", r)
				}
			}()
			s.Observe(meta, accountant.Usage{}, maskResult(), "e", 1)
		}()
	}
}

// A nil sink, or one built with no provider, is a no-op.
func TestSink_NilIsANoOp(t *testing.T) {
	if s := NewSink(nil, testRates(pricing.ProvConfigured)); s != nil {
		t.Errorf("NewSink(nil) = %v, want nil", s)
	}
	(*Sink)(nil).Observe(accountant.RequestMeta{}, accountant.Usage{}, maskResult(), "", 0)
}

// The Host observes a computed request once, however many times Compute is asked, and the pushed
// saved.usd equals what Host.Savings priced for the same request to the micro.
func TestHost_ObservesEachComputedRequestOnce(t *testing.T) {
	mp, rd := manual(t)
	rates := testRates(pricing.ProvConfigured)
	var metas []accountant.RequestMeta
	var mu sync.Mutex
	// The sink gets its own copy of the same rates: fixedRates records its last call unlocked,
	// and the sink prices on its own goroutine.
	h, err := New(Options{Resolver: rates, Sink: NewSink(mp, testRates(pricing.ProvConfigured))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	h.observeHook = func(m accountant.RequestMeta) { mu.Lock(); metas = append(metas, m); mu.Unlock() }

	pctx := requestWithHeaders(t, h, "s1", http.Header{"X-Api-Key": {"sk-ant-api03-x"}}, clientBody("hi"),
		&operator{fwd: forwardedBody("hi"), ops: []accountant.Operation{pruneOp()}}, 200, coldUsage())
	ss, _, ok := h.Savings(pctx)
	if !ok {
		t.Fatal("Savings: no result")
	}
	res, _ := h.Compute(pctx) // latched: no second observation
	h.observing.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(metas) != 1 || metas[0].RequestID == "" || metas[0].Billing.Mode != accountant.BillingAPI {
		t.Fatalf("observed %+v, want one api request with its id", metas)
	}
	if got := sum(t, rd, "llm.savings.requests", nil); got != 1 {
		t.Errorf("llm.savings.requests = %v, want 1", got)
	}
	var want float64
	for _, op := range res.Ops {
		usd, _ := TierUSD(rates, "api.anthropic.com", "claude-sonnet-4-5", 1803, op.Saved)
		want += usd
	}
	got := sum(t, rd, "llm.savings.saved.usd", nil)
	if want <= 0 || !micro(got, want) {
		t.Errorf("pushed saved usd = %.9f, want %.9f", got, want)
	}
	// The request has no cache effect, so the ledger's saving is exactly the ops' saved dollars.
	if len(res.CacheEffects) != 0 {
		t.Fatalf("fixture has %d cache effects; the ledger comparison below needs none", len(res.CacheEffects))
	}
	if s := byOperator(t, ss)["op"]; !micro(s.USD, got) {
		t.Errorf("ledger saving %.9f, pushed %.9f: the two must agree", s.USD, got)
	}
}

// A keep-alive ping is observed with its standalone Result and an empty RequestID.
func TestHost_ObservesAPing(t *testing.T) {
	mp, rd := manual(t)
	rates := pingRates(t)
	h, err := New(Options{Resolver: rates, Sink: NewSink(mp, rates)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	var metas []accountant.RequestMeta
	h.observeHook = func(m accountant.RequestMeta) { metas = append(metas, m) }
	if _, ok := h.Ping("s1", nil, "api.anthropic.com", testPing(), okInfo()); !ok {
		t.Fatal("ping refused")
	}
	h.observing.Wait()
	if len(metas) != 1 || metas[0].RequestID != "" || metas[0].Session != "s1" || metas[0].Model != "claude-sonnet-4-5" {
		t.Fatalf("observed %+v, want one ping with an empty RequestID", metas)
	}
	if got := sum(t, rd, "llm.savings.added.tokens", map[string]string{"component": "keeper", "tier": "cache_read"}); got != 1800 {
		t.Errorf("keeper added cache_read tokens = %v, want 1800", got)
	}
}
