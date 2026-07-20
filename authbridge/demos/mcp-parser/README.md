# MCP Parser Plugin Demo

This guide shows how to enable the `mcp-parser` plugin so AuthBridge
parses outbound MCP JSON-RPC requests and logs tool calls, resource
reads, and prompt invocations.

## Prerequisites

- A running Rossoctl cluster (Kind or OpenShift) with the Ansible installer
  completed. The mcp-parser plugin works in `envoy-sidecar` and
  `proxy-sidecar` modes on any cluster type.
- A namespace (e.g., `team1`) labeled with `rossoctl-enabled: "true"` for
  AuthBridge sidecar injection
- An MCP-based agent already deployed (e.g., the weather agent from
  [demo-ui-advanced](../weather-agent/demo-ui-advanced.md))

## How It Works

The `mcp-parser` plugin runs on the **outbound** pipeline. Agents use the
MCP protocol to call tools — these are outbound HTTP requests from the
agent to tool services. Inbound requests to the agent use A2A protocol,
not MCP.

The plugin:
1. Declares `BodyAccess: true`, which triggers body buffering in all
   listener modes
2. Parses the request body as JSON-RPC 2.0
3. Populates `pctx.Extensions.MCP` with the parsed method, tool name,
   resource URI, or prompt name
4. Returns `Continue` unconditionally (never rejects)

In envoy-sidecar mode, AuthBridge uses ext_proc `ModeOverride` to
dynamically request the body from Envoy when a POST request has a body.
GET/HEAD/OPTIONS/DELETE requests skip body buffering entirely (these
methods have no body).

### Envoy Configuration Requirement

The outbound ext_proc filter **must** have `allow_mode_override: true`
for dynamic body buffering to work. Without it, Envoy ignores the
`ModeOverride` and never sends the request body to authbridge.

```yaml
# In the outbound_listener's ext_proc filter:
- name: envoy.filters.http.ext_proc
  typed_config:
    "@type": type.googleapis.com/envoy.extensions.filters.http.ext_proc.v3.ExternalProcessor
    grpc_service:
      envoy_grpc:
        cluster_name: ext_proc_cluster
      timeout: 300s
    allow_mode_override: true          # Required for mcp-parser
    processing_mode:
      request_header_mode: SEND
      response_header_mode: SKIP
      request_body_mode: NONE
      response_body_mode: NONE
```

If you are using the operator v0.5.0-alpha.8+, this is already
set in the default `envoy-config` ConfigMap. For earlier versions, patch
the ConfigMap manually.

## Step 1: Patch the Runtime Config

The `authbridge-runtime-config` ConfigMap in the agent namespace contains
the `config.yaml` that AuthBridge reads at startup. Add the `pipeline`
section:

```bash
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: ConfigMap
metadata:
  name: authbridge-runtime-config
  namespace: team1
data:
  config.yaml: |
    mode: envoy-sidecar
    pipeline:
      inbound:
        plugins:
          - name: jwt-validation
            config:
              issuer: "http://keycloak.localtest.me:8080/realms/rossoctl"
      outbound:
        plugins:
          - name: token-exchange
            config:
              keycloak_url: "http://keycloak-service.keycloak.svc:8080"
              keycloak_realm: "rossoctl"
              identity:
                type: "spiffe"
          - mcp-parser
EOF
```

> **Note**: Per-plugin config is the only supported shape. Top-level
> `inbound:` / `outbound:` / `identity:` / `bypass:` blocks are no
> longer accepted. Defaults (`audience_file=/shared/client-id.txt`,
> `bypass_paths`=common probes, `jwt_svid_path=/opt/jwt_svid.token`,
> `client_id_file=/shared/client-id.txt`, `default_policy=passthrough`,
> `routes.file=/etc/authproxy/routes.yaml`) kick in for anything you
> omit. For `client-secret` identity type, swap `type: spiffe` to
> `type: client-secret` — the `client_secret_file` default activates
> automatically.

The `mcp-parser` is placed **after** `token-exchange` in the outbound
pipeline. This means:
- Token exchange runs first (injecting credentials for routed hosts)
- The MCP body is parsed after auth decisions are made
- Future policy plugins can read both token metadata and
  `pctx.Extensions.MCP`

## Step 2: Enable Debug Logging (Optional)

To see all MCP parsing output, set `LOG_LEVEL=debug` on the authbridge
container. The easiest way:

```bash
# If using the operator-injected sidecar, patch the ConfigMap:
kubectl patch configmap authbridge-config -n team1 \
  --type merge -p '{"data":{"LOG_LEVEL":"debug"}}'
```

Or toggle at runtime without restart:

```bash
# The container has no standalone kill/grep — use bash builtins to find the PID.
kubectl exec deploy/<agent-name> -n team1 -c envoy-proxy -- \
  bash -c 'for f in /proc/[0-9]*/cmdline; do [ -r "$f" ] || continue; c=$(<"$f"); [[ "$c" == /usr/local/bin/authbridge* ]] && kill -USR1 "${f//[!0-9]/}" && break; done'
```

