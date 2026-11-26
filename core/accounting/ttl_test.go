package accounting

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/cortex/core/accounting/facts"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/pipeline"
)

// Tests for #2136: a 1h cache ask that the bill shows as 5m is priced at 5m (Request.LowerTTL).

// ask1h makes every cache mark of a body ask for the 1h tier.
func ask1h(b []byte) []byte {
	return []byte(strings.ReplaceAll(string(b), `"cache_control":{"type":"ephemeral"}`,
		`"cache_control":{"type":"ephemeral","ttl":"1h"}`))
}

// pruned1h is one request whose client and forwarded bodies ask 1h, with pruneOp.
func pruned1h(t *testing.T, h *Host, usage pipeline.InferenceExtension) *pipeline.Context {
	t.Helper()
	key := http.Header{"X-Api-Key": {"sk-ant-api03-x"}}
	op := &operator{fwd: ask1h(forwardedBody("hi")), ops: []accountant.Operation{pruneOp()}}
	return requestWithHeaders(t, h, "s1", key, ask1h(clientBody("hi")), op, 200, usage)
}

// ttlHost is a Host on a settable clock, with a sink read by rd.
func ttlHost(t *testing.T, at *time.Time) (*Host, func(name string, want map[string]string) float64) {
	t.Helper()
	mp, rd := manual(t)
	h := New(Options{Resolver: pingRates(t), Sink: NewSink(mp, testRates(pricing.ProvConfigured)),
		Now: func() time.Time { return *at }})
	t.Cleanup(func() { _ = h.Close() })
	return h, func(name string, want map[string]string) float64 {
		h.observing.Wait()
		return sum(t, rd, name, want)
	}
}

func mustCompute(t *testing.T, h *Host, pctx *pipeline.Context) accountant.Result {
	t.Helper()
	res, ok := h.Compute(pctx)
	if !ok {
		t.Fatal("Compute: no Result")
	}
	return res
}

// opsSaved adds the Saved tokens of every op of res.
func opsSaved(res accountant.Result) accountant.TierTokens {
	var s accountant.TierTokens
	for _, o := range res.Ops {
		s = s.Add(o.Saved)
	}
	return s
}

// usage1h is a cold write that the provider billed at the 1h tier: every written token is 1h.
func usage1h() pipeline.InferenceExtension {
	u := coldUsage()
	u.CacheWrite1hTokens = u.CacheWriteTokens
	return u
}

// A 1h ask billed with no 1h write is priced at the 5m tier, and counted. A 1h ask billed at 1h
// is not changed.
func TestHost_OneHourAskBilledAs5mIsPricedAt5m(t *testing.T) {
	for _, tc := range []struct {
		name      string
		usage     pipeline.InferenceExtension
		want1h    bool
		downgrade float64
	}{
		{"billed 5m", coldUsage(), false, 1},
		{"billed 1h", usage1h(), true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := pingT0
			h, count := ttlHost(t, &now)
			s := opsSaved(mustCompute(t, h, pruned1h(t, h, tc.usage)))
			if got := s.CacheWrite1h > 0; got != tc.want1h {
				t.Errorf("saved = %+v; want the 1h tier = %v", s, tc.want1h)
			}
			if s.CacheWrite1h == 0 && s.CacheWrite5m == 0 {
				t.Errorf("saved = %+v; want a cache-write saving", s)
			}
			if got := count("llm.cache.ttl_downgrades", map[string]string{"model": "claude-sonnet-4-5"}); got != tc.downgrade {
				t.Errorf("llm.cache.ttl_downgrades = %v, want %v", got, tc.downgrade)
			}
		})
	}
}

// After an idle gap past 5 minutes, a 1h ask billed as 5m has expired: the next request is cold,
// not a cache break that costs a saving.
func TestHost_OneHourAskBilledAs5mExpiresAfter5m(t *testing.T) {
	now := pingT0
	h, count := ttlHost(t, &now)
	mustCompute(t, h, pruned1h(t, h, coldUsage()))
	now = pingT0.Add(20 * time.Minute)
	res := mustCompute(t, h, pruned1h(t, h, coldUsage()))
	if res.Phase != accountant.PhaseCold {
		t.Errorf("phase = %v, want cold: the 5m entry expired", res.Phase)
	}
	for _, e := range res.CacheEffects {
		if e.Caused != (accountant.TierTokens{}) {
			t.Errorf("cache effect %s books a cost %+v on an expired entry", e.Kind, e.Caused)
		}
	}
	if got := count("llm.cache.misses", map[string]string{"cause": MissTTLExpiry}); got != 1 {
		t.Errorf("llm.cache.misses{cause=ttl_expiry} = %v, want 1", got)
	}
}

