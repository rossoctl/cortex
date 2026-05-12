# pipeline — Framework Architecture Reference

Framework-level reference for AuthBridge's plugin pipeline: types, composition, lifecycle, shared state, and the boundary with listeners. Pair this with the plugin-author docs:

- **Tutorial** — [`plugin-tutorial.md`](./plugin-tutorial.md). Writing a plugin from scratch with runnable examples.
- **Plugin author reference** — [`plugin-reference.md`](./plugin-reference.md). Config patterns, invocation emission contract, registration rules.
- **Framework reference** — this file. Pipeline internals and Go surface.

**Audience:**
- Framework maintainers editing `authbridge/authlib/pipeline/`.
- Plugin authors who need to understand pipeline composition, lifecycle hooks, the shared state shape, or the observability contract in depth.
- Anyone debugging the plugin flow via `abctl` or the `:9094` session API.

**Scope:**
- The Go surface in `authbridge/authlib/pipeline/` and `authbridge/authlib/session/`.
- The observability contract carried by `SessionEvent` on the `:9094` API.
- What the pipeline *does* and *does not* own at the boundary with the listener.

For step-by-step "how do I write a plugin?" content, see [`plugin-tutorial.md`](./plugin-tutorial.md) first.

---

## 1. Mental model

AuthBridge intercepts HTTP traffic in two directions and runs a **separate plugin chain** for each. Each chain has two **phases** — request (headers/body going to the upstream) and response (headers/body coming back).

```
          Inbound (caller → this agent)
          ┌────────────────────────────────────────────────────┐
          │  Request phase  →  jwt-validation                  │
          │                 →  a2a-parser                      │
          │                 →  session-recorder   (implicit)   │
          │  Response phase ←  a2a-parser OnResponse           │
          │                 ←  jwt-validation OnResponse       │
          └────────────────────────────────────────────────────┘

          Outbound (this agent → target service)
          ┌────────────────────────────────────────────────────┐
          │  Request phase  →  route-resolver                  │
          │                 →  token-exchange                  │
          │                 →  mcp-parser / inference-parser   │
          │  Response phase ←  mcp-parser / inference-parser   │
          │                 ←  token-exchange OnResponse       │
          └────────────────────────────────────────────────────┘
```

**Key properties:**
- Plugins execute **sequentially** within a phase.
- Response phase runs plugins in **reverse order** (last plugin sees the response first — LIFO, matches middleware conventions).
- Inbound and outbound are **separate `Pipeline` instances**. A plugin that cares about both directions is registered on both.
- All state shared between plugins within one request/response cycle lives on `*pipeline.Context` (`pctx`).
- Cross-request state (per-session telemetry) lives in the `session.Store`, accessed read-only via `pctx.Session`.

---

## 2. The `Plugin` interface

```go
type Plugin interface {
    Name() string
    Capabilities() PluginCapabilities
    OnRequest(ctx context.Context, pctx *Context) Action
    OnResponse(ctx context.Context, pctx *Context) Action
}
```

### `Name() string`
A stable identifier. Used for logs, metrics, `GetState`/`SetState` keys (by convention), and pipeline introspection (`GET /v1/pipeline`).

### `Capabilities() PluginCapabilities`

```go
type PluginCapabilities struct {
    Reads      []string // extension slot names this plugin reads
    Writes     []string // extension slot names this plugin writes
    ReadsBody  bool     // plugin reads pctx.Body / pctx.ResponseBody
    WritesBody bool     // plugin mutates body via pctx.SetBody / pctx.SetResponseBody
    BodyAccess bool     // deprecated: alias for ReadsBody (folded by Normalize)
}
```

Declared once per plugin instance. `pipeline.New` validates that every `Read` is satisfied by an earlier plugin's `Write` — a plugin that depends on `mcp` being populated cannot be registered before `mcp-parser`. A mis-ordered registration fails fast at startup with:

```
plugin "guardrail" reads slot "mcp" but no earlier plugin writes it
```

`ReadsBody: true` (or the legacy `BodyAccess` alias) on *any* plugin in a chain causes `Pipeline.NeedsBody()` to return true, which the **listener** uses to negotiate Envoy's `ProcessingMode` (BUFFERED vs HEADERS-only). Without this, the gRPC ext_proc server never asks for the body and parsers see `pctx.Body == nil`.

`WritesBody: true` declares that the plugin may rewrite the body via `pctx.SetBody` / `pctx.SetResponseBody`; the listener propagates the mutation to the wire. See §6, "Body mutation" for the full body-mutation contract (capability rules, ordering constraints, Content-Encoding policy).

### `OnRequest(ctx, pctx) Action`
Called when a request is entering the pipeline. Plugins typically read request headers / body, mutate one or more extension slots, and return `Continue` or `Reject`.

### `OnResponse(ctx, pctx) Action`
Called after the upstream returns. `pctx.StatusCode`, `pctx.ResponseHeaders`, and `pctx.ResponseBody` are populated. Plugins typically enrich the telemetry extensions with response-side data (completion text, token usage, error code) or apply guardrails on the response content.

Plugins that only care about the request set `OnResponse` to a no-op (`return Action{Type: Continue}`); same for response-only plugins on `OnRequest`.

---

## 3. `pipeline.Context` — the shared state

The entire surface a plugin sees:

```go
type Context struct {
    Direction Direction        // Inbound | Outbound
    Method    string           // HTTP method
    Scheme    string           // "http" | "https" | ... ; empty when the listener can't determine it
    Host      string           // :authority / Host
    Path      string           // :path
    Headers   http.Header
    Body      []byte           // nil unless a plugin declared BodyAccess: true
    StartedAt time.Time        // listener wall-clock at request entry

    Agent    *AgentIdentity   // this workload's SPIFFE / Keycloak identity
    Identity Identity          // caller identity after an auth plugin runs (interface)
    Session  *SessionView      // read-only view of the session bucket

    // Response-phase fields (populated by listener before RunResponse)
    StatusCode      int
    ResponseHeaders http.Header
    ResponseBody    []byte

    Extensions Extensions
}
```

