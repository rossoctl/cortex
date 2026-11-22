package accounting

import (
	"net/http"
	"testing"
	"time"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/session"
)

// The cause label of llm.cache.misses and llm.cache.miss_cost_usd. The values are context-guru's,
// so the central dashboard stacks the two proxies on one axis.
func TestMissCause(t *testing.T) {
	brk := func(cause accountant.CacheEffectCause, change string) []accountant.CacheEffect {
		return []accountant.CacheEffect{{Kind: accountant.EffectBreak, Cause: cause, ChangeID: change}}
	}
	modelBreak := brk(accountant.CauseModelChanged, "")
	// wrote is whether the request wrote cache tokens: the spec's causes after ttl_expiry need a
	// written prefix to explain, so without one the cause is other.
	for name, c := range map[string]struct {
		res          accountant.Result
		first, wrote bool
		want         string
	}{
		"the library saw the model change":      {accountant.Result{Phase: accountant.PhaseHot, CacheEffects: modelBreak}, false, true, "model_changed"},
		"a plugin changed the model":            {accountant.Result{Phase: accountant.PhaseHot, CacheEffects: brk("", modelChangePrefix+"a->b")}, false, true, "model_changed"},
		"an expiry beats a model change":        {accountant.Result{Phase: accountant.PhaseCold, CacheEffects: modelBreak}, false, true, "ttl_expiry"},
		"a cold start beats a model change":     {accountant.Result{Phase: accountant.PhaseUnknown, CacheEffects: modelBreak}, true, true, "cold_start"},
		"a model change that wrote nothing":     {accountant.Result{Phase: accountant.PhaseHot, CacheEffects: modelBreak}, false, false, "other"},
		"the clock says the entry expired":      {accountant.Result{Phase: accountant.PhaseCold}, false, false, "ttl_expiry"},
		"the thread's first request":            {accountant.Result{Phase: accountant.PhaseUnknown}, true, false, "cold_start"},
		"unknown phase on a known thread":       {accountant.Result{Phase: accountant.PhaseUnknown}, false, true, "other"},
		"restored state: hot, cortex forgot":    {accountant.Result{Phase: accountant.PhaseHot}, true, true, "other"},
		"restored state: cold, cortex forgot":   {accountant.Result{Phase: accountant.PhaseCold}, true, true, "ttl_expiry"},
		"a hot entry the provider did not read": {accountant.Result{Phase: accountant.PhaseHot}, false, true, "other"},
		"another operation's break":             {accountant.Result{Phase: accountant.PhaseHot, CacheEffects: brk("", "test:big")}, false, true, "other"},
	} {
		if got := missCause(c.res, c.first, c.wrote); got != c.want {
			t.Errorf("%s: missCause = %q, want %q", name, got, c.want)
		}
	}
}

// Both miss counters carry the cause, so "Miss cost by cause" splits the money the same way
// "Misses by cause" splits the count.
func TestSink_CacheMissCarriesCause(t *testing.T) {
	mp, rd := manual(t)
	s := NewSink(mp, testRates(pricing.ProvConfigured))
	meta := accountant.RequestMeta{RequestID: "r1", Session: "s1", Model: "claude-sonnet-4-5", At: time.Now(), Billing: apiBilling}
	s.Observe(meta, accountant.Usage{Input: 100, CacheWrite: 1000}, 0, accountant.Result{Phase: accountant.PhaseCold}, "api.example.com", 1100, false)

	ttl := map[string]string{"cause": "ttl_expiry"}
	if got := sum(t, rd, "llm.cache.misses", ttl); got != 1 {
		t.Errorf("llm.cache.misses{cause=ttl_expiry} = %v, want 1", got)
	}
	if got := sum(t, rd, "llm.cache.miss_cost_usd", ttl); got <= 0 {
		t.Errorf("llm.cache.miss_cost_usd{cause=ttl_expiry} = %v, want above 0", got)
	}
}

