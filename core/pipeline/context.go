package pipeline

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/rossoctl/cortex/core/contracts"
)

// Identity carries the subject identity established by whichever auth
// plugin ran — jwt-validation, SAML, mTLS, custom. Populated by the
// auth plugin via pctx.Identity = <adapter>. Listener reads via these
// methods — not via concrete-type assertion — so plugins can contribute
// any identity shape without the framework caring.
//
// Returning empty-string / nil from any method is valid (e.g., a
// SPIFFE-SVID authenticator may have no "ClientID" concept). Consumers
// that need plugin-specific fields type-assert to a richer interface
// or to the plugin's known concrete type.
type Identity interface {
	Subject() string  // stable subject ID (sub claim / SPIFFE ID / email)
	ClientID() string // registering-client ID, if applicable
	Scopes() []string // granted scopes / roles
}

// SharedStore is a process-scoped key→value store with TTL, injected by the
// listener so plugins can share state across the inbound→outbound request
// boundary (e.g. credential placeholders). Implemented by core/shared.Store.
type SharedStore interface {
	Put(key string, val any, ttl time.Duration)
	Get(key string) (any, bool)
	Delete(key string)
}

// Direction indicates whether a request is inbound (caller → this agent) or
// outbound (this agent → target service).
type Direction int

const (
	Inbound Direction = iota
	Outbound
)

// String returns "inbound" / "outbound". Used for structured logs and the
// wire format of SessionEvent.
func (d Direction) String() string {
	switch d {
	case Inbound:
		return "inbound"
	case Outbound:
		return "outbound"
	default:
		return "unknown"
	}
}

// MarshalJSON emits the string form ("inbound"/"outbound") so the wire
// format is human-readable without an enum→int lookup.
func (d Direction) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

// UnmarshalJSON decodes a Direction from the string form emitted by
// MarshalJSON. Unknown strings decode to Inbound (zero value) without
// error so downstream consumers stay tolerant of forward-compatible
// additions. A Debug-level log fires on unknown input so wire-format
// drift is at least observable in a verbose test run.
func (d *Direction) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	switch s {
	case "inbound":
		*d = Inbound
	case "outbound":
		*d = Outbound
	default:
		slog.Debug("pipeline: unknown Direction, defaulting to inbound", "value", s)
		*d = Inbound
	}
	return nil
}

