# Token Broker Plugin

The `token-broker` plugin acquires tokens from an external Token Broker service
on behalf of outbound requests. It replaces the caller's bearer token with one
issued by the broker for the target service.

## Architecture

```
┌─────────────┐         ┌──────────────┐         ┌──────────────┐
│   Client    │────────▶│  AuthBridge  │────────▶│ Token Broker │
│             │  JWT    │              │  JWT +  │   Service    │
│             │         │              │  Server │              │
└─────────────┘         └──────────────┘   URL   └──────────────┘
                              │                          │
                              │    ◀────────────────────┘
                              │         Token
                              ▼
                        ┌──────────────┐
                        │ Target Server│
                        │ (e.g. GitHub │
                        │  API)        │
                        └──────────────┘
```

**Flow**:
1. Client sends request with JWT to AuthBridge
2. AuthBridge extracts the bearer token and derives the target server URL from the outbound host
3. AuthBridge calls Token Broker with the bearer token and target server URL
4. Token Broker acquires token (may block waiting for user authorization)
5. AuthBridge replaces JWT with acquired token and forwards to target

## Use Case

Human-in-the-loop OAuth flows where the broker manages user consent and token
caching. The plugin blocks until the broker returns a token (up to 300s to
allow for interactive user login).

**Example**: Your application calls GitHub API on behalf of users, but GitHub
OAuth requires a browser-based authorization flow. The Token Broker handles this
interaction transparently — application code remains unchanged.

## Configuration

```yaml
pipeline:
  outbound:
    plugins:
      - name: token-broker
        config:
          broker_url: "http://token-broker:8080"    # Required
          default_policy: "passthrough"              # "broker" or "passthrough" (default)
          routes:
            file: "/etc/authproxy/routes.yaml"      # Optional
            rules:                                   # Optional inline rules
              - host: "mcp-server"
                action: "broker"
              - host: "internal-*"
                action: "passthrough"
```

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `broker_url` | Yes | — | Base URL of the Token Broker service |
| `default_policy` | No | `passthrough` | Action for hosts matching no route |
| `routes.file` | No | — | Path to a routes YAML file |
| `routes.rules` | No | — | Inline route entries |

### Route Rules

Each rule has a `host` (glob pattern) and an `action` (`"broker"` or `"passthrough"`).
Rules with no explicit action default to `"broker"`.

Host matching strips the port before comparison (`api.example.com:8443` matches
pattern `api.example.com`). First match wins.

### Routes File Format

```yaml
# /etc/authproxy/routes.yaml
- host: "api.github.com"
  action: "broker"
- host: "*.github.com"
  action: "broker"
- host: "internal-service"
  action: "passthrough"
```

### Pipeline Composition

Token Broker can be composed with other plugins in the outbound pipeline:

```yaml
pipeline:
  outbound:
    plugins:
      - name: token-exchange   # Handles exchange routes (service-to-service)
      - name: token-broker     # Handles broker routes (user-delegated)
```

## Broker Protocol

### Endpoint

```
POST {broker_url}/sessions/token
```

### Request Headers

| Header | Value | Description |
|--------|-------|-------------|
| `Authorization` | `Bearer <caller-token>` | The original request's JWT |
| `X-Server-Url` | `{scheme}://{host}` | Target service URL (scheme derived from request) |

### Success Response (200 OK)

```json
{
  "token": "gho_xxxxxxxxxxxx"
}
```

### Error Responses

| Status | Meaning | AuthBridge Action |
|--------|---------|-------------------|
| 401 | Invalid/expired JWT | Returns 401 to client |
| 403 | User denied authorization | Returns 403 to client |
| 408 | Token acquisition timeout | Returns 408 to client |
| 5xx | Service error | Returns 502 Bad Gateway |

### Timeout Behavior

- **Token Broker timeout**: 300 seconds (5 minutes) — allows for user login
- **AuthBridge client timeout**: 310 seconds (broker times out first)
- Broker blocks until token is available or timeout occurs

## Comparison with Token Exchange

| Feature | Token Exchange | Token Broker |
|---------|---------------|--------------|
| **Protocol** | OAuth2 Token Exchange (RFC 8693) | Custom HTTP API |
| **Latency** | Low (~100ms) | High (seconds to minutes) |
| **User Interaction** | No | Yes (may require browser) |
| **Use Case** | Service-to-service | User-delegated access |
| **Examples** | Keycloak, Auth0 | GitHub OAuth, Google OAuth |

## Route Action Semantics

The `action` field in token-broker routes uses `"broker"` / `"passthrough"`,
while the existing `token-exchange` plugin uses `"exchange"` / `"passthrough"`.
Both plugins share the `routing.Router` infrastructure. The router's `Resolve`
method maps action strings to `ResolvedRoute.Passthrough = true` when the action
equals `"passthrough"`. The token-broker plugin only inspects the `Passthrough`
boolean, so its `"broker"` action works identically to token-exchange's
`"exchange"` at the router level — both are simply "not passthrough".

## Debugging

Enable debug logging to see broker operations:

```bash
kubectl logs -f deployment/myapp -c authbridge-proxy
```

Example log output:
```
level=WARN msg="token-broker: broker returned error" status=403 error=access_denied description="insufficient permissions"
level=ERROR msg="token-broker: broker request failed" error="token broker request failed: context deadline exceeded"
```

**Common issues**:

| Symptom | Cause | Fix |
|---------|-------|-----|
| 401 at client | Missing bearer token on outbound request | Ensure app sets Authorization header |
| 502 Bad Gateway | Broker service unreachable | Check broker pod health and networking |
| 408 timeout | User didn't complete login within 300s | User must retry; check broker UI |

## Security Considerations

1. **JWT forwarding**: The inbound bearer token is forwarded to the broker — use TLS for `broker_url` in production
2. **Target binding**: The derived server URL is sent via `X-Server-Url` so the broker can scope tokens appropriately
3. **Token scope**: Token Broker should issue tokens with minimum necessary scopes
4. **Audit**: Token Broker should log all token acquisitions for audit trail

## Files

| Path | Description |
|------|-------------|
| `authlib/plugins/tokenbroker.go` | Plugin implementation |
| `authlib/tokenbroker/client.go` | HTTP client for broker service |
| `authlib/tokenbroker/error.go` | Structured error type |
| `authlib/plugins/tokenbroker_test.go` | Plugin tests |
| `authlib/tokenbroker/client_test.go` | Client tests |
