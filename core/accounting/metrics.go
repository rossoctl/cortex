package accounting

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/rossoctl/accountant"
	acmetrics "github.com/rossoctl/accountant/metrics"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// MeterName is the instrumentation scope the savings counters are created under.
const MeterName = "github.com/rossoctl/cortex/core/accounting"

// Components are the operator names, without their prefix, that may appear as the `component`
// label of the llm.savings.* counters (acmetrics.WithComponents). Any other name is pushed as
// "other", so the label stays bounded.
//
//   - Cortex's own operators: tool-prune (its plugin name), keeper (the keep-alive ping's spend,
//     KeeperOperator), model (cortex.model, the setting op of a model change no plugin claimed),
//     inference-router (the plugin that rewrites the model), and context-guru (the plugin name
//     its own model calls are stamped with).
//   - context-guru's components as of v0.7.4-beta.1: the names its registry lists
//     (components.Names(), which context-guru's own push passes to WithComponents), plus the
//     operators Cortex's envelope ops and context-guru's traces report under names of their own.
//
// core/accounting may not import context-guru (global constraint), so the list is copied here,
// and core/plugins/contextguru's TestComponentsCoverTheRegistry fails when a context-guru bump
// adds a component this list does not hold.
var Components = []string{
	// Cortex
	"tool-prune", KeeperOperator, "model", "inference-router", "context-guru",
	// context-guru registry (components.Names() at v0.7.4-beta.1)
	"agentdiet", "cache_aware_summarizer", "cacheinject", "cachesplit", "cmdfilter", "collapse", "coref",
	"dedup", "dropex", "dropfirst", "extract", "extract_llm", "extract_llm_sweep", "failed_run", "format",
	"keeptail2", "linecap", "mask", "midsum", "readlifecycle", "searchfold", "skeleton", "smartcrush",
	"summarize", "testreorder", "testrevert", "testrewrite", "testrewrite2", "textclean", "toolfilter",
	"toolschema", "toon", "usershrink",
	// context-guru operators outside the registry (envelope and trace ops)
	"adjudicate", "headttl", "tailbreakpoint", "toolcut", "tooldefer", "expand", "expand_restore",
}

// The outcome label of llm.keepalive.pings, with context-guru's values (its observePing), so the
// central dashboard stacks the two proxies on one axis.
const (
	PingOK      = "ok"      // the provider answered 2xx
	PingFailed  = "failed"  // a status outside 200-299, or no response at all (a transport error)
	PingTimeout = "timeout" // the ping ran to the keeper's timeout with no answer (#754)
)

// The cause label of llm.cache.misses and llm.cache.miss_cost_usd, with context-guru's values
// (its missCause). Cortex cannot see the body diffs behind context-guru's prefix_change,
// compaction_after_rewrite and bypass_flip, so such a miss is "other" here, not a guess.
const (
	MissColdStart    = "cold_start"
	MissTTLExpiry    = "ttl_expiry"
	MissModelChanged = "model_changed"
	MissOther        = "other"
)

// missCause says why a request that read nothing from the cache missed, in the order of the
// shared metrics contract (context-guru docs/metrics/spec.md 5.4), so both proxies label one miss
// the same way:
//
//   - cold_start: the thread's first request. The library leaves the phase unknown when it has
//     no previous request, and firstOnThread is Cortex's own record of that. Both must hold: after
//     a restart Cortex has forgotten the thread, but the restored library state gives a hot or
//     cold phase, and that request is not a cold start.
//   - ttl_expiry: the library's phase is cold. It sets cold only when the clock shows the
//     entry the previous request wrote has expired (accountant §5.2). An expiry wins over a model
//     change on the same request.
//   - model_changed: a break the library attributes to a model change (CauseModelChanged), or
//     one a plugin's model change caused (a modelChangeOp's ChangeID). Only when the request wrote
//     cache tokens (wroteCache): the spec's later causes need a written prefix to explain.
//   - other: anything else, such as an entry lost early or a prefix change, which Cortex cannot
//     see.
func missCause(res accountant.Result, firstOnThread, wroteCache bool) string {
	switch {
	case res.Phase == accountant.PhaseUnknown && firstOnThread:
		return MissColdStart
	case res.Phase == accountant.PhaseCold:
		return MissTTLExpiry
	}
	if !wroteCache {
		return MissOther
	}
	for _, e := range res.CacheEffects {
		if e.Kind != accountant.EffectBreak {
			continue
		}
		if e.Cause == accountant.CauseModelChanged || strings.HasPrefix(e.ChangeID, modelChangePrefix) {
			return MissModelChanged
		}
	}
	return MissOther
}

