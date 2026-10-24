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
| [`cpex`](#cpex) | APL DSL + named [CPEX](https://github.com/contextforge-org/cpex) 0.3 plugins (JWT identity, OAuth delegation, Cedar, CEL, OPA) over a single chain step. | Opt-in | Outbound | No |
| [`ibac`](#ibac) | LLM-judge intent-based access control for outbound tool calls. | Alpha | Outbound | No |
| [`inference-parser`](#inference-parser) | Parses LLM completions into `pctx.Extensions.Inference`. | Alpha | Outbound | No |
| [`inference-router`](#inference-router) | Sends each coding agent's new sessions to the inference server chosen for it; a session stays on the server it started on. | Alpha | Outbound | No |
| [`jwt-validation`](#jwt-validation) | Inbound JWT validation (signature, issuer, audience) against JWKS. | Ready | Inbound | YES |
| [`lineage-telemetry`](#lineage-telemetry) | Emits two facts-only OTel lineage spans per HTTP exchange, parented across pods through one `tracestate` member. | Alpha | Both | No |
| [`litellm-budget-track`](#litellm-budget-track) | Accumulates the cost record `inference-parser` settles (authoritative where LiteLLM's cost header is present) and enforces a daily budget limit. Place on whichever chain carries LLM traffic — inbound when fronting the LLM endpoint, outbound when hosting an agent via `agentop exec`. | Alpha | Both | No |
| [`mcp-parser`](#mcp-parser) | Parses MCP tool calls/results into `pctx.Extensions.MCP`. | Beta | Outbound | No |
| [`opa`](#opa) | [OPA](https://www.openpolicyagent.org/docs) policy enforcement for inbound and outbound requests. | Alpha | Both | No |
| [`sparc`](#sparc) | Pre-tool reflection: blocks ungrounded/hallucinated tool calls. | Alpha | Outbound | No |
| [`static-inject`](#static-inject) | Swaps a placeholder credential for a real static credential on outbound requests. | Alpha | Outbound | No |
| [`script`](#script) | Runs the scripts in `~/.cortex/plugins` as pipeline steps, so a small Python or shell script can read, change or block requests and responses. Laptop only. | Alpha | Outbound | No |
| [`session-budget`](#session-budget) | Enforces per-session token, call, and duration budgets via Redis. | Alpha | Outbound | No |
| [`token-broker`](#token-broker) | Exchanges incoming tokens against a configured IdP via a broker service. | Alpha | Outbound | No |
| [`token-exchange`](#token-exchange) | RFC 8693 outbound token exchange per route. | Ready | Outbound | YES |
| [`tool-prune`](#tool-prune) | Removes unused tool definitions from inference requests. | Alpha | Outbound | No |

## `a2a-parser`

Parses A2A JSON-RPC 2.0 request bodies into `pctx.Extensions.A2A`
(method, session ID, message parts, role) for downstream guardrails.

Full reference: [`a2a-parser-plugin.md`](a2a-parser-plugin.md).

## `context-guru`

Compacts an agent's outbound LLM request context before forwarding,
using the embedded context-guru engine. `OnResponse` is currently a
pass-through; model-driven expand/restore is a later integration.
Opt-in at build time (`-tags include_plugin_contextguru`) because its
engine pulls a large transitive dependency set.

Full reference: [`context-guru-plugin.md`](context-guru-plugin.md).

## `cpex`

Bridges AuthBridge hooks to the [CPEX](https://github.com/contextforge-org/cpex)
framework (a policy enforcement runtime for AI agents): an APL DSL plus named
CPEX policy plugins (JWT identity, OAuth delegation, Cedar, CEL, OPA).
Requires a cpex build of `cortex` — the `authbridge-cpex` image or `make
cortex-cpex` (`-tags cpex`, `CGO_ENABLED=1`, links `libcpex_ffi.a` from the
pinned CPEX 0.3.1 release). The CPEX policy
uses 0.3's schema; a 0.2.x policy fails at boot. Full details in [cpex-plugin.md](./cpex-plugin.md);
see also the plugin's [README](../core/plugins/cpex/README.md).

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

## `inference-parser`

Parses outbound LLM inference requests/responses into `pctx.Extensions.Inference` for
downstream policy plugins, **and prices the finished response** — it is the one place
tokens become dollars.

Full reference: [`inference-parser-plugin.md`](inference-parser-plugin.md).

## `inference-router`

Sends a coding agent's new sessions to the inference server chosen for it — a
LiteLLM gateway or a local model server such as Ollama, with its own URL and key —
and keeps every session on the
server it started on, wherever the agent addressed its requests: its provider,
Anthropic, OpenCode Zen or Bob's gateway, so the agent needs no setting of its own
beyond going through the proxy. A `sessions:` entry moves one running session to
another server, dropping the reasoning only its old server can read. Routing is opt-in per agent: an agent not listed
under `agents` is not routed, and its requests go where it sent them, keys untouched. Manage it
with [`agentop server`](../cmd/agentop/README.md#choosing-an-inference-server-agentop-server);
the config stays hand-editable. Built for the laptop: it needs a listener that
honors a redirect, so `cortex-envoy` and a config with an `mtls:` block refuse it
at startup, and in-cluster use has not been examined.

Full reference: [`inference-router-plugin.md`](inference-router-plugin.md).

## `jwt-validation`

Validates inbound JWTs: signature via JWKS, issuer, and audience.

Full reference: [`jwt-validation-plugin.md`](jwt-validation-plugin.md).

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

Full reference: [`lineage-telemetry-plugin.md`](lineage-telemetry-plugin.md).

## `litellm-budget-track`

Keeps the daily spend ledger and enforces a spend budget, from the cost record
`inference-parser` publishes.

**Provider-specific:** `x-litellm-response-cost` is emitted only by
[LiteLLM](https://docs.litellm.ai/), so the *authoritative* cost figure — and
the drift check against it — exists only when LiteLLM is the inference
provider in front of the model. Against a provider that doesn't set the
header, `inference-parser` prices token usage from the [`pricing:`](pricing.md)
table and this plugin accumulates the modelled figure.

Full reference: [`litellm-budget-track-plugin.md`](litellm-budget-track-plugin.md).

## `mcp-parser`

Parses MCP tool calls/results into `pctx.Extensions.MCP` for downstream
policy plugins.

Full reference: [`mcp-parser-plugin.md`](mcp-parser-plugin.md).

## `opa`

Evaluates [OPA](https://www.openpolicyagent.org/docs) (Open Policy Agent)
policy bundles against inbound and outbound requests, using an embedded
OPA engine and four fixed decision paths. Full details in the plugin's
[README](../core/plugins/opa/README.md).

Full reference: [`opa-plugin.md`](opa-plugin.md).

## `sparc`

Pre-tool reflection: sends proposed tool calls to a
[SPARC reflection service](../deploy/sparc-service/README.md) — a companion
HTTP service wrapping the `SPARCReflectionComponent` from the
[agent-lifecycle-toolkit](https://pypi.org/project/agent-lifecycle-toolkit/)
(ALTK) package — and enforces the configured policy on the result. It
must be deployed once per cluster before enabling this plugin. Full
details in [sparc-plugin.md](./sparc-plugin.md).

## `static-inject`

Swaps a placeholder credential for a real static credential on
outbound requests, so the workload never holds the real secret.

Full reference: [`static-inject-plugin.md`](static-inject-plugin.md).

## `session-budget`

Enforces per-session token, call-count, and duration budgets via Redis. Opt-in at build time (`-tags include_plugin_sessionbudget`). Full details in [session-budget-plugin.md](./session-budget-plugin.md).

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

## `token-exchange`

RFC 8693 outbound token exchange per route. Supports Keycloak, Entra
ID, Okta, and any RFC 8693-compliant IdP. For the `IdPProvider`
interface each IdP implements, see
[idp-plugin-contract.md](./idp-plugin-contract.md).

Full reference: [`token-exchange-plugin.md`](token-exchange-plugin.md).

## `script`

Runs every executable file in `~/.cortex/plugins` as a pipeline step, in
file-name order: each is a long-lived process that answers one JSON line per
request. A script reads the request (method, host, path, headers without
credentials, body, and the parsers' fields) and may change its headers or body,
or deny it. A new script runs in **shadow mode**, recording what it would do,
until its handshake says `enforce`. A script that crashes, times out or answers
nonsense lets the request through, with the error on its timeline row, unless
the slot sets `fail_closed: true`. Under `fail_closed`, a script that did not
start makes the slot refuse every request with a 502 until a reload in which it
starts.

On a local install the preset appends one `script` entry to the end of the
outbound chain, so a file dropped into the folder runs with no YAML edit;
adding, editing or removing a script reloads the pipelines. List `script`
yourself, with `config.dir`, to place a second slot elsewhere — between two Go
plugins, for example, as long as no body reader follows it: a slot is a request
writer wherever scripts run, empty or not, so the chain is refused at build
time rather than by the first script dropped into it. Anywhere but a local
install a `script` entry loads nothing, so a config naming it cannot run
commands in a sidecar.

A chain whose `script` entries all name a `dir` still gets the default slot,
inserted just before the first of them.

A script may also read each response, and, if its handshake declares
`rewrite_response`, change or block it. Response readers run once a response
is complete and keep streaming; a response rewriter holds every streamed
response in its slot's pipeline back until it is complete.

Declares what its loaded scripts need, on top of the request-writer floor:
response streaming is unaffected unless a loaded script rewrites responses.

| Field | Default | Meaning |
|---|---|---|
| `dir` | (empty) | Folder inside `~/.cortex/plugins` this slot runs; empty is that folder itself |
| `timeout` | `2s` | How long one script may take to answer one hook |
| `fail_closed` | `false` | Deny a request when an enforcing script errors on it, and every request while a script in the slot did not start |

Full guide, including the Python SDK and replaying a script over a recorded
session: [`script-plugin.md`](script-plugin.md).

## `tool-prune`

Removes unused tool definitions from the outbound inference manifest, so
the tokens for tools an agent never calls are not billed on every turn.
The manifest is assembled by the client, so the proxy is the only place to
trim it without changing every client.

Requires `inference-parser` earlier in the chain, and must sit after any
body-reading plugin (it rewrites the request body). Declares
`WritesRequestBody` only, so response streaming is unaffected. **An empty
`remove` list is the off switch** — the plugin is inert until a name is added,
which is how it ships in the local install.

The saving has to be priced on the response side, not here: the dollar amount depends on which prompt-cache tier the removed tokens came out of — 1x, 1.25x or 0.1x of the same rate — and only the response reveals that. It is inherently a request-fact times a response-fact.

Generate the list from local transcripts with `agentop tools scan`, which
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

Like `pricing:` below, cost is core rather than plugin: `settle.Settle` and the ledger,
aggregator and `/v1/usage` endpoint all live in `core/`, and a plugin only feeds them.

## `pricing:`

Moved. Model rates, gateway discounts, the shipped defaults and how to override them are
in **[`pricing.md`](pricing.md#overriding-in-config)**.

It lives there because `pricing:` is a top-level config section rather than a plugin — it
registers no plugin and this file catalogs "every plugin with a Go implementation that
calls `plugins.RegisterPlugin()`". The plugins that *consume* those rates
(`inference-parser`, `litellm-budget-track`, `tool-prune`) are still catalogued above.
