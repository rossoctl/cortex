package pipeline

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/kagenti/kagenti-extensions/authbridge/authlib/routing"
	"github.com/kagenti/kagenti-extensions/authbridge/authlib/validation"
)


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
	Host      string
	Path      string
	Headers   http.Header
	Body      []byte // nil unless at least one plugin declares BodyAccess: true

	// StartedAt is the wall-clock time this context was constructed by the
	// listener at the start of a request. Used on the response path to
	// compute SessionEvent.Duration without walking the event history.
	StartedAt time.Time

	Agent   *AgentIdentity
	Claims  *validation.Claims    // nil before jwt-validation runs
	Route   *routing.ResolvedRoute
	Session *SessionView // nil unless session tracking is enabled

	// Response-phase fields (populated by listener before RunResponse).
	// ResponseBody may be nil even during response phase if no plugin declared BodyAccess.
	StatusCode      int
	ResponseHeaders http.Header
	ResponseBody    []byte

	Extensions Extensions

	// currentPlugin and currentPhase are framework-owned fields set by
	// Pipeline.Run / RunResponse around each plugin dispatch. They feed
	// the Record / Allow / Skip / Observe / Modify / DenyAndRecord helper
	// methods so plugin code doesn't have to repeat its own Name(),
	// the phase it's in, or the direction on every Invocation literal.
	// Unexported so plugins can only set them indirectly (via the framework).
	currentPlugin string
	currentPhase  InvocationPhase
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
func (c *Context) SetCurrentPlugin(name string, phase InvocationPhase) {
	c.currentPlugin = name
	c.currentPhase = phase
}

// ClearCurrentPlugin resets the framework-owned attribution fields.
// Paired with SetCurrentPlugin.
func (c *Context) ClearCurrentPlugin() {
	c.currentPlugin = ""
	c.currentPhase = ""
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
