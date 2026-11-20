# Plugin Catalog

This page lists the Cortex pipeline plugins. Each section links the plugin's own
doc, which holds its configuration and behavior.

## Plugins on a laptop

A local install runs four plugins on the outbound pipeline: `inference-parser`,
`mcp-parser`, `a2a-parser` and `tool-prune`. `tool-prune` does nothing until its
`remove` list names a tool. Cortex also adds one [`script`](#script) slot at the
end of the outbound pipeline. That slot runs each executable file in
`~/.cortex/plugins`, and it does nothing while the folder is empty.

The installed binary carries more plugins: the `full` profile. A plugin runs only
when the pipeline names it. To turn a plugin on, use one of these:

- `agentop script install <name>` adds a shipped script plugin.
  `agentop script list` shows them. See
  [`agentop script`](../cmd/agentop/README.md#installing-a-bundled-script-plugin-agentop-script).
- `agentop server add` and `agentop server use` turn on
  [`inference-router`](#inference-router).
- Edit `~/.cortex/config.yaml`. Cortex reloads a pipeline change, so you do not
  restart it.

This is the `pipeline:` block that `cortex --local --write-config` writes to
`~/.cortex/config.yaml`, without its comments:

```yaml
pipeline:
  outbound:
    plugins:
      - name: inference-parser
      - name: mcp-parser
      - name: a2a-parser
      - name: tool-prune
        on_error: enforce
        config:
          remove: []
```

## Plugins

"Where" tells where the plugin is useful:

- **Laptop**: a local install, from `scripts/install.sh`.
- **Kubernetes**: the AuthBridge sidecar that the operator injects into a pod.
- **Both**: either of the two.

"Direction" is inbound (a caller sends to this agent) or outbound (this agent
sends to a callee). "Both" means that the plugin runs on the two pipelines.

| Name | Description | Where | Direction |
|------|-------------|-------|-----------|
| [`a2a-parser`](#a2a-parser) | Reads A2A messages into `pctx.Extensions.A2A` for the plugins after it. | Both | Both |
| [`evolve`](#evolve) | Adds the guidelines that [altk-evolve](https://github.com/AgentToolkit/altk-evolve) learned from earlier sessions to a coding agent's requests, and sends finished sessions to it to learn from. | Laptop | Outbound |
| [`context-guru`](#context-guru) | Makes the outbound LLM request context smaller before Cortex sends it. | Both | Outbound |
| [`cpex`](#cpex) | Runs [CPEX](https://github.com/contextforge-org/cpex) 0.3 policy plugins (JWT identity, OAuth delegation, Cedar, CEL, OPA) as one pipeline step. | Kubernetes | Outbound |
| [`ibac`](#ibac) | An LLM judge compares each outbound tool call with the user's intent. | Kubernetes | Outbound |
| [`inference-parser`](#inference-parser) | Reads LLM requests and responses into `pctx.Extensions.Inference`, and prices each response. | Both | Outbound |
| [`inference-router`](#inference-router) | Sends the new sessions of a coding agent to the inference server that you chose for it. | Laptop | Outbound |
| [`jwt-validation`](#jwt-validation) | Checks the signature, issuer and audience of inbound JWTs. | Kubernetes | Inbound |
| [`lineage-telemetry`](#lineage-telemetry) | Sends two OpenTelemetry lineage spans for each HTTP exchange. | Kubernetes | Both |
| [`litellm-budget-track`](#litellm-budget-track) | Keeps a daily spend ledger and refuses requests over the daily budget. | Both | Both |
| [`mcp-parser`](#mcp-parser) | Reads MCP tool calls and results into `pctx.Extensions.MCP`. | Both | Outbound |
| [`opa`](#opa) | Applies [OPA](https://www.openpolicyagent.org/docs) policy to inbound and outbound requests. | Kubernetes | Both |
| [`script`](#script) | Runs the scripts in `~/.cortex/plugins` as pipeline steps. | Laptop | Outbound |
| [`session-budget`](#session-budget) | Applies token, call and duration limits to each session, with Redis. | Kubernetes | Outbound |
| [`sparc`](#sparc) | Sends each proposed tool call to a reflection service, and blocks a call that has no grounds. | Kubernetes | Outbound |
| [`static-inject`](#static-inject) | Replaces a placeholder credential with a real static credential on outbound requests. | Kubernetes | Outbound |
| [`token-broker`](#token-broker) | Exchanges tokens with an IdP through a token broker service. | Kubernetes | Outbound |
| [`token-exchange`](#token-exchange) | Does an RFC 8693 token exchange for each outbound route. | Kubernetes | Outbound |
| [`token-exchange-aiac`](#token-exchange-aiac) | Does an outbound token exchange with no routes. The destination host gives the audience. | Kubernetes | Outbound |
| [`tool-prune`](#tool-prune) | Removes the tool definitions that its `remove` list names from inference requests. | Both | Outbound |

## How plugins are built

Each plugin is opt-in at build time, with `-tags include_plugin_<name>`. A build
with no tags registers no plugin, and it refuses each config that names one.
[`scripts/profile-tags/profiles.go`](../scripts/profile-tags/profiles.go) tells
which shipped artifact carries which plugin.

## `a2a-parser`

Gives the A2A method, session ID, message parts and role to the plugins after it.
See [`a2a-parser-plugin.md`](a2a-parser-plugin.md).

## `evolve`

Connects coding agents to [altk-evolve](https://github.com/AgentToolkit/altk-evolve), a service
that learns guidelines from agent sessions, through its stock MCP server. It adds the
guidelines relevant to a session's task to that session's requests, and sends a session to
altk-evolve once it goes quiet. By default, learned guidelines are served only after a person
reviews them. Fails open. See [`evolve-plugin.md`](evolve-plugin.md).

## `context-guru`

Compacts outbound LLM requests. No shipped build carries it.
See [`context-guru-plugin.md`](context-guru-plugin.md).

## `cpex`

Needs the `authbridge-cpex` image. The laptop cpex build is removed: for laptop
policy, see [`praxis-policy.md`](praxis-policy.md). See [`cpex-plugin.md`](cpex-plugin.md).

## `ibac`

Uses an OpenAI-compatible LLM judge to compare tool calls with the inbound user
intent. See [`ibac-plugin.md`](ibac-plugin.md).

## `inference-parser`

Gives each LLM request and response to the policy plugins, and makes the cost
record. See [`inference-parser-plugin.md`](inference-parser-plugin.md).

## `inference-router`

Keeps each session on the server that it started on. `agentop server` manages it.
See [`inference-router-plugin.md`](inference-router-plugin.md).

## `jwt-validation`

Checks inbound JWTs against a JWKS.
See [`jwt-validation-plugin.md`](jwt-validation-plugin.md).

## `lineage-telemetry`

Sends a request span and a response span for each exchange, with parents across
pods. See [`lineage-telemetry-plugin.md`](lineage-telemetry-plugin.md).

## `litellm-budget-track`

Adds the cost record of each response to a daily ledger, and enforces a budget.
See [`litellm-budget-track-plugin.md`](litellm-budget-track-plugin.md).

## `mcp-parser`

Gives each MCP tool call and result to `ibac`, `sparc`, `cpex` and `opa`.
See [`mcp-parser-plugin.md`](mcp-parser-plugin.md).

## `opa`

Runs OPA policy bundles in an embedded engine.
See [`opa-plugin.md`](opa-plugin.md).

## `script`

Runs small Python or shell scripts that can read, change or block traffic. Laptop
only. See [`script-plugin.md`](script-plugin.md).

## `session-budget`

Keeps session limits in a shared Redis. No shipped build carries it.
See [`session-budget-plugin.md`](session-budget-plugin.md).

## `sparc`

Needs the SPARC reflection service in the cluster.
See [`sparc-plugin.md`](sparc-plugin.md).

## `static-inject`

Keeps the real secret out of the workload.
See [`static-inject-plugin.md`](static-inject-plugin.md).

## `token-broker`

Replaces the outbound `Authorization` header. Use it or `token-exchange` on a
pipeline, not both. See [`token-broker-plugin.md`](token-broker-plugin.md).

## `token-exchange`

Works with Keycloak, Entra ID, Okta and other RFC 8693 IdPs.
See [`token-exchange-plugin.md`](token-exchange-plugin.md).

## `token-exchange-aiac`

Lets Keycloak decide which hosts are targets. Use it or `token-exchange`, not
both. See [`token-exchange-aiac-plugin.md`](token-exchange-aiac-plugin.md).

## `tool-prune`

Rewrites the request body, so it comes after each body reader.
See [`tool-prune-plugin.md`](tool-prune-plugin.md).

## See also

- [`pricing.md#cost-records`](pricing.md#cost-records): the `cost` session-event record.
- [`pricing.md#overriding-in-config`](pricing.md#overriding-in-config): model rates and the `pricing:` section.
- [`plugin-reference.md`](plugin-reference.md): the contract that each plugin implements.
- [`plugin-tutorial.md`](plugin-tutorial.md): how to write a new plugin.
