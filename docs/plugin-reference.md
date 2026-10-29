# Plugin Author Reference

**Audience:** plugin authors who already know the basics and need the
contract — field names, invariants, error behaviour, the rules that the
framework enforces at startup.

**See also:**
- [`plugin-catalog.md`](./plugin-catalog.md) — catalog of implemented plugins.
- [`plugin-tutorial.md`](./plugin-tutorial.md) — step-by-step tutorial for writing a new plugin.
- [`framework-architecture.md`](./framework-architecture.md) — how the pipeline
  composes plugins, the lifecycle, the Context / Extensions wire shape.

How plugins under `core/plugins/` receive, validate, and
apply their configuration; emit session events; and register themselves
with the pipeline builder. Everything here is convention — the framework
only requires `pipeline.Configurable` if the plugin has any config at
all. The rest of this document exists so that the sixth and tenth
plugin don't each invent their own style.

## Scope

- What the YAML entry for a plugin looks like.
- How a plugin decodes that YAML into a typed config struct.
- How a plugin applies defaults and runs validation.
- What the framework does and doesn't do on your behalf.
- A template you can copy for a new plugin.

## YAML entry shape

Each plugin appears in the pipeline as either a bare name or a full entry:

```yaml
pipeline:
  inbound:
    plugins:
      - a2a-parser                       # bare name — no config
      - name: jwt-validation
        id: jwt-validation               # optional; defaults to name
        config:
          issuer: "http://keycloak..."
          audience_file: "/shared/client-id.txt"
          bypass_paths:
            - "/healthz"
      - name: pii-scrubber
        on_error: observe                # canary: log would-blocks, don't block
        config:
          patterns: [ssn, credit_card]
```

- **`name`** — required. Must match a key in the plugin registry.
- **`id`** — optional. Defaults to `name`. Lets two instances of the same
  plugin coexist with different config (not yet exercised, but the shape
  is reserved).
