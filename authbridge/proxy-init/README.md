# proxy-init

The `proxy-init` container programs iptables rules for an
AuthBridge-injected pod. It runs once at pod startup as a Kubernetes
init container, then exits. It has two modes, selected by the `MODE`
env var:

| `MODE` | Used by | What it does |
|---|---|---|
| `redirect` (default) | `envoy-sidecar` | Transparently **REDIRECT**s pod traffic to the Envoy listeners. |
| `enforce-redirect` | `proxy-sidecar`, `lite` | Fail-closed egress guard that **captures**: REDIRECTs bypassing TCP — external **and** in-cluster (except in-cluster DNS) — to AuthBridge's transparent listener; DROPs non-TCP external egress; leaves in-cluster non-TCP (DNS/UDP) direct. |

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

## `enforce-redirect` mode (proxy-sidecar)

In `proxy-sidecar` mode the workload is configured with `HTTP_PROXY`
pointing at AuthBridge's forward proxy. On its own that is purely
cooperative — an app that ignores `HTTP_PROXY` (or sets `NO_PROXY`)
egresses directly and bypasses AuthBridge. `enforce-redirect` closes
that gap **by capturing** the bypass traffic instead of dropping it:
external TCP that did not go through the forward proxy is transparently
REDIRECTed to AuthBridge's **transparent listener** (`TRANSPARENT_PORT`,
default 8082), which recovers the original destination via
`SO_ORIGINAL_DST` and tunnels it through the same outbound pipeline.
Because nothing is dropped, agents that ignore `HTTP_PROXY` keep working
— which is what lets enforcement be always-on.

`init-iptables.sh` installs **two** chains, because `REDIRECT` is a
nat-table target but the nat table forbids `DROP` (`iptables` errors with
"the use of DROP is therefore inhibited"):

- **`nat` OUTPUT / `AB_REDIRECT`** (position 1): `RETURN` ztunnel mark
  `0x539`, the proxy UID (`--uid-owner $PROXY_UID`, avoids the loop),
  loopback, and **DNS-over-TCP** (`-p tcp --dport 53`) to each
  `/etc/resolv.conf` nameserver (so cluster name resolution stays direct);
  then `REDIRECT` all remaining **TCP** — external **and** in-cluster — to
  `TRANSPARENT_PORT`, so agent→in-cluster calls (e.g. agent→tool) are
  captured by the egress pipeline too.
- **`mangle` OUTPUT / `AB_NOTCP`** (position 1): the same UID/mark/loopback
  exemptions (plus `ESTABLISHED,RELATED` first, so UDP conntrack replies
  like DNS pass), then **DNS-over-UDP** (`-p udp --dport 53`) to each
  resolv.conf nameserver so cluster DNS keeps working; then `-p tcp -j
  RETURN` (TCP is handled by the nat REDIRECT) and a terminal `DROP` for
  all other **non-TCP** (UDP/QUIC), so HTTP/3 cannot bypass and non-DNS
  in-cluster UDP is dropped too — well-behaved clients fall back to TCP and
  get captured.

Because the OUTPUT hook order is `raw → mangle → nat → filter`, the
mangle chain drops non-TCP on its original destination while TCP falls
through to the nat REDIRECT. Both chains are inserted at position 1,
ahead of Istio's appended (`-A`) chains. The proxy's own re-originated
egress (`--uid-owner $PROXY_UID`, `RETURN`ed) falls through to
`ISTIO_OUTPUT` → ztunnel for transport mTLS under Istio ambient, and goes
out plain without a mesh — so capturing in-cluster TCP **composes with**
the mesh (AuthBridge does L7, ztunnel does transport) rather than
bypassing it. IPv6 mirrors apply the same rules. See
[`test-enforce-redirect.sh`](./test-enforce-redirect.sh), which proves
the capture, the preemption, and the non-TCP drop via packet counters.

