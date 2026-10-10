package pipeline

import (
	"crypto/tls"
	"encoding/json"
	"log/slog"
	"time"
)

// SessionPhase distinguishes request, response, and terminal-denial events.
type SessionPhase int

const (
	SessionRequest SessionPhase = iota
	SessionResponse
	// SessionDenied is a terminal event for inbound requests a pipeline
	// plugin rejected (e.g., jwt-validation failing a token check). The
	// listener records this instead of a Request/Response pair so agentop
	// and other session consumers can distinguish denials from normal
	// request/response flow without scanning StatusCode.
	SessionDenied
)

func (p SessionPhase) String() string {
	switch p {
	case SessionRequest:
		return "request"
	case SessionResponse:
		return "response"
	case SessionDenied:
		return "denied"
	default:
		return "unknown"
	}
}

// MarshalJSON emits the string form ("request"/"response"/"denied") so
// the wire format stays human-readable.
func (p SessionPhase) MarshalJSON() ([]byte, error) {
	return json.Marshal(p.String())
}

// UnmarshalJSON decodes a SessionPhase from the string form emitted by
// MarshalJSON. Unknown strings decode to SessionRequest (zero value)
// without error — tolerant forward-compat behavior. A Debug-level log
// fires on unknown input so wire-format drift (e.g., a server emitting a
// typo) is at least observable in a verbose test run rather than silent.
func (p *SessionPhase) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	switch s {
	case "request":
		*p = SessionRequest
	case "response":
		*p = SessionResponse
	case "denied":
		*p = SessionDenied
	default:
		slog.Debug("pipeline: unknown SessionPhase, defaulting to request", "value", s)
		*p = SessionRequest
	}
	return nil
}