**Ownership rules:**
- Plugins **read** any field they declared in `Capabilities.Reads`.
- Plugins **write** fields they declared in `Capabilities.Writes`. By convention each extension slot has exactly one writer (the parser plugin).
- Plugins read `pctx.Body` / `pctx.ResponseBody` only if they declared `ReadsBody: true` (or the deprecated `BodyAccess: true`).
- Plugins mutate body content via `pctx.SetBody(newBytes)` / `pctx.SetResponseBody(newBytes)`, and only if they declared `WritesBody: true`. Direct assignment (`pctx.Body = ...`) compiles but bypasses listener propagation and misses the Invocation + body-mutation event emission — see §6, "Body mutation."
- `Identity` is populated by whichever auth plugin ran (jwt-validation ships a `claimsIdentity` adapter around `validation.Claims`; a SAML / mTLS / custom plugin publishes its own adapter). The framework reads it through the `Identity` interface (`Subject()` / `ClientID()` / `Scopes()`) so no plugin-specific type leaks into `pipeline/`.
- `Agent`, `Session` are populated by the listener before `Run`. Plugins treat them as read-only.
- `ResponseBody` appears between `Run` and `RunResponse` — plugins must not read it in `OnRequest`.

**Framework-owned attribution.** `pipeline.Run` / `RunResponse` stamp the currently-dispatching plugin's name and phase onto unexported fields of `pctx` around each plugin call. These drive the `pctx.Record` family of helpers so Invocation entries are auto-attributed without plugin-side ceremony. Plugins can't set them directly (unexported); exported `SetCurrentPlugin` / `ClearCurrentPlugin` exist for test harnesses that invoke plugins outside a `Pipeline.Run` dispatch loop.

**Recording Invocations.** Plugins emit per-call diagnostic records through Context helpers:

```go
pctx.Allow("authorized")                          // gate approved
pctx.Skip("path_bypass")                          // plugin ran but didn't act
pctx.Observe("matched_tools/call")                // parser extracted data
pctx.Modify("token_replaced")                     // plugin mutated the message
pctx.Record(pipeline.Invocation{                  // full form with diagnostic fields
    Action: pipeline.ActionDeny,
    Reason: "jwt_failed",
    Details: map[string]string{
        "expected_issuer":   issuer,
        "expected_audience": audience,
    },
})
return pctx.DenyAndRecord(reason, code, message)  // emit + reject in one call
```

Framework fills `Plugin`, `Phase`, `Path`; authors supply only what's specific to this call. See [`plugin-reference.md`](./plugin-reference.md#emitting-session-events) for the full 5-value action vocabulary and field reference.

**Lifetime:** one `*Context` per HTTP transaction. Not reused across requests. Single-threaded — the pipeline guarantees sequential invocation of plugins within a phase, so plugins don't need internal locking for pctx reads/writes.

---

## 4. `Extensions` — typed plugin-to-plugin communication

```go
type Extensions struct {
    MCP         *MCPExtension
    A2A         *A2AExtension
    Security    *SecurityExtension
    Delegation  *DelegationExtension
    Inference   *InferenceExtension
    Invocations *Invocations       // per-plugin action records for every plugin that ran
    Custom      map[string]any     // plugin-private state + escape-hatch public events
}
```

Three categories of cross-plugin / cross-phase state:

### Invocations — per-plugin action record (always recorded)

Every plugin that runs on a pipeline pass appends at least one `Invocation` to this slot via the `pctx.Record` family of helpers. The listener snapshots it onto `SessionEvent.Invocations` so `abctl` and `/v1/sessions` see a per-plugin timeline.

```go
type Invocation struct {
    Plugin  string           // plugin.Name(); framework-filled
    Action  InvocationAction // 5-value: allow | deny | skip | modify | observe
    Phase   InvocationPhase  // "request" | "response"; framework-filled
    Reason  string           // machine-stable code, e.g. "path_bypass"
    Path    string           // request path; framework-filled

    // Plugin-specific diagnostic context. Opaque to the framework;
    // rendered as key=value rows by abctl. Built-in plugins populate
    // expected_issuer, token_subject, route_host, target_audience,
    // cache_hit (snake_case). Third-party plugins define their own
    // key space. Stringify booleans as "true"/"false" and []string
    // as space-joined to match OAuth scope conventions.
    Details map[string]string
}

type Invocations struct {
    Inbound  []Invocation
    Outbound []Invocation
}
```

