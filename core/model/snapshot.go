package model

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
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

// DurationSince returns the elapsed time since start, or 0 when start
// is zero (pctx constructed without wall-clock stamping, e.g. in unit
// tests).
func DurationSince(start time.Time) time.Duration {
	if start.IsZero() {
		return 0
	}
	return time.Since(start)
}

// DurationBetween returns to − from, or 0 when either endpoint is zero.
// For the optional response-side timestamp (ResponseHeadersAt), whose absence
// means "no headers ever arrived", the zero guard keeps an unset endpoint from
// publishing a nonsense figure — a negative span against time.Now() would read
// as a real duration on the row. The inverse of DurationSince: that one spans
// start-to-now for a figure read at record time, this one spans two recorded
// moments, which is what SessionEvent.Ttfb is (StartedAt → ResponseHeadersAt,
// measured when the row is written long after both).
func DurationBetween(from, to time.Time) time.Duration {
	if from.IsZero() || to.IsZero() {
		return 0
	}
	return to.Sub(from)
}

// StatusClientClosedRequest is nginx's 499, for a request the client abandoned
// before it was answered. No client ever receives it — it has gone — which is
// what makes it useful in a record: it keeps a hangup out of the 5xx series that
// an upstream outage shows up in.
const StatusClientClosedRequest = 499

// ExchangeFailure is what a listener records for an exchange with the upstream
// that ended in err instead of a response it could deliver: the status for the
// event, and why. err must be non-nil.
//
// ctx is the CLIENT's request context, and it is asked first. A client that hangs
// up cancels the upstream call under it, so err is then context.Canceled — and
// since neither proxy bounds how long the upstream may take to answer
// (forwardproxy.NewServer says why), that is how a hung upstream usually ends:
// the client gives up first. Recording a 502 upstream failure there would count a
// 502 nobody received, once per abandoned request, and leave upstream_timeout
// unreachable for the case it names. So it is client_canceled with a 499, which
// /v1/usage still counts as an error but keeps out of the 5xx series.
//
// Otherwise the client got a 502, and TransportError says why.
func ExchangeFailure(ctx context.Context, err error) (int, *EventError) {
	if ctx.Err() != nil {
		return StatusClientClosedRequest, &EventError{Kind: "client_canceled", Message: causeOf(err)}
	}
	return http.StatusBadGateway, TransportError(err)
}

// TransportError classifies a failed exchange with the upstream: a call that
// produced no response, or a response body that broke off while being read. Each
// surfaces to the client as the same synthetic 502, so Kind is what tells an
// operator whether to look at the network, the upstream, DNS, or a trust store.
//
// Matched on types with errors.Is/errors.As, unlike handshakeFailureReason, which
// cannot. TestTransportError verifies every branch against a real failed request.
//
// ORDER MATTERS for the forward proxy, whose client.Do wraps every error in a
// *url.Error. That implements net.Error, so each of its errors satisfies
// errors.As(&netErr) and only Timeout() discriminates — hence timeout first, and
// a bare net.Error is never a classifier. The reverse proxy hands over raw
// RoundTrip errors, with no *url.Error around them; the checks hold either way.
//
// Message is the cause without the request URL — see causeOf.
func TransportError(err error) *EventError {
	if err == nil {
		return nil
	}
	kind := "upstream_error"
	var netErr net.Error
	var dnsErr *net.DNSError
	var certErr *tls.CertificateVerificationError
	switch {
	case errors.As(err, &netErr) && netErr.Timeout(),
		// errors.As stops at the outermost net.Error, and a *url.Error's Timeout()
		// asks only its immediate cause. A deadline under a wrapping layer —
		// forwardproxy's mtlsDialer wraps its handshake error with %w — reads
		// false there, so ask for the deadline itself too.
		errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, os.ErrDeadlineExceeded):
		kind = "upstream_timeout"
	case errors.Is(err, syscall.ECONNREFUSED):
		kind = "upstream_refused"
	case errors.As(err, &dnsErr):
		kind = "upstream_dns"
	case errors.As(err, &certErr), isTLSAlert(err):
		// Unverifiable chain, or the peer refused the handshake with an alert. A
		// mismatch the client detects itself has no typed error and falls through
		// to upstream_error; catching it would mean matching message text.
		kind = "upstream_tls"
	}
	return &EventError{Kind: kind, Message: causeOf(err)}
}

// isTLSAlert reports whether the peer refused the TLS handshake with an alert: a
// protocol version it will not speak, a client certificate it requires.
//
// Not tls.AlertError, which is the QUIC transport's type. Over TCP, crypto/tls
// reports a received alert as a *net.OpError with Op "remote error" around an
// unexported alert value, so the Op is the typed part to match. Walked layer by
// layer because errors.As would stop at the first *net.OpError, and the
// transport's "proxyconnect" one can sit above it.
func isTLSAlert(err error) bool {
	for ; err != nil; err = errors.Unwrap(err) {
		if op, ok := err.(*net.OpError); ok && op.Op == "remote error" {
			return true
		}
	}
	return false
}

// causeOf is err's text without the request URL a *url.Error puts in front of it.
//
// That URL is why this exists. net/http strips the password from the URL it
// quotes but not the query string, so `Get "https://…?key=…": …` would carry a
// query-borne credential (Gemini's ?key=, any api_key=) onto the unauthenticated
// session API and into the log — exactly what HTTPPath is query-stripped to keep
// out. Nothing is lost: the event already carries the host, method and path.
func causeOf(err error) string {
	if err == nil {
		return ""
	}
	var urlErr *url.Error
	for errors.As(err, &urlErr) {
		if urlErr.Err == nil {
			return urlErr.Op
		}
		err = urlErr.Err
	}
	return err.Error()
}
