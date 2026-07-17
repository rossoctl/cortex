#!/usr/bin/env bash
# End-to-end context-guru demo on a real Kagenti (kind) cluster.
#
#   ./run.sh setup            # build+load images, create the 12k-window model, deploy agent+sidecar
#   ./run.sh drive <mode>     # mode = enforce | observe | off ; drives the audit, prints answer + gain
#   ./run.sh all              # setup, then drive off / observe / enforce and print a comparison
#
# The finance agent audits three transactions (TX4827, TX5310, TX2981), pulling a
# large audit trail for each + a customer ledger. Only TX4827 has a planted
# duplicate-settlement anomaly. The raw request (~18K tokens) exceeds the model's
# 12,288-token window; without context-guru it is truncated and the agent misses
# the anomaly, with context-guru it is compacted (~10K tokens), fits, and the
# agent answers correctly.
#
# Prereqs: a running Kagenti kind cluster ("kagenti") with the finance-sparc
# finance-agent:latest image, and a host Ollama (llama3.2:3b) reachable at
# OLLAMA_URL. CG_MODEL_KEY must be a key for an OpenAI-compatible extract-code model.
set -euo pipefail
NS=team1
HERE="$(cd "$(dirname "$0")" && pwd)"
AB="$(cd "$HERE/../.." && pwd)"                 # authbridge/
FS="$AB/demos/finance-sparc"
OLLAMA_HOST_URL="${OLLAMA_HOST_URL:-http://127.0.0.1:11434}"    # host Ollama, as seen from THIS machine
: "${CG_MODEL_NAME:=gpt-4o-mini}"                               # any capable, cheap OpenAI-wire model
CTX_MODEL="llama3.2-ctx12k"

setup() {
  : "${CG_MODEL_KEY:?set CG_MODEL_KEY to the extract-code model API key}"
  : "${CG_MODEL_BASE:?set CG_MODEL_BASE to an OpenAI-compatible endpoint for the extract-code model, e.g. https://api.openai.com}"
  local arch; arch=$(docker exec kagenti-control-plane uname -m | sed 's/aarch64/arm64/;s/x86_64/amd64/')

  echo "==> build authbridge-proxy WITH the context-guru plugin ($arch, pure-Go, -tags include_plugin_contextguru) + image"
  ( cd "$AB" && GOTOOLCHAIN=auto GOOS=linux GOARCH="$arch" CGO_ENABLED=0 \
      go build -tags include_plugin_contextguru -ldflags="-s -w" -o /tmp/authbridge-proxy-cg ./cmd/authbridge-proxy )
  local d=/tmp/cg-img; mkdir -p "$d"; cp /tmp/authbridge-proxy-cg "$d/authbridge-proxy"
  cat > "$d/Dockerfile" <<'DOCKER'
FROM alpine:3.20@sha256:beefdbd8a1da6d2915566fde36db9db0b524eb737fc57cd1367effd16dc0d06d
RUN apk add --no-cache ca-certificates
COPY authbridge-proxy /usr/local/bin/authbridge-proxy
ENTRYPOINT ["/usr/local/bin/authbridge-proxy"]
DOCKER
  docker build -q -t authbridge-cg:latest "$d" >/dev/null
  kind load docker-image authbridge-cg:latest --name kagenti

  echo "==> build+load finance-mcp (large-output tools: get_transaction_audit, get_customer_ledger)"
  ( cd "$FS" && docker build -q -f finance-mcp/Dockerfile -t finance-mcp:latest . >/dev/null )
  kind load docker-image finance-mcp:latest --name kagenti
  kubectl -n "$NS" rollout restart deploy/finance-mcp >/dev/null
  kubectl -n "$NS" rollout status deploy/finance-mcp --timeout=90s

  echo "==> create a 12,288-token-window Ollama model ($CTX_MODEL) so the raw request truncates"
  curl -sf -m120 "$OLLAMA_HOST_URL/api/create" \
    -d "{\"model\":\"$CTX_MODEL\",\"from\":\"llama3.2:3b\",\"parameters\":{\"num_ctx\":12288}}" >/dev/null

  echo "==> secret + configmap + deploy agent + context-guru sidecar"
  # Feed the key via a builtin (process substitution), not --from-literal, so it
  # never appears in kubectl's argv / the process table on a shared host.
  kubectl -n "$NS" create secret generic cg-model-key --from-file=key=<(printf %s "$CG_MODEL_KEY") \
    --dry-run=client -o yaml | kubectl apply -f -
  kubectl -n "$NS" create configmap cg-authbridge-config --from-file=config.yaml="$HERE/k8s/authbridge-config.yaml" \
    --dry-run=client -o yaml | kubectl apply -f -
  kubectl apply -f "$HERE/k8s/agent.yaml"
  kubectl -n "$NS" set env deploy/cg-finance-agent OLLAMA_MODEL="$CTX_MODEL" >/dev/null
  # Point the extract-code model at the operator-provided endpoint (agent.yaml
  # ships only a neutral placeholder; the real endpoint never lands in the repo).
  kubectl -n "$NS" set env deploy/cg-finance-agent -c authbridge-proxy \
    CG_MODEL_BASE="$CG_MODEL_BASE" CG_MODEL_NAME="$CG_MODEL_NAME" >/dev/null
  kubectl -n "$NS" rollout status deploy/cg-finance-agent --timeout=120s
}

