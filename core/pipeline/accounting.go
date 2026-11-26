package pipeline

import (
	"log/slog"
	"sync/atomic"

	"github.com/rossoctl/accountant"
)

// RequestAccountant is the per-request hook the accountant host (core/accounting.Host)
// implements. Pipeline.Run calls RequestStart before the first plugin and RequestEnd after
// the last one, on a request the pipeline let through.
//
// The hook lives in Run, not in each listener (the design put it in the listeners, §5.3a):
// Run is the one place every listener passes that sees the body before the first plugin and
// after the last, so the three listeners need no change of their own.
//
// RequestEnd is not called on a Reject or a cancel. A refused request never went upstream, so
// there is nothing to account for, and no work may start for it (Review Focus 1). Both calls
// run on the request goroutine and must return at once: the host does its hashing on a
// goroutine of its own.
type RequestAccountant interface {
	RequestStart(pctx *Context)
	RequestEnd(pctx *Context)
}

// WithAccountant attaches the process's accountant host to the pipeline. With one set, Run
// keeps the client body (ClientBody) and calls the hooks, and NeedsRequestBody is true, so
// every listener buffers the request body. A nil a attaches nothing.
//
// Only the outbound chain gets one: inference requests leave through it, and the host is
// process-scoped, so the pipelines a reload builds share it and its Tracker state.
func WithAccountant(a RequestAccountant) Option {
	return func(o *options) {
		o.accountant = a
	}
}

// ClientBody is the request body as the client sent it, before the first plugin ran. Nil when
// the pipeline has no accountant: nothing is kept then.
//
// It is the listener's own slice, not a copy. That is safe because SetBody replaces pctx.Body
// rather than writing into it, so a mutator's rewrite leaves the client's bytes as they were.
// A plugin must not write into pctx.Body in place; none does.
func (c *Context) ClientBody() []byte { return c.clientBody }

// ReportOps records the context operations a plugin made on this request, for the accountant
// (accountant/docs/cortex-wiring.md W5.1). The ops are kept in call order, which is pipeline
// order.
//
// The framework stamps two fields, so a plugin does not have to know its own name or policy:
// Operator is set to the plugin's name when the plugin left it empty, and DryRun is set when the
// plugin runs under on_error: observe, because its write was computed and not sent. A DryRun the
// plugin set itself is kept. A plugin whose write did not apply reports the op with DryRun set,
// or no op.
//
// The stamp reads the plugin being dispatched, so ReportOps belongs on the request goroutine,
// inside OnRequest. A report from any other goroutine uses OpReporter.
func (c *Context) ReportOps(ops ...accountant.Operation) {
	if len(ops) == 0 {
		return
	}
	operator, dryRun := c.currentPlugin, c.currentPolicy == ErrorPolicyObserve
	c.appendOps(operator, dryRun, ops)
}

// OpReporter is a goroutine-safe handle for reporting context operations under a fixed operator
// name and dry-run state, captured once on the request goroutine via Context.OpReporter. It is
// the form for a report that may come from a goroutine other than the request's: an engine's
// model call, which context-guru makes on goroutines of its own.
//
// Keeping operator and dryRun inside OpReporter, rather than accepted as Report arguments, keeps
// the Operator stamp under the framework's control: a plugin gets a reporter for the plugin
// dispatching it, and cannot ask to report under another plugin's name (accountant/docs/
// cortex-wiring.md W5.1: "The framework sets Operator. A plugin cannot report under another
// plugin's name.").
type OpReporter struct {
	c        *Context
	operator string
	dryRun   bool
}

// OpReporter captures the plugin being dispatched and its policy (Observing) now, on the request
// goroutine, so the returned OpReporter's Report method can be called later from any goroutine.
func (c *Context) OpReporter() OpReporter {
	return OpReporter{c: c, operator: c.currentPlugin, dryRun: c.currentPolicy == ErrorPolicyObserve}
}

// Report records ops like Context.ReportOps, under the operator and dry-run state r was built
// with.
//
// It returns false when the ops were dropped because the request's op list was already sealed:
// Pipeline.Run seals it right before handing it to the accountant, so an op reported after the
// request left (a detached model call, design §2 non-goal) reaches no figure. Each dropped op is
// counted in LateOpsDropped. The zero OpReporter belongs to no request: it reports nothing and
// returns false.
func (r OpReporter) Report(ops ...accountant.Operation) bool {
	if r.c == nil {
		return false
	}
	if len(ops) == 0 {
		return true
	}
	return r.c.appendOps(r.operator, r.dryRun, ops)
}

