# cpex plugin

> **The `authbridge-cpex` image is a build variant** of the AuthBridge
> proxy-sidecar — `cortex` built with cgo and `-tags cpex` — deployed *in
> place of* the `authbridge` image when CPEX policy enforcement is needed, not
> as an additional sidecar.

**Where it runs:** Kubernetes, in the `authbridge-cpex` image. A laptop has no
cpex build. See [On a laptop](#on-a-laptop).

The `cpex` plugin embeds the [CPEX](https://github.com/contextforge-org/cpex)
runtime inside Cortex so operators can drive policy with CPEX's
APL (Attribute Policy Language) DSL — and CPEX's built-in plugins (JWT
and API-key identity, OAuth delegation, CIBA elicitation, and the Cedar,
CEL and OPA decision points) — without writing Go. The pinned CPEX
release is 0.3.1.

A single Cortex plugin instance (`cpex`) wraps an entire CPEX
`PolicyEngine`. Operators declare which CPEX hook names fire on each
pipeline phase via the `hooks` block; CPEX's own YAML defines the
sub-plugins those hooks dispatch to.

```text
┌──────────────────────────────────────────────────────────────────┐
│ cortex (built with -tags cpex, links libcpex_ffi)                │
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
                                       │  │  CEL    │ │  OPA    │ │
                                       │  └─────────┘ └─────────┘ │
                                       └──────────────────────────┘
```

## When to use

The cpex plugin is the right home for policy that:

- **Combines multiple decision shapes**. APL's flat predicates
  (`require(role.hr)`) compose naturally with Cedar's principal-×-resource
  rules and IdP-side delegation; expressing that in a single Cortex
  Go plugin would mean reimplementing each engine.
- **Needs to be edited by non-developers**. The APL DSL + Cedar
  policies live in operator YAML; updates ship via ConfigMap reload
  rather than a plugin re-deploy.
- **Composes CPEX's built-in plugins** — JWT identity, OAuth
  delegation, Cedar, CEL and OPA — instead of writing those from scratch
  in Go.

It is **not** the right home for:

- Plugins whose only job is one stable Go decision (`jwt-validation`,
  `token-exchange`) — those stay as Cortex-native plugins.
- Policy that needs to run *before* any parser has classified the
  body. cpex requires at least one of `mcp-parser` /
  `inference-parser` / `a2a-parser` upstream in the chain.

## On a laptop

The laptop cpex build is removed. `install.sh --cpex` exits with an error, and
`make cortex-cpex` is removed. The error names `--praxis` and
`docs/praxis-policy.md`. For laptop policy, use praxis mode with
[`praxis.policy`](praxis-policy.md). A former cpex user reads
[Move from the CPEX plugin](praxis-policy.md#move-from-the-cpex-plugin).
The `authbridge-cpex` image does not change, so Kubernetes use of this plugin
does not change. [The cpex build](laptop-service.md#the-cpex-build) tells how
an existing laptop install moves to the default `cortex`.

## Architecture

### Build constraint

The cpex plugin links `libcpex_ffi.a` via cgo. Only `cortex` built with
`-tags cpex`, `CGO_ENABLED=1` and a profile that carries the plugin compiles
it: `cpex` for the `authbridge-cpex` image, `fullcpex` for a cgo build of it.
Every other build (the `authbridge`, `authbridge-lite` and `authbridge-envoy`
images, the default `cortex` tarballs) stays pure-Go and never imports the
plugin. These builds do not register `cpex`, so a config that names it fails at
boot with an `unknown plugin "cpex"` error. It is not a silent no-op.

The plugin's chassis (config decode, dispatch, Invocation recording)
is tag-free; only the cgo adapter that talks to `cpex.PolicyEngine`
is `//go:build cpex`. Unit tests run with `CGO_ENABLED=0` against a
fake Manager. The commands are in the README's
[Building and testing](../core/plugins/cpex/README.md#building-and-testing).

### Hook chains

Each pipeline phase (`OnRequest`, `OnResponse`) dispatches an
ordered list of CPEX hook names. Hooks fire one at a time; the chain
short-circuits on the first sub-plugin that returns deny. A sub-plugin
that returns `modify` records the Invocation, applies its modifications
to pctx (rewritten body, mutated headers, session labels), and lets the
chain continue. CPEX's standard hook names:

| Hook | Fires for | Typical sub-plugins |
|---|---|---|
| `cmf.tool_pre_invoke` | MCP tool call about to be forwarded, or plain egress with `synthetic_egress` | Cedar, CEL or OPA steps, delegation |
| `cmf.tool_post_invoke` | MCP tool result returned | result redaction, post-result checks |
| `cmf.llm_input` | LLM inference request | checks on prompts and tool results, session taint |
| `cmf.llm_output` | LLM inference response | output checks |

### Decision vocabulary

CPEX sub-plugin verdicts map onto the 5-value
`InvocationAction`:

| CPEX | Cortex | Chain effect |
|---|---|---|
| Allow | `allow` | Continue to next hook. Includes a result that only adds labels (`taint`) or returns the headers and payload as sent |
| Deny  | `deny`  | **Stop chain**, return `pipeline.Reject` |
| Modify | `modify` | Continue to next hook (the forwarded body or headers changed) |
| Observe | `observe` | Continue (diagnostics, no flow change) |

A CPEX-internal failure (FFI panic, runtime error) is distinct from a
policy `deny` — it surfaces as `cpex.error` and respects the
`fail_open` flag (see Configuration).

## Configuration

A `cpex` entry carries two kinds of configuration, and they live at
different levels:

- **Cortex-side fields** are the keys of the plugin's `config:`
  block: which CPEX hooks fire on each phase, what happens when CPEX
  itself fails, and which traffic skips CPEX. The table below is the
  complete list. Any other key is rejected at boot with an
  `unknown field` error.
- **The CPEX runtime YAML** declares the sub-plugins, PDPs and routes
  CPEX loads. Cortex does not parse or reshape it. It arrives as a file
  (`config_file`) or as one string (`config`) and goes to CPEX's
  `LoadConfig` verbatim, so its schema is CPEX 0.3's own: see
  [CPEX's configuration reference](https://contextforge-org.github.io/cpex/docs/configuration/)
  and, for an older policy, [Migrating a CPEX 0.2.x policy](#migrating-a-cpex-02x-policy).

```yaml
plugins:
  - name: cpex
    config:
      hooks:
        on_request:
          - cmf.tool_pre_invoke
        on_response:
          - cmf.tool_post_invoke
      # The CPEX runtime YAML, mounted from a ConfigMap.
      config_file: /etc/cpex/cpex.yaml
      fail_open: false
      worker_threads: 2
```

| Field | Type | Default | Purpose |
|---|---|---|---|
| `hooks.on_request` | `[]string` | empty | CPEX hook names fired during OnRequest, in order. Empty makes the phase a no-op. |
| `hooks.on_response` | `[]string` | empty | CPEX hook names fired during OnResponse, in order. |
| `config_file` | string | unset | Path to the CPEX runtime YAML. Read when the plugin is configured; Cortex does not watch it for changes. Mutually exclusive with `config`. |
| `config` | string | unset | The CPEX runtime YAML inline, as a single string. Mutually exclusive with `config_file`. |
| `fail_open` | bool | `false` | On a CPEX-internal error: reject (`false`) or log and continue (`true`). A policy deny is always honored. See [fail_open](#fail_open-when-to-use-it). |
| `worker_threads` | int | `0` | Size of CPEX's tokio worker pool; `0` lets CPEX choose. |
| `bypass_hosts` | `[]string` | Keycloak, SPIRE, observability | Host globs that skip CPEX, on outbound traffic only, matched case-insensitively against the host with its port and IPv6 brackets stripped. Setting the list replaces the default rather than extending it. With `synthetic_egress` the default is dropped; an explicit list is kept. See [Bypass list curation](#bypass-list-curation). |
| `strict_hosts` | `[]string` | empty | Tool servers that may read JSON more leniently than the parsers, such as a Python server, each listed as a host glob (`.`-delimited, as in `delegated_credential_hosts`, matched without the port), an IP address, or a CIDR (`10.96.0.0/12`, `fd00:10:96::/112`). A host that reads as an address — any IP literal spelling, IPv4-mapped IPv6, a zone, a trailing dot, or a numeric `inet_aton` form such as `0x0a.0x60.0.10` — is matched by its canonical address against the addresses and CIDRs only; a name only against the globs, so a DNS name resolving to a listed address is not caught. A host that is neither a name nor an address (`10.96.0.256`, misused brackets) is treated as strict. An outbound request to one of them whose body no parser claimed is refused as `cpex.unparseable_request` when it is a JSON object the parsers cannot read. It protects only the destinations it lists: a server reached by its cluster IP or pod IP is covered only when that address is listed. It is checked before `bypass_hosts` and `bypass_paths`, so a destination on both is still checked. Every entry must be a CIDR without host bits, a canonical IP, or a name glob with a label that is not a number or a wildcard; anything else (`10.1`, `10.96.0.1/12`, `10.96.*.*`, `*.*`), CIDRs covering every address (`0.0.0.0/0`, `::/0`), and entries with a port or a zone are refused. The tool server should also parse strictly, as the HR demo's `hr-mcp` does. Empty, the default, checks no host. |
| `bypass_paths` | `[]string` | `/healthz`, `/readyz`, `/livez`, `/metrics`, `/.well-known/*` | URL path globs that skip CPEX in both directions, or inbound only with `synthetic_egress`. Setting the list replaces the default. |
| `fallback_identification` | string | `global` | How a request naming no tool, prompt, resource or model (A2A, MCP `tools/list` and `initialize`, unparsed traffic) is identified: `global` sends CPEX's global entity (`http`/`*`), so `http:` routes and the global policy govern it; `none` sends no identification, which CPEX denies as `unidentified_request` when the config declares any policy. Inference requests are identified as `llm`/<model>. |
| `session_source` | string | `header` | The session id CPEX keys session state (taint labels) on. `header` reads `X-Session-Id`, or the A2A context id. `resolved` uses the session the forward proxy resolved, but only when a dedicated agent session header (such as `X-Claude-Code-Session-Id`) or the process chain named it; a guessed session (most recent, User-Agent affinity, pending or default bucket) and the generic `X-Session-Id` and `Session-Id` headers give none, and the caller's `X-Session-Id` and A2A context id are ignored. When the process chain shows the sender was started by an agent that named a session, that session is used even if the sender named one of its own, so a child process cannot leave its parent's taint. That protection needs process attribution, which Cortex has only when bound to loopback, as the laptop install is; the `authbridge-cpex` sidecar has none, so there `resolved` trusts the dedicated session header, which the sender sets, and does not stop a process from leaving its session's taint. CPEX binds session state to the subject, so a request with no subject has none either way. |
| `private_destinations` | bool | `false` | Let CPEX builtins (JWKS fetches, token exchange, CIBA, Vault) reach loopback, private and link-local addresses, cloud metadata included. Turn on only for trusted policies on a trusted network, such as a local IdP in development. |
| `synthetic_egress` | bool | `false` | Present every outbound request that names no tool, prompt, resource or model as a call to the tool `http_egress`, including bodies a parser claimed as an MCP protocol method (`tools/list`, `initialize`) or an A2A message, with arguments `method`, `host` (port stripped), `path` (no query), `body` (first 8 KiB), `truncated` and `content_encoding`. A `CONNECT` tunnel carries its method and host only. CEL reads them as `args.<name>` on a `tool: http_egress` route. The excerpt is never recorded; the invocation keeps only its size and the truncated flag. Because the outbound host and path are the caller's choice, `bypass_paths` then applies inbound only and the default `bypass_hosts` is dropped (an explicit list is kept). |
| `principal.token_file` | string | empty | File holding a caller JWT Cortex asserts on outbound tool calls (MCP tools and `http_egress`) and inference requests: it is supplied to CPEX identity resolution and never forwarded to the destination. The file is read again when it changes. A missing file logs a warning at startup and denies those requests with `cpex.principal_unavailable`. |
| `principal.header` | string | `x-user-token` | Header the principal is presented under; the `identity/jwt` plugin's `header:` must match. A caller's own copy never reaches identity resolution. Must not be `Authorization`. |
| `workload.audience` | string | empty | Audience of the pod's JWT-SVID that Cortex asserts as the calling workload, on the same requests as the principal: it is fetched from the in-process SPIFFE provider (the runtime config's `spiffe:` block) and supplied to CPEX identity resolution, so an `identity/jwt` plugin with `role: caller_workload` resolves the agent. Never forwarded to the destination. Empty turns it off. |
| `workload.header` | string | `x-workload-token` | Header the JWT-SVID is presented under. A caller's own copy never reaches identity resolution. Must not be `Authorization` or `principal.header`. |
| `delegated_credentials` | bool | `false` | Forward the credentials `delegate(...)` steps mint: each is written to the request as its header, replacing the caller's value. Off, a request whose policy delegated is denied with `cpex.delegation_unavailable`. The value is never recorded or logged. Needs CPEX 0.3.1 or later; a 0.3.0 library fails it at startup. |
| `delegated_credential_hosts` | `map[string][]string` | none | Required with `delegated_credentials`. Binds each delegation target (the `delegate(...)` step's `target:`, else its audience) to `.`-delimited host globs, matched against the request's host without its port. A credential whose target is not bound to the request's host denies it with `cpex.delegation_destination_mismatch` and nothing is written. An entry may also be an IP address or CIDR, which matches only a request whose host is a canonical IP literal in it (any case, leading zeros, brackets, IPv4-mapped IPv6); a numeric spelling only the resolver reads as that address (`0x0a.0x60.0.10`, `174063626`, a trailing dot) or a host that is neither a name nor an address matches nothing, and a name glob never matches an address. Entries are classified as `strict_hosts` entries are, and refused on the same terms, along with patterns that match every host (`*`, `**`). |
| `delegated_credential_headers` | `[]string` | `[Authorization]` | Headers a delegated credential may be written to. A credential for any other header denies the request with `cpex.delegation_unavailable`. Framing, `proxy-*`, `x-authbridge-*`, `principal.header` and `workload.header` are refused. |
| `observe_codes` | `[]string` | empty | Policy violation codes recorded as `observe` instead of enforced; the request continues and the labels the denying step set stay. PPE 0.5.0 has no other conditional taint (a false CEL step always denies), so this lets an `llm:` route taint a session on content without blocking the request. Later steps of that route do not run, and a code shared with another step would silence that step's deny too, so Configure refuses a code that is not named by exactly one `deny('reason', 'code')` in the policy, sits outside a route's own `pre_invocation`/`post_invocation` list (in `global`, `global.defaults` or a group, the route's steps run after it), or is not the last entry of every list down to it (its step, its `on_deny`, its `when`/`do`). Codes the plugin emits itself (`principal_unavailable`, `delegation_unavailable`, `delegation_destination_mismatch`, `ambiguous_request`, `unnamed_entity`, `unparseable_request`) are refused. |

A `cpex` entry with no hooks and no CPEX YAML is valid and inert, so the
plugin can be installed before its policy ships. Configure succeeds, and both
phases return continue.

### The CPEX runtime YAML

The top-level sections this example uses are `plugins:` (the sub-plugins
policy can call), `global:` (cross-cutting PDPs such as Cedar and CEL, and
the session store) and `routes:` (the APL policy, one entry per tool,
prompt, resource or model). `engine_settings:` holds engine-wide settings;
its default, `dispatch: policy`, runs a plugin only where a route reaches
it. [`cortex-demos/hr-cpex/`](https://github.com/rossoctl/cortex-demos/tree/main/hr-cpex/) has a complete working policy.

```yaml
plugins:
  - name: jwt-user
    kind: identity/jwt
    hooks: [identity.resolve]
    capabilities: [perform_http]    # the JWKS fetch is an outbound call
    on_error: fail
    config:
      role: user
      header: X-User-Token
      trusted_issuers:
        - issuer: "https://keycloak.example.com/realms/prod"
          audiences: ["cpex-gateway"]
          algorithms: ["RS256"]
          decoding_key:
            kind: jwks_url
            url: "https://keycloak.example.com/realms/prod/protocol/openid-connect/certs"
      claim_mapper: standard

global:
  pdp:
    - kind: cel
    - kind: cedar-direct
      policy_text: |
        @id("engineering-internal-repos")
        permit(
          principal,
          action == Action::"read",
          resource is Repo
        ) when {
          principal.roles.contains("engineer") &&
          resource.visibility == "internal"
        };

routes:
  - tool: search_repos
    authentication:
      - jwt-user
    authorization:
      pre_invocation:
        - "require(team.engineering)"
        - cedar:
            action: 'Action::"read"'
            resource:
              type: Repo
              id: ${args.repo_name}
              attributes:
                visibility: ${args.visibility}

  - tool: send_email
    authentication:
      - jwt-user
    authorization:
      pre_invocation:
        - "require(perm.email_send)"
        - cel:
            expr: "!has(args.body) || !args.body.matches('[0-9]{3}-[0-9]{2}-[0-9]{4}')"
            on_deny: ["deny('SSN pattern in body', 'pii.detected')"]
```

Identity plugins run only on routes that list them under `authentication:`,
so a route that reads `subject.*`, `role.*` or `authenticated` must list
one. The CEL step catches the one shape it names; it is not data-loss
prevention.

To keep everything in one place, the same YAML can go inline under
`config:` instead. It is a string, so it needs a block scalar (`|`):

```yaml
plugins:
  - name: cpex
    config:
      hooks:
        on_request:
          - cmf.tool_pre_invoke
      config: |
        global:
          pdp:
            - kind: cel
        routes:
          - tool: send_email
            authorization:
              pre_invocation:
                - cel:
                    expr: "!has(args.body) || !args.body.matches('[0-9]{3}-[0-9]{2}-[0-9]{4}')"
                    on_deny: ["deny('SSN pattern in body', 'pii.detected')"]
```

### Migrating a CPEX 0.2.x policy

CPEX 0.3.0 rejects every key it does not know, so a 0.2.x policy fails at
boot rather than loading with inert sections. The changes most policies
need:

- Drop the `apl:` wrapper: `pdp:`, `session_store:` and `attribute_files:`
  sit directly under `global:`, and a route's `authorization:`, `args:` and
  `result:` sit on the route.
- `plugin_settings:` is `engine_settings:`; `routing_enabled: true` is the
  default `dispatch: policy` and can go.
- `identity:` is `authentication:`; `policy:` and `post_policy:` are
  `authorization.pre_invocation:` and `authorization.post_invocation:`.
- A plugin that makes HTTP calls declares `capabilities: [perform_http]`:
  `identity/jwt` with a `jwks_url`, `delegator/oauth`, `elicitation/ciba`.
- Every JWT trusted issuer lists `audiences`.
- `validator/pii-scan` and `audit/logger` are gone. Use a CEL or APL deny
  step for PII patterns, an APL `redact` field pipeline for redaction, and
  the session timeline for audit: every cpex invocation is recorded there
  with its decision and violation code.
- A declared plugin that no route reaches (through `authentication:`,
  `run(...)` or `delegate(...)`) fails the load.
- Route entities changed. Inference was entity `model`/<name> and is now
  `llm`/<model>, so a `- model: <name>` route becomes `- llm: <model>`. A2A
  was `a2a_method`/<method> and now takes the fallback identification
  (`http`/`*`), so a `- a2a_method: ...` route no longer matches anything;
  move its steps to an `- http: "*"` route or to `global`.

The load error carries no detail: it says `parse / deserialize failed` or
`pipeline / lifecycle error`. CPEX's
[migration guide](https://contextforge-org.github.io/cpex/docs/migration-0.3/)
lists every change.

### Pipeline composition

Place `cpex` after the protocol parsers and any content filters on the phase
that you want to enforce. cpex declares `RequiresAny: [mcp-parser, inference-parser, a2a-parser]`.
At least one must appear earlier in the chain so the parser has populated
`pctx.Extensions.MCP` / `.Inference` / `.A2A` before cpex extracts CMF
content. `Pipeline.Build` rejects misordered chains at boot.

cpex also declares `ReadsBody`, `WritesRequestBody` and `WritesResponseBody`.
Request-body writers chain, so cpex can share a chain with other request
writers, each seeing the body as the one before it left it. **Place cpex first
among them.** It builds its replacement from the parsed client messages
(`pctx.Extensions`), which describe the request as the client sent it, and
writes it back into the body by position — message text for inference and A2A —
or, for MCP, replaces the call's arguments or URI whole. After a writer that
changed those messages, a changed message count is a drift error (fail-closed
unless `fail_open`), and an edit that kept the count is overwritten with the
client's text. Writers after cpex start from its output and are unaffected.
Response writers chain too, so cpex can share a chain with `sparc`. The response
side runs in reverse: with cpex placed first, it runs last among them and
redacts the response the client will receive.

A typical inbound chain:

```yaml
plugins:
  - name: jwt-validation       # identity gate
  - name: mcp-parser           # populate pctx.Extensions.MCP
  - name: cpex                 # APL/Cedar/CEL via CPEX
```

The HR demo enforces on the outbound leg only. Its pipeline is in the README's
[The HR demo pipeline](../core/plugins/cpex/README.md#the-hr-demo-pipeline).

### Requests denied before CPEX

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

The plugin maps the CPEX outcome onto the five-value invocation
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
(rendered as [Status codes & reasons](#status-codes--reasons) says — an MCP error frame for MCP requests, otherwise HTTP 502);
with `true` they log and continue. The default is fail-closed because a CPEX
error usually means a misconfigured policy or an unreachable PDP, and silently
allowing traffic in that state is rarely intended.

## Status codes & reasons

| Code | HTTP (non-MCP) | Meaning |
|---|---|---|
| `cpex.error` | 502 | CPEX FFI returned an error and `fail_open: false`. The underlying error is logged, not returned. |
| `cpex.denied` | 403 | A CPEX sub-plugin returned deny without a violation code. |
| `cpex.<sanitized>` | 403 | A CPEX sub-plugin returned deny with a violation code; the `<sanitized>` part lower-cases letters, replaces other characters with underscores, e.g. `pii.detected` → `cpex.pii_detected`. |
| `cpex.principal_unavailable` | 403 | Principal injection is configured but its token file cannot be read; tool calls that resolve identity are denied. |
| `cpex.delegation_destination_mismatch` | 403 | A `delegate(...)` step minted a credential whose target `delegated_credential_hosts` does not bind to the request's host. Nothing is written to the request. |
| `cpex.delegation_unavailable` | 403 | A `delegate(...)` step granted a token the plugin cannot forward: `delegated_credentials` is off, or the credential names a header outside `delegated_credential_headers`, or came back without its value. Forwarding would send the caller's own bearer where the policy asked for the delegated one, so the request is denied. |
| `cpex.unnamed_entity` | 403 | An MCP `tools/call` or `prompts/get` with no name, or a `resources/read` with no URI. |
| `cpex.unparseable_request` | 403 | An outbound request to a `strict_hosts` destination whose body no parser claimed and that is a JSON object the parsers cannot read: one in UTF-16 or UTF-32 or after a byte order mark, or whose first value `encoding/json` cannot decode (`NaN`, `Infinity`, a number out of float64 range, a truncated object; invalid UTF-8 in a string is replaced with U+FFFD, not refused), whose keys collide, or whose top-level `method` key in any case is not a string. The body's own keys do not decide it, so dropping or renaming `jsonrpc` does not avoid it. Only the first value is read, so NDJSON and trailing data are not refused. A lenient server may still run such a body as the tool call no route judged. Requests to any other host are not checked. Not subject to `fail_open`. |
| `cpex.ambiguous_request` | 403 | A parsed JSON body has a repeated key, or two keys equal under case folding in an object the parsers decode into a struct (the envelope, `params`, messages, content blocks, Responses `input[]` items, A2A message and parts, tool definitions) or a server commonly binds to one (MCP `params.arguments`, at any depth, map-valued arguments such as an `env` or `headers` map included), so policy and the server could read different requests. |

The last six are decided by the plugin rather than by policy, and
`fail_open` does not apply to them.

On a deny, the `Invocation.Reason` field and the response carry the
namespaced code and the static message `request denied by policy`. The
CPEX-side reason is policy-authored text that can quote the request, so
it is logged at debug level only.

**HTTP status / wire shape.** The plugin sets a status on every
rejection — **403 Forbidden** for a policy `deny`, **502 Bad Gateway**
for a CPEX-internal error (the policy engine itself failed — an upstream
fault, not a client error). It must set these explicitly: the namespaced
`cpex.*` codes are dynamic and never appear in the listener's static
`codeToStatus` table, so without an explicit status `Violation.Render`
would default every CPEX outcome to 500.

How that reaches the client depends on the listener and the request. On
the HTTP proxies (forward / reverse), a **classified MCP request** (the
MCP extension is populated with a JSON-RPC `method` and `id`) is rendered
as an **MCP JSON-RPC 2.0 error frame at HTTP 200**, with the id echoed
back:
`{"jsonrpc":"2.0","id":<id>,"error":{"code":-32000,"message":<reason>,"data":{"error":"cpex.<code>","plugin":"cpex"}}}`.
The caller's MCP client then surfaces a failed tool call rather than a
transport break, and the `cpex.*` code rides in `error.data.error` (not
the HTTP status). The MCP frame covers a policy deny and a fail-closed internal error. The two
differ in the frame's `message` and `data.error`, not in the HTTP status.
Non-MCP requests — and JSON-RPC notifications with no
id — get the plain 403 / 502 in the table above, with a
`{"error":"cpex.<code>",...}` body. (See
`core/listener/httpx/render.go`; the gRPC listener,
extproc, doesn't use this renderer.)

## Bypass list curation

`bypass_hosts` and `bypass_paths` short-circuit *before* the FFI call,
so traffic to infrastructure that shouldn't see CPEX policy (Keycloak,
SPIRE, observability) skips the cgo round-trip entirely.

Both lists are operator-extensible glob patterns (`path.Match` syntax).
Default `bypass_hosts` covers `keycloak.*`, `spire-server.*`,
`spire-agent.*`, `otel-collector.*`, `jaeger.*`, `prometheus.*`,
matching the IBAC plugin's default set. Default `bypass_paths` covers
`/healthz`, `/readyz`, `/livez`, `/metrics`, `/.well-known/*` via
`core/bypass.DefaultPatterns` — the same set jwt-validation uses.

`bypass_hosts` is honored on the outbound phase only. On the inbound
reverse-proxy phase the Host header is attacker-controlled and identity is
resolved inside the bypassed chain, so a spoofed Host must not skip CPEX.
`bypass_paths` applies on both phases, except that with `synthetic_egress` it
applies inbound only: the outbound path is the caller's choice for any host. For
the same reason `synthetic_egress` drops the default `bypass_hosts`, whose
wildcards a caller can match with a host of its own (`keycloak.evil.example`);
an explicit list is kept. Neither list accepts `*` or empty; to disable the
plugin, remove it from the pipeline.

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
CPEX sub-plugins may log the payload they receive, and the Cortex
session API has no auth on it.

Stripped prefixes: `authorization`, `cookie`, `set-cookie`,
`proxy-authorization`, `x-amz-security-token`.

Stripped exact names: `x-api-key`, `x-auth-token`, `x-authorization`,
`x-secret-token`, `x-session-token`, `x-csrf-token`,
`x-platform-secret`, `x-authbridge-secret`.

This list is conservative; adding a header here is safer than
discovering one in an audit log.

## Background tasks

CPEX sub-plugins that emit asynchronous work (writing to an external
sink, say) return their results through `BackgroundTasks`. The cpex plugin spawns a goroutine per
Invoke that awaits the background work and routes per-sub-plugin
errors through the default `slog` handler with the request ID.

Operators pipe `slog` output to their observability stack. There is no
configurable audit sink today; future work may add one if operators
need to route background results separately from foreground logs.

## Limitations

- **Single-value body redaction; multi-part rewrites fail closed.** Body
  modifications **are** re-serialized. When a CPEX sub-plugin returns
  `modify` with a body payload change (an APL `redact` field pipeline is
  the canonical case), the cpex plugin rewrites `pctx.Body` format-aware
  per protocol — MCP `tools/call` arguments and results, inference
  messages, and A2A artifact parts (see `applyBodyModFromCMF` and the
  per-format `applyMCP*/applyInference*/applyA2A*` write-backs). The
  residual limitation is value cardinality: the CMF write-back carries a
  single redacted value, so a body with more than one redactable text
  part (an MCP result with multiple text blocks, a multi-part A2A
  artifact) is ambiguous to rewrite and is **rejected fail-closed** rather
  than partially redacted. Single-part bodies — the common case, and what
  the demo exercises — redact and forward normally.
- **Per-sub-plugin Invocations** require the CPEX FFI to expose
  per-sub-plugin outcomes; today CPEX returns an aggregate
  `PipelineResult` with a single `Violation`. The cpex plugin emits
  one aggregate Invocation per Invoke as a result. A CPEX-side
  change to expose `PipelineResult.SubPlugins[]` will let the
  plugin emit one Invocation per sub-plugin.
- **A2A traffic** is not auto-classified into a CPEX hook. Operators
  configure a chain explicitly via `hooks.on_request: [<some hook>]`
  even for A2A; CPEX's own routing handles per-sub-plugin filtering.

## Failure modes (detailed)

| Symptom | Cause | Fix |
|---|---|---|
| Pipeline build fails at boot: `unknown plugin "cpex"` | The `cortex` binary is not a cpex build, and the config names a `cpex` plugin. | Use the `authbridge-cpex` image. Or build with the `cpex` profile, `-tags cpex` and a downloaded `libcpex_ffi.a`. |
| Pipeline build fails: `cpex bypass_paths: invalid bypass pattern` | An entry in `bypass_paths` has bad `path.Match` syntax. | Fix the glob pattern (`/api/*/v1`, `/static/**` etc.). |
| Pipeline build fails: `cpex config: pattern "*" matches everything` | Operator wrote a wildcard pattern in `bypass_hosts` or `bypass_paths`. | If you want to disable cpex, remove it from the pipeline; don't bypass everything. |
| All traffic is rejected with `cpex.error` (a 200 MCP error frame for MCP calls, else 502), JWKS errors in logs | A CPEX `identity/jwt` plugin can't fetch its JWKS from Keycloak. | Check that the plugin's `decoding_key.url` in the CPEX YAML is reachable from inside the pod, and that `bypass_hosts` still covers Keycloak (the default does). |
| Pipeline build fails: `LoadConfig: cpex: parse / deserialize failed` | The CPEX YAML has a key, hook or plugin kind 0.3.0 does not know — usually a 0.2.x policy (`apl:`, `plugin_settings:`, `validator/pii-scan`). | Migrate it; see [Migrating a CPEX 0.2.x policy](#migrating-a-cpex-02x-policy). |
| Pipeline build fails: `LoadConfig: cpex: pipeline / lifecycle error` | A declared plugin no route reaches, or a plugin that cannot start (an unset `env_var` secret, for instance). | Reference the plugin from a route or drop it; check its secrets. |
| Requests deny with `cpex.auth_jwks_unavailable` while the IdP is up | The JWKS URL is on loopback or a private address, which CPEX's HTTP transport refuses by default. | Set `private_destinations: true` if the policy is trusted, or reach the IdP through a public address. |
| Background work fails silently | A `BackgroundTasks` goroutine started but its sink is unreachable. | Search the slog stream for `cpex: background sub-plugin error` — the request ID and elapsed time correlate the failure with the foreground request. |

## See also

- [`core/plugins/cpex/README.md`](../core/plugins/cpex/README.md) — the
  plugin's own reference: CMF mapping, APL examples, building and testing.
- [`cmd/README.md`](../cmd/README.md) — the cpex builds of `cortex` and the
  `authbridge-cpex` image.
- `cmd/cortex-cpex/CPEX_FFI_VERSION` — the CPEX release tag the FFI is
  pinned to. `CPEX_FFI_ABI` beside it holds the FFI ABI integer, and
  `CPEX_FFI_SHA256` the digest of each target's tarball;
  `scripts/fetch-cpex-ffi.sh` downloads the library and checks it against
  both.
- [CPEX documentation](https://contextforge-org.github.io/cpex/docs/) —
  the runtime YAML, APL, and the built-in sub-plugins.
- [CPEX repository](https://github.com/contextforge-org/cpex) — source
  and the FFI ABI.
- [`cortex-demos/hr-cpex/`](https://github.com/rossoctl/cortex-demos/tree/main/hr-cpex/) — runnable demo configurations.
