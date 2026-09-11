// Package litellm_budgettrack provides a pipeline plugin that tracks
// per-request cost and enforces a daily spending budget, rejecting requests
// with HTTP 429 when the budget is exceeded.
//
// Cost is resolved in two ways:
//
//   - Non-streaming responses carry the cost in a response header
//     (x-litellm-response-cost, or the pre-discount -original variant), read
//     on the terminal frame.
//
//   - Streaming responses (text/event-stream — what Claude Code's /v1/messages
//     uses) report cost 0 in the header because the total is not known when the
//     headers are sent. That zero is a placeholder, not an answer. For these the
//     plugin prices the per-tier token counts inference-parser publishes, using the
//     top-level `pricing:` section.
//
//     The plugin no longer carries rate options of its own. input_cost_per_token
//     and its three siblings were REMOVED, and a config still setting them now
//     fails to start with the field named — see docs/litellm-budgettrack-plugin.md.
//     It also requires inference-parser LATER in the chain, because the response
//     pass runs in reverse; see Capabilities.
package litellm_budgettrack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/costevent"
	"github.com/rossoctl/cortex/authbridge/authlib/costing"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/plugins"
)

type budgetTrackConfig struct {
	SpendFile string  `json:"spend_file" required:"true" description:"Path to the JSON spend ledger file."`
	MaxBudget float64 `json:"max_budget" required:"true" description:"Daily budget in USD."`

	// There are deliberately no rate knobs here any more.
	//
	// Four of them (input / output / cache_write / cache_read per token) used to
	// price streamed responses, duplicating both the rates in tool-prune's config
	// and the token parser in inference-parser. Rates now come from the top-level
	// `pricing:` section via authlib/pricing, which also gives them endpoint
	// scoping — the same model bills differently per gateway, and a per-plugin
	// table had no way to say so.
}

// stateKey names the per-request scratch holding the settle guard.
const stateKey = "litellm-budget-track"

// settleState records that the terminal frame already priced this request.
//
// It no longer accumulates token counts. The plugin used to run its own SSE parser
// to gather them, duplicating inference-parser frame for frame; it now reads the
// counts that parser publishes, so all that is left to remember is exactly-once.
type settleState struct {
	settled bool
}

// The per-response cost event this plugin publishes lives in authlib/costevent:
// the usage aggregator and abctl both decode it, so the shape belongs where all
// three can share one declaration rather than in this package.

type spendLedger struct {
	Date       string  `json:"date"`
	TotalSpend float64 `json:"total_spend"`
	TotalCalls int     `json:"total_calls"`
}

// BudgetTrack keeps the daily spend ledger and refuses requests once the budget is spent.
//
// It does NOT decide what a request cost. That is authlib/costing, driven by
// inference-parser — the component that knows when token counts are final — and this plugin
// bills the figure that was published. Before cortex #972 both jobs lived here, which meant
// a pipeline without this plugin had no authoritative figure at all and every cost silently
// became a modelled one.
type BudgetTrack struct {
	cfg    budgetTrackConfig
	mu     sync.Mutex
	ledger spendLedger

	// drift warns when the rate table disagrees with what the gateway charged. It stays
	// here rather than moving with the arithmetic because the dedup, the cap and the
	// operator advice are policy, and a body parser is the wrong home for policy.
	drift driftReporter
}

// New creates an unconfigured BudgetTrack plugin instance.
func New() *BudgetTrack { return &BudgetTrack{} }

func init() {
	plugins.RegisterPlugin("litellm-budget-track", func() pipeline.Plugin { return New() })
}

func (p *BudgetTrack) Name() string { return "litellm-budget-track" }

func (p *BudgetTrack) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{
		// ReadsBody: the plugin parses the response body (streamed usage). It
		// makes Pipeline.NeedsBody() true so the extproc (envoy-sidecar) listener
		// buffers the response body and takes its body-phase branch; without it
		// that listener dispatches a single header-only RunResponseFrame and the
		// streamed accounting silently records nothing (or double-charges if
		// Envoy is statically configured BUFFERED). The proxy listeners gate on
		// HasStreamingResponders() and are unaffected. Mirrors inference-parser.
		ReadsBody: true,
		// RequiresLater, NOT Requires — the direction is the whole point.
		//
		// This plugin prices the per-tier token counts inference-parser publishes
		// while folding response frames. The response passes walk the chain in
		// REVERSE (pipeline.RunResponseFrame), so the parser must sit at a HIGHER
		// index to fold each frame before this plugin settles the cost on the
		// terminal one. Declaring Requires would enforce the opposite order and
		// leave every streamed response unpriced, with nothing reporting that the
		// counts were missed.
		//
		// BREAKING: a pipeline listing litellm-budget-track without
		// inference-parser AFTER it now fails to build. Needs a release note.
		RequiresLater: []string{"inference-parser"},
		Description:   "Track LLM cost (response header or priced token usage) and enforce a daily budget.",
	}
}