// A thread's first request is a cold start, and a request after the entry's TTL is an expiry.
// Driven through the Host, so the "seen before" fact is the one RequestEnd records.
func TestHost_MissCauseFromTheThread(t *testing.T) {
	mp, rd := manual(t)
	rates := testRates(pricing.ProvConfigured)
	h := New(Options{Resolver: rates, Sink: NewSink(mp, testRates(pricing.ProvConfigured))})
	t.Cleanup(func() { _ = h.Close() })
	t0 := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	now := t0
	h.now = func() time.Time { return now }
	key := http.Header{"X-Api-Key": {"sk-ant-api03-x"}}

	run := func() {
		t.Helper()
		pctx := requestWithHeaders(t, h, "s1", key, clientBody("hi"), &operator{}, 200, coldUsage())
		if _, ok := h.Compute(pctx); !ok {
			t.Fatal("Compute: no result")
		}
		h.observing.Wait()
	}
	run()
	if got := sum(t, rd, "llm.cache.misses", map[string]string{"cause": "cold_start"}); got != 1 {
		t.Errorf("first request: misses{cause=cold_start} = %v, want 1", got)
	}
	now = t0.Add(10 * time.Minute) // past the 5-minute TTL of the entry the first request wrote
	run()
	if got := sum(t, rd, "llm.cache.misses", map[string]string{"cause": "ttl_expiry"}); got != 1 {
		t.Errorf("request after the TTL: misses{cause=ttl_expiry} = %v, want 1", got)
	}
	if got := sum(t, rd, "llm.cache.misses", map[string]string{"cause": "cold_start"}); got != 1 {
		t.Errorf("a second request on the thread was counted as a cold start (total %v, want 1)", got)
	}
}

// Every ping the keeper reports is counted once, by outcome, as context-guru counts it. A ping
// with no answer is booked at the keeper's projected cost, all of it estimated: the provider
// probably billed it, but nobody measured it.
func TestPing_CountsEveryOutcome(t *testing.T) {
	mp, rd := manual(t)
	rates := pingRates(t)
	rec := &fakeRecorder{}
	h := New(Options{Resolver: rates, Sink: NewSink(mp, rates), CostRecorders: []session.Recorder{rec}})
	t.Cleanup(func() { _ = h.Close() })

	noUsage := testPing()
	noUsage.Usage = accountant.CallUsage{}
	if _, ok := h.Ping("s1", nil, "api.anthropic.com", testPing(), okInfo()); !ok {
		t.Fatal("the priced ping was refused")
	}
	if _, ok := h.Ping("s1", nil, "api.anthropic.com", testPing(), PingInfo{Status: 401}); ok {
		t.Error("a 401 ping was accepted")
	}
	timedOut := PingInfo{TimedOut: true, ProjectedUSD: 0.01}
	if _, ok := h.Ping("s1", nil, "api.anthropic.com", noUsage, timedOut); ok {
		t.Error("a ping with no answer was accepted: it has no usage to price")
	}
	h.observing.Wait()

	for outcome, want := range map[string]float64{"ok": 1, "failed": 1, "timeout": 1} {
		if got := sum(t, rd, "llm.keepalive.pings", map[string]string{"outcome": outcome}); got != want {
			t.Errorf("llm.keepalive.pings{outcome=%s} = %v, want %v", outcome, got, want)
		}
	}
	if got := sum(t, rd, "llm.keepalive.pings", nil); got != 3 {
		t.Errorf("llm.keepalive.pings = %v, want 3: each ping once", got)
	}
	priced := sum(t, rd, "llm.keepalive.cost_usd", nil) - 0.01
	if priced <= 0 {
		t.Errorf("llm.keepalive.cost_usd holds %v beyond the timeout's 0.01; the priced ping is missing", priced)
	}
	if got := sum(t, rd, "llm.keepalive.estimated_cost_usd", nil); !micro(got, 0.01) {
		t.Errorf("llm.keepalive.estimated_cost_usd = %v, want 0.01: the projection only", got)
	}
	// The ledger is unchanged: a refused ping writes no cost event, as before.
	if _, evs := rec.got(); len(evs) != 1 {
		t.Errorf("cost events = %d, want 1 (the priced ping only)", len(evs))
	}
}

// A refused 2xx ping (no keep-alive window, or no usage) is still a ping that answered: ok, with
// no cost, because there is nothing to price.
func TestPing_Refused2xxIsOKWithNoCost(t *testing.T) {
	mp, rd := manual(t)
	rates := pingRates(t)
	h := New(Options{Resolver: rates, Sink: NewSink(mp, rates)})
	t.Cleanup(func() { _ = h.Close() })
	noWindow := testPing()
	noWindow.Window = time.Time{}
	if _, ok := h.Ping("s1", nil, "api.anthropic.com", noWindow, okInfo()); ok {
		t.Fatal("a ping with no window was accepted")
	}
	h.observing.Wait()
	if got := sum(t, rd, "llm.keepalive.pings", map[string]string{"outcome": "ok"}); got != 1 {
		t.Errorf("llm.keepalive.pings{outcome=ok} = %v, want 1", got)
	}
	if got := sum(t, rd, "llm.keepalive.cost_usd", nil); got != 0 {
		t.Errorf("llm.keepalive.cost_usd = %v, want 0", got)
	}
}