// Context is the shared state passed through the plugin pipeline.
// Plugins read and mutate fields directly — there is no separate mutation API.
type Context struct {
	Direction Direction
	Method    string
	// Scheme is the URL scheme of the request, typically "http" or
	// "https" — future transports ("ws" / "wss" / gRPC-specific
	// schemes) pass through unchanged as free-form strings. Populated
	// by the listener at pctx construction from the transport-native
	// field: the :scheme pseudo-header in ext_proc,
	// r.URL.Scheme in the forward and reverse proxies.
	//
	// Empty when the listener can't determine scheme (legacy test
	// fixtures, an unrecognized transport, etc.). Plugins that need a
	// concrete scheme should pick a default explicitly — treating ""
	// as "assume http" would silently mask missing listener plumbing.
	Scheme string
	Host   string

	// Path is the URL path of the request, never including a query
	// string: the proxy listeners populate it from r.URL.Path, and
	// ext_proc runs the raw request target through the same
	// URL parser (httpx.PathOnly → url.ParseRequestURI), so the value
	// is identical across listener modes — modulo unparseable targets,
	// which net/http rejects with 400 before any pipeline runs and the
	// Envoy-fed listeners keep query-stripped but otherwise raw.
	// Plugins may match, log, or feed Path into policy without
	// stripping a query themselves.
	Path string

	Headers http.Header
	Body    []byte // nil unless at least one plugin declares BodyAccess: true

	// StartedAt is the wall-clock time this context was constructed by the
	// listener at the start of a request. Used on the response path to
	// compute SessionEvent.Duration without walking the event history.
	StartedAt time.Time

	// requestID backs RequestID(), which generates it on first use. See
	// requestid.go for why it is lazy rather than a constructor argument.
	requestID string

	// client and clientParsed memoize ClientInfo's answer.
	//
	// A separate flag rather than a nil check on client, because nil IS a valid
	// answer — a request with no User-Agent — and a nil-guard memo would re-parse
	// on every call for precisely those requests. Each turn calls this at least
	// twice, once per session event, on the request path.
	//
	// Unexported so this stays ONE resolution with one owner. A plugin that could
	// write it could re-file another program's spend under a name of its choosing,
	// and a listener that could write it would be a second source of a truth the
	// context already holds in Headers.
	//
	// WHEN the memo is filled is part of the guarantee rather than an implementation
	// detail: see ResolveClient, which listeners call at construction so the answer
	// predates every plugin.
	client       *EventClient
	clientParsed bool

	Agent    *AgentIdentity
	Identity Identity // nil before an auth plugin runs

	// Session is the session this request belongs to, and the identity a plugin
	// should key any per-session state on (Redis counters, engine state,
	// correlation). Read-only; see SessionView.
	//
	// Three things a plugin must know about it:
	//
	//   - It may be nil, and nil means "no session identity is known" — not
	//     "sessions are off". A plugin that needs one must decide what to do
	//     without it rather than assume a fallback bucket; being handed a shared
	//     bucket instead would silently pool unrelated traffic under one key.
	//   - Events may be empty while ID is set. A session's first request is
	//     hydrated before its first event is recorded, so an empty view means
	//     "this session, nothing recorded yet". Key on ID regardless; treating an
	//     empty view as absent skips the first request of every session.
	//   - ID may be CLIENT-ASSERTED. Listeners resolve it from a client-supplied
	//     header where one is configured (see session.IDFromHeaders), falling
	//     back to the most recently active session and then, for recording only,
	//     a shared default bucket. It is validated but not authenticated, so it
	//     is an observability and correlation key, not an authorization subject.
	Session *SessionView

	// OutboundSessionID pins the session bucket the outbound REQUEST event was
	// recorded under, so the paired RESPONSE event lands in the same session.
	// recordOutboundResponseEvent reuses it instead of re-resolving
	// Store.ActiveSession() at response time: ActiveSession() returns the global
	// "most-recently-updated session", which interleaving traffic (e.g. a health
	// probe bucketed under "default") can flip between a streaming request and
	// its response, mis-filing the response into the wrong session. Empty when
	// no request event was recorded (skip_hosts, sessions disabled), in which
	// case the response recorder falls back to ActiveSession().
	//
	// Listeners MUST assign this only after the request pipeline has run, and
	// must not read it back as the source of truth for recording. It is
	// exported, so a plugin can write it; the listener's own resolution is held
	// in a local for exactly that reason, and assigning last means a plugin
	// write is overwritten rather than able to silently re-file this request and
	// its response into a session of the plugin's choosing. Plugins wanting to
	// influence attribution should say so through their own invocation records,
	// where it is visible.
	OutboundSessionID string

	// TLS is the connection state of the inbound TLS handshake when
	// the request arrived over TLS, nil otherwise. Populated by the
	// reverse-proxy listener; nil for plaintext callers (UI, curl,
	// healthchecks), outbound contexts, and any path that doesn't go
	// through the proxy-sidecar reverse-proxy (envoy-sidecar mode
	// terminates TLS in Envoy and never populates this field).
	//
	// Plugins that want per-caller policy use the convenience method
	// PeerCertificate() to get the leaf cert and core/tls.PeerSPIFFEID
	// to extract the URI SAN. Listeners use it to populate
	// SessionEvent.TLS for the observability surface.
	TLS *tls.ConnectionState

	// Shared is the process-scoped store the listener injects. May be nil
	// when no store is wired; plugins that require it must fail closed.
	Shared SharedStore

	// Response-phase fields (populated by listener before RunResponse).
	// ResponseBody may be nil even during response phase if no plugin declared BodyAccess.
	StatusCode      int
	ResponseHeaders http.Header
	ResponseBody    []byte

	Extensions Extensions

	// responseDelivered says the response has already reached the client, so a refusal
	// recorded after it cannot be in effect; rejectedAfterDelivery remembers that the
	// rejection on record is one of those. See MarkResponseDelivered.
	responseDelivered     bool
	rejectedAfterDelivery bool

	// currentPlugin, currentPhase, and currentPolicy are framework-owned
	// fields set by Pipeline.Run / RunResponse around each plugin
	// dispatch. They feed the Record / Allow / Skip / Observe / Modify /
	// DenyAndRecord helper methods so plugin code doesn't have to repeat
	// its own Name(), the phase it's in, or the direction on every
	// Invocation literal. currentPolicy is consulted by SetBody /
	// SetResponseBody to suppress mutation propagation when the current
	// plugin runs under ErrorPolicyObserve. Unexported so plugins can
	// only set them indirectly (via the framework).
	currentPlugin string
	currentPhase  InvocationPhase
	currentPolicy ErrorPolicy

	// bodyMutated / responseBodyMutated flag that a plugin called
	// SetBody / SetResponseBody on this context. Listeners read the flag
	// via BodyMutated() / ResponseBodyMutated() after Run / RunResponse
	// to decide whether to emit a body mutation on the wire.
	//
	// Flags (not byte-comparison) because a mutator that rewrites to
	// byte-identical content still wants the Invocation recorded —
	// "tried to redact, nothing matched" is valid telemetry.
	bodyMutated         bool
	responseBodyMutated bool

	// dispatched lists the pipeline indices whose OnRequest was actually
	// invoked (including the plugin that denied, if any). Populated by
	// Pipeline.Run before each plugin's OnRequest call; consumed by
	// Pipeline.RunFinish to dispatch OnFinish in LIFO to exactly the set
	// of plugins that "reserved" per-request state.
	//
	// Indices (not plugin pointers) because the pipeline slice is the
	// authoritative owner of plugin identity; pctx avoids holding a back
	// reference so a pctx is safe to pass across pipelines in tests.
	dispatched []int

	// outcome is populated exactly once by Pipeline.RunFinish immediately
	// before dispatching OnFinish on the first plugin. Nil during OnRequest
	// and OnResponse. Read via Outcome() so the "nil outside OnFinish"
	// invariant is enforced at the call site rather than via a documented
	// zero-value contract.
	outcome *Outcome

	// inFinish is true while RunFinish is dispatching OnFinish on one
	// of the Finisher plugins. Read by Record / SetBody / SetResponseBody
	// to enforce the "SessionEvent is frozen during OnFinish" contract
	// chosen for the finish hook: plugins that accidentally call
	// Record / SetBody during cleanup hit a WARN log + no-op rather
	// than silently mutating a SessionEvent that has already been
	// published or a response that is already on the wire.
	inFinish bool

	// rejectingPlugin is populated by Pipeline.Run / RunResponse when
	// a plugin returns Action{Type: Reject} under the enforce policy.
	// It is the framework-authoritative source of "which plugin denied
	// this request," freeing OutcomeFromContext from having to walk
	// Invocations (and freeing plugin authors from the obligation to
	// pair Reject with a pctx.Record call). Stays empty on shadow
	// denials (policy == observe) and on allow paths.
	rejectingPlugin string

	// finished is set at RunFinish entry to prevent accidental double
	// dispatch from a buggy listener (two defers registered, a
	// refactor that routes the finish call through two paths). Second
	// call hits a WARN log and early-returns rather than
	// double-releasing every Finisher's state.
	finished bool
}

