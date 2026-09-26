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
#        mark=0x539, UID!=1337, dst-type=LOCAL → DNAT to Envoy inbound (POD_IP:15124)
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
#   - We use DNAT to POD_IP instead of REDIRECT to avoid the route_localnet problem.
#     REDIRECT in OUTPUT rewrites dst to 127.0.0.1, and when the source is a non-local
#     IP (ztunnel's IP_TRANSPARENT), the kernel treats it as a martian packet and drops
#     it unless route_localnet=1. DNAT to the pod's own IP avoids this because the pod
#     IP is a routable non-loopback address. SO_ORIGINAL_DST still works because
#     conntrack stores the pre-NAT destination identically for both REDIRECT and DNAT.
#   - The mangle rule (step 6) prevents a loop: without mark 0x539, ISTIO_OUTPUT would
#     redirect Envoy's local delivery back to ztunnel (15001). Mangle OUTPUT runs
#     before nat OUTPUT in the netfilter hook ordering.
#
# ─── Inbound flow, enforce-redirect + INBOUND_TRANSPARENT_PORT ───────────────
#
# Opt-in; off unless INBOUND_TRANSPARENT_PORT is set. Gives proxy-sidecar the
# inbound counterpart of the egress guard, so JWT validation cannot be sidestepped
# by another pod dialing the agent's real port. Two capturable paths:
#
#   Plain network:  PREROUTING -> AB_INBOUND -> REDIRECT to the inbound port ->
#                   AuthBridge recovers the app's port via SO_ORIGINAL_DST ->
#                   forwards to 127.0.0.1:<that port>.
#   Ambient HBONE:  ztunnel terminates mTLS and re-originates LOCALLY, so it
#                   appears in OUTPUT, not PREROUTING. Captured by a mark-based
#                   DNAT at the head of AB_REDIRECT (see setup_enforce_redirect),
#                   preceded by the SAME exemptions AB_INBOUND applies — both are
#                   emitted from emit_inbound_exemptions so they cannot drift.
#
# Intra-pod loopback is intentionally NOT captured: containers share a netns and
# are a single entity to every network enforcement layer, so that traffic is
# inside the trust boundary. See setup_transparent_inbound() for the full notes.
#
# ─── Debugging tips ──────────────────────────────────────────────────────────
#
#   conntrack -L               — shows actual DNAT/REDIRECT targets and connection states
#   iptables-legacy-save       — both Istio and AuthProxy use iptables-legacy
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
# This script auto-detects the correct backend by reading the kernel module
# table (/proc/modules): the legacy nat path needs the `iptable_nat` module,
# which is loaded on legacy clusters (Kind, kubeadm — kube-proxy uses it) and
# ABSENT on nft-only platforms (OpenShift/ROSA expose only nf_tables +
# nft_compat). Override with IPTABLES_CMD env var if needed.
#
# ─────────────────────────────────────────────────────────────────────────────

set -e

# --- Mode selection ---
# MODE selects the interception strategy:
#   redirect          (default) — envoy-sidecar: transparently REDIRECT pod
#                     traffic to the Envoy listeners (the behavior documented
#                     above).
#   enforce-redirect  — proxy-sidecar: a fail-closed egress guard that CAPTURES
#                     rather than drops. External TCP egress that bypasses the
#                     forward proxy is transparently REDIRECTed to AuthBridge's
#                     transparent listener (TRANSPARENT_PORT), which recovers the
#                     original destination via SO_ORIGINAL_DST and tunnels it
#                     through the same outbound pipeline. Non-TCP external egress
#                     (UDP/QUIC) is DROPped so it cannot bypass via HTTP/3.
#                     In-cluster + loopback + proxy-UID traffic is left direct.
#                     Nothing breaks for agents that ignore HTTP_PROXY — their
#                     traffic is captured, not dropped. See setup_enforce_redirect().
MODE="${MODE:-redirect}"
case "${MODE}" in
  redirect|enforce-redirect) ;;
  *) echo "ERROR: unknown MODE='${MODE}' (expected: redirect | enforce-redirect)" >&2; exit 1 ;;
esac

# --- Auto-detect iptables backend ---
# Select the backend that is actually FUNCTIONAL in this kernel, not merely
# listable. A readability probe (`iptables-legacy -t nat -L`) mis-selects legacy
# on nft-only nodes: the empty legacy nat table still lists fine, so legacy gets
# chosen and our rules land in a table nothing consults (silent fail-open). A
# write-probe is unreliable too — individual legacy ops succeed via nft_compat
# even where the full nat pipeline later fails. Instead we check whether the
# `iptable_nat` kernel module is loaded: present => legacy nat is in use, match
# it (Kind, kubeadm); absent => nf_tables is the only working path (OpenShift/
# ROSA, EKS-nft). /proc/modules is host-kernel-wide, readable from the pod netns,
# timing-independent (unlike matching Istio's not-yet-installed rules), and not
# foolable by nft_compat. Override with IPTABLES_CMD (the operator can set it
# per-platform). PROC_MODULES is overridable for tests.
PROC_MODULES="${PROC_MODULES:-/proc/modules}"
detect_iptables_cmd() {
  if [ -n "${IPTABLES_CMD:-}" ]; then
    echo "${IPTABLES_CMD}"
    return
  fi
  if command -v iptables-legacy >/dev/null 2>&1 && \
     grep -q '^iptable_nat ' "${PROC_MODULES}" 2>/dev/null; then
    echo "iptables-legacy"
  else
    echo "iptables"
  fi
}

IPT=$(detect_iptables_cmd)
echo "Using iptables command: ${IPT} ($(${IPT} --version 2>/dev/null || echo 'unknown version'))"

