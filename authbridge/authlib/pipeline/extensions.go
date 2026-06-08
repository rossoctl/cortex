package pipeline

import "time"

// Extensions holds typed extension slots for plugin-to-plugin communication.
// Each slot is populated by a specific plugin and consumed by downstream plugins.
//
// The named slots (MCP, A2A, Security, Delegation, Inference, Auth) are
// reserved for telemetry-worthy extensions — data that flows into
// SessionEvent, is serialized on the wire API, and has a published schema
// that unrelated plugins can rely on. Adding a new named slot is a
// core-library change.
//
// For data that shouldn't drive a core-library change, use Custom. Two
// access patterns share the same map:
//
//   - Plugin-PRIVATE state (cross-phase continuity inside one plugin).
//     Use the typed SetState / GetState generics. Key is plugin.Name().
//     Value is typically *T for a plugin-internal struct (may contain
//     sync primitives, unexported fields, channels). Never flows to
//     session events.
//
//   - Plugin-PUBLIC observability. Use a key suffixed with "/event"
//     (e.g., "rate-limiter/event"). Value must be JSON-marshalable.
//     The listener serializes matching entries into SessionEvent.Plugins
//     at record time — keyed by the plugin name (suffix stripped). A
//     new plugin can surface events to /v1/sessions without any
//     authlib modification. See authbridge/docs/plugin-reference.md for the
//     convention + promotion criteria for named-slot graduation.
//
// The suffix convention keeps the two intents unambiguous at write
// time: a plugin author has to deliberately type "/event" to opt into
// serialization, so private state can never leak by accident.
type Extensions struct {
	MCP         *MCPExtension
	A2A         *A2AExtension
	Security    *SecurityExtension
	Delegation  *DelegationExtension
	Inference   *InferenceExtension
	Invocations *Invocations
	Custom      map[string]any
}

// PluginEventSuffix is the key suffix that marks a Custom entry as
// plugin-public observability data destined for SessionEvent.Plugins.
// Plugin authors opt into serialization by writing:
//
//	pctx.Extensions.Custom["rate-limiter"+pipeline.PluginEventSuffix] = ...
//
// The listener strips the suffix when populating SessionEvent.Plugins,
// so consumers see the plugin name as the map key.
const PluginEventSuffix = "/event"

// SetState stashes a typed value on pctx under key. Intended for plugin-
// private per-request state — e.g., a rate-limiter remembering how many
// tokens were available when OnRequest saw the call, for OnResponse to
// consult. The generic type parameter is documentary: it forces callers
// to pass *T rather than an unrelated interface, which pairs with the
// symmetric type-assert in GetState.
//
// Convention: `key` should be the plugin's Name() so keys from unrelated
// plugins don't collide. SetState is not safe for concurrent use — pctx
// is single-threaded per request in the pipeline.
func SetState[T any](pctx *Context, key string, v *T) {
	if pctx.Extensions.Custom == nil {
		pctx.Extensions.Custom = map[string]any{}
	}
	pctx.Extensions.Custom[key] = v
}

// GetState retrieves a typed value previously stored via SetState. Returns
// nil when the key is absent or when the stored value is not a *T —
// safe-fails rather than panicking so a mid-pipeline type migration
// (plugin version skew) degrades instead of crashing the handler.
func GetState[T any](pctx *Context, key string) *T {
	if pctx.Extensions.Custom == nil {
		return nil
	}
	v, ok := pctx.Extensions.Custom[key].(*T)
	if !ok {
		return nil
	}
	return v
}

// MCPExtension carries parsed MCP JSON-RPC metadata.
// Result and Err are mutually exclusive: a response sets exactly one.
//
// IsAction is the parser's classification verdict (see
// pipeline.Context.Classification). The parser explicitly sets it
// true for user-meaningful action methods (tools/call, prompts/get,
// resources/read); the zero value false means "protocol mechanics
// or unclassified." Default-false reflects defense-in-depth: if the
// parser can't confidently classify a method as an action, guardrails
// err toward letting traffic through rather than judging it.
type MCPExtension struct {
	Method   string         `json:"method,omitempty"`
	RPCID    any            `json:"rpcId,omitempty"`
	Params   map[string]any `json:"params,omitempty"`
	Result   map[string]any `json:"result,omitempty"`
	Err      *MCPError      `json:"error,omitempty"`
	IsAction bool           `json:"isAction,omitempty"`
}

