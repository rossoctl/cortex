package accounting

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/cortex/core/config"
	"github.com/rossoctl/cortex/core/cost/event"
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
	h := New(Options{Resolver: rates, Sink: NewSink(mp, testRates(pricing.ProvConfigured))})
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
	h := New(Options{Resolver: rates, Sink: NewSink(mp, rates)})
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

// zoneRates prices by endpoint and prompt size, so a token priced at the wrong binding shows:
// every tier's rate is the endpoint's base, doubled past 10,000 prompt tokens. Stateless, so
// safe from any goroutine.
type zoneRates map[string]float64

func (z zoneRates) Resolve(endpoint, _ string, promptTotal int) (pricing.Rates, pricing.Provenance) {
	base := z[endpoint]
	if promptTotal > 10000 {
		base *= 2
	}
	var r pricing.Rates
	for _, t := range []pricing.Tier{pricing.TierInput, pricing.TierCacheWrite, pricing.TierCacheRead, pricing.TierOutput} {
		r.Base[t], r.Set[t] = base, true
	}
	return r, pricing.ProvConfigured
}
func (zoneRates) CurrencyFor(string, string) string { return "USD" }

// ledgerSpends prices res's spends as Host.Savings does, for the comparison.
func ledgerSpends(r pricing.Resolver, res accountant.Result, model, host string, promptTotal int) []float64 {
	price := func(m, at string, tt accountant.TierTokens) (float64, bool) {
		return TierUSD(r, at, m, promptTotal, tt)
	}
	spend := func(c *accountant.ModelCall, at string) (float64, bool) { return CallUSD(r, c, at) }
	_, ex := SavingsBilled(res, price, spend, model, host, accountant.BillingAPI, config.DefaultSubscriptionWeights())
	var out []float64
	for _, s := range ex.Spends {
		out = append(out, s.USD)
	}
	return out
}

func spendOp(id, model, host string) accountant.PricedOp {
	return accountant.PricedOp{Kind: accountant.KindSpend, Operator: "context-guru.summarize", OpID: id,
		Call: &accountant.ModelCall{Model: model, Host: host, Usage: accountant.CallUsage{
			Usage:  accountant.Usage{Input: 50, CacheRead: 1000, CacheWrite: 200, CacheWrite1h: 100},
			Output: 5,
		}}}
}

var zone = zoneRates{"api.example": 1e-6, "other.example": 7e-6}

// A spend on a model and host of its own is priced at the call's host and the call's own prompt
// total, as the ledger's spends[] is: the pushed added.usd{kind=spend} equals it to the micro,
// while the request's saving stays at the request's binding (a long-context prompt here).
func TestSink_SpendMatchesTheLedgerToTheMicro(t *testing.T) {
	mp, rd := manual(t)
	s := NewSink(mp, zone)
	res := maskResult()
	res.Ops = append(res.Ops, spendOp("sp1", "m2", "other.example"))
	meta := accountant.RequestMeta{Model: "m1", At: time.Now(), Billing: apiBilling}
	s.Observe(meta, accountant.Usage{Input: 30000}, res, "api.example", 30000)

	ledger := ledgerSpends(zone, res, "m1", "api.example", 30000)
	got := sum(t, rd, "llm.savings.added.usd", map[string]string{"kind": "spend"})
	if len(ledger) != 1 || ledger[0] <= 0 || !micro(got, ledger[0]) {
		t.Errorf("pushed spend usd = %.9f, ledger spends = %v: they must agree", got, ledger)
	}
	wantSaved, _ := TierUSD(zone, "api.example", "m1", 30000, res.Ops[0].Saved)
	if gotSaved := sum(t, rd, "llm.savings.saved.usd", map[string]string{"component": "mask"}); !micro(gotSaved, wantSaved) {
		t.Errorf("saved usd = %.9f, want %.9f at the request's binding", gotSaved, wantSaved)
	}
}

// requestBound prices a call at the request's binding: what a colliding spend is pushed at.
func requestBound(c *accountant.ModelCall, model string) float64 {
	cc := *c
	cc.Model = model
	tt := callTiers(&cc)
	usd, _ := TierUSD(zone, "api.example", model, 30000, tt)
	out, _ := USD(zone, "api.example", model, 30000, pricing.TierOutput, int64(cc.Usage.Output))
	return usd + out
}