- **`on_error`** — optional. One of `enforce` (default), `observe`, or
  `off`. See [`on_error` policy](#on_error-policy) below.
- **`config`** — optional. Arbitrary YAML sub-tree owned by the plugin.
  The framework does not interpret it; it's captured as `json.RawMessage`
  and handed to `Configure`.

### `jwt-validation`: `allowed_audiences` (optional, transitional)

The `jwt-validation` plugin accepts an optional JSON array field
`allowed_audiences` (YAML under `config:`). Each entry is an additional
**expected** inbound JWT `aud` value. The plugin **union**s these with
the primary audience from `audience` / `audience_file` (deduplicated,
order preserved). Inbound verification succeeds if the token's `aud`
claim (RFC 7519 — string **or** array) contains **any** configured value.

Use this as a **short-term bridge** when legitimate tokens carry
multiple audiences (for example a public UI client plus `account`) and
the workload must accept more than the SPIFFE / client ID read from
`audience_file` alone. Prefer aligning IdP audience policy and
application token exchange long-term; see team design discussions linked
from release notes.

When `allowed_audiences` is non-empty, the default
`/shared/client-id.txt` `audience_file` is **not** auto-added unless you
still omit `audience`, `audience_file`, and `allowed_audiences` entirely
— i.e. you can run with **only** `allowed_audiences` for static mode.

**Session / log migration:** jwt-validation deny-path `Details` used a
single key `expected_audience` (string). It now emits
`expected_audiences` (comma-joined list of configured inbound audiences).
Update saved queries and dashboards that filtered on the old
key.

### `jwt-validation`: `placeholder_mode` / `placeholder_ttl` (optional, off by default)

Two fields enable the **mint** half of [credential placeholder
swap](token-exchange-plugin.md#credential-placeholder-swap) (read that section for the full
model — these are the field-level reference).

- **`placeholder_mode`** — bool, default `false`. After validating the
  inbound token, replace it with an opaque `abph_`-prefixed placeholder
  before forwarding to the agent. The real token is held in the
  process-scoped shared store for the outbound path to resolve. Requires
  `token-exchange` with `resolve_placeholders: true` on the outbound
  chain to be coherent (see the matched-pair note below).
- **`placeholder_ttl`** — Go duration string (e.g. `30m`, `1h`), default
  `1h`. How long the real token is retained for outbound resolution.

Mint requires `pctx.Shared` to be wired by the listener; if it is nil
when `placeholder_mode` is on, the plugin fails fast at init (a deploy
error) rather than silently forwarding the real token.

## `on_error` policy

> **Naming caveat.** Despite the name, `on_error` controls how the
> framework handles **intentional `Deny` actions** returned by the
> plugin — it is not a panic/error handler. A panic or a
> runtime-error return still surfaces as a 500 regardless of this
> setting. Reach for `on_error` to stage the rollout of a new
> guardrail (observe before enforce) or to toggle a plugin off
> without a redeploy; reach for something else when you want to
> bound misbehavior.

`on_error` is a **framework-owned** wrapper around the plugin — plugin
authors do not read it, implement it, or branch on it. Its job is to
let operators roll out a new guardrail without risking production.

| Policy | Plugin dispatched? | Reject → | Body mutation → | Typical use |
|---|---|---|---|---|
| `enforce` (default) | yes | HTTP error, pipeline stops | applied to wire | Production guardrails |
| `observe` | yes | shadow `Invocation`, request passes | suppressed (no-op) | Canarying a new plugin |
| `off` | **no** | n/a | n/a | Kill-switch without redeploy |

### Observe is plugin-transparent

Under `observe`, the plugin's `OnRequest` / `OnResponse` runs exactly as
under `enforce`. If it returns `pipeline.Deny(...)`, the framework
intercepts: it marks the plugin's `Invocation` with `Shadow: true`, logs
a `WARN pipeline: plugin would have denied (shadow)` line, and continues
the pipeline. The request is not blocked. Body-mutation calls
(`SetBody` / `SetResponseBody`) likewise record a `Shadow: true`
invocation but do not alter the in-memory body or the wire bytes —
downstream plugins and the upstream see the body as it was — and return
`false`, so a plugin that reports its own outcome (applied or only
measured) can tell without knowing its policy.

The upshot: the same plugin binary, dispatched the same way, is safe to
ship in `observe` for a week while operators watch shadow metrics,
then flipped to `enforce` with confidence.

### Shadow timeline query

Operators count would-have-blocked events by filtering Invocations on
`shadow: true`:

- `count(Invocations where shadow=true)` — rollout candidate volume
- `count(Invocations where shadow=true and action="deny") by plugin` —
  per-plugin shadow block rate
- `count(Invocations where shadow=false and action="deny") by plugin` —
  enforced denials (unchanged by this feature)

### Don't put matched content in `Violation.Reason`

`Reason` (the free-text explanation on `pipeline.Deny` / `DenyAndRecord`
and on `Invocation.Reason`) flows to two places: the session store at
`/v1/sessions` and, under `observe`, a `WARN` log line. Both live
outside the authorization domain of the request itself — logs typically
aggregate to a different backend than session events, with different
retention and access policies.

Plugin authors: **keep `Reason` to a machine-stable short code and/or a
generic description.** Do not echo matched content, raw user input, or
credential-shaped substrings into `Reason`.

```go
// GOOD
return pctx.DenyAndRecord("ssn_match", "pii.detected", "matched PII pattern")

// BAD — echoes a matched SSN into logs and the session store:
return pctx.DenyAndRecord("ssn_match", "pii.detected",
    fmt.Sprintf("matched SSN %s at offset 42", ssn))
```

If you need to record which pattern matched for debugging, put a
**hash or stable fingerprint** of the content in `Invocation.Details`
(`Details["match_sha256"] = "a1b2c3..."`), never the raw match. The
same rule applies to `Details` values — see the existing "NEVER put
raw tokens, signatures, or client credentials" note on the Invocation
type below.

### Off vs. removing the entry

Both achieve "don't run this plugin." `off` exists so a single field
flip re-enables the plugin without re-adding the whole block to YAML.
An `off` entry is not `Configure`d and not added to the running
pipeline; its `config:` subtree is not validated. Remove the entry
entirely if you don't anticipate re-enabling it.

### What `on_error` does not do

- Not a circuit breaker — a crashing plugin in `observe` still crashes
  on every request. Bound crash loops with a separate mechanism.
- Not sampling — `observe` runs the plugin on 100% of traffic.
  Percentage rollout is a future feature.
- Not a timeout — a slow plugin still blocks the request. Per-plugin
  deadlines are a separate knob.
- Not a panic / runtime-error handler (see the leading caveat at the
  top of this section).

### Applicability to auth gates

`on_error` is a generic framework knob that applies to every plugin
including built-in auth gates (`jwt-validation`, `token-exchange`).
Shadowing auth turns authentication into a suggestion — don't do this
in production. Shadow-mode is for third-party guardrails being
canaried; auth gates should stay on `enforce` (the default).

## Declaring plugin relationships

`PluginCapabilities` carries three fields that let a plugin express how it
depends on other plugins in the same chain. All are checked at
`plugins.Build` time (startup and hot-reload), and misconfigurations
fail loud before serving traffic.

```go
type PluginCapabilities struct {
    ReadsBody   bool
    WritesRequestBody  bool
    WritesResponseBody bool
    WritesDestination  bool   // plugin may call pctx.Redirect

    Requires      []string   // ALL must be present + earlier (hard)
    RequiresAny   []string   // AT LEAST ONE must be present + run after it (hard)
    RequiresLater []string   // ALL must be present + later in the chain (hard)

    Description string
}
```

| Field | All present? | At least one? | Ordering enforced? |
|---|---|---|---|
| `Requires` | ✓ | — | must be earlier |
| `RequiresAny` | — | ✓ | must be earlier |
| `RequiresLater` | ✓ | — | must be later |

Both are chain-scoped — validation runs within the inbound chain
OR within the outbound chain, independently. Plugin names are
case-sensitive and must match the `Name()` returned by the plugin.

### `Requires` — hard dependency

Use when your plugin hardcodes access to a specific other plugin's
extension state. Each named plugin must be present in the same chain
AND appear earlier (lower index). Missing or misordered fails startup.

```go
// Reads pctx.Extensions.MCP.Params["name"] directly — only makes sense
// with mcp-parser ahead of it.
func (p *ToolAllowlist) Capabilities() pipeline.PluginCapabilities {
    return pipeline.PluginCapabilities{
        Requires: []string{"mcp-parser"},
    }
}
```

### `RequiresAny` — hard OR

Use when your plugin is protocol-agnostic (e.g. reads through
`pctx.ContentSources()`) but genuinely needs at least one parser
present to have anything to do. Each named plugin that IS present
must also appear earlier. Missing-all-of-them fails startup.

```go
// PII scrubber runs against whatever parsers emit fragments; must
// have at least one or it's silent dead code.
func (p *PIIScrubber) Capabilities() pipeline.PluginCapabilities {
    return pipeline.PluginCapabilities{
        ReadsBody: true,
        RequiresAny: []string{
            "a2a-parser", "mcp-parser", "inference-parser",
        },
    }
}
```

### `RequiresLater` — hard dependency on a later plugin

The mirror of `Requires`, for dependencies on another plugin's
*response-phase* output. The response pass walks the chain in reverse,
so "runs before me on the response" means "later in the chain" — declare
the producer with `RequiresLater` and it must sit at a *higher* index
than your plugin.

```go
// Reads what the parser found in the response, on the response pass —
// only makes sense with a parser after it in the chain.
func (p *ResponseAuditor) Capabilities() pipeline.PluginCapabilities {
    return pipeline.PluginCapabilities{
        RequiresLater: []string{"mcp-parser"},
    }
}
```

### Error collection

When validation fails the error aggregates every violation in the
chain, not just the first one found. Operators iterating on a
freshly-edited YAML get one fix-list per startup attempt rather
than a sequence of fix-one-at-a-time restarts.

## The Configurable interface

```go
type Configurable interface {
    Configure(raw json.RawMessage) error
}
```

The framework calls `Configure` exactly once per plugin instance, during
pipeline construction, before `Start`. Plugins without config don't
implement this interface — the builder type-asserts and skips them.

If a plugin **does not** implement `Configurable` but the YAML entry
has a non-empty `config:` block, the builder fails with a clear
`"plugin %q does not accept configuration"` error. This catches
misconfigurations (typo in plugin name, leftover config after a
refactor) at startup.

## The four-step Configure pattern

Every Configurable plugin follows the same shape:

```go
func (p *Plugin) Configure(raw json.RawMessage) error {
    var c pluginConfig
    if len(raw) > 0 {
        dec := json.NewDecoder(bytes.NewReader(raw))
        dec.DisallowUnknownFields()             // 1. strict decode
        if err := dec.Decode(&c); err != nil {
            return fmt.Errorf("plugin config: %w", err)
        }
    }
    c.applyDefaults()                           // 2. fill in defaults
    if err := c.validate(); err != nil {        // 3. validate
        return fmt.Errorf("plugin config: %w", err)
    }
    // 4. construct internal state
    p.verifier = newVerifier(c.Issuer, c.JWKSURL)
    p.bypass = bypass.New(c.BypassPaths)
    return nil
}
```

### 1. Strict decode (`DisallowUnknownFields`)

Always. A stale or misspelled key is a mistake, not a preference. Loud
failure at startup beats a silent wrong default at request time.

### 2. `applyDefaults()`

Fills zero-value fields with sensible defaults and derives computed
fields. Keep it pure — no I/O, no file reads — so it can be unit-tested
with the config struct alone.

```go
func (c *pluginConfig) applyDefaults() {
    if c.DefaultPolicy == "" {
        c.DefaultPolicy = "passthrough"
    }
    if c.JWKSURL == "" && c.Issuer != "" {
        c.JWKSURL = c.Issuer + "/protocol/openid-connect/certs"
    }
}
```

When you need to distinguish "unset" from "explicitly set to zero" —
typically for booleans — use `*bool` / `*int` in the struct and convert
to plain values after `applyDefaults`. `SessionConfig.Enabled` in
`core/config` is the reference pattern.

### 3. `validate()`

Rejects configurations the plugin cannot operate with. Run validation
**after** `applyDefaults` so derived fields are in place.

```go
func (c *pluginConfig) validate() error {
    if c.Issuer == "" {
        return errors.New("issuer is required")
    }
    if c.DefaultPolicy != "passthrough" && c.DefaultPolicy != "exchange" {
        return fmt.Errorf("default_policy must be passthrough or exchange, got %q", c.DefaultPolicy)
    }
    return nil
}
```

Return errors phrased for an operator reading a pod log, not a developer
reading a stack trace.

### 4. Construct internal state

This is the only step allowed to do I/O (read credential files, open
connections, etc.). Everything the plugin needs at request time should
be materialized here, not lazily on first `OnRequest` — lazy init
hides config errors until traffic arrives.

## File-sourced values

Several plugins accept either an inline value or a file path for the
same datum (e.g. `client_secret` vs `client_secret_file`). The
convention:

- Both fields live in the config struct; the file variant has the
  `_file` suffix.
- `applyDefaults` does not read the file.
- `validate` requires exactly one to be set.
- Internal state construction calls the file-read helper from
  `core/config` (not a new one), which tolerates transient absence
  during pod boot (the operator-managed Secret at `/shared/` may not yet be mounted).

## What Configure MUST NOT do

- **Block forever.** Configure runs before traffic starts; the process
  is still holding the startup deadline. Use bounded waits with
  timeouts, not unbounded blocking reads.
- **Start background goroutines.** Use `Init(ctx)` from the
  `pipeline.Initializer` interface for that — it runs after Configure
  and has a process context you can key your goroutine's lifetime to.
- **Mutate global state.** Plugins run in a single process today, but
  the config → runtime mapping must stay per-instance. Two instances
  of the same plugin with different config must not clobber each other.
- **Persist the raw bytes.** Decode into your typed struct and drop
  the `json.RawMessage`. Holding it leaks the original YAML, which
  may contain secrets, into any log that dumps the plugin for
  debugging.

## Testing

Each Configurable plugin ships three kinds of tests:

1. **Config round-trip.** Given a YAML snippet, does Configure produce
   the expected internal state? Exercise defaults-applied and defaults-
   rejected paths explicitly.
2. **Validation failures.** One test per validation error path — name
   a missing-required field, a malformed value, a conflicting pair.
   Assert the error message names the bad field.
3. **Behavior integration.** The existing `OnRequest` / `OnResponse`
   tests, but wired through Configure rather than hand-built internal
   state. This is what keeps the config layer and the plugin behavior
   honest about each other.

## Template

Copy this into a new plugin file as the starting point. Replace
`myPlugin` with your plugin's identifier.

```go
package plugins

import (
    "bytes"
    "encoding/json"
    "errors"
    "fmt"

    "github.com/rossoctl/cortex/core/pipeline"
)

// myPluginConfig is the plugin's private config schema. Fields are JSON-
// tagged so Configure can DisallowUnknownFields against operator-supplied
// YAML (YAML → JSON round-trip preserves key names).
type myPluginConfig struct {
    SomeKnob   string   `json:"some_knob"`
    SomePaths  []string `json:"some_paths"`
    // ...
}

func (c *myPluginConfig) applyDefaults() {
    if c.SomeKnob == "" {
        c.SomeKnob = "default-value"
    }
}

func (c *myPluginConfig) validate() error {
    if c.SomeKnob == "" {
        return errors.New("some_knob is required")
    }
    return nil
}

type MyPlugin struct {
    // internal state populated by Configure
}

func (p *MyPlugin) Configure(raw json.RawMessage) error {
    var c myPluginConfig
    if len(raw) > 0 {
        dec := json.NewDecoder(bytes.NewReader(raw))
        dec.DisallowUnknownFields()
        if err := dec.Decode(&c); err != nil {
            return fmt.Errorf("my-plugin config: %w", err)
        }
    }
    c.applyDefaults()
    if err := c.validate(); err != nil {
        return fmt.Errorf("my-plugin config: %w", err)
    }
    // construct internal state from c
    return nil
}

func (p *MyPlugin) Name() string                             { return "my-plugin" }
func (p *MyPlugin) Capabilities() pipeline.PluginCapabilities { return pipeline.PluginCapabilities{} }
func (p *MyPlugin) OnRequest(_ context.Context, _ *pipeline.Context) pipeline.Action {
    return pipeline.Action{Type: pipeline.Continue}
}
func (p *MyPlugin) OnResponse(_ context.Context, _ *pipeline.Context) pipeline.Action {
    return pipeline.Action{Type: pipeline.Continue}
}
```

## Strictness asymmetry: plugin config vs. runtime top-level

The plugin-level config inside each `plugins[].config` subtree is
**strict** — `DisallowUnknownFields` is part of the Configure
convention, so a typo or a stale key fails the plugin at boot.

The runtime YAML's **top-level** keys (`mode`, `listener`, `pipeline`,
`session`, `stats`) are **forgiving**: unknown top-level keys are
silently ignored by the YAML decoder. This is deliberate forward-
compat — adding a new top-level section (say, `observability:`) in a
future release must not break older binaries reading a newer config.

