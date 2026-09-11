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
	// listener records this instead of a Request/Response pair so abctl
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
	// authbridge/docs/plugin-reference.md for the producer contract.
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

	// Duration is the wall-clock time from request entry into the listener
	// to response recording. Zero on request-phase events. On response
	// events it's computed as now - matching-request.At.
	Duration time.Duration

	// TLS, when non-nil, carries connection-level identity for events
	// that arrived over TLS: negotiated version, cipher, and the peer's
	// SPIFFE ID extracted from the URI SAN. Nil for plaintext events
	// and for events recorded before the TLS handshake completed.
	// Populated by the listener layer; plugins do not write to it.
	TLS *EventTLS

	// Tunnel marks an opaque CONNECT / transparent-redirect tunnel-open: the
	// bytes are not HTTP, so there's no protocol parse. Set only by
	// recordTunnelOpened. Consumers (abctl) use it to fold the tunnel into the
	// decrypted inner request a TLS bridge produces — an explicit producer
	// signal rather than inferring "tunnel" from host/extension shape, which
	// an ordinary unparsed request could otherwise mimic.
	Tunnel bool

	// TunnelReason says WHY the bytes were left opaque. Empty when Tunnel is
	// false, and empty on a bridged CONNECT (abctl folds that row into the
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
	// the listener left the context fields unset — ext_authz never populates
	// Method, though it records no session events either.
	//
	// A query string is always stripped before the path gets here (see
	// pipeline.Context.Path), so query-borne credentials never reach the
	// timeline. A secret embedded in a path SEGMENT does survive, because
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
}

// TunnelReason is why an opaque tunnel stayed opaque.
//
// A named type rather than a bare string so the compiler catches a typo on the
// producer side and on every consumer: these values are decoded from the wire,
// enumerated in the operator docs and pinned by tests, and a misspelling in any of
// those places would otherwise be a value that renders as itself and means nothing.
// It marshals as a plain JSON string, so the wire contract is unchanged.
type TunnelReason string

// Tunnel reasons. Stable strings: abctl renders them and operators grep them.
//
// Each is at most 18 characters, which is the width of abctl's PLUGIN column
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
	// TunnelBridgeDisabled — no TLS bridge is configured.
	TunnelBridgeDisabled TunnelReason = "bridge-disabled"
	// TunnelPassthroughPort, TunnelPassthroughNonTLS and TunnelPassthroughHost mirror
	// Decision.Classify's own reasons for declining to intercept. All three are
	// working as intended.
	TunnelPassthroughPort   TunnelReason = "passthrough-port"
	TunnelPassthroughNonTLS TunnelReason = "passthrough-nontls"
	TunnelPassthroughHost   TunnelReason = "passthrough-host"
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
// to keep this package free of an authlib/tls dependency. Returns
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
	SessionID   string                     `json:"sessionId,omitempty"`
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
	DurationMs  int64                      `json:"durationMs,omitempty"`
	TLS         *EventTLS                  `json:"tls,omitempty"`
	Tunnel      bool                       `json:"tunnel,omitempty"`
	// omitempty so both skew directions are safe: an old abctl ignores an unknown
	// key, and a new abctl against an old proxy sees "" and renders exactly what it
	// renders today.
	TunnelReason TunnelReason `json:"tunnelReason,omitempty"`
	// omitempty for the same skew reason as TunnelReason above: an old abctl
	// ignores keys it does not know, and a new abctl against a proxy that
	// predates these fields sees "" and renders what it renders today.
	HTTPMethod string `json:"httpMethod,omitempty"`
	HTTPPath   string `json:"httpPath,omitempty"`
}

func (e SessionEvent) MarshalJSON() ([]byte, error) {
	return json.Marshal(sessionEventWire{
		SessionID:    e.SessionID,
		At:           e.At,
		Direction:    e.Direction,
		Phase:        e.Phase,
		RequestID:    e.RequestID,
		A2A:          e.A2A,
		MCP:          e.MCP,
		Inference:    e.Inference,
		Invocations:  e.Invocations,
		Plugins:      e.Plugins,
		Identity:     e.Identity,
		StatusCode:   e.StatusCode,
		Error:        e.Error,
		Host:         e.Host,
		DurationMs:   e.Duration.Milliseconds(),
		TLS:          e.TLS,
		Tunnel:       e.Tunnel,
		TunnelReason: e.TunnelReason,
		HTTPMethod:   e.HTTPMethod,
		HTTPPath:     e.HTTPPath,
	})
}

// UnmarshalJSON accepts the on-the-wire form written by MarshalJSON. This
// makes SessionEvent round-trippable through JSON so off-process clients
// (e.g. abctl) can decode straight into the canonical type.
func (e *SessionEvent) UnmarshalJSON(data []byte) error {
	var w sessionEventWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	*e = SessionEvent{
		SessionID:    w.SessionID,
		At:           w.At,
		Direction:    w.Direction,
		Phase:        w.Phase,
		RequestID:    w.RequestID,
		A2A:          w.A2A,
		MCP:          w.MCP,
		Inference:    w.Inference,
		Invocations:  w.Invocations,
		Plugins:      w.Plugins,
		Identity:     w.Identity,
		StatusCode:   w.StatusCode,
		Error:        w.Error,
		Host:         w.Host,
		Duration:     time.Duration(w.DurationMs) * time.Millisecond,
		TLS:          w.TLS,
		Tunnel:       w.Tunnel,
		TunnelReason: w.TunnelReason,
		HTTPMethod:   w.HTTPMethod,
		HTTPPath:     w.HTTPPath,
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
type SessionView struct {
	ID     string         `json:"id"`
	Events []SessionEvent `json:"events"`
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