# Fail loud if a jump rule we just installed is not actually present in the
# chosen backend. `set -e` already aborts on hard programming errors; this also
# catches the subtler case where a command returned 0 but the rule did not land
# (e.g. rules written into a backend that is not the live datapath), so a
# fail-closed traffic guard can never silently no-op. Usage:
#   require_jump <cmd> <table> <parent-chain> <our-chain> [extra match tokens...]
require_jump() {
  _rj_cmd="$1"; _rj_tbl="$2"; _rj_parent="$3"; _rj_chain="$4"; shift 4
  if ! ${_rj_cmd} -t "${_rj_tbl}" -C "${_rj_parent}" "$@" -j "${_rj_chain}" 2>/dev/null; then
    echo "ERROR: ${_rj_cmd}: '${_rj_chain}' is not installed in ${_rj_tbl} ${_rj_parent}." >&2
    echo "ERROR: traffic interception is NOT active — refusing to start with enforcement disabled." >&2
    echo "ERROR: the selected iptables backend could not program rules. On nft-only or managed" >&2
    echo "ERROR: platforms set IPTABLES_CMD=iptables and ensure NET_ADMIN/NET_RAW are granted" >&2
    echo "ERROR: (and, on OpenShift, an SELinux context that permits nf_tables access)." >&2
    exit 1
  fi
}

PROXY_PORT="${PROXY_PORT:-15123}"
INBOUND_PROXY_PORT="${INBOUND_PROXY_PORT:-15124}"
# enforce-redirect mode: the forward proxy's transparent listener port, the
# REDIRECT target for captured external TCP egress. Must match the authbridge
# proxy-sidecar listener.transparent_proxy_addr (default :8082).
TRANSPARENT_PORT="${TRANSPARENT_PORT:-8082}"
# enforce-redirect mode: the INBOUND transparent listener port. Empty (the
# default) leaves inbound interception OFF, so enforce-redirect stays a pure
# egress guard and nothing about existing deployments changes. When set, inbound
# TCP is REDIRECTed here and AuthBridge recovers the port the client actually
# addressed via SO_ORIGINAL_DST. Must match the authbridge proxy-sidecar
# listener.transparent_inbound_addr (preset default :8083).
#
# An env var rather than a fourth MODE value: interception is two independent
# axes (inbound, outbound), so folding it into MODE would multiply the strict
# `case` above combinatorially (redirect, enforce-redirect,
# enforce-redirect+inbound, inbound-only, ...).
INBOUND_TRANSPARENT_PORT="${INBOUND_TRANSPARENT_PORT:-}"
# AuthBridge's own listeners, excluded from the inbound REDIRECT: traffic to
# these is for the sidecar itself, not the app. Redirecting them would put the
# health endpoint behind JWT validation (breaking probes) and gate the stats and
# session-events APIs. The default covers the proxy-sidecar preset layout; the
# operator overrides it when it assigns a non-default forward-proxy port.
# The transparent ports themselves are excluded unconditionally below.
SIDECAR_PORTS_EXCLUDE="${SIDECAR_PORTS_EXCLUDE:-8081,9091,9093,9094}"
PROXY_UID="${PROXY_UID:-1337}"
SSH_PORT="${SSH_PORT:-22}"
OUTBOUND_PORTS_EXCLUDE="${OUTBOUND_PORTS_EXCLUDE:-}"
INBOUND_PORTS_EXCLUDE="${INBOUND_PORTS_EXCLUDE:-}"

# enforce-redirect mode: under the egress guard, cluster DNS must stay direct
# (the forward proxy is HTTP-only and cannot carry DNS), while every other
# in-cluster TCP flow is captured by the pipeline. Rather than guess in-cluster
# CIDRs — the resolver may sit OUTSIDE them (OpenShift service net 172.30/172.31,
# outside 10/8) or at a link-local address (NodeLocal DNSCache, 169.254.x) — we
# read the pod's actual resolvers from /etc/resolv.conf and leave only DNS
# (UDP/53 + TCP/53) to THOSE IPs direct. kubelet writes resolv.conf per the pod's
# dnsPolicy, so it is the authoritative, cluster-agnostic source for "where is my
# resolver". Override the path with RESOLV_CONF (e.g. for tests).
RESOLV_CONF="${RESOLV_CONF:-/etc/resolv.conf}"

# Emit the `nameserver` IPs from resolv.conf, one per line (IPv4 and IPv6 mixed;
# callers split by address family). Empty output if the file is missing/unreadable.
get_nameservers() {
  [ -r "${RESOLV_CONF}" ] || return 0
  while read -r _key _val _rest; do
    [ "${_key}" = "nameserver" ] && [ -n "${_val}" ] && echo "${_val}"
  done < "${RESOLV_CONF}"
  # `read` returns non-zero at EOF; force success so `NS=$(get_nameservers)`
  # under `set -e` does not abort the script (missing/empty resolv.conf is fine).
  return 0
}

# POD_IPS is the pod's full address list (Downward API status.podIPs,
# comma-separated). Needed because the ambient DNAT target must match the address
# family of the traffic: keying only off POD_IP — the PRIMARY address — leaves the
# other family's HBONE delivery hitting AB_REDIRECT's ztunnel-mark RETURN and
# passing unvalidated, while that family's PREROUTING rules ARE installed. That is
# the half-enforcement this mode otherwise refuses to ship. Falls back to POD_IP
# so an older operator that injects only the singular field still works for its
# family.
POD_IPS="${POD_IPS:-${POD_IP}}"

# pod_ip_for_family <v4|v6> echoes the first POD_IPS entry of that family, or
# nothing when the pod has no address in it.
pod_ip_for_family() {
  for _pif in $(echo "${POD_IPS}" | tr ',' ' '); do
    if [ "$1" = "v6" ]; then
      if is_ipv6 "${_pif}"; then echo "${_pif}"; return 0; fi
    else
      if is_ipv6 "${_pif}"; then :; else echo "${_pif}"; return 0; fi
    fi
  done
  return 0
}

