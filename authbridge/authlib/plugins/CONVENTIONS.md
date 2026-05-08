# Plugin Config Conventions

How plugins under `authbridge/authlib/plugins/` receive, validate, and
apply their configuration. Everything here is convention — the framework
only requires `pipeline.Configurable` if the plugin has any config at all.
The rest of this document exists so that the sixth and tenth plugin
don't each invent their own style.

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
```

- **`name`** — required. Must match a key in the plugin registry.
- **`id`** — optional. Defaults to `name`. Lets two instances of the same
  plugin coexist with different config (not yet exercised, but the shape
  is reserved).
- **`config`** — optional. Arbitrary YAML sub-tree owned by the plugin.
  The framework does not interpret it; it's captured as `json.RawMessage`
  and handed to `Configure`.

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
`authlib/config` is the reference pattern.

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
  `authlib/config` (not a new one), which tolerates transient absence
  during pod boot (client-registration may still be writing).

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

    "github.com/kagenti/kagenti-extensions/authbridge/authlib/pipeline"
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

Plugins can surface per-request state into `/v1/sessions` two ways.
Pick based on how many other plugins want to consume the same shape.

### Named category (typed slot)

`MCP`, `A2A`, `Inference`, `Auth` are named fields on
`pipeline.Extensions`. Plugins write a typed struct into the relevant
slot; the listener snapshots it onto `SessionEvent` on the wire.
Consumers (abctl, dashboards, stats) know the exact schema at compile
time.

Use a named category when:

- **Multiple plugins produce the same shape.** Auth is shared by
  `jwt-validation` (inbound) and `token-exchange` (outbound); a future
  `token-broker` drops into the same slot without schema churn.
- **abctl or dashboards need to render a dedicated column / panel.**
- **Stats counters partition on the data** — category fields are
  compile-checked, so a typo in a reason code fails the build.

Adding a new named category is a **core-library change**: edit
`pipeline/extensions.go` (new field), `pipeline/session.go` (wire + JSON
round-trip), the listener (snapshot helper + recorder inclusion), and
abctl if you want bespoke rendering.

### Escape-hatch map (`Custom` with `/event` suffix)

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
- **Payload schema is plugin-owned.** No central registry; abctl
  treats unknown keys as raw JSON in the detail pane.

### Graduation: when to promote map → named category

Graduate to a typed slot when ≥2 of these are true:

1. **Two or more plugins share the shape.** That's the signal the
   "category" concept is worth codifying — it prevents N plugins from
   each shipping their own near-identical struct.
2. **abctl or the session API grows conditional logic on the key.**
   If consumers already parse the payload, making the schema compile-
   checked is a net win.
3. **The data is populated on nearly every deployment.** Core
   semantics (auth, protocol) graduate; niche plugins stay in the map.

Don't graduate speculatively — the map path has no cost if you stay
in it.

## Cross-references

- `authbridge/authlib/pipeline/configurable.go` — the interface.
- `authbridge/authlib/pipeline/README.md` — how plugins compose and
  run; Configure's place in the lifecycle.
- `authbridge/authlib/config/config.go` — `PluginEntry` YAML shape and
  parsing.
- `authbridge/authlib/plugins/registry.go` — how Build calls Configure.
- `authbridge/authlib/pipeline/extensions.go` — named categories
  (`MCP`, `A2A`, `Inference`, `Auth`) + `Custom` map + escape-hatch
  convention.
- `authbridge/authlib/pipeline/session.go` — `SessionEvent` wire shape
  and the `SessionDenied` phase.
