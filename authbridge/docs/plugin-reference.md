# Plugin Author Reference

**Audience:** plugin authors who already know the basics and need the
contract — field names, invariants, error behaviour, the rules that the
framework enforces at startup.

**See also:**
- [`plugin-tutorial.md`](./plugin-tutorial.md) — step-by-step tutorial for writing a new plugin.
- [`framework-architecture.md`](./framework-architecture.md) — how the pipeline
  composes plugins, the lifecycle, the Context / Extensions wire shape.

How plugins under `authbridge/authlib/plugins/` receive, validate, and
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
`expected_audiences` (comma-joined list of configured inbound audiences)
and `expected_audience_host` (waypoint per-request derived audience, may
be empty). Update saved queries and dashboards that filtered on the old
key.

### `jwt-validation`: `placeholder_mode` / `placeholder_ttl` (optional, off by default)

Two fields enable the **mint** half of [credential placeholder
swap](#credential-placeholder-swap) (read that section for the full
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

### `placeholder-resolve`: credential injection config

Resolves OpenShell credential placeholders (`openshell:resolve:env:<KEY>`) in the
**`Authorization`** header to their real values on the wire, fail-closed. Outbound
plugin; opt-in (inert unless listed in `pipeline.outbound`). Only `Authorization` is
rewritten — it is the only header any listener reconciles to the upstream wire.

```yaml
- name: placeholder-resolve
  config:
    prefix: "openshell:resolve:env:"     # default; the KEY follows this prefix
    source: gateway                      # REQUIRED: "gateway" or "secret_dir" (no default — fail-closed)
    gateway:                             # used when source: gateway (OpenShell sandbox sidecar)
      endpoint: "https://openshell.<ns>.svc:8080"    # required
      sandbox_id: "<OPENSHELL_SANDBOX_ID>"           # required; must match the minted JWT
      mtls_cert: "/tls/client/tls.crt"               # client cert (OPENSHELL_TLS_CERT); required for https
      mtls_key:  "/tls/client/tls.key"               # client key  (OPENSHELL_TLS_KEY);  required for https
      mtls_ca:   "/tls/ca/ca.crt"                    # gateway CA  (OPENSHELL_TLS_CA);   required for https
      sa_token_path: "/var/run/secrets/openshell/token"
      insecure: false                    # opt-in plaintext (non-loopback refused by default)
    # secret_dir: "/etc/authbridge-cred"  # used when source: secret_dir (file per KEY)
```

| Field | Default | Notes |
|-------|---------|-------|
| `prefix` | `openshell:resolve:env:` | Placeholder prefix; the KEY after it matches `[A-Za-z_][A-Za-z0-9_]*`. |
| `source` | — (**required**) | `gateway` or `secret_dir`. There is **no implicit fallback** — an unset/unknown source fails `Configure`, so the plugin never silently resolves from an ambient source. |
| `gateway.endpoint` | — (required for `gateway`) | Gateway gRPC endpoint; `https://` selects mTLS, `http://` plaintext (non-loopback refused unless `insecure`). |
| `gateway.sandbox_id` | — (required for `gateway`) | Must equal the id bound into the gateway-minted JWT. |
| `gateway.mtls_cert` / `mtls_key` / `mtls_ca` | — (required for `https://`) | Client cert, client key, and gateway CA — independent file paths, mirroring OpenShell's `OPENSHELL_TLS_CERT`/`KEY`/`CA` (the driver splits the CA from the client keypair, so they need not share a dir). |
| `gateway.sa_token_path` | `/var/run/secrets/openshell/token` | Projected SA token (audience `openshell-gateway`). |
| `gateway.insecure` | `false` | Permit plaintext gRPC to a **non-loopback** gateway. Plaintext sends the SA token + JWT in cleartext, so it is **refused by default** for non-loopback endpoints (loopback always allowed). |
| `secret_dir` | — (required for `secret_dir`) | Directory; each KEY is read from a path-contained `<secret_dir>/<KEY>` file. |

An unresolved placeholder, or a resolved value containing CR/LF/NUL, fails the
request closed (`401 auth.unauthorized`) — the placeholder is never forwarded **under
the default `enforce` policy**. Run placeholder-resolve under `enforce`: with
`on_error: observe` the deny is shadowed and the request is forwarded carrying the
literal placeholder. No real credential leaks (the deny precedes header injection, and
upstream rejects the bogus bearer), but fail-closed no longer holds. The
`gateway` source requires running as a sidecar in the sandbox pod (shared SA + the
`openshell.io/sandbox-id` annotation) and warms its cache in the background, so the
pipeline gates traffic on `Ready()` until the first fetch succeeds. (The shared
resolution + safe-header-injection primitives live in `authlib/credinject`.)

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
downstream plugins and the upstream see the original.

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

`PluginCapabilities` carries two fields that let a plugin express how it
depends on other plugins in the same chain. Both are checked at
`plugins.Build` time (startup and hot-reload), and misconfigurations
fail loud before serving traffic.

```go
type PluginCapabilities struct {
    ReadsBody   bool
    WritesBody  bool

    Requires    []string   // ALL must be present + earlier (hard)
    RequiresAny []string   // AT LEAST ONE must be present + run after it (hard)

    Description string
}
```

| Field | All present? | At least one? | Ordering enforced? |
|---|---|---|---|
| `Requires` | ✓ | — | ✓ |
| `RequiresAny` | — | ✓ | ✓ |

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
5-value vocabulary shared across all plugins. abctl renders one row
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
    // abctl renders as key=value rows in the detail pane.
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
discriminates within an Action value. abctl filters can match
either — `/skip` shows every skip action regardless of reason;
`/path_bypass` narrows to that specific skip flavour.

**What to put in Details:**

Suggested key conventions used by built-in plugins (operators
already know these; abctl filters match substring on both key and
value):

- Auth gates (jwt-validation): `expected_issuer`, `expected_audiences`
  (comma-joined configured inbound audiences), `expected_audience_host`
  (waypoint per-request derived audience, may be empty), `token_subject`,
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
| `time.Duration` | `strconv.FormatInt(d.Milliseconds(), 10)` + `_ms` suffix on the key | milliseconds integer is abctl-friendly |

If your field's elements might contain the delimiter, pick a different
delimiter and document it on the field rather than escape — consumers
doing `strings.Split` are simpler to write against an unambiguous
separator than against an escape convention.

**NEVER put raw tokens, signatures, or client credentials in
`Details`.** The session store has no auth on it; only safe-to-log data
belongs in Invocations.

### 2. Named protocol extension (optional, for parsers)

`MCP`, `A2A`, `Inference` are typed slots on `pipeline.Extensions`.
A parser that successfully extracts structured state populates the
matching slot AND emits an `Invocation` with `ActionObserve`. The
slot carries the parsed payload; the Invocation carries the
attribution.

Adding a new named extension is a core-library change: edit
`pipeline/extensions.go`, `pipeline/session.go` (wire + JSON round-
trip), the listener (snapshot + recorder), and abctl if you want
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

## Body mutation

Plugins that need to rewrite request or response bodies declare
`WritesBody: true` and call the `pctx.SetBody` / `pctx.SetResponseBody`
helpers. The framework propagates the rewrite to the wire, emits a
`modify`-action Invocation, and publishes a `body-mutation/event`
entry in `pctx.Extensions.Custom` with length delta + sha256
before/after (never the raw body).

> For the full lifecycle — per-listener wire behavior, content-encoding
> policy, ordering rules, body-size limits — see
> [`framework-architecture.md` §6, "Body mutation"](./framework-architecture.md#body-mutation).
> This section is the plugin-author field reference.

### Capability fields

```go
type PluginCapabilities struct {
    ReadsBody  bool  // plugin reads pctx.Body / pctx.ResponseBody
    WritesBody bool  // plugin may call pctx.SetBody / pctx.SetResponseBody
}
```

- `ReadsBody`: listener buffers the body; plugin sees bytes.
- `WritesBody`: implies `ReadsBody`. Listener propagates `pctx.SetBody`
  rewrites to the upstream (and `pctx.SetResponseBody` to the
  downstream client).

### Build-time validation (enforced by `pipeline.New`)

- At most **one** `WritesBody` plugin per pipeline. Two mutators in
  the same direction would produce ambiguous ordering; `New` rejects
  with an error naming both plugins.
- A `WritesBody` plugin cannot precede a `ReadsBody`-only plugin. The
  reader must see the original bytes.
- Waypoint mode (ext_authz listener) cannot propagate body mutations —
  the ext_authz API has no body-mutation field. Do not combine
  `WritesBody: true` plugins with `mode: waypoint`.

### Mutation helpers

| Call | Effect |
|---|---|
| `pctx.SetBody(newBytes)` | Replace request body; flip `BodyMutated()` flag |
| `pctx.SetResponseBody(newBytes)` | Replace response body; flip `ResponseBodyMutated()` flag |
| `pctx.BodyMutated()` / `ResponseBodyMutated()` | Read by the listener to decide whether to emit a wire mutation. Plugins normally don't need these. |

Direct assignment (`pctx.Body = newBytes`) still compiles but the
listener won't propagate it, no Invocation fires, and the mutation
event won't appear in the session stream. Always use `SetBody`.

**NEVER log the raw body content.** The framework's
`body-mutation/event` carries only length + sha256 on purpose — the
session store is unauthenticated. Plugin-private debug logs may
include body bytes at DEBUG level, but never publish them to the
session stream or Custom map.

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
contract by implementing [`contracts.ContentSource`](../authlib/contracts/content.go).
Guardrail plugins (PII scrubbers, jailbreak detectors, content classifiers,
prompt-injection filters, etc.) iterate the contract via
`pctx.ContentSources()` and never import any specific parser package.

This section is optional. A parser that doesn't implement `ContentSource`
still works for session bucketing and abctl rendering; it just isn't visible
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

Constants for the standard role values live in the `contracts` package
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
the types in [`authlib/pipeline/content.go`](../authlib/pipeline/content.go).

### Consuming content in a guardrail

```go
import "github.com/kagenti/kagenti-extensions/authbridge/authlib/contracts"

func (p *JailbreakDetector) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
    for _, src := range pctx.ContentSources() {
        for _, f := range src.Fragments() {
            // Jailbreak attempts come through user input and tool_args.
            if f.Role != contracts.RoleUser && f.Role != contracts.RoleToolArgs {
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

One import (`contracts`), one iteration pattern, works across A2A +
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
[`pctx.Classification()`](../authlib/pipeline/context.go) without
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
imports `authlib/plugins` can register a plugin, regardless of whether it
lives in this module. The pattern mirrors `database/sql` drivers and
`log/slog` handlers.

> For a step-by-step walkthrough (in-tree file layout, out-of-tree
> module + side-effect import, operator YAML wiring), see
> [`plugin-tutorial.md` Step 6](./plugin-tutorial.md#step-6--out-of-tree-plugins). This
> section is the field-level reference: the factory shape and the
> panic-on-misuse guarantees that define the registry's contract.

### Factory shape

```go
// authbridge/authlib/plugins/jwtvalidation.go
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

Plugins that need an LLM in the loop — policy judges, content scorers, intent matchers, audit categorizers — should use [`authlib/llmclient`](../authlib/llmclient/) rather than rolling their own HTTP / JSON / error-handling layer.

What `llmclient` gives you:

- An OpenAI-compatible chat-completions client (`Client.Call`, `Client.CallRaw`).
- Generic JSON-from-prose extraction (`ExtractJSON[T]`, `CallStructured[T]`) that handles models which wrap JSON in code fences or prose.
- A two-bucket error model that maps cleanly to `403` (LLM responded but unparseable — wraps `ErrUncertain`) vs `503` (LLM unreachable — does not wrap `ErrUncertain`).
- A reentrancy sentinel (`Options.SentinelHeaderName`) for breaking loops when the LLM call would otherwise pass back through the plugin's own pipeline.

Plugins keep ownership of:

- The system prompt (operator-overridable; default in the plugin source).
- The response schema (a Go struct passed as `T` to `CallStructured[T]`).
- Plugin-named error sentinels that wrap `llmclient.ErrUncertain` so `errors.Is` works at both layers:

  ```go
  var ErrJudgeUncertain = fmt.Errorf("%w: judge produced bad output", llmclient.ErrUncertain)
  ```

- The mapping from LLM output to a pipeline `Action` (e.g. IBAC normalizes `verdict: "allow"|"deny"` and treats anything else as `ActionDeny` with reason `ibac.judge_uncertain`).

The IBAC plugin (`authlib/plugins/ibac/judge.go`) is the in-tree reference; copy its shape when adding a new LLM-using plugin. For what IBAC actually does end-to-end (threat model, configuration, deny-reason vocabulary), see [`ibac-plugin.md`](ibac-plugin.md).

## Credential placeholder swap

An opt-in mode that keeps the **real** user token out of the agent. With
it on, `jwt-validation` validates the inbound token and then replaces it
with an opaque `abph_` placeholder before forwarding to the agent; the
real token is held in a process-scoped shared store. On the way out,
`token-exchange` resolves the placeholder back to the real token (on a
matched route) before its normal RFC 8693 exchange. The agent thus holds
only an opaque handle, never a usable credential.

For the design rationale, data flow, and branch table, see
[`docs/superpowers/specs/2026-06-02-credential-placeholder-swap-design.md`](./superpowers/specs/2026-06-02-credential-placeholder-swap-design.md).

### The two flags are a matched pair

| Flag | Plugin | Chain | Role |
|---|---|---|---|
| `placeholder_mode` | `jwt-validation` | inbound | **mint** — swap real token → `abph_` handle |
| `resolve_placeholders` | `token-exchange` | outbound | **resolve** — swap `abph_` handle → real token, then exchange |

Both must be on to be coherent:

- **Mint on, resolve off** → the outbound side sees an `abph_` subject it
  can't use and **fails closed (deny)**. Safe and visible, but broken.
- **Resolve on, mint off** → no `abph_` tokens are ever produced, so
  resolve is a **no-op**; normal exchange is unaffected.

The pairing can't be expressed via `Requires` (that checks plugin
*name*, not config *state*), so v1 relies on the fail-closed deny plus
this documentation rather than build-time cross-validation.

### `token-exchange`: `resolve_placeholders`

Bool, default `false`. When the outbound bearer carries the `abph_`
prefix, resolve it from the shared store to the real token before the
normal exchange. An unresolvable placeholder (unknown or expired —
e.g. after a sidecar restart) is **denied, fail-closed**; the opaque
string is never sent to an upstream. A non-placeholder bearer skips
resolve and runs the normal exchange (backward compatible). Resolve is
gated by the same route match as the exchange — the handle is never
resolved before a route is confirmed, so a leaked handle can't pull the
real token into a header bound for an unmatched host.

### Passthrough hosts receive the placeholder, not the real token

With mint on, the agent never holds the real token, so any non-exchange
(passthrough) egress forwards the opaque `abph_` handle as-is. It is
useless off-box. Any host that needs a real credential MUST be
configured as a `token-exchange` route — passthrough egress cannot
produce one.

### The store is in-memory and process-scoped

The handle→token store lives in memory and is shared only within a
single process. The mode therefore works only where inbound mint and
outbound resolve run in the **same** process:

- the reverse+forward proxy sidecar (`authbridge-proxy`, and its
  `authbridge-lite` image variant);
- a **single-replica** extproc/extauthz (`authbridge-envoy`).

Multi-replica (HA) or shared/scaled Istio ambient waypoint deployments
can land mint and resolve on different processes, which the in-memory
store cannot bridge. Those need an external store behind the same
interface — a **current limitation**, tracked as a future enhancement.

### Security

- The handle is a random `abph_`-prefixed token (CSPRNG ≥256-bit),
  meaningless outside the minting process's store.
- The real token never reaches the agent and never persists to disk
  (in-memory store only).
- Neither the real token nor the handle is logged in cleartext —
  records carry a hash or the prefix only (see the "NEVER put raw
  tokens" rules under [Emitting session events](#emitting-session-events)).

## Cross-references

- `authbridge/authlib/pipeline/configurable.go` — the interface.
- `authbridge/docs/framework-architecture.md` — how plugins compose and
  run; Configure's place in the lifecycle.
- `authbridge/authlib/config/config.go` — `PluginEntry` YAML shape and
  parsing.
- `authbridge/authlib/plugins/registry.go` — how Build calls Configure.
- `authbridge/authlib/pipeline/extensions.go` — named categories
  (`MCP`, `A2A`, `Inference`, `Auth`) + `Custom` map + escape-hatch
  convention.
- `authbridge/authlib/pipeline/session.go` — `SessionEvent` wire shape
  and the `SessionDenied` phase.
- `authbridge/authlib/llmclient/` — chat-completions helper for
  plugins that call an LLM (see "Plugins making outbound LLM calls"
  above).
