#!/bin/sh
# Smoke test for a deployed central stack. Run after `oc apply -k`, logged in to the cluster, in the
# stack's namespace.
#
#   CG_TOKEN=... sh deploy/central/smoke.sh <collector-host>
#
# The token comes from the environment, not argv, so it does not show in `ps`. The Cortex path
# takes no token.
#
# It pushes one OTLP/JSON counter through each Route, with a forged `proxy` and an extra resource
# attribute, and checks:
#   1. a Cortex push is accepted (2xx) with no token and with any token; a context-guru push is
#      accepted with the right token;
#   2. a context-guru push with no token, or a wrong one, gets 401;
#   3. a plain-http push is not accepted;
#   4. Prometheus stores the counter with proxy="cortex" / proxy="context-guru" (not the forged
#      value), machine set from service.instance.id, and no extra label;
#   5. the same holds when the forged `proxy`/`machine` sit on the data point instead of the
#      resource — Prometheus's OTLP translator lets a point's own attribute override a promoted
#      resource one of the same name, so the collector must strip it there too — and a
#      data-point `session_id` (standing in for any label outside the metrics contract) does
#      not survive either: the collector allow-lists, not denies, data-point labels.
set -eu

host="${1:?usage: smoke.sh <collector-host>}"
: "${CG_TOKEN:?set CG_TOKEN}"

fail=0
pass() { printf 'PASS  %s\n' "$1"; }
bad()  { printf 'FAIL  %s\n' "$1"; fail=1; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
umask 077
printf 'Authorization: Bearer %s\n' "$CG_TOKEN" > "$work/h-cg"
printf 'Authorization: Bearer %s\n' "wrong-$(date +%s)" > "$work/h-wrong"

run_id="smoke-$(date +%s)"
now=$(($(date +%s) * 1000000000))

# body <machine>: one cumulative monotonic sum, with a forged proxy and an extra attribute.
body() {
  cat <<EOF
{"resourceMetrics":[{"resource":{"attributes":[
 {"key":"service.instance.id","value":{"stringValue":"$1"}},
 {"key":"service.name","value":{"stringValue":"smoke"}},
 {"key":"proxy","value":{"stringValue":"evil"}},
 {"key":"extra","value":{"stringValue":"x"}}]},
 "scopeMetrics":[{"scope":{"name":"smoke"},"metrics":[{"name":"smoke_test",
  "sum":{"aggregationTemporality":2,"isMonotonic":true,
   "dataPoints":[{"asInt":"1","startTimeUnixNano":"$now","timeUnixNano":"$now"}]}}]}]}]}
EOF
}

# body_datapoint_forge <machine>: the resource is honest (no forged proxy/machine there); the
# forgery is on the data point itself, where Prometheus's OTLP receiver turns every data-point
# attribute into a series label, promoted resource ones included (createAttributes in
# storage/remote/otlptranslator/prometheusremotewrite/helper.go, which lets a point's own
# attribute of the same name win over a promoted resource one). session_id stands in for any
# label outside the metrics contract's allow-list (docs/metrics/spec.md 5.1-5.5): a deny-list of
# just proxy/machine/etc. would still let it through.
body_datapoint_forge() {
  cat <<EOF
{"resourceMetrics":[{"resource":{"attributes":[
 {"key":"service.instance.id","value":{"stringValue":"$1"}},
 {"key":"service.name","value":{"stringValue":"smoke"}}]},
 "scopeMetrics":[{"scope":{"name":"smoke"},"metrics":[{"name":"smoke_test",
  "sum":{"aggregationTemporality":2,"isMonotonic":true,
   "dataPoints":[{"asInt":"1","startTimeUnixNano":"$now","timeUnixNano":"$now","attributes":[
    {"key":"proxy","value":{"stringValue":"evil-dp"}},
    {"key":"machine","value":{"stringValue":"evil-machine-dp"}},
    {"key":"session_id","value":{"stringValue":"SECRET-SESSION"}}]}]}}]}]}]}
EOF
}

# push <scheme> <path> <header-file|-> <machine> [datapoint-forge]: prints the HTTP status (000 if
# no answer). A fifth argument of "dp" sends body_datapoint_forge instead of body.
push() {
  if [ "${5:-}" = dp ]; then body_datapoint_forge "$4" > "$work/body"; else body "$4" > "$work/body"; fi
  if [ "$3" = - ]; then hdr=""; else hdr="-H @$3"; fi
  # shellcheck disable=SC2086
  curl -s -m 15 -o /dev/null -w '%{http_code}' -X POST $hdr \
    -H 'Content-Type: application/json' --data-binary "@$work/body" \
    "$1://$host$2/v1/metrics" || true
}

expect_2xx() { case "$2" in 2??) pass "$1 ($2)";; *) bad "$1: got $2, want 2xx";; esac; }
expect_401() { if [ "$2" = 401 ]; then pass "$1 (401)"; else bad "$1: got $2, want 401"; fi; }

