# cpex plugin

> **`authbridge-cpex` is a build variant** of the AuthBridge proxy-sidecar
> (`authbridge-proxy`), deployed *in place of* `authbridge-proxy` when
> CPEX policy enforcement is needed — not an additional sidecar.

The `cpex` plugin embeds the [CPEX](https://github.com/contextforge-org/cpex)
runtime inside AuthBridge so operators can drive policy with CPEX's
APL (Attribute Policy Language) DSL — and any of the pre-built CPEX
sub-plugins (Cedar PDP, PII scanner, audit logger, JWT identity,
OAuth delegation) — without writing Go.

A single AuthBridge plugin instance (`cpex`) wraps an entire CPEX
`PluginManager`. Operators declare which CPEX hook names fire on each
AuthBridge phase via the `hooks` block; CPEX's own YAML defines the
sub-plugins those hooks dispatch to.

```text
┌──────────────────────────────────────────────────────────────────┐
│ authbridge-cpex (binary built with -tags cpex, links libcpex_ffi)│
│                                                                  │
│  pipeline:                                                       │
│   ┌─────────────┐  ┌─────────────┐  ┌─────────────────────────┐ │
│   │ jwt-validation│ │ mcp-parser  │→│ cpex (this plugin)      │ │
│   └─────────────┘  └─────────────┘  │                         │ │
│                                     │  on_request:            │ │
│                                     │    cmf.tool_pre_invoke ─┼─┼──► CPEX FFI
│                                     │  on_response:           │ │
│                                     │    cmf.tool_post_invoke ┼─┼──► CPEX FFI
│                                     └─────────────────────────┘ │
└──────────────────────────────────────────────────────────────────┘
                                                       │
                                                       ▼
                                       ┌──────────────────────────┐
                                       │ CPEX runtime (in-process │
                                       │ via cgo + libcpex_ffi.a) │
                                       │                          │
                                       │  ┌─────────┐ ┌─────────┐ │
                                       │  │  APL    │ │ Cedar   │ │
                                       │  │  rules  │ │  PDP    │ │
                                       │  └─────────┘ └─────────┘ │
                                       │  ┌─────────┐ ┌─────────┐ │
                                       │  │ PII scan│ │ Audit   │ │
                                       │  └─────────┘ │ logger  │ │
                                       │              └─────────┘ │
                                       └──────────────────────────┘
```

## When to use

The cpex plugin is the right home for policy that:

- **Combines multiple decision shapes**. APL's flat predicates
  (`require(role.hr)`) compose naturally with Cedar's principal-×-resource
  rules and IdP-side delegation; expressing that in a single AuthBridge
  Go plugin would mean reimplementing each engine.
- **Needs to be edited by non-developers**. The APL DSL + Cedar
  policies live in operator YAML; updates ship via ConfigMap reload
  rather than a plugin re-deploy.
- **Composes pre-built CPEX sub-plugins** — PII scanners, audit
  loggers, OAuth delegators — instead of writing those from scratch in
  Go.

It is **not** the right home for:

- Plugins whose only job is one stable Go decision (`jwt-validation`,
  `token-exchange`) — those stay as AuthBridge-native plugins.
- Policy that needs to run *before* any parser has classified the
  body. cpex requires at least one of `mcp-parser` /
  `inference-parser` / `a2a-parser` upstream in the chain.

## Architecture

### Build constraint

The cpex plugin links `libcpex_ffi.a` via cgo. The `authbridge-cpex`
binary (`cmd/authbridge-cpex/`) is the only build target that compiles
the plugin — built with `-tags cpex` and `CGO_ENABLED=1`. Other
binaries (`authbridge-proxy`, `authbridge-envoy`, `authbridge-lite`)
stay pure-Go and never import the plugin.

The plugin's chassis (config decode, dispatch, Invocation recording)
is tag-free; only the cgo adapter that talks to `cpex.PluginManager`
is `//go:build cpex`. Unit tests run with `CGO_ENABLED=0` against a
fake Manager.

### Hook chains

Each AuthBridge phase (`OnRequest`, `OnResponse`) dispatches an
ordered list of CPEX hook names. Hooks fire one at a time; the chain
short-circuits on the first sub-plugin that returns deny. A sub-plugin
that returns `modify` records the Invocation, applies header/label
changes to pctx, and lets the chain continue. CPEX's standard hook
names:

| Hook | Fires for | Typical sub-plugins |
|---|---|---|
| `cmf.tool_pre_invoke` | MCP tool call about to be forwarded | Cedar PDP, PII scanner, validators |
| `cmf.tool_post_invoke` | MCP tool result returned | Audit logger, post-call delegators |
| `cmf.llm_input` | LLM inference request | Prompt-PII redactors |
| `cmf.llm_output` | LLM inference response | Output redactors, audit |

### Decision vocabulary

CPEX sub-plugin verdicts map onto AuthBridge's 5-value
`InvocationAction`:

| CPEX | AuthBridge | Chain effect |
|---|---|---|
| Allow | `allow` | Continue to next hook |
| Deny  | `deny`  | **Stop chain**, return `pipeline.Reject` |
| Modify | `modify` | Continue to next hook (mutations applied to pctx) |
| Observe | `observe` | Continue (diagnostics, no flow change) |

A CPEX-internal failure (FFI panic, runtime error) is distinct from a
policy `deny` — it surfaces as `cpex.error` and respects the
`fail_open` flag (see Configuration).

## Configuration

```yaml
plugins:
  - name: cpex
    config:
      # Required to do anything useful: at least one hook per phase.
      # An empty hooks block is valid; the phase becomes a no-op.
      hooks:
        on_request:
          - cmf.tool_pre_invoke
        on_response:
          - cmf.tool_post_invoke

      # CPEX-side APL config (identity, pdp, delegator). Passed
      # verbatim to CPEX LoadConfig.
      apl:
        identity:
          jwt:
            issuer: "https://keycloak.example.com/realms/prod"
            jwks_url: "http://keycloak/realms/prod/protocol/openid-connect/certs"

        pdp:
          cedar:
            policies: |
              @id("hr-can-read-comp")
              permit(
                principal,
                action == Action::"read",
                resource is CompensationRecord
              ) when { principal.roles.contains("hr") };

      # CPEX pipelines keyed by hook name. Passed verbatim to CPEX.
      pipelines:
        cmf.tool_pre_invoke:
          - pdp/cedar-direct
          - validator/pii-scan
        cmf.tool_post_invoke:
          - audit/logger

      # Behavior on CPEX-internal errors (NOT on policy deny):
      #   false (default) — request denied with cpex.error
      #   true            — request continues, error logged
      fail_open: false

      # CPEX tokio worker pool size. 0 = automatic (CPU count).
      worker_threads: 2

      # Hosts that skip CPEX entirely (default: keycloak/SPIRE/observability).
      bypass_hosts: ["keycloak.*", "spire-agent.*"]

      # URL paths that skip CPEX (default: /healthz, /readyz, /livez, /.well-known/*).
      bypass_paths: ["/healthz", "/readyz"]
```

### Pipeline composition

cpex declares `RequiresAny: [mcp-parser, inference-parser, a2a-parser]`.
At least one must appear earlier in the chain so the parser has populated
`pctx.Extensions.MCP` / `.Inference` / `.A2A` before cpex extracts CMF
content. `Pipeline.Build` rejects misordered chains at boot.

cpex also declares `ReadsBody: true, WritesBody: true`. Only one
`WritesBody` plugin is permitted per direction; chaining cpex with
another mutator (e.g. an inline transformer) will fail at boot.

A typical inbound chain:

```yaml
plugins:
  - name: jwt-validation       # identity gate
  - name: mcp-parser           # populate pctx.Extensions.MCP
  - name: cpex                 # APL/Cedar/PII via CPEX
```

## Status codes & reasons

| Code | HTTP | Meaning |
|---|---|---|
| `cpex.error` | 502 | CPEX FFI returned an error and `fail_open: false`. Body carries the underlying error message. |
| `cpex.denied` | 403 | A CPEX sub-plugin returned deny without a violation code. |
| `cpex.<sanitized>` | 403 | A CPEX sub-plugin returned deny with a violation code; the `<sanitized>` part lower-cases letters, replaces other characters with underscores, e.g. `pii.detected` → `cpex.pii_detected`. |

The `Invocation.Reason` field carries the CPEX-side reason string for
all of the above so operator dashboards can render the original
sub-plugin message alongside the namespaced code.

**HTTP status.** A policy `deny` is an authorization decision and
renders as **403 Forbidden**; a CPEX-internal error (fail-closed)
renders as **502 Bad Gateway** (the policy engine itself failed — an
upstream fault, not a client error). The plugin sets both statuses
explicitly: the namespaced `cpex.*` codes are dynamic and never appear
in the listener's static `codeToStatus` table, so without an explicit
status `Violation.Render` would default every CPEX outcome to 500.

## Bypass list curation

`bypass_hosts` and `bypass_paths` short-circuit *before* the FFI call,
so traffic to infrastructure that shouldn't see CPEX policy (Keycloak,
SPIRE, observability) skips the cgo round-trip entirely.

