#!/bin/sh
#
# AuthProxy iptables initialization — Istio ambient mesh coexistence
#
# This script sets up iptables rules for the AuthProxy Envoy sidecar to intercept
# outbound traffic (for token injection) and inbound traffic (for JWT validation).
# It is designed to coexist with Istio's ambient mesh ztunnel inside the same pod
# network namespace.
#
# ─── Background ───────────────────────────────────────────────────────────────
#
# Istio ambient mesh runs a ztunnel DaemonSet that holds in-pod sockets inside
# each enrolled pod's netns, listening on:
#   15001 — outbound traffic capture
#   15006 — inbound plaintext delivery
#   15008 — HBONE (mTLS tunneling)
#   15053 — DNS proxy
#
# ztunnel runs as UID 0, GID 1337, and sets fwmark 0x539 on its sockets.
#
# Istio's CNI agent installs its own iptables rules in the pod netns:
#   nat OUTPUT:      ISTIO_OUTPUT  (redirects outbound to ztunnel 15001)
#   nat PREROUTING:  ISTIO_PRERT  (redirects inbound to ztunnel 15006)
#   mangle:          connmark tracking (0x111 for return traffic)
#
# ─── Chain ordering ──────────────────────────────────────────────────────────
#
# This script uses -I (insert at position 1) so our chains always run BEFORE
# Istio's chains, which use -A (append). The resulting order is:
#
#   nat OUTPUT:     1. PROXY_OUTPUT   (ours)
#                   2. ISTIO_OUTPUT   (Istio's)
#
#   nat PREROUTING: 1. PROXY_INBOUND (ours)
#                   2. ISTIO_PRERT   (Istio's)
#
#   mangle OUTPUT:  1. our MARK rule
#                   2. ISTIO_OUTPUT   (Istio's connmark restore)
#
# This ordering is stable regardless of timing because:
#   - Kubernetes runs CNI plugins before init containers
#   - Istio CNI agent sets up ambient rules asynchronously (pod event watch)
#   - Both use -A (append); we use -I 1 (insert first) — so our rules land first
#     whether Istio's rules exist yet or not
#
# ─── Outbound flow (app → external service) ──────────────────────────────────
#
# Without ambient mesh:
#   1. App connects to ClusterIP:port
#   2. PROXY_OUTPUT catches → REDIRECT to Envoy outbound (15123)
#   3. Envoy ext_proc injects auth token
#   4. Envoy connects to original destination (original_dst cluster)
#   5. PROXY_OUTPUT: UID 1337 → RETURN (skip our rules)
#   6. Packet exits pod → kube-proxy DNAT → target pod
#
# With ambient mesh:
#   Steps 1-5 same as above, then:
#   6. ISTIO_OUTPUT catches (mark != 0x539) → REDIRECT to ztunnel (15001)
#   7. ztunnel wraps in HBONE (mTLS) → target pod's ztunnel
#   8. Target ztunnel delivers plaintext to target app
#
# Key interaction: Envoy's outbound connection (UID 1337) RETURNs from
# PROXY_OUTPUT and falls through to ISTIO_OUTPUT, which redirects to ztunnel.
# This is intentional — ztunnel provides mTLS for the connection.
#
# ─── Inbound flow (external → app in this pod) ──────────────────────────────
#
# Without ambient mesh (traffic arrives via network):
#   1. Packet arrives at pod → PREROUTING
#   2. PROXY_INBOUND catches → REDIRECT to Envoy inbound (15124)
#   3. Envoy ext_proc validates JWT
#   4. Envoy connects to original destination (app port) via original_dst cluster
#   5. App receives request
#
# With ambient mesh (traffic arrives via HBONE):
#   1. Source ztunnel sends HBONE to this pod's ztunnel (port 15008)
#   2. ztunnel terminates mTLS, delivers plaintext to local app
#   3. Delivery goes through OUTPUT chain (ztunnel uses in-pod socket)
#      - ztunnel preserves original client IP as source (IP_TRANSPARENT)
#      - ztunnel's socket has fwmark 0x539; ztunnel runs as UID 0
#   4. PROXY_OUTPUT mark-based rule matches:
#        mark=0x539, UID!=1337, dst-type=LOCAL → REDIRECT to Envoy inbound (15124)
#   5. Envoy ext_proc validates JWT
#   6. Envoy (UID 1337) connects to original destination (app port)
#      - mangle OUTPUT sets mark 0x539 (UID 1337 + dst-type LOCAL)
#      - PROXY_OUTPUT rule 1: mark ✓ but UID IS 1337 → skip
#      - PROXY_OUTPUT rule 2: mark 0x539 → RETURN
#      - ISTIO_OUTPUT: sees mark 0x539 → ACCEPT
#   7. App receives request
#
# Key interactions:
#   - The mark-based rule (step 4) MUST use --dst-type LOCAL. Without it, ztunnel's
#     outbound HBONE connections (to remote pod IPs, also mark 0x539, also UID!=1337)
#     would match and get redirected to the inbound listener, causing
#     InvalidContentType errors (Envoy speaks HTTP, ztunnel expects mTLS).
#   - route_localnet=1 is required because REDIRECT in OUTPUT changes dst to 127.0.0.1,
#     but ztunnel's delivery has a non-local source IP (original client). Without
#     route_localnet, the kernel treats 127.0.0.1 as martian during re-routing after
#     NAT and silently drops the SYN (visible as OutNoRoutes in /proc/net/snmp).
#   - The mangle rule (step 6) prevents a loop: without mark 0x539, ISTIO_OUTPUT would
#     redirect Envoy's local delivery back to ztunnel (15001). Mangle OUTPUT runs
#     before nat OUTPUT in the netfilter hook ordering.
#
# ─── Debugging tips ──────────────────────────────────────────────────────────
#
#   conntrack -L               — shows actual REDIRECT targets and connection states
#   /proc/net/snmp OutNoRoutes — reveals routing failures (route_localnet issue)
#   iptables-legacy-save       — dump legacy iptables rules
#   curl 127.0.0.1:9901/stats  — Envoy listener/cluster stats
#   ztunnel logs               — connections logged only on completion, not start
#
# ─── iptables backend selection ─────────────────────────────────────────────
#
# Alpine's default `iptables` command uses the nf_tables backend (iptables-nft).
# Many Kubernetes distributions (Kind, kubeadm, EKS with kube-proxy) and Istio
# use the legacy backend (iptables-legacy). If we set rules in the nft tables
# but the cluster's networking stack uses legacy tables, our PREROUTING rules
# have no effect — traffic bypasses Envoy's inbound listener entirely.
#
# This script auto-detects the correct backend by checking if iptables-legacy
# is available and can manipulate the nat table in this network namespace.
# Override with IPTABLES_CMD env var if needed.
#
# ─────────────────────────────────────────────────────────────────────────────