// MCPError mirrors a JSON-RPC 2.0 error object.
type MCPError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// A2AExtension carries parsed A2A protocol metadata from inbound requests
// and response summaries for debugging.
//
// IsAction is the parser's classification verdict; see MCPExtension.IsAction
// for the contract. Set true for user-meaningful methods (message/send,
// message/stream); zero value covers protocol/discovery methods.
type A2AExtension struct {
	// Request fields
	Method    string    `json:"method,omitempty"`
	RPCID     any       `json:"rpcId,omitempty"`
	SessionID string    `json:"sessionId,omitempty"`
	MessageID string    `json:"messageId,omitempty"`
	TaskID    string    `json:"taskId,omitempty"`
	Role      string    `json:"role,omitempty"`
	Parts     []A2APart `json:"parts,omitempty"`

	// Response fields (populated by a2a-parser OnResponse)
	FinalStatus  string `json:"finalStatus,omitempty"`  // "completed", "failed", "canceled"
	Artifact     string `json:"artifact,omitempty"`     // final artifact text
	ErrorMessage string `json:"errorMessage,omitempty"` // failure reason if status is "failed"

	// Classification — see MCPExtension.IsAction.
	IsAction bool `json:"isAction,omitempty"`
}

// A2APart represents a message part in an A2A request.
type A2APart struct {
	Kind    string `json:"kind"`
	Content string `json:"content,omitempty"`
}

// InferenceExtension carries parsed LLM inference request and response metadata.
// Request fields are populated by OnRequest; response fields by OnResponse.
//
// IsAction is the parser's classification verdict; see MCPExtension.IsAction
// for the contract. Inference calls are always actions when populated, so
// inference-parser sets this true unconditionally.
type InferenceExtension struct {
	Model       string             `json:"model,omitempty"`
	Messages    []InferenceMessage `json:"messages,omitempty"`
	Temperature *float64           `json:"temperature,omitempty"`
	MaxTokens   *int               `json:"maxTokens,omitempty"`
	TopP        *float64           `json:"topP,omitempty"`
	Stream      bool               `json:"stream,omitempty"`
	Tools       []InferenceTool    `json:"tools,omitempty"`
	ToolChoice  any                `json:"toolChoice,omitempty"` // "auto" | "none" | {type,function:{name}}

	// Response fields (populated after OnResponse runs).
	Completion       string              `json:"completion,omitempty"`
	FinishReason     string              `json:"finishReason,omitempty"`
	PromptTokens     int                 `json:"promptTokens,omitempty"`
	CompletionTokens int                 `json:"completionTokens,omitempty"`
	TotalTokens      int                 `json:"totalTokens,omitempty"`
	ToolCalls        []InferenceToolCall `json:"toolCalls,omitempty"`

	// Classification — see MCPExtension.IsAction.
	IsAction bool `json:"isAction,omitempty"`
}

// InferenceMessage represents a single message in the conversation.
type InferenceMessage struct {
	Role    string `json:"role"`
	Content string `json:"content,omitempty"`
}

// InferenceTool is a function/tool the client declared the model may call.
// Parameters is the OpenAI-style JSON Schema object describing valid args.
type InferenceTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// InferenceToolCall is a tool invocation the model emitted in its response.
// Arguments is the raw JSON string as returned by the LLM (often needs
// json.Unmarshal by the caller) — kept as a string so malformed output
// from the model doesn't prevent capture.
type InferenceToolCall struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name"`
	Arguments string `json:"arguments,omitempty"`
}

// SecurityExtension carries guardrail output.
// Caller identity is already in ctx.Agent and ctx.Claims — this slot is only
// for downstream signals from content-inspection plugins.
type SecurityExtension struct {
	Labels      []string `json:"labels,omitempty"`
	Blocked     bool     `json:"blocked,omitempty"`
	BlockReason string   `json:"blockReason,omitempty"`
}