// SessionEvent represents a single pipeline event captured by the session store.
// At most one of A2A, MCP, or Inference is non-nil on any given event, but
// the event may also carry Invocations (records from any plugin that ran on
// the pass) and/or Plugins (the escape-hatch map from Extensions.Custom)
// regardless of which protocol extension is present. An event with
// Phase=SessionDenied typically carries only Invocations (+ Identity, Host,
// Error) because the request never reached a protocol parser before the
// pipeline rejected it.
type SessionEvent struct {
	// SessionID is the session bucket the event was appended to. Populated by
	// Store.Append so downstream consumers (particularly the SSE stream
	// filter) can attribute any event — including outbound MCP/Inference
	// events that have no protocol-native session concept — to a session
	// without needing a side-channel lookup.
	SessionID string

	// Seq orders the events within one session, counting from 1. Assigned by
	// Store.Append; zero on an event that never went through a store.
	//
	// It exists because nothing else here can order a session reliably. At ties —
	// two events recorded in the same instant are common on a fast local proxy —
	// and RequestID pairs a request with its response without saying which pair
	// came first. That made a stable pagination cursor impossible: an offset into
	// the slice shifts under eviction, and a timestamp cannot break its own ties.
	//
	// Seq is per session, not global, so it says nothing about ordering across
	// sessions. Gaps are expected and are not lost events in the paging sense:
	// FIFO eviction drops a prefix, and trimEventsPinIntent can leave one much
	// older event pinned ahead of the retained tail. What is guaranteed is that
	// the events a session holds are ascending in Seq, which is what lets both
	// the store and a client binary-search them.
	//
	// The numbering is unique within one INCARNATION of a session, not forever:
	// the counter lives on the store's entry, so whole-session eviction followed
	// by traffic under the same id starts again at 1. A client holding a cursor
	// across that boundary cannot detect it from Seq and must order pages by At —
	// see session.entry.nextSeq and agentop's applyOlderPage.
	//
	// Serialized via sessionEventWire like every other field here, not by a tag on
	// this struct — SessionEvent has a custom MarshalJSON.
	Seq uint64

	At        time.Time
	Direction Direction
	Phase     SessionPhase
	// RequestID pairs a request event with its response event. Without it a
	// consumer can only pair positionally, which misattributes whenever a
	// client has concurrent requests in flight.
	RequestID string
	A2A       *A2AExtension
	MCP       *MCPExtension
	Inference *InferenceExtension

	// Invocations carries records of every plugin that ran on the
	// pipeline pass — gate, parser, rate-limiter, etc. Nil when no
	// plugin appended a record, non-nil with at least one Inbound or
	// Outbound entry otherwise. See Invocations godoc for the per-
	// plugin shape.
	Invocations *Invocations

	// Plugins carries plugin-public observability events in JSON form.
	// Populated by the listener from Extensions.Custom entries whose keys
	// end in PluginEventSuffix ("/event"); the suffix is stripped, so
	// consumers see the plugin name as the map key. Value is the plugin-
	// provided struct marshaled to JSON — opaque from the listener's
	// perspective. Consumers decode each key into their own type. See
	// docs/plugin-reference.md for the producer contract.
	Plugins map[string]json.RawMessage

	// Identity snapshot at record time. Lets downstream plugins attribute an
	// event to the caller (Subject) and the handling sidecar (AgentID)
	// without re-parsing the original request. Nil for events recorded
	// before jwt-validation ran or when session tracking is disabled.
	Identity *EventIdentity

	// StatusCode is the HTTP status of the response. Zero on request events
	// and on response events that were recorded before the status was known
	// (e.g., connection reset). A non-zero value >= 400 also populates Error.
	StatusCode int

	// Error captures a terminal error condition for this event. Nil on
	// successful requests and 2xx responses. Populated for non-2xx responses,
	// guardrail blocks, and parse failures.
	Error *EventError

	// Host is the HTTP :authority (or Host header) of the event. For inbound
	// events it's the agent's own address; for outbound events it's the
	// target service, which is the useful case — a session with many
	// outbound calls can be attributed to the tool / LLM / target each
	// landed on. Empty when the listener didn't populate pctx.Host.
	Host string

	// RequestedHost is the host the client asked for, present only when a plugin
	// redirected the request (pctx.Redirect) and Host is therefore somewhere else.
	// Host stays the host the bytes went to, because usage, the cost ledger and
	// pricing key on it.
	RequestedHost string

	// Duration is the wall-clock time from request entry into the listener
	// to response recording. Zero on request-phase events. On response
	// events it's computed as now - matching-request.At. On denied events
	// it's DurationSince(pctx.StartedAt) — a denial ends the request, so
	// it has a duration even though no response was ever recorded.
	Duration time.Duration

	// TLS, when non-nil, carries connection-level identity for events
	// that arrived over TLS: negotiated version, cipher, and the peer's
	// SPIFFE ID extracted from the URI SAN. Nil for plaintext events
	// and for events recorded before the TLS handshake completed.
	// Populated by the listener layer; plugins do not write to it.
	TLS *EventTLS

	// Tunnel marks both rows of an opaque CONNECT / transparent-redirect tunnel:
	// the bytes are not HTTP, so there's no protocol parse. Set only by
	// recordTunnelOpened (the request-phase open) and recordTunnelClosed (the
	// response-phase close, which shares the open's RequestID). Consumers (agentop)
	// use it to fold the OPEN into the decrypted inner request a TLS bridge
	// produces — an explicit producer signal rather than inferring "tunnel" from
	// host/extension shape, which an ordinary unparsed request could otherwise
	// mimic. Only the request-phase row folds; a close row never does.
	//
	// A bridged tunnel whose decrypted requests carry their own responses records
	// no close: its open folds into the first of them. Every other tunnel does —
	// opaque ones when they end, a failed dial at once, and a bridged one that
	// carried no request at all.
	Tunnel bool

	// BytesUp and BytesDown are how many bytes went each way: up is client to
	// destination, down is destination to client. A row carries whichever side it
	// is in a position to know, so the two never collide on one row:
	//
	//   request row        BytesUp   — the request body as FORWARDED, which is
	//                                  len(pctx.Body) at record time and so
	//                                  reflects any plugin that rewrote it
	//   response row       BytesDown — the response body the listener observed
	//                                  FROM THE DESTINATION, read from
	//                                  pctx.ResponseBytes
	//   tunnel close row   both      — the opaque bytes the tunnel carried
	//
	// The two sides differ on where the pipeline sits, and not by preference: the
	// request figure is read at record time, after any rewrite, because that is
	// when pctx.Body is what will be forwarded; the response figure is summed as
	// the bytes arrive, because on a streamed response there is no later moment
	// at which the whole body exists. A response mutator's rewrite is therefore
	// not reflected — extproc records the row before it even emits the
	// replacement — so BytesDown is what the destination sent.
	//
	// ZERO MEANS NOT COUNTED, NOT ZERO BYTES, and the field is absent from the
	// wire when zero. Body buffering is gated on a pipeline capability in all
	// three listeners, so a request no plugin wanted the body of is relayed
	// without ever being measured — and a body-less GET is indistinguishable from
	// it, which is why neither may render as a real 0 B. A bridged tunnel's close
	// reports zero for the same reason: its bytes were TLS the bridge terminated
	// rather than counted, and its decrypted inner requests carry their own
	// figures. Consumers render zero as blank (agentop's bytesCell).
	//
	// Headers are deliberately not counted. The question these answer is how big
	// a payload was — a prompt that overran a model's context window is the case
	// they exist for — and a kilobyte of constant header noise on every row
	// obscures it. They are therefore NOT a wire-cost figure.
	BytesUp   int64
	BytesDown int64

	// TunnelReason says WHY the bytes were left opaque. Empty when Tunnel is
	// false, and empty on a bridged CONNECT (agentop folds that row into the
	// decrypted inner request, whose own action is the interesting one).
	//
	// It exists because "tunnel" with no reason is indistinguishable from a
	// routine egress passthrough, and the two demand opposite responses: a
	// configured passthrough is working as intended, while a client that
	// rejected the bridge certificate means every plugin is blind to that
	// traffic and someone has to restart something. Diagnosing the latter
	// previously required the proxy log, the CA's NotBefore and a process
	// listing — none of which the timeline hinted at.
	TunnelReason TunnelReason

	// HTTPMethod and HTTPPath are the request's HTTP verb and its
	// query-stripped path, copied from the pipeline context at record
	// time.
	//
	// They exist because an event Cortex could not parse as A2A, MCP or
	// inference previously reached the timeline carrying only a host: the
	// operator saw that *something* went to an address, with no way to tell
	// a token refresh from an object download. The verb and path are the
	// cheapest thing that distinguishes them, and both were already on the
	// context — they simply never made it onto the event.
	//
	// Distinct from the Method on A2AExtension and MCPExtension, which is a
	// protocol-level method name ("message/stream", "tools/call") and not an
	// HTTP verb; hence the prefix on these two.
	//
	// HTTPPath is empty on an opaque tunnel, where the bytes are never parsed
	// as HTTP and there is no request line to read: recordTunnelOpened pairs
	// that empty path with a synthetic CONNECT. A blank path on a tunnel row
	// is therefore correct rather than missing plumbing. Both are empty when
	// the listener left the context fields unset.
	//
	// A query string is always stripped before the path gets here (see
	// pipeline.Context.Path), so query-borne credentials never reach the
	// timeline. What is recorded is the DECODED path, not the raw request
	// target: every listener mode runs the target through net/url, so
	// "/x/..%2f..%2fetc/passwd" is stored as "/x/../../etc/passwd". That is
	// deliberate cross-mode parity (net/http decodes identically for the proxy
	// listeners), but it means the timeline does not preserve how a caller
	// encoded a path — read it as the resolved path, and do not infer from it
	// that no encoded-traversal attempt was made. A secret embedded in a path SEGMENT does survive, because
	// nothing can tell it from a resource id — a bot token or a webhook path
	// lands here verbatim. That is the same exposure Host already carried on
	// this unauthenticated surface, which serves request bodies besides; it is
	// worth knowing before these events are exported off-box.
	//
	// HTTPPath duplicates the per-invocation Invocation.Path (serialized as
	// "path"), deliberately: both are copies of the same Context.Path, so the
	// two keys cannot disagree. They differ in when they are THERE, which is
	// why this one exists. HTTPPath is on every event the listener recorded
	// from a parsed HTTP request, whether or not a plugin ran; Invocation.Path
	// appears only where some plugin recorded an invocation. An event can
	// carry invocations and no HTTPPath (an opaque tunnel that ran a gate), so
	// a consumer that wants "the path of this request" should read HTTPPath
	// and treat Invocation.Path as per-invocation context.
	HTTPMethod string
	HTTPPath   string

	// Client is the coding agent that made the request, parsed from the request's
	// User-Agent by Context.ClientInfo. Nil when the request carried none —
	// ABSENCE, not an agent named "unknown"; EventClient.Label() is the only place
	// absence becomes a display string, so a consumer that aggregates on Label
	// gets a reserved bucket rather than a fabricated agent name.
	//
	// CLIENT-ASSERTED AND TRIVIALLY SPOOFABLE: an observability and
	// cost-attribution key, never an authorization subject. Deliberately the same
	// caveat, in the same words, that Context.Session carries about client-asserted
	// session ids. Distinct from Identity above, which is the AUTHENTICATED
	// principal — the two sit on the same event and answer different questions
	// ("what program is calling" versus "who is calling"), and treating this one as
	// the other means building authorization on a request header.
	Client *EventClient
}