// Sink hands each computed Result to accountant/metrics, which adds it to the llm.savings.*
// counters (design §5.10). It makes no network call: the counters are read by the
// MeterProvider's own reader (core/accounting/push), on its own goroutine.
//
// The library's Pricer has no endpoint and no prompt total (accountant/docs/metrics.md), and
// Cortex's rates depend on both. So the pricer is bound to one observation at a time: Observe
// holds mu, sets that observation's bindings on the pricer, calls the library's Observe, and
// releases mu (design §5.6). The library's Observe only adds to in-memory counters, so the lock
// is held for microseconds.
//
// The pricer is given only a model, so the bindings are keyed by model (bindingsFor): the
// request's model prices at the request's endpoint and prompt total, as the ledger's savings
// do, and a spend call's model at the call's own host and prompt total, as the ledger's
// spends[] do (CallUSD).
type Sink struct {
	mu     sync.Mutex
	pricer *boundPricer
	sink   *acmetrics.Sink

	// Tier 1 instruments: llm.inference.*, llm.keepalive.*, llm.cache.* — pushed alongside
	// llm.savings.* on the same meter, read directly from the request's accountant.Usage and
	// accountant.Result rather than through the acmetrics library (which only knows savings).
	inferenceTokens       metric.Int64Counter
	inferenceCostUSD      metric.Float64Counter
	inferenceEstimatedUSD metric.Float64Counter
	keepalivePings        metric.Int64Counter
	keepaliveCostUSD      metric.Float64Counter
	keepaliveEstimatedUSD metric.Float64Counter
	cacheMisses           metric.Int64Counter
	cacheMissCostUSD      metric.Float64Counter
}

