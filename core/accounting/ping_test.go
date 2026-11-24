package accounting

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/cortex/core/cost/event"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/model"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/session"
)

// fakeRecorder is a session.Recorder that keeps what it is given.
type fakeRecorder struct {
	mu     sync.Mutex
	events []*model.SessionEvent
	ids    []string
}

func (r *fakeRecorder) Record(sessionID string, e *model.SessionEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, sessionID)
	r.events = append(r.events, e)
}

func (r *fakeRecorder) got() ([]string, []*model.SessionEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ids...), append([]*model.SessionEvent(nil), r.events...)
}

// pingRates prices every model at $3/M input, $3.75/M cache write, $0.30/M cache read and
// $15/M output.
func pingRates(t *testing.T) pricing.Resolver {
	t.Helper()
	var r pricing.Rates
	for tier, v := range map[pricing.Tier]float64{
		pricing.TierInput: 3e-6, pricing.TierCacheWrite: 3.75e-6, pricing.TierCacheRead: 0.3e-6, pricing.TierOutput: 15e-6,
	} {
		r.Base[tier], r.Set[tier] = v, true
	}
	tab, err := pricing.NewTable([]pricing.Entry{{Host: "*", Model: "*", Rates: r, Prov: pricing.ProvConfigured}})
	if err != nil {
		t.Fatal(err)
	}
	return pricing.NewRegistry(tab)
}

var pingT0 = time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)

// testPing is a ping on the primary thread of session s1 that read 1,800 cached tokens, sent
// four minutes after the request at pingT0.
func testPing() accountant.Ping {
	return accountant.Ping{
		Session: "s1", ThreadID: "", Model: "claude-sonnet-4-5", Host: "api.anthropic.com",
		Usage:  accountant.CallUsage{Usage: accountant.Usage{Input: 1, CacheRead: 1800}, Output: 1},
		At:     pingT0.Add(4 * time.Minute),
		Window: pingT0,
	}
}

func okInfo() PingInfo { return PingInfo{Status: 200, Duration: 300 * time.Millisecond} }

// TestPing_Prices1hCacheWrite: a ping whose usage carries a 1-hour cache write must
// have that subset priced at the 1h rate, not the 5m one — recordPing fills
// InferenceExtension.CacheWrite1hTokens from p.Usage.CacheWrite1h, and
// settle.SettleInput prices it through the same 1h-tier logic a real response
// uses.
func TestPing_Prices1hCacheWrite(t *testing.T) {
	rec := &fakeRecorder{}
	h := New(Options{Resolver: pingRates(t), CostRecorders: []session.Recorder{rec}})
	t.Cleanup(func() { _ = h.Close() })

	p := accountant.Ping{
		Session: "s1", ThreadID: "", Model: "claude-sonnet-4-5", Host: "api.anthropic.com",
		Usage:  accountant.CallUsage{Usage: accountant.Usage{CacheWrite: 1000, CacheWrite1h: 600}},
		At:     pingT0.Add(4 * time.Minute),
		Window: pingT0,
	}
	if _, ok := h.Ping("s1", nil, "api.anthropic.com", p, okInfo()); !ok {
		t.Fatal("ping refused")
	}

	_, evs := rec.got()
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	e := evs[0]
	if e.Inference == nil || e.Inference.CacheWriteTokens != 1000 || e.Inference.CacheWrite1hTokens != 600 {
		t.Fatalf("inference = %+v, want CacheWriteTokens=1000 CacheWrite1hTokens=600", e.Inference)
	}
	recOut, ok := event.Record(e)
	if !ok {
		t.Fatal("no cost record on the event")
	}
	// pingRates: input 3e-6, cache write (5m) 3.75e-6. No explicit 1h rate, so the
	// Anthropic-shaped fallback (model "claude-sonnet-4-5") derives 1h = input x 2 =
	// 6e-6. 400 tokens stay at the 5m rate, 600 price at the 1h rate:
	// 400*3.75e-6 + 600*6e-6 = 0.0051.
	want := 400*3.75e-6 + 600*6e-6
	if !recOut.Priced() || recOut.CostUSD < want-1e-9 || recOut.CostUSD > want+1e-9 {
		t.Errorf("cost = %v (priced %v), want %v", recOut.CostUSD, recOut.Priced(), want)
	}
	// Priced strictly above what pricing every write token at the 5m rate would give
	// (0.00375), so the test would fail if the 1h subset were silently ignored.
	if flat := 1000 * 3.75e-6; recOut.CostUSD <= flat {
		t.Errorf("cost = %v, want more than the all-5m figure %v (the 1h premium was not applied)", recOut.CostUSD, flat)
	}
}

