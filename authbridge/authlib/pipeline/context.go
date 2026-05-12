package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/contracts"
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
	// field: the :scheme pseudo-header in ext_proc and ext_authz,
	// r.URL.Scheme in the forward and reverse proxies.
	//
	// Empty when the listener can't determine scheme (legacy test
	// fixtures, an unrecognized transport, etc.). Plugins that need a
	// concrete scheme should pick a default explicitly — treating ""
	// as "assume http" would silently mask missing listener plumbing.
	Scheme  string
	Host    string
	Path    string
	Headers http.Header
	Body    []byte // nil unless at least one plugin declares BodyAccess: true

	// StartedAt is the wall-clock time this context was constructed by the
	// listener at the start of a request. Used on the response path to
	// compute SessionEvent.Duration without walking the event history.
	StartedAt time.Time

	Agent    *AgentIdentity
	Identity Identity     // nil before an auth plugin runs
	Session  *SessionView // nil unless session tracking is enabled

	// Response-phase fields (populated by listener before RunResponse).
	// ResponseBody may be nil even during response phase if no plugin declared BodyAccess.
	StatusCode      int
	ResponseHeaders http.Header
	ResponseBody    []byte

	Extensions Extensions

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

// setRejectingPlugin records the name of the plugin that returned
// Reject. Framework-internal; callers in Pipeline.Run / RunResponse
// set this once per request, never overwrite (first rejection wins,
// but in practice no plugin runs after Reject so the check is
// defensive).
func (c *Context) setRejectingPlugin(name string) {
	if c.rejectingPlugin == "" {
		c.rejectingPlugin = name
	}
}

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

// SetBody replaces the request body with newBody. Only meaningful when
// the plugin declares WritesBody: true in its Capabilities — the
// listener consults pctx.BodyMutated() after Run to decide whether to
// emit the new bytes on the wire. Plugins without WritesBody that call
// SetBody mutate the in-memory Context (readers downstream see the
// change), but the wire is unchanged.
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
