package pipeline

import (
	"github.com/tidwall/gjson"

	"crypto/tls"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// SnapshotA2A returns a shallow copy of ext. The record helpers attach
// the snapshot to the SessionEvent rather than the live pointer so
// response-phase mutations on pctx.Extensions.A2A (e.g. the parser
// stamping the server-assigned contextId onto SessionID during
// OnResponse) don't retroactively rewrite request-phase events that
// were already appended. Slice fields are reused intentionally — they
// are only assigned, never mutated in place, after the parser
// completes.
func SnapshotA2A(ext *A2AExtension) *A2AExtension {
	if ext == nil {
		return nil
	}
	c := *ext
	return &c
}

// SnapshotMCP returns a shallow copy of ext. Important for outbound
// request events: the same pctx.Extensions.MCP pointer receives Result
// or Err on the response side, so without snapshotting, the
// already-recorded request event would display the future response's
// result map.
func SnapshotMCP(ext *MCPExtension) *MCPExtension {
	if ext == nil {
		return nil
	}
	c := *ext
	return &c
}

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

// SnapshotInference returns a shallow copy of ext. Scalar response
// fields (Completion, FinishReason, *Tokens) get assigned on the live
// extension during OnResponse; without snapshotting, the request event's
// view would contain the eventual response's token counts and completion.
//
// EVERY RECORDER MUST CALL THIS, IN EITHER DIRECTION. It is the only route by which
// token counts reach a SessionEvent, and cost/usage reads every figure it reports
// off SessionEvent.Inference (usage.go's foldInto) — so a recorder that omits it
// publishes a turn whose counts reach nothing. The omission does not present as
// one: the cost record travels independently, published into Extensions.Custom and
// collected by SnapshotPlugins, so a consumer sees a whole, correctly priced cost
// beside zero tokens with nothing to say a measurement is missing rather than
// small. The inbound recorders omitted it until inbound inference traffic existed —
// a reverse proxy in front of a model endpoint — and then it read as free traffic.
//
// The leak described above is, on today's paths, defended a second time downstream —
// session.Interner.InternEvent clones the extension inside Store.Append, so the store holds
// its own copy whether or not the recorder took one. Measured, not assumed: the parity suite
// stays green against a recorder that keeps the live pointer. That is a reason to keep calling
// this at every site rather than a reason to stop — the alternative makes each recorder's
// correctness depend on the internals of another package, and on that package continuing to
// clone. The counts claim above is unaffected either way: a recorder that passes nil here
// reports nothing, and no downstream copy can restore a figure that was never attached.
//
// Five recorders in this tree are exempt, and the exemptions are listed here rather
// than left to be rediscovered: the four SessionDenied recorders and
// forwardproxy.recordTunnelOpened. A denied request was never forwarded, so it has no
// counts and no listener records protocol extensions on a deny; a CONNECT tunnel's
// bytes are opaque, so there is nothing to parse. Anything else that builds a
// SessionEvent and skips this call is the bug above, not a sixth exemption.
func SnapshotInference(ext *InferenceExtension) *InferenceExtension {
	if ext == nil {
		return nil
	}
	c := *ext
	return &c
}

// SnapshotInvocations is an alias for FilteredByPhase that participates
// in the Snapshot* family for symmetry with the other shallow-copy
// helpers. The underlying call already returns a fresh slice.
func SnapshotInvocations(ext *Invocations, phase InvocationPhase) *Invocations {
	return ext.FilteredByPhase(phase)
}

// SnapshotPlugins collects plugin-public observability events from
// pctx.Extensions.Custom entries whose keys end in PluginEventSuffix.
// Each matching value is json.Marshaled into the wire-form map under
// the plugin name (suffix stripped). Marshal errors downgrade to slog
// Debug and skip the entry rather than aborting recording — that keeps
// a misbehaving plugin from taking out the whole session stream.
func SnapshotPlugins(custom map[string]any) map[string]json.RawMessage {
	if len(custom) == 0 {
		return nil
	}
	var out map[string]json.RawMessage
	for k, v := range custom {
		if !strings.HasSuffix(k, PluginEventSuffix) {
			continue
		}
		raw, err := json.Marshal(v)
		if err != nil {
			slog.Debug("session: skipping non-marshalable plugin event",
				"key", k, "error", err)
			continue
		}
		if out == nil {
			out = make(map[string]json.RawMessage)
		}
		pluginName := strings.TrimSuffix(k, PluginEventSuffix)
		out[pluginName] = raw
	}
	return out
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

// DurationSince returns the elapsed time since start, or 0 when start
// is zero (pctx constructed without wall-clock stamping, e.g. in unit
// tests).
func DurationSince(start time.Time) time.Duration {
	if start.IsZero() {
		return 0
	}
	return time.Since(start)
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
// else what DeriveError can infer from the response. fail must win: it is only
// set where the upstream call failed outright, and there DeriveError returns nil.
func EventErrorOr(pctx *Context, fail *EventError) *EventError {
	if fail != nil {
		return fail
	}
	return DeriveError(pctx)
}

// TransportError classifies a failed upstream call — one that never produced a
// response, so there is no status or body for DeriveError to read. Each surfaces
// to the client as the same synthetic 502, so Kind is what tells an operator
// whether to look at the network, the upstream, DNS, or a trust store.
//
// Matched with errors.Is/errors.As, unlike handshakeFailureReason, which cannot:
// these arrive wrapped in *url.Error, which unwraps. TestTransportError verifies
// every branch against a real failed request.
//
// ORDER MATTERS: *url.Error implements net.Error, so every error here satisfies
// errors.As(&netErr) and only Timeout() discriminates — hence timeout first, and
// a bare net.Error is never a classifier.
func TransportError(err error) *EventError {
	if err == nil {
		return nil
	}
	kind := "upstream_error"
	var netErr net.Error
	var dnsErr *net.DNSError
	var certErr *tls.CertificateVerificationError
	var alertErr tls.AlertError
	switch {
	case errors.As(err, &netErr) && netErr.Timeout():
		// Deliberately wide: covers a Client.Timeout and a caller's context
		// deadline, which also satisfies errors.Is(err, context.DeadlineExceeded).
		kind = "upstream_timeout"
	case errors.Is(err, syscall.ECONNREFUSED):
		kind = "upstream_refused"
	case errors.As(err, &dnsErr):
		kind = "upstream_dns"
	case errors.As(err, &certErr), errors.As(err, &alertErr):
		// Unverifiable chain, or the peer sent an alert. A version/cipher/ALPN
		// mismatch has no typed error and falls through to upstream_error;
		// catching it would mean matching message text.
		kind = "upstream_tls"
	}
	return &EventError{Kind: kind, Message: err.Error()}
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