# is_ipv6 reports whether an address literal is IPv6, by the presence of a
# colon. Sufficient here because the only inputs are resolv.conf nameservers and
# the downward-API POD_IP, both of which are bare literals (never host:port).
is_ipv6() {
  case "$1" in
    *:*) return 0 ;;
    *) return 1 ;;
  esac
}

# IPv6 counterpart of the detected iptables backend (iptables-legacy ->
# ip6tables-legacy, iptables -> ip6tables). Override with IP6TABLES_CMD.
IP6T="${IP6TABLES_CMD:-$(echo "${IPT}" | sed 's/iptables/ip6tables/')}"

# Istio ztunnel defaults
ZTUNNEL_HBONE_PORT="${ZTUNNEL_HBONE_PORT:-15008}"
ZTUNNEL_MARK="${ZTUNNEL_MARK:-0x539/0xfff}"  # 0x539 = 1337 decimal, ztunnel's socket fwmark
ISTIO_HEALTH_PROBE_SRC="${ISTIO_HEALTH_PROBE_SRC:-169.254.7.127}"

# POD_IP is required for the ztunnel inbound interception rule (DNAT target).
# It must be passed via the Kubernetes Downward API (status.podIP) or set manually.
# We use DNAT to the pod IP instead of REDIRECT to avoid needing route_localnet=1,
# which would require a privileged init container (to write to read-only /proc/sys).
# POD_IP is only needed by redirect mode (DNAT target for the ambient inbound
# rule). enforce-redirect does no DNAT, so it does not require it.
if [ "${MODE}" = "redirect" ] && [ -z "${POD_IP}" ]; then
  echo "ERROR: POD_IP environment variable is not set (required for redirect mode)." >&2
  echo "Set it via the Kubernetes Downward API (status.podIP) or manually." >&2
  exit 1
fi

# Transparent inbound needs POD_IP for the ambient DNAT target, for the same
# route_localnet reason redirect mode does (see the inbound-flow notes above).
# Fail loud rather than install PREROUTING-only rules: those silently miss every
# mesh-delivered request, since ztunnel re-originates inbound locally through
# OUTPUT and never traverses PREROUTING. A pod that validates direct traffic but
# waves through all mesh traffic is far worse than a failed init container.
# Tests the RESOLVED address list, not POD_IP alone: a deployment that injects
# only status.podIPs has everything the ambient DNAT needs (for both families),
# and aborting on the absence of the singular field would reject a strictly
# better-specified pod.
if [ -n "${INBOUND_TRANSPARENT_PORT}" ] && [ -z "${POD_IPS}" ]; then
  echo "ERROR: neither POD_IP nor POD_IPS is set, but INBOUND_TRANSPARENT_PORT=${INBOUND_TRANSPARENT_PORT} requests inbound interception." >&2
  echo "ERROR: without it the Istio ambient inbound path (ztunnel -> OUTPUT, not PREROUTING) cannot be captured," >&2
  echo "ERROR: so mesh traffic would bypass inbound validation entirely. Refusing to start half-enforced." >&2
  echo "Set POD_IP (status.podIP) or POD_IPS (status.podIPs) via the Kubernetes Downward API." >&2
  exit 1
fi

# emit_inbound_exemptions <cmd> <chain> [match-prefix...]
#
# Emits the RETURN rules for every port/source that must NOT be inbound-
# intercepted. ONE definition, because inbound arrives over TWO hooks:
# nat PREROUTING (plain network) and nat OUTPUT (ambient — ztunnel terminates
# HBONE and re-originates a LOCAL connection, so it never reaches PREROUTING).
#
# The redirect-mode rules drifted in exactly this way: PROXY_INBOUND's
# exclusions had no effect on ambient traffic until a second, hand-maintained
# copy was added to PROXY_OUTPUT ("Rule 1" below, whose comment names the
# pitfall). Emitting both from one function makes that drift impossible.
#
# RETURN and not ACCEPT: an exempt port must fall through to Istio's appended
# chain (ISTIO_PRERT / ISTIO_OUTPUT) so ambient still terminates mTLS for it.
# ACCEPT would end nat traversal and silently drop the port out of the mesh.
# That also rules out a shared sub-chain of RETURNs — a sub-chain RETURN resumes
# in the caller, landing straight on the terminal REDIRECT/DNAT it was meant to
# skip.
#
# $3.. is an optional match prefix. On the OUTPUT hook it scopes every rule to
# ztunnel's inbound delivery, so egress capture is untouched.
emit_inbound_exemptions() {
  _ec="$1"; _ech="$2"; shift 2

  # Istio rewrites kubelet health checks to a synthetic source. Gating them
  # fails probes and crash-loops the pod. The literal is IPv4-only, so emit it
  # only for the v4 command — branching on family rather than suppressing the
  # error keeps a genuine IPv4 failure (gated probes) loud.
  case "${_ec}" in
    *ip6tables*) ;;
    *) ${_ec} -t nat -A "${_ech}" "$@" -s "${ISTIO_HEALTH_PROBE_SRC}/32" -p tcp -j RETURN ;;
  esac

  # The transparent ports themselves (redirecting INBOUND_TRANSPARENT_PORT to
  # itself would self-loop), SSH, and ztunnel's HBONE port — which takes mTLS
  # tunnels straight from the network and must reach ztunnel's in-pod socket,
  # not an HTTP listener.
  for _p in "${INBOUND_TRANSPARENT_PORT}" "${TRANSPARENT_PORT}" "${SSH_PORT}" "${ZTUNNEL_HBONE_PORT}"; do
    if [ -n "${_p}" ]; then
      ${_ec} -t nat -A "${_ech}" "$@" -p tcp --dport "${_p}" -j RETURN
    fi
  done

  # SIDECAR_PORTS_EXCLUDE: AuthBridge's own listeners (health, stats,
  # session-events, forward proxy). Gating health breaks probes; gating the rest
  # breaks abctl and operator introspection.
  # INBOUND_PORTS_EXCLUDE: the operator/user escape hatch for app ports that must
  # not be validated — e.g. an OpenShift oauth-proxy doing its own auth on 8443.
  for _p in $(echo "${SIDECAR_PORTS_EXCLUDE},${INBOUND_PORTS_EXCLUDE}" | tr ',' ' '); do
    if [ -n "${_p}" ]; then
      ${_ec} -t nat -A "${_ech}" "$@" -p tcp --dport "${_p}" -j RETURN
    fi
  done
}

