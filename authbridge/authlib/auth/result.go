// Package auth composes authlib building blocks into HandleInbound and
// HandleOutbound — the two functions that all listeners call.
package auth

import (
	"github.com/rossoctl/cortex/authbridge/authlib/plugins/jwtvalidation/validation"
)

// Inbound actions.
const (
	ActionAllow = "allow"
	ActionDeny  = "deny"
)

// Outbound actions.
const (
	ActionReplaceToken = "replace_token"
	// ActionAllow and ActionDeny are reused from inbound.
)

// No-token outbound policies (set by mode preset).
const (
	NoTokenPolicyClientCredentials = "client-credentials"
	NoTokenPolicyAllow             = "allow"
	NoTokenPolicyDeny              = "deny"
)

// InboundResult is the outcome of inbound JWT validation.
type InboundResult struct {
	Action         string             // ActionAllow or ActionDeny
	Claims         *validation.Claims // non-nil when a valid JWT was present
	DenyStatus     int                // HTTP status code (e.g., 401)
	DenyReason     string             // human-readable error — safe for logs, response bodies
	DenyReasonCode InboundDenialReason // machine-stable enum paired with the /stats counter; use for filtering / indexing session events
}

// OutboundResult is the outcome of outbound token exchange.
type OutboundResult struct {
	Action         string               // ActionAllow, ActionReplaceToken, or ActionDeny
	Token          string               // replacement token (when Action == ActionReplaceToken)
	DenyStatus     int                  // HTTP status code (e.g., 503)
	DenyReason     string               // human-readable error
	DenyReasonCode OutboundDenialReason // machine-stable enum paired with the /stats counter
	CacheHit       bool                 // true when Token was served from the exchange cache; safe to read on any Action

	// Route context populated when the router matched the request. Gives
	// observability callers (token-exchange plugin writing a SessionEvent)
	// a way to surface which route was chosen without reaching into
	// routing.ResolvedRoute directly. RouteMatched=false means the
	// request passed through because no route fired.
	RouteMatched    bool   // true when Router.Resolve returned a non-nil route (regardless of action)
	TargetAudience  string // resolved route's audience (or deriver's output); empty on passthrough
	RequestedScopes string // resolved route's scopes, raw space-separated; empty on passthrough
}
