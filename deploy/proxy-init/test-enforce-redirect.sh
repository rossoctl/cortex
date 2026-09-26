#!/usr/bin/env bash
#
# Test harness for init-iptables.sh "enforce-redirect" mode (proxy-sidecar
# fail-closed egress guard, capture variant).
#
# It validates, in a private network namespace:
#   1. Rule STRUCTURE — the AB_REDIRECT chain is hooked from nat OUTPUT at
#      position 1 with the expected RETURN exemptions and a `-p tcp` REDIRECT to
#      TRANSPARENT_PORT (no DROP — the nat table forbids it); and the AB_NOTCP
#      chain is hooked from mangle OUTPUT with `-p tcp RETURN` then a terminal
#      DROP for external non-TCP egress. DNS (TCP/53 + UDP/53) to the resolv.conf
#      nameservers is left direct; all OTHER TCP is captured and all OTHER non-TCP
#      is dropped. The resolver here (172.31.0.10) is OUTSIDE 10/8 on purpose —
#      it proves the exemption follows the actual resolver, not a guessed CIDR
#      (the OpenShift/HyperShift regression: services + DNS live in 172.31/16).
#   2. CAPTURE (not drop) + AMBIENT ROBUSTNESS — external TCP egress is
#      REDIRECTed to TRANSPARENT_PORT, preempting a simulated Istio ambient
#      "nat OUTPUT REDIRECT" appended after our chain. Proven via packet
#      counters: our REDIRECT increments, the simulated ISTIO REDIRECT does not.
#   3. NON-TCP DROP — an external UDP datagram (QUIC/HTTP-3 bypass attempt) hits
#      the mangle AB_NOTCP DROP, proving non-TCP external egress cannot bypass.
#   4. TRANSPARENT INBOUND (INBOUND_TRANSPARENT_PORT) — off by default; when on,
#      AB_INBOUND is hooked at nat PREROUTING position 1 with the sidecar's own
#      ports exempted (health 9091 in particular, or kubelet probes would be
#      JWT-gated and crash-loop the pod), and the ambient DNAT is installed
#      BEFORE AB_REDIRECT's ztunnel-mark RETURN — the ordering that decides
#      whether mesh-delivered traffic is validated or waved through. Also covers
#      the POD_IP fail-closed guard and re-run idempotency of the mark rule.
#
# Requirements: root (for unshare --net + iptables), iproute2, iptables-nft,
# bash, the dummy kernel module. Runs on Linux / CI (e.g. ubuntu-latest); not on
# macOS. Uses `unshare --net` so it also works inside nested containers. Exit
# code 0 = all pass.
set -euo pipefail

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
INIT="${INIT_SCRIPT:-${SCRIPT_DIR}/init-iptables.sh}"
IPT="${IPTABLES_CMD:-iptables-nft}"
EXTERNAL="198.51.100.7"   # RFC5737 TEST-NET-2, guaranteed unused
TPORT="8082"
IN_TPORT="8083"                  # inbound transparent listener port
POD_IP_MOCK="10.244.1.7"         # stands in for the downward-API status.podIP
RESOLVER_V4="172.31.0.10"        # OCP-style resolver, deliberately OUTSIDE 10/8
RESOLVER_V6="fd00:10:96::10"     # IPv6 resolver, exercises the ip6tables path

# Re-exec into a private network namespace.
if [ -z "${_AB_NETNS_REEXEC:-}" ]; then
  exec unshare --net env _AB_NETNS_REEXEC=1 INIT_SCRIPT="${INIT}" \
       IPTABLES_CMD="${IPT}" bash "$0" "$@"
fi

fail=0

# Fresh netns: bring up lo and a dummy default route so packets to an external
# destination are actually generated and traverse the OUTPUT chain.
ip link set lo up
if ip link add eth-test type dummy 2>/dev/null; then
  ip addr add 10.255.255.2/24 dev eth-test
  ip link set eth-test up
  ip route add default via 10.255.255.1