// InvocationAction is the universal 5-value vocabulary every plugin uses
// to describe what it did on a single pipeline pass. Every plugin —
// gate, parser, rate-limiter, guardrail, whatever we add next —
// MUST emit exactly one of these per Invocation so abctl and /v1/sessions
// can render a consistent per-plugin timeline.
//
//	allow   — a gate plugin permitted the request. jwt-validation
//	          returns this on successful signature + issuer + audience.
//	deny    — a gate plugin rejected the request. Terminal for the
//	          pipeline pass. jwt-validation on bad token,
//	          token-exchange on upstream IdP failure.
//	skip    — the plugin ran but didn't act. jwt-validation on a
//	          bypass path, token-exchange on a host with no matching
//	          route, a parser whose body didn't match its format.
//	modify  — the plugin mutated the message. token-exchange replaced
//	          the Authorization header with a freshly-issued token.
//	observe — the plugin attached diagnostic data without altering
//	          flow. All parsers use this when they successfully parse.
//
// Reason (the stable machine code alongside Action) can discriminate
// within a value — e.g. skip/path_bypass vs skip/no_matching_route
// tell different stories at the detail-pane level, but both read
// "skip" in the at-a-glance timeline.
//
// Named InvocationAction rather than Action because pipeline.Action is
// already the pipeline-directive struct (Continue / Reject); keeping
// the names distinct avoids a shadowing foot-gun.
type InvocationAction string

const (
	ActionAllow   InvocationAction = "allow"
	ActionDeny    InvocationAction = "deny"
	ActionSkip    InvocationAction = "skip"
	ActionModify  InvocationAction = "modify"
	ActionObserve InvocationAction = "observe"
)

// Invocations carries one record per plugin that ran on a pipeline pass,
// split by direction so a single event's inbound and outbound plugin
// activity stays distinguishable. Multiple plugins can contribute — each
// appends an entry — so chained plugins cooperate without schema churn.
// Directions are disjoint per request: a single listener pass populates
// at most one of Inbound / Outbound.
//
// Replaces the earlier AuthExtension; parsers and any other plugin class
// share the list now. abctl renders one row per Invocation, so operators
// get a per-plugin timeline without guessing which plugins touched each
// event.
type Invocations struct {
	Inbound  []Invocation `json:"inbound,omitempty"`
	Outbound []Invocation `json:"outbound,omitempty"`
}

// FilteredByPhase returns a new *Invocations containing only entries
// whose Phase matches the argument. Strict match — untagged entries
// (Phase == "") are dropped because the framework always populates
// Phase via Context.Record; an untagged entry is a plugin bug and
// including it in the wrong phase would double-report in one event
// and be missing from the other.
//
// The underlying Invocation values are copied shallowly; mutating a
// returned entry's Details map mutates the source. Acceptable for
// the per-request flow where the original lives only on pctx and is
// discarded after recording.
//
// Returns nil when no entries match so callers can drop the field
// from the SessionEvent without a null check.
//
// Intended for reject-event recording in listeners and for the
// accept-path phase split (one SessionEvent per phase). Listeners
// that need full independence from pctx's lifecycle layer their own
// snapshot on top.
func (in *Invocations) FilteredByPhase(phase InvocationPhase) *Invocations {
	if in == nil {
		return nil
	}
	out := &Invocations{}
	for _, inv := range in.Inbound {
		if inv.Phase == phase {
			out.Inbound = append(out.Inbound, inv)
		}
	}
	for _, inv := range in.Outbound {
		if inv.Phase == phase {
			out.Outbound = append(out.Outbound, inv)
		}
	}
	if out.Inbound == nil && out.Outbound == nil {
		return nil
	}
	return out
}

// Invocation records one plugin's action on one pipeline pass. Plugin is
// the plugin's Name() for traceability. Action is the universal 5-value
// verb (see Action). Reason is a stable machine-readable label paired
// with the counters plugins already feed into /stats — use Reason for
// filtering / indexing rather than Action alone when you need to
// distinguish skip/path_bypass from skip/no_matching_route.
//
// Diagnostic fields are populated selectively per plugin. Auth gates
// populate ExpectedIssuer/Audience/Token*; outbound routers populate
// Route* and CacheHit; parsers typically populate only Plugin/Action/
// Reason because their semantic payload lives on the typed extension
// slots (A2A / MCP / Inference).
//
// NEVER contains the raw bearer token, token signature, or client
// credentials. The session API has no auth on it; only safe-to-log data
// belongs here.
// InvocationPhase identifies whether an Invocation was appended during
// the request pass or the response pass. Without this tag the full list
// on pctx — which is cumulative across both phases — can't be correctly
// partitioned by the listener when it records the request event and the
// response event separately. Keeping the full list on pctx is deliberate
// (plugins may need cross-phase context); the phase tag lets consumers
// filter by pass.
type InvocationPhase string