// The session store is never touched by a ping, and that guarantee is structural: Host holds no
// store reference, so Ping has nothing to append to. The recorders are the only sink.
func TestPing_RecordsSpendToCostRecorders(t *testing.T) {
	a, b := &fakeRecorder{}, &fakeRecorder{}
	h := New(Options{Resolver: pingRates(t), CostRecorders: []session.Recorder{a}})
	t.Cleanup(func() { _ = h.Close() })
	h.AddCostRecorder(b) // registered after New, as cmd/cortex does

	client := &model.EventClient{Name: "claude-code"}
	res, ok := h.Ping("s1", client, "api.anthropic.com", testPing(), okInfo())
	if !ok {
		t.Fatal("a 200 ping with usage and a window was refused")
	}
	if len(res.Ops) != 1 || res.Ops[0].Kind != accountant.KindSpend {
		t.Fatalf("Result.Ops = %+v, want one spend", res.Ops)
	}
	for name, r := range map[string]*fakeRecorder{"New": a, "AddCostRecorder": b} {
		ids, evs := r.got()
		if len(evs) != 1 {
			t.Fatalf("recorder from %s got %d events, want 1", name, len(evs))
		}
		e := evs[0]
		if ids[0] != "s1" || e.SessionID != "s1" {
			t.Errorf("%s: session = %q / %q, want s1", name, ids[0], e.SessionID)
		}
		if e.Phase != model.SessionResponse || e.Direction != model.Outbound || e.StatusCode != 200 {
			t.Errorf("%s: phase/direction/status = %v/%v/%d", name, e.Phase, e.Direction, e.StatusCode)
		}
		if e.Host != "api.anthropic.com" || e.Client != client || !e.At.Equal(testPing().At) {
			t.Errorf("%s: host/client/at = %q/%v/%v", name, e.Host, e.Client, e.At)
		}
		if e.Inference == nil || e.Inference.Model != "claude-sonnet-4-5" || e.Inference.CacheReadTokens != 1800 ||
			e.Inference.InputTokens != 1 || e.Inference.OutputTokens != 1 {
			t.Errorf("%s: inference = %+v", name, e.Inference)
		}
		rec, ok := event.Record(e)
		if !ok {
			t.Fatalf("%s: no cost record on the event", name)
		}
		// Purpose marks the ping; Source and Provenance keep how the cost was arrived at: from
		// the token counts, at the configured rate table.
		if rec.Purpose != PingPurpose || PingPurpose != event.PurposeKeepAlive {
			t.Errorf("%s: Purpose = %q, want %q", name, rec.Purpose, event.PurposeKeepAlive)
		}
		if rec.Source != event.SourceUsageFallback {
			t.Errorf("%s: Source = %q, want %q (the provenance settle set)", name, rec.Source, event.SourceUsageFallback)
		}
		if rec.Provenance != pricing.ProvConfigured.String() || !rec.Settled {
			t.Errorf("%s: provenance %q settled %v, want %q and settled", name, rec.Provenance, rec.Settled,
				pricing.ProvConfigured.String())
		}
		// 1 x $3/M + 1,800 x $0.30/M + 1 x $15/M = $0.000558.
		if want := 0.000558; !rec.Priced() || rec.CostUSD < want-1e-9 || rec.CostUSD > want+1e-9 {
			t.Errorf("%s: cost = %v (priced %v), want %v", name, rec.CostUSD, rec.Priced(), want)
		}
	}
}