Both lists are operator-extensible glob patterns (`path.Match` syntax).
Default `bypass_hosts` covers `keycloak.*`, `spire-server.*`,
`spire-agent.*`, `otel-collector.*`, `jaeger.*`, `prometheus.*`,
matching the IBAC plugin's default set. Default `bypass_paths` covers
`/healthz`, `/readyz`, `/livez`, `/.well-known/*` via
`authlib/bypass.DefaultPatterns` — the same set jwt-validation uses.

Patterns that match everything (`"*"`, `""`, `"/*"`) are rejected at
Configure time; that gesture is better expressed by removing the cpex
plugin from the pipeline.

## fail_open: when to use it

`fail_open: false` (default) treats a CPEX-internal failure as a
deny. This is the right choice when CPEX is the only enforcement layer
for the traffic class — silently allowing requests when the policy
engine is broken is rarely what operators want.

`fail_open: true` allows requests through when CPEX errors, logging
the failure. Use this only when:

- Another enforcement layer (Envoy ext_authz, an upstream gateway)
  will gate the same traffic, and cpex is observation-only; OR
- The cost of brief unavailability outweighs the cost of brief
  unenforced traffic during a known-recoverable CPEX issue.

The `Invocation.Reason` field captures the underlying error in both
modes so the failure is auditable.

