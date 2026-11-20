#!/bin/sh
# Static checks for the central metrics stack. Needs docker or podman; uses kubectl or oc for the
# Kustomize render when one is installed, else a Kustomize container; uses a kubeconform binary on
# PATH when there is one, else the kubeconform container.
#
#   sh deploy/central/check.sh
#
# Checks, in order: the Prometheus rules parse; the rule and dashboard-query unit tests pass; the
# collector config is valid; the committed dashboards match gen_dashboards.py; the generator's own
# tests pass; the OpenShift overlay renders and every
# object passes kubeconform.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
base="$here/base"
overlay="$here/overlays/openshift"

PROM_IMAGE="${PROM_IMAGE:-docker.io/prom/prometheus:v3.2.1}"
OTELCOL_IMAGE="${OTELCOL_IMAGE:-docker.io/otel/opentelemetry-collector-contrib:0.165.0}"
KUBECONFORM_IMAGE="${KUBECONFORM_IMAGE:-ghcr.io/yannh/kubeconform:v0.6.7}"
KUSTOMIZE_IMAGE="${KUSTOMIZE_IMAGE:-registry.k8s.io/kustomize/kustomize:v5.6.0}"

if command -v docker >/dev/null 2>&1; then run=docker
elif command -v podman >/dev/null 2>&1; then run=podman
else echo "check.sh: needs docker or podman" >&2; exit 2
fi

step() { printf '\n== %s\n' "$1"; }

step "promtool check rules"
$run run --rm -v "$base/prometheus:/p:ro" --entrypoint promtool "$PROM_IMAGE" \
  check rules /p/rules/recording.yml /p/rules/alerts.yml

step "promtool test rules"
$run run --rm -v "$base/prometheus:/p:ro" -w /p/tests --entrypoint promtool "$PROM_IMAGE" \
  test rules alerts_test.yml dashboards_test.yml

step "otelcol-contrib validate"
# The token files are read by the bearertokenauth extensions; validate gets placeholders.
tokens=$(mktemp -d)
trap 'rm -rf "$tokens"' EXIT
echo placeholder > "$tokens/cortex.txt"
echo placeholder > "$tokens/context-guru.txt"
chmod 0644 "$tokens"/*.txt
chmod 0755 "$tokens"
$run run --rm -v "$base/otel-collector/otel-collector.yml:/etc/otelcol/config.yml:ro" \
  -v "$tokens:/etc/otelcol/tokens:ro" "$OTELCOL_IMAGE" validate --config=/etc/otelcol/config.yml
echo "collector config valid"

step "dashboards up to date"
python3 "$base/grafana/gen_dashboards.py" --check

step "dashboard generator tests"
python3 -m unittest discover -s "$base/grafana" -p 'test_*.py'

if [ ! -f "$overlay/kustomization.yaml" ]; then
  echo "no overlay yet: skipping render and kubeconform"
  exit 0
fi

step "kustomize render + kubeconform"
rendered=$(mktemp)
trap 'rm -rf "$tokens" "$rendered"' EXIT
if command -v kubectl >/dev/null 2>&1; then kubectl kustomize "$overlay" > "$rendered"
elif command -v oc >/dev/null 2>&1; then oc kustomize "$overlay" > "$rendered"
else $run run --rm -v "$here:/c:ro" "$KUSTOMIZE_IMAGE" build /c/overlays/openshift > "$rendered"
fi
# Route is a built-in OpenShift API, not a CRD, so no public schema exists for it. kubeconform skips
# it; `oc apply -k ... --dry-run=server` checks the Routes against the cluster (README.md).
if command -v kubeconform >/dev/null 2>&1; then kc=kubeconform; else kc="$run run --rm -i $KUBECONFORM_IMAGE"; fi
$kc -strict -summary -skip Route - < "$rendered"

step "generated ConfigMaps carry a content hash"
# configMapGenerator appends -<hash> so a config edit rolls the pods. A name without it means the
# generator was bypassed.
bad=$(awk '/^kind: ConfigMap/{cm=1} cm && /^  name: /{print $2; cm=0}' "$rendered" | grep -v -E -- '-[a-z0-9]{10}$' || true)
if [ -n "$bad" ]; then echo "ConfigMaps without a hash suffix: $bad" >&2; exit 1; fi
echo "ok"

echo
echo "all checks passed"