func TestPing_RefusesZeroWindowNon2xxZeroUsage(t *testing.T) {
	rec := &fakeRecorder{}
	h := New(Options{Resolver: pingRates(t), CostRecorders: []session.Recorder{rec}})
	t.Cleanup(func() { _ = h.Close() })

	noWindow := testPing()
	noWindow.Window = time.Time{}
	noUsage := testPing()
	noUsage.Usage = accountant.CallUsage{}
	for name, c := range map[string]struct {
		p    accountant.Ping
		info PingInfo
	}{
		"zero window": {noWindow, okInfo()},
		"status 401":  {testPing(), PingInfo{Status: 401}},
		"status 0":    {testPing(), PingInfo{}},
		"zero usage":  {noUsage, okInfo()},
	} {
		if _, ok := h.Ping("s1", nil, "api.anthropic.com", c.p, c.info); ok {
			t.Errorf("%s: the ping was accepted", name)
		}
	}
	if _, evs := rec.got(); len(evs) != 0 {
		t.Errorf("refused pings recorded %d cost events, want 0", len(evs))
	}
	if _, ok := h.peekPing("s1", "main"); ok {
		t.Error("a refused ping left facts for the next request")
	}
}

// TestPing_RefusesImplausibleUsage: Ping's only usage gate was once
// u.Input+u.CacheRead+u.CacheWrite+u.Output == 0 — mixed-sign counters that sum to zero (or an
// outright negative count) passed it, and computeStandalone/Units/observe would then book
// negative spend or savings from a count no provider could actually have sent. Compute already
// refuses this via pricing.PlausibleUsage; Ping must too.
func TestPing_RefusesImplausibleUsage(t *testing.T) {
	rec := &fakeRecorder{}
	h := New(Options{Resolver: pingRates(t), CostRecorders: []session.Recorder{rec}})
	t.Cleanup(func() { _ = h.Close() })

	negative := testPing()
	negative.Usage = accountant.CallUsage{Usage: accountant.Usage{Input: 1800, CacheRead: -1800}} // sums to 0, not actually 0 usage
	badTTLSubset := testPing()
	badTTLSubset.Usage = accountant.CallUsage{Usage: accountant.Usage{Input: 1, CacheWrite: 100, CacheWrite1h: 200}} // 1h > CacheWrite
	for name, p := range map[string]accountant.Ping{
		"mixed-sign sums to zero":         negative,
		"CacheWrite1h exceeds CacheWrite": badTTLSubset,
	} {
		if _, ok := h.Ping("s1", nil, "api.anthropic.com", p, okInfo()); ok {
			t.Errorf("%s: the ping was accepted", name)
		}
	}
	if _, evs := rec.got(); len(evs) != 0 {
		t.Errorf("refused pings recorded %d cost events, want 0", len(evs))
	}
}

// factsSeen collects the EntryFacts each record ends with.
func factsSeen(h *Host) func() []accountant.EntryFacts {
	var mu sync.Mutex
	var seen []accountant.EntryFacts
	h.factsHook = func(f accountant.EntryFacts) {
		mu.Lock()
		seen = append(seen, f)
		mu.Unlock()
	}
	return func() []accountant.EntryFacts {
		mu.Lock()
		defer mu.Unlock()
		return append([]accountant.EntryFacts(nil), seen...)
	}
}

// clockHost is a Host on a settable clock.
func clockHost(t *testing.T, at *time.Time) *Host {
	t.Helper()
	h := New(Options{Resolver: pingRates(t), Now: func() time.Time { return *at }})
	t.Cleanup(func() { _ = h.Close() })
	return h
}