else
  echo "WARN: dummy interface unavailable; capture packet may not be generated"
fi

# Mock resolv.conf: the script must derive the DNS exemption from these
# nameservers, NOT from any CIDR. The v4 resolver is outside 10/8 on purpose.
RESOLV_MOCK=$(mktemp)
cat > "${RESOLV_MOCK}" <<EOF
search team1.svc.cluster.local svc.cluster.local cluster.local
nameserver ${RESOLVER_V4}
nameserver ${RESOLVER_V6}
options ndots:5
EOF

echo "### Installing enforce-redirect rules (resolvers from ${RESOLV_MOCK})"
env MODE=enforce-redirect PROXY_UID=1337 RESOLV_CONF="${RESOLV_MOCK}" \
    TRANSPARENT_PORT="${TPORT}" \
    IPTABLES_CMD="${IPT}" IP6TABLES_CMD=ip6tables-nft \
    sh "${INIT}" || { echo "FAIL: init script exited non-zero"; exit 1; }

natdump=$("${IPT}" -t nat -S)
mangledump=$("${IPT}" -t mangle -S)
echo "--- nat ruleset ---"; echo "${natdump}"
echo "--- mangle ruleset ---"; echo "${mangledump}"

assert() { if echo "$3" | grep -qE "$2"; then echo "PASS: $1"; else echo "FAIL: $1"; fail=1; fi; }
# nat AB_REDIRECT — TCP capture (no DROP; nat forbids it).
assert "AB_REDIRECT hooked from nat OUTPUT" '^-A OUTPUT -j AB_REDIRECT' "${natdump}"
assert "nat ztunnel mark RETURN"            'AB_REDIRECT .*mark.*0x539.*-j RETURN' "${natdump}"
assert "nat proxy UID RETURN"               'AB_REDIRECT .*--uid-owner 1337 -j RETURN' "${natdump}"
assert "nat loopback iface RETURN"          'AB_REDIRECT -o lo -j RETURN' "${natdump}"
assert "nat loopback cidr RETURN"           'AB_REDIRECT -d 127.0.0.0/8 -j RETURN' "${natdump}"
# DNS-over-TCP (TCP/53) to the resolv.conf resolver is left direct so cluster
# name resolution is not captured; all OTHER TCP falls through to the REDIRECT.
assert "nat DNS-over-TCP RETURN to resolver" "AB_REDIRECT.*${RESOLVER_V4}.*dport 53.*RETURN" "${natdump}"
# the exemption must follow the resolver, not a CIDR: no 10/8 remnant at all.
if echo "${natdump}" | grep -qE '10\.0\.0\.0/8'; then
  echo "FAIL: nat ruleset still references 10.0.0.0/8 (CLUSTER_CIDRS not fully removed)"; fail=1
else echo "PASS: no 10/8 / CLUSTER_CIDRS remnant — DNS exemption is resolver-scoped"; fi
# the resolver RETURN must be port-53-scoped, not a blanket dest RETURN (else
# non-DNS in-cluster TCP to that host would escape capture).
if echo "${natdump}" | grep -qE "^-A AB_REDIRECT -d ${RESOLVER_V4}/32 -j RETURN\$"; then
  echo "FAIL: resolver RETURN is blanket (should be scoped to tcp/53)"; fail=1
else echo "PASS: resolver DNS RETURN is port-53-scoped — non-DNS in-cluster TCP still captured"; fi
assert "nat tcp REDIRECT to transparent"    "AB_REDIRECT -p tcp -j REDIRECT --to-ports ${TPORT}" "${natdump}"
if echo "${natdump}" | grep -qE 'AB_REDIRECT -j DROP'; then
  echo "FAIL: nat AB_REDIRECT must not contain DROP (nat table forbids it)"; fail=1