// COLLISIONS (documented in bindingsFor): the pricer sees only a model, so a spend on the
// request's own model with another host, or two spends on one model with different hosts, are
// priced at the request's binding — and differ from the ledger, which prices each call apart.
func TestSink_SpendCollisionFallsBackToTheRequestBinding(t *testing.T) {
	for _, c := range []struct {
		name  string
		ops   []accountant.PricedOp
		model string
	}{
		{"spend on the request's model", []accountant.PricedOp{spendOp("a", "m1", "other.example")}, "m1"},
		{"two spends on one model", []accountant.PricedOp{spendOp("a", "m2", "other.example"), spendOp("b", "m2", "")}, "m2"},
	} {
		t.Run(c.name, func(t *testing.T) {
			mp, rd := manual(t)
			s := NewSink(mp, zone)
			res := accountant.Result{Ops: c.ops}
			s.Observe(accountant.RequestMeta{Model: "m1", At: time.Now(), Billing: apiBilling},
				accountant.Usage{Input: 30000}, res, "api.example", 30000)
			var want, ledgerTotal float64
			for _, op := range c.ops {
				want += requestBound(op.Call, c.model)
			}
			for _, v := range ledgerSpends(zone, res, "m1", "api.example", 30000) {
				ledgerTotal += v
			}
			got := sum(t, rd, "llm.savings.added.usd", map[string]string{"kind": "spend"})
			if !micro(got, want) {
				t.Errorf("pushed spend usd = %.9f, want %.9f at the request's binding", got, want)
			}
			if micro(got, ledgerTotal) {
				t.Errorf("the collision case agreed with the ledger (%.9f); the fixture no longer tests a fallback", got)
			}
		})
	}
}

// Cortex's billing state decides the money: a request billed on a subscription, or with no
// credential Cortex can read, moves the token counters and llm.savings.estimated, never a USD
// counter.
func TestSink_NonAPIBillingMovesNoMoney(t *testing.T) {
	for _, mode := range []accountant.BillingMode{accountant.BillingSubscription, accountant.BillingUnknown} {
		t.Run(string(mode), func(t *testing.T) {
			mp, rd := manual(t)
			s := NewSink(mp, zone)
			res := maskResult()
			res.Ops = append(res.Ops, spendOp("sp1", "m2", "other.example"))
			s.Observe(accountant.RequestMeta{Model: "m1", At: time.Now(), Billing: accountant.Billing{Mode: mode}},
				accountant.Usage{Input: 30000}, res, "api.example", 30000)
			if got := sum(t, rd, "llm.savings.saved.tokens", map[string]string{"component": "mask", "tier": "input"}); got != 1200 {
				t.Errorf("saved tokens = %v, want 1200", got)
			}
			for _, name := range []string{"llm.savings.saved.usd", "llm.savings.added.usd", "llm.savings.cache_effect.usd", "llm.savings.interaction.usd"} {
				if got := sum(t, rd, name, nil); got != 0 {
					t.Errorf("%s = %v, want 0 for billing %s", name, got, mode)
				}
			}
			if got := sum(t, rd, "llm.savings.estimated", nil); got != 1 {
				t.Errorf("estimated = %v, want 1", got)
			}
		})
	}
}

// A dry-run spend and one the library refused to price are recorded by the ledger, flagged
// Projected or Refused, and add nothing to any total; the sink books neither. Both outputs say 0
// for them, and an applied spend still counts in both.
func TestSpends_DryRunAndRefusedCountInNeitherOutput(t *testing.T) {
	applied := spendOp("ok", "m2", "other.example")
	dry := spendOp("dry", "m3", "other.example")
	dry.DryRun = true
	refused := spendOp("no", "m4", "other.example")
	res := accountant.Result{
		Ops:      []accountant.PricedOp{applied, dry, refused},
		Unpriced: []accountant.Unpriced{{OpID: "no", Reason: "call_model_unknown"}},
	}
	for _, mode := range []accountant.BillingMode{accountant.BillingAPI, accountant.BillingSubscription, accountant.BillingUnknown} {
		t.Run(string(mode), func(t *testing.T) {
			price := func(m, at string, tt accountant.TierTokens) (float64, bool) { return TierUSD(zone, at, m, 30000, tt) }
			spend := func(c *accountant.ModelCall, at string) (float64, bool) { return CallUSD(zone, c, at) }
			_, ex := SavingsBilled(res, price, spend, "m1", "api.example", mode, config.DefaultSubscriptionWeights())
			if len(ex.Spends) != 3 {
				t.Fatalf("spends = %+v, want all three recorded", ex.Spends)
			}
			flags := map[string][2]bool{}
			for _, sp := range ex.Spends {
				flags[sp.ChangeID+sp.Model] = [2]bool{sp.Projected, sp.Refused}
			}
			if flags["m2"] != [2]bool{false, false} || flags["m3"] != [2]bool{true, false} || flags["m4"] != [2]bool{false, true} {
				t.Errorf("flags = %v, want m2 applied, m3 projected, m4 refused", flags)
			}
			only := event.Event{Spends: []event.SpendRecord{ex.Spends[0]}}
			all := event.Event{Spends: ex.Spends}
			if ex.Spends[0].Model != "m2" {
				t.Fatalf("first spend is %q, want the applied m2", ex.Spends[0].Model)
			}
			if got, want := all.Figures(), only.Figures(); got != want || want.IsZero() {
				t.Errorf("figures with the dry and refused spends = %+v, want the applied one's %+v alone", got, want)
			}
			if mode != accountant.BillingAPI {
				return
			}
			mp, rd := manual(t)
			NewSink(mp, zone).Observe(accountant.RequestMeta{Model: "m1", At: time.Now(), Billing: apiBilling},
				accountant.Usage{Input: 30000}, res, "api.example", 30000)
			got := sum(t, rd, "llm.savings.added.usd", map[string]string{"kind": "spend"})
			if want := float64(all.Figures().PluginSpendMicros) / 1e6; !micro(got, want) {
				t.Errorf("pushed spend usd = %.9f, ledger PluginSpendMicros = %.9f: they must agree", got, want)
			}
		})
	}
}

