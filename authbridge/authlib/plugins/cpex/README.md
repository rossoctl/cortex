# CPEX

> **Deployment note:** `authbridge-cpex` is a **build variant** of the
> AuthBridge proxy-sidecar (`authbridge-proxy`), deployed *in place of*
> `authbridge-proxy` — not an additional sidecar. The operator selects
> the image when CPEX policy enforcement is needed for a workload.

The CPEX plugin routes AuthBridge pipeline hooks through the [CPEX](https://github.com/contextforge-org/cpex) framework,
so operators can define authorization flows inline and declaratively, and turn decisions into ordered effects (including invoking CPEX plugins).

## Why CPEX

PDPs like OPA and Cedar are designed for stateless policy decisions (allow/deny).
CPEX leverages PDPs and organizes policy decisions for agentic workloads.
It calls your PDPs at the level of agentic entities (tools, prompts, models, A2A methods),
composes their verdicts with custom policy written inline, and turns each decision into ordered effects.

That orchestration allows you to see the full scope of your authorization flow (not obfuscated in code, not hidden behind an API):

- **Cross-call session reasoning.** An agent reads compensation data, then sends
  an email. Each call is allowed on its own. CPEX carries session labels across
  calls, so a policy can see the sequence and block the exfiltration.
- **Pre and post-result enforcement.** A tool returns a bulk directory dump the gateway
  never saw before the call fired. The pre-invoke check ran before the result
  existed, so CPEX adds a post-result phase that evaluates the actual response.
- **Ordered effects beyond allow/deny.** A decision drives concrete actions:
  redact a response field, call a guardrail, write an audit record, in a defined order.
  A PDP's allow or deny becomes the trigger for these effects through `on_allow` / `on_deny`.

## How it works

AuthBridge parses traffic into a typed pipeline context ([inspired by CPEX's specification](https://github.com/kagenti/kagenti-extensions/commit/4c53164d02809ddb19f3b79b3abf9c288a8bc4fb)).
The cpex plugin projects that context into CMF (a normalized, typed policy context), invokes the
CPEX hooks the operator configured, applies any modifications CPEX returns
(redacted bodies, mutated headers, session labels), and maps the CPEX outcome
back into a pipeline action.

```text
request ─► token-exchange ─► parsers ─► cpex ─────────► upstream ─► response
                              (MCP,        │
                               inference,  │ context
                               A2A)        ▼
                                          CMF        what you evaluate
                                           │
                                           ▼
                                          APL        how you define policy
```

- **CMF** is what you evaluate: a typed view of identity, content, and metadata.
- **APL** is how you define policy: the DSL plus named CPEX plugins.
- **cpex** is where enforcement happens in the AuthBridge pipeline.

The plugin fires the CPEX hooks listed in its `hooks` block on each phase. Hooks
run in declaration order and the chain short-circuits on the first sub-plugin
that returns deny. An empty hook list makes the phase a no-op, so you can install
the plugin and ship a policy update later.

### Build tag

The CPEX backend uses cgo and links `libcpex_ffi.a`. Only the `authbridge-cpex`
binary compiles this package with `-tags cpex`; the `authbridge-proxy`,
`authbridge-envoy`, and `authbridge-lite` binaries do not import it. Configuring
the `cpex` plugin in any other binary fails at boot with a clear "build the
cpex binary" error rather than registering a silent no-op. The package itself
compiles tag-free for unit tests via a fake manager, so
`go test ./authlib/plugins/cpex/...` runs with `CGO_ENABLED=0`.

## CPEX hooks

AuthBridge dispatches operator-selected subsets of these hooks on each phase. The
constants live in `hooks.go`.

| Hook | Fires | APL home |
|---|---|---|
| `cmf.tool_pre_invoke` | before an MCP tool call is forwarded | tool-args validation, PDP checks, delegation |
| `cmf.tool_post_invoke` | after a tool call returns, before it reaches the agent | post-result checks, audit |
| `cmf.llm_input` | on an LLM request before it leaves the pod | prompt PII redaction |
| `cmf.llm_output` | on an LLM response before it reaches the agent | output redaction, audit |

CPEX also runs its own `identity.resolve` hook ahead of the entity hooks when
identity resolvers are wired. AuthBridge calls the fused resolve-then-invoke path
so resolved credentials reach delegation in CPEX memory.

## CPEX plugins

APL routes pull from a toolbox of named CPEX sub-plugins declared in the CPEX
YAML. Routes invoke them by name through APL steps: `plugin(pii-scan)`,
`delegate(workday-oauth, ...)`, `cedar: { ... }`. See the HR demo for examples.

## CPEX APL

APL is a per-entity policy that orchestrates ABAC and ReBAC predicates, ordered effects, and
sequencing, over the common CMF vocabulary. A route binds a policy to one entity
(a tool, a prompt, a model, an A2A method). From the HR demo:

```yaml
routes:
  - tool: get_compensation
    apl:
      policy:
        - "require(role.hr)"
        - "delegate(workday-oauth, target: workday-api, audience: workday-api, permissions: [read_compensation])"
        - "taint(secret, session)"          # session label persists across calls
        - "plugin(audit-log)"
      args:
        ssn: "str | redact(!perm.view_ssn)" # redact request arg when caller lacks the perm
      result:
        ssn: "str | redact(!perm.view_ssn)" # redact the response field too

  - tool: send_email
    apl:
      policy:
        - "require(perm.email_send)"
        - "plugin(pii-scan)"
        - "security.labels contains \"secret\": deny('external email blocked', 'session_tainted_secret')"
        - "plugin(audit-log)"
```

`get_compensation` taints the session with `secret`. The label persists in the
CPEX session store keyed by the session id, threaded from `X-Session-Id`. A later
`send_email` in the same session reads that label and denies, blocking the
exfiltration at the session level rather than with a per-call filter.

### External PDPs (BYOP)

APL steps can call out to external policy decision points and act on their
verdict with the same effect vocabulary:

```yaml
policy:
  - require(authenticated)
  - opa("http://opa:8181/v1/data/hr/compensation/deny"):
      on_deny:
        - deny
        - taint(compensation_violation, session)
        - plugin(audit-log)
  - cedar:
      action: "Jans::Action::Read"
      resource_type: "Jans::CompensationRecord"
      on_deny: [deny, plugin(compliance_alert)]
  - authzen("https://authz.corp.com/access/v1/evaluation"):
      on_deny: [deny]
```

Local structural checks run fast and first; external PDPs run when the local
checks pass; their decisions feed the same `on_deny` / `on_allow` effect lists.

> **OPA in two places.** AuthBridge ships a native `opa` pipeline plugin
> (standalone, no CPEX dependency) for simple per-request OPA checks.
> The CPEX `opa(...)` step shown above runs OPA as a CPEX sub-plugin —
> within APL's effect framework, so its verdict feeds `on_deny`/`on_allow`
> effects, session tainting, and ordered composition with other PDPs.
> Use the native `opa` plugin for standalone binary OPA gates; use
> CPEX's `opa(...)` when OPA participates in a multi-PDP orchestration
> flow. Note: the CPEX `opa(...)` and `authzen(...)` steps above are
> doc-level BYOP examples — the hr-cpex demo exercises only the embedded
> `cedar` PDP, not external OPA/AuthZEN.

## CMF / extension mapping

The AuthBridge pipeline context is mapped into the CMF Message plus
typed extensions that CPEX policies read.

| AuthBridge source | CPEX CMF / extension target | Notes |
|---|---|---|
| MCP `tools/call`, `prompts/get`, `resources/read`, result | `ContentPart` tool_call / prompt_request / resource_ref / tool_result | structured; rewritable on write-back |
| MCP tool / prompt / resource name | `MetaExtension.{EntityType, EntityName}` | drives per-route dispatch |
| Inference request messages | CMF text parts + `AgentExtension.Conversation.History` | drives `cmf.llm_input` |
| Inference model | `LLMExtension.{ModelID, Provider}` | provider derived from model id |
| Inference finish reason + token usage | `CompletionExtension.{StopReason, Tokens, Model}` | response phase |
| A2A text parts | CMF text parts | role from phase |
| A2A method | `MetaExtension.{EntityType: a2a_method, EntityName}` | drives per-route dispatch |
| A2A session / task / message id | `AgentExtension.{SessionID, ConversationID}` + `ProvenanceExtension.MessageID` | |
| Identity subject + scopes | `SecurityExtension.Subject.{ID, Roles}` | always mapped |
| Identity auth method + curated claims | `SecurityExtension.AuthMethod`, `Subject.Claims` | issuer / audience / exp, not the raw claim map |
| Agent workload identity | `SecurityExtension.Agent.{ClientID, WorkloadID, TrustDomain}` | the agent's own identity, distinct from the caller |
| Delegation chain | `DelegationExtension.{Chain, Depth, OriginSubjectID, ActorSubjectID}` | populated by token exchange |
| Request id / trace headers | `RequestExtension.{RequestID, TraceID, SpanID}` | from `X-Request-Id` / B3 / traceparent |
| Session id | `AgentExtension.SessionID` | from A2A natively or `X-Session-Id` |
| HTTP headers | `HttpExtension.RequestHeaders` | Authorization and Cookie stripped |
| `pctx.Extensions.Custom["cpex/<key>"]` | `Extensions.Custom["<key>"]` | only the `cpex/`-prefixed entries cross |

Caller identity lives in `SecurityExtension.Subject`. Policies read it from
`security.subject`, not `agent.agent_id`. The `agent` slot models the agent's own
execution identity.

CMF extension slots are visible to all plugins (unrestricted) or require a
declared capability (capability-gated). `http`, `security`, `agent`, and
`delegation` are gated; a plugin must hold `read_headers`, `read_agent`,
`read_delegation`, and so on to see them. Sensitive headers are stripped at the
AuthBridge boundary before the slot is built, so a misconfigured grant or the
unauthenticated session API cannot leak cookies or api-keys.

## Pipeline configuration

Place `cpex` after the protocol parsers and any content filters on the phase you
want to enforce. The parsers populate `pctx.Extensions`; cpex reads from there to
build CMF. The plugin declares `RequiresAny: [mcp-parser, inference-parser,
a2a-parser]`, so `pipeline.Build` fails fast if no parser feeds it.

The HR demo enforces on the agent's egress (the forward proxy), with inbound left
as passthrough:

```text
outbound:  mcp-parser ──► cpex ──► (token exchange happens as part of the policy via delegate)
```

```yaml
pipeline:
  inbound:
    plugins: []                 # passthrough; forward identity headers to the agent
  outbound:
    plugins:
      - name: mcp-parser        # populates pctx.Extensions.MCP
      - name: cpex
        config:
          hooks:
            on_request:  [cmf.tool_pre_invoke]
            on_response: [cmf.tool_post_invoke]
          config_file: /etc/cpex/cpex.yaml
          fail_open: false
```

Identity resolution, policy, delegation, redaction, PII scanning, and session
taint all run inside cpex via its own sub-plugins, so the AuthBridge-side
`jwt-validation` and `token-exchange` plugins are not needed on this path.

### Plugin config

| Field | Default | Purpose |
|---|---|---|
| `hooks.on_request` | empty | CPEX hooks fired during OnRequest, in order |
| `hooks.on_response` | empty | CPEX hooks fired during OnResponse, in order |
| `config` | empty | CPEX runtime YAML inline; mutually exclusive with `config_file` |
| `config_file` | empty | path to CPEX YAML; mounted from a ConfigMap in production |
| `fail_open` | `false` | on a CPEX internal error: deny with 502 (false) or continue (true) |
| `worker_threads` | `0` | CPEX tokio worker pool size; 0 lets CPEX pick |
| `bypass_paths` | health + `.well-known` | URL path globs that skip CPEX entirely |
| `bypass_hosts` | keycloak / SPIRE / observability | host globs that skip CPEX, outbound only |

A `cpex` plugin with no hooks, no config, and no config file is installed but
inert. Configure succeeds and both phases return continue. `config` and
`config_file` carry the CPEX YAML verbatim, exactly what CPEX's own docs
describe, with no AuthBridge-side reshaping. Unknown config keys are rejected at
boot.

`bypass_hosts` is honored on the outbound phase only. On the inbound
reverse-proxy phase the Host header is attacker-controlled and identity is
resolved inside the bypassed chain, so a spoofed Host must not skip CPEX.
`bypass_paths` applies on both phases. Neither list accepts `*` or empty; to
disable the plugin, remove it from the pipeline.

## Decisions and denials

The plugin maps the CPEX outcome onto AuthBridge's five-value invocation
vocabulary:

- **allow**: all sub-plugins continued, request proceeds untouched.
- **deny**: a sub-plugin returned a policy violation. Rejects with HTTP 403 and a
  `cpex.<code>` violation code (`cpex.pii_detected`,
  `cpex.session_tainted_secret`, and so on).
- **modify**: policy rewrote the body, headers, or labels. Changes are applied,
  the request continues.
- **observe**: audit-only outcome, recorded, request continues.

A CPEX policy `deny` is a normal outcome and is always honored. `fail_open` only
governs CPEX-internal failures (an FFI error, an unreachable backend, a
modification that cannot be applied). With `fail_open: false` those reject with
HTTP 502; with `true` they log and continue. The default is fail-closed because a
CPEX error usually means a misconfigured policy or an unreachable PDP, and
silently allowing traffic in that state is rarely intended.

## Building and testing

```sh
# Unit tests (no cgo) cover every CMF mapping and re-serializer via a fake manager
cd authbridge/authlib && CGO_ENABLED=0 go test ./plugins/cpex/...

# Build the real backend: links libcpex_ffi.a, -tags cpex, CGO on
cd authbridge && podman build -f cmd/authbridge-cpex/Dockerfile -t authbridge-cpex:latest .
```

The pinned CPEX FFI ABI version lives in `cmd/authbridge-cpex/CPEX_FFI_VERSION`.

## See also

- `authbridge/demos/hr-cpex` — end-to-end demo with Bob, Eve, and Alice personas
  covering the Workday flow, GitHub flow with Cedar, PII scanning, and session
  taint propagation.