else echo "PASS: nat AB_REDIRECT has no DROP (correctly delegated to mangle)"; fi
# mangle AB_NOTCP — non-TCP drop, TCP passes through to the nat REDIRECT.
assert "AB_NOTCP hooked from mangle OUTPUT"  '^-A OUTPUT -j AB_NOTCP' "${mangledump}"
assert "mangle established/related RETURN"   'AB_NOTCP -m conntrack --ctstate (ESTABLISHED,RELATED|RELATED,ESTABLISHED) -j RETURN' "${mangledump}"
assert "mangle proxy UID RETURN"             'AB_NOTCP .*--uid-owner 1337 -j RETURN' "${mangledump}"
assert "mangle DNS-over-UDP RETURN to resolver" "AB_NOTCP.*${RESOLVER_V4}.*udp.*dport 53.*RETURN" "${mangledump}"
assert "mangle tcp RETURN (defer to nat)"    'AB_NOTCP -p tcp -j RETURN' "${mangledump}"
assert "mangle terminal DROP (non-tcp)"      'AB_NOTCP -j DROP' "${mangledump}"

# --- IPv6 mirror: DNS to the IPv6 resolver must be exempt (tcp/53 + udp/53) ---
if ip6tables-nft -t nat -S AB_REDIRECT >/dev/null 2>&1; then
  nat6=$(ip6tables-nft -t nat -S)
  mangle6=$(ip6tables-nft -t mangle -S)
  assert "v6 nat DNS-over-TCP RETURN to resolver"    "AB_REDIRECT.*${RESOLVER_V6}.*dport 53.*RETURN" "${nat6}"
  assert "v6 mangle DNS-over-UDP RETURN to resolver" "AB_NOTCP.*${RESOLVER_V6}.*udp.*dport 53.*RETURN" "${mangle6}"
else
  echo "SKIP: ip6tables AB_REDIRECT chain absent — IPv6 resolver assertions skipped"
fi

pos1=$("${IPT}" -t nat -L OUTPUT --line-numbers -n | awk '$1=="1"{print $2}')
if [ "${pos1}" = "AB_REDIRECT" ]; then echo "PASS: AB_REDIRECT at nat OUTPUT position 1"
else echo "FAIL: AB_REDIRECT not at nat OUTPUT position 1 (got '${pos1}')"; fail=1; fi
mpos1=$("${IPT}" -t mangle -L OUTPUT --line-numbers -n | awk '$1=="1"{print $2}')
if [ "${mpos1}" = "AB_NOTCP" ]; then echo "PASS: AB_NOTCP at mangle OUTPUT position 1"
else echo "FAIL: AB_NOTCP not at mangle OUTPUT position 1 (got '${mpos1}')"; fail=1; fi

echo "### Capture + preemption test: append a simulated ISTIO_OUTPUT nat REDIRECT"
"${IPT}" -t nat -A OUTPUT -p tcp -d "${EXTERNAL}" -j REDIRECT --to-ports 19999
# Generate an external TCP SYN (uid 0, like an agent bypass attempt). With no
# listener on TPORT the redirected SYN gets an RST; the rule counter still ticks.
timeout 2 bash -c "exec 3<>/dev/tcp/${EXTERNAL}/80" 2>/dev/null || true

# $3 is the target column under -v. Matching the column rather than the line is
# required: /REDIRECT/ also matches the "Chain AB_REDIRECT" header (yielding the
# literal "Chain") and, in OUTPUT, the "-j AB_REDIRECT" jump rule's own counter.
capc=$("${IPT}" -t nat -L AB_REDIRECT -n -v | awk '$3=="REDIRECT"{print $1; exit}')
istioc=$("${IPT}" -t nat -L OUTPUT -n -v | awk '$3=="REDIRECT"{print $1; exit}')
echo "AB_REDIRECT REDIRECT pkts=${capc:-?} | simulated ISTIO REDIRECT pkts=${istioc:-?}"
if [ "${capc:-0}" -gt 0 ] && [ "${istioc:-0}" -eq 0 ]; then
  echo "PASS: external TCP captured to transparent port, preempting nat REDIRECT (ambient-robust)"
