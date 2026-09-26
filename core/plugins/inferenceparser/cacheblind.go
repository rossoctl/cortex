package inferenceparser

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/rossoctl/cortex/core/costing"
	"github.com/rossoctl/cortex/core/pipeline"
	"github.com/rossoctl/cortex/core/pricing"
)

// maxCacheBlindKeys bounds the dedup set, for the reason litellm-budget-track's drift reporter
// bounds its own: both halves of the key come off the request, so an unbounded map grows for as
// long as a caller varies them, in a process designed to run for weeks, to serve a diagnostic.
const maxCacheBlindKeys = 256

// cacheBlindReporter says, once per endpoint and model, that this gateway's cost header prices
// only the uncached tiers — so the rate table was charged instead of it.
//
// WHY THIS LIVES WITH THE COST OWNER and not with the budget plugin's drift reporter, which
// asks a neighbouring question: litellm-budget-track is opt-in — it needs a spend_file and a
// max_budget — so the default pipeline (parsers plus tool-prune, which is what `--local` builds)
// carries no drift check at all. That is how a gateway whose header omitted cache cost went
// unreported for as long as it did: the information was on the wire, the one component that
// looks at it was not installed, and the figures it produced were quietly low. Everything that
// prices a request runs this parser, so a signal here reaches every deployment.
//
// Once per (endpoint, model), not per request: an agent makes thousands of calls, and a
// per-request warning would bury every other line in the log and get filtered out.
type cacheBlindReporter struct {
	mu     sync.Mutex
	seen   map[string]struct{}
	capped bool
	log    *slog.Logger
}

func (c *cacheBlindReporter) logger() *slog.Logger {
	if c.log != nil {
		return c.log
	}
	return slog.Default()
}

// setCacheBlindLogger overrides the logger used for these warnings. For tests.
func (p *InferenceParser) setCacheBlindLogger(l *slog.Logger) {
	p.blind.mu.Lock()
	p.blind.log = l
	p.blind.mu.Unlock()
}

// reportCacheBlind warns when costing.Settle refused a cache-blind header for this
// endpoint and model.
//
// The decision itself is costing's — see Settled.HeaderOmittedCache — and is not re-derived
// here: a reporter that recomputed the predicate would be describing its own arithmetic rather
// than the substitution that actually happened, and the two could drift apart with nothing
// failing.
//
// Deliberately NOT advising a pricing change, which is the trap. The neighbouring drift warning
// ends with "set pricing.endpoints[].multiplier for this endpoint", and that advice is exactly
// wrong here: the table is the figure that matched the gateway's own spend records, and an
// operator who "corrected" it would break the 65% of responses whose headers agree with it
// exactly. What this line reports is the gateway under-reporting, and the only action is to
// know that the charged figure came from the table.
func (p *InferenceParser) reportCacheBlind(pctx *pipeline.Context, settled costing.Settled) {
	if !settled.HeaderOmittedCache || pctx == nil {
		return
	}
	inf := pctx.Extensions.Inference
	if inf == nil || inf.Model == "" {
		return
	}
	// Normalized the same way rate resolution normalizes, via the pricing package rather than
	// a local copy, so one endpoint cannot occupy two keys as "GW.internal:443" and
	// "gw.internal" — which would let the same warning repeat and drain the key budget.
	key := pricing.EndpointKey(pctx.Host) + "\x00" + strings.ToLower(inf.Model)
	p.blind.mu.Lock()
	if p.blind.seen == nil {
		p.blind.seen = map[string]struct{}{}
	}
	if _, dup := p.blind.seen[key]; dup {
		p.blind.mu.Unlock()
		return
	}
	if len(p.blind.seen) >= maxCacheBlindKeys {
		alreadyCapped := p.blind.capped
		p.blind.capped = true
		log := p.blind.logger()
		p.blind.mu.Unlock()
		if !alreadyCapped {
			// Said once, because going quiet without a word would read as "it stopped
			// happening" — the opposite of what happened.
			log.Warn("pricing: no longer reporting gateway figures that omit cache cost",
				"reason", fmt.Sprintf("hit the %d endpoint/model limit", maxCacheBlindKeys))
		}
		return
	}
	p.blind.seen[key] = struct{}{}
	log := p.blind.logger()
	p.blind.mu.Unlock()

	log.Warn("pricing: the gateway's cost header omits cache cost, so the rate table was charged",
		"endpoint", pctx.Host,
		"model", inf.Model,
		"header_usd", fmt.Sprintf("%.6f", settled.ReportedUSD),
		"charged_usd", fmt.Sprintf("%.6f", settled.CostUSD),
		"effect", "the header priced input and output only; every cached token on this endpoint would otherwise be recorded as free",
		"action", "none on the rate table — it is the figure that matches this gateway's own spend records")
}