Every plugin is expected to call one of `pctx.Allow` / `Skip` / `Observe` / `Modify` / `Record` / `DenyAndRecord` per active phase — see [`plugin-reference.md`](./plugin-reference.md#emitting-session-events) for the full field reference and 5-value vocabulary.

### Named protocol slots (telemetry-worthy, optional per plugin)
MCP, A2A, Inference, plus Security and Delegation. These are:
- Part of the **published schema** carried on `SessionEvent` to `:9094` / `abctl`.
- Consumable by multiple downstream plugins.
- Added to the core struct only when the data has a public contract.

A parser populates its slot AND records an Invocation with `ActionObserve`. The slot carries the structured payload (method, token counts, etc.); the Invocation carries the attribution.

Adding a named slot is an authlib-core change: edit `Extensions`, add a wire field on `sessionEventWire`, update `snapshotXXX` helpers in the listener, and add filtering rules in `abctl`.

**Capability interfaces on the slot types.** Named-slot extensions may implement optional capability interfaces declared in [`authlib/contracts/`](../authlib/contracts/) so consumer plugins can interact with them without importing any specific parser package. The current capability is [`ContentSource`](../authlib/contracts/content.go) — implemented by `A2AExtension`, `MCPExtension`, and `InferenceExtension` — which lets guardrail plugins iterate inspectable text fragments via `pctx.ContentSources()`. Parser authors opt in by adding one method (`Fragments() []contracts.Fragment`); consumers see a uniform view across every protocol that implements the contract. See [`plugin-reference.md` "Exposing content to guardrails"](./plugin-reference.md#exposing-content-to-guardrails) for the pattern.

### `Custom map[string]any` — plugin-private state + escape-hatch public events
Two access patterns share the same map, disambiguated by key suffix.

**Plugin-private cross-phase state.** Use `GetState[T]` / `SetState[T]`:

```go
// Plugin's private state type:
type rlState struct {
    TokensAtStart int
    Decision      string
}

// In OnRequest:
pipeline.SetState(pctx, "rate-limiter", &rlState{TokensAtStart: 100})

// In OnResponse:
s := pipeline.GetState[rlState](pctx, "rate-limiter")
if s != nil { /* use s */ }
```

Convention: **key = plugin's Name()** so collisions across plugins don't happen. Storage is lazy (`Custom` is nil-initialized until first write).

`GetState[T]` type-asserts and returns `nil` on mismatch instead of panicking — a plugin whose type evolves across versions degrades gracefully.

**Plugin-public escape-hatch events.** Write a key ending in `pipeline.PluginEventSuffix` (`"/event"`) with a JSON-marshalable value; the listener promotes it to `SessionEvent.Plugins[pluginName]` on the wire:

```go
pctx.Extensions.Custom["rate-limiter" + pipeline.PluginEventSuffix] = rateLimiterEvent{
    Allowed:    true,
    TokensLeft: 42,
}
```

The suffix is the opt-in marker — private state stays out of the session stream. Graduate to a named slot when two or more plugins share the shape. See [`plugin-reference.md`](./plugin-reference.md#emitting-session-events) for the graduation criteria.

### Built-in extension shapes

All at `authbridge/authlib/pipeline/extensions.go`:

```go
type MCPExtension struct {
    Method string          // JSON-RPC method, e.g. "tools/call"
    RPCID  any             // JSON-RPC id (could be int or string)
    Params map[string]any  // request params
    Result map[string]any  // response result (mutually exclusive with Err)
    Err    *MCPError
}

type A2AExtension struct {
    Method      string
    RPCID       any
    SessionID   string  // contextId from the client, or server-assigned on first turn
    MessageID   string
    TaskID      string
    Role        string  // "user" | "agent"
    Parts       []A2APart
    FinalStatus string  // response: "completed" | "failed" | "canceled"
    Artifact    string  // response: assembled artifact text
    ErrorMessage string // response: failure reason
}

type InferenceExtension struct {
    // Request side:
    Model       string
    Messages    []InferenceMessage
    Temperature *float64
    MaxTokens   *int
    TopP        *float64
    Stream      bool
    Tools       []InferenceTool  // full definition incl. parameters schema
    ToolChoice  any
    // Response side:
    Completion       string
    FinishReason     string
    PromptTokens     int
    CompletionTokens int
    TotalTokens      int
    ToolCalls        []InferenceToolCall
}

type SecurityExtension struct {
    Labels      []string // classifier / guardrail output
    Blocked     bool
    BlockReason string
}

type DelegationExtension struct {
    Origin string   // original caller subject
    Actor  string   // current actor subject
    // chain is append-only via AppendHop; reads via Chain()
}
```

Mutability: **always assigned, never mutated in place** after the parser sets the slot. This guarantees that `snapshotXXX` in the listener (shallow-copy for event recording) stays correct even when OnResponse enriches the struct — the response snapshot is taken from the now-enriched pointer, but any earlier request-phase snapshot was taken of a frozen copy.

---

## 5. `Action` — control flow

```go
type Action struct {
    Type      ActionType // Continue | Reject
    Violation *Violation // populated iff Type == Reject
}

type Violation struct {
    // Structured machine-readable error:
    Code        string         // machine-readable, e.g. "auth.missing-token"
    Reason      string         // short human message
    Description string         // longer explanation; optional
    Details     map[string]any // plugin-arbitrary structured context; optional

    // HTTP rendering hints — all optional; defaults from Code:
    Status   int         // when 0, StatusFromCode(Code) is used
    Body     []byte      // when nil, synthesized JSON
    BodyType string      // Content-Type for Body; defaults to application/json
    Headers  http.Header // merged into the response (e.g. WWW-Authenticate, Retry-After)

    // Framework-populated from Plugin.Name(); plugins leave it empty:
    PluginName string
}
```

Returning `Reject` from `OnRequest` halts the request pipeline; from `OnResponse` halts the response pipeline. The listener calls `Violation.Render()` to produce `(status, headers, body)` and emits that as the HTTP response. The default body when `Body` is nil:

```json
{
  "error":       "auth.missing-token",
  "message":     "Bearer token required",
  "description": "No Authorization header present",
  "plugin":      "jwt-validation",
  "details":     { "realm": "kagenti" }
}
```

Helper constructors cover the common cases so the reject site stays one line:

```go
pipeline.Deny("auth.invalid-token", "token expired")
pipeline.DenyStatus(451, "policy.forbidden", "unavailable for legal reasons")
pipeline.DenyWithDetails("policy.rate-limited", "quota hit", map[string]any{
    "remaining": 0, "window": "1h",
})
pipeline.Challenge("kagenti", "Authorization required")   // 401 + WWW-Authenticate
pipeline.RateLimited(30*time.Second, "", "slow down")     // 429 + Retry-After
```

The `Code` → HTTP-status mapping for well-known codes lives at `codeToStatus` in `action.go`; unknown codes default to 500. Plugins that need a non-default status set `Violation.Status` explicitly or use `DenyStatus`.

There is no "soft error" channel today — a plugin that wants to fail open logs and returns `Continue`. A future iteration may add a per-plugin `on_error` policy.

---

## 6. `Pipeline` — composition and execution

```go
func New(plugins []Plugin, opts ...Option) (*Pipeline, error)
func (p *Pipeline) Run(ctx context.Context, pctx *Context) Action                  // request phase
func (p *Pipeline) RunResponse(ctx context.Context, pctx *Context) Action          // response phase (reverse)
func (p *Pipeline) RunFinish(ctx context.Context, pctx *Context, outcome Outcome)  // finish phase (reverse)
func (p *Pipeline) Start(ctx context.Context) error                                // invoke Init on Initializer plugins
func (p *Pipeline) Stop(ctx context.Context)                                       // invoke Shutdown on Shutdowner plugins
func (p *Pipeline) Plugins() []Plugin                                              // defensive copy
func (p *Pipeline) NeedsBody() bool                                                // OR over all plugins' BodyAccess
```

`New` validates capability wiring at startup: every `Read` must be satisfied by some earlier plugin's `Write`. `plugins.Build` additionally validates the cross-plugin relationship declarations — `Requires`, `RequiresAny`, `After`, `Claims` — before returning the pipeline to the listener. See [`plugin-reference.md` "Declaring plugin relationships"](./plugin-reference.md#declaring-plugin-relationships).

### Plugin lifecycle (`Start` / `Stop`)

Plugins that need one-time setup (load a model, warm a cache, register metrics, spawn a background goroutine) implement the optional `Initializer` interface:

```go
type Initializer interface {
    Init(ctx context.Context) error
}
```

Plugins that need graceful cleanup (flush audit events, close a connection, cancel a goroutine) implement `Shutdowner`:

```go
type Shutdowner interface {
    Shutdown(ctx context.Context) error
}
```

Both are **optional** via Go's type-assertion idiom — a plugin that doesn't need them simply doesn't implement them, and the pipeline skips it. Existing plugins (jwt-validation, a2a-parser, mcp-parser, inference-parser, token-exchange) don't implement these; they keep working unchanged.

The host (e.g. `cmd/authbridge/main.go`) drives the lifecycle:

```go
// After pipeline.New, before listeners accept traffic:
initCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
defer cancel()
if err := inboundPipeline.Start(initCtx); err != nil {
    log.Fatalf("inbound pipeline Start: %v", err) // fail-fast on bad plugin init
}
if err := outboundPipeline.Start(initCtx); err != nil {
    log.Fatalf("outbound pipeline Start: %v", err)
}

// ... serve traffic ...

// After listeners have drained on SIGTERM:
outboundPipeline.Stop(shutdownCtx) // reverse order within each pipeline
inboundPipeline.Stop(shutdownCtx)
```

Semantics:
- `Start` — Init runs **in declaration order**, fails fast on the first error. The returned error names the offending plugin. No Shutdown is invoked on plugins whose Init already ran successfully — the intent is hard-fail on startup, not unwind.
- `Stop` — Shutdown runs **in reverse declaration order (LIFO)** so a plugin that depends on an earlier plugin's resources can still use them while cleaning up. Best-effort: errors from one Shutdown are logged but do not stop the sequence. Bounded by the caller's ctx deadline.

A minimal Init/Shutdown plugin example — a rate-limiter that refreshes its quota store in the background:

```go
type RateLimiter struct {
    store  *quotaStore
    cancel context.CancelFunc
}

func (p *RateLimiter) Name() string { return "rate-limiter" }
func (p *RateLimiter) Capabilities() pipeline.PluginCapabilities { return pipeline.PluginCapabilities{} }

func (p *RateLimiter) Init(ctx context.Context) error {
    p.store = newQuotaStore()
    bg, cancel := context.WithCancel(context.Background())
    p.cancel = cancel
    go p.store.refreshLoop(bg, 10*time.Second) // lives until Shutdown
    return nil
}

func (p *RateLimiter) Shutdown(ctx context.Context) error {
    p.cancel()             // stop the refresh loop
    return p.store.flush(ctx) // best-effort write-back of pending counters
}

func (p *RateLimiter) OnRequest(ctx context.Context, pctx *pipeline.Context) pipeline.Action {
    if !p.store.allow(pctx) {
        return pipeline.RateLimited(30*time.Second, "", "quota exceeded")
    }
    return pipeline.Action{Type: pipeline.Continue}
}

func (p *RateLimiter) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
    return pipeline.Action{Type: pipeline.Continue}
}
```

### Per-request finish hook (`Finisher`)

Plugins that reserve per-request state in `OnRequest` need a guaranteed release point — whether the request was allowed, denied by a later plugin, or errored at the upstream. Without such a hook, a rate-limiter that reserves a slot in `OnRequest` and releases it in `OnResponse` silently leaks the slot whenever a later plugin denies (because `OnResponse` is only walked when the pipeline returned `Continue`). The `Finisher` optional interface closes this:

```go
type Finisher interface {
    OnFinish(ctx context.Context, pctx *Context)
}
```

`OnFinish` fires **once per request**, after `OnResponse` if it ran, on every plugin whose `OnRequest` was actually invoked — including the plugin that denied (if any). Dispatch is LIFO, symmetric with `Shutdowner` and `RunResponse`, so a plugin's cleanup can still depend on earlier plugins' resources. The listener calls `Pipeline.RunFinish` in a `defer` wrapping the response-produce block:

```go
pctx := newContext(...)
defer func() {
    pipeline.RunFinish(ctx, pctx, pipeline.Outcome{
        FinalAction:   finalAction, // OutcomeAllow | OutcomeDeny | OutcomeError
        StatusCode:    statusCode,
        DenyingPlugin: denyingPlugin, // "" unless FinalAction == OutcomeDeny
    })
}()
// ... Run, write response, RunResponse ...
```

Key contract points, chosen to make the plugin-author surface small and hard to misuse:

- **Fresh ctx with a framework-set deadline.** `OnFinish` does not receive the request `ctx` — that one may be cancelled (client disconnect) by the time the response is on the wire. The framework derives a fresh ctx from `context.Background()` with `DefaultFinishTimeout` (2s) applied, overridable via `WithFinishTimeout`. Plugins doing network I/O (flushing audits, releasing a distributed lease) see a live ctx by default.
- **`pctx.Outcome()` returns non-nil only during OnFinish.** The `*Outcome` carries `FinalAction` (three values: `OutcomeAllow` / `OutcomeDeny` / `OutcomeError`), `StatusCode`, `DenyingPlugin`, and `Duration`. In `OnRequest` / `OnResponse` the getter returns nil, so the "only meaningful in OnFinish" contract is enforced at read time rather than via documented zero-value semantics.
- **Full pctx is available**, including response body — the framework buffers through the finish window so audit / metrics plugins can read the final payload.
- **Best-effort dispatch.** A panic is recovered and logged; later plugins in the LIFO chain still run. One misbehaving plugin does not leak state in every other plugin.
- **OnFinish is silent.** The framework does NOT auto-emit `Invocation` records for this phase. `pctx.Record` called from within `OnFinish` is dropped with a WARN log (SessionEvent is frozen once the response is published). Plugins that want observability on cleanup publish through their own sink (Prometheus, external audit service) or the `pctx.Extensions.Custom` escape-hatch map.
- **Body mutation is forbidden.** `pctx.SetBody` / `SetResponseBody` called from `OnFinish` are dropped with a WARN log — the response is already on the wire.
- **`on_error: off` plugins are never dispatched in any phase**, so their `OnFinish` never runs either (consistent with the "only plugins whose OnRequest actually ran" participation rule).

Canonical rate-limiter:

```go
type rlState struct{ tenant string }

func (p *RateLimiter) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
    tenant := pctx.Identity.ClientID()
    p.slots.Reserve(tenant)
    pipeline.SetState(pctx, "rate-limiter", &rlState{tenant: tenant})
    return pipeline.Action{Type: pipeline.Continue}
}

func (p *RateLimiter) OnFinish(ctx context.Context, pctx *pipeline.Context) {
    s, ok := pipeline.GetState[*rlState](pctx, "rate-limiter")
    if !ok {
        return
    }
    p.slots.Release(s.tenant)
    p.metrics.RecordOutcome(pctx.Outcome().FinalAction) // allow / deny / error buckets
}
```

Contrast with the pre-Finisher workaround: reserve in `OnRequest`, release in `OnResponse` — leaks on every denied request. Or: reserve in `OnRequest`, spawn a goroutine with a TTL-based release — leaks goroutines on process shutdown and runs on a timer, not on the actual response.

### Extension slots known to the validator

Built-in: `mcp`, `a2a`, `security`, `delegation`, `inference`, `custom`.

**For plugins that write new slot names:** use the `WithSlots` option:

```go
pipeline, err := pipeline.New(plugins,
    pipeline.WithSlots("provenance", "risk-score"))
```

This tells the validator those slot names are legal, so a downstream plugin can `Capabilities.Reads = []string{"provenance"}` without being rejected as "unknown slot".

### Execution order
- Request phase: `plugins[0].OnRequest → plugins[1].OnRequest → …`
- Response phase: `plugins[N-1].OnResponse → plugins[N-2].OnResponse → …`
- A `Reject` from any plugin halts its phase immediately.
- `ctx.Err() != nil` between plugins also halts with `Reject{Status: 499}`.

### Concurrency model
Always sequential. No priority / mode / fire-and-forget semantics yet. This is the 80% case for auth-and-parse pipelines; richer modes would require an executor layer above the current loop.

### Body mutation

A plugin that declares `WritesBody: true` may rewrite the request or response body. The framework owns the propagation to the wire; plugins only call `pctx.SetBody(newBytes)` / `pctx.SetResponseBody(newBytes)`.

**Capability model.** Three booleans on `PluginCapabilities`:

| Field | Meaning | Listener effect |
|---|---|---|
| `ReadsBody` | plugin reads `pctx.Body` / `pctx.ResponseBody` | buffers the body; plugin sees the bytes |
| `WritesBody` | plugin may call `pctx.SetBody` / `pctx.SetResponseBody` | implies `ReadsBody`; propagates mutations |
| `BodyAccess` (deprecated) | legacy alias for `ReadsBody` | folded by `Normalize()`, removed in a future release |

`pipeline.New` enforces two rules at build time:

1. **At most one `WritesBody` plugin per pipeline.** Multiple mutators would have ambiguous ordering semantics; the error names both plugins so an operator debugging pod logs knows which two to reconcile.
2. **`WritesBody` cannot precede a `ReadsBody`-only plugin.** A reader expects to see the original bytes; putting a mutator before it would silently feed the reader the post-rewrite content.

**Mutation helpers.** `SetBody` / `SetResponseBody` replace the byte slice and flip an internal `bodyMutated` / `responseBodyMutated` flag that listeners read via `pctx.BodyMutated()` / `pctx.ResponseBodyMutated()`. They also auto-emit:

- A `modify`-action Invocation with `Reason: "body_rewritten"`, framework-attributed to the mutating plugin.
- A plugin-public event under `pctx.Extensions.Custom["body-mutation" + PluginEventSuffix]` with the phase (`request` / `response`), plugin name, byte length before/after, and sha256 before/after. Never the raw body content — the session store is unauthenticated.

The flags (not byte-compare) are the source of truth. A rewrite that produces byte-identical output still records the Invocation because "redactor ran, nothing matched" is valid telemetry.

**Wire propagation (per listener).**

| Listener | Request body | Response body |
|---|---|---|
| `extproc` | `withBodyMutation` helper wraps the `RequestBody ProcessingResponse` with an ext_proc `BodyMutation`. Envoy replaces the buffered body and recomputes `Content-Length`. | Same pattern in the response-body handler. |
| `forwardproxy` | On mutation, rebuild `r.Body` from `pctx.Body`, set `r.ContentLength` + `Content-Length` header. | Replace `resp.Body` + `resp.ContentLength` + `Content-Length`. |
| `reverseproxy` | Same as forwardproxy on the inbound request before handing to `httputil.ReverseProxy`. | Same as forwardproxy on the response from the upstream. |

Every mutation path also **clears `Content-Encoding`**: the framework can't know whether the plugin decompressed a gzipped body before rewriting, so shipping plain bytes without the old encoding header beats shipping a malformed archive. Auto-decompress/recompress is a possible future feature, explicitly out of scope today.

**Body-size limits.** Bodies over `maxBodySize` (1 MB) are rejected by the listener at buffer time — before the mutator sees anything. Plugins don't need to guard against oversized input.

**Streaming.** Only buffered mutation is supported. Responses that stream (SSE, chunked bodies beyond the buffer cap) are not mutable today; a streaming-transform API is a separate project.

---

## 7. `Session` + `SessionEvent` — the observability side-channel

The pipeline itself is **in-band** (plugins alter request handling). Alongside it runs an **out-of-band** observability layer: the listener snapshots `pctx` into a `SessionEvent` after each phase and appends it to a per-session bucket in the `session.Store`. This store is what powers the `:9094` HTTP API and `abctl`.

```go
type SessionEvent struct {
    SessionID      string                     // bucket the event landed in
    At             time.Time
    Direction      Direction                  // inbound | outbound
    Phase          SessionPhase               // request | response | denied
    A2A         *A2AExtension              // snapshot of pctx.Extensions.A2A
    MCP         *MCPExtension
    Inference   *InferenceExtension
    Invocations *Invocations               // per-plugin action records, filtered by phase
    Plugins     map[string]json.RawMessage // plugin-public events (escape-hatch /event suffix)
    Identity    *EventIdentity             // Subject, ClientID, AgentID, Scopes
    StatusCode  int                        // response phase only
    Error       *EventError                // populated on 4xx/5xx
    Host        string                     // :authority
    Duration    time.Duration              // response: wall-clock since request entry
}
```

**Three phase values:**
- `request` — snapshot taken after the request pipeline completes, carrying request-phase invocations.
- `response` — snapshot taken after the response pipeline completes, carrying response-phase invocations. Status, duration, and response parser output live here.
- `denied` — terminal denial by a pipeline plugin (jwt-validation reject, token-exchange failure, guardrail block). Carries the request-phase invocations plus the Violation's structured `Error`.

**Plugins do not touch `SessionEvent` directly.** The listener records events. Plugins append Invocations via `pctx.Record` / `Allow` / `Skip` / `Observe` / `Modify` / `DenyAndRecord`, populate extension slots (A2A / MCP / Inference) via assignment, and read `pctx.Session` (a `*SessionView`) when they want to correlate the current request with prior ones in the same conversation — e.g. a rate-limiter that counts a session's inference events.

Wire format (`SessionEvent.MarshalJSON`) translates enums to strings and `Duration` to `DurationMs`. Round-trip stable — `json.Marshal(e) → json.Unmarshal → json.Marshal` is byte-identical. Tested at `pipeline/session_test.go:TestSessionEvent_JSONRoundTrip`.

---

## 8. Boundary: pipeline vs listener

The pipeline **does not own**:

| Concern | Owner | Why |
|---|---|---|
| HTTP wire protocol (ext_proc gRPC, ext_authz, reverse/forward proxy) | `cmd/authbridge/listener/` | Each mode speaks a different wire; pipeline stays protocol-free |
| Body buffering negotiation (`ProcessingMode: BUFFERED`) | Listener reads `Pipeline.NeedsBody()` | Only listener can respond to the ext_proc handshake |
| JWT issuance, client registration, Keycloak admin calls | Outside the pipeline (agent sidecars / kagenti-operator) | Async concerns happening before/after any request flow |
| Session store writes (`Store.Append`) | Listener, called after each phase | Plugins see only the read-only `SessionView` |
| SSE streaming of events to abctl | `authlib/sessionapi` | Observability API, not a plugin concern |

The pipeline **does own**:
- The `Plugin` interface contract.
- `pipeline.Context` structure and invariants.
- Validation of capability wiring at construction.
- Sequential dispatch and reject-short-circuit semantics.
- Typed extension slots and `GetState`/`SetState` helpers.
- The session-event *shape* (the listener uses it but doesn't define it).

---

## 9. Config hot-reload

Editing `authbridge-config-<agent>` no longer requires a pod restart. `authlib/reloader` watches the mounted config file and atomically swaps the inbound / outbound pipelines when content changes; listeners drain onto the new pipeline, old in-flight requests finish on the previous one.

**The moving parts**

| Component | Responsibility |
|---|---|
| `pipeline.Holder` | `atomic.Pointer[*Pipeline]` slot that listeners read every request. Delegating methods (`Run`, `RunResponse`, `NeedsBody`, `Ready`, `NotReadyPlugin`, `Plugins`) so call sites don't change. |
| `authlib/reloader.Reloader` | Owns the fsnotify watcher, debouncer, content-hash dedup, validation, and drain scheduling. |
| `main.go` | Provides a `PipelineBuilder` closure that mirrors the startup `Load → ApplyPreset → Validate → plugins.Build` sequence, so startup and reload run identical code. |

**Operator workflow**

```sh
# 1. Edit the mounted ConfigMap
kubectl edit configmap authbridge-config-<agent> -n <ns>
#    (or: kubectl apply -f …)

# 2. Wait ~60s for kubelet to sync the new content into the mount.
#    For instant reload during testing, restart the pod instead.

# 3. Confirm the swap via the stats port
kubectl port-forward -n <ns> deploy/<agent> 9093:9093 &
curl http://localhost:9093/reload/status        # last_success, counters, sha256
curl http://localhost:9093/config               # now-active config
```

**Reload lifecycle (what happens when the file changes)**

1. fsnotify event on the parent directory — we watch `/etc/authbridge/`, not `/etc/authbridge/config.yaml` directly, because ConfigMap mounts use symlink swap (`..data → ..<timestamp>`). A direct file watch misses the retarget.
2. Debounce 250 ms so a symlink-swap's REMOVE+CREATE+CHMOD burst fires one reload, not three.
3. SHA-256 dedup — identical bytes are ignored (mtime-only touches don't trigger rebuilds).
4. `PipelineBuilder` runs: `config.Load` → mode override → `ApplyPreset` → `Validate` → `plugins.Build`. Any failure records the error in `Status.LastError` and leaves the active pipeline untouched.
5. Compare the new config to the active one on unreloadable fields (`Mode`, `Listener.*`); refuse if they differ (see below).
6. `Start` the new pipelines with a 60 s budget. On Start failure, `Stop` any partially-started pipelines so their goroutines don't leak.
7. `inboundH.Store(newIn)` + `outboundH.Store(newOut)` — new requests now route to the new pipelines.
8. Background goroutine: `time.Sleep(drainWindow)` (default 30 s) → `oldPipeline.Stop(ctx)` with a 15 s budget. In-flight requests that already Loaded the old pipeline finish against it.
9. `Status.LastSuccess`, `ReloadsOK`, `ActiveConfigSHA256` update atomically.

**What's reloadable, what isn't**

| Change | Reloadable? | Why |
|---|---|---|
| Plugin list (add / remove / reorder plugins) | ✅ | Pipeline is rebuilt from scratch |
| A plugin's `config:` subtree (issuer, bypass paths, routes, JWKS URL, etc.) | ✅ | Plugin's `Configure` runs again with new bytes |
| `session.*` (TTL, MaxEvents, MaxSessions) | ⚠️ Reloaded into the `*Config`, but the live session store is built at startup — changes don't take effect until pod restart |
| `mode` (`envoy-sidecar` / `waypoint` / `proxy-sidecar`) | ❌ | Different wire protocol + listener set; refuse reload |
| `listener.*` (ports) | ❌ | Bound sockets; refuse reload |

For the unreloadable cases, `Status.LastError` names the field(s) that changed and `ReloadsFailed` bumps — the operator can see from `/reload/status` that a pod restart is required.

**Validation guarantee: bad YAML never takes the pod down.** Any failure during Load / Validate / Build / Start results in the status being updated and the active pipeline continuing to serve traffic on the previous config. Only a successful end-to-end reload swaps the holders.

**Non-reloadable choices elsewhere.** The in-memory session store, the stat server, the session API server, and the reloader itself are all process-scoped — they live from startup to shutdown. A change to `session.enabled`, `listener.session_api_addr`, or the reloader's own knobs (drain window, debounce) requires a pod restart.

---

## 10. Writing a plugin

For a step-by-step tutorial that walks through building a new plugin from scratch — minimal plugin, recording invocations, rejection, config, body access, out-of-tree packaging, testing — see [`plugin-tutorial.md`](./plugin-tutorial.md).

For the plugin-author reference (config conventions, invocation field list, registration rules, 5-value action vocabulary), see [`plugin-reference.md`](./plugin-reference.md).

This document stays focused on the pipeline framework internals — how plugins compose, how the shared state is shaped, how control flows. The two plugins-side docs build on top of it.

---

## 11. Open questions

- **Priority / on-error policies.** Plugins don't declare these today. If fail-open / fail-closed behavior becomes important to express per plugin, it would be added to `PluginCapabilities` (or a sibling metadata struct) and interpreted by `Pipeline`.
- **Body mutation semantics.** Today plugins generally don't rewrite `pctx.Body` or `pctx.ResponseBody`. If a plugin needs to modify the payload, we'd need a clear contract about whether downstream plugins see the modified or original bytes.
- **Execution modes.** The pipeline is sequential-only. Concurrent or fire-and-forget modes would require an executor layer; no concrete use case yet.

---

## 12. Versioning

The plugin interface is **not** semver-stable yet (AuthBridge is pre-1.0). Changes since the initial release:
- Added `BodyAccess` to `PluginCapabilities`.
- Added `WithSlots` to `New` for bridge-plugin slot registration.
- Added `GetState[T]` / `SetState[T]` generic helpers.
- Extended `A2AExtension` with response-side fields (TaskID, FinalStatus, Artifact, ErrorMessage).
- Extended `InferenceExtension` with structured tools + tool calls + TopP / ToolChoice.
- Added `SessionEvent.MarshalJSON`/`UnmarshalJSON` round-trip contract.
- **Breaking**: replaced `Action.Status`/`Action.Reason` with `Action.Violation` (see §5). Migration: use `Deny()`, `DenyStatus()`, `Challenge()`, `RateLimited()` helpers.
- Added optional `Initializer` / `Shutdowner` / `Readier` interfaces + `Pipeline.Start` / `Pipeline.Stop` (see §6). Existing plugins are unaffected because the interfaces are opt-in via type-assertion.
- Added `SessionDenied` phase and `recordInboundReject` in the listener so denials surface as session events with full diagnostic context.
- **Unified invocation contract**: `AuthExtension` + `InboundAuth` + `OutboundAuth` collapsed into `Invocations` + `Invocation`. Every plugin (gate, parser, future) emits an Invocation record per pipeline pass using the 5-value `InvocationAction` vocabulary (allow / deny / skip / modify / observe). `SessionEvent.Auth` is now `SessionEvent.Invocations`.
- **`pctx.Record` helpers**: `Allow` / `Skip` / `Observe` / `Modify` / `Record` / `DenyAndRecord` on `Context`. Framework-managed attribution (`currentPlugin`, `currentPhase`, `Path`) fills Invocation fields automatically.
- **Open plugin registry**: plugins self-register from `init()` via `plugins.RegisterPlugin`. Third-party plugins in external modules drop in via a side-effect import. Closed `registry` map literal removed.
- **Config hot-reload**: new `pipeline.Holder` (atomic wrapper) + `authlib/reloader` package (fsnotify-driven). Listeners receive `*Holder` instead of `*Pipeline`; the reloader atomically swaps the holder's contents when the config file changes. `mode` and `listener.*` edits are refused (pod restart required); any other change is picked up within the kubelet sync window (~60s). See §9.
- **Body mutation**: `PluginCapabilities.BodyAccess` split into `ReadsBody` / `WritesBody`. New `pctx.SetBody` / `pctx.SetResponseBody` helpers flip a mutation flag; all three listeners (extproc / forwardproxy / reverseproxy) propagate the rewrite to the upstream with correct `Content-Length` and cleared `Content-Encoding`. `BodyAccess` kept as deprecated alias. See §6, "Body mutation."
- **Detyped framework**: `pipeline/` no longer imports plugin-specific packages. **Breaking**: `Context.Claims *validation.Claims` → `Context.Identity Identity` (interface with `Subject()`/`ClientID()`/`Scopes()`); plugins publish adapters. `Context.Route` removed (was dead code). `Invocation`'s nine jwt-validation + token-exchange specific fields (`ExpectedIssuer`, `TokenSubject`, `RouteHost`, `CacheHit`, etc.) collapsed into `Details map[string]string`; built-in plugins migrated to `Details["expected_issuer"]` etc. `SessionEvent.TargetAudience` removed (was only populated from dead `pctx.Route`). Third-party plugins get a clean diagnostic slot they can populate without framework edits.
- **Single-owner packages relocated**: `authlib/validation` → `authlib/plugins/jwtvalidation/validation`. `authlib/exchange` / `authlib/cache` / `authlib/spiffe` → `authlib/plugins/tokenexchange/{exchange,cache,spiffe}`. Each plugin now lives in its own directory (`plugins/jwtvalidation/plugin.go`, `plugins/tokenexchange/plugin.go`) and self-registers via its own init(). `authlib/bypass`, `authlib/routing`, `authlib/auth` stay shared.
- **Plugin relationship declarations**: `PluginCapabilities` extended with four chain-scoped fields — `Requires` (all-must-be-earlier), `RequiresAny` (at-least-one-earlier), `After` (soft ordering), `Claims` (mutex on a semantic resource). Validated at `plugins.Build` time (startup + hot-reload); all errors per chain are collected into one report. `authlib/contracts/claims.go` ships `ClaimAuthorizationHeader` as the initial canonical claim constant. `token-exchange` and `token-broker` migrated to declare it, so configuring both on the same outbound chain now fails startup instead of silently clobbering each other's Authorization header. See [`plugin-reference.md` "Declaring plugin relationships"](./plugin-reference.md#declaring-plugin-relationships).
- **Per-request finish hook (`Finisher`)**: new optional interface `Finisher { OnFinish(ctx, pctx) }` gives stateful plugins a guaranteed release point that fires after every request end — regardless of whether the pipeline allowed, denied, or errored. Dispatch is LIFO across Finisher-implementing plugins whose OnRequest was invoked (including the denier). `pctx.Outcome()` returns a non-nil `*Outcome{FinalAction, StatusCode, DenyingPlugin, Duration}` during OnFinish and nil in all other phases. OnFinish runs under a fresh ctx derived from `context.Background()` with a framework-set deadline (default 2s, `WithFinishTimeout` overrides) so client disconnect during the request doesn't cancel cleanup I/O. Best-effort: panics are recovered; OnFinish is silent (no auto-emitted Invocations); `pctx.Record` / `SetBody` / `SetResponseBody` called during OnFinish are dropped with WARN logs since the SessionEvent is published and the response is on the wire. Closes the rate-limiter / audit / lease class of "cleanup leaks on denial" bugs. See §6 "Per-request finish hook".

Breaking changes will be announced in `authbridge/CHANGELOG.md` (TBD) before a 1.0 tag.

---

## 13. Cross-references

**Plugin-author docs** (pair with this framework reference):

- [`plugin-tutorial.md`](./plugin-tutorial.md) — step-by-step tutorial for writing a plugin.
- [`plugin-reference.md`](./plugin-reference.md) — plugin-author reference: config patterns, invocation emission contract, registration rules.

**Package sources:**

- `pipeline.go` — `Pipeline` type, `New`, `Run`, `RunResponse`, `Start`, `Stop`, `Plugins`, `NeedsBody`, `WritesBody`.
- `holder.go` — `Holder`, the atomic slot listeners hold in place of a raw `*Pipeline`.
- `plugin.go` — `Plugin` interface, `PluginCapabilities` (with `ReadsBody` / `WritesBody` / deprecated `BodyAccess` + `Normalize()`; chain-scoped relationship fields `Requires` / `RequiresAny` / `After` / `Claims`), `Configurable`, `Initializer`, `Shutdowner`, `Readier`, `Finisher`.
- `outcome.go` — `Outcome` struct + `OutcomeAction` (allow / deny / error) for `Finisher` consumers; `Context.Outcome()` getter.
- `action.go` — `Action`, `ActionType`, `Violation`, helper constructors (`Deny`, `DenyStatus`, `DenyWithDetails`, `Challenge`, `RateLimited`), `StatusFromCode`.
- `context.go` — `Context`, `Direction`, `AgentIdentity`, the `pctx.Record` / `Allow` / `Skip` / `Observe` / `Modify` / `DenyAndRecord` helpers, and `pctx.SetBody` / `SetResponseBody` / `BodyMutated` / `ResponseBodyMutated` for body mutation.
- `extensions.go` — `Extensions` struct, `Invocation`, `Invocations`, `InvocationAction`, named protocol extensions, `GetState` / `SetState`.
- `session.go` — `SessionEvent`, `SessionView`, `SessionPhase`, marshalers.
- `authlib/reloader/` — `Reloader`, `Status`, `PipelineBuilder`, `WithDrainWindow` / `WithDebounce` / `WithStartTimeout`, `Handler()` (serves `/reload/status`).

**Downstream integrators:**

- `authlib/session/` — `Store`, `SessionSummary`, ring buffer, TTL / max-events caps.
- `authlib/sessionapi/` — HTTP API (`/v1/sessions`, `/v1/events`, `/v1/pipeline`) surfacing all of the above.
- `authlib/plugins/` — built-in plugin implementations and registry.
- `cmd/authbridge/listener/extproc/` — reference usage for all three phases.
- `cmd/abctl/` — TUI consumer of the session API, useful as a reference integrator.