else
  echo "FAIL: capture/preemption not demonstrated (AB=${capc:-?}, ISTIO=${istioc:-?})"; fail=1
fi

echo "### Non-TCP drop test: send an external UDP datagram (QUIC bypass attempt)"
timeout 2 bash -c "echo -n x >/dev/udp/${EXTERNAL}/53" 2>/dev/null || true
dropc=$("${IPT}" -t mangle -L AB_NOTCP -n -v | awk '/DROP/{print $1; exit}')
echo "mangle AB_NOTCP DROP pkts=${dropc:-?}"
if [ "${dropc:-0}" -gt 0 ]; then
  echo "PASS: external UDP dropped (HTTP/3 cannot bypass)"
else
  echo "FAIL: external UDP not dropped (DROP=${dropc:-?})"; fail=1
fi

echo "### Fail-closed test: empty resolv.conf must abort init (exit non-zero)"
# The zero-resolver check runs before any iptables mutation, so this re-invocation
# leaves the rules above untouched. A running-but-DNS-dead pod is worse than a
# failed init, so enforce-redirect refuses to start without a resolver to exempt.
EMPTY_RESOLV=$(mktemp)   # created empty: no `nameserver` lines
if env MODE=enforce-redirect PROXY_UID=1337 RESOLV_CONF="${EMPTY_RESOLV}" \
       TRANSPARENT_PORT="${TPORT}" IPTABLES_CMD="${IPT}" IP6TABLES_CMD=ip6tables-nft \
       sh "${INIT}" >/dev/null 2>&1; then
  echo "FAIL: init succeeded with empty resolv.conf (should exit non-zero)"; fail=1
else
  echo "PASS: init aborts fail-closed when resolv.conf has no nameservers"
fi

# =============================================================================
# Transparent inbound (INBOUND_TRANSPARENT_PORT) — opt-in inbound capture
# =============================================================================

echo "### Inbound OFF by default: no AB_INBOUND without INBOUND_TRANSPARENT_PORT"
if echo "${natdump}" | grep -q 'AB_INBOUND'; then
  echo "FAIL: AB_INBOUND present without INBOUND_TRANSPARENT_PORT (inbound must be opt-in)"; fail=1
else
  echo "PASS: inbound capture off by default — egress guard unchanged"
fi

echo "### Fail-closed test: inbound port without POD_IP must abort init"
# PREROUTING-only rules would silently miss every ambient (HBONE) request, since
# ztunnel re-originates inbound locally through OUTPUT. Half-enforced is worse
# than not started, so this must exit non-zero.
if env MODE=enforce-redirect PROXY_UID=1337 RESOLV_CONF="${RESOLV_MOCK}" \
       TRANSPARENT_PORT="${TPORT}" INBOUND_TRANSPARENT_PORT="${IN_TPORT}" \
       IPTABLES_CMD="${IPT}" IP6TABLES_CMD=ip6tables-nft \
       sh "${INIT}" >/dev/null 2>&1; then
  echo "FAIL: init succeeded with INBOUND_TRANSPARENT_PORT but no POD_IP"; fail=1
else
  echo "PASS: init aborts fail-closed when inbound capture is requested without POD_IP"
fi

echo "### Installing enforce-redirect + transparent inbound (POD_IP=${POD_IP_MOCK})"
env MODE=enforce-redirect PROXY_UID=1337 RESOLV_CONF="${RESOLV_MOCK}" \
    TRANSPARENT_PORT="${TPORT}" INBOUND_TRANSPARENT_PORT="${IN_TPORT}" \
    POD_IP="${POD_IP_MOCK}" INBOUND_PORTS_EXCLUDE=8443 \
    IPTABLES_CMD="${IPT}" IP6TABLES_CMD=ip6tables-nft \
    sh "${INIT}" || { echo "FAIL: init script exited non-zero with inbound enabled"; exit 1; }

