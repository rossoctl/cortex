package pipeline

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
)

// MarkRedirectable says the listener builds the upstream request from this context
// itself, after the pipeline, so a Redirect can take effect. The forward proxy calls
// it for every request it re-originates, plain or TLS-bridged.
//
// A listener must not call it for a context it cannot redirect. The outbound
// pipeline also runs once per CONNECT and once per transparently redirected
// connection, and both dial the address the client chose whatever a plugin does: a
// redirect accepted there would be recorded and never happen.
//
// Listener-facing, like SetCurrentPlugin. Production plugins never call this.
func (c *Context) MarkRedirectable() { c.redirectable = true }

// Redirectable reports whether the listener will honor a Redirect on this request. A
// routing plugin checks it first, so that it acts only on requests it can move.
func (c *Context) Redirectable() bool { return c.redirectable }

// Redirect sends the request to target instead of the host the client named.
//
// target must be absolute — scheme http or https, a host, an optional port — and
// carry nothing else: no user info, no path beyond "/", no query, no fragment. A
// redirect changes where a request goes and nothing about what it says; the path,
// headers and body stay the plugin's to change through the existing APIs.
//
// It sets Scheme and Host to the target's. Host following the redirect is the point:
// the session events, usage, the cost ledger and modelled pricing all key on it, and
// they must describe where the bytes went. Plugins that run after this one and key on
// Host see the new host for the same reason. The host the client named stays
// available as RequestedHost. The validated target is also kept privately, and that
// copy is what the listener applies (see RedirectTarget): Scheme and Host are any
// plugin's to write, so a later write to them cannot move the request.
//
// The framework records the redirect as a modify/redirected Invocation with "from"
// and "to" in Details, as SetBody records a body rewrite: a redirect moves the request
// and its credentials to another host, so the timeline shows it even if the plugin
// records nothing of its own. Under on_error: observe nothing moves and the
// Invocation is marked Shadow, so plugin code looks the same under enforce and observe.
//
// What a redirect does not do. The request's headers — credentials included — go to
// the new host unchanged. And it does not gate the plugin's own header writes: under
// observe nothing moves while those still apply, so a plugin that attaches
// credentials meant for the target must do so only when the request actually goes
// there — check Redirected after the call, which stays false under observe and on a
// refusal. Redirected is the only safe gate: pctx.Host names what the client asked for
// and, on a TLS-bridged request, is the client's own Host header, which need not match
// the address the proxy dials. A plugin that attaches credentials meant for a server
// should always Redirect to that server, even when pctx.Host already names it, so the
// request is dialed there. Plugins earlier in the chain made their decisions on the
// requested host and are not run again.
//
// Refused, with nothing changed and nothing recorded, when the calling plugin does
// not declare WritesDestination, when the listener did not mark the context
// redirectable, when the target is malformed, and outside OnRequest.
func (c *Context) Redirect(target *url.URL) error {
	if c.inFinish {
		slog.Warn("pipeline: plugin called pctx.Redirect during OnFinish — refused (the response is already sent)",
			"plugin", c.currentPlugin)
		return errors.New("pipeline: Redirect refused in OnFinish: the request has already been sent")
	}
	if c.currentPhase != InvocationPhaseRequest {
		slog.Warn("pipeline: plugin called pctx.Redirect outside OnRequest — refused",
			"plugin", c.currentPlugin, "phase", c.currentPhase)
		return fmt.Errorf("pipeline: Redirect refused in phase %q: it is accepted only from OnRequest", c.currentPhase)
	}
	if !c.currentMayRedirect {
		return fmt.Errorf("pipeline: plugin %q called Redirect without declaring WritesDestination", c.currentPlugin)
	}
	if !c.redirectable {
		return errors.New("pipeline: this request cannot be redirected: the listener does not re-originate it (a CONNECT, or a transparently redirected connection)")
	}
	if err := checkRedirectTarget(target); err != nil {
		return err
	}
	details := map[string]string{"from": c.Host, "to": target.Host}
	if c.currentPolicy == ErrorPolicyObserve {
		c.Record(Invocation{Action: ActionModify, Reason: "redirected", Shadow: true, Details: details})
		return nil
	}
	if !c.redirected {
		c.requestedHost = c.Host
		c.redirected = true
	}
	c.redirectScheme, c.redirectHost = target.Scheme, target.Host
	c.Scheme, c.Host = target.Scheme, target.Host
	c.Record(Invocation{Action: ActionModify, Reason: "redirected", Details: details})
	return nil
}

