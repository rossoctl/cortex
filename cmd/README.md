# AuthBridge Binaries

Four authbridge binaries (proxy, envoy, cpex, praxis) plus the
`abctl` TUI — see the table below for which are published and which are paused.
Proxy, envoy and cpex each pin one deployment shape and refuse a mismatching
`mode:` at boot; praxis pins none. Note proxy and cpex both pin
`proxy-sidecar`, so `mode:` names a shape, not a binary. Mode is selected at
build time by which binary you run, not at runtime via a flag. The `authbridge-lite`
image is a build variant of the proxy binary (proxy Dockerfile +
the `lite` profile's tags), not a separate binary.

## Binaries

| Directory | Mode | Listeners | Plugins | Image (CI) |
|---|---|---|---|---|
| [`authbridge-proxy/`](authbridge-proxy/) | `proxy-sidecar` (default) | HTTP forward + reverse proxies | full (jwt-validation, token-exchange, a2a-parser, mcp-parser, inference-parser) | `ghcr.io/rossoctl/cortex/authbridge` |
| [`authbridge-envoy/`](authbridge-envoy/) | `envoy-sidecar` | gRPC ext_proc on `:9090` (hooked into Envoy) | full | `ghcr.io/rossoctl/cortex/authbridge-envoy` |
| `authbridge-lite` _(build variant of `authbridge-proxy`)_ | `proxy-sidecar` | HTTP forward + reverse proxies | lite — `authbridge-proxy` built with the `lite` profile, a sidecar minimum (see [`../scripts/profile-tags`](../scripts/profile-tags)) | `ghcr.io/rossoctl/cortex/authbridge-lite` |
| [`authbridge-cpex/`](authbridge-cpex/) | `proxy-sidecar` | HTTP forward + reverse proxies | full + `cpex` (needs cgo; links `libcpex_ffi.a`) | `ghcr.io/rossoctl/cortex/authbridge-cpex` |
| [`authbridge-praxis/`](authbridge-praxis/) | `proxy-sidecar` _(output shape; pins no input mode)_ | HTTP, from a rendered Praxis config | **none** — defines no `plugins_*.go`. **Paused, not abandoned:** kept and kept compiling (it is in the `ci.yaml` matrix for that reason). Do not delete. | not published |
| [`abctl/`](abctl/) | n/a | n/a | n/a | not published as an image; released as a standalone binary by `release-binaries.yaml` |

Each sidecar binary directory contains `main.go`, `go.mod`/`go.sum`,
`Dockerfile`, and `entrypoint.sh`; `abctl/` has neither a Dockerfile nor an
entrypoint, since it ships as a binary rather than an image. The images carry the authbridge
binary and — for the envoy variant — the Envoy proxy itself. There is
no bundled `spiffe-helper` daemon and no `SPIRE_ENABLED` gate: SVIDs
are fetched in-process by `authlib/spiffe`'s Provider over the SPIRE
Workload API.

## Configuration

Every sidecar binary accepts `--config <path>`, pointing
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
| 8080 | Reverse proxy (inbound, `inbound_interception: reverse-proxy` — the default) |
| 8081 | Forward proxy (outbound; HTTP_PROXY target) |
| 8082 | Transparent egress listener (enforce-redirect capture target) |
| 8083 | Transparent inbound listener (`inbound_interception: transparent`) |
| 9091 | Health (`listener.health_addr`) |
| 9093 | Stats / config inspection |
| 9094 | Session Events API (consumed by `abctl`) |

`8080` and `8083` are mutually exclusive: `inbound_interception` picks one
inbound mechanism, and the preset fills only that one's address.

All of these are overridable, which matters for running two proxies on one host:
a second instance on the default ports dies on a bind conflict. They are not all
under the same config key — everything above is a `listener.*` address except
`9093`, which is `stats.stats_address`. The defaults bind every interface, which
is what Kubernetes probes and sidecar traffic need but not what a laptop wants;
local single-host setups typically pin them all to `127.0.0.1`. `authbridge-proxy
--local` ships exactly such a config — see
[`docs/laptop-token-savings.md`](../docs/laptop-token-savings.md).

`8082` and `8083` are the iptables REDIRECT targets installed by
[`proxy-init`](../deploy/proxy-init/) and must match its `TRANSPARENT_PORT` /
`INBOUND_TRANSPARENT_PORT`. A mismatch redirects traffic to a dead port.

**Envoy-sidecar (`authbridge-envoy`):**

| Port | Purpose |
|---|---|
| 15123 | Envoy outbound listener (iptables redirects here) |
| 15124 | Envoy inbound listener |
| 9090 | gRPC ext_proc (called by Envoy) |
| 9901 | Envoy admin |

## Choosing a binary

- **Default deployment**: use `authbridge-proxy`. No Envoy, observable via
  abctl. Cooperative egress (HTTP_PROXY) needs no iptables; the always-on
  `enforce-redirect` egress guard and the opt-in transparent inbound listener
  both use [`proxy-init`](../deploy/proxy-init/).
- **Need ambient/transparent interception via Envoy**: use
  `authbridge-envoy`. Requires the [`proxy-init`](../deploy/proxy-init/)
  iptables init container.
- **Size-constrained, no protocol-aware events needed**: use the
  `authbridge-lite` image — the `authbridge-proxy` binary built with the
  `lite` profile from `scripts/profile-tags` (a sidecar
  minimum). Same listener layout, but without parsers/OPA — abctl
  will only see denial events and basic auth-level invocations, not
  full A2A/MCP/Inference protocol context.
