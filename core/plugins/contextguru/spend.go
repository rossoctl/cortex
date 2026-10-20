package contextguru

import (
	"net/url"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	cgcomponents "github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/cost/spend"
	"github.com/rossoctl/cortex/core/pipeline"
)

// SetCostRecorder receives where this plugin records the money it spends outside the pipeline:
// keep-alive pings and the engine's model calls (plugins.Deps, before Configure). Nil-safe: with
// no recorder both are only logged.
func (p *ContextGuru) SetCostRecorder(r spend.CostRecorder) { p.costs = r }

// chargeTo is who a call made on a request's behalf is charged to: the session it served and the
// agent that sent the request. nil means nothing records, which every caller treats as "log only".
type chargeTo struct {
	costs   spend.CostRecorder
	session string
	client  *pipeline.EventClient
}

// chargeFor is the attribution for calls made on behalf of pctx, or nil when there is no recorder
// or no session to charge.
func (p *ContextGuru) chargeFor(pctx *pipeline.Context, sid string) *chargeTo {
	if p.costs == nil || sid == "" {
		return nil
	}
	return &chargeTo{costs: p.costs, session: sid, client: pctx.ClientInfo()}
}

// record charges one call. A call that reported no tokens spent nothing anyone can price, so it
// is not recorded.
func (t *chargeTo) record(purpose, host, model string, u pricing.Usage, status int, at time.Time, d time.Duration) {
	if t == nil || u == (pricing.Usage{}) {
		return
	}
	t.costs.RecordCost(spend.Charge{
		Plugin: "context-guru", Purpose: purpose, Session: t.session, Client: t.client,
		Host: host, Model: model, Usage: u, Status: status, At: at, Duration: d,
	})
}

// modelsFor is the engine's model clients for one request, each charging its calls to that
// request's session. Built per request rather than shared, because a detached summarizer call can
// finish after the request is answered and must still be charged to the session that caused it.
//
// An absent client stays a nil interface, never a typed nil: the engine checks the interface.
func (p *ContextGuru) modelsFor(pctx *pipeline.Context, provider bschemas.ModelProvider, api, sid string) cgcomponents.ModelSpec {
	to := p.chargeFor(pctx, sid)
	var spec cgcomponents.ModelSpec
	if p.static != nil {
		spec.Static = p.static.chargedTo(to)
	}
	if provider == bschemas.Anthropic {
		if m, ok := p.anthropicIncoming(pctx, to); ok {
			spec.Incoming = m
		}
	} else if m := p.incomingModel(pctx, provider, api); m != nil {
		spec.Incoming = m.chargedTo(to)
	}
	return spec
}

// pingEmitter is the keep-alive emitter for the entry pctx establishes. Each keeper Record
// carries its own emitter, so the ping knows the session, agent and endpoint it is charged to.
func (p *ContextGuru) pingEmitter(pctx *pipeline.Context, sid string) cgcomponents.Emitter {
	return pingEmitter{to: p.chargeFor(pctx, sid), host: pctx.Host}
}

// pingEmitter logs each ping, as logEmitter does, and records its cost.
type pingEmitter struct {
	logEmitter
	to   *chargeTo
	host string
}

func (e pingEmitter) KeepAlivePing(r cgcomponents.KeepAliveReport) {
	e.logEmitter.KeepAlivePing(r)
	// CacheWrite includes the one-hour part (CacheWrite1h); pricing.Usage has one cache-write tier.
	u := pricing.Usage{Input: int(r.FreshInput), CacheWrite: int(r.CacheWrite),
		CacheRead: int(r.CacheRead), Output: int(r.Output)}
	at := time.Now()
	if r.TS > 0 {
		at = time.UnixMilli(r.TS)
	}
	e.to.record(spend.PurposeKeepAlive, e.host, r.Model, u, r.Status, at,
		time.Duration(r.DurationMs*float64(time.Millisecond)))
}

// hostOf is the host part of a base URL, the endpoint a static model's calls are billed at.
func hostOf(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	return u.Host
}