set -e

# --- Auto-detect iptables backend ---
# Prefer iptables-legacy for maximum compatibility with Kubernetes networking.
# The nft backend sets rules in a different netfilter table that may not be
# processed for PREROUTING when the cluster uses legacy iptables.
detect_iptables_cmd() {
  if [ -n "${IPTABLES_CMD:-}" ]; then
    echo "${IPTABLES_CMD}"
    return
  fi
  if command -v iptables-legacy >/dev/null 2>&1 && \
     iptables-legacy -t nat -L -n >/dev/null 2>&1; then
    echo "iptables-legacy"
  else
    echo "iptables"
  fi
}

IPT=$(detect_iptables_cmd)
echo "Using iptables command: ${IPT} ($(${IPT} --version 2>/dev/null || echo 'unknown version'))"

PROXY_PORT="${PROXY_PORT:-15123}"
INBOUND_PROXY_PORT="${INBOUND_PROXY_PORT:-15124}"
PROXY_UID="${PROXY_UID:-1337}"
SSH_PORT="${SSH_PORT:-22}"
OUTBOUND_PORTS_EXCLUDE="${OUTBOUND_PORTS_EXCLUDE:-}"
INBOUND_PORTS_EXCLUDE="${INBOUND_PORTS_EXCLUDE:-}"

# Istio ztunnel defaults
ZTUNNEL_HBONE_PORT="${ZTUNNEL_HBONE_PORT:-15008}"
ZTUNNEL_MARK="${ZTUNNEL_MARK:-0x539/0xfff}"  # 0x539 = 1337 decimal, ztunnel's socket fwmark
ISTIO_HEALTH_PROBE_SRC="${ISTIO_HEALTH_PROBE_SRC:-169.254.7.127}"

# Required for inbound ambient mesh flow. REDIRECT in OUTPUT changes dst to
# 127.0.0.1, but ztunnel's transparent delivery preserves the original client IP
# as the packet source. The kernel's ip_route_me_harder() re-routes the packet
# after NAT, and with route_localnet=0 (default) it considers 127.0.0.1 a martian
# address and drops the packet. Istio sidecar mode uses the same setting.
# Requires privileged init container (/proc/sys is read-only otherwise).
sysctl -w net.ipv4.conf.all.route_localnet=1

# =============================================================================
# OUTBOUND traffic interception (nat OUTPUT)
# =============================================================================

echo "Setting up iptables rules for outbound traffic interception..."

# Create custom chain (ignore error if it already exists)
${IPT} -t nat -N PROXY_OUTPUT 2>/dev/null || true

# Flush any existing rules in our chain to ensure idempotency
${IPT} -t nat -F PROXY_OUTPUT 2>/dev/null || true

