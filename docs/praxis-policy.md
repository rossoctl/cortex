# Policy in praxis mode

In praxis mode, the policy filter runs inside `cortex-gateway`. It uses PPE, the
Praxis policy engine. `praxis.policy` is the replacement for the laptop CPEX build.

## Turn policy on

1. Install praxis mode: `agentop setup --praxis`. It installs `cortex-gateway` and
   sets `praxis: {enabled: true}`.
2. Write the policy file, for example `~/.cortex/policy.yaml`. Copy one of the examples
   below.
3. Set the path in `~/.cortex/config.yaml`. It must be an absolute path to an existing
   file:

```yaml
praxis:
  enabled: true
  policy:
    config_path: /Users/you/.cortex/policy.yaml
```

4. Run `agentop service restart`. The supervisor reads `praxis.*` only when it starts,
   so a save alone changes nothing.
5. Edit the policy file later without a restart. The gateway reloads it. A file that
   does not parse keeps the last good policy.

## What the gateway runs

Built:

- `identity/jwt`, with its JWKS, PEM and JWK key sources, and its claim presets.
- `cel:` steps. They need `global: { pdp: [{ kind: cel }] }`.
- Everything in PPE that does not need a plugin from the list below.

Not built:

- Cedar, OPA, OAuth delegation, CIBA, Vault and Valkey.
- A2A policy. The gateway has no A2A route.

## Where policy runs

The supervisor writes the filter chain into `~/.cortex/gateway.yaml` from
`praxis.policy.config_path`. The chain is `ip_acl`, `cortex_attribution`,
`cortex_observe`, `policy`, `access_log`, `router`, `load_balancer`
(`cmd/cortex/gateway.go`). So `policy` runs after the row is recorded, and a denied
request still gets its row.

## Record the decision in the session row

Add the `cortex-recorder` plugin to the top of the policy file. The response row then
carries `{plugin: "policy", action: "allow"|"deny", reason: <violation code>}`:

```yaml
plugins:
  - name: cortex-recorder
    kind: cortex/recorder
    capabilities: [read_headers]
```

## Examples

The model allow list admits only the models that you name. Every other model is
denied:

```yaml policy
routes:
  - llm: claude-sonnet-4
    authorization:
      pre_invocation:
        - "llm.provider == 'never': deny('unreachable', 'never')"
  - llm: "*"
    authorization:
      pre_invocation:
        - "deny('model is not permitted', 'model_not_allowed')"
```

This policy permits the MCP tool `read_file` and denies every other tool. The
`tool: read_file` route holds a guard that never denies. PPE route selection uses
specificity, so this route outranks `tool: "*"`. To deny `read_file`, remove its
route. The first `llm:` route admits inference requests, which carry no MCP method:

```yaml policy
routes:
  - llm: "*"
    authorization:
      pre_invocation:
        - "llm.provider == 'never': deny('unreachable', 'never')"
  - tool: read_file
    authorization:
      pre_invocation:
        - "http.method == 'NEVER': deny('unreachable', 'never')"
  - tool: "*"
    authorization:
      pre_invocation:
        - "deny('tool is not permitted', 'tool_not_allowed')"
```

This policy reads the user JWT from `X-Agent-Token`. On a laptop, `Authorization`
carries the agent's provider key, so a user JWT goes in another header. PPE does not
remove `X-Agent-Token`, so the origin receives it. Production uses `jwks_url` or
`pem_file`, not an inline PEM. A loopback JWKS needs `allow_private_idp: true`, which
`praxis.policy` does not set. A JWKS URL also needs `capabilities: [perform_http]`:

```yaml policy
plugins:
  - name: user-jwt
    kind: identity/jwt
    hooks: [identity.resolve]
    config:
      credential: { kind: header, name: X-Agent-Token }
      trusted_issuers:
        - issuer: https://idp.example.com
          audiences: [cortex]
          algorithms: [ES256]
          decoding_key:
            kind: pem
            pem: |
              -----BEGIN PUBLIC KEY-----
              MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEaSRwuPUoZ6cKqrpd88zDaoK8Ph2N
              bkonAXfrgNqiB9ccLFGc/vkCNYh2Gndh7L1Cc07oMseoSM4MoJWjYhnp/w==
              -----END PUBLIC KEY-----
routes:
  - llm: "*"
    authentication: [user-jwt]
```