func (c *Context) appendOps(operator string, dryRun bool, ops []accountant.Operation) bool {
	c.opsMu.Lock()
	defer c.opsMu.Unlock()
	if c.opsSealed {
		lateOpsDropped.Add(int64(len(ops)))
		slog.Debug("pipeline: context operation reported after the request was accounted; dropped",
			"operator", operator, "ops", len(ops))
		return false
	}
	for _, op := range ops {
		if op.Operator == "" {
			op.Operator = operator
		}
		if dryRun {
			op.DryRun = true
		}
		c.reportedOps = append(c.reportedOps, op)
	}
	return true
}

// sealOps closes the op list: Run calls it right before RequestEnd reads ReportedOps.
func (c *Context) sealOps() {
	c.opsMu.Lock()
	c.opsSealed = true
	c.opsMu.Unlock()
}

// lateOpsDropped counts, per process, the ops reported after their request's list was sealed.
var lateOpsDropped atomic.Int64

// LateOpsDropped returns how many context operations this process dropped because they were
// reported after their request was handed to the accountant (OpReporter.Report). Model calls an
// engine detaches from the request it served land here: they are a non-goal of the accountant
// integration, and this count says how often they happen.
func LateOpsDropped() int64 { return lateOpsDropped.Load() }

// ReportedOps returns a copy of the ops reported so far, in the order they were reported.
func (c *Context) ReportedOps() []accountant.Operation {
	c.opsMu.Lock()
	defer c.opsMu.Unlock()
	if len(c.reportedOps) == 0 {
		return nil
	}
	return append([]accountant.Operation(nil), c.reportedOps...)
}

// SetAccountingThread names the accountant thread for this request, overriding the default rule
// (core/accounting.ThreadOf: "main", or the subagent id). The context-guru plugin calls it with
// its own thread split, which it builds from prefix continuity. An empty id is ignored, so a
// plugin that could not name a thread leaves an earlier choice, or the default, in place.
func (c *Context) SetAccountingThread(id, parent string) {
	if id == "" {
		return
	}
	c.accountingThread, c.accountingParent, c.accountingThreadSet = id, parent, true
}

// AccountingThread returns the thread SetAccountingThread named, with ok false when none did.
func (c *Context) AccountingThread() (id, parent string, ok bool) {
	return c.accountingThread, c.accountingParent, c.accountingThreadSet
}

// SetClientCompaction records that this request is the first one after a client compaction
// (accountant R55). The context-guru plugin calls it from its thread tracker. The accounting host
// passes it to the accountant as RequestMeta.Compaction. An empty trigger is ignored, so a plugin
// that saw no compaction leaves an earlier verdict in place.
func (c *Context) SetClientCompaction(t accountant.CompactionTrigger) {
	if t == "" {
		return
	}
	c.clientCompaction = t
}

// ClientCompaction returns the verdict SetClientCompaction recorded, or "" when none did.
func (c *Context) ClientCompaction() accountant.CompactionTrigger {
	return c.clientCompaction
}

// BillingSnapshot returns accountant.BillingFrom(c.Headers) as it stood the FIRST time this pctx
// was handed to a Pipeline.Run — before any plugin, inbound or outbound, can have rewritten the
// credential header BillingFrom reads. Computed and cached lazily, so a caller that reaches it
// before any Run (a test, or a listener that reads it itself) still gets a stable answer: once
// taken, the snapshot never changes, even if Headers does later.
//
// Why it has to be this early: what BillingFrom must see is the CLIENT's credential, and the
// outbound chain's own plugins replace it before the request leaves — token-exchange, tokenbroker,
// inference-router's setKey and staticinject all rewrite Authorization. Read after them, the mode
// would be misclassified (often as "unknown", which drops every saving out of the dollar totals).
//
// No shipped listener shares one pctx between chains: extproc builds a fresh one in each of
// handleInbound, handleInboundBody, handleOutbound and handleOutboundBody; reverseproxy runs only
// the inbound chain and forwardproxy only the outbound one. Pipeline.Run still calls this on every
// direction's Pipeline, so a listener that did run both chains on one pctx would get the
// snapshot from the first Run, before an inbound rewrite (jwt-validation's placeholder_mode) too.
func (c *Context) BillingSnapshot() accountant.Billing {
	if !c.billingSet {
		c.billing, c.billingSet = accountant.BillingFrom(c.Headers), true
	}
	return c.billing
}
