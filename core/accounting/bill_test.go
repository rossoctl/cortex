package accounting

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/pipeline"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// billCase is one request for the Tier 1 booking tests: what differs from a priced, API-billed
// /v1/messages request with a session.
type billCase struct {
	session string
	path    string
	body    []byte
	status  int
	resend  bool
	header  http.Header
	usage   *pipeline.InferenceExtension // nil: billedUsage
	host    func(h *Host)                // changes the Host before the request runs
}

// billHost is a Host with a sink on a manual reader.
func billHost(t *testing.T) (*Host, *sdkmetric.ManualReader) {
	t.Helper()
	mp, rd := manual(t)
	// The sink gets its own copy of the rates: fixedRates records its last call unlocked.
	h := New(Options{Resolver: testRates(pricing.ProvConfigured), Sink: NewSink(mp, testRates(pricing.ProvConfigured))})
	t.Cleanup(func() { _ = h.Close() })
	return h, rd
}

// billRequest runs c through a pipeline built with h, as a listener would, up to settle.
func billRequest(t *testing.T, h *Host, c billCase) *pipeline.Context {
	t.Helper()
	op := &operator{fwd: forwardedBody("hi"), ops: []accountant.Operation{pruneOp()}}
	p, err := pipeline.New([]pipeline.Plugin{op}, pipeline.WithAccountant(h))
	if err != nil {
		t.Fatal(err)
	}
	if c.path == "" {
		c.path = "/v1/messages"
	}
	if c.body == nil {
		c.body = clientBody("hi")
	}
	if c.status == 0 {
		c.status = 200
	}
	if c.header == nil {
		c.header = http.Header{"X-Api-Key": {"sk-ant-api03-x"}}
	}
	inf := billedUsage()
	if c.usage != nil {
		inf = *c.usage
	}
	inf.Model = "claude-sonnet-4-5"
	pctx := &pipeline.Context{Host: "api.anthropic.com", Path: c.path, Method: "POST", Body: c.body,
		SessionID: c.session, Headers: c.header}
	pctx.Extensions.Inference = &inf
	if act := p.Run(context.Background(), pctx); act.Type != pipeline.Continue {
		t.Fatalf("action = %v", act.Type)
	}
	if c.resend {
		pctx.MarkResend("limit")
	}
	pctx.StatusCode = c.status
	return pctx
}

// billedUsage is a first request that wrote its prompt to the cache and answered 50 tokens.
func billedUsage() pipeline.InferenceExtension {
	u := coldUsage()
	u.OutputTokens = 50
	return u
}

// tier1 is what one request booked in the Tier 1 counters.
type tier1 struct {
	tokens, cost, misses float64
	ok                   bool
}

func runBill(t *testing.T, c billCase) (tier1, *sdkmetric.ManualReader) {
	t.Helper()
	h, rd := billHost(t)
	if c.host != nil {
		c.host(h)
	}
	pctx := billRequest(t, h, c)
	_, ok := h.Compute(pctx)
	h.observing.Wait()
	return tier1{
		tokens: sum(t, rd, "llm.inference.tokens", nil),
		cost:   sum(t, rd, "llm.inference.cost_usd", nil),
		misses: sum(t, rd, "llm.cache.misses", nil),
		ok:     ok,
	}, rd
}

// A request the accountant does not price is billed by the provider all the same, so it books
// the Tier 1 counters as a priced request with the same usage does (rossoctl/cortex#2013).
func TestHost_UnpricedRequestBooksTier1(t *testing.T) {
	want, _ := runBill(t, billCase{session: "s1"})
	if !want.ok || want.tokens != 1853 || want.cost <= 0 || want.misses != 1 {
		t.Fatalf("priced control booked %+v, want ok, 1853 tokens, a cost and one miss", want)
	}

	cases := map[string]billCase{
		"no session ID":       {},
		"unknown wire format": {session: "s1", path: "/v1/other"},
		"two upstream rounds": {session: "s1", resend: true},
		"server-held history": {session: "s1", path: "/v1/responses",
			body: []byte(`{"model":"gpt-5","previous_response_id":"resp_1","input":[{"role":"user","content":"hi"}]}`)},
		"non-2xx status with usage": {session: "s1", status: 499},
		"record not ended in time": {session: "s1", host: func(h *Host) {
			release := make(chan struct{})
			t.Cleanup(func() { close(release) })
			h.beforeEnd = func() { <-release }
			h.computeWait = time.Nanosecond
		}},
		"library panic": {session: "s1", host: func(h *Host) { h.computeHook = func() { panic("library bug") } }},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, rd := runBill(t, c)
			if got.ok {
				t.Fatal("Compute priced the request; the case does not test a skip")
			}
			if got.tokens != want.tokens || !micro(got.cost, want.cost) || got.misses != want.misses {
				t.Errorf("booked %+v, want %+v", got, want)
			}
			if n := sum(t, rd, "llm.savings.requests", nil); n != 0 {
				t.Errorf("llm.savings.requests = %v, want 0: a skip books no saving", n)
			}
		})
	}
}

// A request with no usage, or with counts no request could have reported, books nothing.
func TestHost_UnpricedRequestWithoutUsageBooksNothing(t *testing.T) {
	cases := map[string]pipeline.InferenceExtension{
		"no usage":    {InputTokens: 0},
		"implausible": {InputTokens: -5, CacheWriteTokens: 1800},
	}
	for name, u := range cases {
		t.Run(name, func(t *testing.T) {
			h, rd := billHost(t)
			pctx := billRequest(t, h, billCase{usage: &u})
			h.Compute(pctx)
			h.observing.Wait()
			if n := sum(t, rd, "llm.inference.tokens", nil); n != 0 {
				t.Errorf("llm.inference.tokens = %v, want 0", n)
			}
		})
	}
}

// Compute is called once per settle, but a repeated call must not book a request twice. The
// no-session path has no pending handle to latch on.
func TestHost_UnpricedRequestBooksOnce(t *testing.T) {
	for name, session := range map[string]string{"no record": "", "skipped record": "s1"} {
		t.Run(name, func(t *testing.T) {
			h, rd := billHost(t)
			pctx := billRequest(t, h, billCase{session: session, status: 502})
			h.Compute(pctx)
			h.Compute(pctx)
			h.observing.Wait()
			if n := sum(t, rd, "llm.inference.tokens", nil); n != 1853 {
				t.Errorf("llm.inference.tokens = %v, want 1853", n)
			}
		})
	}
}

// The billing gate is the priced path's: a subscription login books tokens and no dollars.
func TestHost_UnpricedSubscriptionBooksNoDollars(t *testing.T) {
	got, _ := runBill(t, billCase{header: http.Header{"Authorization": {"Bearer sk-ant-oat01-x"}}})
	if got.tokens != 1853 || got.cost != 0 {
		t.Errorf("booked %+v, want 1853 tokens and no cost", got)
	}
}

// A skipped request has no Result, so its miss cause has no phase: cold_start on the thread's
// first request.
func TestHost_UnpricedMissCause(t *testing.T) {
	_, rd := runBill(t, billCase{session: "s1", status: 499})
	if n := sum(t, rd, "llm.cache.misses", map[string]string{"cause": MissColdStart}); n != 1 {
		t.Errorf("llm.cache.misses{cause=cold_start} = %v, want 1", n)
	}
	_, rd = runBill(t, billCase{})
	if n := sum(t, rd, "llm.cache.misses", map[string]string{"cause": MissOther}); n != 1 {
		t.Errorf("no record: llm.cache.misses{cause=other} = %v, want 1", n)
	}
}