// TunnelReason is why an opaque tunnel stayed opaque.
//
// A named type rather than a bare string so the compiler catches a typo on the
// producer side and on every consumer: these values are decoded from the wire,
// enumerated in the operator docs and pinned by tests, and a misspelling in any of
// those places would otherwise be a value that renders as itself and means nothing.
// It marshals as a plain JSON string, so the wire contract is unchanged.
type TunnelReason string

// Tunnel reasons. Stable strings: agentop renders them and operators grep them.
//
// Each is at most 18 characters, which is the width of agentop's PLUGIN column
// (events_columns.go). A longer value truncates in the cell, which would break the
// one property that makes these useful — that the token in the timeline is the same
// token you grep for in the proxy log.
const (
	// TunnelClientRejectedCA — the client sent a TLS alert rejecting our leaf, so it
	// does not trust the bridge CA. Claimed ONLY on a "remote error: tls:
	// bad certificate" / "unknown certificate authority", which is the peer actively
	// refusing us. Usually a process that started before the CA was minted, since CA
	// files are read once at startup.
	TunnelClientRejectedCA TunnelReason = "client-rejected-ca"
	// TunnelClientHungUp — the client disappeared mid-handshake without sending an
	// alert. CA distrust is one cause, but so is a cancelled request or a dead
	// socket, so this deliberately does NOT tell anyone to restart anything.
	TunnelClientHungUp TunnelReason = "client-hung-up"
	// TunnelHandshakeFailed — the handshake failed for some other reason: a version,
	// cipher or ALPN mismatch, or our own certificate minting failing. Not the
	// client's fault as far as we can tell, so it gets no client-side advice. The
	// logged error= field carries the specific cause.
	TunnelHandshakeFailed TunnelReason = "handshake-failed"
	// TunnelOriginUnverified — WE could not verify the origin, so bridging would have
	// meant vouching for a certificate we could not check.
	TunnelOriginUnverified TunnelReason = "origin-unverified"
	// TunnelSkipCached — an earlier handshake FAILURE for this host is still inside the
	// skip window, so no interception was attempted at all.
	//
	// Deliberately does not say why that earlier attempt failed. The skip is seeded by
	// any failed forge — a rejection, a hang-up, a cipher mismatch — because in every
	// one of those the connection died mid-handshake and the client's retry needs a
	// tunnel to work at all. Claiming "another client rejected the CA" here would be
	// right only some of the time. The seeding failure logged its own specific reason
	// when it happened; that is where the why lives.
	//
	// Distinct from client-rejected-ca either way: THIS client may well trust the CA
	// and is being tunnelled because an earlier one had trouble.
	TunnelSkipCached TunnelReason = "skip-cached"
	// TunnelProgramRefused — an earlier handshake by the program that opened this
	// connection failed, so no interception was attempted. Usually that failure was a CA
	// rejection, but, as with skip-cached, any failed handshake seeds it: the connection
	// died mid-handshake either way, and the program's retry needs a tunnel. The earlier
	// failure's proxy.log line names the program (program=, and agent= when it runs
	// under one) and gives its own reason.
	//
	// Recorded against the client's program (tlsbridge.Program), not the host, so
	// another program talking to the same host is still bridged. The window starts at
	// 30s and doubles only on rejections, and a rejection counts only once the window
	// before it has ended; after three rejections in a row the program is not retried
	// until Cortex restarts. A hang-up or a cipher mismatch never adds to the count or
	// lengthens the window, but when it is the program's first failure it starts the
	// count at one, so a hang-up followed by two spaced rejections stops the program. A
	// process that started before the bridge CA is recorded on its own instead
	// (Program.ProcessKey): it cannot have loaded that CA, so its refusal says nothing
	// about the program, and restarting that process is enough.
	TunnelProgramRefused TunnelReason = "program-refused"
	// TunnelOSTrustOnly — the program trusts only the operating system's certificate
	// store, and the store does not trust the bridge CA, so no leaf was shown: it could
	// only have been refused. Today that is a Go program on macOS (tlsbridge.ClientTrust),
	// which reads no CA file there. The connection works from its first try; adding the
	// CA to the keychain is what has Cortex read it.
	TunnelOSTrustOnly TunnelReason = "os-trust-only"
	// TunnelBridgeDisabled — no TLS bridge is configured.
	TunnelBridgeDisabled TunnelReason = "bridge-disabled"
	// TunnelPassthroughPort, TunnelPassthroughNonTLS and TunnelPassthroughHost mirror
	// Decision.Classify's own reasons for declining to intercept. All three are
	// working as intended.
	TunnelPassthroughPort   TunnelReason = "passthrough-port"
	TunnelPassthroughNonTLS TunnelReason = "passthrough-nontls"
	TunnelPassthroughHost   TunnelReason = "passthrough-host"
	// TunnelDialFailed — the proxy could not reach the destination at all, so no
	// tunnel ever opened. Recorded with a 502 close row carrying the dial error, so
	// an unreachable destination shows in the timeline instead of leaving no trace.
	TunnelDialFailed TunnelReason = "dial-failed"
	// TunnelPassthroughUnknown is the fallback when a lower layer declines to
	// intercept for a reason this vocabulary does not yet name. It exists so that
	// case can never produce the EMPTY string, which a consumer reads as "bridged" —
	// the exact opposite of what happened, and invisible in a timeline.
	TunnelPassthroughUnknown TunnelReason = "passthrough-unknown"
)