func TestPing_FactsAppliedToNextRequest(t *testing.T) {
	now := pingT0
	h := clockHost(t, &now)
	seen := factsSeen(h)

	first := request(t, h, "s1", "/v1/messages", clientBody("hi"), &operator{}, 200, coldUsage())
	if _, ok := h.Compute(first); !ok {
		t.Fatal("first request: no Result")
	}
	p := testPing()
	if _, ok := h.Ping("s1", nil, "api.anthropic.com", p, okInfo()); !ok {
		t.Fatal("ping refused")
	}

	now = pingT0.Add(8 * time.Minute)
	second := request(t, h, "s1", "/v1/messages", clientBody("hi"), &operator{}, 200,
		pipeline.InferenceExtension{InputTokens: 3, CacheReadTokens: 1800})
	if _, ok := h.Compute(second); !ok {
		t.Fatal("second request: no Result")
	}
	third := request(t, h, "s1", "/v1/messages", clientBody("hi"), &operator{}, 200,
		pipeline.InferenceExtension{InputTokens: 3, CacheReadTokens: 1800})
	if _, ok := h.Compute(third); !ok {
		t.Fatal("third request: no Result")
	}

	got := seen()
	if len(got) != 3 {
		t.Fatalf("saw %d EntryFacts, want 3", len(got))
	}
	if !got[0].LastPing.IsZero() {
		t.Errorf("first request has LastPing %v before any ping", got[0].LastPing)
	}
	wantWindow := "ka:main:" + pingT0.Format(time.RFC3339)
	if !got[1].LastPing.Equal(p.At) || got[1].PingWindow != wantWindow || got[1].PingUsage == nil ||
		got[1].PingUsage.CacheRead != 1800 {
		t.Errorf("second request facts = LastPing %v window %q usage %+v; want the ping's", got[1].LastPing,
			got[1].PingWindow, got[1].PingUsage)
	}
	if !got[2].LastPing.IsZero() || got[2].PingWindow != "" {
		t.Errorf("third request still carries the ping (LastPing %v, window %q): the facts were not cleared",
			got[2].LastPing, got[2].PingWindow)
	}
}

// A request that is not priced leaves the ping's facts for the next one, as the proxy's
// PingStore does: they belong to the next PRICED request of the thread.
func TestPing_FactsSurviveAnUnpricedRequest(t *testing.T) {
	now := pingT0
	h := clockHost(t, &now)
	seen := factsSeen(h)

	first := request(t, h, "s1", "/v1/messages", clientBody("hi"), &operator{}, 200, coldUsage())
	h.Compute(first)
	h.Ping("s1", nil, "api.anthropic.com", testPing(), okInfo())
	now = pingT0.Add(8 * time.Minute)
	refused := request(t, h, "s1", "/v1/messages", clientBody("hi"), &operator{}, 429, pipeline.InferenceExtension{})
	if _, ok := h.Compute(refused); ok {
		t.Fatal("a 429 was priced")
	}
	if _, ok := h.peekPing("s1", "main"); !ok {
		t.Fatal("an unpriced request took the ping's facts")
	}
	ok := request(t, h, "s1", "/v1/messages", clientBody("hi"), &operator{}, 200,
		pipeline.InferenceExtension{InputTokens: 3, CacheReadTokens: 1800})
	h.Compute(ok)
	if got := seen(); got[len(got)-1].LastPing.IsZero() {
		t.Error("the priced request after the 429 did not get the ping's facts")
	}
}

