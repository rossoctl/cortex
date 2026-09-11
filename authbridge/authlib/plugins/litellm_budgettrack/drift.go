package litellm_budgettrack

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/rossoctl/cortex/authbridge/authlib/costing"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/pricing"
)

// driftTolerance is how far the modelled cost may sit from the gateway's own figure
// before it is worth saying so.
//
// 5% absorbs rounding and the micro quantization while still catching every mistake
// that matters: a forgotten gateway discount is 32% off, a missing multiplier or a
// decimal-place slip is a factor of ten or a hundred. Anything inside 5% is not
// actionable and would train an operator to ignore the line.
const driftTolerance = 0.05

// maxDriftKeys bounds the dedup set.
//
// Both halves of the key come off the request — Host is a client-supplied header, the
// model is a body field — so an unbounded map grows for as long as a caller varies them,
// in a process designed to run for weeks, to serve a diagnostic. 256 distinct
// endpoint/model pairs is far past any real deployment and long past the point where the
// operator has been told their rate table is wrong.
//
// At the cap reporting STOPS rather than dropping the dedup: continuing to warn without
// dedup would turn a bounded-memory problem into an unbounded-log one.
const maxDriftKeys = 256

// driftReporter warns, once per endpoint and model, when the rate table disagrees with
// what the gateway actually charged.
//
// This exists because the information was already on the wire and nobody looked. A
// non-streamed response carries BOTH the gateway's settled cost and the token counts,
// so the modelled figure can be checked against the real one for free — and for months
// a gateway billing 0.76x vendor list was reported at list, with the discrepancy
// visible in every such response and no signal anywhere.
//
// Once per (endpoint, model), not per request: an agent makes thousands of calls, and a
// per-request warning would bury every other line in the log and get filtered out.
type driftReporter struct {
	mu     sync.Mutex
	seen   map[string]struct{}
	capped bool
	log    *slog.Logger
}

func (d *driftReporter) logger() *slog.Logger {
	if d.log != nil {
		return d.log
	}
	return slog.Default()
}

// SetDriftLogger overrides the logger used for drift warnings. For tests.
func (p *BudgetTrack) SetDriftLogger(l *slog.Logger) {
	p.drift.mu.Lock()
	p.drift.log = l
	p.drift.mu.Unlock()
}

// checkDrift compares the gateway's own cost against what the rate table modelled, and
// warns on a material divergence.
//
// Reads BOTH figures off the settled outcome rather than recomputing either. That is the
// point of the outcome carrying the pair: a drift check that re-derives the modelled figure
// is comparing its own arithmetic, not the arithmetic that was actually used, and the two
// can drift apart without anything failing.
//
// Called only where an authoritative figure exists — a non-streamed response with a usable
// cost header. A streamed response reports 0 there by design, so there is nothing to compare
// and silence is correct.
//
// The ledger keeps using the authoritative figure regardless: drift is a diagnostic about the
// rate TABLE, never a reason to distrust the gateway's own number.
func (p *BudgetTrack) checkDrift(pctx *pipeline.Context, settled costing.Settled) {
	authoritative := settled.CostUSD
	if authoritative <= 0 || !settled.HasModelled {
		return // unpriced by the table is a coverage gap, already reported as one
	}
	inf := pctx.Extensions.Inference
	if inf == nil || inf.Model == "" {
		return
	}
	modelled, prov := settled.ModelledUSD, settled.ModelledProv
	ratio := modelled / authoritative
	// Inclusive bounds: the contract above says "more than 5%", and a strict compare
	// warned AT exactly 5%.
	if ratio >= 1-driftTolerance && ratio <= 1+driftTolerance {
		return
	}
	// Only compare figures that mean the same thing. See the header block in plugin.go:
	// the "-original" fallback is the charged cost on a gateway that runs no LiteLLM
	// discount/margin layer, but where one IS configured it is the figure BEFORE that
	// layer, and comparing a modelled cost against it would report drift that is really
	// the operator's own gateway-side adjustment. Skipped rather than guessed at,
	// because the arithmetic relating the two is not something this code can verify.
	if !costing.Comparable(pctx) {
		return
	}

	// Normalized the SAME way resolution normalizes, via the pricing package rather
	// than a local copy. Keying on the raw Host gave "GW.internal:443" and "gw.internal"
	// separate entries for one endpoint, so the same drift could warn repeatedly and the
	// key budget drained faster than the cap implies.
	key := pricing.EndpointKey(pctx.Host) + "\x00" + strings.ToLower(inf.Model)
	p.drift.mu.Lock()
	if p.drift.seen == nil {
		p.drift.seen = map[string]struct{}{}
	}
	if _, dup := p.drift.seen[key]; dup {
		p.drift.mu.Unlock()
		return
	}
	if len(p.drift.seen) >= maxDriftKeys {
		alreadyCapped := p.drift.capped
		p.drift.capped = true
		log := p.drift.logger()
		p.drift.mu.Unlock()
		if !alreadyCapped {
			// Said once, because going quiet without a word would read as "the drift
			// stopped" — the opposite of what happened.
			log.Warn("pricing: no longer reporting rate-table drift",
				"reason", fmt.Sprintf("hit the %d endpoint/model limit", maxDriftKeys),
				"fix", "`abctl pricing --host <endpoint>` still shows the rates in effect")
		}
		return
	}
	p.drift.seen[key] = struct{}{}
	log := p.drift.logger()
	p.drift.mu.Unlock()

	direction := "over"
	if ratio < 1 {
		direction = "under"
	}
	log.Warn("pricing: the rate table disagrees with what the gateway charged",
		"endpoint", pctx.Host,
		"model", inf.Model,
		"modelled_usd", fmt.Sprintf("%.6f", modelled),
		"gateway_usd", fmt.Sprintf("%.6f", authoritative),
		"ratio", fmt.Sprintf("%.3fx", ratio),
		"effect", direction+"stating every request this table prices, including streamed ones where no gateway figure exists",
		"rates_from", prov.String(),
		"fix", "set pricing.endpoints[].multiplier for this endpoint (a fraction of list), or per-model rates; `abctl pricing --host <endpoint>` shows what is in effect")
}

// resetDrift clears the dedup set and the cap.
//
// Called on Configure, which is also the hot-reload path: a reload is an operator having
// changed something, quite possibly the multiplier this check told them to set, so the
// old "already warned about that" state is stale. It is also the only way out of the cap
// short of a restart, which answers the note that a capped reporter otherwise stays off
// for the life of the process.
func (p *BudgetTrack) resetDrift() {
	p.drift.mu.Lock()
	p.drift.seen = nil
	p.drift.capped = false
	p.drift.mu.Unlock()
}