This step taints the session with the label `oversized` when the request asks for more
than 8000 output tokens. The step needs a resolved subject and an `X-Session-Id`
header. It does not block. `on_allow` on a `cel:` step is not a replacement, because a
false `cel:` step denies. Taints live in memory, so a gateway restart or a policy
reload loses them:

```yaml policy
routes:
  - llm: "*"
    authorization:
      pre_invocation:
        - when: "llm.input.max_tokens > 8000"
          do: ["taint(oversized, session)"]
```

A streamed (SSE) inference response skips the response-phase policy. Deny the stream in
the request phase instead:

```yaml policy
routes:
  - llm: "*"
    authorization:
      pre_invocation:
        - "custom.llm.stream == true: deny('a streamed response is not allowed', 'stream_not_allowed')"
```

## Deny responses

| Case | Response |
|---|---|
| Identity or transport failure | 401, `WWW-Authenticate: Bearer`, `X-Policy-Violation: <code>` |
| Inference request deny | 403 by default, from the policy's `denyWith`, with an OpenAI-compatible error envelope, `X-Policy-Violation: <code>` |
| Generic HTTP deny | 403 by default, from the policy's `denyWith`, `X-Policy-Violation: <code>` |
| MCP deny | HTTP 200 with a JSON-RPC error, `code: -32001`, `X-Policy-Violation: <code>` |
| MCP request with no `mcp.method` metadata | 500 |

## Known gaps

- No A2A policy.
- A streamed (SSE) inference response skips the response-phase policy.
- Taints are held in memory and are lost at each gateway restart and each policy
  reload.
- WebSocket upgrades do not go through the gateway (`cmd/cortex/praxis.go`), so no
  policy runs on them.
- These Cortex-only CPEX options have no equivalent: `observe_codes`,
  `principal.token_file`, `workload`, `bypass_hosts`, `bypass_paths`, `strict_hosts`,
  `synthetic_egress`, `delegated_credential_hosts`, `session_source`,
  `fallback_identification`, `hooks`, `fail_open`.

## Move from the CPEX plugin

The constructs in `core/plugins/cpex/testdata/hr-cpex/cpex-policy.yaml` and in the
examples in `docs/cpex-plugin.md` exist in PPE 0.6.0. In a load check against 0.6.0,
the `hr-cpex` file failed only with `missing field store`.

1. Move the CPEX policy YAML out of the `cpex` plugin entry and into its own file.
2. Apply the 0.5 to 0.6 changes. A CIBA config needs `store: { kind: memory }`.
   `ConversationContext.history` is now `Vec<Message>`, in CMF shape. `&` and `|`
   behave differently when an operand cannot be ordered. There are new `llm.input.*`,
   `llm.model.*` and `llm.output.*` facts, and the old `llm.model_id` keys stay as
   aliases.
3. Remove the `cpex` entry from `pipeline.outbound`.
4. Set `praxis.policy.config_path` and restart, as "Turn policy on" says.
5. Check the options in the table below. Move each one you use another way, or accept
   that it is gone.

| CPEX option | In praxis mode |
|---|---|
| `observe_codes` | No equivalent. Write the step as a `when:`/`do:` taint step. |
| `principal.token_file` | No equivalent. `identity/jwt` reads a header, cookie or query parameter, not a file. |
| `workload` (`workload.audience`, `workload.header`) | No equivalent. The gateway asserts no workload token. |
| `bypass_hosts` | No equivalent. Every request that reaches the gateway is judged. |
| `bypass_paths` | No equivalent. |
| `strict_hosts` | No equivalent. |
| `synthetic_egress` | No equivalent. |
| `delegated_credential_hosts` | No equivalent. The gateway builds no OAuth delegation. |
| `session_source` | No equivalent. A taint step reads the `X-Session-Id` header. |
| `fallback_identification` | No equivalent. Write the rule in the policy. |
| `hooks` | The `hooks:` key of each plugin in the policy file. |
| `fail_open` | No equivalent. |