// NewSink builds the sink on mp, priced with r through USD, the function the ledger prices with.
// It returns nil, a no-op sink, when mp is nil (no push configured) or the meter refuses an
// instrument. Every instrument must be created or none is: a Sink with some Tier 1 counters
// missing would make Observe branch on which fields are non-nil instead of failing closed like
// the rest of this constructor.
func NewSink(mp metric.MeterProvider, r pricing.Resolver) *Sink {
	if mp == nil {
		return nil
	}
	p := &boundPricer{resolver: r}
	sink, err := acmetrics.New(mp.Meter(MeterName), p, acmetrics.WithComponents(Components...))
	if err != nil {
		slog.Warn("accounting: the savings counters could not be created; no savings metrics are pushed", "error", err)
		return nil
	}
	meter := mp.Meter(MeterName)
	s := &Sink{pricer: p, sink: sink}
	instruments := []struct {
		name string
		new  func() error
	}{
		{"llm.inference.tokens", func() (err error) {
			s.inferenceTokens, err = meter.Int64Counter("llm.inference.tokens",
				metric.WithDescription("Provider-billed tokens per tier (prompt tiers and output), for non-ping inference requests."))
			return err
		}},
		{"llm.inference.cost_usd", func() (err error) {
			// No metric.WithUnit: setting it to "USD" makes the Prometheus remote-write
			// exporter append the unit, turning this into llm_inference_cost_usd_USD_total
			// — a name no dashboard queries. llm.savings.* sets none either, for the same
			// reason.
			s.inferenceCostUSD, err = meter.Float64Counter("llm.inference.cost_usd",
				metric.WithDescription("Provider cost in USD of non-ping inference requests, every tier."))
			return err
		}},
		{"llm.inference.estimated_cost_usd", func() (err error) {
			s.inferenceEstimatedUSD, err = meter.Float64Counter("llm.inference.estimated_cost_usd",
				metric.WithDescription("Part of llm.inference.cost_usd priced from a fallback (bundled/unknown) rate."))
			return err
		}},
		{"llm.keepalive.pings", func() (err error) {
			s.keepalivePings, err = meter.Int64Counter("llm.keepalive.pings",
				metric.WithDescription("Keep-alive pings the keeper reported, by outcome (ok, failed, timeout)."))
			return err
		}},
		{"llm.keepalive.cost_usd", func() (err error) {
			s.keepaliveCostUSD, err = meter.Float64Counter("llm.keepalive.cost_usd",
				metric.WithDescription("Provider cost in USD of keep-alive pings, every tier."))
			return err
		}},
		{"llm.keepalive.estimated_cost_usd", func() (err error) {
			s.keepaliveEstimatedUSD, err = meter.Float64Counter("llm.keepalive.estimated_cost_usd",
				metric.WithDescription("Part of llm.keepalive.cost_usd that is not measured: priced from a fallback (bundled/unknown) rate, or the keeper's projection for a ping that timed out."))
			return err
		}},
		{"llm.cache.misses", func() (err error) {
			s.cacheMisses, err = meter.Int64Counter("llm.cache.misses",
				metric.WithDescription("Non-ping inference requests with no cache-read tokens, by cause (cold_start, ttl_expiry, model_changed, other)."))
			return err
		}},
		{"llm.cache.miss_cost_usd", func() (err error) {
			s.cacheMissCostUSD, err = meter.Float64Counter("llm.cache.miss_cost_usd",
				metric.WithDescription("Write premium of cache-miss requests (docs/metrics/spec.md 5.4): the written prefix priced above a read, by cause."))
			return err
		}},
	}
	for _, inst := range instruments {
		if err := inst.new(); err != nil {
			slog.Warn("accounting: an inference metric could not be created; no inference metrics are pushed",
				"instrument", inst.name, "error", err)
			return nil
		}
	}
	return s
}

// Observe adds one computed request, or one ping (an empty RequestID), to the counters. output is
// the response's generated tokens: accountant.Usage holds only the prompt tiers, so the output
// tier reaches the Tier 1 token and cost counters through this argument. endpoint and
// promptTotal are the ones the ledger priced the request at: the server that billed (after any
// redirect) and the request's whole prompt. A panic in the library or the pricer is
// recovered and logged: it costs this observation and nothing else. Safe on a nil Sink.
//
// firstOnThread says the request is its thread's first that the Host has seen (missCause). It is
// ignored for a ping.
//
// Money moves only for meta.Billing.Mode == api; the library decides that, not this sink.
func (s *Sink) Observe(meta accountant.RequestMeta, u accountant.Usage, output int, res accountant.Result, endpoint string, promptTotal int, firstOnThread bool) {
	if s == nil || s.sink == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			slog.Error("accounting: the savings metrics panicked; this request books no metric", "panic", r)
		}
	}()
	s.pricer.request = binding{endpoint: endpoint, promptTotal: promptTotal}
	s.pricer.byModel = bindingsFor(meta.Model, s.pricer.request, res)
	s.sink.Observe(acmetrics.Observation{Meta: meta, Usage: u, Result: res})
	s.observeInferenceMetrics(meta, u, output, missCause(res, firstOnThread, u.CacheWrite > 0))
}

