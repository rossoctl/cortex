# `opa`

The `opa` plugin evaluates [OPA](https://www.openpolicyagent.org/docs) (Open
Policy Agent) policy bundles against inbound and outbound traffic. It uses an
embedded OPA engine and four fixed decision paths. A deny returns HTTP 403.

This page is for the operator: when to use the plugin, where it runs, and how
to configure it. To write a policy, read the plugin's
[README](../core/plugins/opa/README.md). It has the decision paths, the input
document, the policy contract, example policies and the bundle layout.

## When to use it

Use `opa` for a standalone allow or deny check on each request or response.
Examples: tool access control by caller, a list of approved models, a limit on
`max_tokens`, or the scopes that an agent can use against a target.

When OPA is one decision among several, or a decision must taint a session or
redact a field, use the CPEX `opa:` step instead. See
[OPA in two places](../core/plugins/cpex/README.md#external-pdps-byop).

## Where it runs

Kubernetes. The plugin downloads its policy from the Rossoctl Bundle Server,
an in-cluster service. The server serves one
[OPA policy bundle](https://www.openpolicyagent.org/docs/management-bundles)
for each agent, keyed by SPIFFE ID (see
[how it works](../core/plugins/opa/README.md#how-it-works)). The operator
mounts the agent's client ID at `/shared/client-id.txt`. A laptop has no
bundle server.

The `full`, `fullcpex` and `envoy` profiles build the plugin
([`profiles.go`](../scripts/profile-tags/profiles.go)).

## Default configuration

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
          bundle_url: "http://bundle-server.rossoctl.svc:8080"
  outbound:
    plugins:
      - name: token-exchange
        config: { ... }
      - name: opa
        config:
          bundle_url: "http://bundle-server.rossoctl.svc:8080"
```

With this config, policies can decide based on caller identity, tool names,
model names, hosts, and methods — without any bulk content crossing the OPA
evaluation boundary. To put more content in the input, use `include`. The
README shows [two examples](../core/plugins/opa/README.md#example-content-filtering-policy).

### Outbound ordering

Put `opa` after `token-exchange` on the outbound leg. `token-exchange` records
the target audience and the granted scopes of the token that it mints in the
delegation chain. OPA shows them as `input.delegation` and as a synthesized
`input.identity` (see
[Outbound identity + delegation input](../core/plugins/opa/README.md#outbound-identity--delegation-input)).
If `opa` runs first, these fields are empty.

On inbound, `input.identity` comes from a validated JWT. On outbound, no JWT
is validated, so the plugin synthesizes `input.identity` from the delegation
hop. Thus a policy can use `input.identity` on the two legs.
`input.delegation` keeps the full chain.

## Bundle server transport

The plugin gets bundles over plaintext HTTP, with no client authentication.
This is sufficient when the bundle server is in the same Kubernetes cluster
and in-cluster HTTP is acceptable. mTLS with a SPIFFE certificate, and TLS
with a service account token, are not available yet.

## Config fields

| Field | Required | Default | Description |
|---|---|---|---|
| `bundle_url` | yes | | Base URL of the Rossoctl Bundle Server (HTTP, in-cluster) |
| `agent_id_file` | no | `/shared/client-id.txt` | Path to the file containing the agent's client ID |
| `agent_id` | no | | Inline agent ID. When set, `agent_id_file` is ignored |
| `polling_min_delay` | no | `10` | Minimum bundle polling interval in seconds |
| `polling_max_delay` | no | `120` | Maximum bundle polling interval in seconds |
| `include` | no | `[]` | List of optional field groups to expose in the OPA input. See [the `include` mechanism](../core/plugins/opa/README.md#the-include-mechanism) |