// ClientInfo returns the calling coding agent, parsed from this request's
// User-Agent and memoized.
//
// CLIENT-ASSERTED AND TRIVIALLY SPOOFABLE — an observability and cost-attribution
// key, never an authorization subject. See EventClient, and note that this is NOT
// Identity: that field is the authenticated principal, this one is a self-reported
// software label.
//
// Nil means no User-Agent was sent. Callers do not nil-check: EventClient.Label()
// is nil-safe and answers "unknown".
//
// Resolved HERE — once, on the context — rather than assigned by each listener or
// re-derived by each consumer. That follows the same doctrine Session states: the
// value is resolved in one place and never re-derived downstream, because two
// derivations of one fact are how the two drift apart. Session needs a listener to
// assign it since it comes from a store lookup; this one does not, because Context
// already carries the request headers, so an assignment would be a second source
// of a truth already present. It is also stronger than an assignment for the
// failure that actually happens: a listener-populated field can be forgotten at
// one of several context-construction sites and serialize a clean empty value,
// whereas an accessor over Headers cannot be.
//
// "Once" is a claim about WHICH ANSWER, and on its own it is weaker than it sounds:
// the memo fills on the FIRST CALL, and the first call is at an event-construction
// site downstream of the pipeline. Headers is mutable and plugins write to it, so a
// plugin that rewrote User-Agent would change what an event is attributed to, and
// which recording site asked first would decide the answer. What makes "once" an
// ordering guarantee too is ResolveClient, which the listeners call at construction:
// see there for the guarantee in full, and for what holds on a Context that skips it.
//
// NOT goroutine-safe, and that is correct: a Context belongs to one request and
// the pipeline runs its phases sequentially. Said explicitly because the
// surrounding type does have fields other goroutines read.
//
// THE PIN IS WHAT MAKES THAT SAFE FOR RECORDING SITES, WHICH ARE NOT PIPELINE PHASES. The
// first call writes clientParsed and client, unsynchronised, and the callers are recorders —
// a response-path recorder, and on ext_proc a streaming-response append — so two of them
// reaching a FIRST ClientInfo() on one Context is a data race, not merely a wrong label.
// ResolveClient having already filled the memo is what rules that out: after it, every call
// here is a pure read.
//
// A nil Headers map is fine and answers nil. A nil RECEIVER panics, unlike
// PeerCertificate above, and that difference is deliberate rather than an
// oversight: this mutates the memo, so it cannot be a no-op on nil, and every
// caller is an event-construction site that already dereferences pctx for Host and
// Method on adjacent lines. A guard here would convert a programming error into a
// silently unattributed event instead of a stack trace.
//
// A COPY IS RETURNED, NOT THE MEMO. Every caller is an event-construction site that stores
// the result on a SessionEvent, so handing out the memoized pointer would make ten events
// share one mutable struct, and a single write through it would relabel events already
// appended to the store and already being served by the session API. See snapshotClient: the
// same rule every other extension on that event follows. The memo is what stops the header
// being parsed ten times; it does not escape.
//
// THE COPY IS AN ALLOCATION PER CALL, roughly ten per request, and that is the price of the
// line above. sanitizeUA's fast path is written not to allocate, which bounds what a hostile
// header costs; this one is unconditional and buys the integrity claim instead.
func (c *Context) ClientInfo() *EventClient {
	if c.clientParsed {
		return snapshotClient(c.client)
	}
	c.clientParsed = true
	if c.Headers != nil {
		c.client = ParseUserAgent(c.Headers.Get("User-Agent"))
	}
	return snapshotClient(c.client)
}