The obvious gap — an operator keeping the pre-migration top-level
schema (`inbound:`, `outbound:`, `identity:`, `bypass:`, `routes:`)
would have their config silently accepted with those keys dropped —
is closed by `config.Validate`, which errors when either pipeline
list is empty. The error message names the likely cause so the
operator is pointed at the migration, not left wondering why
authentication isn't happening.

## Emitting session events

Every plugin MUST emit at least one `Invocation` record per active
`OnRequest` / `OnResponse` call. Plugins may also populate one of the
typed protocol extensions (`MCP`, `A2A`, `Inference`) when they carry
structured semantic payload, and may additionally publish plugin-
specific events through the `Custom` escape-hatch map.

> For a tutorial on emitting Invocations — the `pctx.Record` / `Allow`
> / `Skip` / `Observe` / `Modify` / `DenyAndRecord` helpers with
> runnable examples — see [`plugin-tutorial.md` Step 2](./plugin-tutorial.md#step-2--record-what-your-plugin-did).
> This section is the field-level reference for the `Invocation`
> struct, the 5-value action vocabulary, and the rules around the
> Custom escape-hatch map.

### 1. Invocation record — field reference

An `Invocation` says *which* plugin ran and *what* it did, in a
5-value vocabulary shared across all plugins. agentop renders one row
per invocation. Every plugin that runs on a pipeline pass produces
at least one.

```go
type Invocation struct {
    Plugin  string           // plugin.Name(); framework-filled
    Action  InvocationAction // 5-value: allow | deny | skip | modify | observe
    Phase   InvocationPhase  // "request" | "response"; framework-filled
    Reason  string           // machine-stable code
    Path    string           // request path; framework-filled

    // Plugin-specific diagnostic context. Opaque to the framework;
    // agentop renders as key=value rows in the detail pane.
    Details map[string]string

    // Shadow is framework-set; plugins never write it. True when the
    // plugin ran under on_error: observe and its decision (deny or
    // modify) was NOT applied to the request. Dashboards partition
    // on Shadow: enforced outcomes (shadow=false) vs rollout
    // candidates (shadow=true).
    Shadow bool
}
```

The framework fills `Plugin`, `Phase`, and `Path` when the plugin
emits via `pctx.Record` / `Allow` / `Skip` / `Observe` / `Modify` /
`DenyAndRecord`. A plugin may override those fields explicitly — but
only in test harnesses where the plugin runs outside a
`Pipeline.Run` dispatch loop.

**The 5-value action vocabulary** (complete):

| Action | Meaning | Example |
|---|---|---|
| `allow` | Gate plugin permitted the request | jwt-validation on valid token |
| `deny` | Gate plugin rejected the request; pipeline stops | jwt-validation on bad token, token-exchange on IdP failure |
| `skip` | Plugin ran but didn't act on this message | jwt-validation bypass path; parser whose body didn't match |
| `modify` | Plugin mutated the message | token-exchange replaced the Authorization header |
| `observe` | Plugin attached diagnostic data; flow unchanged | parsers extracting MCP / A2A / Inference state |

`Reason` is a stable machine-readable label (e.g. `path_bypass`,
`no_matching_route`, `jwt_failed`, `matched_tools/call`) that
discriminates within an Action value. agentop filters can match
either — `/skip` shows every skip action regardless of reason;
`/path_bypass` narrows to that specific skip flavour.

**What to put in Details:**

Suggested key conventions used by built-in plugins (operators
already know these; agentop filters match substring on both key and
value):

- Auth gates (jwt-validation): `expected_issuer`, `expected_audiences`
  (comma-joined configured inbound audiences), `token_subject`,
  `token_audience`, `token_scopes`.
- Outbound routers (token-exchange): `route_matched` (`"true"`/`"false"`),
  `route_host`, `target_audience`, `requested_scopes`, `cache_hit`.
- Parsers: usually no Details — their semantic payload lives on the
  typed extension slot (A2A / MCP / Inference). Emit with just
  Action + Reason.
- Third-party plugins: pick snake_case keys scoped to your semantic
  domain (`tokens_remaining`, `quota_bucket`, `redaction_count`, etc.).

**Value encoding:** `Details` values are strings; plugins must
stringify non-string data themselves. Conventions across built-ins:

| Go type | Encoding | Rationale |
|---|---|---|
| `string` | as-is | no transform |
| `bool` | `"true"` / `"false"` | stable, parse-friendly |
| `int` / `float` | `strconv.Itoa` / `strconv.FormatFloat` | decimal, no unit suffix |
| `[]string` — OAuth scopes | space-joined (`"openid email"`) | RFC 6749 forbids spaces in scope tokens, so space-delimited is unambiguous |
| `[]string` — JWT audiences | comma-joined (`"aud-a,aud-b"`) | RFC 7519 permits spaces in `aud`, so space-joining would be ambiguous |
| `[]string` — other | comma-joined by default; pick a delimiter that can't appear in your values | operator split-on-delimiter needs one predictable choice |
| `time.Time` | RFC 3339 | consistent with logs |
| `time.Duration` | `strconv.FormatInt(d.Milliseconds(), 10)` + `_ms` suffix on the key | milliseconds integer is agentop-friendly |

If your field's elements might contain the delimiter, pick a different
delimiter and document it on the field rather than escape — consumers
doing `strings.Split` are simpler to write against an unambiguous
separator than against an escape convention.

**NEVER put raw tokens, signatures, or client credentials in
`Details`.** The session store has no auth on it; only safe-to-log data
belongs in Invocations.

### 1b. Operator-facing counters (`MetricsProvider`)

Invocations describe *this* request. For running totals an operator reads while
debugging — how many requests a plugin acted on, how many bytes it saved —
implement `pipeline.MetricsProvider`:

```go
type Metric struct {
    Name  string  `json:"name"`
    Value float64 `json:"value"`
    Unit  string  `json:"unit,omitempty"` // count | bytes | tokens | ratio
    Note  string  `json:"note,omitempty"` // e.g. "estimate, n=1284"
}

type MetricsProvider interface { Metrics() []Metric }
```

`describePipeline` calls `Metrics()` while serving `/v1/pipeline`, so it must be
safe for concurrent use with the request path and must not block — take a
mutex, copy, release. Returning `nil` is fine; agentop renders `(none)`.

Rules worth honouring:

- **Label anything derived.** A figure the plugin computed rather than counted
  goes in with a `Note` naming its sample size. A derived number with no `Note`
  reads as a measurement.
- **Report the sample alongside the conclusion.** Counters are per-process and
  reset on restart *and on config hot-reload* (a reload rebuilds the plugin), so
  a bare average is uninterpretable without the count behind it.
- **Don't route counters through `auth.Stats`.** That type is auth-shaped —
  typed approval/denial enums and a custom `MarshalJSON` — and carrying
  unrelated totals through it distorts its meaning.

This is an optional interface, so it is **not** promoted through
`configuredPlugin`'s embedded `Plugin`. The wrapper forwards it explicitly, the
same way it forwards `Initializer` / `Shutdowner` / `Finisher` / `Readier`; a new
optional interface must be added there too or it will be invisible for every
plugin that has config.

### 2. Named protocol extension (optional, for parsers)

`MCP`, `A2A`, `Inference` are typed slots on `pipeline.Extensions`.
A parser that successfully extracts structured state populates the
matching slot AND emits an `Invocation` with `ActionObserve`. The
slot carries the parsed payload; the Invocation carries the
attribution.

Adding a new named extension is a core-library change: edit
`pipeline/extensions.go`, `pipeline/session.go` (wire + JSON round-
trip), the listener (snapshot + recorder), and agentop if you want
bespoke rendering. Most new plugins don't need one — they emit an
Invocation and publish extra context through the Custom map
(below).

### 3. Escape-hatch map (`Custom` with `/event` suffix)

For plugin-specific observability that doesn't warrant a category yet,
write to `pctx.Extensions.Custom` with a key ending in
`pipeline.PluginEventSuffix` (`"/event"`):

```go
// Plugin-PUBLIC event. Listener serializes this to SessionEvent.Plugins
// under key "rate-limiter" (suffix stripped).
pctx.Extensions.Custom["rate-limiter"+pipeline.PluginEventSuffix] = rateLimiterEvent{
    Allowed:    true,
    TokensLeft: 42,
}

// Plugin-PRIVATE cross-phase state. Never serialized. Used via the
// typed SetState / GetState generics.
pipeline.SetState(pctx, "rate-limiter", &rateLimiterState{Bucket: b})
```

The `/event` suffix is the opt-in marker: the listener only promotes
matching keys into `SessionEvent.Plugins`. Private state stays out.

Rules for plugin-public events:

- **Value must be JSON-marshalable.** The listener calls `json.Marshal`;
  failures downgrade to `slog.Debug` and skip the entry (a misbehaving
  plugin can't break the session stream).
- **NEVER put raw credentials or tokens in the value.** The session
  store has no auth on it — only safe-to-log data belongs there.
- **Key prefix MUST be the plugin's `Name()`.** Keeps namespaces clean
  so unrelated plugins don't collide.
- **Payload schema is plugin-owned.** No central registry; agentop
  treats unknown keys as raw JSON in the detail pane.

### Graduation: when to promote map → named category

Graduate to a typed slot when ≥2 of these are true:

1. **Two or more plugins share the shape.** That's the signal the
   "category" concept is worth codifying — it prevents N plugins from
   each shipping their own near-identical struct.
2. **agentop or the session API grows conditional logic on the key.**
   If consumers already parse the payload, making the schema compile-
   checked is a net win.
3. **The data is populated on nearly every deployment.** Core
   semantics (auth, protocol) graduate; niche plugins stay in the map.

Don't graduate speculatively — the map path has no cost if you stay
in it.

## Body mutation

Plugins that need to rewrite request or response bodies declare
`WritesRequestBody: true` and call the `pctx.SetBody` / `pctx.SetResponseBody`
helpers. The framework propagates the rewrite to the wire, emits a
`modify`-action Invocation, and publishes a `body-mutation/event`
entry in `pctx.Extensions.Custom` with length delta + sha256
before/after (never the raw body). With several mutators in one direction there
is one entry for that direction's chain. For a request, `before` is the bytes
the client sent and `after` the bytes sent upstream. For a response, `before` is
the upstream's bytes and `after` the bytes the client receives. `plugins` lists
the writers whose writes took effect, in the order they ran (`plugin` is the
last of them) — or, while none has, the observed writer whose would-be rewrite
the entry describes (its invocation carries `shadow: true`).

> For the full lifecycle — per-listener wire behavior, content-encoding
> policy, ordering rules, body-size limits — see
> [`framework-architecture.md` §6, "Body mutation"](./framework-architecture.md#body-mutation).
> This section is the plugin-author field reference.

### Capability fields

```go
type PluginCapabilities struct {
    ReadsBody          bool // plugin reads pctx.Body / pctx.ResponseBody
    WritesRequestBody  bool // plugin may call pctx.SetBody and pctx.SetRequestModel
    WritesResponseBody bool // plugin may call pctx.SetResponseBody
}
```

- `ReadsBody`: listener buffers the body; plugin sees bytes.
- `WritesRequestBody`: implies `ReadsBody`. Listener propagates `pctx.SetBody`
  rewrites to the upstream. Required by `pctx.SetRequestModel`, which refuses a
  plugin that does not declare it.
- `WritesResponseBody`: implies `ReadsBody`. Listener propagates
  `pctx.SetResponseBody` rewrites to the downstream client.

**Declare the direction you actually write.** The two flags are not
interchangeable, and getting this wrong is silent: `WritesResponseBody` is the
SSE streaming predicate. A plugin that declares it forces every response on
that chain onto the buffered path, so a long completion arrives in one lump
after a silent wait instead of appearing incrementally. Declaring
`WritesRequestBody` costs nothing — requests arrive complete with a
`Content-Length` and are read end to end before dispatch, so rewriting one says
nothing about how the response may be relayed.

| Plugin shape | Declares | Streams responses? |
|---|---|---|
| request-only mutator (`tool-prune`, `context-guru`, `inference-router`) | `WritesRequestBody` | yes |
| response mutator (`sparc`) | `WritesResponseBody` | no — buffered |
| both (`cpex`) | both | no — buffered |
| pure reader (parsers) | `ReadsBody` | yes |

**A configured instance may declare less than its ceiling.** What an
*unconfigured* instance's `Capabilities()` returns is the ceiling: it is what
`plugins.Catalog()` reads, once, and so what `/v1/plugins` and agentop's catalog
show. After `Configure`, an instance may return less, and the pipeline it is
built into reads that narrower set. A plugin whose config means it will never
rewrite a response should drop `WritesResponseBody` there, so that it stops
buffering every streamed response on its chain. What it may return
(`PluginCapabilities.WithinCeiling`):

- no flag the ceiling leaves false, and no `Requires` or `RequiresLater` name
  the ceiling does not list — a flag or a name may be dropped;
- not `ReadsBody` alone when the ceiling writes a body: a pure reader must
  precede every mutator, which a writer need not;
- `RequiresAny` as the ceiling's set or empty: one of its names satisfies it and
  every one present must come earlier, so dropping a name and adding one each
  refuse chains the ceiling accepts.

Anything else, `plugins.BuildWithDeps` logs as a WARN naming the difference,
because the catalog no longer describes the plugin. It warns rather than refuses
because the older rule — capabilities never vary — was never enforced. The
listener check is the exception: a listener-dependent capability such as
`WritesDestination` is checked against the configured set, and refused where
the listener cannot honor it.

```go
func (p *Redactor) Capabilities() pipeline.PluginCapabilities {
    caps := pipeline.PluginCapabilities{WritesRequestBody: true, WritesResponseBody: true}
    if p.configured && !p.cfg.RedactResponses {
        caps.WritesResponseBody = false
    }
    return caps
}
```

### Build-time validation (enforced by `pipeline.New`)

- Request mutators **chain**: any number may share a pipeline. They run in
  chain order, each seeing `pctx.Body` as the one before it left it, and the
  listener sends the last one's bytes — so `tool-prune` and `context-guru` run
  together, and a plugin that changes the model can follow them. Two things
  follow for a writer that may not be first:
  - **Derive your edit from `pctx.Body`.** The parser extensions
    (`pctx.Extensions.Inference` and the rest) are not re-parsed after a write:
    they describe the client's request — apart from the model, which
    `pctx.SetRequestModel` keeps current — so an edit built from them can undo
    an earlier writer's rewrite or land on the wrong element.
  - **Learn whether your own write applied from `SetBody`'s result**, not from
    `pctx.BodyMutated()`. `BodyMutated()` is request-wide: once any writer's
    bytes took effect it is true, including for a later writer whose own call
    was a shadow under `on_error: observe`. A writer that reports a saving as
    applied or only measured, or counts it, reads its own result.
- Response mutators **chain** too. The response pass runs in reverse, so the
  mutator placed last in the chain sees the upstream's bytes first. Each one
  sees `pctx.ResponseBody` as the one that ran before it left it, and the
  listener sends what the last to run left. A request mutator and a response
  mutator coexist, and one plugin may be both (`cpex`).
- A mutator of **either** direction cannot precede a `ReadsBody`-only plugin.
  The reader must see the bytes the client sent, however many mutators follow it.

> **Readers see what the agent saw.** Every reader precedes every mutator, so
> a reader sees the request as the client sent it and the response as the
> client receives it. The response pass runs in reverse, so every response
> mutator has run before any reader does, and the parsers' per-frame pass runs
> after it. Parser output, the session events agentop shows, and the cost
> settled from them therefore describe what the agent saw. The flip side: a
> response mutator cannot use what the parsers found in this response, because
> they have not seen it yet. It parses `pctx.ResponseBody` itself, as `cpex`
> does.
>
> **Declaring is a contract, not an enforcement.** `SetBody` flips
> `bodyMutated` unconditionally outside observe mode and the listeners gate
> purely on that flag, so a plugin that calls `SetBody` *without* declaring the
> capability still reaches the wire. This divergence is documented rather than
> closed, because adding the check silently would break out-of-tree plugins
> relying on today's behaviour. Do not read it as a way to keep response
> streaming — declare `WritesRequestBody`, which costs no streaming anyway.

### Mutation helpers

| Call | Effect |
|---|---|
| `pctx.SetBody(newBytes)` | Replace request body; flip `BodyMutated()` flag. Returns whether **this** write took effect: `false` under `on_error: observe` (a shadow write, body unchanged) and in `OnFinish` |
| `pctx.SetResponseBody(newBytes)` | Replace response body; flip `ResponseBodyMutated()` flag. Returns whether this write took effect, as `SetBody` does |
| `pctx.BodyMutated()` / `ResponseBodyMutated()` | Read by the listener to decide whether to emit a wire mutation. Request-wide (response-wide): true once **any** writer's bytes took effect, so not a writer's own outcome. Plugins normally don't need these. |

Direct assignment (`pctx.Body = newBytes`) still compiles but the
listener won't propagate it, no Invocation fires, and the mutation
event won't appear in the session stream. Always use `SetBody`.

**NEVER log the raw body content.** The framework's
`body-mutation/event` carries only length + sha256 on purpose — the
session store is unauthenticated. Plugin-private debug logs may
include body bytes at DEBUG level, but never publish them to the
session stream or Custom map.

## Redirecting a request

A plugin that decides where a request goes — a router, a failover — declares
`WritesDestination: true` and calls `pctx.Redirect` from `OnRequest`:

```go
if pctx.Redirectable() {
    if err := pctx.Redirect(&url.URL{Scheme: "https", Host: "glm-litellm.example.com"}); err != nil {
        return pctx.DenyAndRecord("redirect_failed", "router.redirect", err.Error())
    }
    // Only when the request really goes there: under on_error: observe, Redirect
    // returns nil and moves nothing, but this header write would still apply.
    if pctx.Redirected() {
        pctx.Headers.Set("Authorization", "Bearer "+glmKey)
    }
}
```

| Call | Effect |
|---|---|
| `pctx.Redirect(target)` | Sends the request to `target`'s scheme and host. `target` carries nothing else — no user info, no path beyond `/`, no query, no fragment. |
| `pctx.Redirectable()` | Whether the listener will honor a redirect on this request. False on a `CONNECT` and on a transparently redirected connection, which are dialed where the client chose. True on the TLS-bridged requests decrypted from either, which the forward proxy re-originates — that is how HTTPS inference traffic arrives. |
| `pctx.Redirected()` | Whether a redirect took effect. False under `on_error: observe` and after a refusal. |
| `pctx.RedirectTarget()` | The scheme and host the last accepted redirect validated, and whether there was one. Listeners apply this. |
| `pctx.RequestedHost()` | The host the client named, or `""` unless the request now goes elsewhere. |

**`pctx.Host` follows the redirect.** The session events, usage, the cost ledger
and modelled pricing all key on it, so they describe where the bytes went. A
plugin that runs after the redirect and keys on the host sees the new one. Each
session event records the requested host beside it as `requestedHost`. Writing
`pctx.Host` does not move a request: the listener applies `RedirectTarget`, the
copy `Redirect` validated, and resets `pctx.Host` to match before recording.

The framework records every redirect as `modify/redirected` with `from` and `to`
in `Details`. Under `on_error: observe` nothing moves and the record is a shadow.

**What a redirect does not do.**

- **It does not touch the headers.** The client's headers — credentials
  included — go to the new host unchanged. A plugin that must not forward them
  removes or replaces them itself.
- **It does not gate your header writes.** Under `on_error: observe` nothing
  moves while header writes still apply, so a plugin that attaches credentials
  meant for the target must do so only when the request actually goes there:
  check `pctx.Redirected()` after the call, as above — it stays false under
  observe and on a refusal. Redirected is the only safe gate. pctx.Host names
  what the client asked for, and on a TLS-bridged request is the client's own
  Host header, which need not match the address the proxy dials. A plugin that
  attaches credentials meant for a server should always Redirect to that server,
  even when pctx.Host already names it, so the request is dialed there. Otherwise
  the target's key goes to the host the client named.
- **It does not re-run earlier plugins.** Plugins before the redirecting one in
  the chain made their decisions on the requested host.

`Redirect` is refused, with nothing changed, from a plugin that did not declare
the capability, outside `OnRequest`, for a malformed target, and on a context the
listener did not mark redirectable.

**Build-time rules.** At most one `WritesDestination` plugin per pipeline. And
`plugins.BuildWithDeps` refuses one for a listener that cannot honor it — on
startup and on reload — using the `pipeline.ListenerSupport` each listener
package supplies:

| Listener | Honors a redirect |
|---|---|
| forward proxy — the outbound chain in `cortex` | yes, unless `mtls:` is configured |
| reverse proxy — the inbound chain in `cortex` | no |
| ext_proc — both chains in `cortex-envoy` | no |

A capability some listener cannot honor is added the same way: a field on
`pipeline.ListenerSupport`, a case in its `Unsupported`, and each listener's
`Support` saying whether it can.

## Changing the model

A plugin that changes which model serves a request — a router, a downgrader, an
A/B test — declares `WritesRequestBody: true` and calls `pctx.SetRequestModel`
from `OnRequest`. Do not edit the body's `model` yourself, and do not write
`pctx.Extensions.Inference`: that record belongs to the parser.

```go
if name, ok := pctx.RequestModel(); ok && strings.Contains(name, "opus") {
    if err := pctx.SetRequestModel("glm-5.3"); err != nil {
        // A refusal is about the client's body, so it is the client's error: a 400.
        pctx.Record(pipeline.Invocation{Action: pipeline.ActionDeny, Reason: "model_rewrite_failed"})
        return pipeline.DenyStatus(http.StatusBadRequest, "inference.model-rewrite-failed", err.Error())
    }
}
```

| Call | Effect |
|---|---|
| `pctx.RequestModel()` | The body's top-level `model` string, exactly as `SetRequestModel` would replace it; `false` for a body it would refuse. |
| `pctx.SetRequestModel(name)` | Replaces that one value in place — every other byte stays as the client sent it, which a prompt cache depends on — and sends the result through `SetBody`. |

What it changes besides the bytes:

- **The inference record.** When the parser built `pctx.Extensions.Inference`,
  `model` becomes the new name and `requestedModel` keeps the one the client sent
  (`omitempty`, present only while the two differ). Settlement prices `model`, so
  the cost record and agentop's model column describe the model the request was
  sent for, and agentop's detail pane shows both. With no extension none is
  created.
- **The timeline.** The framework records `modify/model_rewritten` with `from`
  and `to` in `Details`, after the `modify/body_rewritten` every `SetBody` records.

Under `on_error: observe` nothing changes — not the body, not the record — and
both rows are shadows. A name equal to the current model changes nothing and
records nothing. `SetRequestModel` is refused, with nothing changed, from a plugin
that does not declare `WritesRequestBody`, outside `OnRequest`, and for a body
with no top-level string `model`, one whose `model` is empty, one that is not
valid JSON, or one that names `model` more than once, in any letter case. gjson and sjson read the
first key spelled exactly `model`, while encoding/json — the parser, and most
servers — reads the last key that matches it in any case, so `{"model": "a",
"Model": "b"}` is served as `b`, and changing one would leave the other serving.
The value is read only from the key spelled exactly `model`: a body whose only
match is `Model` names no model to change. An empty `model` is refused because
`requestedModel` could not record it as the client's name.

## Finishing requests (stateful plugins)

Plugins that **reserve per-request state** in `OnRequest` — rate-limiter
slots, distributed leases, audit "request started" events, in-flight
trace spans — need a guaranteed release point, regardless of whether
the request was allowed, denied by a later plugin, or errored at the
upstream. Releasing in `OnResponse` alone is a trap: `OnResponse` is
only walked for plugins on a `Continue` pipeline; a deny from a later
plugin leaves earlier plugins' state leaked.

The `Finisher` optional interface closes this:

```go
type Finisher interface {
    OnFinish(ctx context.Context, pctx *pipeline.Context)
}
```

`OnFinish` fires **once per request, after `OnResponse` if it ran**, on
every Finisher-implementing plugin whose `OnRequest` was actually
invoked — including the plugin that denied, if any. Stateless plugins
don't implement the interface and see nothing new; the framework skips
them.

> The full contract (dispatch order, context rules, error handling,
> the silent-phase invariants) lives in
> [`framework-architecture.md` §6 "Per-request finish hook"](./framework-architecture.md#per-request-finish-hook-finisher).
> This section is the plugin-author usage surface.

### Canonical shape: acquire in OnRequest, release in OnFinish

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
    if !ok { // OnRequest didn't reach the reservation; nothing to release
        return
    }
    p.slots.Release(s.tenant)
    p.metrics.RecordOutcome(pctx.Outcome().FinalAction)
}
```

`pipeline.SetState` / `GetState` is the typed cross-phase state API — it
keeps the state private to one plugin (unlike `Extensions.Custom`,
which is shared). The key convention is the plugin's `Name()`.

### Reading the outcome

`pctx.Outcome()` returns non-nil **only during OnFinish**:

```go
type Outcome struct {
    FinalAction   OutcomeAction // OutcomeAllow | OutcomeDeny | OutcomeError
    StatusCode    int            // final HTTP status written downstream
    DenyingPlugin string         // name of rejecting plugin, "" if allowed
    Duration      time.Duration  // wall-clock request duration
}
```

- `OutcomeAllow` — pipeline returned Continue end-to-end; response produced.
- `OutcomeDeny` — a plugin (request-side or response-side) denied. `DenyingPlugin` names it.
- `OutcomeError` — non-deny termination: upstream failure, framework panic, context cancellation. `DenyingPlugin` is empty.

Plugins that don't care about the outcome just ignore it — `OnFinish`
still fires and state still gets released.

### Rules you can't break

- **Don't call `pctx.SetBody` / `pctx.SetResponseBody` in OnFinish.** The response is already on the wire. Both calls are dropped with a WARN log.
- **Don't call `pctx.Record` or its `Allow` / `Skip` / `Observe` / `Modify` / `DenyAndRecord` siblings in OnFinish.** The SessionEvent is frozen once OnFinish starts. Recorded Invocations are dropped with a WARN log.
- **Don't spawn goroutines from OnFinish that outlive it.** Use `Initializer` / `Shutdowner` for process-lifetime background work. OnFinish-scoped goroutines leak under `context.WithTimeout`-based cleanup semantics.
- **Do use the `ctx` the framework provides** — it's a fresh `context.Background()`-derived ctx with a ~2s deadline, specifically so client disconnect during the request doesn't cancel your I/O. Don't reach for pctx-carried cancellation tokens.

### Publishing observability from OnFinish

OnFinish doesn't auto-emit Invocations. Plugins that want per-request
cleanup telemetry go to their own sinks:

- **Prometheus / OTEL metrics** — fire-and-forget counters and histograms. Don't need session-event plumbing.
- **External audit service** — POST the event over HTTP using the ctx the framework supplies.
- **`pctx.Extensions.Custom["my-plugin/event"]` (escape hatch)** — NO. The SessionEvent is already published once OnFinish runs; writes to Custom after that are visible to nothing.

If you find yourself wanting OnFinish-phase Invocations in session events, the design choice behind OnFinish being silent (explicit in the [versioning entry](./framework-architecture.md#12-versioning)) is worth re-reading before asking for a framework change.

## Exposing content to guardrails

Parser plugins whose extensions carry user-visible text (message bodies,
tool arguments, LLM completions) can opt into a shared content-inspection
contract by implementing [`capabilities.ContentSource`](../core/capabilities/content.go).
Guardrail plugins (PII scrubbers, jailbreak detectors, content classifiers,
prompt-injection filters, etc.) iterate the contract via
`pctx.ContentSources()` and never import any specific parser package.

This section is optional. A parser that doesn't implement `ContentSource`
still works for session bucketing and agentop rendering; it just isn't visible
to content-oriented guardrails.

### The contract

```go
type ContentSource interface {
    Fragments() []Fragment
}

type Fragment struct {
    Role string // "user" | "assistant" | "system" | "tool" | "tool_args" | "tool_result" | ...
    Text string // non-empty; producers filter empties
}
```

Constants for the standard role values live in the `capabilities` package
(`RoleUser`, `RoleAssistant`, `RoleSystem`, `RoleTool`, `RoleToolArgs`,
`RoleToolResult`). Parsers should use them when the semantic fit is clear.
The vocabulary is open — a protocol that carries a role outside this list
may emit its own string; guardrails that don't recognize it treat it per
their own policy.

### Role mapping across in-tree parsers

| Role | MCP source | A2A source | Inference source |
|---|---|---|---|
| `user` | — | user text parts | user messages |
| `assistant` | — | artifact | completion |
| `system` | — | — | system messages |
| `tool` | tools/call name | — | model's tool call name |
| `tool_args` | tools/call argument values | — | model's tool call arguments |
| `tool_result` | tools/call result text **and** JSON-RPC error messages | — | conversation's prior tool messages |

Empty cells are intentional. A2A has no system-prompt concept; MCP has no
user-message concept. Guardrails ignore roles they don't care about; no
fabricated content fills the gaps.

### Implementing Fragments

Keep each implementation to a pure function over the extension's fields.
Skip fragments with empty text — consumers never want zero-length entries.
Stringify non-string values with `json.Marshal` so nested maps/slices
become flat inspectable text. Reference implementations live alongside
the types in [`core/pipeline/content.go`](../core/pipeline/content.go).

### Consuming content in a guardrail

```go
import "github.com/rossoctl/cortex/core/capabilities"

func (p *JailbreakDetector) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
    for _, src := range pctx.ContentSources() {
        for _, f := range src.Fragments() {
            // Jailbreak attempts come through user input and tool_args.
            if f.Role != capabilities.RoleUser && f.Role != capabilities.RoleToolArgs {
                continue
            }
            if hit := p.classify(f.Text); hit.IsJailbreak {
                return pipeline.Deny("jailbreak.detected", hit.Category)
            }
        }
    }
    return pipeline.Action{Type: pipeline.Continue}
}
```

One import (`capabilities`), one iteration pattern, works across A2A +
MCP + Inference. A fourth protocol that implements `Fragments` is
picked up with no guardrail change.

### When NOT to implement ContentSource

`ContentSource` is for **content-oriented** checks: free-text scanning
that's the same logic regardless of protocol. It doesn't help
**structure-oriented** guardrails — e.g., "only the `url` parameter of
`fetch_url` must match the allowlist." Those guardrails are inherently
protocol-aware; they should import the parser's extension type and read
fields directly. The two patterns coexist cleanly.

Binary protocols, control-plane RPCs (MCP `initialize` / `ping`,
tools/list), and identity-only auth messages have no inspectable text —
simply don't implement the interface.

## Classifying requests as actions vs protocol mechanics

A second protocol-agnostic contract sits alongside `ContentSource`:
parser plugins set `IsAction bool` on their extensions to tell guardrails
"is this a user-meaningful action or just protocol mechanics?"
Guardrails read the aggregated verdict via
[`pctx.Classification()`](../core/pipeline/context.go) without
importing any specific parser package.

This is the mechanism that lets a defense-in-depth guardrail (IBAC, a
rate limiter, an audit logger) handle multi-protocol traffic uniformly:
each parser owns its own protocol's bypass-vs-action vocabulary; the
guardrail asks one question and gets one answer.

### The contract

```go
// On every protocol extension:
type MCPExtension struct {
    // ... existing fields ...
    IsAction bool  // set true for user-meaningful action methods
}
```

Default-false: zero-value (uninitialized) means "not classified as an
action," which guardrails treat as bypass. Parsers explicitly set
`IsAction = true` for the small set of methods that carry user intent
on the wire. This matches IBAC's defense-in-depth posture: when in
doubt, err toward letting traffic through, not toward judging it.

### Classification per in-tree parser

| Parser | `IsAction = true` for | Notes |
|---|---|---|
| `mcp-parser` | `tools/call`, `prompts/get`, `resources/read` | All other JSON-RPC methods (housekeeping, notifications, list ops, subscribe/unsubscribe) inherit default false. Synthetic transport extensions (`$transport/stream`, `$transport/terminate`) also stay at default false. |
| `a2a-parser` | `message/send`, `message/stream` | Discovery / protocol setup methods inherit default false. |
| `inference-parser` | every populated case | An LLM call is always an action on the wire; "don't judge inference by default" lives as IBAC operator policy, not in the classification. |

### Consuming the classification in a guardrail

```go
func (p *RateLimiter) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
    isAction, isBypass := pctx.Classification()
    if isBypass {
        // Some parser said "this is protocol mechanics" — don't count it.
        return pipeline.Action{Type: pipeline.Continue}
    }
    if !isAction {
        // No parser claimed this request — defense in depth, pass through.
        return pipeline.Action{Type: pipeline.Continue}
    }
    // Action — count it against the rate limit.
    return p.limit(pctx)
}
```

The shape matches IBAC's gate: skip on bypass, pass-through when
unclassified, act on action.

### When NOT to set IsAction=true

Most parser methods are not actions. The bar for marking a method
`IsAction = true` is "guardrails would correctly want to judge or
rate-limit or audit this on a per-call basis." Discovery, capability
listing, subscription management, notifications, transport
housekeeping — none of those qualify. Adding a method to the action
set is a security-relevant change because it pulls the method into
every guardrail's evaluation surface; review accordingly.

## Registering a plugin

A plugin advertises itself to the pipeline builder through `RegisterPlugin`
in its package `init()`. The registration is open — any package that
imports `core/plugins` can register a plugin, regardless of whether it
lives in this module. The pattern mirrors `database/sql` drivers and
`log/slog` handlers.

> For a step-by-step walkthrough (in-tree file layout, out-of-tree
> module + side-effect import, operator YAML wiring), see
> [`plugin-tutorial.md` Step 6](./plugin-tutorial.md#step-6--out-of-tree-plugins). This
> section is the field-level reference: the factory shape and the
> panic-on-misuse guarantees that define the registry's contract.

### Factory shape

```go
// core/plugins/jwtvalidation/plugin.go
func init() {
    RegisterPlugin("jwt-validation", func() pipeline.Plugin { return NewJWTValidation() })
}
```

The factory is called once per pipeline instance during `Build`. It must
return a fresh `pipeline.Plugin`; the registry does not cache the returned
value. Two pipeline entries with the same name produce two independent
plugin instances, each decoded from its own `config:` block.

### Rules and guardrails

- **Double-registration panics.** If two packages both register under the
  same name, the second call panics at process start. This is the
  correct behaviour: silent last-write-wins would let a version
  conflict poison the pipeline composition in ways that only surface as
  mysterious runtime behaviour.
- **Empty name panics.** An empty plugin name cannot be referenced from
  YAML; registering under one is a programmer bug, not a recoverable
  condition.
- **Nil factory panics.** A nil factory would defer the crash until
  `Build` tried to call it; panic at registration is closer to the bug.
- **Unknown plugin fails Build.** `Build` rejects entries whose name
  isn't in the registry; the error message includes every registered
  name so typos are easy to spot.

### Testing against the registry

Tests that need a fake plugin use `RegisterPlugin` + `t.Cleanup` with
`UnregisterPlugin`:

```go
func TestMyScenario(t *testing.T) {
    plugins.RegisterPlugin("fake-auth", func() pipeline.Plugin {
        return &fakeAuth{}
    })
    t.Cleanup(func() { plugins.UnregisterPlugin("fake-auth") })

    p, err := plugins.Build([]config.PluginEntry{{Name: "fake-auth"}})
    // ... assert on p ...
}
```

`UnregisterPlugin` is test-only by convention — production code should
never call it. It exists to keep tests isolated from each other under
`-parallel`.

## Plugins making outbound LLM calls

Plugins that need an LLM in the loop — policy judges, content scorers, intent matchers, audit categorizers — should use [`core/llmclient`](../core/llmclient/) rather than rolling their own HTTP / JSON / error-handling layer.

What `llmclient` gives you:

- An OpenAI-compatible chat-completions client (`Client.Call`, `Client.CallRaw`).
- Generic JSON-from-prose extraction (`ExtractJSON[T]`, `CallStructured[T]`) that handles models which wrap JSON in code fences or prose.
- A two-bucket error model that maps cleanly to `403` (LLM responded but unparseable — wraps `ErrUncertain`) vs `503` (LLM unreachable — does not wrap `ErrUncertain`).
- A reentrancy sentinel (`Options.SentinelHeaderName` + `Options.SentinelValue` from `NewSentinelValue`) for breaking loops when the LLM call would otherwise pass back through the plugin's own pipeline. Compare against the exact value — outbound headers are the agent's own request, so a caller who can set headers must not be able to switch the plugin off. The default client also strips the sentinel from cross-origin redirects (`SentinelRedirectPolicy`); a custom `Options.HTTPClient` replaces that policy and must install it itself.

Plugins keep ownership of:

- The system prompt (operator-overridable; default in the plugin source).
- The response schema (a Go struct passed as `T` to `CallStructured[T]`).
- Plugin-named error sentinels that wrap `llmclient.ErrUncertain` so `errors.Is` works at both layers:

  ```go
  var ErrJudgeUncertain = fmt.Errorf("%w: judge produced bad output", llmclient.ErrUncertain)
  ```

- The mapping from LLM output to a pipeline `Action` (e.g. IBAC normalizes `verdict: "allow"|"deny"` and treats anything else as `ActionDeny` with reason `ibac.judge_uncertain`).

The IBAC plugin (`core/plugins/ibac/judge.go`) is the in-tree reference; copy its shape when adding a new LLM-using plugin. For what IBAC actually does end-to-end (threat model, configuration, deny-reason vocabulary), see [`ibac-plugin.md`](ibac-plugin.md).

## Cross-references

- `core/pipeline/configurable.go` — the interface.
- `docs/framework-architecture.md` — how plugins compose and
  run; Configure's place in the lifecycle.
- `core/config/config.go` — `PluginEntry` YAML shape and
  parsing.
- `core/plugins/registry.go` — how Build calls Configure.
- `core/pipeline/extensions.go` — named categories
  (`MCP`, `A2A`, `Inference`, `Security`, `Delegation`, `Auth`) + `Custom` map + escape-hatch
  convention.
- `core/pipeline/session.go` — `SessionEvent` wire shape
  and the `SessionDenied` phase.
- `core/llmclient/` — chat-completions helper for
  plugins that call an LLM (see "Plugins making outbound LLM calls"
  above).
- `core/plugins/staticinject/` — the `static-inject`
  plugin (see [`static-inject-plugin.md`](static-inject-plugin.md));
  resolver + header-safety helpers are self-contained in this
  package.
