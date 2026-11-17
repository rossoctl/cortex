package accounting

import (
	"log/slog"
	"sync"
	"time"

	"github.com/rossoctl/accountant"
	acmetrics "github.com/rossoctl/accountant/metrics"
	"github.com/rossoctl/cortex/core/cost/pricing"
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
}

// NewSink builds the sink on mp, priced with r through USD, the function the ledger prices with.
// It returns nil, a no-op sink, when mp is nil (no push configured) or the meter refuses an
// instrument.
func NewSink(mp metric.MeterProvider, r pricing.Resolver) *Sink {
	if mp == nil {
		return nil
	}
	p := &boundPricer{resolver: r}
	s, err := acmetrics.New(mp.Meter(MeterName), p, acmetrics.WithComponents(Components...))
	if err != nil {
		slog.Warn("accounting: the savings counters could not be created; no savings metrics are pushed", "error", err)
		return nil
	}
	return &Sink{pricer: p, sink: s}
}

// Observe adds one computed request, or one ping (an empty RequestID), to the counters. endpoint
// and promptTotal are the ones the ledger priced the request at: the server that billed (after
// any redirect) and the request's whole prompt. A panic in the library or the pricer is
// recovered and logged: it costs this observation and nothing else. Safe on a nil Sink.
//
// Money moves only for meta.Billing.Mode == api; the library decides that, not this sink.
func (s *Sink) Observe(meta accountant.RequestMeta, u accountant.Usage, res accountant.Result, endpoint string, promptTotal int) {
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
func (h *Host) observe(meta accountant.RequestMeta, u accountant.Usage, res accountant.Result, endpoint string, promptTotal int) {
	if h.sink == nil && h.observeHook == nil {
		return
	}
	h.observing.Add(1)
	go func() {
		defer h.observing.Done()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("accounting: observing a request panicked; it books no metric", "panic", r)
			}
		}()
		if h.observeHook != nil {
			h.observeHook(meta)
		}
		h.sink.Observe(meta, u, res, endpoint, promptTotal)
	}()
}