> **DNS stays direct by following the pod's actual resolvers — no CIDR
> guessing.** The only thing left direct is DNS (`tcp/53` + `udp/53`) to the
> `nameserver` IPs in `/etc/resolv.conf`, which kubelet writes per the pod's
> `dnsPolicy`. This is cluster-agnostic by construction: it works whether the
> resolver is a Kind/OpenShift/EKS service ClusterIP (any service CIDR — incl.
> OpenShift's `172.30.0.0/16`, which is **outside** `10/8`) or a NodeLocal
> DNSCache at a link-local `169.254.x` address. The script logs the resolved
> nameservers at startup; override the file path with `RESOLV_CONF` (mainly
> for tests). There is **no** in-cluster CIDR knob — `enforce-redirect` no
> longer needs one. (A prior `CLUSTER_CIDRS` env was removed; its `10.0.0.0/8`
> default silently dropped DNS on OpenShift, where the resolver sits outside
> `10/8`.)

> **`enforce-redirect` intentionally ignores `OUTBOUND_PORTS_EXCLUDE`** (a
> `redirect`-mode knob). Any destination previously bypassed that way —
> e.g. a direct LLM endpoint at `host.docker.internal:11434` — is now
> captured (external TCP) or dropped (external non-TCP). In-cluster TCP is
> captured as well (only DNS to the resolvers stays direct). That is the
> point: `enforce-redirect` closes direct-egress holes, and the DNS
> exemption is scoped to `port 53` to the resolver IPs — not a TCP bypass.

## iptables backend

The script auto-detects `iptables-legacy` vs `iptables-nft` and uses
whichever the host kernel exposes. Override with `IPTABLES_CMD` (and
`IP6TABLES_CMD`) if needed.

## Environment variables

| Variable | Default | Mode | Purpose |
|---|---|---|---|
| `MODE` | `redirect` | all | `redirect` (envoy-sidecar) or `enforce-redirect` (proxy-sidecar / lite) |
| `PROXY_UID` | `1337` | all | UID of the AuthBridge sidecar process; exempted from redirect |
| `PROXY_PORT` | `15123` | redirect | AuthBridge outbound listener port |
| `INBOUND_PROXY_PORT` | `15124` | redirect | AuthBridge inbound listener port |
| `TRANSPARENT_PORT` | `8082` | enforce-redirect | AuthBridge transparent listener port; REDIRECT target for captured external TCP egress |
| `OUTBOUND_PORTS_EXCLUDE` | (empty) | redirect | Comma-separated outbound port list to skip (e.g. `8080`) |
| `INBOUND_PORTS_EXCLUDE` | (empty) | redirect | Comma-separated inbound port list to skip |
| `POD_IP` | (required in `redirect`) | redirect | Set via Downward API; DNAT target for ambient-mesh inbound. Not used by `enforce-redirect`. |
| `RESOLV_CONF` | `/etc/resolv.conf` | enforce-redirect | Path read at init for `nameserver` IPs; DNS (`tcp/53` + `udp/53`) to those IPs is left direct (IPv4→`iptables`, IPv6→`ip6tables`). Override mainly for tests. |
| `IPTABLES_CMD` | auto-detected | all | Override iptables binary (`iptables-legacy` / `iptables-nft`) |
| `IP6TABLES_CMD` | derived from `IPTABLES_CMD` | enforce-redirect | Override ip6tables binary |

## Required Kubernetes capabilities

The container needs `NET_ADMIN` and `NET_RAW` capabilities and runs as
UID 0 — but **not** privileged mode. The operator's webhook
sets up the SecurityContext correctly when injecting the init
container.

## Building

```sh
make docker-build-init
make load-image          # load into a kind cluster
```

The image is published from CI as
`ghcr.io/rossoctl/rossocortex/proxy-init:<tag>` (build defined
in [`.github/workflows/build.yaml`](../../.github/workflows/build.yaml)).

## Testing

[`test-enforce-redirect.sh`](./test-enforce-redirect.sh) validates
`enforce-redirect` mode in a private network namespace (`unshare --net`):
it asserts the `AB_REDIRECT` / `AB_NOTCP` rule structure, proves external
TCP is captured to `TRANSPARENT_PORT` while preempting a simulated Istio
ambient `nat OUTPUT` REDIRECT, and proves external UDP is dropped — all via
packet counters. Requires root + iptables-nft on Linux (runs on CI; not
macOS):

```sh
sudo ./test-enforce-redirect.sh
```

## Where it gets injected

The operator's mutating webhook injects the proxy-init
container automatically:

- `redirect` mode (`MODE` unset) when the resolved AuthBridge mode is
  `envoy-sidecar`.
- `enforce-redirect` mode (`MODE=enforce-redirect`) when the resolved
  AuthBridge mode is `proxy-sidecar` / `lite` — the transparent listener
  in those images receives the captured egress. This is always-on for
  those modes (the operator injects it unconditionally).

See
[`authbridge/demos/weather-agent/demo-ui-advanced.md`](../demos/weather-agent/demo-ui-advanced.md)
for an end-to-end demo and
[`authbridge/demos/token-exchange-routes/README.md`](../demos/token-exchange-routes/README.md)
for the route-config reference.