# --- Rule 1: Intercept ztunnel's inbound delivery for JWT validation ---
# When ambient mesh is active, ztunnel terminates inbound HBONE and delivers
# plaintext to the local app via its in-pod socket. This delivery goes through
# the OUTPUT chain (not PREROUTING) because ztunnel creates a new local connection
# from within the pod netns.
#
# We match on three criteria to identify this traffic:
#   mark 0x539  — ztunnel sets this fwmark on all its sockets
#   UID != 1337 — excludes our own Envoy (which also gets 0x539 via mangle below)
#   dst-type LOCAL — only local delivery (to app in this pod)
#
# The --dst-type LOCAL constraint is critical: ztunnel uses mark 0x539 for BOTH
# its inbound delivery (dst = local app port) AND its outbound HBONE connections
# (dst = remote pod IP on port 15008). Without --dst-type LOCAL, outbound HBONE
# would be hijacked into the inbound listener, where Envoy speaks plain HTTP
# back to ztunnel which expects mTLS — producing InvalidContentType errors.
${IPT} -t nat -A PROXY_OUTPUT -m mark --mark ${ZTUNNEL_MARK} -m owner ! --uid-owner "${PROXY_UID}" -m addrtype --dst-type LOCAL -p tcp -j REDIRECT --to-ports "${INBOUND_PROXY_PORT}"

# --- Rule 2: Skip all remaining ztunnel traffic ---
# After rule 1 captured ztunnel's inbound delivery, any other ztunnel traffic
# (outbound HBONE, DNS proxy, etc.) must pass through unmodified. Matching on
# fwmark 0x539 is more robust than excluding individual port numbers — it covers
# all ztunnel sockets (15001, 15006, 15008, 15053, and any future ports).
${IPT} -t nat -A PROXY_OUTPUT -m mark --mark ${ZTUNNEL_MARK} -j RETURN

# --- Rule 3: Skip Envoy's own outbound connections ---
# Envoy (UID 1337) makes outbound connections after processing via ext_proc.
# These must RETURN from PROXY_OUTPUT so they fall through to Istio's
# ISTIO_OUTPUT chain (position 2 in OUTPUT), which redirects them to ztunnel
# (port 15001) for HBONE/mTLS wrapping. This is the desired behavior —
# ztunnel provides the mTLS transport layer for outbound connections.
${IPT} -t nat -A PROXY_OUTPUT -m owner --uid-owner "${PROXY_UID}" -j RETURN

# --- Rules 4-5: Exclusions ---
${IPT} -t nat -A PROXY_OUTPUT -p tcp --dport "${SSH_PORT}" -j RETURN
${IPT} -t nat -A PROXY_OUTPUT -p tcp -d 127.0.0.1/32 -j RETURN

if [ -n "${OUTBOUND_PORTS_EXCLUDE}" ]; then
  for port in $(echo "${OUTBOUND_PORTS_EXCLUDE}" | tr ',' ' '); do
    echo "Excluding outbound port ${port} from redirection"
    ${IPT} -t nat -A PROXY_OUTPUT -p tcp --dport "${port}" -j RETURN
  done
fi

# --- Catch-all: redirect remaining outbound TCP to Envoy outbound listener ---
${IPT} -t nat -A PROXY_OUTPUT -p tcp -j REDIRECT --to-port "${PROXY_PORT}"

# Insert PROXY_OUTPUT at position 1 in the OUTPUT chain. Istio's ISTIO_OUTPUT
# (appended by CNI agent) will be at position 2. This ensures our rules evaluate
# first: we handle app traffic and ztunnel delivery, then RETURN lets Envoy's
# outbound connections fall through to ISTIO_OUTPUT for ztunnel wrapping.
if ! ${IPT} -t nat -C OUTPUT -p tcp -j PROXY_OUTPUT 2>/dev/null; then
  ${IPT} -t nat -I OUTPUT 1 -p tcp -j PROXY_OUTPUT
fi

# --- Mangle rule: prevent Envoy→app loop through ISTIO_OUTPUT ---
# After inbound JWT validation, Envoy (UID 1337) connects to the local app.
# Without this mark, ISTIO_OUTPUT would see mark != 0x539 and redirect Envoy's
# local connection to ztunnel (port 15001), creating an infinite loop:
#   Envoy → ISTIO_OUTPUT → ztunnel 15001 → HBONE to self → ztunnel 15008 → ...
#
# Setting mark 0x539 makes ISTIO_OUTPUT's rule (mark != 0x539 → REDIRECT) skip
# this traffic, and ISTIO_OUTPUT's loopback rule (!dst 127.0.0.1 + -o lo → ACCEPT)
# lets it through to the app.
#
# This rule is in the mangle table because mangle OUTPUT runs before nat OUTPUT
# in netfilter hook ordering, so the mark is set before ISTIO_OUTPUT evaluates it.
#
# The --dst-type LOCAL constraint ensures we only mark Envoy's local delivery
# (to the app in this pod), not Envoy's outbound connections to external services.
# Outbound connections must NOT have mark 0x539, otherwise ISTIO_OUTPUT would skip
# them and they'd bypass ztunnel's mTLS wrapping.
${IPT} -t mangle -I OUTPUT 1 -m owner --uid-owner "${PROXY_UID}" -m addrtype --dst-type LOCAL -p tcp -j MARK --set-mark ${ZTUNNEL_MARK}

