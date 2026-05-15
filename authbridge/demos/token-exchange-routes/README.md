# Configuring Token Exchange Routes

This guide explains how AuthBridge's outbound `token-exchange` plugin
turns a workload's outbound HTTP requests into properly-audienced
tokens, **without** changing the agent's code. It covers both single-
and multi-target setups in one place — the difference is just how
many entries you put in the `authproxy-routes` ConfigMap.

This is a **configuration reference**, not an end-to-end demo.
Pair it with one of the deployment demos for a working stack:

- [`weather-agent/demo-ui-advanced.md`](../weather-agent/demo-ui-advanced.md)
  — agent + tool with token exchange, runs end-to-end.
- [`webhook/README.md`](../webhook/README.md) — manual webhook
  injection with the auth-target demo app.

## How outbound routing works

When AuthBridge's outbound chain runs, the `token-exchange` plugin
matches the request's HTTP `Host` header against entries in the
`authproxy-routes` ConfigMap. The first match wins. On a hit, the
plugin:

1. Reads the caller's `Authorization: Bearer <token>` header.
2. Performs an RFC 8693 token exchange against Keycloak with
   `audience=<route.target_audience>` and `scope=<route.token_scopes>`,
   using the workload's own client credentials as the authenticated
   client.
3. Replaces the request's `Authorization` header with the exchanged
   token before the request leaves the sidecar.

If no route matches, the plugin falls back to its `default_policy`
(`passthrough` by default — the original token is forwarded
unchanged; `exchange` would have it block any unrouted host). See
[`authbridge/docs/plugin-reference.md`](../../docs/plugin-reference.md)
for the full plugin contract.

The `Host` header value is what the HTTP client puts in the URL
hostname — for in-cluster calls this is typically a **short
Kubernetes Service name** (e.g., `weather-tool-mcp`), not the FQDN.

## ConfigMap shape

The plugin reads `routes.yaml` from a ConfigMap named
`authproxy-routes` in the same namespace as the workload. The file
is a YAML list of route entries:

```yaml
# authproxy-routes ConfigMap, key: routes.yaml
- host: "weather-tool-mcp"
  target_audience: "weather-tool"
  token_scopes: "openid weather-tool-aud"
```

Apply with:

```bash
kubectl create configmap authproxy-routes \
  -n team1 \
  --from-file=routes.yaml=routes.yaml \
  --dry-run=client -o yaml | kubectl apply -f -
```

Per-route fields:

| Field | Purpose |
|---|---|
| `host` | Glob pattern matched against the request's `Host` header. `filepath.Match` semantics (`*`, `?`, `[...]`). |
| `target_audience` | OAuth audience to request from Keycloak. Must exist as a Keycloak client. |
| `token_scopes` | Space-separated list of scopes to request. Each scope must be assigned (default or optional) to the workload's own Keycloak client. |

## Single-target example

The simplest case — one workload with one outbound target.

`authproxy-routes` (one entry):

```yaml
- host: "weather-tool-mcp"
  target_audience: "weather-tool"
  token_scopes: "openid weather-tool-aud"
```

Keycloak needs:

- A client with `clientId: "weather-tool"` (the target audience).
- Client scope `weather-tool-aud` set to **optional** on the
  agent's own Keycloak client (so it can request the scope at
  exchange time).
- The `weather-tool-aud` scope's audience mapper set to add
  `"weather-tool"` to the resulting token's `aud` claim.

When the agent makes any HTTP call to `http://weather-tool-mcp/...`,
AuthBridge intercepts it, runs the exchange, and replaces the
`Authorization` header. The agent's source code never knows token
exchange happened.

## Multi-target example

Same shape, just multiple entries — `authproxy-routes` is a single
list, evaluated top-to-bottom. The first matching `host` wins, so
order more-specific patterns before broader ones.

`routes.yaml` in this directory shows the canonical multi-target
example with three target audiences:

```yaml
- host: "target-alpha-service**"
  target_audience: "target-alpha"
  token_scopes: "openid target-alpha-aud"

- host: "target-beta-service**"
  target_audience: "target-beta"
  token_scopes: "openid target-beta-aud"

- host: "target-gamma-service**"
  target_audience: "target-gamma"
  token_scopes: "openid target-gamma-aud"
```

Each target audience requires its own Keycloak client + audience
scope, and the agent's own Keycloak client needs each
`*-aud` scope assigned (optional).

## Mixing exchange and passthrough

The default policy when no route matches is `passthrough`. That's
what makes mixed deployments natural — for example, an agent that
calls one tool with token exchange and a public LLM endpoint
without:

```yaml
# Match: token-exchange runs.
- host: "github-tool-mcp"
  target_audience: "github-tool"
  token_scopes: "openid github-tool-aud"

# No entry for ollama / openai / etc. — outbound passthrough applies.
```

Switching the default to `exchange` (so unrouted hosts are
**rejected** with 503 instead of forwarded) is a per-plugin
setting:

```yaml
# pipeline.outbound.plugins[*].config (in authbridge-runtime-config)
- name: token-exchange
  config:
    default_policy: "exchange"
    routes:
      file: "/etc/authproxy/routes.yaml"
```

Use this when the workload should never reach an unauthenticated
upstream — typical for tool-only namespaces.

## Glob pattern tips

- `*` matches any sequence of non-`/` characters within a path
  segment. For Host headers (no `/`) it effectively matches
  anything.
- `**` is **not** a special pattern in `filepath.Match` — it's
  the same as `*`. Multi-segment matches just need a single `*`.
- `host: "*-tool-mcp"` matches `weather-tool-mcp`, `github-tool-mcp`,
  etc.
- `host: "internal-*"` matches any short service name starting with
  `internal-`.
- Exact-match host strings (no glob characters) are valid and the
  cheapest at runtime.

## Troubleshooting

**Symptom:** `503` from the outbound side / `default_policy=exchange`
rejected the call.
- Check the `Host` header the agent actually sent (`kubectl logs`
  on the AuthBridge sidecar — `authbridge-proxy` in proxy-sidecar
  mode, `envoy-proxy` in envoy-sidecar mode).
- Confirm the host string in `authproxy-routes` matches that header.
- Glob mismatch is the most common cause; try the literal short
  service name first.

**Symptom:** Token exchange returns `400 invalid_scope` from Keycloak.
- The scope in `token_scopes` is not assigned to the agent's own
  Keycloak client. Add it as an optional client scope.

**Symptom:** Target service rejects the exchanged token with
`aud` mismatch.
- The target's expected audience doesn't match `target_audience` in
  the route. Check the target's Keycloak client (its `clientId`
  determines the audience clients can request).

**Symptom:** Routes update doesn't take effect.
- The `token-exchange` plugin reads `routes.yaml` once at Configure
  time. After editing the ConfigMap, restart the workload pod
  (`kubectl rollout restart deployment/<name> -n <ns>`) so the
  sidecar picks up the new content.

## See also

- [`authbridge/docs/plugin-reference.md`](../../docs/plugin-reference.md)
  — full `token-exchange` plugin reference.
- [`weather-agent/demo-ui-advanced.md`](../weather-agent/demo-ui-advanced.md)
  — end-to-end demo that exercises the single-target route pattern.
- [`webhook/README.md`](../webhook/README.md) — webhook injection
  walkthrough that the routes here plug into.
