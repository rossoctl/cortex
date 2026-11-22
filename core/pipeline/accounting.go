package pipeline

import "github.com/rossoctl/accountant"

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
func (c *Context) ReportOps(ops ...accountant.Operation) {
	for _, op := range ops {
		if op.Operator == "" {
			op.Operator = c.currentPlugin
		}
		if c.currentPolicy == ErrorPolicyObserve {
			op.DryRun = true
		}
		c.reportedOps = append(c.reportedOps, op)
	}
}

// ReportedOps returns a copy of the ops reported so far, in the order they were reported.
func (c *Context) ReportedOps() []accountant.Operation {
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
