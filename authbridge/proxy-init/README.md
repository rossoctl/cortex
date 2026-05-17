# proxy-init

The `proxy-init` container sets up iptables rules so that traffic in
and out of an AuthBridge-injected pod is transparently redirected to
the AuthBridge sidecar's listeners. It runs once at pod startup as a
Kubernetes init container, then exits.

**`proxy-init` is only used in `envoy-sidecar` mode.** In
`proxy-sidecar` mode (the AuthBridge cluster default after
kagenti-operator#361) traffic interception is done via `HTTP_PROXY`
env vars on the workload container — no iptables, no init container.

## What it does

`init-iptables.sh` writes iptables rules that:

- **Outbound** — Redirect traffic leaving the workload container to
  AuthBridge's outbound listener (port 15123). Adds an exclusion for
  the AuthBridge sidecar's own UID (1337) so its traffic doesn't loop
  back into itself.
- **Inbound** — Redirect traffic arriving at the workload container's
  service port to AuthBridge's inbound listener (port 15124).
- **Istio ambient coexistence** — Cooperates with ztunnel by
  preserving the Istio fwmark (0x539) and respecting the HBONE port
  (15008). Designed to work alongside `istio.io/dataplane-mode:
  ambient`.
- **Configurable exclusions** — Honors `OUTBOUND_PORTS_EXCLUDE` and
  `INBOUND_PORTS_EXCLUDE` env vars (commonly used to exclude
  Keycloak's port 8080 to avoid token-exchange loops).

The script auto-detects `iptables-legacy` vs `iptables-nft` and uses
whichever the host kernel exposes. Override with `IPTABLES_CMD` if
needed.

## Environment variables

| Variable | Default | Purpose |
|---|---|---|
| `PROXY_PORT` | `15123` | AuthBridge outbound listener port |
| `INBOUND_PROXY_PORT` | `15124` | AuthBridge inbound listener port |
| `PROXY_UID` | `1337` | UID of the AuthBridge sidecar process; excluded from outbound redirect |
| `OUTBOUND_PORTS_EXCLUDE` | (empty) | Comma-separated outbound port list to skip (e.g. `8080`) |
| `INBOUND_PORTS_EXCLUDE` | (empty) | Comma-separated inbound port list to skip |
| `POD_IP` | (required) | Set via Downward API; used as the DNAT target for ambient-mesh inbound |
| `IPTABLES_CMD` | auto-detected | Override iptables binary (`iptables-legacy` / `iptables-nft`) |

## Required Kubernetes capabilities

The container needs `NET_ADMIN` and `NET_RAW` capabilities and runs as
UID 0 — but **not** privileged mode. The kagenti-operator's webhook
sets up the SecurityContext correctly when injecting the init
container.

## Building

```sh
make docker-build-init
make load-image          # load into a kind cluster
```

The image is published from CI as
`ghcr.io/kagenti/kagenti-extensions/proxy-init:<tag>` (build defined
in [`.github/workflows/build.yaml`](../../.github/workflows/build.yaml)).

## Where it gets injected

The kagenti-operator's mutating webhook injects the proxy-init
container automatically when the resolved AuthBridge mode is
`envoy-sidecar`. See
[`authbridge/demos/weather-agent/demo-ui-advanced.md`](../demos/weather-agent/demo-ui-advanced.md)
for an end-to-end demo and
[`authbridge/demos/token-exchange-routes/README.md`](../demos/token-exchange-routes/README.md)
for the route-config reference.