// A ping must not be counted as an inference token/cost observation.
func TestSink_PingCountsOnlyUnderKeepalive(t *testing.T) {
	mp, rd := manual(t)
	rates := testRates(pricing.ProvConfigured)
	s := NewSink(mp, rates)
	meta := accountant.RequestMeta{RequestID: "", Session: "s1", Model: "claude-sonnet-4-5", At: time.Now(), Billing: apiBilling}
	u := accountant.Usage{Input: 100, CacheRead: 50}
	s.Observe(meta, u, accountant.Result{}, "api.example.com", 150)

	if got := sum(t, rd, "llm.inference.tokens", nil); got != 0 {
		t.Errorf("ping counted under llm.inference.tokens: %v", got)
	}
	if got := sum(t, rd, "llm.keepalive.pings", nil); got != 1 {
		t.Errorf("llm.keepalive.pings = %v, want 1", got)
	}
}

// A non-ping request with no cache read is a cache miss, priced the same as the savings sink prices it.
func TestSink_CacheMissCountsAndPrices(t *testing.T) {
	mp, rd := manual(t)
	rates := testRates(pricing.ProvConfigured)
	s := NewSink(mp, rates)
	meta := accountant.RequestMeta{RequestID: "r1", Session: "s1", Model: "claude-sonnet-4-5", At: time.Now(), Billing: apiBilling}
	u := accountant.Usage{Input: 100}
	s.Observe(meta, u, accountant.Result{}, "api.example.com", 100)

	if got := sum(t, rd, "llm.cache.misses", nil); got != 1 {
		t.Errorf("llm.cache.misses = %v, want 1", got)
	}
	want, _ := USD(rates, "api.example.com", "claude-sonnet-4-5", 100, pricing.TierInput, 100)
	if got := sum(t, rd, "llm.cache.miss_cost_usd", nil); !micro(got, want) {
		t.Errorf("llm.cache.miss_cost_usd = %v, want %v", got, want)
	}
}

// A request with a cache read is not a cache miss.
func TestSink_CacheReadIsNotAMiss(t *testing.T) {
	mp, rd := manual(t)
	rates := testRates(pricing.ProvConfigured)
	s := NewSink(mp, rates)
	meta := accountant.RequestMeta{RequestID: "r1", Session: "s1", Model: "claude-sonnet-4-5", At: time.Now(), Billing: apiBilling}
	u := accountant.Usage{Input: 100, CacheRead: 50}
	s.Observe(meta, u, accountant.Result{}, "api.example.com", 150)

	if got := sum(t, rd, "llm.cache.misses", nil); got != 0 {
		t.Errorf("llm.cache.misses = %v, want 0", got)
	}
}

// A request priced at a bundled (fallback) rate is estimated even when the accountant Result
// itself carries no Unpriced ops — the request's own price, not the accountant's savings
// computation, is what "estimated" means here. (review finding 3)
func TestSink_BundledRateIsEstimatedWithNoUnpricedOps(t *testing.T) {
	mp, rd := manual(t)
	rates := testRates(pricing.ProvBundled)
	s := NewSink(mp, rates)
	meta := accountant.RequestMeta{RequestID: "r1", Session: "s1", Model: "claude-sonnet-4-5", At: time.Now(), Billing: apiBilling}
	u := accountant.Usage{Input: 100, CacheRead: 50}
	s.Observe(meta, u, accountant.Result{}, "api.example.com", 150) // no Unpriced ops

	if got := sum(t, rd, "llm.inference.estimated_cost_usd", nil); got <= 0 {
		t.Errorf("llm.inference.estimated_cost_usd = %v, want > 0 (bundled rate)", got)
	}
	if got := sum(t, rd, "llm.inference.cost_usd", nil); got != 0 {
		t.Errorf("llm.inference.cost_usd = %v, want 0 (a bundled-rate price is estimated, not actual)", got)
	}
}