func (p *BudgetTrack) Configure(raw json.RawMessage) error {
	// DisallowUnknownFields, matching tool-prune's Configure. Without it the four
	// rate knobs this plugin used to accept — input_cost_per_token and friends —
	// were silently dropped from an existing config: no error, no warning, no log
	// line, and the deployment switched to bundled vendor-list rates, which by this
	// change's own accounting OVERSTATES a discounted gateway. An operator would
	// see their cost figures move and have nothing pointing at the cause.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p.cfg); err != nil {
		return fmt.Errorf("litellm-budget-track config: %w (rates moved to the top-level `pricing:` section; see docs/litellm-budgettrack-plugin.md)", err)
	}
	// Decode stops at the first JSON value, so trailing content was silently
	// accepted — a regression from json.Unmarshal, which rejected it. Switching to a
	// decoder to gain DisallowUnknownFields must not loosen this.
	if dec.More() {
		return fmt.Errorf("litellm-budget-track config: unexpected trailing content after the config object")
	}
	if p.cfg.SpendFile == "" {
		return fmt.Errorf("litellm-budget-track: spend_file is required")
	}
	if p.cfg.MaxBudget <= 0 {
		return fmt.Errorf("litellm-budget-track: max_budget must be > 0")
	}
	p.loadLedger()
	// Configure is also the hot-reload path, so drift's "already said that" state is
	// cleared here: the reload may well BE the operator acting on a drift warning.
	p.resetDrift()
	return nil
}

// OnRequest checks if the daily budget has been exceeded before allowing the request.
func (p *BudgetTrack) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	p.mu.Lock()
	p.resetIfNewDay()
	spend := p.ledger.TotalSpend
	calls := p.ledger.TotalCalls
	p.mu.Unlock()

	if spend >= p.cfg.MaxBudget {
		// Record before returning — phase:"denied" events only surface when a
		// plugin recorded first, so skipping this hides the "why was I cut off?"
		// answer from the session API.
		pctx.Record(pipeline.Invocation{
			Action: pipeline.ActionDeny,
			Reason: "budget.exceeded",
			Details: map[string]string{
				// Key + precision mirror the 429 wire message and
				// costEvent.DailyTotalUSD — one ledger, one name.
				"daily_total_usd": strconv.FormatFloat(spend, 'f', 4, 64),
				"daily_max_usd":   strconv.FormatFloat(p.cfg.MaxBudget, 'f', 2, 64),
				"total_calls":     strconv.Itoa(calls),
			},
		})
		return pipeline.DenyStatus(429, "budget.exceeded",
			fmt.Sprintf("Cortex ExceededTokenBudget: daily spend $%.4f exceeds budget $%.2f. Reset at midnight UTC.", spend, p.cfg.MaxBudget))
	}
	return pipeline.Action{Type: pipeline.Continue}
}

// OnResponse handles the buffered path on listeners that do not route through
// OnResponseFrame. On the proxy listeners this plugin is a StreamingResponder,
// so pipeline.RunResponse skips it and OnResponseFrame drives accumulation
// instead; this remains for listeners that only call OnResponse.
func (p *BudgetTrack) OnResponse(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	p.bill(pctx)
	return pipeline.Action{Type: pipeline.Continue}
}

// OnResponseFrame bills the terminal frame.
//
// It no longer prices anything. inference-parser settles the cost — it is the component
// that knows when usage is final — and this plugin's job is the ledger and the budget. See
// authlib/costing and inferenceparser/cost.go for why the split runs that way.
func (p *BudgetTrack) OnResponseFrame(_ context.Context, pctx *pipeline.Context, frame []byte, last bool) pipeline.Action {
	if !last {
		return pipeline.Action{Type: pipeline.Continue}
	}
	p.bill(pctx)
	return pipeline.Action{Type: pipeline.Continue}
}