// A request that only reads has no write tier of its own: it keeps the thread's last billed
// tier. So the rescue of a ping is priced at the 5m tier the thread was billed at.
func TestHost_ReadOnlyTurnKeepsTheThreadsBilledTier(t *testing.T) {
	now := pingT0
	h, count := ttlHost(t, &now)
	mustCompute(t, h, pruned1h(t, h, coldUsage()))
	h.Ping("s1", nil, "api.anthropic.com", testPing(), okInfo())
	now = pingT0.Add(8 * time.Minute)
	res := mustCompute(t, h, pruned1h(t, h, pipeline.InferenceExtension{InputTokens: 3, CacheReadTokens: 1800}))
	var rescue *accountant.CacheEffect
	for i := range res.CacheEffects {
		if res.CacheEffects[i].Kind == accountant.EffectRescue {
			rescue = &res.CacheEffects[i]
		}
	}
	if rescue == nil {
		t.Fatalf("no rescue; effects = %+v, phase %v", res.CacheEffects, res.Phase)
	}
	if rescue.Saved.CacheWrite1h != 0 || rescue.Saved.CacheWrite5m == 0 {
		t.Errorf("rescue saved = %+v, want the 5m write tier", rescue.Saved)
	}
	if got := count("llm.cache.ttl_downgrades", nil); got != 2 {
		t.Errorf("llm.cache.ttl_downgrades = %v, want 2 (the write and the read-only turn)", got)
	}
}

// billedOneHour's rule, case by case.
func TestHost_BilledOneHour(t *testing.T) {
	key := threadBillingKey{session: "s1", thread: "main"}
	h := newHost(t)
	// No entry: a write bills its own tier, a read keeps the ask.
	if !h.billedOneHour(key, true, 0, 0) {
		t.Error("read, no entry, asked 1h: want 1h")
	}
	if h.billedOneHour(key, false, 0, 0) {
		t.Error("read, no entry, asked 5m: want 5m")
	}
	if h.billedOneHour(key, true, 100, 0) {
		t.Error("write billed 5m: want 5m")
	}
	// RequestEnd makes the entry and billedOneHour only updates it, so that write was not kept.
	if !h.billedOneHour(key, true, 0, 0) {
		t.Error("read after an unrecorded write: want the ask")
	}

	h.setThreadBilling("s1", "main", accountant.BillingAPI)
	for _, step := range []struct {
		name           string
		asked1h        bool
		cw, cw1h       int
		want, wantKept bool
	}{
		{"read before any write keeps the ask", true, 0, 0, true, false},
		{"write billed 5m", true, 100, 0, false, true},
		{"read keeps 5m", true, 0, 0, false, true},
		{"part of the write at 1h is 5m", true, 100, 40, false, true},
		{"write billed 1h", true, 100, 100, true, true},
		{"read keeps 1h", false, 0, 0, true, true},
	} {
		if got := h.billedOneHour(key, step.asked1h, step.cw, step.cw1h); got != step.want {
			t.Errorf("%s: billedOneHour = %v, want %v", step.name, got, step.want)
		}
		h.mu.Lock()
		kept := h.threadBilling[key].tierKnown
		h.mu.Unlock()
		if kept != step.wantKept {
			t.Errorf("%s: tierKnown = %v, want %v", step.name, kept, step.wantKept)
		}
	}

	// RequestEnd's next call keeps the tier and the new mode.
	h.setThreadBilling("s1", "main", accountant.BillingSubscription)
	h.mu.Lock()
	e := h.threadBilling[key]
	h.mu.Unlock()
	if !e.tierKnown || !e.billed1h || e.mode != accountant.BillingSubscription {
		t.Errorf("after setThreadBilling: entry = %+v, want the 1h tier kept and the new mode", e)
	}
}

// An OpenAI request has a minimum lifetime, not an exact one: no tier is lowered or kept.
func TestHost_MinimumLifetimeHasNoTier(t *testing.T) {
	if f := facts.For(facts.ProviderOpenAI, "gpt-5", true); f.Lifetime == accountant.LifetimeExact {
		t.Fatalf("OpenAI facts have an exact lifetime: %+v; the test no longer covers the case", f)
	}
	now := pingT0
	h, count := ttlHost(t, &now)
	body := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hi` + strings.Repeat(" lorem", 400) + `"}]}`)
	p, err := pipeline.New([]pipeline.Plugin{&operator{}}, pipeline.WithAccountant(h))
	if err != nil {
		t.Fatal(err)
	}
	pctx := &pipeline.Context{Host: "api.openai.com", Path: "/v1/chat/completions", Method: "POST", Body: body,
		SessionID: "s1", Headers: http.Header{"Authorization": {"Bearer sk-x"}}}
	pctx.Extensions.Inference = &pipeline.InferenceExtension{Model: "gpt-5", InputTokens: 600, CacheWriteTokens: 0}
	if act := p.Run(context.Background(), pctx); act.Type != pipeline.Continue {
		t.Fatalf("action = %v", act.Type)
	}
	pctx.StatusCode = 200
	mustCompute(t, h, pctx)
	if got := count("llm.cache.ttl_downgrades", nil); got != 0 {
		t.Errorf("llm.cache.ttl_downgrades = %v, want 0", got)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for k, e := range h.threadBilling {
		if e.tierKnown {
			t.Errorf("thread %v remembers a tier for a minimum lifetime", k)
		}
	}
}

// Requests of one thread priced at the same time share the tier entry: run under -race.
func TestHost_BilledTierConcurrent(t *testing.T) {
	now := pingT0
	h, _ := ttlHost(t, &now)
	var wg sync.WaitGroup
	for i := range 8 {
		u := coldUsage()
		if i%2 == 0 {
			u = usage1h()
		}
		pctx := pruned1h(t, h, u)
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.Compute(pctx)
		}()
	}
	wg.Wait()
}
