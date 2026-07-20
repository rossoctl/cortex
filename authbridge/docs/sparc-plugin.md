# SPARC (Pre-Tool Reflection) Plugin

The `sparc` plugin verifies that an agent's proposed tool call is *grounded* in the
conversation and the available tool specifications before it executes — catching
**hallucinated / ungrounded arguments** (e.g. an invented transaction id) and inappropriate
tool selection. On a reflected reject it returns SPARC's clarification to the agent so the
agent re-asks the user; the bad call never runs.

SPARC is the `SPARCReflectionComponent` from the
[`agent-lifecycle-toolkit`](https://pypi.org/project/agent-lifecycle-toolkit/) (ALTK) Python
package, served over HTTP by the companion [SPARC reflection service](../sparc-service/README.md).
Because AuthBridge plugins are Go, this plugin calls the service over HTTP (the same shape
`ibac` uses for its judge). All enforcement policy lives in the plugin; the service only
returns SPARC's verdict.

It is **complementary to [`ibac`](ibac-plugin.md)**: SPARC verifies argument *grounding*;
IBAC verifies *intent alignment* (prompt-injection / exfiltration).

## Generic by design — works for any agent

SPARC's three inputs are collected from exactly what the agent produces, with no per-agent
wiring or out-of-band data:

- **conversation history, including the system prompt** — from the agent's LLM call captured
  by `inference-parser` (`pctx.Extensions.Inference.Messages`, every role preserved);
- **tool specifications in OpenAI function-calling format** — from the same call
  (`pctx.Extensions.Inference.Tools`);
- **the proposed tool call** — from the outbound MCP `tools/call` (`mcp` mode) or from the LLM
  response (`inference` mode).

If the conversation/tool context isn't available (no `inference-parser`, or session tracking
off), the plugin **skips** (`no_inference_context`) rather than reflect on partial data.

## Enforcement is format-aware

The verdict is returned to the agent in the shape it expects, selected by `enforcement`:

- **`mcp` (default — the rossoctl norm).** Gate the outbound MCP `tools/call`. The tool call
  comes from the MCP request; conversation + tool specs are correlated from the session's most
  recent inference request (robust to LLM streaming). On a reflected reject, return SPARC's
  clarification as a **JSON-RPC MCP tool result** (`Reject` + `Violation{Status:200, Body}`);
  the agent's MCP client consumes it like any tool output.
- **`inference` (for non-MCP agents).** Gate the agent's LLM response, where all three inputs
  are co-located. On a reflected reject, rewrite the completion (via `SetResponseBody`) so the
  assistant turn carries the clarification and the tool_call is dropped — norms-correct for any
  OpenAI-style agent.

## On-reject policy

When SPARC returns `reject`, `on_reject_action` decides, with optional score escalation:

| `on_reject_action` | Behavior | Session record |
|---|---|---|
| `observe` | Log only, let the call through (shadow). | `observe` / `reject_observed` |
| `reflect` (default) | Return SPARC's clarification to the agent (MCP result or rewritten completion). | `modify` / `reflected` |
| `deny` | Hard block (403 in `mcp` mode; refusal completion in `inference` mode). | `deny` / `blocked` |

When `deny_score_threshold > 0` and SPARC's `overall_avg_score` (normalized 0..1, higher = better
grounded) is `<=` it, any reject is escalated to `deny`.

> **`fail_policy` defaults to `open`** — when SPARC is unreachable, times out, or returns
> `decision=error`, the proposed tool call is allowed through (and recorded). This is deliberate
> and **diverges from `ibac`, which fails closed**: SPARC is a grounding *quality gate*, not an
> auth control, so reflector downtime shouldn't take the agent's tools offline. Set
> `fail_policy: closed` when you want tool execution gated on reflector availability (e.g. a
> high-assurance deployment where an unverified call is worse than a failed one). The same policy
> governs the case where the conversation/tool context is missing (no `inference-parser` event):
> `open` skips and records `no_inference_context`; `closed` blocks.

## Prerequisite: deploy the SPARC service

The plugin is a thin client — it calls the [SPARC reflection service](../sparc-service/README.md)
over HTTP. **Deploy that service once per cluster before enabling this plugin**, or every tool call
hits an unreachable reflector (and falls back to `fail_policy`). It's one command:

```bash
cd authbridge/sparc-service/deploy
export WX_API_KEY=... WX_PROJECT_ID=...   # or PROVIDER=ollama, openai, ...
make install                              # deploys into rossoctl-system by default
```

See [`sparc-service/deploy/README.md`](../sparc-service/deploy/README.md) for providers, the
local-image path (`make image install` on kind), and configuration. Point `reflector_endpoint`
below at the resulting service (`http://sparc-service.<namespace>.svc:8090`).

## Configuration

```yaml
pipeline:
  outbound:
    plugins:
      - name: sparc
        config:
          reflector_endpoint: "http://sparc-service.rossoctl-system.svc:8090"
          enforcement: "mcp"             # mcp | inference
          track: "fast_track"            # fast_track|slow_track|syntax|spec_free|transformations_only
          on_reject_action: "reflect"    # observe | reflect | deny
          deny_score_threshold: 0        # 0 disables; e.g. 2.0 → deny rejects scoring <= 2
          fail_policy: "open"            # open=allow+log on SPARC error; closed=block
          timeout_ms: 30000
          skip_tools: ["list_*"]         # tool-name globs NOT reflected on (e.g. read-only tools)
          reflect_tools: []              # if non-empty, ONLY these globs are reflected
```

