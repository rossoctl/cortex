# Plugin Catalog

Catalog of AuthBridge pipeline plugins — every plugin with a Go
implementation that calls `plugins.RegisterPlugin()`. For the config
convention, session-event contract, and lifecycle interfaces plugins
implement, see [`plugin-reference.md`](./plugin-reference.md). For
writing a new plugin, see [`plugin-tutorial.md`](./plugin-tutorial.md).

"Production ready?" reflects whether the plugin is carried by a shipped
profile in `authbridge/scripts/profile-tags` versus available only on
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
`?beta=true` path reports cache counts on `message_delta`, not `message_start`). It also
means cost can never be missing while token counts are present — previously the figure came
from `litellm-budget-track`, so a pipeline without that plugin showed tokens and no money,
with the same field silently meaning "modelled" rather than "authoritative" depending on
configuration.

The arithmetic and the gateway header semantics are in `authlib/costing`, not in the parser:
a provider-shaped body parser has no business knowing one gateway's header names. The result
is published as a cost record keyed `cost` on the session event (see
[Cost records](#cost-records)).

No configuration — no config struct, does not implement `Configurable`. Rates arrive by
injection from the top-level [`pricing:`](#pricing) section; with none configured the parser
still parses and reports the traffic as unpriced.

## `jwt-validation`

Validates inbound JWTs: signature via JWKS, issuer, and audience.

- `issuer` (string) — expected `iss` claim; required.
- `jwks_url` (string) — JWKS endpoint; derived from Keycloak URL/realm or issuer when omitted.
- `keycloak_url` / `keycloak_realm` (string) — used to derive `jwks_url` when omitted.
- `audience` (string) — expected `aud` claim; one of `audience` / `audience_file` / `audience_mode=per-host` required.
- `audience_file` (string) — file to read expected audience from. Default `/shared/client-id.txt`.
- `audience_mode` (string) — `static` (default) or `per-host` (derived from the `Host` header via waypoint routing).
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
- **No rate options, and no pricing at all.** This plugin bills a figure it does not compute: `inference-parser` settles the cost and publishes the record, and this plugin adds the day's total, enforces the cap, and warns when the rate table disagrees with what the gateway charged. Rates live in the top-level [`pricing:`](#pricing) section.
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
- `audience_from_host` (bool) — derive audience from host for unrouted requests (waypoint mode). Default `false`.
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

Every priced request publishes one record on its response session event, under the key
`cost`. One producer, one record, one number.

The key names the **concern, not the producer**. It used to be the producing plugin's name
(`litellm-budget-track`), which made moving costing to the component that actually knows the
token counts a breaking wire change — for live consumers and for every event already in a
session store. The framework set that precedent itself: `pipeline/context.go` publishes
`body-mutation` from the core, "because a switch of plugin names in a future refactor
shouldn't break operators' dashboards". The legacy key is still written and still read, and
comes out a release after the rename ships.

| field | meaning |
|---|---|
| `cost_usd` | what the request cost. `source` says whether the gateway reported it (`gateway-header`) or it was modelled from token counts (`usage-fallback`); `provenance` says how much to trust the rates behind a modelled figure |
| `settled` | a figure exists — **including a deliberate zero**, which is the gateway saying the call was free. Without it, "free" and "nobody priced this" were indistinguishable, and a cache hit got re-priced from the rate table |
| `prompt_usd` | the modelled cost of the prompt alone, tier-weighted. A breakdown, **not** a component of a sum: it is the table's figure even when `cost_usd` is the gateway's |
| `avoided[]` | cost that was **not** incurred, attributed per component, with `tokensAvoided`, `usd`, the `tier` it came out of, and two honesty flags: `estimated` (derived from a byte ratio, not a tokenizer) and `projected` (measured but never applied — that money *was* spent) |
| `daily_total_usd`, `daily_max_usd` | added by `litellm-budget-track` when it is in the pipeline; the budget's business, not the cost owner's |

**Nothing in `avoided` is spend.** No consumer may add it to a cost, a budget or a usage
total; a test in `authlib/usage` asserts the aggregator's totals are unchanged by its
presence. It is a container rather than a few flat fields because more counterfactuals are
coming — compaction, redaction, "what a cheaper model would have cost" — and as siblings of
`cost_usd` the record would become half-real and half-hypothetical, which is how someone
eventually sums two fields that must never be summed.

## `pricing:`

Top-level section owning every model rate in the process. One section rather than a
knob per plugin, so `tool-prune`, `litellm-budget-track`, `/v1/usage` and `abctl`
cannot disagree about what a request cost.

```yaml
pricing:
  # The bundled table ships vendor-list rates for the Claude families, generated
  # from LiteLLM's public price map. On by default: internal usage should work with
  # no setup. Set false to price only what you configure.
  bundled: true
  endpoints:
    - hosts: ["gw.internal"]       # host globs, port stripped; "*" or omitted = any
      models:
        "*claude-opus-*":          # model glob, matched case-insensitively
          input_cost_per_million: 3.80
          cache_write_cost_per_million: 4.75
          cache_read_cost_per_million: 0.38
          output_cost_per_million: 19.00
          above:                   # optional long-context override
            - prompt_tokens: 200000
              input_cost_per_million: 7.60
```

`hosts` is a LIST because gateways commonly share a rate card — two replicas, or a
service name and its external alias, bill identically, and repeating the whole models
block per host invites the two copies to drift. Each host becomes its own table row.

`multiplier` scales every rate that resolves for those hosts, including tiers and
long-context thresholds. Absent means 1.0. A multiplier-only endpoint needs no `models`
block. The factor is capped at 10, because `multiplier: 76` for `0.76` inflates every
figure a hundredfold and reads as plausible in a config file.

**Inspecting what is in effect.** No config file can answer this: the figures a request
is charged come from your `pricing:` section *plus* the table compiled into the binary
*plus* any shipped gateway discount. Two views:

```
abctl pricing                      every row, unscaled
abctl pricing --host <gateway>     what that endpoint is charged, discount applied
```

Both are served by `GET /pricing/table[?host=]` on the diagnostic listener, beside
`/config` and `/reload/status`.

**Rates are scoped per endpoint**, which a per-plugin table could not express: the
same model bills differently on a discounted gateway than on the vendor endpoint,
and only the request's target host distinguishes them.

### Pinning a gateway that bills below list

The bundled table ships vendor-list prices, so a gateway that bills below list is
overstated until Cortex knows the discount. **Most gateways bill a uniform fraction of
list, and for those the whole answer is one scalar:**

```yaml
pricing:
  endpoints:
    - hosts: ["my-gateway.example.com"]
      multiplier: 0.76        # a FRACTION of list, so 0.76 is a 24% discount
```

One number rather than twelve, and it **tracks upstream repricing**: the gateway's price
is derived from list, so refreshing the bundled table moves both together. A copied rate
card freezes today's numbers and goes stale silently.

Some gateways already have a discount shipped with Cortex and need no configuration at
all — `abctl pricing --host <gateway>` says which, and shows the rates in effect with
their provenance. The `multiplier` you set outranks any shipped one.

**Provenance decides before specificity, so a catch-all you configure beats a specific
rule Cortex ships.** These are not equivalent:

```yaml
pricing:
  endpoints:
    - hosts: ["*"]            # applies to EVERY endpoint, including ones with a
      multiplier: 0.9         # shipped discount — 0.9 replaces their 0.76
```

That is deliberate: a configured factor means an operator checked their bill, and a rule
compiled into a binary should never silently win over that. But it does mean a catch-all
written for one gateway quietly reprices the rest. Scope the `hosts` list unless you mean
every endpoint, and check the result with `abctl pricing --host <gateway>`.

To drop a shipped discount for an endpoint without scaling it, set `multiplier: 1.0`
explicitly — that is a configured rule of 1.0, which outranks the shipped factor and
leaves the rates at vendor list. Omitting `multiplier` does NOT do this; it leaves the
shipped rule in force.

**Measuring your gateway's factor.** You do not have to be told it — a LiteLLM gateway
reports what it charged, so the factor is one division:

```sh
# Non-streamed, so the gateway settles the cost before replying. A streamed response
# reports 0 in that header by design, which is why this cannot be learned from live
# agent traffic.
# --proto '=https' so a mistyped http:// URL fails instead of putting $KEY on the wire
# in cleartext.
curl -sD - -o /dev/null --proto '=https' "$GATEWAY/v1/messages" \
  -H "Authorization: Bearer $KEY" -H 'content-type: application/json' \
  -d '{"model":"claude-opus-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}' \
  | grep -i x-litellm-response-cost-original
```

Divide that by what the same token counts cost at list (`abctl pricing` shows the list
rates; the response's `usage` block gives the counts). Repeat for one more model: a single
factor across both means a uniform discount and `multiplier` is the whole answer, while
figures that disagree mean the prices are negotiated per model and need the `models`
block below.

Cortex checks this for you as traffic flows. When a non-streamed response carries a
settled cost that disagrees with the modelled figure by more than 5%, `litellm-budget-track`
warns once per endpoint and model with both numbers and the ratio — so a stale or missing
factor announces itself rather than quietly misreporting spend.

**Pinned rates are never scaled by a shipped multiplier.** If you pin per-model rates for
a host that also matches a discount Cortex ships, the shipped factor is dropped for that
host — your figures are already what the gateway charges, and scaling them again would
understate spend by the factor. `abctl pricing --host <gateway>` shows `multiplier 1` there
to confirm it. A multiplier you configure yourself does still apply on top of your own
rates, since asking for both is a thing an operator can legitimately mean.

Reach for per-model rates only when a gateway's prices are genuinely negotiated per
model rather than derived from list:

```yaml
pricing:
  endpoints:
    - hosts: ["litellm.internal*"]     # your gateway; ports are stripped before matching
      models:
        "*claude-opus-*":
          input_cost_per_million: 3.80
          cache_write_cost_per_million: 4.75
          cache_read_cost_per_million: 0.38
```

Everything else keeps resolving from the bundled table, so `api.anthropic.com` still
prices at vendor list while your gateway prices at yours. Check it took effect with
`abctl`: the cost total is annotated `[configured]` rather than `[bundled]`.

Rates on a gateway change on the order of months, which is why this is a static block
rather than something fetched. Asking the gateway for its own rates via LiteLLM's
`GET /model/info` was designed and prototyped and then dropped: it needed a virtual key
minted and mounted, an outbound dependency and a refresh loop, to save transcribing
three numbers. If your gateway's rates do change often, the resolution order is built
for it — see `ProvDiscovered` in `authlib/pricing`.

**Bundled rates are VENDOR LIST.** A gateway billing below list is *overstated*
until you pin it with a host-scoped entry, which outranks anything bundled. This is
the opposite direction from the hand-measured defaults it replaced, so an operator
carrying over an old correction should re-check its sign.

Resolution per `(endpoint, model)`: **provenance first** — `configured` beats
`discovered` beats `bundled` — then specificity within a level, endpoint axis
before model axis, exact before glob, longer glob before shorter.

**Both units are accepted per tier**; setting *both* for one tier fails startup
naming the tier, since they differ by 10^6 and silently picking a winner would
misprice by that factor with nothing in the readout to say which was honoured.

A request is priced only if **every tier that carried tokens had a rate**.
Otherwise it is reported *unpriced* — never under-priced — and named in
`/v1/usage`'s `unpricedBy` so the missing entry is nameable rather than merely
counted.

Regenerate the bundled table with `make pricing-table COMMIT=<litellm sha>`.