# =============================================================================
# enforce-redirect mode (proxy-sidecar fail-closed egress guard, capture variant)
# =============================================================================
#
# Forces all external egress through AuthBridge regardless of whether the app
# honors HTTP_PROXY, by CAPTURING bypass traffic: external TCP is transparently
# REDIRECTed to the forward proxy's transparent listener (TRANSPARENT_PORT),
# which recovers the original destination via SO_ORIGINAL_DST and tunnels it
# through the same outbound pipeline. Because nothing is dropped, agents that
# ignore HTTP_PROXY keep working — this is what lets enforcement be always-on.
#
# Placement — a dedicated chain hooked from *nat* OUTPUT at position 1 (REDIRECT
# is a nat-table target). Inserted before Istio's appended ISTIO_OUTPUT so we
# preempt ambient's nat redirect for external destinations, exactly as
# redirect mode does for the Envoy path.
#
# Rule order: RETURN ztunnel's own sockets (fwmark 0x539, no-op without ambient)
# -> RETURN the proxy's own re-originated egress (PROXY_UID, avoids the loop) ->
# RETURN loopback (app -> forward proxy via HTTP_PROXY, and any loopback) ->
# RETURN DNS-over-TCP (TCP/53) to the resolv.conf nameservers (left direct) ->
# REDIRECT all remaining TCP -- external AND in-cluster -- to TRANSPARENT_PORT
# (so agent->in-cluster calls are captured too) -> DROP all other egress
# (UDP/QUIC, so HTTP/3 can't bypass; clients fall back to TCP). DNS-over-UDP
# (UDP/53) to the resolvers is left direct by the mangle chain below; all other
# non-TCP -- including non-DNS in-cluster UDP -- is dropped.
#
# The nat REDIRECT chain has no conntrack ESTABLISHED rule: nat only evaluates
# the first packet of a flow, so replies and established connections are not
# re-translated.
# Two chains are needed because REDIRECT is a nat-table target but the nat table
# forbids DROP ("the use of DROP is therefore inhibited"):
#   * nat   OUTPUT / AB_REDIRECT — REDIRECT external TCP to TRANSPARENT_PORT.
#   * mangle OUTPUT / AB_NOTCP   — DROP external non-TCP (UDP/QUIC) so HTTP/3
#                                  cannot bypass; `-p tcp -j RETURN` lets TCP
#                                  fall through to the nat REDIRECT.
# mangle runs before nat in the OUTPUT hook, so non-TCP is dropped on its
# original destination and TCP is passed to the nat REDIRECT. Both are inserted
# at position 1 to precede Istio's appended chains.
setup_enforce_redirect() {
  REDIR_CHAIN="AB_REDIRECT"
  NOTCP_CHAIN="AB_NOTCP"

  # Fail closed and LOUD on zero resolvers: without a nameserver to exempt, the
  # rules below would drop UDP/53 and capture TCP/53, leaving a running-but-DNS-
  # dead pod that is far harder to triage than a failed init container. In a
  # Kubernetes pod kubelet always populates resolv.conf, so an empty result means
  # a real misconfiguration — surface it as Init:Error rather than silent breakage.
  NAMESERVERS=$(get_nameservers)
  if [ -z "${NAMESERVERS}" ]; then
    echo "enforce-redirect: ERROR: no nameservers found in ${RESOLV_CONF}" >&2
    echo "enforce-redirect: refusing to start — DNS egress would be dropped (UDP/53) / captured (TCP/53), silently breaking name resolution." >&2
    echo "enforce-redirect: set RESOLV_CONF to a file with valid 'nameserver' entries if running outside Kubernetes." >&2
    exit 1
  fi

  echo "enforce-redirect: installing fail-closed egress capture"
  echo "enforce-redirect: external TCP -> 127.0.0.1:${TRANSPARENT_PORT} (nat REDIRECT); external non-TCP -> DROP (mangle)"
  echo "enforce-redirect: exempt proxy UID=${PROXY_UID}; in-cluster TCP captured; DNS/53 to resolvers left direct (resolvers=$(echo "${NAMESERVERS}" | tr '\n' ' '))"

  # --- IPv4: nat REDIRECT for TCP ---
  ${IPT} -t nat -N "${REDIR_CHAIN}" 2>/dev/null || true
  ${IPT} -t nat -F "${REDIR_CHAIN}"
  # Transparent inbound, ambient path — MUST precede the ztunnel-mark RETURN
  # below, which would otherwise let every mesh-delivered request through
  # unvalidated. Ambient inbound does NOT traverse PREROUTING: the remote
  # ztunnel sends HBONE to this pod's ztunnel on 15008, which terminates mTLS
  # and re-originates a LOCAL connection to the app — so it appears in OUTPUT.
  #
  # All three matches are load-bearing:
  #   mark 0x539       — identifies ztunnel's sockets.
  #   ! --uid-owner    — excludes AuthBridge's own delivery to the app, which the
  #                      mangle rule below also marks 0x539; without this the
  #                      forward hop would be DNATed back into the listener.
  #   --dst-type LOCAL — ztunnel reuses 0x539 for OUTBOUND HBONE to remote pods
  #                      on :15008 too. Capturing those would hand an HTTP
  #                      listener an mTLS stream (InvalidContentType).
  #
  # DNAT to POD_IP rather than REDIRECT: REDIRECT in OUTPUT hardcodes dst to
  # 127.0.0.1, and ztunnel preserves the original client IP via IP_TRANSPARENT,
  # so the resulting src=external/dst=loopback packet is dropped as martian
  # unless route_localnet=1 (which needs a privileged write to /proc/sys).
  # SO_ORIGINAL_DST is unaffected — conntrack records the pre-NAT tuple for DNAT
  # and REDIRECT identically.
  _dnat4=$(pod_ip_for_family v4)
  if [ -n "${INBOUND_TRANSPARENT_PORT}" ] && [ -n "${_dnat4}" ]; then
    # The SAME exemptions AB_INBOUND applies, scoped to ztunnel's inbound
    # delivery and emitted BEFORE the DNAT. Without them every exemption
    # (health 9091, the operator's INBOUND_PORTS_EXCLUDE, ztunnel's own HBONE
    # port) is a silent no-op on the ambient path — and a JWT-gated 9091
    # crash-loops the pod.
    emit_inbound_exemptions "${IPT}" "${REDIR_CHAIN}" \
      -m mark --mark "${ZTUNNEL_MARK}" -m owner ! --uid-owner "${PROXY_UID}" \
      -m addrtype --dst-type LOCAL
    ${IPT} -t nat -A "${REDIR_CHAIN}" -m mark --mark "${ZTUNNEL_MARK}" \
      -m owner ! --uid-owner "${PROXY_UID}" -m addrtype --dst-type LOCAL \
      -p tcp -j DNAT --to-destination "${_dnat4}:${INBOUND_TRANSPARENT_PORT}"
  fi
  # ztunnel's own sockets (ambient) carry fwmark 0x539 — let them through.
  ${IPT} -t nat -A "${REDIR_CHAIN}" -m mark --mark "${ZTUNNEL_MARK}" -j RETURN
  # the AuthBridge proxy's own re-originated egress (runs as PROXY_UID) — avoids
  # redirecting the proxy's upstream dial back into itself.
  ${IPT} -t nat -A "${REDIR_CHAIN}" -m owner --uid-owner "${PROXY_UID}" -j RETURN
  # app -> forward proxy over loopback (HTTP_PROXY target), and any loopback.
  ${IPT} -t nat -A "${REDIR_CHAIN}" -o lo -j RETURN
  ${IPT} -t nat -A "${REDIR_CHAIN}" -d 127.0.0.0/8 -j RETURN
  # DNS-over-TCP (TCP/53) to the pod's resolvers — left direct so cluster name
  # resolution is not captured (the forward proxy can't carry DNS). All OTHER
  # in-cluster TCP falls through to the REDIRECT below, so the egress pipeline
  # sees agent->in-cluster calls (e.g. agent->tool). The proxy's re-originated
  # egress (PROXY_UID, RETURNed above) is exempt, and in an Istio ambient mesh it
  # falls through to ISTIO_OUTPUT -> ztunnel for mTLS.
  for ns in ${NAMESERVERS}; do
    case "${ns}" in *:*) continue ;; esac   # IPv6 resolver — handled in the v6 block
    ${IPT} -t nat -A "${REDIR_CHAIN}" -p tcp --dport 53 -d "${ns}" -j RETURN
  done
  # all remaining TCP (external + in-cluster, minus in-cluster DNS) — capture it transparently.
  ${IPT} -t nat -A "${REDIR_CHAIN}" -p tcp -j REDIRECT --to-port "${TRANSPARENT_PORT}"
  if ! ${IPT} -t nat -C OUTPUT -j "${REDIR_CHAIN}" 2>/dev/null; then
    ${IPT} -t nat -I OUTPUT 1 -j "${REDIR_CHAIN}"
  fi
  require_jump "${IPT}" nat OUTPUT "${REDIR_CHAIN}"

  # --- IPv4: mangle DROP for non-TCP ---
  ${IPT} -t mangle -N "${NOTCP_CHAIN}" 2>/dev/null || true
  ${IPT} -t mangle -F "${NOTCP_CHAIN}"
  # established/related replies (incl. UDP conntrack, e.g. DNS replies) first.
  ${IPT} -t mangle -A "${NOTCP_CHAIN}" -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
  ${IPT} -t mangle -A "${NOTCP_CHAIN}" -m mark --mark "${ZTUNNEL_MARK}" -j RETURN
  ${IPT} -t mangle -A "${NOTCP_CHAIN}" -m owner --uid-owner "${PROXY_UID}" -j RETURN
  ${IPT} -t mangle -A "${NOTCP_CHAIN}" -o lo -j RETURN
  ${IPT} -t mangle -A "${NOTCP_CHAIN}" -d 127.0.0.0/8 -j RETURN
  # DNS-over-UDP (UDP/53) to the pod's resolvers — left direct (the terminal DROP
  # below would otherwise kill cluster name resolution). Scoped to the resolvers
  # and port 53: all other non-TCP, including non-DNS in-cluster UDP, is dropped
  # so nothing can bypass the pipeline over UDP.
  for ns in ${NAMESERVERS}; do
    case "${ns}" in *:*) continue ;; esac   # IPv6 resolver — handled in the v6 block
    ${IPT} -t mangle -A "${NOTCP_CHAIN}" -p udp --dport 53 -d "${ns}" -j RETURN
  done
  # TCP is handled by the nat REDIRECT above — let it pass mangle untouched.
  ${IPT} -t mangle -A "${NOTCP_CHAIN}" -p tcp -j RETURN
  # everything else == external non-TCP egress (UDP/QUIC) — drop it.
  ${IPT} -t mangle -A "${NOTCP_CHAIN}" -j DROP
  if ! ${IPT} -t mangle -C OUTPUT -j "${NOTCP_CHAIN}" 2>/dev/null; then
    ${IPT} -t mangle -I OUTPUT 1 -j "${NOTCP_CHAIN}"
  fi
  require_jump "${IPT}" mangle OUTPUT "${NOTCP_CHAIN}"
  echo "enforce-redirect: IPv4 egress capture configured"

  # --- IPv6 ---
  # Mirror of IPv4: allow loopback + link-local (fe80::/10 unicast, ff02::/16
  # NDP/MLD multicast) and the proxy UID / ztunnel mark; leave DNS to any IPv6
  # resolv.conf nameservers direct; REDIRECT external v6 TCP; DROP other v6 egress.
  if command -v "${IP6T%% *}" >/dev/null 2>&1 && ${IP6T} -t nat -L >/dev/null 2>&1; then
    ${IP6T} -t nat -N "${REDIR_CHAIN}" 2>/dev/null || true
    ${IP6T} -t nat -F "${REDIR_CHAIN}"
    # Ambient inbound DNAT, v6 mirror. Keyed on the pod's v6 address rather than
    # on POD_IP's family, so a dual-stack pod (whose primary is usually v4) still
    # gets v6 ambient covered. A v4 target in ip6tables is rejected outright.
    _dnat6=$(pod_ip_for_family v6)
    if [ -n "${INBOUND_TRANSPARENT_PORT}" ] && [ -n "${_dnat6}" ]; then
      emit_inbound_exemptions "${IP6T}" "${REDIR_CHAIN}" \
        -m mark --mark "${ZTUNNEL_MARK}" -m owner ! --uid-owner "${PROXY_UID}" \
        -m addrtype --dst-type LOCAL
      ${IP6T} -t nat -A "${REDIR_CHAIN}" -m mark --mark "${ZTUNNEL_MARK}" \
        -m owner ! --uid-owner "${PROXY_UID}" -m addrtype --dst-type LOCAL \
        -p tcp -j DNAT --to-destination "[${_dnat6}]:${INBOUND_TRANSPARENT_PORT}"
    elif [ -n "${INBOUND_TRANSPARENT_PORT}" ]; then
      # v6 PREROUTING rules are still installed below, so non-mesh v6 inbound is
      # covered; only the v6 ambient path is not. Say so rather than leave it
      # implicit — on a v4-only cluster this is expected and harmless.
      echo "transparent-inbound: WARNING: no IPv6 pod address in POD_IPS — IPv6 ambient (HBONE) inbound is NOT captured" >&2
      echo "transparent-inbound: set POD_IPS from the Downward API (status.podIPs) if this pod is dual-stack" >&2
    fi
    ${IP6T} -t nat -A "${REDIR_CHAIN}" -m mark --mark "${ZTUNNEL_MARK}" -j RETURN
    ${IP6T} -t nat -A "${REDIR_CHAIN}" -m owner --uid-owner "${PROXY_UID}" -j RETURN
    ${IP6T} -t nat -A "${REDIR_CHAIN}" -o lo -j RETURN
    ${IP6T} -t nat -A "${REDIR_CHAIN}" -d ::1/128 -j RETURN
    ${IP6T} -t nat -A "${REDIR_CHAIN}" -d fe80::/10 -j RETURN
    ${IP6T} -t nat -A "${REDIR_CHAIN}" -d ff02::/16 -j RETURN
    # DNS-over-TCP (TCP/53) to IPv6 resolvers only — mirror of IPv4; all other
    # in-cluster v6 TCP is captured below.
    for ns in ${NAMESERVERS}; do
      case "${ns}" in *:*) ;; *) continue ;; esac   # IPv4 resolver — handled in the v4 block
      ${IP6T} -t nat -A "${REDIR_CHAIN}" -p tcp --dport 53 -d "${ns}" -j RETURN
    done
    ${IP6T} -t nat -A "${REDIR_CHAIN}" -p tcp -j REDIRECT --to-port "${TRANSPARENT_PORT}"
    if ! ${IP6T} -t nat -C OUTPUT -j "${REDIR_CHAIN}" 2>/dev/null; then
      ${IP6T} -t nat -I OUTPUT 1 -j "${REDIR_CHAIN}"
    fi
    require_jump "${IP6T}" nat OUTPUT "${REDIR_CHAIN}"

    ${IP6T} -t mangle -N "${NOTCP_CHAIN}" 2>/dev/null || true
    ${IP6T} -t mangle -F "${NOTCP_CHAIN}"
    ${IP6T} -t mangle -A "${NOTCP_CHAIN}" -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
    ${IP6T} -t mangle -A "${NOTCP_CHAIN}" -m mark --mark "${ZTUNNEL_MARK}" -j RETURN
    ${IP6T} -t mangle -A "${NOTCP_CHAIN}" -m owner --uid-owner "${PROXY_UID}" -j RETURN
    ${IP6T} -t mangle -A "${NOTCP_CHAIN}" -o lo -j RETURN
    ${IP6T} -t mangle -A "${NOTCP_CHAIN}" -d ::1/128 -j RETURN
    ${IP6T} -t mangle -A "${NOTCP_CHAIN}" -d fe80::/10 -j RETURN
    ${IP6T} -t mangle -A "${NOTCP_CHAIN}" -d ff02::/16 -j RETURN
    # DNS-over-UDP (UDP/53) to IPv6 resolvers only — mirror of IPv4.
    for ns in ${NAMESERVERS}; do
      case "${ns}" in *:*) ;; *) continue ;; esac   # IPv4 resolver — handled in the v4 block
      ${IP6T} -t mangle -A "${NOTCP_CHAIN}" -p udp --dport 53 -d "${ns}" -j RETURN
    done
    ${IP6T} -t mangle -A "${NOTCP_CHAIN}" -p tcp -j RETURN
    ${IP6T} -t mangle -A "${NOTCP_CHAIN}" -j DROP
    if ! ${IP6T} -t mangle -C OUTPUT -j "${NOTCP_CHAIN}" 2>/dev/null; then
      ${IP6T} -t mangle -I OUTPUT 1 -j "${NOTCP_CHAIN}"
    fi
    require_jump "${IP6T}" mangle OUTPUT "${NOTCP_CHAIN}"
    echo "enforce-redirect: IPv6 egress capture configured"
  else
    echo "enforce-redirect: ip6tables unavailable — skipping IPv6 egress capture"
  fi

  echo "enforce-redirect: fail-closed egress capture active"
}