// ResolveClient pins ClientInfo's answer to the User-Agent AS THE CLIENT SENT IT.
//
// Listeners call it immediately after building a Context from the request, and that call
// is the whole of the ordering guarantee: the label is resolved before the pipeline runs,
// so no plugin can change what an event is attributed to, and no recording site can get a
// different answer by asking first or last. Attribution keys cost — see EventClient — and
// "which program spent this" must not depend on call order.
//
// A named call rather than a bare `_ = pctx.ClientInfo()` at each site, so the line reads
// as the invariant it is and a later reader cannot mistake it for a leftover. Deleting it
// is a behaviour change, and the tests in forwardproxy's client_test.go say so.
//
// It does NOT replace the memo, and the memo is deliberately still lazy. An accessor over
// Headers answers correctly at a construction site that forgets this call — one answer,
// for the life of the context, from the headers as they stood when something first asked —
// where a listener-assigned field would have serialized a clean empty value instead.
//
// What a forgotten call costs is TWO things, not one. The ordering half: on such a Context the
// answer is pre-plugin by coincidence rather than by construction. And the concurrency half:
// this call is a pure read only AFTER the memo is filled, so without the pin whichever
// recording site asks first performs the write instead — see ClientInfo.
//
// Call it AFTER Headers is populated. Called before, it pins nil and the request's own
// User-Agent is lost — which is why this is a listener's call to make at construction and
// not something a constructor could do earlier.
//
// WHICH LISTENERS CALL IT, stated precisely because this is the authoritative answer to what
// "unknown" means on a given listener. In THIS change: the forward proxy, at three
// construction sites — serveOutbound, handleConnect and HandleTransparentConn. Arriving with
// the cost work later in this series: ext_proc, at its four, and the reverse proxy, at its
// one.
//
// Until the second half lands, an inbound event records no client and Label() answers
// "unknown" for it — while that string is documented as "this request carried no
// User-Agent". For those events nil means "this listener is not wired yet", which an
// operator reading a per-agent breakdown cannot tell from absence. Read "unknown" on
// inbound traffic as unattributed rather than as absent until then.
//
// Idempotent: the second call is the memo's own no-op.
func (c *Context) ResolveClient() { _ = c.ClientInfo() }

// PeerCertificate returns the verified peer leaf certificate from
// the TLS connection state, or nil when the connection was plaintext
// or carried no peer cert. Convenience accessor so plugins don't
// have to bounds-check the slice.
func (c *Context) PeerCertificate() *x509.Certificate {
	if c == nil || c.TLS == nil || len(c.TLS.PeerCertificates) == 0 {
		return nil
	}
	return c.TLS.PeerCertificates[0]
}

// SetCurrentPlugin is called by Pipeline.Run / RunResponse immediately
// before dispatching into a plugin's OnRequest / OnResponse. It stamps
// the plugin name and phase onto pctx so the Record family of helpers
// can fill those fields without plugin-side ceremony. Reset with
// ClearCurrentPlugin after dispatch.
//
// Exported (rather than framework-private) because listeners that embed
// pctx in their own dispatch loops (not strictly via Pipeline.Run) need
// to set the same fields to get consistent Invocation attribution.
// Production plugins should never call this directly.
//
// Sets the policy to the default (enforce). Call setCurrent (unexported)
// to stamp a non-default policy — only Pipeline.Run/RunResponse should
// supply a non-default, and they use the unexported path.
func (c *Context) SetCurrentPlugin(name string, phase InvocationPhase) {
	c.setCurrent(name, phase, ErrorPolicyEnforce)
}