echo "Outbound iptables rules configured successfully"
echo "Outbound traffic will be redirected to port ${PROXY_PORT}"

# =============================================================================
# INBOUND traffic interception (nat PREROUTING)
# =============================================================================
#
# This handles inbound traffic arriving via the network interface (PREROUTING path).
# In non-ambient mode, all inbound traffic takes this path. In ambient mode, only
# traffic from non-mesh sources (e.g., NodePort, LoadBalancer) takes this path;
# mesh traffic arrives via ztunnel's HBONE delivery through OUTPUT (handled above
# by the mark-based rule in PROXY_OUTPUT).
#
# Interaction with Istio's ISTIO_PRERT:
#   PREROUTING: 1. PROXY_INBOUND (ours, -I position 1)
#               2. ISTIO_PRERT   (Istio's, -A appended)
#
#   Since PROXY_INBOUND runs first and terminates with REDIRECT for app traffic,
#   ISTIO_PRERT never sees it. ISTIO_PRERT's redirect-to-15006 only applies to
#   traffic that RETURNs from PROXY_INBOUND (excluded ports, health probes, etc.).
#   This is safe because ztunnel's port 15006 handles traffic not meant for Envoy.

echo "Setting up iptables rules for inbound traffic interception..."

${IPT} -t nat -N PROXY_INBOUND 2>/dev/null || true

${IPT} -t nat -F PROXY_INBOUND 2>/dev/null || true

# Istio uses 169.254.7.127 as a synthetic source for health probes (kubelet
# health checks are rewritten by ztunnel). These must bypass Envoy to avoid
# false health check failures.
${IPT} -t nat -A PROXY_INBOUND -s "${ISTIO_HEALTH_PROBE_SRC}/32" -p tcp -j RETURN

# Exclude sidecar/infrastructure ports — traffic to these ports is for Envoy
# or ext_proc themselves, not for the app. Redirecting would create loops.
${IPT} -t nat -A PROXY_INBOUND -p tcp --dport "${PROXY_PORT}" -j RETURN
${IPT} -t nat -A PROXY_INBOUND -p tcp --dport "${INBOUND_PROXY_PORT}" -j RETURN
${IPT} -t nat -A PROXY_INBOUND -p tcp --dport 9090 -j RETURN
${IPT} -t nat -A PROXY_INBOUND -p tcp --dport 9901 -j RETURN

${IPT} -t nat -A PROXY_INBOUND -p tcp --dport "${SSH_PORT}" -j RETURN

# Exclude ztunnel's HBONE port. Remote ztunnel sends mTLS tunnels to this port
# from the network — it must reach ztunnel's in-pod socket directly, not Envoy.
# (Ports 15001 and 15006 are not excluded here because they only receive traffic
# via iptables REDIRECT from OUTPUT/PREROUTING, never directly from the network.)
${IPT} -t nat -A PROXY_INBOUND -p tcp --dport "${ZTUNNEL_HBONE_PORT}" -j RETURN

if [ -n "${INBOUND_PORTS_EXCLUDE}" ]; then
  for port in $(echo "${INBOUND_PORTS_EXCLUDE}" | tr ',' ' '); do
    echo "Excluding inbound port ${port} from redirection"
    ${IPT} -t nat -A PROXY_INBOUND -p tcp --dport "${port}" -j RETURN
  done
fi

# Catch-all: redirect remaining inbound TCP to Envoy inbound listener
${IPT} -t nat -A PROXY_INBOUND -p tcp -j REDIRECT --to-port "${INBOUND_PROXY_PORT}"

# Insert PROXY_INBOUND at position 1 in PREROUTING. Istio's ISTIO_PRERT
# (appended by CNI agent) will be at position 2.
if ! ${IPT} -t nat -C PREROUTING -p tcp -j PROXY_INBOUND 2>/dev/null; then
  ${IPT} -t nat -I PREROUTING 1 -p tcp -j PROXY_INBOUND
fi

echo "Inbound iptables rules configured successfully"
echo "Inbound traffic will be redirected to port ${INBOUND_PROXY_PORT}"
echo "Istio ztunnel compatibility enabled"