# =============================================================================
# transparent inbound (proxy-sidecar hard inbound boundary)
# =============================================================================
#
# Opt-in companion to the egress guard, enabled by INBOUND_TRANSPARENT_PORT.
# Closes the pod-to-pod inbound bypass: without it, inbound validation only
# covers traffic that happens to reach AuthBridge's listener, so any pod dialing
# the agent's real port talks to it directly. The pod is the granularity both
# Kubernetes NetworkPolicy and ztunnel enforce at, so that is the boundary that
# matters — and it is the one this closes.
#
# Inbound arrives by two capturable paths, and BOTH must be covered or the guard
# is half a guard:
#
#   A. Plain network (ClusterIP/NodePort/non-mesh pod) -> nat PREROUTING.
#      Handled by the AB_INBOUND chain here.
#   B. Istio ambient HBONE -> remote ztunnel to local ztunnel :15008, which
#      terminates mTLS and re-originates a LOCAL connection. That appears in
#      nat OUTPUT, never PREROUTING, and is handled by the DNAT rule installed
#      at the head of AB_REDIRECT in setup_enforce_redirect().
#
# A third path is deliberately NOT captured: a container in this pod dialing
# 127.0.0.1:<appPort>. Containers share a network namespace and are one entity to
# every network enforcement layer, so intra-pod traffic is inside the trust
# boundary by construction — not a gap. Capturing it is also impossible without
# breaking AuthBridge's own forward hop, which is loopback by design.
#
# The forward hop (AuthBridge -> app) targets 127.0.0.1:<recovered port>, which
# AB_REDIRECT exempts three ways (-o lo, -d 127.0.0.0/8, --uid-owner PROXY_UID),
# so it can never be re-captured by our own egress rules. Consequence: the app
# must bind 0.0.0.0, not just its pod IP.
setup_transparent_inbound() {
  IN_CHAIN="AB_INBOUND"

  echo "transparent-inbound: installing inbound capture -> :${INBOUND_TRANSPARENT_PORT}"
  echo "transparent-inbound: exempt sidecar ports=${SIDECAR_PORTS_EXCLUDE} transparent ports=${TRANSPARENT_PORT},${INBOUND_TRANSPARENT_PORT} operator excludes=${INBOUND_PORTS_EXCLUDE:-<none>}"

  # inbound_chain_rules emits the shared rule set for one address family. Both
  # families get identical policy; only the command differs.
  inbound_chain_rules() {
    _ic="$1"
    ${_ic} -t nat -N "${IN_CHAIN}" 2>/dev/null || true
    ${_ic} -t nat -F "${IN_CHAIN}"

    emit_inbound_exemptions "${_ic}" "${IN_CHAIN}"

    # Everything else — the app's ports, whichever and however many they are.
    # SO_ORIGINAL_DST recovers which one each connection targeted, so no per-port
    # configuration is needed.
    ${_ic} -t nat -A "${IN_CHAIN}" -p tcp -j REDIRECT --to-port "${INBOUND_TRANSPARENT_PORT}"

    # -I 1 so we precede Istio's appended ISTIO_PRERT, exactly as redirect mode
    # does. No conntrack ESTABLISHED rule: nat evaluates only a flow's first
    # packet, so replies are un-NATed by conntrack automatically.
    if ! ${_ic} -t nat -C PREROUTING -p tcp -j "${IN_CHAIN}" 2>/dev/null; then
      ${_ic} -t nat -I PREROUTING 1 -p tcp -j "${IN_CHAIN}"
    fi
    require_jump "${_ic}" nat PREROUTING "${IN_CHAIN}" -p tcp
  }

  inbound_chain_rules "${IPT}"
  echo "transparent-inbound: IPv4 inbound capture configured"

  # Keep AuthBridge's delivery to the app off ISTIO_OUTPUT's redirect. Without
  # the mark, ambient's ISTIO_OUTPUT sees mark != 0x539 and can redirect our
  # forward hop to ztunnel :15001, looping the request back into the mesh
  # instead of handing it to the app. mangle OUTPUT runs before nat OUTPUT, so
  # the mark is set before ISTIO_OUTPUT evaluates it. Scoped to PROXY_UID +
  # dst-type LOCAL so only the local forward hop is marked, never AuthBridge's
  # external egress (which must stay unmarked so ztunnel wraps it in mTLS).
  #
  # Mirrors the equivalent rule redirect mode installs for Envoy. That one
  # targets a podIP delivery while ours is loopback, so whether ambient's
  # loopback branch makes this strictly necessary should be confirmed against a
  # live ambient pod; it is scoped narrowly enough to be harmless if not.
  # -C-guarded: an init container can re-run on pod restart, and an unguarded
  # -I would stack a duplicate mark rule on every pass.
  mark_local_delivery() {
    _mc="$1"
    if ! ${_mc} -t mangle -C OUTPUT -m owner --uid-owner "${PROXY_UID}" \
         -m addrtype --dst-type LOCAL -p tcp -j MARK --set-mark "${ZTUNNEL_MARK}" 2>/dev/null; then
      ${_mc} -t mangle -I OUTPUT 1 -m owner --uid-owner "${PROXY_UID}" \
        -m addrtype --dst-type LOCAL -p tcp -j MARK --set-mark "${ZTUNNEL_MARK}"
    fi
  }
  mark_local_delivery "${IPT}"

  if command -v "${IP6T%% *}" >/dev/null 2>&1 && ${IP6T} -t nat -L >/dev/null 2>&1; then
    inbound_chain_rules "${IP6T}"
    mark_local_delivery "${IP6T}"
    echo "transparent-inbound: IPv6 inbound capture configured"
  else
    echo "transparent-inbound: ip6tables unavailable — skipping IPv6 inbound capture"
  fi

  echo "transparent-inbound: hard inbound boundary active"
}

