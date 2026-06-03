#!/bin/bash
# Stage an Ollama model onto the kind node for the in-cluster Ollama (mounted at
# /opt/ollama-models via hostPath). Copies the model from the host's local
# Ollama store with `docker cp` (local, no network). If the model isn't present
# on the host, the in-cluster Ollama will pull it on first use instead.
#
# Usage: stage-ollama-model.sh [model]   (default llama3.2:3b)
set -euo pipefail

MODEL=${1:-${OLLAMA_MODEL:-llama3.2:3b}}
CLUSTER=${KIND_CLUSTER_NAME:-kagenti}
NODE="${CLUSTER}-control-plane"
STORE="${OLLAMA_MODELS_DIR:-$HOME/.ollama/models}"
REPO="${MODEL%%:*}"; TAG="${MODEL##*:}"
# Ollama library models live under registry.ollama.ai/library/<repo>/<tag>.
case "$REPO" in */*) NS_PATH="$REPO" ;; *) NS_PATH="library/$REPO" ;; esac
MAN="$STORE/manifests/registry.ollama.ai/$NS_PATH/$TAG"

if [[ ! -f "$MAN" ]]; then
  echo "[stage] model $MODEL not found in host store ($MAN)."
  echo "[stage] the in-cluster Ollama will pull it on first use (needs egress)."
  exit 0
fi

echo "[stage] copying $MODEL from host store to node $NODE:/opt/ollama-models ..."
docker exec "$NODE" mkdir -p "/opt/ollama-models/manifests/registry.ollama.ai/$NS_PATH" /opt/ollama-models/blobs
docker cp "$MAN" "$NODE:/opt/ollama-models/manifests/registry.ollama.ai/$NS_PATH/$TAG"
for d in $(python3 -c "import json;m=json.load(open('$MAN'));print(m['config']['digest']);[print(l['digest']) for l in m['layers']]"); do
  blob="sha256-${d#sha256:}"
  if ! docker exec "$NODE" test -s "/opt/ollama-models/blobs/$blob" 2>/dev/null; then
    docker cp "$STORE/blobs/$blob" "$NODE:/opt/ollama-models/blobs/$blob"
  fi
done
echo "[stage] done: $(docker exec "$NODE" du -sh /opt/ollama-models | awk '{print $1}') staged for $MODEL"