// EventTLS describes the TLS state of a connection that produced a
// session event. Populated by the reverse-proxy listener when mTLS is
// enabled and the inbound connection completed a TLS handshake;
// otherwise nil.
type EventTLS struct {
	// Version is the negotiated TLS version, e.g. "TLS 1.3".
	Version string `json:"version,omitempty"`

	// CipherSuite is the negotiated cipher suite name, e.g.
	// "TLS_AES_128_GCM_SHA256".
	CipherSuite string `json:"cipherSuite,omitempty"`

	// PeerSPIFFEID is the URI SAN of the verified peer cert when it
	// is a SPIFFE URI; otherwise empty. Empty means either the peer
	// cert had no SPIFFE URI (workload outside SPIRE) or had multiple
	// (non-conformant cert).
	PeerSPIFFEID string `json:"peerSpiffeId,omitempty"`
}

// NewEventTLS builds an EventTLS from a *tls.ConnectionState (the
// shape http.Request.TLS exposes). peerSpiffeID is the caller's
// pre-extracted SPIFFE URI — passed in rather than re-extracted here
// to keep this package free of a core/tlsconfig dependency. Returns
// nil when state is nil so callers can pass r.TLS unconditionally.
func NewEventTLS(state *tls.ConnectionState, peerSpiffeID string) *EventTLS {
	if state == nil {
		return nil
	}
	return &EventTLS{
		Version:      tlsVersionString(state.Version),
		CipherSuite:  tls.CipherSuiteName(state.CipherSuite),
		PeerSPIFFEID: peerSpiffeID,
	}
}