// ClearCurrentPlugin resets the framework-owned attribution fields.
// Paired with SetCurrentPlugin.
func (c *Context) ClearCurrentPlugin() {
	c.clearCurrent()
}

// setCurrent stamps the per-dispatch attribution including the current
// plugin's on_error policy. Internal to the pipeline package; exported
// SetCurrentPlugin is the listener-facing entry point and defaults
// policy to enforce.
func (c *Context) setCurrent(name string, phase InvocationPhase, policy ErrorPolicy) {
	c.currentPlugin = name
	c.currentPhase = phase
	c.currentPolicy = policy.Resolved()
}

// clearCurrent zeroes the per-dispatch attribution fields.
func (c *Context) clearCurrent() {
	c.currentPlugin = ""
	c.currentPhase = ""
	c.currentPolicy = ""
}

// RejectingPlugin returns the name of the plugin whose Reject action
// stopped the pipeline, or "" if the pipeline allowed end-to-end.
// Populated by Pipeline.Run / RunResponse before they return; callable
// from OnFinish, listener code, or anywhere else that needs to know
// the denier without walking Invocations.
//
// Shadow-mode denials (policy == observe, where the plugin's Reject
// was converted to a pass-through) do not set this field — the
// framework treats shadow rejections as "the pipeline effectively
// allowed," which matches how abctl and the session store classify
// them.
func (c *Context) RejectingPlugin() string { return c.rejectingPlugin }

// CurrentPhase reports the phase of the plugin dispatch currently in
// flight — InvocationPhaseRequest while Pipeline.Run is iterating
// OnRequest, InvocationPhaseResponse while Pipeline.RunResponse is
// iterating OnResponse, and "" outside a dispatch. Plugins read it to
// distinguish request from response without inferring the phase from
// body presence (an empty-bodied 204 response would otherwise look
// like a request). Set by the framework via setCurrent before each
// dispatch; see SetCurrentPlugin.
func (c *Context) CurrentPhase() InvocationPhase { return c.currentPhase }

// setRejectingPlugin records the name of the plugin that returned
// Reject. Framework-internal; callers in Pipeline.Run / RunResponse
// set this once per request, never overwrite (first rejection wins,
// but in practice no plugin runs after Reject so the check is
// defensive).
func (c *Context) setRejectingPlugin(name string) {
	if c.rejectingPlugin == "" {
		c.rejectingPlugin = name
		// Remember that this refusal arrived too late to take effect, so the outcome can say
		// what happened rather than what a plugin wanted. See MarkResponseDelivered.
		c.rejectedAfterDelivery = c.responseDelivered
	}
}

// MarkResponseDelivered records that the response has already gone downstream, so any refusal
// from here on cannot take effect.
//
// A LISTENER'S STATEMENT OF FACT, and only a listener can make it: the pipeline has no idea
// whether bytes reached a client. ext_proc calls it before its teardown flush — Envoy has
// finished with the stream by then — and the proxies' finalization paths are the same shape.
//
// WHAT IT CHANGES IS THE OUTCOME, NOT THE RECORD. Invocations appended afterwards are still
// recorded, and still say deny; they are marked Late so a reader can tell "a plugin refused
// this" from "this request was refused". What stops being true is the outcome:
// OutcomeFromContext no longer reports OutcomeDeny on the strength of a refusal that arrived
// after the response, because a request answered with a 200 was not denied — and every Finisher,
// every audit row and every dashboard that reads the outcome would otherwise say it was.
//
// Idempotent, and one-way: a response cannot become undelivered.
func (c *Context) MarkResponseDelivered() { c.responseDelivered = true }

// ResponseDelivered reports whether the response has already gone downstream.
func (c *Context) ResponseDelivered() bool { return c.responseDelivered }

// Record appends an Invocation to pctx under the current pipeline
// direction and framework-stamped plugin + phase. The author supplies
// only what's specific to this call (Action, Reason, plus any
// diagnostic fields like ExpectedIssuer, RouteHost, CacheHit); Plugin,
// Phase, and Path are populated automatically from pctx.
//
// Authors may set Plugin, Phase, or Path on the argument explicitly to
// override the framework defaults — useful for test helpers or for a
// plugin synthesizing an invocation on behalf of a delegated sub-plugin.
// In normal plugin code, leave those fields zero.
//
// For the bare (Action, Reason) case, prefer the convenience wrappers
// (Allow / Skip / Observe / Modify) below — one line each.
func (c *Context) Record(inv Invocation) {
	if c.inFinish {
		slog.Warn("pipeline: plugin called pctx.Record during OnFinish — dropped",
			"plugin", inv.Plugin,
			"action", inv.Action,
			"reason", inv.Reason)
		return
	}
	if inv.Plugin == "" {
		inv.Plugin = c.currentPlugin
	}
	if inv.Phase == "" {
		inv.Phase = c.currentPhase
	}
	if inv.Path == "" {
		inv.Path = c.Path
	}
	// Stamped by the framework, like Shadow, and for the same reason: plugin code is identical
	// before and after delivery, so the plugin cannot know. Recorded rather than dropped —
	// "a plugin refused this after it shipped" is a real fact about a rollout — and read by
	// OutcomeFromContext, which must not turn it into a denial.
	if c.responseDelivered {
		inv.Late = true
	}
	c.appendInvocation(inv)
}