drive() {
  local mode="${1:-enforce}"
  echo "==> mode=$mode : configure + drive the multi-transaction audit"
  sed "s/on_error: enforce/on_error: $mode/" "$HERE/k8s/authbridge-config.yaml" > /tmp/cg-cfg.yaml
  kubectl -n "$NS" create configmap cg-authbridge-config --from-file=config.yaml=/tmp/cg-cfg.yaml \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  kubectl -n "$NS" rollout restart deploy/cg-finance-agent >/dev/null
  kubectl -n "$NS" rollout status deploy/cg-finance-agent --timeout=120s >/dev/null
  cat > /tmp/cg-drive.json <<JSON
{"jsonrpc":"2.0","id":"1","method":"message/send","params":{"message":{"role":"user","messageId":"m1","contextId":"cg-$mode","parts":[{"kind":"text","text":"Audit these transactions for duplicate settlements: TX4827, TX5310, and TX2981. For EACH transaction call get_transaction_audit to pull its full audit trail. Also call get_customer_ledger for customer C921. After gathering all audit trails, tell me which transaction (if any) shows a duplicate settlement and whether a refund is warranted."}]}}}
JSON
  kubectl -n "$NS" create cm cg-drive --from-file=drive.json=/tmp/cg-drive.json --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  kubectl -n "$NS" delete pod cgdrive --ignore-not-found >/dev/null; sleep 2
  kubectl -n "$NS" run cgdrive --restart=Never --image=curlimages/curl:8.10.1 \
    --overrides='{"spec":{"containers":[{"name":"c","image":"curlimages/curl:8.10.1","command":["sh","-c","curl -m 300 -s -X POST http://cg-finance-agent.team1.svc.cluster.local:8080/ -H '"'"'Content-Type: application/json'"'"' --data @/data/drive.json"],"volumeMounts":[{"name":"d","mountPath":"/data"}]}],"volumes":[{"name":"d","configMap":{"name":"cg-drive"}}]}}' >/dev/null
  # Wait for the driver pod to finish. Break explicitly on terminal failure so a
  # crashed/unschedulable pod gives a readable error instead of piping non-JSON
  # logs into json.loads below.
  while :; do
    phase=$(kubectl -n "$NS" get pod cgdrive -o jsonpath='{.status.phase}' 2>/dev/null || echo Unknown)
    case "$phase" in
      Succeeded) break ;;
      Running|Pending) sleep 5 ;;
      *) echo "!! cgdrive pod ended in phase=$phase — the audit did not complete:"
         kubectl -n "$NS" describe pod cgdrive | tail -n 25
         kubectl -n "$NS" logs cgdrive 2>/dev/null || true
         exit 1 ;;
    esac
  done
  echo "--- agent answer ($mode) ---"
  kubectl -n "$NS" logs cgdrive | python3 -c 'import sys,json;b=sys.stdin.read().split("EXIT=")[0].strip();d=json.loads(b);r=d.get("result",{});m=(r.get("status")or{}).get("message")or{};p=m.get("parts") or (r.get("artifacts") or [{}])[0].get("parts",[]);print(" ".join(x.get("text","") for x in p)[:600])'
  echo "--- context-guru gain ($mode) ---"
  local pod; pod=$(kubectl -n "$NS" get pod -l app.kubernetes.io/name=cg-finance-agent --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}')
  kubectl -n "$NS" exec "$pod" -c authbridge-proxy -- sh -c 'wget -T5 -qO- http://localhost:9094/v1/sessions/default' | python3 -c '
import sys,json
evs=json.load(sys.stdin).get("events") or []
raw=cmp=0; pt=[]
for e in evs:
    if e.get("direction")!="outbound": continue
    pm=(e.get("plugins") or {}).get("body-mutation")
    if pm and e.get("phase")=="request": raw=max(raw,pm["length_before"]); cmp=max(cmp,pm["length_after"])
    inf=e.get("inference") or {}
    if e.get("phase")=="response" and inf.get("promptTokens"): pt.append(inf["promptTokens"])
if raw: print(f"raw={raw}B would/did-compact-to={cmp}B ({100*(raw-cmp)/raw:.0f}%)")
else:   print("context-guru disabled (no body-mutation)")
print(f"ollama prompt_tokens processed: {sorted(pt)}  (num_ctx=12288)")'
}

case "${1:-all}" in
  setup) setup ;;
  drive) drive "${2:-enforce}" ;;
  all)   setup; for m in off observe enforce; do echo; drive "$m"; done ;;
  *) echo "usage: $0 {setup|drive <enforce|observe|off>|all}"; exit 1 ;;
esac