// tlsVersionString maps the uint16 TLS version to its human name. Go
// 1.21+ exposes tls.VersionName but we render the canonical "TLS X.Y"
// form for consistency with operator expectations.
func tlsVersionString(v uint16) string {
	switch v {
	case tls.VersionTLS13:
		return "TLS 1.3"
	case tls.VersionTLS12:
		return "TLS 1.2"
	case tls.VersionTLS11:
		return "TLS 1.1"
	case tls.VersionTLS10:
		return "TLS 1.0"
	}
	return ""
}

// MarshalJSON emits SessionEvent in a form consumable by off-process clients:
// Direction and Phase become strings instead of opaque int enums, and Duration
// is emitted as milliseconds (the field name reflects the unit). The default
// json marshaler would stringify enums as numbers and duration as nanoseconds
// — both awkward for CLI / dashboard consumption.
// sessionEventWire is the on-the-wire shape for SessionEvent. MarshalJSON
// writes to it directly; UnmarshalJSON reads into it and converts back.
// Keeping the layout in one place guarantees round-trip symmetry.
type sessionEventWire struct {
	SessionID string `json:"sessionId,omitempty"`
	// omitempty for the same skew reason as the fields at the bottom of this struct:
	// a new agentop against a proxy that predates paging decodes 0 and can tell that
	// this event carries no cursor, rather than mistaking it for the first event of
	// the session.
	Seq         uint64                     `json:"seq,omitempty"`
	At          time.Time                  `json:"at"`
	Direction   Direction                  `json:"direction"`
	Phase       SessionPhase               `json:"phase"`
	RequestID   string                     `json:"requestId,omitempty"`
	A2A         *A2AExtension              `json:"a2a,omitempty"`
	MCP         *MCPExtension              `json:"mcp,omitempty"`
	Inference   *InferenceExtension        `json:"inference,omitempty"`
	Invocations *Invocations               `json:"invocations,omitempty"`
	Plugins     map[string]json.RawMessage `json:"plugins,omitempty"`
	Identity    *EventIdentity             `json:"identity,omitempty"`
	StatusCode  int                        `json:"statusCode,omitempty"`
	Error       *EventError                `json:"error,omitempty"`
	Host        string                     `json:"host,omitempty"`
	// omitempty for the same skew reason as TunnelReason: an old agentop ignores the
	// key, and an event from a proxy that predates redirects decodes to "".
	RequestedHost string    `json:"requestedHost,omitempty"`
	DurationMs    int64     `json:"durationMs,omitempty"`
	TLS           *EventTLS `json:"tls,omitempty"`
	Tunnel        bool      `json:"tunnel,omitempty"`
	// omitempty so both skew directions are safe: an old agentop ignores an unknown
	// key, and a new agentop against an old proxy sees "" and renders exactly what it
	// renders today.
	TunnelReason TunnelReason `json:"tunnelReason,omitempty"`
	// omitempty for the same skew reason, and because a row reports only the side
	// it knows: a request row has no BytesDown and a response row no BytesUp. The
	// absent key and a counted zero are therefore the same wire bytes, which is
	// intended — both mean "no figure", per SessionEvent.BytesUp.
	BytesUp   int64 `json:"bytesUp,omitempty"`
	BytesDown int64 `json:"bytesDown,omitempty"`
	// omitempty for the same skew reason as TunnelReason above: an old agentop
	// ignores keys it does not know, and a new agentop against a proxy that
	// predates these fields sees "" and renders what it renders today.
	HTTPMethod string `json:"httpMethod,omitempty"`
	HTTPPath   string `json:"httpPath,omitempty"`
	// omitempty, and a POINTER, so both skew directions are safe and absence stays
	// absence: an old agentop ignores a key it does not know, an event recorded before
	// this field existed decodes to nil rather than to an empty struct, and a
	// request that sent no User-Agent emits no key at all. A value type here would
	// make "no client" and "a client that named nothing" the same wire bytes.
	Client *EventClient `json:"client,omitempty"`
}