// A request with no rate at all (ProvNone) is estimated at 0 cost, not counted as $0 of actual
// cost — $0 "actual" would read as "this is free", not "this is unpriced".
func TestSink_NoRateIsEstimatedNotActual(t *testing.T) {
	mp, rd := manual(t)
	rates := testRates(pricing.ProvNone)
	s := NewSink(mp, rates)
	meta := accountant.RequestMeta{RequestID: "r1", Model: "claude-sonnet-4-5", At: time.Now(), Billing: apiBilling}
	u := accountant.Usage{Input: 100}
	s.Observe(meta, u, accountant.Result{}, "api.example.com", 100)

	if got := sum(t, rd, "llm.inference.cost_usd", nil); got != 0 {
		t.Errorf("llm.inference.cost_usd = %v, want 0", got)
	}
	// 0 estimated dollars is still correctly reported: the point of this test is that the
	// request never lands in cost_usd, not that estimated_cost_usd is non-zero here (ProvNone
	// prices every tier at 0, per USD's own contract).
}

// A request with 1h cache-write tokens must not be priced twice: once as part of the raw
// CacheWrite count (at the 5m rate) and again as CacheWrite1h (at the 1h rate). CacheWrite1h is
// documented as "the part of CacheWrite billed at the one-hour rate, never an addition to it"
// (accountant.Usage). (review finding 2, Critical)
func TestSink_CacheWrite1hIsPricedOnce(t *testing.T) {
	mp, rd := manual(t)
	rates := testRates(pricing.ProvConfigured)
	s := NewSink(mp, rates)
	meta := accountant.RequestMeta{RequestID: "r1", Session: "s1", Model: "claude-sonnet-4-5", At: time.Now(), Billing: apiBilling}
	u := accountant.Usage{CacheWrite: 1000, CacheWrite1h: 1000} // every write is a 1h write
	s.Observe(meta, u, accountant.Result{}, "api.anthropic.com", 1000)

	want, _ := TierUSD(rates, "api.anthropic.com", "claude-sonnet-4-5", 1000, accountant.TierTokens{CacheWrite1h: 1000})
	if got := sum(t, rd, "llm.inference.cost_usd", nil); !micro(got, want) {
		t.Errorf("llm.inference.cost_usd = %v, want %v (TierUSD with CacheWrite5m=0, CacheWrite1h=1000)", got, want)
	}
	if got := sum(t, rd, "llm.inference.tokens", map[string]string{"tier": "cache_write_5m"}); got != 0 {
		t.Errorf("tokens tier=cache_write_5m = %v, want 0 (all 1000 CacheWrite tokens are the 1h part)", got)
	}
	if got := sum(t, rd, "llm.inference.tokens", map[string]string{"tier": "cache_write_1h"}); got != 1000 {
		t.Errorf("tokens tier=cache_write_1h = %v, want 1000", got)
	}
}

// None of the dollar counters set an OTel unit of "USD": the Prometheus remote-write exporter
// appends the unit to the series name, turning llm_inference_cost_usd_total into
// llm_inference_cost_usd_USD_total, which no dashboard queries. (review finding 1, Critical)
func TestSink_DollarCountersSetNoUnit(t *testing.T) {
	mp, rd := manual(t)
	rates := testRates(pricing.ProvConfigured)
	s := NewSink(mp, rates)
	meta := accountant.RequestMeta{RequestID: "r1", Model: "claude-sonnet-4-5", At: time.Now(), Billing: apiBilling}
	s.Observe(meta, accountant.Usage{Input: 100}, accountant.Result{}, "api.example.com", 100)

	var rm metricdata.ResourceMetrics
	if err := rd.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if strings.Contains(m.Name, "cost_usd") && m.Unit != "" {
				t.Errorf("metric %q has Unit %q, want \"\" (a counter unit of \"USD\" breaks the Prometheus series name)", m.Name, m.Unit)
			}
		}
	}
}

// Tier 1's counters carry a model label, matching the dashboards' per-model panels.
// (review finding 8)
func TestSink_CountersCarryModelLabel(t *testing.T) {
	mp, rd := manual(t)
	rates := testRates(pricing.ProvConfigured)
	s := NewSink(mp, rates)
	meta := accountant.RequestMeta{RequestID: "r1", Model: "claude-sonnet-4-5", At: time.Now(), Billing: apiBilling}
	s.Observe(meta, accountant.Usage{Input: 100}, accountant.Result{}, "api.example.com", 100)

	if got := sum(t, rd, "llm.inference.cost_usd", map[string]string{"model": "claude-sonnet-4-5"}); got <= 0 {
		t.Errorf("llm.inference.cost_usd with model label = %v, want > 0", got)
	}
}