# Dispatch enforce-redirect here and exit; redirect mode falls through to the
# transparent-interception logic below.
if [ "${MODE}" = "enforce-redirect" ]; then
  setup_enforce_redirect
  # Inbound interception is opt-in and layers on top of the egress guard: the
  # ambient DNAT rule it depends on is installed at the head of AB_REDIRECT
  # above, so setup_enforce_redirect must run first.
  if [ -n "${INBOUND_TRANSPARENT_PORT}" ]; then
    setup_transparent_inbound
  else
    echo "enforce-redirect: inbound interception disabled (INBOUND_TRANSPARENT_PORT unset)"
  fi
  exit 0
fi

# =============================================================================
# OUTBOUND traffic interception (nat OUTPUT)
# =============================================================================

echo "Setting up iptables rules for outbound traffic interception..."

# Create custom chain (ignore error if it already exists)
${IPT} -t nat -N PROXY_OUTPUT 2>/dev/null || true

# Flush any existing rules in our chain to ensure idempotency
${IPT} -t nat -F PROXY_OUTPUT 2>/dev/null || true

# --- Rule 1: Exclude inbound ports from ztunnel delivery redirection ---
# When ambient mesh is active, ztunnel delivers inbound traffic via the OUTPUT
# chain (not PREROUTING), so INBOUND_PORTS_EXCLUDE rules in PROXY_INBOUND have
# no effect on ambient traffic. We must also exclude those ports here, before
# the catch-all redirect below. This is essential for services like OpenShift
# oauth-proxy (port 8443) that need direct access without Envoy intercepting
# WebSocket upgrades.
if [ -n "${INBOUND_PORTS_EXCLUDE}" ]; then
  for port in $(echo "${INBOUND_PORTS_EXCLUDE}" | tr ',' ' '); do
    echo "Excluding inbound port ${port} from ztunnel delivery redirection"
    ${IPT} -t nat -A PROXY_OUTPUT -m mark --mark ${ZTUNNEL_MARK} -m owner ! --uid-owner "${PROXY_UID}" -m addrtype --dst-type LOCAL -p tcp --dport "${port}" -j RETURN
  done