func (e SessionEvent) MarshalJSON() ([]byte, error) {
	return json.Marshal(sessionEventWire{
		SessionID:     e.SessionID,
		Seq:           e.Seq,
		At:            e.At,
		Direction:     e.Direction,
		Phase:         e.Phase,
		RequestID:     e.RequestID,
		A2A:           e.A2A,
		MCP:           e.MCP,
		Inference:     e.Inference,
		Invocations:   e.Invocations,
		Plugins:       e.Plugins,
		Identity:      e.Identity,
		StatusCode:    e.StatusCode,
		Error:         e.Error,
		Host:          e.Host,
		RequestedHost: e.RequestedHost,
		DurationMs:    e.Duration.Milliseconds(),
		TLS:           e.TLS,
		Tunnel:        e.Tunnel,
		TunnelReason:  e.TunnelReason,
		BytesUp:       e.BytesUp,
		BytesDown:     e.BytesDown,
		HTTPMethod:    e.HTTPMethod,
		HTTPPath:      e.HTTPPath,
		Client:        e.Client,
	})
}

// UnmarshalJSON accepts the on-the-wire form written by MarshalJSON. This
// makes SessionEvent round-trippable through JSON so off-process clients
// (e.g. agentop) can decode straight into the canonical type.
func (e *SessionEvent) UnmarshalJSON(data []byte) error {
	var w sessionEventWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	*e = SessionEvent{
		SessionID:     w.SessionID,
		Seq:           w.Seq,
		At:            w.At,
		Direction:     w.Direction,
		Phase:         w.Phase,
		RequestID:     w.RequestID,
		A2A:           w.A2A,
		MCP:           w.MCP,
		Inference:     w.Inference,
		Invocations:   w.Invocations,
		Plugins:       w.Plugins,
		Identity:      w.Identity,
		StatusCode:    w.StatusCode,
		Error:         w.Error,
		Host:          w.Host,
		RequestedHost: w.RequestedHost,
		Duration:      time.Duration(w.DurationMs) * time.Millisecond,
		TLS:           w.TLS,
		Tunnel:        w.Tunnel,
		TunnelReason:  w.TunnelReason,
		BytesUp:       w.BytesUp,
		BytesDown:     w.BytesDown,
		HTTPMethod:    w.HTTPMethod,
		HTTPPath:      w.HTTPPath,
		Client:        w.Client,
	}
	return nil
}

