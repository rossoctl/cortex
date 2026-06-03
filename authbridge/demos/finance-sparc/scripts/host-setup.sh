#!/bin/bash
# Optional network preflight for the demo — generic, no machine-specific tweaks.
#
# On healthy networks this is a pure no-op: it just checks that the kind node
# can reach the public internet (TLS), which it needs for image pulls and (when
# PROVIDER=watsonx) for the SPARC service to reach watsonx.
#
# A few links — some cellular hotspots and VPNs — silently blackhole PMTUD: they
# drop large TCP segments without sending ICMP "fragmentation needed", so TLS
# handshakes hang. The portable, vendor-neutral remedy is TCP MSS clamping. We
# apply it ONLY if the egress check fails, and ONLY on the kind node itself
# (standard iptables, no host/VM privileges). If your network is healthy you'll
# see "egress healthy" and nothing is changed.
#
# The demo's reasoning + reflection LLMs run in-cluster (see k8s/ollama.yaml),
# so no host-Ollama reachability or host networking is required.
set -euo pipefail

CLUSTER=${KIND_CLUSTER_NAME:-kagenti}
NODE="${CLUSTER}-control-plane"
PROBE_URL=${EGRESS_PROBE_URL:-https://us-south.ml.cloud.ibm.com}
say() { printf '\033[1m%s\033[0m\n' "$*"; }

# Probe egress from inside the node (no extra image pulls). Success = we got ANY
# HTTP response (the TLS endpoint is reachable); we do NOT require a 2xx, since a
# bare TLS endpoint often answers 4xx. Only a DNS/connect failure (empty code)
# counts as a blackhole. `curl` without -f returns 0 even on 4xx.
egress_ok() {
  local code
  code=$(docker exec "$NODE" sh -c \
    "curl -s -m 8 -o /dev/null -w '%{http_code}' '$PROBE_URL' 2>/dev/null" 2>/dev/null || true)
  [ -n "$code" ] && [ "$code" != "000" ]
}

clamp_node() {
  for chain in POSTROUTING FORWARD OUTPUT; do
    docker exec "$NODE" iptables -t mangle -C "$chain" \
      -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu 2>/dev/null \
      || docker exec "$NODE" iptables -t mangle -A "$chain" \
        -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu 2>/dev/null || true
  done
}

say "[net] checking cluster internet egress (TLS) ..."
if egress_ok; then
  echo "  egress healthy — no changes made."
else
  echo "  egress looks blackholed (likely a hotspot/VPN PMTUD issue); applying a portable TCP MSS clamp on the node ..."
  clamp_node
  if egress_ok; then echo "  egress restored via MSS clamp."; else
    echo "  WARNING: egress still failing after clamp; image pulls / watsonx may be slow or fail on this network." >&2
  fi
fi