// bill adds the settled cost to today's ledger, exactly once per request.
//
// The idempotence guard stays here even though the parser has one of its own: a listener
// can dispatch a terminal frame twice (extproc does, once for headers and once for the
// buffered body), and this is the ledger — double-counting money is not recoverable from a
// later correction, because the file has already been written.
func (p *BudgetTrack) bill(pctx *pipeline.Context) {
	st := pipeline.GetState[settleState](pctx, stateKey)
	if st == nil {
		st = &settleState{}
		pipeline.SetState(pctx, stateKey, st)
	}
	if st.settled {
		return
	}

	settled, ok := costing.Load(pctx)
	if !ok || !settled.Priced {
		// Nothing was priced, so there is nothing to bill and no ledger fields to
		// add. NOT marked settled: a listener that calls OnResponse before the
		// parser has finalized must not lock this request out of being billed by the
		// terminal frame that follows.
		return
	}
	st.settled = true

	// Only a gateway figure is worth comparing against the table. Comparing a modelled
	// figure with itself would always agree and say nothing.
	if settled.Source == costevent.SourceGatewayHeader && settled.CostUSD > 0 {
		p.checkDrift(pctx, settled)
	}

	total, added := p.accumulate(settled.CostUSD)
	if !added {
		// A settled zero: the gateway charged nothing, so nothing enters the ledger.
		// The record still needs this plugin's fields, because a client showing a
		// budget needs the daily total for a free call as much as for a billed one.
		p.mu.Lock()
		total = p.ledger.TotalSpend
		p.mu.Unlock()
	}
	p.amend(pctx, total)
}

// amend adds this plugin's own fields to the record the cost owner published.
//
// The one sanctioned second write to that key: the daily total and the configured maximum
// are the budget's business and nothing else's, and the alternative — a second event just
// for two numbers — would make every consumer join two records to render one line.
func (p *BudgetTrack) amend(pctx *pipeline.Context, dailyTotal float64) {
	if costing.Amend(pctx, func(ev *costevent.Event) {
		ev.DailyTotalUSD = dailyTotal
		ev.DailyMaxUSD = p.cfg.MaxBudget
	}) {
		return
	}
	// No record to amend means no cost owner ran. Nothing to say, and inventing a
	// record here would report a cost this plugin did not compute.
}

// accumulate adds one priced call to today's ledger and persists it,
// returning the post-add TotalSpend and whether the cost was recorded.
// A non-finite or non-positive cost is ignored (added=false): NaN/±Inf
// would poison TotalSpend (making the budget check meaningless) and
// break the JSON marshal, so this is the single chokepoint that
// guarantees the ledger only ever holds finite money.
func (p *BudgetTrack) accumulate(cost float64) (dailyTotal float64, added bool) {
	if cost <= 0 || math.IsNaN(cost) || math.IsInf(cost, 0) {
		return 0, false
	}
	p.mu.Lock()
	p.resetIfNewDay()
	p.ledger.TotalSpend += cost
	p.ledger.TotalCalls++
	total := p.ledger.TotalSpend
	p.saveLedger()
	p.mu.Unlock()
	return total, true
}

func (p *BudgetTrack) todayUTC() string {
	return time.Now().UTC().Format("2006-01-02")
}

func (p *BudgetTrack) resetIfNewDay() {
	today := p.todayUTC()
	if p.ledger.Date != today {
		p.ledger = spendLedger{Date: today}
	}
}

func (p *BudgetTrack) loadLedger() {
	data, err := os.ReadFile(p.cfg.SpendFile)
	if err != nil {
		p.ledger = spendLedger{Date: p.todayUTC()}
		return
	}
	var l spendLedger
	if json.Unmarshal(data, &l) != nil || l.Date != p.todayUTC() {
		p.ledger = spendLedger{Date: p.todayUTC()}
		return
	}
	p.ledger = l
}

func (p *BudgetTrack) saveLedger() {
	data, err := json.MarshalIndent(p.ledger, "", "  ")
	if err != nil {
		// Never overwrite a good ledger with a failed marshal (e.g. a
		// non-finite TotalSpend that slipped through). accumulate already
		// rejects non-finite costs; this is the belt-and-suspenders guard.
		return
	}
	_ = os.WriteFile(p.cfg.SpendFile, data, 0644)
}

var (
	_ pipeline.Plugin             = (*BudgetTrack)(nil)
	_ pipeline.Configurable       = (*BudgetTrack)(nil)
	_ pipeline.StreamingResponder = (*BudgetTrack)(nil)
)