// EventIdentity carries the "who" for a session event.
type EventIdentity struct {
	Subject  string   `json:"subject,omitempty"`
	Scopes   []string `json:"scopes,omitempty"`
	ClientID string   `json:"clientId,omitempty"`
	AgentID  string   `json:"agentId,omitempty"`
}

// EventError describes why a response event represents a failure.
type EventError struct {
	Kind    string `json:"kind"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"` // human-readable reason; safe to surface in logs/metrics
}

// SessionView is a read-only snapshot of a session, safe to pass to plugins.
// It contains a copy of events — plugins cannot mutate the store.
//
// ID is the session identity: stable for the life of one session, and what a
// plugin should key per-session state on. The listener resolves it (see
// Context.Session for the order, and for why it may be client-asserted), so a
// plugin must not re-derive it from headers or bodies of its own accord — two
// plugins deriving it differently is how per-session state drifts apart.
//
// Events is a snapshot and may be EMPTY for a session that genuinely exists: a
// view is available as soon as the id is known, which is before that session's
// first event is recorded. The accessors below therefore return nothing on a
// fresh session, which is a truthful answer rather than a missing one. None of
// them distinguishes "this session has no such events" from "this session is
// new", so a plugin that needs to tell those apart must key on ID and track it,
// not infer it from an empty result.
type SessionView struct {
	ID     string         `json:"id"`
	Events []SessionEvent `json:"events"`

	// TotalEvents is how many events the session holds, which is more than
	// len(Events) when the view is a tail of a longer session.
	//
	// Absent means Events IS the session: every caller that builds a whole view
	// leaves this zero, so a consumer that ignores the field sees exactly what it
	// saw before the field existed. A consumer that reads it can tell "the timeline
	// starts here" from "the timeline starts where I can see", which is otherwise
	// indistinguishable — and was, for a 5000-event session whose snapshot request
	// simply timed out with nothing to show for it.
	TotalEvents int `json:"totalEvents,omitempty"`

	// OldestSeq is the Seq of the oldest event the session still HOLDS — not the
	// oldest in this response. It is what lets a paging client know when to stop:
	// having received an event with this Seq, there is nothing older to ask for.
	//
	// A count cannot answer that question. TotalEvents says how many events exist,
	// but eviction and the pinned intent make "how many" a poor guide to "how far
	// back can I go", and a client comparing counts would keep asking for pages the
	// store cannot produce. Comparing two Seq values is exact and survives events
	// being evicted between one page and the next.
	//
	// Absent when the view already starts at the oldest held event, so the common
	// whole-session response is unchanged — same reasoning as TotalEvents above.
	OldestSeq uint64 `json:"oldestSeq,omitempty"`

	// View names the projection the server applied, and is set only when one was:
	// "summary" means the message bodies were omitted (see sessionapi.summarizeEvent).
	//
	// An ECHO, not a request. It exists because agentop and the proxy install
	// separately, so a client asking for a summary cannot assume it got one — an
	// older proxy ignores the parameter and returns full events. Absence therefore
	// has a precise meaning to a client that asked: "this server does not project",
	// which is the difference between telling the operator their proxy predates the
	// feature and leaving them to wonder why opening a session is slow.
	//
	// Inferring it from the response instead would be wrong: a session with no
	// inference traffic has no message bodies to omit, so "no bodies present" does
	// not distinguish a projected response from a naturally empty one.
	View string `json:"view,omitempty"`
}