innat=$("${IPT}" -t nat -S)
inmangle=$("${IPT}" -t mangle -S)
echo "--- nat ruleset (inbound enabled) ---"; echo "${innat}"

assert "AB_INBOUND hooked from nat PREROUTING" '^-A PREROUTING -p tcp -j AB_INBOUND' "${innat}"
assert "inbound catch-all REDIRECTs to the inbound port" \
       "AB_INBOUND -p tcp -j REDIRECT --to-ports ${IN_TPORT}" "${innat}"
# Self-loop and sidecar-port exemptions. Redirecting the health port would put
# kubelet probes behind JWT validation and crash-loop the pod.
assert "inbound port exempted (no self-loop)"  "AB_INBOUND -p tcp -m tcp --dport ${IN_TPORT} -j RETURN" "${innat}"
assert "egress transparent port exempted"      "AB_INBOUND -p tcp -m tcp --dport ${TPORT} -j RETURN" "${innat}"
assert "health port 9091 exempted (probes)"    'AB_INBOUND -p tcp -m tcp --dport 9091 -j RETURN' "${innat}"
assert "stats port 9093 exempted"              'AB_INBOUND -p tcp -m tcp --dport 9093 -j RETURN' "${innat}"
assert "session-events port 9094 exempted"     'AB_INBOUND -p tcp -m tcp --dport 9094 -j RETURN' "${innat}"
assert "forward-proxy port 8081 exempted"      'AB_INBOUND -p tcp -m tcp --dport 8081 -j RETURN' "${innat}"
assert "ztunnel HBONE 15008 exempted"          'AB_INBOUND -p tcp -m tcp --dport 15008 -j RETURN' "${innat}"
assert "operator exclude 8443 honored"         'AB_INBOUND -p tcp -m tcp --dport 8443 -j RETURN' "${innat}"

# The ambient path is the one a PREROUTING-only implementation silently misses.
assert "ambient inbound DNAT installed in AB_REDIRECT" \
       "AB_REDIRECT .*0x539.*! --uid-owner 1337.*--dst-type LOCAL.*-j DNAT --to-destination ${POD_IP_MOCK}:${IN_TPORT}" "${innat}"
# The negation is load-bearing and easy to lose: without it the rule matches
# AuthBridge's OWN delivery to the app and DNATs it back into the inbound
# listener, looping. Asserted separately so a regex that merely tolerates its
# absence cannot pass.
if echo "${innat}" | grep -E 'AB_REDIRECT.*-j DNAT' | grep -qv '! --uid-owner'; then
  echo "FAIL: an ambient DNAT rule lacks '! --uid-owner' (would loop the proxy's own forward hop)"; fail=1
else
  echo "PASS: every ambient DNAT rule negates the proxy UID"
fi