// CountPing counts a ping the Host refused to price (Host.Ping): one llm.keepalive.pings with
// outcome. projectedUSD is the keeper's projected cost of a ping with no answer; it is booked as
// cost and, all of it, as estimated cost, because the provider probably billed the ping but
// nobody measured it. 0 books no money. A priced ping is counted by Observe instead, never here
// too. Safe on a nil Sink.
func (s *Sink) CountPing(model, outcome string, projectedUSD float64) {
	if s == nil || s.sink == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Error("accounting: counting a ping panicked; it books no metric", "panic", r)
		}
	}()
	ctx := context.Background()
	modelAttr := attribute.String("model", model)
	s.keepalivePings.Add(ctx, 1, metric.WithAttributes(modelAttr, attribute.String("outcome", outcome)))
	if projectedUSD > 0 {
		s.keepaliveCostUSD.Add(ctx, projectedUSD, metric.WithAttributes(modelAttr))
		s.keepaliveEstimatedUSD.Add(ctx, projectedUSD, metric.WithAttributes(modelAttr))
	}
}

// observeInferenceMetrics adds one observation to the Tier 1 counters: llm.inference.* for a
// non-ping request, llm.keepalive.* for a ping (meta.RequestID == ""). Called under s.mu, after
// s.pricer.request/byModel are set for this observation, so priceUsageSplit prices at the same
// endpoint and prompt total the ledger and llm.savings.* use.
//
// The cost counters follow the shared metrics contract (context-guru docs/metrics/spec.md 5.1),
// which every proxy pushing to the central stack honours: cost_usd is the request's WHOLE price,
// every tier including output, and estimated_cost_usd is the part of it priced from a fallback
// rate — a subset of cost_usd, never an addition to it. So total spend is cost_usd alone, and the
// fallback share is estimated_cost_usd / cost_usd. The keep-alive pair follows the same rule.
//
// "Estimated" is USD's own per-tier signal (a bundled or unknown rate, or a 1h write priced at
// the 5m fallback) — not res.Unpriced, which is the accountant library's "this savings op could
// not be priced" marker and says nothing about this request's own price. So a request with one
// estimated tier and three priced ones books only that tier's dollars as estimated.
func (s *Sink) observeInferenceMetrics(meta accountant.RequestMeta, u accountant.Usage, output int, cause string) {
	ctx := context.Background()
	modelAttr := attribute.String("model", meta.Model)
	actual, estimated := s.priceUsageSplit(meta.Model, u, output)
	total := actual + estimated

	if meta.RequestID == "" {
		// A ping reaches here only once Host.Ping has priced it, which needs a 2xx answer.
		s.keepalivePings.Add(ctx, 1, metric.WithAttributes(modelAttr, attribute.String("outcome", PingOK)))
		if total > 0 {
			s.keepaliveCostUSD.Add(ctx, total, metric.WithAttributes(modelAttr))
		}
		if estimated > 0 {
			s.keepaliveEstimatedUSD.Add(ctx, estimated, metric.WithAttributes(modelAttr))
		}
		return
	}

	cacheWrite5m := u.CacheWrite - u.CacheWrite1h
	tiers := [...]struct {
		name string
		n    int
	}{
		{"input", u.Input}, {"cache_read", u.CacheRead}, {"cache_write_5m", cacheWrite5m}, {"cache_write_1h", u.CacheWrite1h},
		{"output", output},
	}
	for _, t := range tiers {
		if t.n > 0 {
			s.inferenceTokens.Add(ctx, int64(t.n), metric.WithAttributes(modelAttr, attribute.String("tier", t.name)))
		}
	}
	if total > 0 {
		s.inferenceCostUSD.Add(ctx, total, metric.WithAttributes(modelAttr))
	}
	if estimated > 0 {
		s.inferenceEstimatedUSD.Add(ctx, estimated, metric.WithAttributes(modelAttr))
	}
	if u.CacheRead == 0 {
		missAttrs := metric.WithAttributes(modelAttr, attribute.String("cause", cause))
		s.cacheMisses.Add(ctx, 1, missAttrs)
		if usd := s.missPremiumUSD(meta.Model, u); usd > 0 {
			s.cacheMissCostUSD.Add(ctx, usd, missAttrs)
		}
	}
}

