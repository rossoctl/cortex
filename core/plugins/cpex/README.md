# CPEX

> **Deployment note:** the `authbridge-cpex` image is a **build variant** of
> the AuthBridge proxy-sidecar — `cortex` built with cgo and the `cpex` tag —
> deployed *in place of* the `authbridge` image, not as an additional sidecar.
> The operator selects the image when CPEX policy enforcement is needed for a
> workload. `make cortex-cpex` builds the same variant for a laptop.

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
  redact a response field, call a guardrail, taint the session, in a defined order.
  A PDP's allow or deny becomes the trigger for these effects through `on_allow` / `on_deny`.

## How it works

AuthBridge parses traffic into a typed pipeline context ([inspired by CPEX's specification](https://github.com/rossoctl/cortex/commit/4c53164d02809ddb19f3b79b3abf9c288a8bc4fb)).
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

The CPEX backend uses cgo and links `libcpex_ffi.a`. Only `cortex` built with
`-tags cpex` and an `include_plugin_cpex` profile (`cpex` for the
`authbridge-cpex` image, `fullcpex` for `make cortex-cpex`) links it; other
builds do not register the plugin, so a config naming it fails at boot with an
unknown-plugin error rather than running a silent no-op. The package itself
compiles tag-free for unit tests via a fake manager, so
`go test ./core/plugins/cpex/...` runs with `CGO_ENABLED=0`.

## CPEX hooks

AuthBridge dispatches operator-selected subsets of these hooks on each phase. The
constants live in `hooks.go`.

| Hook | Fires | APL home |
|---|---|---|
| `cmf.tool_pre_invoke` | before an MCP tool call (or, with `synthetic_egress`, plain egress) is forwarded | tool-args validation, PDP checks, delegation |
| `cmf.tool_post_invoke` | after a tool call returns, before it reaches the agent | post-result checks, result redaction |
| `cmf.llm_input` | on an LLM request before it leaves the pod | prompt and tool-result checks, session taint |
| `cmf.llm_output` | on an LLM response before it reaches the agent | output checks |

CPEX also runs its own `identity.resolve` hook ahead of the entity hooks when
identity resolvers are wired. AuthBridge calls the fused resolve-then-invoke path
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
denials](#decisions-and-denials)).

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

> **OPA in two places.** AuthBridge ships a native `opa` pipeline plugin
> (standalone, no CPEX dependency) for simple per-request OPA checks.
> The CPEX `opa:` step runs OPA inside APL, so its verdict feeds
> `on_deny`/`on_allow` effects, session tainting, and ordered composition
> with other PDPs. Use the native plugin for a standalone OPA gate and the
> CPEX step when OPA is one decision among several.

## CMF / extension mapping

The AuthBridge pipeline context is mapped into the CMF Message plus
typed extensions that CPEX policies read.

| AuthBridge source | CPEX CMF / extension target | Notes |
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

Identity resolution, policy, delegation, redaction, and session taint all run
inside cpex via its own sub-plugins, so the AuthBridge-side
`jwt-validation` and `token-exchange` plugins are not needed on this path.

### Plugin config

| Field | Default | Purpose |
|---|---|---|
| `hooks.on_request` | empty | CPEX hooks fired during OnRequest, in order |
| `hooks.on_response` | empty | CPEX hooks fired during OnResponse, in order |
| `config` | empty | CPEX runtime YAML inline; mutually exclusive with `config_file` |
| `config_file` | empty | path to CPEX YAML; mounted from a ConfigMap in production |
| `fail_open` | `false` | on a CPEX internal error: deny (false) or continue (true). The rejection renders per the listener — MCP error frame for MCP requests, else HTTP 502 (see Decisions and denials) |
| `worker_threads` | `0` | CPEX tokio worker pool size; 0 lets CPEX pick |
| `bypass_paths` | health + `.well-known` | URL path globs that skip CPEX entirely |
| `bypass_hosts` | keycloak / SPIRE / observability | host globs that skip CPEX, outbound only |
| `strict_hosts` | empty | tool servers, as host globs, IP addresses or CIDRs, whose unclaimed, unreadable JSON-object bodies are refused (`cpex.unparseable_request`), outbound only, before `bypass_hosts` and `bypass_paths` are consulted |
| `fallback_identification` | `global` | identification for requests naming no tool, prompt, resource or model: `global` (CPEX's `http`/`*` entity) or `none` |
| `session_source` | `header` | session id for CPEX session state: `header` (`X-Session-Id` or the A2A context id) or `resolved` (the listener's session, only when a dedicated agent session header or the process chain named it; a session a parent agent named wins over one its child names, but only where Cortex has process attribution — `cortex` bound to loopback, as the laptop install is; in the `authbridge-cpex` sidecar `resolved` trusts the dedicated header, which the sender sets) |
| `private_destinations` | `false` | let CPEX builtins (JWKS, token exchange, CIBA, Vault) reach loopback, private and link-local addresses |
| `synthetic_egress` | `false` | present requests no parser claimed as calls to the tool `http_egress` (see below) |
| `principal.token_file` | empty | file holding a caller JWT that Cortex asserts on outbound tool calls and inference requests; empty turns it off |
| `principal.header` | `x-user-token` | identity header the token is presented under; must not be `Authorization` |
| `workload.audience` | empty | audience of the pod's JWT-SVID that Cortex asserts as the calling workload; empty turns it off |
| `workload.header` | `x-workload-token` | identity header the JWT-SVID is presented under; must not be `Authorization` or `principal.header` |
| `observe_codes` | empty | policy violation codes recorded as observe instead of enforced; the labels the denying step set still apply (see below). Plugin-emitted codes are refused |

A `cpex` plugin with no hooks, no config, and no config file is installed but
inert. Configure succeeds and both phases return continue. `config` and
`config_file` carry the CPEX YAML verbatim, exactly what CPEX's own docs
describe, with no AuthBridge-side reshaping. Unknown config keys are rejected at
boot.

`bypass_hosts` is honored on the outbound phase only. On the inbound
reverse-proxy phase the Host header is attacker-controlled and identity is
resolved inside the bypassed chain, so a spoofed Host must not skip CPEX.
`bypass_paths` applies on both phases, except that with `synthetic_egress` it
applies inbound only: the outbound path is the caller's choice for any host. For
the same reason `synthetic_egress` drops the default `bypass_hosts`, whose
wildcards a caller can match with a host of its own (`keycloak.evil.example`);
an explicit list is kept. Neither list accepts `*` or empty; to disable the
plugin, remove it from the pipeline.

A request a parser claimed is denied before CPEX sees it when its JSON body has
an object with a repeated key or two keys equal under case folding
(`cpex.ambiguous_request`): the parsers match keys case-insensitively and keep
the last repeat, a server may not, and policy would judge a different request
from the one the server runs. Case folding counts only in the objects the
parsers decode into structs: the request envelope, JSON-RPC `params`, the A2A
message and its parts, inference `messages[]` and content blocks, Responses
`input[]` items, and tool definitions. It also counts at every depth under MCP
`params.arguments`, which servers commonly bind into a struct with
`encoding/json`; the body cannot tell a struct from a map, so a map-valued
argument such as an `env` or `headers` map is denied too when two of its keys
differ only in case. Everywhere else (tool schemas, A2A data parts, `metadata`,
`response_format`, and inference tool calls, whose arguments are a JSON string
or the model's earlier call) only exact repeats count. An MCP `tools/call` or `prompts/get` with no name, or a
`resources/read` with no URI, is denied as `cpex.unnamed_entity` rather than
taking the global identification. An outbound request to a host listed in
`strict_hosts` (empty by default) whose body no parser claimed is denied as
`cpex.unparseable_request` when that body is a JSON object the parsers cannot
read: one in UTF-16 or UTF-32 or after a byte order mark, one whose first value
`encoding/json` rejects (`NaN`, `Infinity`, a number out of float64 range, a
truncated object), one whose keys collide, or one whose top-level `method` key,
in any case, does not decode as a string. A parser declines such a body, so it
would otherwise take the global identification, while a server may still run
it as the tool call no route judged: Python's `json.loads` accepts `NaN`,
`Infinity` and out-of-range numbers by default, and reads bytes in UTF-16 or
UTF-32 and UTF-8 after a byte order mark (it rejects a byte order mark in a
`str`, invalid UTF-8 and truncated JSON). Invalid UTF-8 inside a string is not
refused: `encoding/json` replaces it with U+FFFD and parses the body. List the
tool servers that read JSON leniently. The check is scoped by destination, not by the
body's shape: a caller cannot avoid it by dropping or renaming `jsonrpc`, and
traffic to every other host (a `{"loss": NaN}` metrics post, NDJSON uploads) is
not checked. Only the first value is read, so trailing data and NDJSON are not
refused on a strict host either. None of these denials is subject to
`fail_open`.

`strict_hosts` protects only the destinations it lists. An entry is a host glob
(lowercased, matched without the port or a trailing dot), an IP address, or a
CIDR such as `10.96.0.0/12` or `fd00:10:96::/112`. A request whose host reads as
an address is matched against the addresses and CIDRs only, by its canonical
address, port removed: an IP literal in any case or with leading zeros,
bracketed or not, IPv4-mapped IPv6, with a zone (`%eth0`, `%25eth0`) or a
trailing dot, or a numeric spelling `inet_aton` and so the system resolver
accept (`0x0a.0x60.0.10`, `012.0140.0.012`, `10.6291466`, `174063626`). A
name is matched against the globs only. A name list alone does not cover a tool
server reached by its cluster IP or pod IP, so list those addresses or ranges
too; a DNS name that resolves to a listed address is not caught, so list it by
name. A request host that is neither a name nor an address (characters outside
a host name, misused brackets, an all-numeric host `inet_aton` refuses such as
`10.96.0.256`) is treated as strict, so an odd spelling fails closed.

Configure classifies every entry as exactly one of a CIDR (contains `/`; no
host bits set), a canonical IP address (an IPv6 one optionally in brackets), or
a name glob (letters, digits, `-`, `_`, `.`, `*` and `?`, with at least one
label that is not a number or a wildcard), and refuses anything else rather
than keep an entry that would never match: `10.1`, `010.96.0.1`,
`10.96.0.1/12`, `10.96.*.*`, `*.*`, `[10.96.0.1]`. It also refuses a CIDR
covering every address (`0.0.0.0/0`, `::/0`) and an entry with a port or a
zone.

`delegated_credential_hosts` takes the same entries but matches the other way
round, because it decides where a credential may go rather than what to check:
an address entry matches a canonical IP literal only, so a numeric spelling the
resolver alone would read as that address is not bound and the credential is
withheld, a name entry never matches an address, and a host it cannot read
matches nothing. The check runs
before `bypass_hosts` and `bypass_paths`, so a destination on both lists is
still checked. It is a backstop, not a substitute for a strict tool server:
the server should also refuse what it cannot parse strictly, as the HR demo's
`hr-mcp` does.

### Plain egress as a tool call

With `synthetic_egress: true`, an outbound request that names no tool, prompt,
resource or model (plaintext HTTP or a bridged HTTPS request) is presented to
CPEX as a call to the tool `http_egress`, on the request phase. That includes
bodies a parser claimed as an MCP protocol method (`tools/list`, `initialize`)
or an A2A message, so shaping a body as one does not route around the egress
policy; a changed `http_egress` payload has no re-serializer and is an error
that `fail_open` governs. Inbound requests are never egress. Its arguments:

| Argument | Value |
|---|---|
| `method` | upper-case HTTP method |
| `host` | lower-case host, port stripped |
| `path` | URL path, never the query |
| `body` | the first 8 KiB of the body, as UTF-8 |
| `truncated` | whether the body was longer than the excerpt |
| `content_encoding` | the lower-case `Content-Encoding`, empty when absent |

An opaque `CONNECT` tunnel is presented with its method and host, an empty path
and no body, so a host allowlist governs tunnels too. On a `tool:` route CEL
reads the arguments as `args.<name>`:

```yaml
global:
  pdp:
    - kind: cel
routes:
  - tool: http_egress
    authorization:
      pre_invocation:
        - cel:
            expr: "args.host in allowed"
            allowed: ["support.local", "github.com"]
            on_deny: ["deny('host not allowed', 'egress_host')"]
```

The excerpt is policy input only. The invocation records its size and whether
it was cut (`egress_excerpt_bytes`, `egress_truncated`), never its content. A
policy that inspects bodies should deny when `truncated` is true or
`content_encoding` is set, since it cannot see the rest.

With or without `synthetic_egress`, plain egress is refused before CPEX sees it
in one case only: an unreadable JSON object sent to a `strict_hosts` host
(`cpex.unparseable_request`, above), such as a UTF-16 tool call or one with a
`NaN` in its arguments. That check is not subject to `fail_open`. Bodies to
any other host, JSON or not, reach the egress policy as they were sent.

### Cortex-asserted principal

`principal.token_file` lets Cortex assert who the caller is, for an agent that
must not hold the credential. For requests presented as tool calls (MCP tools
and `http_egress`) and for inference requests, the token is added under
`principal.header` to the headers CPEX identity resolution reads, so an
`identity/jwt` plugin configured with that `header:` resolves the subject, and a
`taint` on an `llm:` route has one. Only outbound requests get it: an inbound
caller is never asserted as the principal. The token is never written to the
forwarded request, and a caller's own copy of the header reaches neither
identity resolution nor the `http` extension. When the file cannot be read,
Cortex logs a warning at startup and denies those requests with
`cpex.principal_unavailable` until it can. The file is read again whenever
its modification time or size changes, so a rotated token is picked up; a
read that fails or finds the file empty keeps the previous token.

### Cortex-asserted workload

`workload.audience` does the same for the calling workload: on the same
requests, Cortex fetches the pod's JWT-SVID for that audience from the
in-process SPIFFE provider and adds it under `workload.header`, so an
`identity/jwt` plugin with `role: caller_workload` resolves the agent as
itself and `delegate(..., subject: caller_workload)` can present the SVID
as a client assertion. The sidecar shares the pod with the agent, so the
pod's SVID is the agent's identity. It needs the runtime config's
`spiffe:` block. Without one, or when a fetch fails, nothing is added and
routes that authenticate by it deny. The SVID is never written to the
forwarded request, and a caller's own copy of the header reaches neither
identity resolution nor the `http` extension.

## Decisions and denials

The plugin maps the CPEX outcome onto AuthBridge's five-value invocation
vocabulary:

- **allow**: all sub-plugins continued and the forwarded message is unchanged.
  A result that only adds labels (`taint`) or runs a `delegate` step is an
  allow, as is one returning the headers and payload as sent, which CPEX 0.3
  does on every allowed call.
- **deny**: a sub-plugin returned a policy violation with a `cpex.<code>` code
  (`cpex.pii_detected`, `cpex.session_tainted_secret`, and so on). The plugin
  emits a `Reject`; how it appears on the wire is the listener's call, not the
  plugin's (see below). The response and the invocation carry only that code
  and the message `request denied by policy`: the policy's own reason can quote
  the request, so it goes to the debug log.
- **modify**: policy changed the forwarded body or headers. Changes are
  applied, the request continues. A body rewrite of an inference request is
  supported on OpenAI chat requests with a `messages` array only, and of an
  inference response on a single non-streaming OpenAI chat completion's text
  only; one on any other dialect or shape, a stream, a changed tool result or tool call, or a removed
  part is an error that `fail_open` governs. Request headers are written back as a diff against
  those sent to CPEX, so an assertion can strip or replace a header. Headers
  never sent (Cookie, API keys), the hop-by-hop and framing headers (`host`,
  `content-length`, `transfer-encoding`, `connection`, `keep-alive`, `te`,
  `trailer`, `upgrade`), `proxy-*`, `x-authbridge-*` and `principal.header`
  are never written; such writes are dropped and logged by name. A returned
  header name or value that is invalid on the wire (a CR or LF, say) is an
  error that `fail_open` governs, and nothing is written.
- **observe**: audit-only outcome, recorded, request continues.

Some denials come from the plugin rather than from policy, and `fail_open`
does not apply to them: `cpex.ambiguous_request`, `cpex.unnamed_entity` and
`cpex.unparseable_request` (above), `cpex.principal_unavailable` (above),
`cpex.delegation_destination_mismatch` (below), and
`cpex.delegation_unavailable`. The last is returned when a `delegate(...)`
step granted a token the plugin cannot forward: `delegated_credentials` is
off (the engine then keeps the token), or a credential came back for a
header outside `delegated_credential_headers`, or without its value.
Forwarding would send the caller's own bearer where the policy asked for the
delegated one.

**Delegated credentials.** With `delegated_credentials: true` the engine
returns each minted credential to the plugin, which writes it to the request
as its header (default allowlist: `Authorization`), replacing the caller's
value, after the header write-back. The value never enters the invocation,
the session record or a log, and later CPEX calls on the same request do not
see it. It needs CPEX 0.3.1 or later; a 0.3.0 library rejects the
option, failing the plugin at startup.

The agent picks the tool and the destination, so each credential is bound
to the hosts it may reach. `delegated_credential_hosts` maps a delegation
target (the `target:` of the `delegate(...)` step, else its audience) to
`.`-delimited host globs, matched against the request's host without its
port; it is required with `delegated_credentials`. A credential whose target
is unbound, or bound elsewhere, denies the request (403,
`cpex.delegation_destination_mismatch`) and nothing is written to it:

```yaml
delegated_credentials: true
delegated_credential_hosts:
  workday-api: [hr-mcp, hr-mcp.cpex-demo]
```

**How a rejection is rendered.** The plugin sets the decision and a status
(`403` for a policy deny, `502` for a CPEX-internal error); the HTTP/proxy
listeners then choose the wire shape (`core/listener/httpx/render.go`):

- **Classified MCP request** (an MCP parser populated the MCP extension with a
  JSON-RPC `method` and `id`) → an **MCP JSON-RPC 2.0 error frame at HTTP 200**,
  with the original id echoed back:
  `{"jsonrpc":"2.0","id":<id>,"error":{"code":-32000,"message":<reason>,"data":{"error":"cpex.<code>","plugin":"cpex"}}}`.
  The caller's MCP client surfaces this as a failed tool call rather than a
  transport break. This covers both a policy deny and a fail-closed internal
  error — they differ in the frame's `message` / `data.error`, not the HTTP
  status.
- **Everything else** (non-MCP requests, or JSON-RPC notifications with no id) →
  a plain HTTP response carrying the status the plugin set: **403 Forbidden** for
  a policy deny, **502 Bad Gateway** for a CPEX-internal error, with a
  `{"error":"cpex.<code>",...}` body.

(The gRPC listener — extproc — doesn't use this renderer and surfaces
denials per its own protocol.)

**A call waiting for approval.** A `require_approval(...)` step that has not
been answered yet suspends the call rather than denying it. The client gets
`cpex.elicitation_pending` with the JSON-RPC code `-32120` on an MCP frame (a
403 otherwise), a static message, and `error.data.details.elicitation_id`
(plus `approver` and `expires_at` when known). It retries the same call with
`X-Policy-Elicitation-Id: <id>`; once the approver answers, the retry goes
through or is denied. A retry that also sends `X-Policy-Elicitation-Peek: true`
only reports an approval, as `cpex.elicitation_approved` (`-32121`), without
running the tool. The session record keeps the approver, not the id.

A CPEX policy `deny` is a normal outcome and is always honored. `fail_open` only
governs CPEX-internal failures (an FFI error, an unreachable backend, a
modification that cannot be applied): with `fail_open: false` those are rejected
(rendered as above — an MCP error frame for MCP requests, otherwise HTTP 502);
with `true` they log and continue. The default is fail-closed because a CPEX
error usually means a misconfigured policy or an unreachable PDP, and silently
allowing traffic in that state is rarely intended.

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

# Build cortex with cpex for this host, to ./bin/cortex-cpex/cortex
make cortex-cpex
```

The tagged run also loads every CPEX policy in this README and in
`docs/cpex-plugin.md` on the real engine. The pins live in `cmd/cortex-cpex/`:
`CPEX_FFI_VERSION` is the CPEX release tag, `CPEX_FFI_ABI` the FFI ABI integer,
and `CPEX_FFI_SHA256` the digest of each target's tarball. On macOS, set
`MACOSX_DEPLOYMENT_TARGET` to the SDK version (`xcrun --show-sdk-version`) if
the linker warns that objects target a newer macOS; `make cortex-cpex` does.

## See also

- `demos/hr-cpex` — end-to-end demo with Bob, Eve, and Alice personas
  covering the Workday flow, GitHub flow with Cedar, PII scanning, and session
  taint propagation.
