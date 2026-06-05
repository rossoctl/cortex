#!/usr/bin/env bash
#
# Test harness for init-iptables.sh "enforce-drop" mode (proxy-sidecar
# fail-closed egress guard).
#
# It validates two things in a private network namespace:
#   1. Rule STRUCTURE — the AB_EGRESS chain is hooked from mangle OUTPUT at
#      position 1 with the expected RETURN exemptions and a terminal DROP, and
#      that no nat/filter rules are created.
#   2. AMBIENT ROBUSTNESS — a DROP in mangle OUTPUT preempts a simulated Istio
#      ambient "nat OUTPUT REDIRECT" (ISTIO_OUTPUT). Proven via packet counters:
#      after generating an external SYN, the mangle DROP increments and the nat
#      REDIRECT does NOT.
#
# Requirements: root (for unshare --net + iptables), iproute2, iptables-nft,
# bash, the dummy kernel module. Runs on Linux / CI (e.g. ubuntu-latest); not
# on macOS. Uses `unshare --net` (not named `ip netns`) so it also works inside
# nested containers. Exit code 0 = all pass.
set -euo pipefail

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
INIT="${INIT_SCRIPT:-${SCRIPT_DIR}/init-iptables.sh}"
IPT="${IPTABLES_CMD:-iptables-nft}"
EXTERNAL="198.51.100.7"   # RFC5737 TEST-NET-2, guaranteed unused

# Re-exec into a private network namespace. unshare avoids the /sys remount that
# named `ip netns exec` performs, so this works inside nested containers too.
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
  echo "WARN: dummy interface unavailable; preemption packet may not be generated"
fi

echo "### Installing enforce-drop rules"
env MODE=enforce-drop PROXY_UID=1337 CLUSTER_CIDRS=10.0.0.0/8 \
    IPTABLES_CMD="${IPT}" IP6TABLES_CMD=ip6tables-nft \
    sh "${INIT}" || { echo "FAIL: init script exited non-zero"; exit 1; }

dump=$("${IPT}" -t mangle -S)
echo "--- mangle ruleset ---"; echo "${dump}"

assert() { if echo "${dump}" | grep -qE "$2"; then echo "PASS: $1"; else echo "FAIL: $1"; fail=1; fi; }
assert "AB_EGRESS hooked from OUTPUT"  '^-A OUTPUT -j AB_EGRESS'
assert "established/related RETURN"    'AB_EGRESS -m conntrack --ctstate (ESTABLISHED,RELATED|RELATED,ESTABLISHED) -j RETURN'
assert "ztunnel mark RETURN"           'AB_EGRESS .*mark.*0x539.*-j RETURN'
assert "proxy UID RETURN"              'AB_EGRESS .*--uid-owner 1337 -j RETURN'
assert "loopback iface RETURN"         'AB_EGRESS -o lo -j RETURN'
assert "loopback cidr RETURN"          'AB_EGRESS -d 127.0.0.0/8 -j RETURN'
assert "cluster cidr RETURN"           'AB_EGRESS -d 10.0.0.0/8 -j RETURN'
assert "terminal DROP"                 'AB_EGRESS -j DROP'

pos1=$("${IPT}" -t mangle -L OUTPUT --line-numbers -n | awk '$1=="1"{print $2}')
if [ "${pos1}" = "AB_EGRESS" ]; then echo "PASS: AB_EGRESS at OUTPUT position 1"
else echo "FAIL: AB_EGRESS not at OUTPUT position 1 (got '${pos1}')"; fail=1; fi

# the established/related RETURN must be the first rule in the chain (replies
# must be let through before any owner/dest evaluation).
first_rule=$("${IPT}" -t mangle -S AB_EGRESS | grep '^-A AB_EGRESS' | head -1)
if echo "${first_rule}" | grep -q 'conntrack'; then echo "PASS: established/related RETURN is first in AB_EGRESS"
else echo "FAIL: first AB_EGRESS rule is not the conntrack RETURN (got: ${first_rule})"; fail=1; fi

natcount=$("${IPT}" -t nat -S | grep -cE 'AB_EGRESS|REDIRECT|PROXY_' || true)
if [ "${natcount:-0}" -eq 0 ]; then echo "PASS: no nat-table rules created"
else echo "FAIL: enforce-drop created nat rules"; fail=1; fi

echo "### Ambient-preemption test: append a simulated ISTIO_OUTPUT nat REDIRECT"
"${IPT}" -t nat -A OUTPUT -p tcp -d "${EXTERNAL}" -j REDIRECT --to-ports 19999
# Generate an external SYN (uid 0, like an agent bypass attempt).
timeout 2 bash -c "exec 3<>/dev/tcp/${EXTERNAL}/80" 2>/dev/null || true

dropc=$("${IPT}" -t mangle -L AB_EGRESS -n -v | awk '/DROP/{print $1; exit}')
redirc=$("${IPT}" -t nat -L OUTPUT -n -v | awk '/REDIRECT/{print $1; exit}')
echo "mangle AB_EGRESS DROP pkts=${dropc:-?} | nat REDIRECT pkts=${redirc:-?}"
if [ "${dropc:-0}" -gt 0 ] && [ "${redirc:-0}" -eq 0 ]; then
  echo "PASS: mangle DROP preempted nat REDIRECT (ambient-robust)"
else
  echo "FAIL: preemption not demonstrated (DROP=${dropc:-?}, REDIRECT=${redirc:-?})"; fail=1
fi

echo
[ "${fail}" -eq 0 ] && echo "ALL TESTS PASSED" || echo "SOME TESTS FAILED"
exit "${fail}"
