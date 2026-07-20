# AuthBridge Binaries

Two mode-specific authbridge binaries (proxy, envoy) plus the `abctl` TUI.
Each binary is hardcoded to a single deployment shape; the YAML `mode:`
field must match the binary or boot fails. Mode is selected at build time
by which binary you run, not at runtime via a flag. The `authbridge-lite`
image is a build variant of the proxy binary (proxy Dockerfile +
`exclude_plugin_*` tags), not a separate binary.

## Binaries

| Directory | Mode | Listeners | Plugins | Image (CI) |
|---|---|---|---|---|
| [`authbridge-proxy/`](authbridge-proxy/) | `proxy-sidecar` (default) | HTTP forward + reverse proxies | full (jwt-validation, token-exchange, a2a-parser, mcp-parser, inference-parser) | `ghcr.io/rossoctl/rossocortex/authbridge` |
| [`authbridge-envoy/`](authbridge-envoy/) | `envoy-sidecar` | gRPC ext_proc on `:9090` (hooked into Envoy) | full | `ghcr.io/rossoctl/rossocortex/authbridge-envoy` |
| `authbridge-lite` _(build variant of `authbridge-proxy`)_ | `proxy-sidecar` | HTTP forward + reverse proxies | lite — `authbridge-proxy` built with `exclude_plugin_*` tags (jwt-validation + token-exchange only; OPA + parsers dropped) | `ghcr.io/rossoctl/rossocortex/authbridge-lite` |
| [`abctl/`](abctl/) | n/a | n/a | n/a | not published — local TUI for the Session Events API |

Each binary directory contains `main.go`, `go.mod`/`go.sum`,
`Dockerfile`, and `entrypoint.sh`. The Dockerfiles produce
combined images that bundle the authbridge binary, the
[`spiffe-helper`](https://github.com/spiffe/spiffe-helper) daemon
(started conditionally on `SPIRE_ENABLED=true`), and — for the envoy
variant — the Envoy proxy itself.

## Configuration

Both binaries accept a single flag, `--config <path>`, pointing
at the YAML config file the operator mounts at
`/etc/authbridge/config.yaml`. The config schema and per-plugin
options are documented in
[`../docs/plugin-reference.md`](../docs/plugin-reference.md).
Hot-reload, the session-events API at `:9094`, and the supporting
ConfigMap contracts are documented in
[`../CLAUDE.md`](../CLAUDE.md).

## Ports

**Proxy-sidecar (`authbridge-proxy`, and its `authbridge-lite` image variant):**

| Port | Purpose |
|---|---|
| 8080 | Reverse proxy (inbound) |
| 8081 | Forward proxy (outbound; HTTP_PROXY target) |
| 9091 | Health |
| 9093 | Stats / config inspection |
| 9094 | Session Events API (consumed by `abctl`) |

**Envoy-sidecar (`authbridge-envoy`):**

| Port | Purpose |
|---|---|
| 15123 | Envoy outbound listener (iptables redirects here) |
| 15124 | Envoy inbound listener |
| 9090 | gRPC ext_proc (called by Envoy) |
| 9901 | Envoy admin |

## Choosing a binary

- **Default deployment**: use `authbridge-proxy`. No iptables, no
  Envoy, observable via abctl.
- **Need ambient/transparent interception via Envoy**: use
  `authbridge-envoy`. Requires the [`proxy-init`](../authproxy/)
  iptables init container.
- **Size-constrained, no protocol-aware events needed**: use the
  `authbridge-lite` image — the `authbridge-proxy` binary built with
  `exclude_plugin_*` tags (auth-only). Same listener layout, but without
  parsers/OPA — abctl will only see denial events and basic auth-level
  invocations, not full A2A/MCP/Inference protocol context.
