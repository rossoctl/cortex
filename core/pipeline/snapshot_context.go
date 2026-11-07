package pipeline

import (
	"github.com/tidwall/gjson"

	"strconv"
)

// snapshotClient returns a copy of the parsed client label.
//
// UNEXPORTED, unlike its five siblings, and not an oversight: a listener never calls this one.
// Its only caller is Context.ClientInfo, which snapshots on the caller's behalf, so every
// recording site already receives a copy. Exported, it would invite
// SnapshotClient(pctx.ClientInfo()) at some future site — a second copy of a copy, which reads
// as belt-and-braces and is really a hint that the ownership is unclear.
//
// Every field on a SessionEvent is snapshotted by one of these helpers precisely so an
// already-appended event cannot be rewritten later. Without one here, Client would be the live
// pointer Context.ClientInfo memoizes, shared by every recording site of the request: one
// mutation through it would relabel every event, including those already handed to the session
// store and being read by the session API.
//
// THAT IS AN INTEGRITY CLAIM, not a tidiness one. This label is what attributes spend to a
// program, and context.go states that a caller lying about itself mis-attributes "that
// caller's own spend and nothing else". A shared mutable pointer is exactly how that stops
// being true: a plugin could re-file spend already recorded under another agent's name. The
// label is still self-reported and still spoofable — nothing here changes that — but it is
// now fixed at the moment it is recorded, which is what the claim requires.
//
// All three fields are strings, so a shallow copy is a deep one.
func snapshotClient(c *EventClient) *EventClient {
	if c == nil {
		return nil
	}
	cp := *c
	return &cp
}

// SnapshotIdentity copies the caller identity off pctx so the session
// event stays valid after pctx is discarded. Returns nil when no
// identity information is available (e.g., jwt-validation didn't run
// on this path and no agent identity was attached).
func SnapshotIdentity(pctx *Context) *EventIdentity {
	if pctx.Identity == nil && pctx.Agent == nil {
		return nil
	}
	id := &EventIdentity{}
	if pctx.Identity != nil {
		id.Subject = pctx.Identity.Subject()
		id.ClientID = pctx.Identity.ClientID()
		if scopes := pctx.Identity.Scopes(); len(scopes) > 0 {
			id.Scopes = append([]string(nil), scopes...)
		}
	}
	if pctx.Agent != nil {
		id.AgentID = pctx.Agent.WorkloadID
	}
	return id
}

// DeriveError constructs an EventError from response-side signals.
// Returns nil for 2xx / no guardrail block / no parser error.
func DeriveError(pctx *Context) *EventError {
	if pctx.Extensions.Security != nil && pctx.Extensions.Security.Blocked {
		return &EventError{
			Kind:    "blocked",
			Message: pctx.Extensions.Security.BlockReason,
		}
	}
	if pctx.StatusCode >= 400 {
		return &EventError{
			Kind:    "backend_error",
			Code:    strconv.Itoa(pctx.StatusCode),
			Message: upstreamErrorKind(pctx.ResponseBody),
		}
	}
	return nil
}

// EventErrorOr returns fail when the caller has an explicit failure to report,
// else what DeriveError can infer from the response. fail must win: it is set
// where the exchange failed — at the call, or while buffering the response — and
// DeriveError reads only the response. With no response it returns nil; with one
// it describes the upstream's status as though that had been delivered. A fail
// for a response the upstream did send carries that status in Code.
func EventErrorOr(pctx *Context, fail *EventError) *EventError {
	if fail != nil {
		return fail
	}
	return DeriveError(pctx)
}

// upstreamErrorKind extracts the provider's machine-readable error type from an
// error response body, or "" when there isn't one.
//
// REQUIRES A BUFFERED BODY. pctx.ResponseBody is only populated when some
// plugin in the chain declares ReadsBody, so on an auth-only chain — and in
// the authbridge-lite build, where the parsers are compiled out — this yields
// "" and the event stays the bare backend_error/<code> it was before. That is
// precisely where an operator has the fewest other diagnostics; closing it
// would mean buffering error responses on chains that otherwise never read a
// body, which is a listener-level decision, not one to make here.
//
// A bare `backend_error / 400` tells an operator nothing about why, which turns
// every upstream rejection into a guessing exercise. The provider already
// classifies its own failures, and the classification is what an operator acts
// on: invalid_request_error means fix the request, rate_limit_error means back
// off, authentication_error means fix credentials.
//
// The human-readable error.message is deliberately NOT captured. Provider
// messages routinely quote the offending part of the request, and the session
// store is unauthenticated — the same reason body-mutation events carry only
// length and sha256. The type and code are enum-like: bounded vocabularies
// chosen by the provider, carrying no request content.
func upstreamErrorKind(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	// Bound the parse: an error body is small, and a huge one here means this
	// isn't an error document at all.
	if len(body) > 64*1024 {
		body = body[:64*1024]
	}
	if !gjson.ValidBytes(body) {
		return ""
	}
	// Only accept a JSON string. gjson's String() on an object or array returns
	// that node's RAW JSON, so {"error":{"type":{...}}} would put response body
	// content — quoted request data, credentials — straight into the
	// unauthenticated session store, defeating the whole point of excluding
	// error.message. A numeric code is accepted because a number carries no
	// payload; anything structured is refused.
	t := stringOrNumber(gjson.GetBytes(body, "error.type"))
	if t == "" {
		t = stringOrNumber(gjson.GetBytes(body, "error.code"))
	}
	if t == "" {
		return ""
	}
	if len(t) > 64 {
		t = t[:64]
	}
	return t
}

// stringOrNumber returns the value only when the node is a JSON string or
// number. Every other type — object, array, true/false, absent — yields "",
// because String() on a container returns its raw JSON and that is body content.
func stringOrNumber(r gjson.Result) string {
	switch r.Type {
	case gjson.String, gjson.Number:
		return r.String()
	default:
		return ""
	}
}
