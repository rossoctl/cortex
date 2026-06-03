# opa

OPA (Open Policy Agent) plugin for AuthBridge. Downloads policy bundles from a
Kagenti Bundle Server based on the agent's identity and evaluates requests and
responses against the loaded policy using four fixed decision paths.

## How it works

1. At startup the plugin reads the agent's client ID from `/shared/client-id.txt`
   (mounted by the kagenti-operator from a Keycloak-credentials Secret).
2. It creates an embedded OPA engine via the OPA Go SDK and configures it to
   fetch `bundles/<agent-id>.tar.gz` from the bundle server.
3. The SDK downloads the bundle, activates the policy, and begins periodic
   polling for updates (respecting `ETag` / `If-None-Match` for lightweight
   304 responses).
4. On every request and response the plugin queries the appropriate decision
   path based on traffic direction and phase. If the policy denies, the plugin
   returns HTTP 403. If the path is undefined (rule not present in the bundle),
   the plugin skips evaluation — treating the absence of a rule as "no opinion".

The plugin reports not-ready (`Ready() == false`) until the first bundle is
successfully loaded. The Kubernetes readiness probe holds traffic off the pod
until then, so requests never arrive before the policy is active.

## Decision paths

The plugin uses four fixed OPA decision paths, one for each evaluation point:

| Path | Phase | Purpose |
|------|-------|---------|
| `authbridge/inbound/request` | Inbound request | Primary authorization — validate caller identity, enforce access control |
| `authbridge/inbound/response` | Inbound response | Fine-grained response evaluation (rare, expensive) — inspect response body/headers |
| `authbridge/outbound/request` | Outbound request | Control outgoing requests to external systems — ensure delegated tokens are used in line with task intent |
| `authbridge/outbound/response` | Outbound response | Protect the agent from data attacks in responses (rare) |

Each path is independent. A bundle only needs to include the rules it cares
about — undefined paths are skipped (treated as allow). Most deployments will
only define `authbridge/inbound/request`.

## Configuration

### Default configuration

Most deployments only need `bundle_url`. The lean default input is sufficient
for tool-access-control and model-restriction policies:

```yaml
pipeline:
  inbound:
    plugins:
      - name: jwt-validation
        config: { ... }
      - name: opa
        config:
          bundle_url: "http://bundle-server.kagenti.svc:8080"
  outbound:
    plugins:
      - name: opa
        config:
          bundle_url: "http://bundle-server.kagenti.svc:8080"
      - name: token-exchange
        config: { ... }
```

With this config, policies can decide based on caller identity, tool names,
model names, hosts, and methods — without any bulk content crossing the OPA
evaluation boundary.

### Example: content-filtering policy

If a policy needs to inspect the actual user prompt (e.g., block tasks
containing sensitive keywords), extend the input with `a2a.content`:

```yaml
- name: opa
  config:
    bundle_url: "http://bundle-server.kagenti.svc:8080"
    include:
      - "a2a.content"
```

This adds `a2a.parts[].content`, `a2a.artifact`, and `a2a.error_message` to
the OPA input so the policy can match against user-submitted text.

### Example: full MCP arguments + LLM conversation audit

A deployment that needs to write policies against tool argument values and
the full conversation history:

```yaml
- name: opa
  config:
    bundle_url: "http://bundle-server.kagenti.svc:8080"
    include:
      - "mcp.params"            # full tool arguments (not just name/uri)
      - "mcp.result"            # tool response data (response path)
      - "inference.messages"    # full conversation history
      - "inference.tools.detail" # tool descriptions and JSON schemas
```

### Config fields

| Field | Required | Default | Description |
|---|---|---|---|
| `bundle_url` | yes | | Base URL of the Kagenti Bundle Server |
| `agent_id_file` | no | `/shared/client-id.txt` | Path to the file containing the agent's client ID |
| `agent_id` | no | | Inline agent ID; when set, `agent_id_file` is ignored |
| `polling_min_delay` | no | `10` | Minimum bundle polling interval in seconds |
| `polling_max_delay` | no | `120` | Maximum bundle polling interval in seconds |
| `include` | no | `[]` | List of optional field groups to expose in the OPA input (see below) |

### The `include` mechanism

The OPA input document is **lean by default** — it contains only structural
metadata needed for authorization decisions (method, path, host, identity, tool
names, model names). Bulk content fields (conversation history, full tool
arguments, prompt text) are excluded unless explicitly requested via the
`include` config list.

Each entry in `include` is a named group key that unlocks additional fields:

| Key | What it unlocks | Default | Size concern |
|-----|----------------|---------|--------------|
| `mcp.params.name` | `params.name` in MCP input | **ON** | Tiny |
| `mcp.params.uri` | `params.uri` in MCP input | **ON** | Tiny |
| `mcp.params.<key>` | Any specific params key (e.g., `mcp.params.cursor`) | OFF | Varies |
| `mcp.params` | Full `params` map (all keys) | OFF | Can be large |
| `mcp.result` | `result` map (response path) | OFF | Can be very large |
| `mcp.error` | `error` object (code + message + data) | OFF | Usually small |
| `a2a.content` | `parts[].content`, `artifact`, `error_message` | OFF | KBs (prompts/responses) |
| `inference.messages` | Full `messages[]` array | OFF | Tens of KBs |
| `inference.completion` | `completion` text | OFF | Can be large |
| `inference.tools.detail` | Tool `description` + `parameters` | OFF | Moderate |
| `inference.tool_calls` | `tool_calls[]` with `arguments` | OFF | Can be large |

Default-on keys (`mcp.params.name`, `mcp.params.uri`) are always included even
with an empty `include` list.

## OPA input document

### Default (lean) input

```json
{
  "direction": "inbound",
  "method": "POST",
  "path": "/api/v1/invoke",
  "host": "my-agent",
  "headers": {
    "authorization": "Bearer eyJ...",
    "content-type": "application/json"
  },
  "identity": {
    "subject": "user-123",
    "client_id": "caller-agent",
    "scopes": ["openid", "profile"]
  },
  "agent": {
    "client_id": "my-agent"
  },
  "a2a": {
    "method": "invoke",
    "session_id": "sess-123",
    "task_id": "task-789",
    "role": "user"
  },
  "mcp": {
    "method": "tools/call",
    "params": { "name": "create_issue", "uri": "file:///workspace/main.go" }
  },
  "inference": {
    "model": "gpt-4",
    "stream": false,
    "max_tokens": 4000,
    "tools": ["create_issue", "list_issues"]
  }
}
```

Notes:
- `mcp.params` contains only the default-on keys (`name`, `uri`) that are
  present in the original params. Add more via `include`.
- `inference.tools` is a string array of tool names only (not full objects).
  Use `inference.tools.detail` in `include` for descriptions and parameters.

### With `include: ["a2a.content", "mcp.params", "inference.messages", "inference.tools.detail"]`

```json
{
  "a2a": {
    "method": "invoke",
    "session_id": "sess-123",
    "task_id": "task-789",
    "role": "user",
    "parts": [
      { "kind": "text", "content": "Create a GitHub issue for bug XYZ" }
    ],
    "artifact": "Issue #42 created",
    "error_message": ""
  },
  "mcp": {
    "method": "tools/call",
    "params": { "name": "create_issue", "arguments": {"title": "Bug XYZ", "body": "..."} }
  },
  "inference": {
    "model": "gpt-4",
    "stream": false,
    "max_tokens": 4000,
    "messages": [
      {"role": "user", "content": "Help me create an issue"}
    ],
    "tools": [
      {"name": "create_issue", "description": "Creates a GitHub issue", "parameters": {"type": "object"}}
    ]
  }
}
```

On the response path the document also includes:

```json
{
  "response": {
    "status_code": 200,
    "headers": {
      "content-type": "application/json"
    }
  }
}
```

### Input field reference

| Field | Present | Description |
|---|---|---|
| `direction` | always | `"inbound"` or `"outbound"` |
| `method` | always | HTTP method (`GET`, `POST`, ...) |
| `path` | always | Request URL path |
| `host` | always | HTTP `Host` header value |
| `headers` | always | Flattened request headers (lowercase keys, multi-values joined with `,`) |
| `identity` | when jwt-validation ran | Subject, client ID, and scopes from the validated JWT |
| `agent` | when agent identity is set | The agent's own client ID |
| `a2a` | when a2a-parser ran | A2A protocol metadata (method, session_id, task_id, role) |
| `mcp` | when mcp-parser ran | MCP method + filtered params |
| `inference` | when inference-parser ran | Model, stream, max_tokens, tool names |
| `response` | response path only | Status code and response headers |

## Policy contract

Each decision path evaluates to an `allow` rule. The plugin supports two
return shapes:

**Boolean** -- the simplest form:

```rego
package authbridge.inbound.request

default allow := false

allow if {
    input.identity.subject != ""
}
```

**Object with reason** -- for detailed deny messages:

```rego
package authbridge.inbound.request

default allow := false

allow if {
    input.identity.subject != ""
}

reason := "anonymous access not permitted" if {
    not allow
}
```

## Example policies

### Tool access control (MCP) — inbound request