// priceUsageSplit prices u's tiers and output's tokens at s.pricer's current binding (endpoint, promptTotal) for
// model, with the same USD function the ledger and llm.savings.* price with — splitting the
// result by each tier's own `estimated` return rather than combining them into one flag, so a
// request with one estimated tier and three priced ones books most of its cost as actual.
// CacheWrite1h is "the part of CacheWrite billed at the one-hour rate, never an addition to it"
// (accountant.Usage): priced as cacheWrite5m (CacheWrite minus CacheWrite1h) plus CacheWrite1h,
// never as raw CacheWrite plus CacheWrite1h, which would price the 1h tokens twice. Output is
// priced at TierOutput: it is most of the bill on a long answer, and the provider charges it.
func (s *Sink) priceUsageSplit(model string, u accountant.Usage, output int) (actual, estimated float64) {
	price := func(t pricing.Tier, n int) {
		if n <= 0 {
			return
		}
		v, est := USD(s.pricer.resolver, s.pricer.request.endpoint, model, s.pricer.request.promptTotal, t, int64(n))
		if est {
			estimated += v
		} else {
			actual += v
		}
	}
	price(pricing.TierInput, u.Input)
	price(pricing.TierCacheRead, u.CacheRead)
	price(pricing.TierCacheWrite, u.CacheWrite-u.CacheWrite1h)
	price(pricing.TierCacheWrite1h, u.CacheWrite1h)
	price(pricing.TierOutput, output)
	return
}

// missPremiumUSD is what a cache miss cost above a hit (context-guru docs/metrics/spec.md 5.4):
// the prefix it wrote, priced at the write rate minus the read rate it would have paid instead.
//
//	(CacheWrite - CacheWrite1h) x (write_5m - read) + CacheWrite1h x (write_1h - read)
//
// CacheWrite1h is a part of CacheWrite, not an addition to it (see priceUsageSplit), and is
// clamped to it. Each term is never below zero. Output and fresh input are not in it: a hit pays
// them too. The rates are USD's, at s.pricer's current binding, the same as priceUsageSplit's: a
// tier USD fills from a bundled or standard rate is used as it is, and a model with no price at
// all prices every tier at 0, so its miss is counted and books $0.
func (s *Sink) missPremiumUSD(model string, u accountant.Usage) float64 {
	rate := func(t pricing.Tier) float64 {
		v, _ := USD(s.pricer.resolver, s.pricer.request.endpoint, model, s.pricer.request.promptTotal, t, 1)
		return v
	}
	rd := rate(pricing.TierCacheRead)
	w1hTok := min(max(u.CacheWrite1h, 0), u.CacheWrite)
	w5Tok := u.CacheWrite - w1hTok
	var usd float64
	if w5Tok > 0 {
		usd += float64(w5Tok) * max(rate(pricing.TierCacheWrite)-rd, 0)
	}
	if w1hTok > 0 {
		usd += float64(w1hTok) * max(rate(pricing.TierCacheWrite1h)-rd, 0)
	}
	return usd
}

// binding is where and at what prompt size a model's tokens are priced.
type binding struct {
	endpoint    string
	promptTotal int
}