fi

# --- Rule 2: Intercept ztunnel's inbound delivery for JWT validation ---
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
#
# We use DNAT to POD_IP instead of REDIRECT here. REDIRECT in the OUTPUT chain
# always rewrites the destination to 127.0.0.1 (hardcoded in the kernel's
# nf_nat_redirect_ipv4). When the source IP is non-local (ztunnel preserves the
# original client IP via IP_TRANSPARENT), the kernel treats the resulting
# src=external, dst=127.0.0.1 packet as martian and drops it — unless
# route_localnet=1 is set, which requires a privileged container to write to
# /proc/sys. DNAT to the pod's own IP avoids this entirely: the pod IP is a
# routable non-loopback address, so no martian filtering occurs.
# SO_ORIGINAL_DST (used by Envoy's original_dst cluster) works identically
# with DNAT — conntrack stores the pre-NAT original tuple for both targets.
${IPT} -t nat -A PROXY_OUTPUT -m mark --mark ${ZTUNNEL_MARK} -m owner ! --uid-owner "${PROXY_UID}" -m addrtype --dst-type LOCAL -p tcp -j DNAT --to-destination "${POD_IP}:${INBOUND_PROXY_PORT}"

# --- Rule 3: Skip all remaining ztunnel traffic ---
# After rule 2 captured ztunnel's inbound delivery, any other ztunnel traffic
# (outbound HBONE, DNS proxy, etc.) must pass through unmodified. Matching on
# fwmark 0x539 is more robust than excluding individual port numbers — it covers
# all ztunnel sockets (15001, 15006, 15008, 15053, and any future ports).
${IPT} -t nat -A PROXY_OUTPUT -m mark --mark ${ZTUNNEL_MARK} -j RETURN

# --- Rule 4: Skip Envoy's own outbound connections ---
# Envoy (UID 1337) makes outbound connections after processing via ext_proc.
# These must RETURN from PROXY_OUTPUT so they fall through to Istio's
# ISTIO_OUTPUT chain (position 2 in OUTPUT), which redirects them to ztunnel
# (port 15001) for HBONE/mTLS wrapping. This is the desired behavior —
# ztunnel provides the mTLS transport layer for outbound connections.
${IPT} -t nat -A PROXY_OUTPUT -m owner --uid-owner "${PROXY_UID}" -j RETURN

# --- Rules 5-6: Exclusions ---
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
require_jump "${IPT}" nat OUTPUT PROXY_OUTPUT -p tcp

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
require_jump "${IPT}" nat PREROUTING PROXY_INBOUND -p tcp

echo "Inbound iptables rules configured successfully"
echo "Inbound traffic will be redirected to port ${INBOUND_PROXY_PORT}"
echo "Istio ztunnel compatibility enabled"