## Custom extension passthrough

Operators upstream of cpex can stash policy-input blobs in
`pctx.Extensions.Custom` under the `cpex/` prefix; cpex forwards them
into the CPEX `Extensions.Custom` map with the `cpex/` prefix
stripped. CPEX sub-plugins read them at the bare key.

```go
// In an upstream plugin's OnRequest:
pctx.Extensions.Custom["cpex/tenant_tier"] = "enterprise"

// On the CPEX side, an APL predicate sees it as:
//   custom.tenant_tier == "enterprise"
```

Entries without the `cpex/` prefix stay private to the plugin that
set them (rate-limiter cookies, plugin-internal state) and never
cross the FFI boundary.

## Sensitive headers

A fixed allowlist of sensitive header prefixes and exact names is
stripped from the CMF payload before crossing the FFI boundary —
CPEX sub-plugins (notably `audit/logger`) frequently log the payload
they receive, and the AuthBridge session API has no auth on it.

Stripped prefixes: `authorization`, `cookie`, `set-cookie`,
`proxy-authorization`, `x-amz-security-token`.

Stripped exact names: `x-api-key`, `x-auth-token`, `x-authorization`,
`x-secret-token`, `x-session-token`, `x-csrf-token`,
`x-platform-secret`, `x-authbridge-secret`.

This list is conservative; adding a header here is safer than
discovering one in an audit log.

## Background tasks

CPEX sub-plugins that emit asynchronous work (the canonical case is
`audit/logger` writing to an external sink) return their results
through `BackgroundTasks`. The cpex plugin spawns a goroutine per
Invoke that awaits the background work and routes per-sub-plugin
errors through the default `slog` handler with the request ID.

Operators pipe `slog` output to their observability stack. There is no
configurable audit sink today; future work may add one if operators
need to route background results separately from foreground logs.

## Limitations

- **Body modifications aren't yet re-serialized.** When a CPEX
  sub-plugin returns `modify` with a body payload change (the PII
  scanner's redaction path is the canonical case), the cpex plugin
  applies header and label changes to pctx but logs a warning and
  leaves `pctx.Body` unchanged. Format-aware re-serialization
  (CMF → JSON-RPC / OpenAI) is on the roadmap.
- **Per-sub-plugin Invocations** require the CPEX FFI to expose
  per-sub-plugin outcomes; today CPEX returns an aggregate
  `PipelineResult` with a single `Violation`. The cpex plugin emits
  one aggregate Invocation per Invoke as a result. A CPEX-side
  change to expose `PipelineResult.SubPlugins[]` will let the
  AuthBridge plugin emit one Invocation per sub-plugin.
- **A2A traffic** is not auto-classified into a CPEX hook. Operators
  configure a chain explicitly via `hooks.on_request: [<some hook>]`
  even for A2A; CPEX's own routing handles per-sub-plugin filtering.

## Failure modes (detailed)

| Symptom | Cause | Fix |
|---|---|---|
| Pipeline build fails at boot: `cpex plugin: this binary was not built with -tags cpex` | Operator pointed `authbridge-proxy` (no cgo) at a config containing a `cpex` plugin. | Use the `authbridge-cpex` image instead; or rebuild with `-tags cpex` against a downloaded `libcpex_ffi.a`. |
| Pipeline build fails: `cpex bypass_paths: invalid bypass pattern` | An entry in `bypass_paths` has bad `path.Match` syntax. | Fix the glob pattern (`/api/*/v1`, `/static/**` etc.). |
| Pipeline build fails: `cpex config: pattern "*" matches everything` | Operator wrote a wildcard pattern in `bypass_hosts` or `bypass_paths`. | If you want to disable cpex, remove it from the pipeline; don't bypass everything. |
| All traffic returns 502 with `cpex.error`, JWKS errors in logs | CPEX's APL identity plugin can't reach Keycloak. | Check the `apl.identity.jwt.jwks_url` is reachable from inside the pod; check the cpex bypass list doesn't include Keycloak (it should). |
| Audit logger silently produces no records | `BackgroundTasks` goroutine started but the audit sink is unreachable. | Search the slog stream for `cpex: background sub-plugin error` — the request ID and elapsed time correlate the failure with the foreground request. |

## See also

- `cmd/authbridge-cpex/README.md` — binary build + deployment.
- `cmd/authbridge-cpex/CPEX_FFI_VERSION` — pinned CPEX FFI ABI version
  the binary was built against.
- [CPEX repository](https://github.com/contextforge-org/cpex) — APL
  DSL, sub-plugin reference, FFI ABI.
- `demos/hr-cpex/` — runnable demo configurations.