func TestPing_RescueOnNextRequest(t *testing.T) {
	now := pingT0
	h := clockHost(t, &now)

	first := request(t, h, "s1", "/v1/messages", clientBody("hi"), &operator{}, 200, coldUsage())
	if _, ok := h.Compute(first); !ok {
		t.Fatal("first request: no Result")
	}
	h.Ping("s1", nil, "api.anthropic.com", testPing(), okInfo())

	// Eight minutes after the first request: without the ping its 5m entry would have expired.
	now = pingT0.Add(8 * time.Minute)
	second := request(t, h, "s1", "/v1/messages", clientBody("hi"), &operator{}, 200,
		pipeline.InferenceExtension{InputTokens: 3, CacheReadTokens: 1800})
	res, ok := h.Compute(second)
	if !ok {
		t.Fatal("second request: no Result")
	}
	var rescue *accountant.CacheEffect
	for i := range res.CacheEffects {
		if res.CacheEffects[i].Kind == accountant.EffectRescue {
			rescue = &res.CacheEffects[i]
		}
	}
	if rescue == nil {
		t.Fatalf("no rescue cache effect; effects = %+v, phase %v, checks %+v", res.CacheEffects, res.Phase, res.Checks)
	}
	if !strings.HasPrefix(rescue.ChangeID, "ka:main:") {
		t.Errorf("rescue ChangeID = %q, want prefix ka:main:", rescue.ChangeID)
	}

	// The control: the same two requests with no ping between them credit no rescue.
	now = pingT0
	ctl := clockHost(t, &now)
	ctl.Compute(request(t, ctl, "s1", "/v1/messages", clientBody("hi"), &operator{}, 200, coldUsage()))
	now = pingT0.Add(8 * time.Minute)
	res, _ = ctl.Compute(request(t, ctl, "s1", "/v1/messages", clientBody("hi"), &operator{}, 200,
		pipeline.InferenceExtension{InputTokens: 3, CacheReadTokens: 1800}))
	for _, e := range res.CacheEffects {
		if e.Kind == accountant.EffectRescue {
			t.Errorf("a rescue with no ping: %+v", e)
		}
	}
}

// A manual ping is priced, but its facts are not kept: the write it avoided is already booked
// from the user's estimate, and a library rescue on the next request would count it twice
// (context-guru#768 B3).
func TestPing_ManualKeepsNoFacts(t *testing.T) {
	rec := &fakeRecorder{}
	h := New(Options{Resolver: pingRates(t), CostRecorders: []session.Recorder{rec}})
	t.Cleanup(func() { _ = h.Close() })
	info := okInfo()
	info.Manual = true
	if _, ok := h.Ping("s1", nil, "api.anthropic.com", testPing(), info); !ok {
		t.Fatal("a manual ping was not priced")
	}
	if _, ok := h.peekPing("s1", "main"); ok {
		t.Error("a manual ping kept facts for the next request")
	}
	if _, evs := rec.got(); len(evs) != 1 {
		t.Errorf("a manual ping recorded %d cost events, want 1", len(evs))
	}
}

// The ping's billing comes from the thread's last request (design §5.7).
func TestPing_UsesTheThreadsBilling(t *testing.T) {
	h := newHost(t)
	h.setThreadBilling("s1", "main", accountant.BillingSubscription)
	var got accountant.Billing
	orig := pingOps
	t.Cleanup(func() { pingOps = orig })
	pingOps = func(p accountant.Ping, b accountant.Billing) (accountant.RequestOps, accountant.PingFacts) {
		got = b
		return orig(p, b)
	}
	if _, ok := h.Ping("s1", nil, "api.anthropic.com", testPing(), okInfo()); !ok {
		t.Fatal("ping refused")
	}
	if got.Mode != accountant.BillingSubscription {
		t.Errorf("billing = %v, want the thread's subscription", got.Mode)
	}
}

// A panic in the library costs the ping its figure and nothing else: the keeper calls the
// emitter on its own goroutine, where an unrecovered panic would end the process.
func TestPing_PanicIsRecovered(t *testing.T) {
	rec := &fakeRecorder{}
	h := New(Options{CostRecorders: []session.Recorder{rec}})
	t.Cleanup(func() { _ = h.Close() })
	orig := pingOps
	t.Cleanup(func() { pingOps = orig })
	pingOps = func(accountant.Ping, accountant.Billing) (accountant.RequestOps, accountant.PingFacts) {
		panic("boom")
	}
	if _, ok := h.Ping("s1", nil, "api.anthropic.com", testPing(), okInfo()); ok {
		t.Error("a panicking ping reported ok")
	}
	if _, ok := h.peekPing("s1", "main"); ok {
		t.Error("a panicking ping left facts")
	}
}

