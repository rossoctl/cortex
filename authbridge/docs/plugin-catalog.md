# Plugin Catalog

Catalog of AuthBridge pipeline plugins — every plugin with a Go
implementation that calls `plugins.RegisterPlugin()`. For the config
convention, session-event contract, and lifecycle interfaces plugins
implement, see [`plugin-reference.md`](./plugin-reference.md). For
writing a new plugin, see [`plugin-tutorial.md`](./plugin-tutorial.md).

"Production ready?" reflects whether the plugin is compiled into the
default build of `cmd/authbridge-proxy` / `cmd/authbridge-envoy` (opt-out
via `-tags exclude_plugin_<name>`) versus opt-in (`-tags
include_plugin_<name>`) or requiring a separate binary. It is a build-tag
signal, not a claim about test coverage or operational maturity.

## Plugins

"Direction" is inbound (caller → this agent) or outbound (this agent →
callee); "both" means the plugin evaluates on both pipelines. "Default
config?" marks whether the plugin is enabled in Rossoctl's default
AuthBridge pipeline YAML, not whether it is compiled into the binary
(see "Production ready?" above for that).

| Name | Description | Production ready? | Direction | Default config? |
|------|-------------|--------------------|-----------|------------------|
| [`a2a-parser`](#a2a-parser) | Parses A2A messages into `pctx.Extensions.A2A` for downstream plugins. | Beta | Inbound | No |
| [`context-guru`](#context-guru) | Compacts the outbound LLM request context before forwarding. | Coming Soon | Outbound | No |
| [`cpex`](#cpex) | APL DSL + named CPEX plugins (Cedar, PII, audit, …) over a single chain step. | Coming Soon | Outbound | No |
| [`ibac`](#ibac) | LLM-judge intent-based access control for outbound tool calls. | Alpha | Outbound | No |
| [`inference-parser`](#inference-parser) | Parses LLM completions into `pctx.Extensions.Inference`. | Alpha | Outbound | No |
| [`jwt-validation`](#jwt-validation) | Inbound JWT validation (signature, issuer, audience) against JWKS. | Ready | Inbound | YES |
| [`lineage-telemetry`](#lineage-telemetry) | Emits two facts-only OTel lineage spans per HTTP exchange, parented across pods through one `tracestate` member. | Alpha | Both | No |
| [`litellm-budget-track`](#litellm-budget-track) | Tracks `x-litellm-response-cost` and enforces a daily budget limit. | Alpha | Inbound | No |
| [`mcp-parser`](#mcp-parser) | Parses MCP tool calls/results into `pctx.Extensions.MCP`. | Beta | Outbound | No |
| [`opa`](#opa) | OPA policy enforcement for inbound and outbound requests. | Alpha | Both | No |
| [`sparc`](#sparc) | Pre-tool reflection: blocks ungrounded/hallucinated tool calls. | Alpha | Outbound | No |
| [`static-inject`](#static-inject) | Swaps a placeholder credential for a real static credential on outbound requests. | Alpha | Outbound | No |
| [`session-budget`](#session-budget) | Enforces per-session token, call, and duration budgets via Redis. | Alpha | Outbound | No |
| [`token-broker`](#token-broker) | Exchanges incoming tokens against a configured IdP via a broker service. | Alpha | Outbound | No |
| [`token-exchange`](#token-exchange) | RFC 8693 outbound token exchange per route. | Ready | Outbound | YES |

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

Bridges AuthBridge hooks to the CPEX framework: an APL DSL plus named
CPEX policy plugins (Cedar, PII, audit, …). Requires the separate
`authbridge-cpex` binary (`-tags cpex`, `CGO_ENABLED=1`, links a pinned
`libcpex_ffi.a`).

- `hooks.on_request` / `hooks.on_response` (`[]string`) — CPEX hook names to fire on each phase, in order.
- `config` (string) — inline CPEX runtime YAML (`plugins:`/`global:`/`plugin_settings:`); mutually exclusive with `config_file`.
- `config_file` (string) — path to a file with the CPEX runtime YAML; mutually exclusive with `config`.
- `fail_open` (bool) — allow traffic through if CPEX itself errors/panics. A CPEX policy *deny* is always honored regardless. Default `false`.
- `worker_threads` (int) — size of CPEX's tokio worker pool; `0` = automatic.
- `bypass_hosts` / `bypass_paths` (`[]string`) — globs skipped entirely (outbound only for hosts); default to Keycloak/SPIRE/observability infra.

## `ibac`

LLM-judge intent-based access control: judges outbound tool calls
against recorded inbound user intent.

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
`pctx.Extensions.Inference` for downstream policy plugins.

No configuration — no config struct, does not implement `Configurable`.

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
- `mint_traceparent` (bool) — forward a `traceparent` naming this request span when the request carried no valid one; `false` = a pure observer that writes no `traceparent`. Default `true`.
- `bypass_paths` (`[]string`) — path prefixes that produce no spans. Default `/.well-known/`, `/healthz`, `/readyz`, `/health`. Setting either bypass key replaces its default list rather than extending it, as in `ibac` / `sparc` / `cpex`; an entry matching everything is refused at start.
- `bypass_hosts` (`[]string`) — outbound host globs (`path.Match`, port stripped, case folded) that produce no spans; ignored inbound, where `Host` is caller-controlled. Default `otel-collector`, `otel-collector.*`, `jaeger`, `jaeger.*`, `zipkin`, `zipkin.*`, `prometheus`, `prometheus.*`.
- `self_id` (string) — this workload's identity, emitted as `lineage.self.id`.
- `self_id_file` (string) — read when `self_id` is empty; the plugin refuses to start if neither yields an identity. Default `/shared/client-id.txt`.

## `litellm-budget-track`

Tracks the `x-litellm-response-cost` response header and enforces a
daily spend budget.

- `spend_file` (string) — path to the JSON spend ledger file; required.
- `max_budget` (float64) — daily budget in USD; required, must be > 0.

## `mcp-parser`

Parses MCP tool calls/results into `pctx.Extensions.MCP` for downstream
policy plugins.

- `paths` (`[]string`) — URL path globs treated as MCP endpoints (for body-less transport detection: SSE GET, session-terminate DELETE). Default `["/mcp"]`.

## `opa`

Evaluates OPA policy bundles against inbound and outbound requests.

- `bundle_url` (string) — base URL of the Rossoctl Bundle Server; required.
- `agent_id_file` (string) — path to the agent's client-ID file. Default `/shared/client-id.txt`.
- `agent_id` (string) — inline agent ID; overrides `agent_id_file` when set.
- `polling_min_delay` / `polling_max_delay` (int) — bundle polling interval bounds in seconds. Defaults 10 / 120.
- `include` (`[]string`) — optional field groups exposed in the OPA input document (e.g. `mcp.params`, `a2a.content`, `inference.messages`); default lean/empty.

## `sparc`

Pre-tool reflection: sends proposed tool calls to a SPARC reflection
service and enforces the configured policy on the result.

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

Enforces per-session token, call-count, and duration budgets via Redis. Opt-in at build time (`-tags include_plugin_sessionbudget`).

- `redis_url` (string) — Redis/Valkey connection URL; required.
- `max_tokens` (int64) — cumulative token ceiling per session. `0` = no limit.
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

Cold-cache behavior is mode-dependent; see
[session-budget-plugin.md](session-budget-plugin.md#cold-cache-behavior)
for details.


## `token-broker`

Exchanges incoming tokens against a configured IdP through an external
token broker service, per host-based routing rules.

- `broker_url` (string) — base URL of the token broker service; required.
- `default_policy` (string) — behavior when no route matches: `passthrough` (default) or `broker`.
- `routes.file` (string) — path to a `routes.yaml` file; merged with inline rules.
- `routes.rules` (list) — inline route entries; each has:
  - `host` — glob pattern to match the target host.
  - `action` — `broker` (default) or `passthrough`.
  - `authorization_endpoint` / `token_endpoint` — per-route OAuth endpoint overrides sent to the broker.


## `token-exchange`

RFC 8693 outbound token exchange per route. Supports Keycloak, Entra
ID, Okta, and any RFC 8693-compliant IdP.

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
