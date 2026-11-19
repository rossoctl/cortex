# CPEX

> **Deployment:** where this plugin runs, its build and its configuration are on
> the operator page, [`docs/cpex-plugin.md`](../../../docs/cpex-plugin.md).

The CPEX plugin routes Cortex pipeline hooks through the [CPEX](https://github.com/contextforge-org/cpex) framework,
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
  redact a response field, call a guardrail, taint the session, in a defined order.
  A PDP's allow or deny becomes the trigger for these effects through `on_allow` / `on_deny`.

## How it works

Cortex parses traffic into a typed pipeline context ([inspired by CPEX's specification](https://github.com/rossoctl/cortex/commit/4c53164d02809ddb19f3b79b3abf9c288a8bc4fb)).
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
- **cpex** is where enforcement happens in the Cortex pipeline.

The plugin fires the CPEX hooks listed in its `hooks` block on each phase. Hooks
run in declaration order and the chain short-circuits on the first sub-plugin
that returns deny. An empty hook list makes the phase a no-op, so you can install
the plugin and ship a policy update later.

The build constraint is in [Build constraint](../../../docs/cpex-plugin.md#build-constraint).

## CPEX hooks

Cortex dispatches operator-selected subsets of these hooks on each phase. The
constants live in `hooks.go`.

| Hook | Fires | APL home |
|---|---|---|
| `cmf.tool_pre_invoke` | before an MCP tool call (or, with `synthetic_egress`, plain egress) is forwarded | tool-args validation, PDP checks, delegation |
| `cmf.tool_post_invoke` | after a tool call returns, before it reaches the agent | post-result checks, result redaction |
| `cmf.llm_input` | on an LLM request before it leaves the pod | prompt and tool-result checks, session taint |
| `cmf.llm_output` | on an LLM response before it reaches the agent | output checks |

CPEX also runs its own `identity.resolve` hook ahead of the entity hooks when
identity resolvers are wired. Cortex calls the fused resolve-then-invoke path
so resolved credentials reach delegation in CPEX memory.

## CPEX plugins

APL routes pull from a toolbox of named CPEX sub-plugins declared in the CPEX
YAML. Routes invoke them by name: `authentication: [jwt-user]`,
`run(<name>)`, `delegate(workday-oauth, ...)`. PDPs declared under
`global.pdp` are called with `cedar:`, `cel:` and `opa:` steps. CPEX 0.3
bundles JWT and API-key identity, the OAuth delegator, CIBA elicitation and
the Cedar, CEL and OPA PDPs; the 0.2.x `validator/pii-scan` and
`audit/logger` plugins are gone. See the HR demo for examples.

## CPEX APL

APL is a per-entity policy that orchestrates ABAC and ReBAC predicates, ordered effects, and
sequencing, over the common CMF vocabulary. A route binds a policy to one entity
(a tool, a prompt, a model, an A2A method). Modelled on the HR demo:

```yaml
plugins:
  - name: jwt-user                          # resolves the caller from X-User-Token
    kind: identity/jwt
    hooks: [identity.resolve]
    capabilities: [perform_http]            # needed to fetch the JWKS
    config:
      role: user
      header: X-User-Token
      claim_mapper: standard
      trusted_issuers:
        - issuer: "https://keycloak.example.com/realms/hr"
          audiences: ["cpex-gateway"]       # required since 0.3.0
          algorithms: ["RS256"]
          decoding_key:
            kind: jwks_url
            url: "https://keycloak.example.com/realms/hr/protocol/openid-connect/certs"

  - name: workday-oauth                     # RFC 8693 exchange for the Workday API
    kind: delegator/oauth
    hooks: [token.delegate]
    capabilities: [read_inbound_credentials, write_delegated_tokens, perform_http]
    config:
      token_endpoint: "https://keycloak.example.com/realms/hr/protocol/openid-connect/token"
      client_id: cpex-gateway
      client_secret_source: {kind: env_var, name: WORKDAY_CLIENT_SECRET}

global:
  pdp:
    - kind: cel

routes:
  - tool: get_compensation
    authentication: [jwt-user]
    authorization:
      pre_invocation:
        - "require(role.hr)"
        - "delegate(workday-oauth, target: workday-api, audience: workday-api, permissions: [read_compensation])"
        - "taint(secret, session)"          # session label persists across calls
    args:
      ssn: "str | redact(!perm.view_ssn)"   # redact request arg when caller lacks the perm
    result:
      ssn: "str | redact(!perm.view_ssn)"   # redact the response field too

  - tool: send_email
    authentication: [jwt-user]
    authorization:
      pre_invocation:
        - "require(perm.email_send)"
        - cel:
            expr: "!has(args.body) || !args.body.matches('[0-9]{3}-[0-9]{2}-[0-9]{4}')"
            on_deny: ["deny('SSN pattern in body', 'pii.detected')"]
        - "security.labels contains \"secret\": deny('external email blocked', 'session_tainted_secret')"
```

`get_compensation` taints the session with `secret`. The label persists in the
CPEX session store keyed by the subject and the session id, threaded from
`X-Session-Id` (or, with `session_source: resolved`, the session the forward
proxy resolved). A later `send_email` in the same session reads that label and
denies, blocking the exfiltration at the session level rather than with a
per-call filter. Unless `delegated_credentials` is on, the `delegate(...)`
step's token does not reach the host, so `get_compensation` is denied with
`cpex.delegation_unavailable` once the exchange succeeds (see [Decisions and
denials](../../../docs/cpex-plugin.md#decisions-and-denials)).

### External PDPs (BYOP)

APL steps call policy decision points and act on their verdict with the same
effect vocabulary. Cedar, CEL and OPA run inside CPEX (OPA as embedded Rego, no
sidecar); `authzen:` and `nemo:` steps need a resolver the host registers,
and Cortex registers none.

```yaml
global:
  pdp:
    - kind: opa
      modules:
        - |
          package hr.compensation

          default allow := false

          allow if input.args.employee_id == input.subject.id
    - kind: cedar-direct
      policy_text: |
        permit(principal, action == Action::"read", resource is CompensationRecord)
        when { principal.roles.contains("hr") };

routes:
  - tool: get_compensation
    authorization:
      pre_invocation:
        - opa:
            query: data.hr.compensation.allow
            on_deny:
              - "taint(compensation_violation, session)"
              - "deny('not your record', 'opa_denied')"
        - cedar:
            action: 'Action::"read"'
            resource:
              type: CompensationRecord
              id: ${args.employee_id}
            on_deny: ["deny('not permitted', 'cedar_denied')"]
```

Steps run in order, so cheap local checks go first; each PDP's decision feeds
its own `on_deny` / `on_allow` effect list.

> **OPA in two places.** Cortex ships a native `opa` pipeline plugin
> (standalone, no CPEX dependency) for simple per-request OPA checks.
> The CPEX `opa:` step runs OPA inside APL, so its verdict feeds
> `on_deny`/`on_allow` effects, session tainting, and ordered composition
> with other PDPs. Use the native plugin for a standalone OPA gate and the
> CPEX step when OPA is one decision among several.

## CMF / extension mapping

The Cortex pipeline context is mapped into the CMF Message plus
typed extensions that CPEX policies read.

| Cortex source | CPEX CMF / extension target | Notes |
|---|---|---|
| MCP `tools/call`, `prompts/get`, `resources/read`, result | `ContentPart` tool_call / prompt_request / resource_ref / tool_result | structured; rewritable on write-back |
| MCP tool / prompt / resource name | `MetaExtension.{EntityType, EntityName}` | drives per-route dispatch |
| Inference request messages | CMF text parts + `AgentExtension.Conversation.History` | drives `cmf.llm_input` |
| Inference request tool output (Anthropic `tool_result` blocks, OpenAI chat `tool` messages, Responses-API `function_call_output` items) | CMF tool_result parts + `Extensions.Custom["tool_results"]`, `["tool_result_count"]` | read-only; APL and CEL on `llm:` routes read the custom keys (see below) |
| Inference response completion and tool calls (Anthropic Messages, OpenAI chat, Responses API; JSON or SSE) | CMF text + tool_call parts + `Extensions.Custom["tool_calls"]`, `["tool_call_count"]` | read with inference-parser's normalizer, so a streamed call is the assembled one; deny only, except a non-streaming OpenAI chat completion's text |
| Inference model | `MetaExtension.{EntityType: llm, EntityName}` + `LLMExtension.{ModelID, Provider}` | `llm:` routes; provider derived from model id |
| Inference finish reason + token usage | `CompletionExtension.{StopReason, Tokens, Model}` | response phase |
| A2A text parts | CMF text parts | role from phase |
| A2A, MCP protocol methods (`tools/list`, `initialize`), unparsed traffic (with `synthetic_egress`, inbound only) | `MetaExtension.{EntityType: http, EntityName: *}` | CPEX's global entity; `fallback_identification: none` sends no meta, which CPEX denies as `unidentified_request` |
| A2A session / task / message id | `AgentExtension.{SessionID, ConversationID}` + `ProvenanceExtension.MessageID` | |
| Identity subject + scopes | `SecurityExtension.Subject.{ID, Roles}` | always mapped |
| Identity auth method + curated claims | `SecurityExtension.AuthMethod`, `Subject.Claims` | issuer / audience / exp, not the raw claim map |
| Agent workload identity | `SecurityExtension.ThisWorkload.{SpiffeID, TrustDomain, ClientID}` | the agent's own identity, distinct from the caller |
| Delegation chain | `DelegationExtension.{Chain, Depth, OriginSubjectID, ActorSubjectID}` | populated by token exchange |
| Request id / trace headers | `RequestExtension.{RequestID, TraceID, SpanID}` | from `X-Request-Id` / B3 / traceparent |
| Session id | `AgentExtension.SessionID` | from A2A natively or `X-Session-Id`; with `session_source: resolved`, the listener's session when an agent session header or the process chain named it |
| HTTP headers | `HttpExtension.RequestHeaders` | Authorization and Cookie stripped |
| `pctx.Extensions.Custom["cpex/<key>"]` | `Extensions.Custom["<key>"]` | only the `cpex/`-prefixed entries cross |

Tool-result content parts are not visible to APL or CEL on an `llm:` route, so
an inference request also carries its tool results in the custom extension:
`custom.tool_result_count` (always set) and `custom.tool_results`, an object
keyed by position (`"0"`, `"1"`, ...) because the attribute bag cannot hold a
list of objects. Each entry has `tool_use_id` (empty for OpenAI chat),
`content` (the first 8 KiB), `truncated` and `is_error`. They are policy input
only, never recorded, and no other plugin can supply them. Guard on the count,
since `custom.tool_results` is absent when there are none:

```yaml
global:
  pdp:
    - kind: cel
routes:
  - llm: "*"
    authorization:
      pre_invocation:
        - cel:
            expr: "custom.tool_result_count == 0 || !custom.tool_results.exists(k, custom.tool_results[k].content.matches('AKIA[0-9A-Z]{16}'))"
            on_deny: ["taint(secret, session)", "deny('secret in a tool result', 'secret_in_tool_result')"]
```

A response carries its tool calls the same way, for `cmf.llm_output`:
`custom.tool_call_count` and `custom.tool_calls`, each entry with `id`, `name`,
`arguments` (the JSON text the model sent, first 8 KiB) and `truncated`. APL's
`args.*` there sees only the first call's arguments. A response that cannot be
read as its dialect fails closed; one read cleanly with no parts (an error
body, an empty completion) does not.

```yaml
      post_invocation:
        - cel:
            expr: "!('secret' in security.labels) || custom.tool_call_count == 0 || !custom.tool_calls.exists(k, custom.tool_calls[k].arguments.matches(net))"
            net: "\\b(curl|wget|nc)\\b"
            on_deny: ["deny('tool call from a tainted session', 'tainted_tool_call')"]
```

A CEL step allows when its expression is true and denies when it is false, so
the request-side step taints the session and denies the inference request together; a
later `http_egress` route can then deny on `security.labels contains "secret"`.
To taint without blocking the request, list the code in the plugin's
`observe_codes` (`observe_codes: [secret_in_tool_result]`): the deny is
recorded as `observe`, the request is forwarded, and the label stays. Steps
after the denying one do not run, and a code another step shares would be
silenced there too, so the plugin refuses at Configure an observed code that is
not the code of exactly one `deny('reason', 'code')`, is not in a route's own
phase list, or is not the last entry of every list down to it (step, `on_deny`,
`when`/`do`).

Caller identity lives in `SecurityExtension.Subject`. Policies read it from
`security.subject`, not `agent.agent_id`. The `agent` slot models the agent's own
execution identity.

CMF extension slots are visible to all plugins (unrestricted) or require a
declared capability (capability-gated). `http`, `security`, `agent`, and
`delegation` are gated; a plugin must hold `read_headers`, `read_agent`,
`read_delegation`, and so on to see them. Sensitive headers are stripped at the
Cortex boundary before the slot is built, so a misconfigured grant or the
unauthenticated session API cannot leak cookies or api-keys.

## The HR demo pipeline

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

Identity resolution, policy, delegation, redaction, and session taint all run
inside cpex via its own sub-plugins, so the Cortex
`jwt-validation` and `token-exchange` plugins are not needed on this path.

The order rules, the plugin config and the requests that the plugin denies before
CPEX are in [Configuration](../../../docs/cpex-plugin.md#configuration) and
[Pipeline composition](../../../docs/cpex-plugin.md#pipeline-composition).
Plain egress, the Cortex-asserted principal and the Cortex-asserted workload are in
[Plain egress as a tool call](../../../docs/cpex-plugin.md#plain-egress-as-a-tool-call),
[Cortex-asserted principal](../../../docs/cpex-plugin.md#cortex-asserted-principal) and
[Cortex-asserted workload](../../../docs/cpex-plugin.md#cortex-asserted-workload).

The decisions, the denials and the delegated credentials are in
[Decisions and denials](../../../docs/cpex-plugin.md#decisions-and-denials).

## Building and testing

```sh
# Unit tests (no cgo) cover every CMF mapping and re-serializer via a fake manager
CGO_ENABLED=0 go -C core test ./plugins/cpex/...

# The same tests plus the real engine: fetch the pinned FFI, then link it
FFI=$(scripts/fetch-cpex-ffi.sh)
CGO_ENABLED=1 CGO_LDFLAGS="-L$FFI" go -C core test -tags cpex ./plugins/cpex/...

# Build the image: links libcpex_ffi.a, -tags cpex, CGO on
podman build -f cmd/cortex-cpex/Dockerfile \
  --build-arg GO_BUILD_TAGS="$(go -C scripts/profile-tags run . cpex)" \
  -t authbridge-cpex:latest .
```

The tagged run also loads every CPEX policy in this README and in
`docs/cpex-plugin.md` on the real engine. The pins live in `cmd/cortex-cpex/`:
`CPEX_FFI_VERSION` is the CPEX release tag, `CPEX_FFI_ABI` the FFI ABI integer,
and `CPEX_FFI_SHA256` the digest of each target's tarball. On macOS, set
`MACOSX_DEPLOYMENT_TARGET` to the SDK version (`xcrun --show-sdk-version`) if
the linker warns that objects target a newer macOS.

## See also

- `cortex-demos/hr-cpex` — end-to-end demo with Bob, Eve, and Alice personas
  covering the Workday flow, GitHub flow with Cedar, PII scanning, and session
  taint propagation.