// Redirected reports whether a Redirect took effect on this request, and is the ok
// RedirectTarget reports. The listener keys on that, not on RequestedHost: a redirect
// that changed only the scheme, or that pointed back at the host the client named,
// still has to be applied.
//
// It is also the only safe gate for a plugin that attaches credentials meant for the
// target. The request's headers go wherever the request goes, and Redirect returns nil
// under on_error: observe without moving anything, so a key set on the strength of that
// nil alone would reach the host the client named. Redirected stays false under observe
// and after a refusal. Comparing pctx.Host with the target is unsafe: on a TLS-bridged
// request, pctx.Host is the client's own Host header, not necessarily the address the
// proxy will dial.
func (c *Context) Redirected() bool { return c.redirected }

// RedirectTarget is the scheme and host the last accepted Redirect validated, with ok
// exactly when Redirected. Listeners apply this, never the exported Scheme and Host:
// any plugin may write those, declared or not, so a listener that read them back would
// let a later plugin steer a redirected request somewhere no WritesDestination plugin
// chose, and the modify/redirected record would name a host the bytes never went to.
func (c *Context) RedirectTarget() (scheme, host string, ok bool) {
	if !c.redirected {
		return "", "", false
	}
	return c.redirectScheme, c.redirectHost, true
}

// SetRedirectPath sends a redirected request to path on its new host instead of the
// path the client named. A redirect alone changes only where a request goes, and that
// is enough between servers that take an API under the same path. A server that takes
// it under another — /v1/chat/completions where the client named
// /zen/v1/chat/completions — needs the path too, and this is how a routing plugin says
// so.
//
// Accepted from OnRequest, from a plugin that declares WritesDestination, once a
// Redirect has taken effect; refused otherwise, with nothing changed. Under on_error:
// observe a Redirect moves nothing, so there is nothing to give a path to. path must
// be absolute, with no query or fragment and no ".." segment; the client's query
// string is kept. Errors quote no part of path.
//
// It sets Path, as Redirect sets Host, so the request's row names the path the bytes
// went to, and the path the client named is kept in the modify/path_rewritten record.
// The listener applies the copy RedirectPath reports.
func (c *Context) SetRedirectPath(path string) error {
	switch {
	case c.inFinish:
		return errors.New("pipeline: SetRedirectPath refused in OnFinish: the request has already been sent")
	case c.currentPhase != InvocationPhaseRequest:
		return fmt.Errorf("pipeline: SetRedirectPath refused in phase %q: it is accepted only from OnRequest", c.currentPhase)
	case !c.currentMayRedirect:
		return fmt.Errorf("pipeline: plugin %q called SetRedirectPath without declaring WritesDestination", c.currentPlugin)
	case !c.redirected:
		return errors.New("pipeline: SetRedirectPath refused: no Redirect has taken effect on this request")
	case !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "?#") || slices.Contains(strings.Split(path, "/"), ".."):
		return errors.New("pipeline: SetRedirectPath path must be absolute, with no query, fragment or \"..\" segment")
	}
	from := c.Path
	c.redirectPath, c.Path = path, path
	c.Record(Invocation{Action: ActionModify, Reason: "path_rewritten", Details: map[string]string{"from": from, "to": path}})
	return nil
}

// RedirectPath is the path SetRedirectPath set, with ok only when one was set on a
// request a Redirect moved. Listeners apply it with RedirectTarget, never the exported
// Path, for the reason RedirectTarget gives.
func (c *Context) RedirectPath() (string, bool) {
	return c.redirectPath, c.redirected && c.redirectPath != ""
}

// RequestedHost is the host the client named when a redirect sent the request
// somewhere else, and "" otherwise — including when the redirects ended back at
// that host. It is what a session event records beside Host. It compares against
// the target RedirectTarget reports, not the exported Host, so a later write to Host
// can neither hide a redirect nor invent one.
func (c *Context) RequestedHost() string {
	if !c.redirected || strings.EqualFold(c.requestedHost, c.redirectHost) {
		return ""
	}
	return c.requestedHost
}

// checkRedirectTarget enforces Redirect's shape rule. Errors quote no part of the
// target: a refused target is exactly where a key sits — as user info, in the
// query, in a path segment — and Redacted masks only a password.
func checkRedirectTarget(u *url.URL) error {
	if u == nil {
		return errors.New("pipeline: Redirect target is nil")
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return errors.New("pipeline: Redirect target's scheme must be http or https")
	case u.Opaque != "" || u.Hostname() == "":
		return errors.New("pipeline: Redirect target has no host")
	case u.User != nil:
		return errors.New("pipeline: Redirect target carries user info; credentials belong in headers")
	case u.Path != "" && u.Path != "/":
		return errors.New("pipeline: Redirect target has a path; a redirect changes only the scheme and host")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return errors.New("pipeline: Redirect target has a query or fragment")
	}
	return nil
}