// Allow records an Invocation with Action=allow and the given Reason.
// Convenience for gate plugins on the approved branch.
func (c *Context) Allow(reason string) {
	c.Record(Invocation{Action: ActionAllow, Reason: reason})
}

// Skip records an Invocation with Action=skip. Convenience for plugins
// that ran but didn't act on this message (path bypass, no route match,
// parser skipping a non-matching body).
func (c *Context) Skip(reason string) {
	c.Record(Invocation{Action: ActionSkip, Reason: reason})
}

// Observe records an Invocation with Action=observe. Convenience for
// parsers that successfully extracted diagnostic data without
// modifying the message.
func (c *Context) Observe(reason string) {
	c.Record(Invocation{Action: ActionObserve, Reason: reason})
}

// Modify records an Invocation with Action=modify. Convenience for
// plugins that mutated the message (token-exchange replacing the
// Authorization header, a header-rewriter).
func (c *Context) Modify(reason string) {
	c.Record(Invocation{Action: ActionModify, Reason: reason})
}

// DenyAndRecord records an Invocation with Action=deny AND returns a
// Reject Action. Bundles the two steps a gate plugin always does
// together on the deny path — emit the diagnostic record, then return
// the Action that changes control flow.
//
// code/message become the pipeline.Violation that the listener
// serializes to an HTTP response. Reason becomes the Invocation's
// machine-stable reason code.
//
// If the plugin has richer diagnostic data to attach to the Invocation
// (ExpectedIssuer, TokenScopes, etc.), use the two-step form: call
// pctx.Record(Invocation{...}) explicitly, then return pipeline.Deny.
func (c *Context) DenyAndRecord(reason, code, message string) Action {
	c.Record(Invocation{Action: ActionDeny, Reason: reason})
	return Deny(code, message)
}

// SetBody replaces the request body with newBody. A plugin that calls it
// must declare WritesRequestBody: true in its Capabilities — the listener
// consults pctx.BodyMutated() after Run to decide whether to emit the new
// bytes on the wire.
//
// NOTE — the capability is a contract, not an enforcement. SetBody sets
// bodyMutated unconditionally outside observe mode, and the listeners gate
// purely on pctx.BodyMutated(), so a plugin that calls SetBody WITHOUT
// declaring the capability still reaches the wire. This divergence is
// documented rather than closed: adding the enforcement silently would
// break any out-of-tree plugin relying on today's behaviour, so it needs
// its own compatibility review. Do not read it as licence to skip the
// declaration in order to keep response streaming — declaring
// WritesRequestBody costs no streaming (see PluginCapabilities).
//
// Under ErrorPolicyObserve (shadow mode) SetBody is a NO-OP on bytes:
// the in-memory body is not replaced, bodyMutated stays false, and
// downstream plugins continue to see the original. A modify
// Invocation is still recorded — with Shadow=true — so operators can
// count "would have redacted" on the rollout dashboard. Plugin code
// therefore looks identical under enforce and observe.
//
// SetBody auto-emits a modify-action Invocation with Reason
// "body_rewritten" and publishes a plugin-public event under
// "body-mutation/event" carrying the before/after length and sha256 —
// never the body content. The session store has no auth, so raw bodies
// would be a privacy / credential leak.
//
// Callers should NOT assign pctx.Body directly — the listener wouldn't
// know to propagate the change, and the Invocation wouldn't be emitted.
func (c *Context) SetBody(newBody []byte) {
	if c.inFinish {
		slog.Warn("pipeline: plugin called pctx.SetBody during OnFinish — dropped (response already sent)",
			"plugin", c.currentPlugin,
			"new_len", len(newBody))
		return
	}
	if c.currentPolicy == ErrorPolicyObserve {
		c.recordShadowBodyMutation("request", c.Body, newBody)
		return
	}
	old := c.Body
	c.Body = newBody
	c.bodyMutated = true
	c.emitBodyMutation("request", old, newBody)
}