const (
	InvocationPhaseRequest  InvocationPhase = "request"
	InvocationPhaseResponse InvocationPhase = "response"
)

type Invocation struct {
	Plugin string           `json:"plugin"`
	Action InvocationAction `json:"action"`
	// Phase is the pass (request or response) that appended this
	// record. The listener uses it to filter invocations per event at
	// record time — the request event carries only request-phase
	// entries, the response event only response-phase entries, even
	// though pctx carries the union.
	Phase  InvocationPhase `json:"phase,omitempty"`
	Reason string          `json:"reason,omitempty"`

	// Path is the request path the invocation ran on. Populated so
	// operators can disambiguate invocations on the same plugin (e.g.
	// a jwt-validation skip on /healthz vs /.well-known/agent.json;
	// a mcp-parser observe on tools/call vs tools/list). Left empty
	// when the plugin has no path context.
	Path string `json:"path,omitempty"`

	// Details carries plugin-specific context as a flat string→string
	// map. Opaque to the framework; abctl renders it as key=value rows
	// in the invocation detail pane. Suggested convention: snake_case
	// keys scoped to the plugin's semantic domain. Built-in plugins
	// use keys like expected_issuer, token_subject, route_host,
	// target_audience, cache_hit. Third-party plugins define their own.
	//
	// Stringify booleans as "true"/"false" and []string as space-
	// joined (matching OAuth scope conventions). Keep values short
	// enough for a detail pane — bulky diagnostics belong in the
	// Extensions.Custom escape-hatch event.
	//
	// NEVER put raw tokens, signatures, or client credentials here.
	// The session API has no auth on it — only safe-to-log data
	// belongs in Invocation.Details.
	Details map[string]string `json:"details,omitempty"`

	// Shadow reports that the plugin ran under on_error: observe and
	// its decision (deny or modify) was NOT applied to the request.
	// An operator reading a would-have-blocked timeline filters on
	// Shadow=true to count rollout-candidate events; a dashboard that
	// aggregates "effective denies" filters Shadow=false. The
	// framework, not the plugin, sets this — plugin code looks
	// identical under enforce and observe.
	Shadow bool `json:"shadow,omitempty"`
}

// DelegationExtension tracks the token delegation chain across hops.
// The chain is append-only and unexported to prevent forgery or truncation.
type DelegationExtension struct {
	chain  []DelegationHop
	Origin string // original caller's subject ID
	Actor  string // current actor's subject ID
}

// Chain returns a copy of the delegation chain. The copy prevents callers from
// mutating the backing slice (truncation, reordering, forgery).
func (d *DelegationExtension) Chain() []DelegationHop {
	out := make([]DelegationHop, len(d.chain))
	copy(out, d.chain)
	return out
}

// Depth returns the number of hops in the delegation chain.
func (d *DelegationExtension) Depth() int {
	return len(d.chain)
}

// DelegationHop represents one hop in the delegation chain.
//
// Audience, Strategy, and FromCache enrich a hop produced by an
// AuthBridge auth plugin (today: token-exchange) so downstream policy
// can reason about WHAT a token was minted for and HOW. They map onto
// the corresponding CPEX DelegationHop fields. All three are optional —
// a producer that only knows the subject leaves them zero.
type DelegationHop struct {
	SubjectID string
	Scopes    []string
	Timestamp time.Time

	// Audience is the target audience the hop's token was minted for
	// (RFC 8693 `audience`). Empty when the hop isn't an exchange.
	Audience string

	// Strategy names how the hop was produced — e.g. "token-exchange"
	// for an RFC 8693 exchange. Empty when unclassified.
	Strategy string

	// FromCache reports the hop's token was served from the exchange
	// cache rather than freshly minted at the IdP.
	FromCache bool
}

// AppendHop adds a hop to the delegation chain. This is the only way to extend
// the chain — direct mutation is prevented by the unexported slice.
//
// AppendHop is not safe for concurrent use. The pipeline guarantees sequential
// invocation.
func (d *DelegationExtension) AppendHop(hop DelegationHop) {
	d.chain = append(d.chain, hop)
	if d.Origin == "" {
		d.Origin = hop.SubjectID
	}
	d.Actor = hop.SubjectID
}