```rego
package authbridge.inbound.request

default allow := false

# Define allowed tools per client
allowed_tools := {
    "github-agent": ["create_issue", "list_issues", "get_issue"],
    "admin-agent": ["create_issue", "list_issues", "get_issue", "delete_issue"],
}

# Allow tool calls only if the tool is in the allowed list
allow if {
    input.mcp.method == "tools/call"
    input.identity.client_id
    tool_name := input.mcp.params.name
    allowed_tools[input.identity.client_id][_] == tool_name
}

# Allow tool listing for all authenticated users
allow if {
    input.mcp.method == "tools/list"
    input.identity.subject != ""
}
```

### LLM model restriction (Inference) — outbound request

```rego
package authbridge.outbound.request

default allow := {"allow": false, "reason": "default deny"}

approved_models := ["gpt-4", "gpt-3.5-turbo", "claude-3-sonnet"]

allow := {"allow": true} if {
    input.inference.model
    approved_models[_] == input.inference.model
    not excessive_token_request
}

excessive_token_request if {
    input.inference.max_tokens
    input.inference.max_tokens > 4000
}

allow := {"allow": false, "reason": "token limit exceeds policy"} if {
    excessive_token_request
}
```

### Dangerous tool combination block (Inference) — outbound request

```rego
package authbridge.outbound.request

default allow := false

allow if {
    input.inference.model
    not has_dangerous_combo
}

has_dangerous_combo if {
    tools := input.inference.tools
    has_value(tools, "write_file")
    has_value(tools, "execute_command")
}

has_value(arr, val) if {
    arr[_] == val
}
```

### Task content filtering (A2A) — requires `include: ["a2a.content"]`

```rego
package authbridge.inbound.request

default allow := {"allow": false, "reason": "default deny"}

allow := {"allow": true} if {
    input.identity.subject != ""
    not contains_sensitive_keywords
}

contains_sensitive_keywords if {
    input.a2a.parts[_].content
    task := lower(input.a2a.parts[_].content)
    sensitive := ["delete database", "drop table", "rm -rf", "sudo"]
    some keyword in sensitive
    contains(task, keyword)
}

allow := {"allow": false, "reason": "task contains sensitive keyword"} if {
    contains_sensitive_keywords
}
```

### Multi-path bundle example

A single bundle can contain rules for multiple decision paths:

```
bundles/my-agent.tar.gz
  authbridge/
    inbound/
      request.rego      # package authbridge.inbound.request
    outbound/
      request.rego      # package authbridge.outbound.request
```

The plugin interprets the decision as follows:

| Result | Action |
|---|---|
| `true` | Allow |
| `false` | Deny with "policy denied" |
| `{"allow": true}` | Allow |
| `{"allow": false}` | Deny with "policy denied" |
| `{"allow": false, "reason": "..."}` | Deny with the provided reason |
| Anything else | Deny (safe default) |

## Bundle layout

The bundle server must serve a standard OPA bundle at the path
`bundles/<agent-id>.tar.gz`. A minimal bundle contains a single `.rego` file
for the inbound request path:

```
bundles/my-agent.tar.gz
  authbridge/
    inbound/
      request.rego
```

A full bundle covering all four decision paths:

```
bundles/my-agent.tar.gz
  authbridge/
    inbound/
      request.rego       # package authbridge.inbound.request
      response.rego      # package authbridge.inbound.response (optional)
    outbound/
      request.rego       # package authbridge.outbound.request (optional)
      response.rego      # package authbridge.outbound.response (optional)
```

Only include the paths you need — the plugin skips evaluation for any
undefined path.

See the [OPA bundle documentation](https://www.openpolicyagent.org/docs/latest/management-bundles/)
for the full specification.

## Pipeline ordering

The plugin declares `After: ["jwt-validation", "a2a-parser", "mcp-parser", "inference-parser"]`
(soft ordering). When these plugins are present in the same pipeline, OPA runs
after them so `input.identity`, `input.a2a`, `input.mcp`, and `input.inference`
are populated. If any are absent, OPA still runs — the corresponding input
fields will be missing and the policy must handle that case.

## Deny behavior

- **Request path**: OPA not initialized or decision error -> 503. Policy deny -> 403. Path undefined -> skip.
- **Response path**: OPA not initialized -> skip. Policy deny -> 403. Path undefined -> skip.
- **Before bundle loads**: the readiness probe holds traffic off the pod. If a
  request arrives anyway (e.g. in tests), the plugin denies with 503.

## Session events

The plugin records `Invocation` entries for every decision:

| Action | Reason | When |
|---|---|---|
| `allow` | `policy_allowed` / `response_policy_allowed` | OPA returned allow |
| `deny` | `policy_denied` / `response_policy_denied` | OPA returned deny |
| `deny` | `decision_error` / `response_decision_error` | OPA evaluation failed |
| `skip` | `opa_not_ready` | Response path, OPA not yet initialized |
| `skip` | `no_policy_rule` | Decision path undefined in bundle (no rule for this phase) |

These appear in the session events API (`:9094`) and in `abctl`.