| Field | Required | Default | Description |
|---|---|---|---|
| `reflector_endpoint` | Yes | — | Base URL of the SPARC service; the plugin POSTs to `{endpoint}/reflect`. |
| `reflector_bearer` | No | `""` | Bearer for the service. Empty for in-cluster unauthenticated calls. |
| `enforcement` | No | `mcp` | `mcp` (gate MCP tools/call, return MCP result) or `inference` (gate LLM response, rewrite completion). |
| `track` | No | `fast_track` | SPARC track. `fast_track` = hallucination + function-selection checks. |
| `on_reject_action` | No | `reflect` | `observe` \| `reflect` \| `deny` (see table above). |
| `deny_score_threshold` | No | `0` | Escalate a reject to `deny` when score `<=` this (`[0,1]`; `0` disables). |
| `fail_policy` | No | `open` | On SPARC unreachable / `decision=error`: `open` allows+logs; `closed` blocks. |
| `timeout_ms` | No | `30000` | Per-call reflection timeout. Rejected below `100`. Reflection is **out-of-band** (the plugin calls the SPARC service directly), so this is independent of the forward proxy's upstream timeout — raise it for slow/CPU-bound providers (e.g. an in-cluster Ollama). |
| `skip_tools` | No | — | Tool-name globs (`path.Match`) to NOT reflect on (e.g. trivial read tools). |
| `reflect_tools` | No | — | If non-empty, ONLY reflect tools matching these globs; all others skipped. |
| `bypass_hosts` / `bypass_paths` | No | built-in | Host / path globs skipped without reflecting. |

### Pipeline composition

`sparc` declares `RequiresAny: ["inference-parser", "mcp-parser"]`. For the richest, generic
reflection wire all three parsers:

```yaml
pipeline:
  inbound:
    plugins:
      - name: a2a-parser       # seeds the session with the user turn
      - name: jwt-validation
  outbound:
    plugins:
      - name: token-exchange
      - name: inference-parser # captures messages (incl. system) + tool specs
      - name: mcp-parser       # parses the tool call (mcp mode)
      - name: sparc
```

## Observability

`sparc` records a flat `Invocation` per call (action `allow`/`modify`/`deny`/`observe`,
`reason`, `tool`, `score`) **and** publishes the full structured verdict via the plugin-event
escape-hatch (`Extensions.Custom["sparc/event"]` → `SessionEvent.Plugins["sparc"]`), so abctl
and other consumers can render the decision, score, track, enforcement mode, and per-issue
explanations/corrections.

## Status codes & reasons

| Reason | HTTP / effect | Meaning |
|---|---|---|
| `sparc.reflected` | 200 MCP result / rewritten completion | A reject was reflected back to the agent as a clarification. |
| `sparc.blocked` | 403 (mcp) / refusal completion (inference) | A reject handled by `deny` (or escalated via score). |
| `sparc.reflector_unavailable` | 503 / refusal (closed); pass-through (open) | SPARC unreachable or `decision=error`, per `fail_policy`. |
| `sparc.no_inference_context` | 503 (closed); pass-through (open) | No conversation/tool context to ground against, per `fail_policy`. |

## Security / network posture

The reflection service is an **in-cluster backend** — same trust model as the authbridge session
API (`:9094`): no ingress, never exposed publicly. Its `/reflect` endpoint is unauthenticated by
default and runs outside the agent-side ambient mesh (`rossoctl.io/inject: disabled`), so it makes
its own egress to the LLM provider. Treat the cluster network boundary as the control: restrict
who can reach it with a `NetworkPolicy` (allow only the authbridge sidecars), since any pod that
can POST to `/reflect` can trigger LLM calls billed to the configured credentials. For an extra
hop, set `reflector_bearer` on the plugin and terminate it at the service (or a sidecar). The
service never echoes provider exception text back to callers; details stay in its logs.

## Limitations & non-goals
- Not an intent/auth control — use `ibac` + `token-exchange` routes / NetworkPolicy for that.
- No principal/identity input — SPARC reflects on `(messages, tool_specs, tool_calls)` only.
- `inference` mode can't see streamed tool_calls (use `mcp` mode for streaming MCP agents).
- Cost: one reflection round-trip per (non-skipped) tool call.

## See also
- [`sparc-service/README.md`](../sparc-service/README.md) — the reflection service.
- [`demos/finance-sparc/`](../demos/finance-sparc/README.md) — the end-to-end demo.
- [`ibac-plugin.md`](ibac-plugin.md) — the complementary intent control.

## Files
- `authlib/plugins/sparc/plugin.go` — config, gates, policy, structured event.
- `authlib/plugins/sparc/collect.go` — generic input collection.
- `authlib/plugins/sparc/respond.go` — MCP result + completion-rewrite responders.
- `authlib/plugins/sparc/reflector.go` — HTTP client to the service.