// SetResponseBody is the response-side analogue of SetBody. Used by
// plugins that redact or rewrite the upstream response (prompt-safety
// guardrails on LLM output, content filters, DLP). Same contract —
// Invocation + body-mutation/event emitted; never logs the body —
// and the same observe-mode suppression: under ErrorPolicyObserve the
// response body is untouched and the Invocation is marked Shadow=true.
//
// A plugin that calls this must declare WritesResponseBody: true. That
// declaration is what makes listeners buffer the response instead of
// relaying SSE frames incrementally, so it must not be omitted.
func (c *Context) SetResponseBody(newBody []byte) {
	if c.inFinish {
		slog.Warn("pipeline: plugin called pctx.SetResponseBody during OnFinish — dropped (response already sent)",
			"plugin", c.currentPlugin,
			"new_len", len(newBody))
		return
	}
	if c.currentPolicy == ErrorPolicyObserve {
		c.recordShadowBodyMutation("response", c.ResponseBody, newBody)
		return
	}
	old := c.ResponseBody
	c.ResponseBody = newBody
	c.responseBodyMutated = true
	c.emitBodyMutation("response", old, newBody)
}

// recordShadowBodyMutation emits the would-mutate Invocation for a
// SetBody / SetResponseBody call that was suppressed under observe
// mode. Mirrors emitBodyMutation's telemetry (length + sha256 delta)
// so dashboards get the same shape they see under enforce, just with
// Shadow=true and no wire-level effect.
func (c *Context) recordShadowBodyMutation(phase string, oldBody, newBody []byte) {
	c.Record(Invocation{
		Action: ActionModify,
		Reason: "body_rewritten",
		Shadow: true,
	})
	if c.Extensions.Custom == nil {
		c.Extensions.Custom = map[string]any{}
	}
	c.Extensions.Custom["body-mutation"+PluginEventSuffix] = bodyMutationEvent{
		Phase:        phase,
		Plugin:       c.currentPlugin,
		LengthBefore: len(oldBody),
		LengthAfter:  len(newBody),
		SHA256Before: hashHex(oldBody),
		SHA256After:  hashHex(newBody),
	}
}

// BodyMutated reports whether a plugin called SetBody during this
// request. Listeners check this after Run to decide whether to emit a
// body mutation on the wire. Stream-scoped — a new Context starts with
// false regardless of what a previous request did.
func (c *Context) BodyMutated() bool { return c.bodyMutated }

// ResponseBodyMutated is the response-side analogue of BodyMutated.
func (c *Context) ResponseBodyMutated() bool { return c.responseBodyMutated }

// ContentSources returns every protocol extension on this Context that
// implements contracts.ContentSource. Guardrail plugins call this to
// iterate inspectable text across whatever protocol a request happens
// to carry, without importing any specific parser package:
//
//	for _, src := range pctx.ContentSources() {
//	    for _, f := range src.Fragments() {
//	        if f.Role == contracts.RoleUser { scan(f.Text) }
//	    }
//	}
//
// Order is A2A, MCP, Inference — but guardrails shouldn't rely on it;
// treat the result as an unordered set. Returns an empty slice when no
// parser produced an extension or when none of the populated extensions
// implement ContentSource.
func (c *Context) ContentSources() []contracts.ContentSource {
	out := make([]contracts.ContentSource, 0, 3)
	if c.Extensions.A2A != nil {
		out = append(out, c.Extensions.A2A)
	}
	if c.Extensions.MCP != nil {
		out = append(out, c.Extensions.MCP)
	}
	if c.Extensions.Inference != nil {
		out = append(out, c.Extensions.Inference)
	}
	return out
}

// Classification reports the request's protocol classification, aggregated
// across every populated protocol extension on Extensions:
//
//   - anyAction is true if at least one populated extension has IsAction=true
//     (e.g. mcp-parser saw "tools/call"; inference-parser saw any inference call).
//   - anyBypass is true if at least one populated extension has IsAction=false
//     (e.g. mcp-parser saw "tools/list" or a $transport/* synthetic event).
//
// Both false means no parser populated anything and the request is
// unclassified — guardrails treating IBAC-style defense in depth (only
// fire on traffic a parser claimed) should pass through.
//
// Parser-disjointness assumption. The current in-tree parsers fire on
// disjoint request shapes — mcp-parser on JSON-RPC bodies (or body-
// less MCP-shaped requests on configured paths), a2a-parser on A2A
// JSON-RPC bodies, inference-parser on /v1/{chat/,}completions paths
// — so a single request typically populates at most one extension.
// The aggregation above is defensive (handles the multi-extension
// case if a future hybrid transport ever does double-claim), but
// parser authors should not rely on the aggregation as a feature: a
// parser that populates an extension on a request another parser
// already classified breaks the contract that classification belongs
// to whichever parser owns the wire shape.
//
// Conflict resolution. If anyAction && anyBypass both end up true,
// callers decide their own precedence:
//
//   - Defense-in-depth gates (IBAC, rate limiters): treat anyBypass
//     as winning — skip first. Safer default when you can't tell who
//     to trust.
//   - Audit-style guardrails: probably want to log the action even
//     if some extension said bypass; flip the precedence.
//
// Either choice is valid; the contract here just provides both signals.
// In practice the question rarely comes up because of the disjointness
// above.
func (c *Context) Classification() (anyAction, anyBypass bool) {
	if ext := c.Extensions.MCP; ext != nil {
		if ext.IsAction {
			anyAction = true
		} else {
			anyBypass = true
		}
	}
	if ext := c.Extensions.A2A; ext != nil {
		if ext.IsAction {
			anyAction = true
		} else {
			anyBypass = true
		}
	}
	if ext := c.Extensions.Inference; ext != nil {
		if ext.IsAction {
			anyAction = true
		} else {
			anyBypass = true
		}
	}
	return anyAction, anyBypass
}

