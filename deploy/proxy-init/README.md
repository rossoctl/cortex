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
| `INBOUND_TRANSPARENT_PORT` | (empty = off) | enforce-redirect | AuthBridge **inbound** transparent listener port; PREROUTING REDIRECT target. Opt-in — see below. Requires `POD_IP`. |
| `SIDECAR_PORTS_EXCLUDE` | `8081,9091,9093,9094` | enforce-redirect + inbound | AuthBridge's own listeners, exempted from the inbound REDIRECT. Override when the forward proxy is not on 8081. |
| `OUTBOUND_PORTS_EXCLUDE` | (empty) | redirect | Comma-separated outbound port list to skip (e.g. `8080`) |
| `INBOUND_PORTS_EXCLUDE` | (empty) | redirect + enforce-redirect w/ inbound | Comma-separated inbound app-port list to skip validation for (e.g. an oauth-proxy doing its own auth) |
| `POD_IP` | required in `redirect` | both | Set via Downward API (`status.podIP`); DNAT target for ambient-mesh inbound |
| `POD_IPS` | falls back to `POD_IP` | enforce-redirect w/ inbound | Set via Downward API (`status.podIPs`). Supplies a per-family DNAT target so a dual-stack pod covers ambient on BOTH families — `POD_IP` alone is the primary address, leaving the other family's HBONE delivery unvalidated |

With `INBOUND_TRANSPARENT_PORT` set, **either** `POD_IP` or `POD_IPS` satisfies the
requirement (the guard tests the resolved list). Supplying neither is fail-closed.
| `RESOLV_CONF` | `/etc/resolv.conf` | enforce-redirect | Path read at init for `nameserver` IPs; DNS (`tcp/53` + `udp/53`) to those IPs is left direct (IPv4→`iptables`, IPv6→`ip6tables`). Override mainly for tests. |
| `IPTABLES_CMD` | auto-detected | all | Override iptables binary (`iptables-legacy` / `iptables-nft`) |
| `IP6TABLES_CMD` | derived from `IPTABLES_CMD` | enforce-redirect | Override ip6tables binary |

## Transparent inbound (opt-in)

Setting `INBOUND_TRANSPARENT_PORT` under `MODE=enforce-redirect` adds the
**inbound** counterpart of the egress guard, so JWT validation cannot be
sidestepped by another pod dialing the agent's real port. Without it,
`enforce-redirect` is egress-only.

Inbound arrives by two capturable paths and both are covered — handling only the
first would silently wave all mesh traffic through:

| Path | Netfilter hook | Rule |
|---|---|---|
| Plain network (ClusterIP / NodePort / non-mesh pod) | `nat PREROUTING` | `AB_INBOUND` chain, inserted at position 1 to precede Istio's `ISTIO_PRERT` |
| Istio ambient HBONE | `nat OUTPUT` | ztunnel terminates mTLS and re-originates a LOCAL connection, so PREROUTING never runs. A mark-based `DNAT` at the **head** of `AB_REDIRECT` — it must precede that chain's ztunnel-mark `RETURN`, which would otherwise let every mesh-delivered request through unvalidated. |

`POD_IP` is required (and its absence is fail-closed at init) because the ambient
rule DNATs to it. `REDIRECT` cannot be used there: it hardcodes the destination
to `127.0.0.1`, and ztunnel preserves the client IP via `IP_TRANSPARENT`, so the
resulting packet is dropped as martian without `route_localnet=1`.

**The exemptions apply to both hooks.** Every port and source exempted from the
PREROUTING chain is also exempted on the ambient path, emitted from a single
`emit_inbound_exemptions` function. This is not incidental: ztunnel delivers via
`OUTPUT`, so an exemption living only in `AB_INBOUND` is a silent no-op for mesh
traffic — a JWT-gated `:9091` crash-loops the pod, and a captured `:8443` breaks
an oauth-proxy doing its own auth. The redirect-mode rules drifted in exactly
this way and needed a second hand-maintained copy in `PROXY_OUTPUT`.

`RETURN` rather than `ACCEPT`, so an exempt port still falls through to Istio's
appended chain and keeps ambient mTLS. That is also why the exemptions cannot be
a shared sub-chain: a sub-chain `RETURN` resumes in the caller, landing on the
terminal `REDIRECT`/`DNAT` it was meant to skip.

On a dual-stack pod set `POD_IPS`; with only `POD_IP` the non-primary family's
ambient inbound is not captured, and init warns about it explicitly rather than
leaving it implicit.

Intra-pod loopback is deliberately **not** captured. Containers share a network
namespace and are a single entity to every network enforcement layer, so that
traffic is inside the trust boundary — and capturing it would break AuthBridge's
own forward hop, which targets `127.0.0.1:<recovered port>` by design. That also
means **the app must bind `0.0.0.0`**, not only its pod IP.

Pair with authbridge's `listener.inbound_interception: transparent` and
`listener.transparent_inbound_addr` (preset default `:8083`); the ports must
match or inbound traffic is redirected to a dead port.

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
`ghcr.io/rossoctl/cortex/proxy-init:<tag>` (build defined
in [`.github/workflows/build.yaml`](../.github/workflows/build.yaml)).

## Testing

[`test-enforce-redirect.sh`](./test-enforce-redirect.sh) validates
`enforce-redirect` mode in a private network namespace (`unshare --net`):
it asserts the `AB_REDIRECT` / `AB_NOTCP` rule structure, proves external
TCP is captured to `TRANSPARENT_PORT` while preempting a simulated Istio
ambient `nat OUTPUT` REDIRECT, and proves external UDP is dropped — all via
packet counters.

With transparent inbound it additionally asserts that `AB_INBOUND` lands at
`nat PREROUTING` position 1 with AuthBridge's own ports exempted (gating `9091`
would put kubelet probes behind JWT validation and crash-loop the pod), that the
ambient DNAT precedes `AB_REDIRECT`'s ztunnel-mark `RETURN`, that a missing
`POD_IP` is fail-closed, that the ambient path carries the SAME exemptions as the
PREROUTING chain and that they precede the DNAT, that every ambient DNAT rule
negates the proxy UID, that `POD_IPS` yields a DNAT for both families, and that
re-running init does not stack duplicate mark rules. Requires root + iptables-nft
on Linux (runs on CI; not macOS):

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
[`demos/weather-agent/demo-ui-advanced.md`](../demos/weather-agent/demo-ui-advanced.md)
for an end-to-end demo and
[`demos/token-exchange-routes/README.md`](../demos/token-exchange-routes/README.md)
for the route-config reference.