// bindingsFor maps each model the library will price for res onto its binding. The request's
// model gets the request's binding. Each spend the library books — not a dry run, with a Call,
// and not refused in res.Unpriced (accountant/docs/metrics.md, rules 1 and 3) — gets its call's
// model (the request's model when the call names none, as the library prices it) bound to the
// call's host (the request's endpoint when it names none) and the call's own prompt total.
//
// COLLISIONS FALL BACK TO THE REQUEST'S BINDING. The pricer sees a model and nothing else, so one
// model can have only one binding per observation. When a spend's model is the request's own
// model with a different binding, or two spends share a model with different bindings, that
// model is priced at the request's binding, and the pushed spend dollars can differ from the
// ledger's spends[] (which price each call apart). A spend on a model of its own, the common
// case, agrees to the micro.
func bindingsFor(reqModel string, req binding, res accountant.Result) map[string]binding {
	out := map[string]binding{reqModel: req}
	collided := map[string]bool{}
	refused := map[string]bool{}
	for _, u := range res.Unpriced {
		if u.OpID != "" {
			refused[u.OpID] = true
		}
	}
	for _, op := range res.Ops {
		if op.DryRun || op.Kind != accountant.KindSpend || op.Call == nil || refused[op.OpID] {
			continue
		}
		model := op.Call.Model
		if model == "" {
			model = reqModel
		}
		b := binding{endpoint: op.Call.Host, promptTotal: callPromptTotal(op.Call)}
		if b.endpoint == "" {
			b.endpoint = req.endpoint
		}
		if model == reqModel || collided[model] {
			continue // the request's own model keeps its binding; a collided one stays on it
		}
		if prev, ok := out[model]; ok && prev != b {
			out[model] = req
			collided[model] = true
			continue
		}
		out[model] = b
	}
	return out
}

// boundPricer is the acmetrics.Pricer, bound to the observation Sink.Observe is making. Its
// fields are written and read only under Sink.mu.
type boundPricer struct {
	resolver pricing.Resolver
	request  binding
	byModel  map[string]binding
}

// Price is the rate of one token: USD for one token, at the model's binding (bindingsFor), or
// the request's for a model with none. The same function prices the ledger, so the two cannot
// price one token differently.
func (p *boundPricer) Price(model string, _ time.Time, t acmetrics.Tier) (float64, bool) {
	tier, ok := pricingTier(t)
	if !ok {
		// An unknown tier has no rate: (0, true) is "no money, and estimated", which is what
		// the Pricer contract asks for a rate the host does not know.
		return 0, true
	}
	b, ok := p.byModel[model]
	if !ok {
		b = p.request
	}
	return USD(p.resolver, b.endpoint, model, b.promptTotal, tier, 1)
}

// pricingTier maps the library's metrics tier onto Cortex's rate table.
func pricingTier(t acmetrics.Tier) (pricing.Tier, bool) {
	switch t {
	case acmetrics.TierInput:
		return pricing.TierInput, true
	case acmetrics.TierCacheRead:
		return pricing.TierCacheRead, true
	case acmetrics.TierCacheWrite5m:
		return pricing.TierCacheWrite, true
	case acmetrics.TierCacheWrite1h:
		return pricing.TierCacheWrite1h, true
	case acmetrics.TierOutput:
		return pricing.TierOutput, true
	}
	return 0, false
}

// observe gives one Result to the sink, off the caller's goroutine: Compute runs on the response
// path, and the sink's lock is shared by every request. Close waits for these.
func (h *Host) observe(meta accountant.RequestMeta, u accountant.Usage, output int, res accountant.Result, endpoint string, promptTotal int, firstOnThread bool) {
	if h.sink == nil && h.observeHook == nil {
		return
	}
	h.goObserve(func() {
		if h.observeHook != nil {
			h.observeHook(meta)
		}
		h.sink.Observe(meta, u, output, res, endpoint, promptTotal, firstOnThread)
	})
}

// countPing counts a ping Host.Ping refused to price (Sink.CountPing), off the keeper's goroutine
// like observe. It is not an observation of a Result, so observeHook does not see it.
func (h *Host) countPing(model, outcome string, projectedUSD float64) {
	if h.sink == nil {
		return
	}
	h.goObserve(func() { h.sink.CountPing(model, outcome, projectedUSD) })
}

// goObserve runs fn on a goroutine of its own that Close waits for, unless Close has started.
func (h *Host) goObserve(fn func()) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.observing.Add(1)
	h.mu.Unlock()
	go func() {
		defer h.observing.Done()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("accounting: observing a request panicked; it books no metric", "panic", r)
			}
		}()
		fn()
	}()
}
