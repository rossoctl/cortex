package pipeline

import "context"

// Resender is an optional interface for a plugin that may have the listener send a
// request again after its upstream refused it. A router is the case it was made for:
// its server refused the model the client asked for, and the router has one of the
// server's own models to send instead, before the client sees the refusal.
//
// Resend is called after an attempt the upstream answered with a status outside 2xx,
// before any byte of that answer reached the client, and at most MaxResends times per
// request. status is the answer's status, and body is its first ResendPeekLimit bytes.
// A plugin that wants the request sent again changes it through the usual APIs —
// SetRequestModel, SetBody, Headers — names why with MarkResend, and returns true. The
// listener then records the refused answer, sends the request as it now stands to the
// same destination, and asks again only if that attempt is refused too and fewer than
// MaxResends resends were made. A plugin must not answer the same kind of refusal
// twice for one request; ResendReasons says what it already answered. Returning false
// lets the refused answer through unchanged.
//
// It is dispatched like OnRequest, in the request phase and under the plugin's
// WritesRequestBody declaration, so SetRequestModel is accepted. Redirect and
// SetRedirectPath are not: the destination was fixed by the first send. Only a plugin
// under on_error: enforce is asked, because a resend changes the request, and a plugin
// under observe must not. Only the forward proxy asks today.
type Resender interface {
	Resend(ctx context.Context, pctx *Context, status int, body []byte) bool
}

// ResendPeekLimit is how much of a refused answer's body a Resender is shown. An
// error body is small; a larger one is cut, not refused.
const ResendPeekLimit = 64 << 10

// MaxResends is how many times a listener asks the Resenders about one request: once
// after each refused attempt, so at most MaxResends+1 attempts reach the upstream.
const MaxResends = 2

// HasResenders reports whether any plugin in the pipeline may ask for a resend, so a
// listener reads a refused answer's body only when one might.
func (p *Pipeline) HasResenders() bool {
	for i, plugin := range p.plugins {
		if _, ok := resenderOf(plugin); ok && p.PolicyAt(i) == ErrorPolicyEnforce {
			return true
		}
	}
	return false
}

// Resend asks the pipeline's Resenders, in chain order, whether the request should be
// sent again, and the first that says yes decides. See Resender for the contract.
func (p *Pipeline) Resend(ctx context.Context, pctx *Context, status int, body []byte) bool {
	for i, plugin := range p.plugins {
		r, ok := resenderOf(plugin)
		if !ok || p.PolicyAt(i) != ErrorPolicyEnforce {
			continue
		}
		pctx.setCurrent(plugin.Name(), InvocationPhaseRequest, ErrorPolicyEnforce)
		pctx.currentMayWriteRequestBody = p.writesRequestAt(i)
		again := r.Resend(ctx, pctx, status, body)
		pctx.clearCurrent()
		if again {
			return true
		}
	}
	return false
}

// resenderOf is plugin's Resender. A configured plugin is asked through the plugin it
// wraps, rather than by a forwarding method on the wrapper: one there would make every
// configured plugin a Resender, and every refused answer would then be read for
// nothing.
func resenderOf(plugin Plugin) (Resender, bool) {
	switch w := plugin.(type) {
	case *configuredPlugin:
		plugin = w.Plugin
	case *configuredStreamingPlugin:
		plugin = w.Plugin
	}
	r, ok := plugin.(Resender)
	return r, ok
}

// RenewRequestID gives the context a new request id. Listener-facing: a listener that
// sends a request again calls it before recording the second attempt, which is a
// request of its own on the timeline and must not pair with the first one's answer.
func (c *Context) RenewRequestID() { c.requestID = newRequestID() }

// SetResendable is listener-facing. A listener calls it before Run when it will ask the
// Resenders after a refused answer of this request. Only the forward proxy does. A plugin
// that changes a request in a way the provider may refuse, and that must then send the
// original instead (context-guru's tool deferral), reads Resendable first: on a listener
// that never resends, the client would see the refusal.
func (c *Context) SetResendable() { c.resendable = true }

// Resendable reports whether this request's listener asks the Resenders after a refused
// answer. A Resender is still asked only under on_error: enforce (Resender).
func (c *Context) Resendable() bool { return c.resendable }

// MarkResend names why a Resender that returns true wants the request sent again. A
// plugin that answers more than one kind of refusal reads ResendReasons to answer each
// kind at most once per request.
func (c *Context) MarkResend(reason string) { c.resendReasons = append(c.resendReasons, reason) }

// ResendReasons returns the reasons named so far for this request, oldest first.
func (c *Context) ResendReasons() []string {
	return append([]string(nil), c.resendReasons...)
}