// emitBodyMutation records the Invocation and publishes the
// plugin-public event carrying length delta + sha256 before/after.
// Never logs raw body bytes — the session store is unauthenticated.
func (c *Context) emitBodyMutation(phase string, oldBody, newBody []byte) {
	c.Record(Invocation{Action: ActionModify, Reason: "body_rewritten"})

	if c.Extensions.Custom == nil {
		c.Extensions.Custom = map[string]any{}
	}
	// Prefix with a synthetic "body-mutation" plugin name — per the
	// convention in extensions.go, keys MUST be the plugin's Name(). We
	// use a fixed plugin-like prefix here because the framework (not a
	// specific plugin) owns this event: a switch of plugin names in a
	// future refactor shouldn't break operators' dashboards.
	c.Extensions.Custom["body-mutation"+PluginEventSuffix] = bodyMutationEvent{
		Phase:        phase,
		Plugin:       c.currentPlugin,
		LengthBefore: len(oldBody),
		LengthAfter:  len(newBody),
		SHA256Before: hashHex(oldBody),
		SHA256After:  hashHex(newBody),
	}
}

// bodyMutationEvent is the public payload shape under the
// body-mutation/event key. Purely observational — no raw body bytes.
// Consumers (abctl, audit systems) can render a per-mutation timeline
// with these fields alone.
type bodyMutationEvent struct {
	Phase        string `json:"phase"`  // "request" | "response"
	Plugin       string `json:"plugin"` // plugin that called SetBody
	LengthBefore int    `json:"length_before"`
	LengthAfter  int    `json:"length_after"`
	SHA256Before string `json:"sha256_before"`
	SHA256After  string `json:"sha256_after"`
}

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// appendInvocation routes an Invocation to the right direction bucket
// based on pctx.Direction. Private — plugins call Record or the
// Allow/Skip/Observe/Modify helpers above. Not exported so external
// plugin authors discover the ergonomic API first and only drop to the
// full Invocation struct when they need diagnostic fields.
func (c *Context) appendInvocation(inv Invocation) {
	if c.Extensions.Invocations == nil {
		c.Extensions.Invocations = &Invocations{}
	}
	switch c.Direction {
	case Inbound:
		c.Extensions.Invocations.Inbound = append(c.Extensions.Invocations.Inbound, inv)
	case Outbound:
		c.Extensions.Invocations.Outbound = append(c.Extensions.Invocations.Outbound, inv)
	}
}

// AgentIdentity carries the agent's own workload identity.
type AgentIdentity struct {
	ClientID    string
	WorkloadID  string
	TrustDomain string
}

// markLastInvocationShadow finds the most recent Invocation authored
// by pluginName in the given phase (within the current direction
// bucket) and flips its Shadow flag to true. Returns true when a
// matching record was found. Used by the pipeline to retroactively
// tag a plugin's deny record as shadow once Pipeline.Run has observed
// that the plugin returned Reject under ErrorPolicyObserve.
//
// Walking backwards and matching on Plugin+Phase is O(N) in the
// worst case but typically short-circuits at index -1 — plugins
// almost always Record right before returning Reject, so the last
// entry matches.
func (c *Context) markLastInvocationShadow(pluginName string, phase InvocationPhase) bool {
	if c.Extensions.Invocations == nil {
		return false
	}
	var list []Invocation
	switch c.Direction {
	case Inbound:
		list = c.Extensions.Invocations.Inbound
	case Outbound:
		list = c.Extensions.Invocations.Outbound
	default:
		return false
	}
	for i := len(list) - 1; i >= 0; i-- {
		if list[i].Plugin != pluginName || list[i].Phase != phase {
			continue
		}
		list[i].Shadow = true
		return true
	}
	return false
}