func TestPing_NilHost(t *testing.T) {
	var h *Host
	if _, ok := h.Ping("s1", nil, "h", testPing(), okInfo()); ok {
		t.Error("a nil Host priced a ping")
	}
}

func TestPing_StoreIsBounded(t *testing.T) {
	now := pingT0
	h := clockHost(t, &now)
	for i := 0; i < pingFactsCap+5; i++ {
		now = now.Add(time.Millisecond)
		h.putPing("s", strings.Repeat("t", 1)+time.Duration(i).String(), accountant.PingFacts{PingWindow: "w"})
	}
	h.mu.Lock()
	n := len(h.pingFacts)
	h.mu.Unlock()
	if n > pingFactsCap {
		t.Fatalf("pingFacts holds %d entries, cap %d", n, pingFactsCap)
	}
}

// Two concurrent requests on the pinged thread both read the same ping facts (peek, before
// either is priced), and still only one rescue is credited. The accountant de-duplicates by its
// thread state, not by ChangeID: Request.Compute replaces the ThreadState atomically and prices
// again against the current one when another request replaced it first (R53,
// internal/engine/compute.go Compute), and phaseAndBoundary credits a rescue only when the gap
// since the stored previous request is past the TTL. Once the first request is stored, the
// second one's gap is measured from it, so it is hot and has no rescue.
func TestPing_ConcurrentRequestsCreditOneRescue(t *testing.T) {
	now := pingT0
	h := clockHost(t, &now)

	h.Compute(request(t, h, "s1", "/v1/messages", clientBody("hi"), &operator{}, 200, coldUsage()))
	h.Ping("s1", nil, "api.anthropic.com", testPing(), okInfo())

	// A two-party barrier in factsHook, which runs on each record's goroutine right after its
	// peek of the ping facts and before the record is ended. Compute cannot drop the facts
	// before its record is ended, so with both goroutines held here neither request can be
	// priced until both have read the same facts: the race this test is about, every run. A
	// broken barrier hangs rather than fails fast; go test's own -timeout ends it with every
	// goroutine's stack.
	var (
		mu      sync.Mutex
		facts   []accountant.EntryFacts
		release = make(chan struct{})
	)
	h.factsHook = func(f accountant.EntryFacts) {
		mu.Lock()
		facts = append(facts, f)
		if len(facts) == 2 {
			close(release)
		}
		mu.Unlock()
		<-release
	}

	now = pingT0.Add(8 * time.Minute)
	read := pipeline.InferenceExtension{InputTokens: 3, CacheReadTokens: 1800}
	a := request(t, h, "s1", "/v1/messages", clientBody("hi"), &operator{}, 200, read)
	b := request(t, h, "s1", "/v1/messages", clientBody("hi"), &operator{}, 200, read)
	var results []accountant.Result
	var wg sync.WaitGroup
	for _, pctx := range []*pipeline.Context{a, b} {
		wg.Add(1)
		go func(pctx *pipeline.Context) {
			defer wg.Done()
			res, ok := h.Compute(pctx)
			if !ok {
				t.Error("a request was not priced")
			}
			mu.Lock()
			results = append(results, res)
			mu.Unlock()
		}(pctx)
	}
	wg.Wait()

	if len(facts) != 2 || facts[0].PingWindow == "" || facts[0].PingWindow != facts[1].PingWindow {
		t.Fatalf("both concurrent requests should have read the same ping facts; facts = %+v", facts)
	}
	rescues := 0
	for _, res := range results {
		for _, e := range res.CacheEffects {
			if e.Kind == accountant.EffectRescue {
				rescues++
			}
		}
	}
	if rescues != 1 {
		t.Errorf("rescues = %d across the two requests, want 1", rescues)
	}
}
