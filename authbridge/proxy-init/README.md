# proxy-init

The `proxy-init` container programs iptables rules for an
AuthBridge-injected pod. It runs once at pod startup as a Kubernetes
init container, then exits. It has two modes, selected by the `MODE`
env var:

| `MODE` | Used by | What it does |
|---|---|---|
| `redirect` (default) | `envoy-sidecar` | Transparently **REDIRECT**s pod traffic to the Envoy listeners. |
| `enforce-drop` | `proxy-sidecar` | Fail-closed egress guard — **DROP**s any egress that bypasses the forward proxy. |

## `redirect` mode (envoy-sidecar)

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

## `enforce-drop` mode (proxy-sidecar)

In `proxy-sidecar` mode the workload is configured with `HTTP_PROXY`
pointing at AuthBridge's forward proxy. On its own that is purely
cooperative — an app that ignores `HTTP_PROXY` (or sets `NO_PROXY`)
egresses directly and bypasses AuthBridge. `enforce-drop` closes that
gap **without** transparently redirecting (you cannot REDIRECT raw
traffic into a CONNECT forward proxy): it installs a fail-closed guard
that DROPs any direct egress, forcing all external traffic through the
proxy regardless of whether the app honors `HTTP_PROXY`.

`init-iptables.sh` builds a dedicated `AB_EGRESS` chain hooked from
**`mangle` OUTPUT at position 1**, with this order:

1. `RETURN` ztunnel's own sockets (fwmark `0x539`) — keeps the mesh path working; a no-op when ambient is absent.
2. `RETURN` the proxy's own re-originated egress (`--uid-owner $PROXY_UID`, default 1337).
3. `RETURN` loopback (the app → proxy hop) and in-cluster CIDRs (`CLUSTER_CIDRS`, mesh/DNS).
4. `DROP` everything else — direct external egress, including UDP (QUIC/HTTP-3).

An IPv6 mirror drops external v6 egress (allowing loopback, link-local,
the proxy UID, and `CLUSTER_CIDRS6`).

**Why `mangle` OUTPUT, not `filter`:** when Istio ambient is active it
installs an in-pod `nat OUTPUT` REDIRECT (`ISTIO_OUTPUT` → ztunnel
`:15001`). The netfilter OUTPUT hook order is `raw → mangle → nat →
filter`, so a DROP in `mangle` evaluates the original destination and
fires **before** ambient's nat redirect can rewrite it; a DROP in
`filter` would run after nat and be defeated. `-I 1` also places the
chain ahead of Istio's appended (`-A`) mangle chain. This makes the
guard robust with no ambient, in-pod ambient, or node-level ambient.
See [`test-enforce-drop.sh`](./test-enforce-drop.sh), which proves the
preemption via packet counters.

## iptables backend

The script auto-detects `iptables-legacy` vs `iptables-nft` and uses
whichever the host kernel exposes. Override with `IPTABLES_CMD` (and
`IP6TABLES_CMD`) if needed.

## Environment variables

| Variable | Default | Mode | Purpose |
|---|---|---|---|
| `MODE` | `redirect` | both | `redirect` (envoy-sidecar) or `enforce-drop` (proxy-sidecar) |
| `PROXY_UID` | `1337` | both | UID of the AuthBridge sidecar process; exempted from redirect / drop |
| `PROXY_PORT` | `15123` | redirect | AuthBridge outbound listener port |
| `INBOUND_PROXY_PORT` | `15124` | redirect | AuthBridge inbound listener port |
| `OUTBOUND_PORTS_EXCLUDE` | (empty) | redirect | Comma-separated outbound port list to skip (e.g. `8080`) |
| `INBOUND_PORTS_EXCLUDE` | (empty) | redirect | Comma-separated inbound port list to skip |
| `POD_IP` | (required in `redirect`) | redirect | Set via Downward API; DNAT target for ambient-mesh inbound. Not used by `enforce-drop`. |
| `CLUSTER_CIDRS` | `10.0.0.0/8` | enforce-drop | Comma-separated in-cluster CIDRs allowed direct (pods/services/DNS) |
| `CLUSTER_CIDRS6` | (empty) | enforce-drop | IPv6 in-cluster CIDRs (dual-stack); empty drops all external v6 egress |
| `IPTABLES_CMD` | auto-detected | both | Override iptables binary (`iptables-legacy` / `iptables-nft`) |
| `IP6TABLES_CMD` | derived from `IPTABLES_CMD` | enforce-drop | Override ip6tables binary |

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

## Testing

[`test-enforce-drop.sh`](./test-enforce-drop.sh) validates `enforce-drop`
mode in a private network namespace (`unshare --net`): it asserts the
`AB_EGRESS` rule structure and proves the `mangle` DROP preempts a
simulated Istio ambient `nat OUTPUT` REDIRECT via packet counters.
Requires root + iptables-nft on Linux (runs on CI; not macOS):

```sh
sudo ./test-enforce-drop.sh
```

## Where it gets injected

The kagenti-operator's mutating webhook injects the proxy-init
container automatically:

- `redirect` mode (`MODE` unset) when the resolved AuthBridge mode is
  `envoy-sidecar`.
- `enforce-drop` mode (`MODE=enforce-drop`) when `proxy-sidecar`
  egress enforcement is enabled (opt-in). _The operator wiring that
  sets this lands in the follow-up kagenti-operator PR; this PR only
  adds the mode to the image._

See
[`authbridge/demos/weather-agent/demo-ui-advanced.md`](../demos/weather-agent/demo-ui-advanced.md)
for an end-to-end demo and
[`authbridge/demos/token-exchange-routes/README.md`](../demos/token-exchange-routes/README.md)
for the route-config reference.