## Step 3: Restart the Agent Pod

The config is read at startup, so restart the pod:

```bash
kubectl rollout restart deploy/<agent-name> -n team1
```

Wait for the new pod to be ready:

```bash
kubectl rollout status deploy/<agent-name> -n team1
```

## Step 4: Send a Request Through the Agent

Use the Rossoctl UI or curl to trigger a tool call through the agent.
For the weather agent, simply ask it a weather question — the agent
will call the weather-tool-mcp service over MCP (outbound):

```bash
# Via the Rossoctl UI:
# Navigate to the weather agent, type "What's the weather in NYC?"
```

The agent receives an A2A request (inbound, validated by jwt-validation),
then makes an outbound MCP `tools/call` to the weather tool. The
mcp-parser sees this outbound call.

## Step 5: Verify in Logs

Check the envoy-proxy container logs for MCP parsing output:

```bash
kubectl logs deploy/<agent-name> -n team1 -c envoy-proxy | grep mcp-parser
```

### Expected Output (LOG_LEVEL=info)

When a tool call flows through:

```
level=INFO msg="mcp-parser: parsed tools/call" tool=get_weather
```

When a resource read flows through:

```
level=INFO msg="mcp-parser: parsed resources/read" uri=file:///tmp/data.csv
```

### Expected Output (LOG_LEVEL=debug)

With debug enabled, you also see the body buffering flow:

```
level=DEBUG msg="ext_proc: requesting body from Envoy" direction=""
level=DEBUG msg="ext_proc: received request body" direction="" bodyLen=106
level=INFO  msg="outbound passthrough" host=weather-tool-mcp:9090 reason="no matching route"
level=DEBUG msg="pipeline: plugin completed" plugin=token-exchange
level=INFO  msg="mcp-parser: parsed tools/call" tool=get_weather
level=DEBUG msg="pipeline: plugin completed" plugin=mcp-parser
```

The `direction=""` (empty) indicates outbound — only inbound requests
have the `x-authbridge-direction: inbound` header set by Envoy's Lua
filter.

### Requests That Are NOT MCP

Non-JSON or non-MCP requests pass through silently at debug level:

```
level=DEBUG msg="mcp-parser: body is not valid JSON-RPC" error="invalid character..." bodyLen=42
```

Or if the request has no body (e.g., GET requests skip body buffering):

```
level=DEBUG msg="mcp-parser: no body, skipping"
```

## Troubleshooting

### No mcp-parser logs at all

1. Confirm the ConfigMap was applied:
   ```bash
   kubectl get configmap authbridge-runtime-config -n team1 -o yaml | grep mcp-parser
   ```
2. Confirm the pod restarted after the ConfigMap change
3. Check authbridge startup logs for `"mode", "envoy-sidecar"` — if it
   says the wrong mode, the config isn't being read

### "waypoint mode does not support plugins that require body access"

This fatal error means you configured `mcp-parser` in waypoint mode.
ext_authz cannot forward request bodies (hard Envoy constraint). Use
envoy-sidecar or proxy-sidecar mode instead.

### "Spurious response message received on gRPC stream"

This Envoy warning means ext_proc sent a response that Envoy wasn't
expecting. Two common causes:

1. **Missing `allow_mode_override: true`** — Envoy ignores the
   `ModeOverride` in the headers response, never sends RequestBody, but
   authbridge already replied to RequestHeaders. The next request on the
   same stream then gets a stale response. Fix: add
   `allow_mode_override: true` to the ext_proc filter config.

2. **Wrong response phase** — If authbridge sends a
   `ProcessingResponse_RequestHeaders` when Envoy expects a
   `ProcessingResponse_RequestBody` (because body buffering was
   requested), Envoy logs this warning. This was fixed in
   v0.5.0-alpha.9+.

### Body not reaching the parser

If you see `mcp-parser: no body, skipping` for requests that should
have a body, check:
1. The request is POST with a `Content-Length` or `Transfer-Encoding`
   header (GET/HEAD/OPTIONS/DELETE requests skip body buffering)
2. The `allow_mode_override: true` is set on the ext_proc filter in the
   envoy-config ConfigMap (check both outbound and inbound listeners)
3. Envoy's `per_stream_buffer_limit_bytes` isn't set too low (default
   1MB is fine for MCP)

Verify the envoy config:
```bash
kubectl exec deploy/<agent-name> -n team1 -c envoy-proxy -- \
  cat /etc/envoy/envoy.yaml | grep allow_mode_override
```
You should see `allow_mode_override: true` for each ext_proc filter.

## What This Enables (Future)

With `pctx.Extensions.MCP` populated, future plugins can:

- **tool-policy**: Allow/deny specific tools based on caller identity
  (`pctx.Claims.Scopes` + `pctx.Extensions.MCP.Params["name"]`)
- **audit**: Log every tool invocation with full caller attribution
- **guardrails**: Inspect tool arguments for PII or injection patterns
- **rate-limit**: Per-tool rate limiting based on caller identity

These are Phase 2/3 plugins that read the `mcp` extension slot.