// Intents returns only inbound A2A request events (user messages).
func (v *SessionView) Intents() []SessionEvent {
	var out []SessionEvent
	for _, e := range v.Events {
		if e.Direction == Inbound && e.Phase == SessionRequest && e.A2A != nil {
			out = append(out, e)
		}
	}
	return out
}

// ToolCalls returns only outbound MCP request events.
func (v *SessionView) ToolCalls() []SessionEvent {
	var out []SessionEvent
	for _, e := range v.Events {
		if e.Direction == Outbound && e.Phase == SessionRequest && e.MCP != nil {
			out = append(out, e)
		}
	}
	return out
}

// ToolResponses returns only outbound MCP response events.
func (v *SessionView) ToolResponses() []SessionEvent {
	var out []SessionEvent
	for _, e := range v.Events {
		if e.Direction == Outbound && e.Phase == SessionResponse && e.MCP != nil {
			out = append(out, e)
		}
	}
	return out
}

// InferenceRequests returns only outbound inference request events.
func (v *SessionView) InferenceRequests() []SessionEvent {
	var out []SessionEvent
	for _, e := range v.Events {
		if e.Direction == Outbound && e.Phase == SessionRequest && e.Inference != nil {
			out = append(out, e)
		}
	}
	return out
}

// LastIntent returns the most recent inbound A2A user message, or nil.
func (v *SessionView) LastIntent() *SessionEvent {
	for i := len(v.Events) - 1; i >= 0; i-- {
		e := v.Events[i]
		if e.Direction == Inbound && e.Phase == SessionRequest && e.A2A != nil {
			return &v.Events[i]
		}
	}
	return nil
}

// Len returns total event count.
func (v *SessionView) Len() int { return len(v.Events) }

// FailedEvents returns response-phase events that carry an Error.
func (v *SessionView) FailedEvents() []SessionEvent {
	var out []SessionEvent
	for _, e := range v.Events {
		if e.Phase == SessionResponse && e.Error != nil {
			out = append(out, e)
		}
	}
	return out
}

// LastError returns the most recent response event with an Error, or nil.
func (v *SessionView) LastError() *SessionEvent {
	for i := len(v.Events) - 1; i >= 0; i-- {
		if v.Events[i].Phase == SessionResponse && v.Events[i].Error != nil {
			return &v.Events[i]
		}
	}
	return nil
}

// ArchiveUsage is the session archive's size, bounds and losses, as `GET /v1/sessions?archived=true`
// reports them beside the sessions it lists.
//
// HERE RATHER THAN IN core/session/archive so a client decodes it without linking the archive's
// zstd encoder: agentop reads this and nothing else of the archive.
type ArchiveUsage struct {
	Bytes         int64 `json:"bytes"`
	MaxBytes      int64 `json:"maxBytes"`
	RetentionDays int   `json:"retentionDays"`
	// DroppedEvents, WriteErrors and DroppedRenames are what never reached disk, process-wide.
	// Nonzero means the history shown has gaps; see core/session/archive.Stats.
	DroppedEvents  uint64 `json:"droppedEvents,omitempty"`
	WriteErrors    uint64 `json:"writeErrors,omitempty"`
	DroppedRenames uint64 `json:"droppedRenames,omitempty"`
	// Paused is true after the disk filled, until the archive's next retention pass.
	Paused bool `json:"paused,omitempty"`
}