code=$(push https /cortex - "$run_id-cortex");                   expect_2xx "cortex push with no token" "$code"
code=$(push https /cortex "$work/h-wrong" "$run_id-cortex-tok"); expect_2xx "cortex push with a token (ignored)" "$code"
code=$(push https /context-guru "$work/h-cg" "$run_id-cg");       expect_2xx "context-guru push with token" "$code"

code=$(push https /context-guru - "$run_id-none")
expect_401 "context-guru push with no token" "$code"
code=$(push https /context-guru "$work/h-wrong" "$run_id-wrong")
expect_401 "context-guru push with a wrong token" "$code"

code=$(push http /cortex - "$run_id-http")
case "$code" in 2??) bad "plain-http push was accepted ($code)";; *) pass "plain-http push not accepted ($code)";; esac

# query <promql>: the JSON result vector, from inside the Prometheus pod (it has no Route).
query() {
  q=$(python3 -I -c 'import sys,urllib.parse;print(urllib.parse.quote(sys.argv[1]))' "$1")
  oc exec deploy/prometheus -- wget -qO- "http://localhost:9090/api/v1/query?query=$q"
}

# check_series <machine> <want proxy>
check_series() {
  i=0
  while [ $i -lt 12 ]; do
    out=$(query "smoke_test_total{machine=\"$1\"}")
    if printf '%s' "$out" | python3 -I -c 'import json,sys; sys.exit(0 if json.load(sys.stdin)["data"]["result"] else 1)'; then
      break
    fi
    i=$((i + 1)); sleep 5
  done
  printf '%s' "$out" | python3 -I -c '
import json, sys
want = sys.argv[1]
r = json.load(sys.stdin)["data"]["result"]
if not r:
    print("FAIL  no series for machine " + sys.argv[2] + " after 60s"); sys.exit(1)
labels = r[0]["metric"]
problems = []
if labels.get("proxy") != want: problems.append("proxy=%r, want %r" % (labels.get("proxy"), want))
if "extra" in labels: problems.append("extra label kept")
allowed = {"__name__", "machine", "proxy", "preset", "service_version"}
extra = sorted(set(labels) - allowed)
if extra: problems.append("unexpected labels " + ",".join(extra))
if problems:
    print("FAIL  series " + sys.argv[2] + ": " + "; ".join(problems) + "  " + json.dumps(labels)); sys.exit(1)
print("PASS  series " + sys.argv[2] + " stored with proxy=" + want + ", labels " + ",".join(sorted(labels)))
' "$2" "$1" || fail=1
}

check_series "$run_id-cortex" cortex
check_series "$run_id-cg" context-guru

code=$(push https /cortex - "$run_id-dp" dp)
expect_2xx "cortex push with a data-point-level forged proxy/machine" "$code"
check_series "$run_id-dp" cortex

out=$(query "count(smoke_test_total{machine=~\"$run_id-(none|wrong|http)\"})")
if printf '%s' "$out" | python3 -I -c 'import json,sys; sys.exit(0 if not json.load(sys.stdin)["data"]["result"] else 1)'; then
  pass "refused pushes stored nothing"
else
  bad "a refused push was stored: $out"
fi

echo
if [ $fail = 0 ]; then echo "smoke test passed (run $run_id)"; else echo "smoke test FAILED (run $run_id)"; exit 1; fi