echo "### Ambient path must honor the SAME exemptions as AB_INBOUND"
# The bug this guards: ztunnel delivers inbound via OUTPUT, so exemptions living
# only in AB_INBOUND are silently a no-op for mesh traffic. A JWT-gated 9091
# crash-loops the pod; a captured 8443 breaks an oauth-proxy doing its own auth.
for port_desc in "9091 health" "9093 stats" "9094 session-api" "8443 operator-exclude" "15008 ztunnel-hbone" "${IN_TPORT} inbound-transparent"; do
  _p=${port_desc%% *}; _d=${port_desc#* }
  if echo "${innat}" | grep -qE "AB_REDIRECT.*0x539.*! --uid-owner 1337.*--dst-type LOCAL.*--dport ${_p} -j RETURN"; then
    echo "PASS: ambient path exempts ${_p} (${_d})"
  else
    echo "FAIL: ambient path does NOT exempt ${_p} (${_d}) — exemption is a no-op for mesh traffic"; fail=1
  fi
done
# Ordering: the exemptions are useless if the DNAT is evaluated first.
ex_line=$(echo "${innat}" | grep -nE "AB_REDIRECT.*0x539.*! --uid-owner 1337.*--dst-type LOCAL.*--dport 9091 -j RETURN" | head -1 | cut -d: -f1)
dn_line=$(echo "${innat}" | grep -nE 'AB_REDIRECT.*-j DNAT' | head -1 | cut -d: -f1)
if [ -n "${ex_line}" ] && [ -n "${dn_line}" ] && [ "${ex_line}" -lt "${dn_line}" ]; then
  echo "PASS: ambient exemptions precede the DNAT"
else
  echo "FAIL: ambient exemptions do not precede the DNAT (exempt=${ex_line:-?} dnat=${dn_line:-?})"; fail=1
fi

echo "### Health-probe source exemption present on IPv4, absent from IPv6"
# Branching on family rather than suppressing the error: the v4 rule must exist
# (or probes are gated), and the v4-only literal cannot be emitted into ip6tables.
assert "IPv4 health-probe source exempted in AB_INBOUND" \
       "AB_INBOUND -s 169.254.7.127/32 -p tcp -j RETURN" "${innat}"
in6nat=$(ip6tables-nft -t nat -S 2>/dev/null || true)
if [ -n "${in6nat}" ]; then
  if echo "${in6nat}" | grep -q "169.254.7.127"; then
    echo "FAIL: IPv4 health-probe literal leaked into the IPv6 ruleset"; fail=1
  else
    echo "PASS: IPv4-only health-probe literal not emitted into ip6tables"
  fi
fi
assert "forward-hop mark rule in mangle OUTPUT" \
       'OUTPUT -p tcp -m owner --uid-owner 1337 -m addrtype --dst-type LOCAL -j MARK --set-x?mark 0x539' "${inmangle}"

echo "### Ambient DNAT must precede the ztunnel-mark RETURN (else mesh bypasses)"
# Rule ORDER is the whole correctness argument here: AB_REDIRECT's ztunnel-mark
# RETURN would let every HBONE-delivered request through unvalidated if it were
# evaluated first.
dnat_line=$(echo "${innat}" | grep -n 'AB_REDIRECT.*-j DNAT' | head -1 | cut -d: -f1)
ret_line=$(echo "${innat}" | grep -n 'AB_REDIRECT -m mark --mark 0x539/0xfff -j RETURN' | head -1 | cut -d: -f1)
if [ -n "${dnat_line}" ] && [ -n "${ret_line}" ] && [ "${dnat_line}" -lt "${ret_line}" ]; then
  echo "PASS: ambient DNAT precedes the ztunnel-mark RETURN"
else
  echo "FAIL: ambient DNAT not before ztunnel RETURN (dnat=${dnat_line:-?} return=${ret_line:-?})"; fail=1
fi

echo "### AB_INBOUND must be at nat PREROUTING position 1 (precede ISTIO_PRERT)"
inpos1=$("${IPT}" -t nat -L PREROUTING --line-numbers 2>/dev/null | awk '$1=="1"{print $2}')
if [ "${inpos1}" = "AB_INBOUND" ]; then echo "PASS: AB_INBOUND at nat PREROUTING position 1"
else echo "FAIL: AB_INBOUND not at nat PREROUTING position 1 (got '${inpos1}')"; fail=1; fi

echo "### Idempotency: re-running init must not stack duplicate mark rules"
env MODE=enforce-redirect PROXY_UID=1337 RESOLV_CONF="${RESOLV_MOCK}" \
    TRANSPARENT_PORT="${TPORT}" INBOUND_TRANSPARENT_PORT="${IN_TPORT}" \
    POD_IP="${POD_IP_MOCK}" IPTABLES_CMD="${IPT}" IP6TABLES_CMD=ip6tables-nft \
    sh "${INIT}" >/dev/null 2>&1 || true
markcount=$("${IPT}" -t mangle -S OUTPUT | grep -c 'MARK --set-x\?mark 0x539' || true)
if [ "${markcount}" -eq 1 ]; then
  echo "PASS: forward-hop mark rule is idempotent across init re-runs"
else
  echo "FAIL: mark rule stacked ${markcount} times across re-runs (expected 1)"; fail=1
fi

echo "### AB_INBOUND chain is present in the live backend (not a capture test)"
# PREROUTING only sees packets arriving on an interface, which a netns cannot
# easily synthesise without a peer. Assert the REDIRECT rule exists and is
# reachable instead; live capture is covered by the Kind e2e.
if "${IPT}" -t nat -L AB_INBOUND -n >/dev/null 2>&1; then
  echo "PASS: AB_INBOUND chain exists and is listable in the live backend"
else
  echo "FAIL: AB_INBOUND chain missing from the live backend"; fail=1
fi

echo "### Dual-stack: POD_IPS must drive a DNAT for BOTH families"
# With only POD_IP (primary, usually v4) the other family's HBONE delivery hit
# AB_REDIRECT's ztunnel-mark RETURN and passed unvalidated, while that family's
# PREROUTING rules WERE installed — exactly the half-enforcement this mode
# refuses to ship elsewhere.
env MODE=enforce-redirect PROXY_UID=1337 RESOLV_CONF="${RESOLV_MOCK}" \
    TRANSPARENT_PORT="${TPORT}" INBOUND_TRANSPARENT_PORT="${IN_TPORT}" \
    POD_IP="${POD_IP_MOCK}" POD_IPS="${POD_IP_MOCK},fd00:10:244::7" \
    IPTABLES_CMD="${IPT}" IP6TABLES_CMD=ip6tables-nft \
    sh "${INIT}" >/dev/null 2>&1 || { echo "FAIL: init failed with dual-stack POD_IPS"; fail=1; }
ds4=$("${IPT}" -t nat -S 2>/dev/null || true)
ds6=$(ip6tables-nft -t nat -S 2>/dev/null || true)
if echo "${ds4}" | grep -qE "AB_REDIRECT.*-j DNAT --to-destination ${POD_IP_MOCK}:${IN_TPORT}"; then
  echo "PASS: dual-stack v4 ambient DNAT installed"
else
  echo "FAIL: dual-stack v4 ambient DNAT missing"; fail=1
fi
if [ -n "${ds6}" ]; then
  if echo "${ds6}" | grep -qE "AB_REDIRECT.*-j DNAT --to-destination \[?fd00:10:244::7\]?:${IN_TPORT}"; then
    echo "PASS: dual-stack v6 ambient DNAT installed (v6 HBONE cannot bypass)"
  else
    echo "FAIL: dual-stack v6 ambient DNAT missing — v6 HBONE delivery bypasses validation"; fail=1
  fi
fi

echo "### POD_IPS alone must satisfy the inbound guard (POD_IP not required)"
# A deployment that injects only status.podIPs has everything the ambient DNAT
# needs for both families; aborting on the absence of the singular field would
# reject a strictly better-specified pod.
if env MODE=enforce-redirect PROXY_UID=1337 RESOLV_CONF="${RESOLV_MOCK}" \
       TRANSPARENT_PORT="${TPORT}" INBOUND_TRANSPARENT_PORT="${IN_TPORT}" \
       POD_IPS="${POD_IP_MOCK}" \
       IPTABLES_CMD="${IPT}" IP6TABLES_CMD=ip6tables-nft \
       sh "${INIT}" >/dev/null 2>&1; then
  only6=$("${IPT}" -t nat -S 2>/dev/null || true)
  if echo "${only6}" | grep -qE "AB_REDIRECT.*-j DNAT --to-destination ${POD_IP_MOCK}:${IN_TPORT}"; then
    echo "PASS: POD_IPS alone satisfies the guard and yields the ambient DNAT"
  else
    echo "FAIL: init accepted POD_IPS but installed no ambient DNAT"; fail=1
  fi
else
  echo "FAIL: init rejected a pod that supplied POD_IPS but not POD_IP"; fail=1
fi

echo "### Neither POD_IP nor POD_IPS must still be fail-closed"
if env MODE=enforce-redirect PROXY_UID=1337 RESOLV_CONF="${RESOLV_MOCK}" \
       TRANSPARENT_PORT="${TPORT}" INBOUND_TRANSPARENT_PORT="${IN_TPORT}" \
       IPTABLES_CMD="${IPT}" IP6TABLES_CMD=ip6tables-nft \
       sh "${INIT}" >/dev/null 2>&1; then
  echo "FAIL: init succeeded with neither POD_IP nor POD_IPS"; fail=1
else
  echo "PASS: init still aborts when no pod address is available at all"
fi

echo "### Malformed exclude list must not abort init (set -e trap)"
# A trailing comma yields an empty field. If the port loop used `[ -n ] && cmd`
# as its last statement, the loop would exit non-zero and `set -e` would abort
# init — turning a cosmetic annotation typo into a pod that never starts.
if env MODE=enforce-redirect PROXY_UID=1337 RESOLV_CONF="${RESOLV_MOCK}" \
       TRANSPARENT_PORT="${TPORT}" INBOUND_TRANSPARENT_PORT="${IN_TPORT}" \
       POD_IP="${POD_IP_MOCK}" INBOUND_PORTS_EXCLUDE="8443," SIDECAR_PORTS_EXCLUDE="9091," \
       IPTABLES_CMD="${IPT}" IP6TABLES_CMD=ip6tables-nft \
       sh "${INIT}" >/dev/null 2>&1; then
  echo "PASS: trailing comma in an exclude list is tolerated"
else
  echo "FAIL: init aborted on a trailing comma in an exclude list"; fail=1
fi

echo "### Backend detection unit test (/proc/modules seam)"
# Pull detect_iptables_cmd (and its PROC_MODULES default) out of the script and
# exercise it against fixture module tables — no real kernel needed. The legacy
# branch also requires the iptables-legacy binary, so skip the legacy-positive
# case when it is not installed on the host.
eval "$(sed -n '/^PROC_MODULES=/,/^}/p' "${INIT}")"
mods_legacy=$(mktemp); printf 'ip_tables 28672 4 - Live 0x0\niptable_nat 12288 19 - Live 0x0\n' > "${mods_legacy}"
mods_nft=$(mktemp);    printf 'nf_tables 315392 344 nft_compat - Live 0x0\nnft_compat 20480 0 - Live 0x0\n' > "${mods_nft}"
if command -v iptables-legacy >/dev/null 2>&1; then
  got=$(IPTABLES_CMD= PROC_MODULES="${mods_legacy}" detect_iptables_cmd)
  [ "${got}" = "iptables-legacy" ] && echo "PASS: iptable_nat loaded => iptables-legacy" \
    || { echo "FAIL: expected iptables-legacy, got '${got}'"; fail=1; }
else
  echo "SKIP: iptables-legacy not installed on host — legacy-positive case skipped"
fi
got=$(IPTABLES_CMD= PROC_MODULES="${mods_nft}" detect_iptables_cmd)
[ "${got}" = "iptables" ] && echo "PASS: iptable_nat absent => iptables (nft)" \
  || { echo "FAIL: expected iptables, got '${got}'"; fail=1; }
got=$(IPTABLES_CMD=iptables-legacy PROC_MODULES="${mods_nft}" detect_iptables_cmd)
[ "${got}" = "iptables-legacy" ] && echo "PASS: IPTABLES_CMD override wins over detection" \
  || { echo "FAIL: override ignored, got '${got}'"; fail=1; }
rm -f "${mods_legacy}" "${mods_nft}"

echo
[ "${fail}" -eq 0 ] && echo "ALL TESTS PASSED" || echo "SOME TESTS FAILED"
exit "${fail}"
