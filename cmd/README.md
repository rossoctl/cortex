# AuthBridge Binaries

Three authbridge binaries (proxy, envoy, praxis) plus the
`agentop` TUI — see the table below for which are published and which are paused.
Proxy and envoy each pin one deployment shape and refuse a mismatching
`mode:` at boot; praxis pins none. Mode is selected at
build time by which binary you run, not at runtime via a flag. The `authbridge-lite`
and `authbridge-cpex` images are build variants of the proxy binary, not
separate binaries: `lite` is the proxy Dockerfile with the `lite` profile's
tags, `cpex` is `cortex` built with cgo and the `cpex` tag (see
[cpex builds](#cpex-builds)).

## Binaries

| Directory | Mode | Listeners | Plugins | Image (CI) |
|---|---|---|---|---|
| [`cortex/`](cortex/) | `proxy-sidecar` (default) | HTTP forward + reverse proxies | full, incl. parsers (see [`../scripts/profile-tags`](../scripts/profile-tags)) | `ghcr.io/rossoctl/cortex/authbridge` |
| [`cortex-envoy/`](cortex-envoy/) | `envoy-sidecar` | gRPC ext_proc on `:9090` (hooked into Envoy) | full | `ghcr.io/rossoctl/cortex/authbridge-envoy` |
| `authbridge-lite` _(build variant of `cortex`)_ | `proxy-sidecar` | HTTP forward + reverse proxies | lite — `cortex` built with the `lite` profile, a sidecar minimum (see [`../scripts/profile-tags`](../scripts/profile-tags)) | `ghcr.io/rossoctl/cortex/authbridge-lite` |
| `authbridge-cpex` _(build variant of `cortex`; [`cortex-cpex/`](cortex-cpex/) holds its Dockerfile and FFI pins)_ | `proxy-sidecar` | HTTP forward + reverse proxies | the `cpex` profile: sidecar plugins + `cpex` (needs cgo; links `libcpex_ffi.a` from the CPEX release pinned in `CPEX_FFI_VERSION`) | `ghcr.io/rossoctl/cortex/authbridge-cpex` |
| [`cortex-praxis/`](cortex-praxis/) | `proxy-sidecar` _(output shape; pins no input mode)_ | HTTP, from a rendered Praxis config | **none** — defines no `plugins_*.go`. **Paused, not abandoned:** kept and kept compiling (it is in the `ci.yaml` matrix for that reason). Do not delete. | not published |
| [`agentop/`](agentop/) | n/a | n/a | n/a | not published as an image; released as a standalone binary by `release-binaries.yaml` |

Each sidecar binary directory contains `main.go`, `go.mod`/`go.sum`,
`Dockerfile`, and `entrypoint.sh`; `agentop/` has neither a Dockerfile nor an
entrypoint, since it ships as a binary rather than an image, and `cortex-cpex/`
has no Go code — only the image's `Dockerfile`, `entrypoint.sh` and the
`CPEX_FFI_*` pins. The images carry the authbridge
binary and — for the envoy variant — the Envoy proxy itself. There is
no bundled `spiffe-helper` daemon and no `SPIRE_ENABLED` gate: SVIDs
are fetched in-process by `core/spiffe`'s Provider over the SPIRE
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

**Proxy-sidecar (`cortex`, and its `authbridge-lite` and `authbridge-cpex` image variants):**

| Port | Purpose |
|---|---|
| 8080 | Reverse proxy (inbound, `inbound_interception: reverse-proxy` — the default) |
| 8081 | Forward proxy (outbound; HTTP_PROXY target) |
| 8082 | Transparent egress listener (enforce-redirect capture target) |
| 8083 | Transparent inbound listener (`inbound_interception: transparent`) |
| 9091 | Health (`listener.health_addr`) |
| 9093 | Stats / config inspection |
| 9094 | Session Events API (consumed by `agentop`) |

`8080` and `8083` are mutually exclusive: `inbound_interception` picks one
inbound mechanism, and the preset fills only that one's address.

All of these are overridable, which matters for running two proxies on one host:
a second instance on the default ports dies on a bind conflict. They are not all
under the same config key — everything above is a `listener.*` address except
`9093`, which is `stats.address`. The defaults bind every interface, which
is what Kubernetes probes and sidecar traffic need but not what a laptop wants;
local single-host setups typically pin them all to `127.0.0.1`. `cortex
--local` ships exactly such a config — see
[`docs/laptop-token-savings.md`](../docs/laptop-token-savings.md).

`8082` and `8083` are the iptables REDIRECT targets installed by
[`proxy-init`](../deploy/proxy-init/) and must match its `TRANSPARENT_PORT` /
`INBOUND_TRANSPARENT_PORT`. A mismatch redirects traffic to a dead port.

### Listener roles and interception (proxy-sidecar)

- `listener.roles` — which proxies run in proxy-sidecar mode. Empty (the
  default) runs **both** the reverse proxy (inbound) and the forward proxy
  (outbound). Set a subset to run a single shape: `roles: [forward]` is
  egress-only (the laptop TLS-bridge shape), `roles: [reverse]` is
  inbound-only JWT validation. `reverse_proxy_backend` is required only when
  the reverse role is active. Ignored outside proxy-sidecar mode.
- `listener.inbound_interception` — how inbound traffic reaches the reverse
  proxy's pipeline, with the reverse role active:
  - `reverse-proxy` (default) — the operator binds the sidecar on the agent's
    original port and relocates the agent to a free one ("port stealing"), so
    the Service needs no patching. Forwards to `reverse_proxy_backend`.
  - `transparent` — proxy-init PREROUTING-REDIRECTs inbound TCP to
    `listener.transparent_inbound_addr` (`:8083` by default); the listener
    recovers the port the client actually addressed via `SO_ORIGINAL_DST` and
    forwards there over loopback. The agent keeps its own port and every port
    it listens on is covered — not just the first declared one. Requires
    proxy-init (`NET_ADMIN`) and is Linux-only.
- `listener.transparent_proxy_addr` — the outbound transparent listener
  (`:8082` by default in the proxy-sidecar / lite presets) that
  enforce-redirect mode REDIRECTs the agent's bypass egress to. Binding it is
  harmless when nothing is redirected to it — cooperative HTTP_PROXY
  deployments simply never receive connections on it.

**Envoy-sidecar (`cortex-envoy`):**

| Port | Purpose |
|---|---|
| 15123 | Envoy outbound listener (iptables redirects here) |
| 15124 | Envoy inbound listener |
| 9090 | gRPC ext_proc (called by Envoy) |
| 9901 | Envoy admin |

## Choosing a binary

- **Default deployment**: use `cortex`. No Envoy, observable via
  agentop. Cooperative egress (HTTP_PROXY) needs no iptables; the always-on
  `enforce-redirect` egress guard and the opt-in transparent inbound listener
  both use [`proxy-init`](../deploy/proxy-init/).
- **Need ambient/transparent interception via Envoy**: use
  `cortex-envoy`. Requires the [`proxy-init`](../deploy/proxy-init/)
  iptables init container.
- **Size-constrained, no protocol-aware events needed**: use the
  `authbridge-lite` image — the `cortex` binary built with the
  `lite` profile from `scripts/profile-tags` (a sidecar
  minimum). Same listener layout, but without parsers/OPA — agentop
  will only see denial events and basic auth-level invocations, not
  full A2A/MCP/Inference protocol context.

## cpex builds

The `cpex` plugin links `libcpex_ffi.a` through cgo, so it is only in builds
of `cortex` made with cgo, the `cpex` build tag, and a profile that carries
the plugin. [`../scripts/fetch-cpex-ffi.sh`](../scripts/fetch-cpex-ffi.sh)
downloads and verifies the library pinned in `cortex-cpex/`.

- **`authbridge-cpex` image** — `cortex-cpex/Dockerfile` builds `cmd/cortex`
  with the `cpex` profile against the musl library, and installs it as
  `/usr/local/bin/cortex-cpex` so the entrypoint and container args are
  unchanged.
- **Laptop** — `make cortex-cpex` builds `cmd/cortex` with the `fullcpex`
  profile (`full` plus `cpex`) for the host (linux amd64/arm64, darwin arm64)
  to `bin/cortex-cpex/cortex`. It needs a C toolchain (Xcode Command Line
  Tools on macOS) and `cosign`. The binary is still called `cortex` and runs
  `--local` like the default build; `cortex --version` prints
  `cortex (cpex) <version>`.

The image used to run its own `main.go`, a reduced copy of `cortex`'s. With
the same config, the `cortex` build differs from it in these ways:

- It binds the transparent egress listener (`:8082`), as the `authbridge`
  image does, so `enforce-redirect` egress works. Nothing reaches it without
  `proxy-init`'s iptables rules.
- It logs as `binary=cortex` instead of `cortex-cpex`.
- It honors the options the old main ignored — `listener.roles`,
  `inbound_interception`, `skip_hosts`, `tls_bridge` — and serves `/v1/usage`.
  The cost ledger and session archive stay off in a pod.
- It builds the SPIFFE provider only when something consumes it — mTLS,
  `identity.type: spiffe`, or a cpex `workload.audience` — rather than whenever
  a `spiffe:` block is present.
- `--supervise` exists but is not used by the entrypoint.
