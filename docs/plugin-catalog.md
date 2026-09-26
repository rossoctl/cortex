# Plugin Catalog

Catalog of AuthBridge pipeline plugins — every plugin with a Go
implementation that calls `plugins.RegisterPlugin()`. For the config
convention, session-event contract, and lifecycle interfaces plugins
implement, see [`plugin-reference.md`](./plugin-reference.md). For
writing a new plugin, see [`plugin-tutorial.md`](./plugin-tutorial.md).

"Production ready?" reflects whether the plugin is carried by a shipped
profile in `scripts/profile-tags` versus available only on
explicit request or requiring a separate binary. Every plugin is opt-in
(`-tags include_plugin_<name>`); a build with no tags registers none. It is
a packaging signal, not a claim about test coverage or operational maturity.

## Plugins

"Direction" is inbound (caller → this agent) or outbound (this agent →
callee); "both" means the plugin evaluates on both pipelines. "Default
config?" marks whether the plugin is enabled in Rossoctl's default
AuthBridge pipeline YAML, not whether it is compiled into the binary
(see "Production ready?" above for that).

| Name | Description | Production ready? | Direction | Default config? |
|------|-------------|--------------------|-----------|------------------|
| [`a2a-parser`](#a2a-parser) | Parses A2A messages into `pctx.Extensions.A2A` for downstream plugins. | Beta | Inbound | No |
| [`context-guru`](#context-guru) | Compacts the outbound LLM request context before forwarding. | Opt-in | Outbound | No |
| [`cpex`](#cpex) | APL DSL + named [CPEX](https://github.com/contextforge-org/cpex) plugins (Cedar, PII, audit, …) over a single chain step. | Opt-in | Outbound | No |
| [`ibac`](#ibac) | LLM-judge intent-based access control for outbound tool calls. | Alpha | Outbound | No |
| [`inference-parser`](#inference-parser) | Parses LLM completions into `pctx.Extensions.Inference`. | Alpha | Outbound | No |
| [`jwt-validation`](#jwt-validation) | Inbound JWT validation (signature, issuer, audience) against JWKS. | Ready | Inbound | YES |
| [`lineage-telemetry`](#lineage-telemetry) | Emits two facts-only OTel lineage spans per HTTP exchange, parented across pods through one `tracestate` member. | Alpha | Both | No |
| [`litellm-budget-track`](#litellm-budget-track) | Tracks `x-litellm-response-cost` (with `-original` fallback) and enforces a daily budget limit. Place on whichever chain carries LLM traffic — inbound when fronting the LLM endpoint, outbound when hosting an agent via `authbridge exec`. | Alpha | Both | No |
| [`mcp-parser`](#mcp-parser) | Parses MCP tool calls/results into `pctx.Extensions.MCP`. | Beta | Outbound | No |
| [`opa`](#opa) | [OPA](https://www.openpolicyagent.org/docs) policy enforcement for inbound and outbound requests. | Alpha | Both | No |
| [`sparc`](#sparc) | Pre-tool reflection: blocks ungrounded/hallucinated tool calls. | Alpha | Outbound | No |
| [`static-inject`](#static-inject) | Swaps a placeholder credential for a real static credential on outbound requests. | Alpha | Outbound | No |
| [`session-budget`](#session-budget) | Enforces per-session token, call, and duration budgets via Redis. | Alpha | Outbound | No |
| [`token-broker`](#token-broker) | Exchanges incoming tokens against a configured IdP via a broker service. | Alpha | Outbound | No |
| [`token-exchange`](#token-exchange) | RFC 8693 outbound token exchange per route. | Ready | Outbound | YES |
| [`tool-prune`](#tool-prune) | Removes unused tool definitions from inference requests. | Alpha | Outbound | No |

## `a2a-parser`

Parses A2A JSON-RPC 2.0 request bodies into `pctx.Extensions.A2A`
(method, session ID, message parts, role) for downstream guardrails.

No configuration — registered as a bare plugin name, no `config:` block.

## `context-guru`

Compacts an agent's outbound LLM request context before forwarding,
using the embedded context-guru engine. `OnResponse` is currently a
pass-through; model-driven expand/restore is a later integration.
Opt-in at build time (`-tags include_plugin_contextguru`) because its
engine pulls a large transitive dependency set.

- `paths` (`[]string`) — inference request paths to compact. Default: `/v1/chat/completions`, `/v1/completions`, `/v1/messages`.
- `model` (object) — optional "cheap" LLM endpoint for model-backed components (summarize, extract:code); omitted means those degrade to deterministic/no-op.
  - `base_url` — OpenAI-compatible endpoint base.
  - `model` — model name to call.
  - `api_key` — optional bearer token.
  - `max_tokens` — completion cap, default 4096.
  - `timeout_ms` — per-call timeout, default 150000.
- `engine` (object) — native context-guru config (preset / pipeline / per-component / store), passed through verbatim. Default: `preset: balanced`.

## `cpex`

Bridges AuthBridge hooks to the [CPEX](https://github.com/contextforge-org/cpex)
framework (a policy enforcement runtime for AI agents): an APL DSL plus named
CPEX policy plugins (Cedar, PII, audit, …). Requires the separate
`authbridge-cpex` binary (`-tags cpex`, `CGO_ENABLED=1`, links a pinned
`libcpex_ffi.a`). Full details in [cpex-plugin.md](./cpex-plugin.md);
see also the plugin's [README](../authlib/plugins/cpex/README.md).

- `hooks.on_request` / `hooks.on_response` (`[]string`) — [CPEX hook names](https://contextforge-org.github.io/cpex/docs/0.1.x/hook-types/) to fire on each phase, in order (AuthBridge classifies traffic onto the `cmf.*` hooks — see [Hook chains](./cpex-plugin.md#hook-chains)).
- `config` (string) — inline CPEX runtime YAML (`plugins:`/`global:`/`plugin_settings:`); mutually exclusive with `config_file`.
- `config_file` (string) — path to a file with the CPEX runtime YAML; mutually exclusive with `config`.
- `fail_open` (bool) — allow traffic through if CPEX itself errors/panics. A CPEX policy *deny* is always honored regardless. Default `false`.
- `worker_threads` (int) — size of CPEX's tokio worker pool; `0` = automatic.
- `bypass_hosts` / `bypass_paths` (`[]string`) — globs skipped entirely (outbound only for hosts); default to Keycloak/SPIRE/observability infra.

## `ibac`

LLM-judge intent-based access control: judges outbound tool calls
against recorded inbound user intent. Full details, including the
prompt-injection threat model, in [ibac-plugin.md](./ibac-plugin.md).

The "LLM-judge service" is any OpenAI-compatible chat-completions
endpoint (a local Ollama/vLLM, or a hosted provider) — AuthBridge ships
no judge of its own. The plugin POSTs the recorded user intent plus the
proposed action to `{judge_endpoint}/v1/chat/completions` and parses an
allow/deny verdict from the reply — see
[Request Flow](./ibac-plugin.md#request-flow). For guidance on which
model to point it at, see
[Choosing a Judge Model](./ibac-plugin.md#choosing-a-judge-model).

- `judge_endpoint` (string) — base URL of the LLM-judge service (`{endpoint}/v1/chat/completions`).
- `judge_model` (string) — model name passed to the judge.
- `judge_bearer` (string) — optional bearer token; empty for unauthenticated local LLMs.
- `system_prompt` (string) — override the built-in judge system prompt.
- `timeout_ms` (int) — per-call timeout; values below 100 rejected. Default 5000.
- `judge_max_tokens` (int) — cap on judge reply length. Default 1024.
- `judge_json_mode` (`*bool`) — force `response_format: json_object`. Default `true`.
- `judge_inference` (bool) — also judge outbound LLM-reasoning traffic (high cost). Default `false`.
- `agent_llm_host` (string) — the agent's own LLM host; auto-added to `bypass_hosts`.
- `bypass_hosts` / `bypass_paths` (`[]string`) — globs skipped without judging.
- `no_intent_policy` (string) — behavior when an action has no recorded intent: `allow` (default) or `deny`.
- `unclassified_policy` (string) — behavior when no parser claimed the request: `passthrough` (default) or `judge`.

## `inference-parser`

Parses outbound OpenAI-compatible LLM inference requests/responses into
`pctx.Extensions.Inference` for downstream policy plugins, **and prices the finished
response** — it is the one place tokens become dollars.

Costing lives here because this is the only component that knows when usage is *final*: it
owns the three response-finalization paths and the assembled-usage handling (Claude Code's
`?beta=true` path reports cache counts on `message_delta`, not `message_start`). It also means a
priced request can no longer be missing its record: previously the figure came from
`litellm-budget-track`, so a pipeline without that plugin showed tokens and no money, with
the same field silently meaning "modelled" rather than "authoritative" depending on
configuration.

A record is not the same as a price. Where no rate resolves for the model, the record is
still published — carrying the token counts, any avoided cost, and no dollar figure — and the
gap is named in `/v1/usage`'s `unpricedBy` so an operator knows which `pricing:` entry to
add. An absent figure is reported as absent, never as `$0.00`.

The arithmetic and the gateway header semantics are in `authlib/costing`, not in the parser:
a provider-shaped body parser has no business knowing one gateway's header names. The result
is published as a cost record keyed `cost` on the session event (see
[Cost records](pricing.md#cost-records)).

No configuration — no config struct, does not implement `Configurable`. Rates arrive by
injection from the top-level [`pricing:`](pricing.md#overriding-in-config) section; with none configured the parser
still parses and reports the traffic as unpriced.

## `jwt-validation`

Validates inbound JWTs: signature via JWKS, issuer, and audience.

- `issuer` (string) — expected `iss` claim; required.
- `jwks_url` (string) — JWKS endpoint; derived from Keycloak URL/realm or issuer when omitted.
- `keycloak_url` / `keycloak_realm` (string) — used to derive `jwks_url` when omitted.
- `audience` (string) — expected `aud` claim; one of `audience` / `audience_file` / `audience_mode=per-host` required.
- `audience_file` (string) — file to read expected audience from. Default `/shared/client-id.txt`.
- `audience_mode` (string) — `static` (default) or `per-host` (derived from the `Host` header).
- `allowed_audiences` (`[]string`) — extra audience values accepted (OR semantics).
- `bypass_paths` (`[]string`) — path globs skipped. Default `/healthz`, `/readyz`, `/livez`, `/metrics`, `/.well-known/*`.
- `placeholder_mode` (bool) — replace the validated inbound token with an opaque placeholder before forwarding, for later outbound resolution. Default `false`.
- `placeholder_ttl` (string) — how long the real token is retained. Default `1h`.

## `lineage-telemetry`

Emits two facts-only OpenTelemetry spans per HTTP exchange — a request
span on sight and a response span at stream end, paired by
`lineage.exchange.id` — carrying direction, protocol, endpoints, outcome
and, optionally, the parsed payload. Cross-pod parenting rides one
`tracestate` member, `lineage-parent`; a request that arrives with no
valid `traceparent` is forwarded with one naming the request span, and a
valid one is never modified. The wire format is
[lineage-wire-contract.md](./lineage-wire-contract.md). Place it after
the protocol parsers (declared in `RequiresAny`) and after
`jwt-validation` when the principal facts are wanted; a request-phase
denial by a plugin ordered before it emits no spans.

- `otel_endpoint` (string) — OTLP gRPC target: `host:port`, `http://host:port` or `https://host:port`; any other scheme is refused. Default `localhost:4317`.
- `otel_tls` (bool) — dial the collector with TLS, verified against the system roots or `otel_ca_file`. An `https://` endpoint implies it; `https://` with `otel_tls: false`, and `http://` with `otel_tls: true` or `otel_ca_file`, are refused. A plaintext dial to a non-loopback collector is allowed and logged as a WARN at start. Default `false`.
- `otel_ca_file` (string) — PEM bundle to verify the collector's certificate against (a private CA, e.g. cert-manager issued). Implies `otel_tls`; with an explicit `otel_tls: false` it is refused; an unreadable file or one with no certificate refuses to start. Default: system roots.
- `capture_io` (bool) — attach the parsed request/response content as `input.value` / `output.value`. Default `false`.
- `max_payload_bytes` (int) — cap on those two values, cut on a UTF-8 boundary with a `…[truncated]` marker; `0` or unset takes the default, `-1` attaches whole, any other negative is refused at start. Default `4096`.
- `max_attr_bytes` (int) — cap on every variable-content string attribute (`url.path`, `lineage.peer.host`, `mcp.tool`, …) and the span name — except the two identity facts `lineage.self.id` and `lineage.self.namespace`, which are operator configuration and never truncated; same `0` / `-1` / negative semantics as `max_payload_bytes`. Default `256`.
- `mint_traceparent` (bool) — forward a `traceparent` naming this request span when the request carried no valid one; `false` = a pure observer that writes no `traceparent`. Default `true`.
- `bypass_paths` (`[]string`) — path globs (`path.Match`, query stripped, path normalized — the shared bypass matcher `jwt-validation` and `sparc` use) that produce no spans. Default `/.well-known/*`, `/healthz`, `/readyz`, `/health`. Setting either bypass key replaces its default list rather than extending it, as in `ibac` / `sparc` / `cpex`; an entry matching everything is refused at start.
- `bypass_hosts` (`[]string`) — outbound host globs (`path.Match`, port stripped, case folded) that produce no spans; ignored inbound, where `Host` is caller-controlled. Default `otel-collector`, `otel-collector.*`, `jaeger`, `jaeger.*`, `zipkin`, `zipkin.*`, `prometheus`, `prometheus.*`.
- `self_id` (string) — this workload's identity, emitted as `lineage.self.id` (a SPIFFE ID reduced to its last path segment); a blank value, or one with no non-empty `/`-segment (`/`), is refused at start.
- `self_id_file` (string) — read when `self_id` is empty. Until it is readable and carries an identity the plugin is not ready and skips every exchange (no span, no header), re-reading the file in the background while `/readyz` names it — the same handling `jwt-validation` gives this path, so a late Secret mount never fails the sidecar (a pod probing `/readyz` stays out of rotation until the file lands). Refused at start only when `self_id` is also empty. Default `/shared/client-id.txt`.
- `namespace` (string) — this workload's Kubernetes namespace (an RFC 1123 DNS label), emitted as `lineage.self.namespace` on every span: the other half of its identity, since `self_id` is the last segment of a SPIFFE ID and the same segment in two namespaces is two workloads. **Required** (or `namespace_file`) — absent, blank, or not a DNS label is refused at start; never derived from the SPIFFE path. The attach kit writes its `NAMESPACE`; a sidecar older than this key rejects a config that carries it, so image and config flip together.
- `namespace_file` (string) — read once at start when `namespace` is empty; meant for `/var/run/secrets/kubernetes.io/serviceaccount/namespace`, the file the kubelet projects from the pod's own metadata — the one source that is right in every copy of a ConfigMap shared across namespaces (the platform's `authbridge-runtime-config`), where an inline literal would be wrong everywhere but one. Absent, blank, or not a DNS label refuses at start; no default, no poller.

## `litellm-budget-track`

Keeps the daily spend ledger and enforces a spend budget, from the cost record
`inference-parser` publishes. Full details in
[litellm-budgettrack-plugin.md](./litellm-budgettrack-plugin.md).

**Provider-specific:** `x-litellm-response-cost` is emitted only by
[LiteLLM](https://docs.litellm.ai/), so this plugin works only when
LiteLLM is the inference provider in front of the model. Against a
provider that doesn't set the header (raw OpenAI, Ollama, vLLM, …), no
cost is ever accumulated and the budget never trips.

- `spend_file` (string) — path to the JSON spend ledger file; required. The ledger is a small JSON file the plugin creates and rewrites, holding the current UTC date plus the cumulative spend and call count for that day (it resets automatically at midnight UTC) — see [Ledger Format](./litellm-budgettrack-plugin.md#ledger-format).
- `max_budget` (float64) — daily budget in USD; required, must be > 0.
- **No rate options, and no pricing at all.** This plugin bills a figure it does not compute: `inference-parser` settles the cost and publishes the record, and this plugin adds the day's total, enforces the cap, and warns when the rate table disagrees with what the gateway charged. Rates live in the top-level [`pricing:`](pricing.md#overriding-in-config) section.
- **Requires `inference-parser` LATER in the chain** (`RequiresLater`). The response passes walk the chain in reverse, so the parser must sit at a higher index to fold each frame before this plugin settles the cost. A chain without it — or with it earlier — fails to build.

## `mcp-parser`

Parses MCP tool calls/results into `pctx.Extensions.MCP` for downstream
policy plugins.

This plugin makes no decisions of its own — it exists to feed others. The
plugins that consume `pctx.Extensions.MCP` are:

| Consumer | How it uses the MCP extension |
|---|---|
| [`ibac`](#ibac) | Reads the tool name and arguments to judge the call against user intent. Declares `mcp-parser` in `RequiresAny`. |
| [`sparc`](#sparc) | Extracts the tool name/arguments to reflect on, and returns clarifications as MCP results. Declares `mcp-parser` in `RequiresAny`; required in `enforcement: mcp` mode. |
| [`cpex`](#cpex) | Converts the parsed call/result into a CMF message for the `cmf.tool_pre_invoke` / `cmf.tool_post_invoke` hooks. Declares `mcp-parser` in `RequiresAny`. |
| [`opa`](#opa) | Exposes the parsed call as `input.mcp` for policy (add `mcp.params` to `include` for arguments). |

Place `mcp-parser` **before** these plugins on the outbound chain;
without it they see no MCP data and pass the traffic through
unclassified.

- `paths` (`[]string`) — URL path globs treated as MCP endpoints (for body-less transport detection: SSE GET, session-terminate DELETE). Default `["/mcp"]`.

## `opa`

Evaluates [OPA](https://www.openpolicyagent.org/docs) (Open Policy Agent)
policy bundles against inbound and outbound requests, using an embedded
OPA engine and four fixed decision paths. Full details in the plugin's
[README](../authlib/plugins/opa/README.md).

- `bundle_url` (string) — base URL of the Rossoctl Bundle Server, the in-cluster service that serves per-agent [OPA policy bundles](https://www.openpolicyagent.org/docs/management-bundles) keyed by SPIFFE ID (see [how it works](../authlib/plugins/opa/README.md#how-it-works)); required.
- `agent_id_file` (string) — path to the agent's client-ID file. Default `/shared/client-id.txt`.
- `agent_id` (string) — inline agent ID; overrides `agent_id_file` when set.
- `polling_min_delay` / `polling_max_delay` (int) — bundle polling interval bounds in seconds. Defaults 10 / 120.
- `include` (`[]string`) — optional field groups exposed in the OPA input document (e.g. `mcp.params`, `a2a.content`, `inference.messages`); default lean/empty.

## `sparc`

Pre-tool reflection: sends proposed tool calls to a
[SPARC reflection service](../sparc-service/README.md) — a companion
HTTP service wrapping the `SPARCReflectionComponent` from the
[agent-lifecycle-toolkit](https://pypi.org/project/agent-lifecycle-toolkit/)
(ALTK) package — and enforces the configured policy on the result. It
must be deployed once per cluster before enabling this plugin. Full
details in [sparc-plugin.md](./sparc-plugin.md).

- `reflector_endpoint` (string) — base URL of the SPARC reflection service (`{endpoint}/reflect`); required.
- `reflector_bearer` (string) — optional bearer token.
- `enforcement` (string) — `mcp` (gate outbound MCP `tools/call`, default) or `inference` (gate/rewrite LLM completions).
- `track` (string) — reflection track: `fast_track` (default), `slow_track`, `syntax`, `spec_free`, `transformations_only`.
- `timeout_ms` (int) — per-call timeout; values below 100 rejected. Default 30000.
- `on_reject_action` (string) — `observe` (log only), `reflect` (default, return clarification), or `deny` (hard block).
- `deny_score_threshold` (float64) — escalate a reject to hard deny when the grounding score is at or below this value. `0` disables escalation.
- `fail_policy` (string) — behavior when SPARC is unreachable: `open` (default, allow + record) or `closed` (block).
- `skip_tools` / `reflect_tools` (`[]string`) — tool-name globs to exclude from, or restrict, reflection.
- `bypass_hosts` / `bypass_paths` (`[]string`) — globs skipped without reflecting; default to Keycloak/SPIRE/otel/etc.

## `static-inject`

Swaps a placeholder credential for a real static credential on
outbound requests, so the workload never holds the real secret.

- `source` (string) — `secret_dir` (read one file per key from `secret_dir`) or `mappings` (inline map; tests/dev only).
- `secret_dir` (string) — directory of per-key credential files.
- `mappings` (`map[string]string`) — inline key-to-credential map; not for real secrets.
- `key_by` (string) — `host` (default, use the outbound destination host) or `static` (always use `key`).
- `key` (string) — lookup key used when `key_by=static`.
- `placeholder` (string) — if set, the inbound bearer must exactly equal this value before injection proceeds.
- `inject_header` (string) — header to inject the credential into. Default `Authorization` (writes `Bearer <value>`); any other value writes the raw credential and drops the inbound `Authorization` header.

## `session-budget`

Enforces per-session token, call-count, and duration budgets via Redis. Opt-in at build time (`-tags include_plugin_sessionbudget`). Full details in [session-budget-plugin.md](./session-budget-plugin.md).

- `redis_url` (string) — Redis/Valkey connection URL; required.
- `max_tokens` (int64) — cumulative token ceiling per session. `0` = no limit.
- `max_input_tokens` (int64) — per-kind ceiling for uncached prompt tokens. `0` = no limit.
- `max_cache_read_tokens` (int64) — per-kind ceiling for prompt tokens served from cache. `0` = no limit.
- `max_cache_write_tokens` (int64) — per-kind ceiling for prompt tokens written to cache. `0` = no limit.
- `max_output_tokens` (int64) — per-kind ceiling for generated completion tokens. `0` = no limit.
- `max_reasoning_tokens` (int64) — per-kind ceiling for reasoning-only output tokens (a subset of output). `0` = no limit.
- `max_calls` (int64) — max inference calls per session. `0` = no limit.
- `max_duration_seconds` (int64) — wall-clock session lifetime. `0` = no limit.
- `on_exceed` (string) — `deny` (default, block), `observe` (log only), or `pause` (HITL webhook approval).
- `pause_webhook` (string) — URL to POST for approval when `on_exceed=pause`. Required in pause mode.
- `pause_timeout` (string) — how long to wait for webhook response. Default `30s`.
- `pause_timeout_action` (string) — fallback on timeout/error: `deny` (default) or `allow`.
- `pause_grace_period` (string) — suppress repeated webhooks after approval. Default `5m`.
- `session_ttl_seconds` (int) — Redis key TTL; must be ≥ `max_duration_seconds` when the latter is set (enforced at Configure time). Default 7200.
- `refresh_interval` (string) — how often the local cache syncs from Redis. Default `5s`.
- `redis_unavailable` (string) — only `fail_open` (default) is implemented; `fail_closed` is rejected at Configure time.
- `default_session_fallback` (bool) — pool sessionless traffic into a shared `default` bucket. Single-workload only: one caller exhausting the budget denies the rest. Default `false`.

At least one of `max_tokens`, the five per-kind ceilings, `max_calls`, or
`max_duration_seconds` must be > 0, or Configure fails.

Cold-cache behavior is mode-dependent; see
[session-budget-plugin.md](session-budget-plugin.md#cold-cache-behavior)
for details.


## `token-broker`

Exchanges incoming tokens against a configured IdP through an external
token broker service, per host-based routing rules. An alternative to
[`token-exchange`](#token-exchange), not a complement — both replace the
outbound `Authorization` header, so use one or the other on a given
chain. Full details in [token-broker-plugin.md](./token-broker-plugin.md).

- `broker_url` (string) — base URL of the token broker service; required.
- `default_policy` (string) — behavior when no route matches: `passthrough` (default) or `broker`.
- `routes.file` (string) — path to a `routes.yaml` file; merged with inline rules.
- `routes.rules` (list) — inline route entries; each has:
  - `host` — glob pattern to match the target host.
  - `action` — `broker` (default) or `passthrough`.
  - `authorization_endpoint` / `token_endpoint` — per-route OAuth endpoint overrides sent to the broker.


## `token-exchange`

RFC 8693 outbound token exchange per route. Supports Keycloak, Entra
ID, Okta, and any RFC 8693-compliant IdP. For the `IdPProvider`
interface each IdP implements, see
[idp-plugin-contract.md](./idp-plugin-contract.md).

- `token_url` (string) — OAuth token endpoint; required unless derived from `provider` + `provider_url`(+`provider_realm`), or the deprecated `keycloak_url`/`keycloak_realm`.
- `provider` (string) — IdP selector for endpoint derivation and client auth: `keycloak`, `generic`.
- `provider_url` / `provider_realm` (string) — IdP base URL and realm/tenant, meaning varies by provider.
- `keycloak_url` / `keycloak_realm` (string) — deprecated aliases for `provider_url`/`provider_realm` with `provider=keycloak`.
- `default_policy` (string) — behavior when no route matches: `passthrough` (default) or `exchange` (empty-audience client-credentials) for hosts explicitly configured in `authproxy-routes`.
- `no_token_policy` (string) — behavior for outbound requests with no bearer token: `client-credentials`, `allow`, or `deny` (default).
- `identity.type` (string) — `spiffe` (JWT-SVID assertion) or `client-secret`; required.
- `identity.client_id` / `identity.client_id_file` — OAuth client ID, inline or from file (default `/shared/client-id.txt`).
- `identity.client_secret` / `identity.client_secret_file` — client secret, inline or from file (default `/shared/client-secret.txt`).
- `identity.jwt_audience` (string) — audience claim minted on the JWT-SVID assertion; required when `type=spiffe`.
- `identity.assertion_type` (string) — client-assertion URN: `jwt-spiffe` (default) or `jwt-bearer` (Okta).
- `routes.file` (string) — path to `routes.yaml`. Default `/etc/authproxy/routes.yaml`.
- `routes.rules` (list) — inline route entries (`host`, `target_audience`, `token_scopes`, `token_url`, `action`), combined with file-loaded routes.
- `audience_from_host` (bool) — derive audience from host for unrouted requests. Default `false`.
- `resolve_placeholders` (bool) — resolve an inbound placeholder-prefixed bearer to its real token before exchange; unresolvable placeholders are denied. Default `false`.

## `tool-prune`

Removes unused tool definitions from the outbound inference manifest, so
the tokens for tools an agent never calls are not billed on every turn.
The manifest is assembled by the client, so the proxy is the only place to
trim it without changing every client.

Requires `inference-parser` earlier in the chain, and must sit after any
body-reading plugin (it rewrites the request body). Declares
`WritesRequestBody` only, so response streaming is unaffected.

- `remove` (`[]string`) — tool names to delete from the manifest. The complete verdict: no learning, no state, no storage. Names absent from a given request are ignored. **An empty list is the off switch** — the plugin is inert until a name is added, which is how it ships in the local install.
- `paths` (`[]string`) — request paths to act on, matched exactly or by suffix. Defaults to `/v1/chat/completions`, `/v1/completions`, `/v1/messages`.
- **No rates and no pricing.** This plugin reduces tokens; pricing the reduction belongs to whoever owns cost. It publishes what it removed — tool names, byte delta, and whether the prune was applied or only measured — and reads the priced figure back off the cost record to fill its `$ saved` metric. A figure derived from the bundled table is still labelled `bundled`, and a model with no rate anywhere is still counted in a `requests unpriced` row rather than charged at another model's rate. There is still no output rate: pruning only shrinks the prompt.

  The saving has to be priced on the response side, not here: the dollar amount depends on which prompt-cache tier the removed tokens came out of — 1x, 1.25x or 0.1x of the same rate — and only the response reveals that. It is inherently a request-fact times a response-fact.

Generate the list from local transcripts with `abctl tools scan`, which
proposes only tools it recognises as Claude Code built-ins and never proposes
one it has seen called. `--days N` sets the recency window (30 by default) and
`--all` drops it; widening is the cautious direction, since a longer window
finds more tools in use and so proposes fewer for removal. With `--write` it
refuses when it observed no tool calls at all, because "tools you have not
called" would then mean every tool it knows. See
[`tool-prune-plugin.md`](./tool-prune-plugin.md) for the measure-then-enforce
rollout, the metrics readout, and what the saving does and does not change.


## Cost records

Moved. The `cost` session-event record — its fields, and the rule that nothing in
`avoided` is spend — is in **[`pricing.md`](pricing.md#cost-records)**.

Like `pricing:` below, cost is core rather than plugin: `costing.Settle` and the ledger,
aggregator and `/v1/usage` endpoint all live in `authlib/`, and a plugin only feeds them.

## `pricing:`

Moved. Model rates, gateway discounts, the shipped defaults and how to override them are
in **[`pricing.md`](pricing.md)**.

It lives there because `pricing:` is a top-level config section rather than a plugin — it
registers no plugin and this file catalogs "every plugin with a Go implementation that
calls `plugins.RegisterPlugin()`". The plugins that *consume* those rates
(`inference-parser`, `litellm-budget-track`, `tool-prune`) are still catalogued above.
