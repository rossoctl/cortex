#!/usr/bin/env bash
# sidecar-patch.sh — attach the lineage sidecar to an EXISTING Deployment.
#
# The live applier. The Deployment is deployed and owned by someone else; this
# script only ADDS the lineage pieces through a strategic-merge patch (lists
# merge by name, so nothing the owner wrote changes; the app container is
# touched only when APP_CONTAINER opts it in). Every YAML byte comes from
# attach-lineage.sh: EMIT=cm (the plugin ConfigMap), EMIT=patch (the sidecar).
# This script checks, applies both, and waits for the rollout.
#
# Not durable: the owner keeps owning the object, and a platform rewrite (an
# operator reconcile, a UI redeploy) silently drops the patch — observed live
# when an operator reconciled a patched Deployment. Re-run after any
# platform-side change, or keep the attachment in your own manifests instead
# (README.md "Bring your own manifests"). To back out: the reverse-patch line
# this script prints last — a strategic merge that deletes, by name, exactly
# what the attach added and restores the app image it replaced, so it is right
# at ANY later time, whatever else rolled the Deployment since — then delete
# the CM. (A `rollout undo` is NOT the back-out: it restores a whole earlier
# pod template, silently taking the owner's later changes with it.)
#
# Refused: a target that already carries a container named `envoy-proxy` or an
# init container named `proxy-init` (the operator's sidecar, another mesh's
# init, a leftover of an earlier attach — the merge would silently take it
# over rather than sit beside it), one that declares 9090, 15123 or 15124
# (an undeclared sidecar, or the app itself on a port the sidecar binds), or
# one that already has a volume named `envoy-config` or `authbridge-runtime`
# (volumes merge by name too — the merge would repoint the volume's source
# while the owner's volumeMounts keep serving it).
#
# Propagation: an uninstrumented app also needs the shim — bake it with
# build-otel-shim.sh, then pass APP_CONTAINER (+ APP_IMAGE) so the patch
# flips LINEAGE_PROPAGATE=1 on the app's own container. Without it this is
# capture only (README.md "The propagation half").
#
# Usage:
#   DEPLOY=echo-upstream ./sidecar-patch.sh
#   DEPLOY=my-agent APP_CONTAINER=agent \
#     APP_IMAGE=docker.io/library/my-agent-otel:latest ./sidecar-patch.sh
#
# Env — read here:
#   DEPLOY         target Deployment (required)
#   NAMESPACE      default team1
#   SELF_ID        lineage identity (default: DEPLOY)
#   APP_CONTAINER  the app container to switch propagation on (optional)
# Env — inherited by attach-lineage.sh and validated there (see its header):
#   APP_IMAGE, OTEL_ENDPOINT, CAPTURE_IO (content capture, off unless true),
#   MAX_PAYLOAD_BYTES (cap on a captured value; plugin default 4096, -1 = whole),
#   SIDECAR_IMAGE, PROXY_INIT_IMAGE, NO_EMIT,
#   OUTBOUND_PORTS_EXCLUDE (an app's OWN telemetry port, or a plaintext non-HTTP store
#                   port such as Postgres/SMTP — never LLM/tool/S3 ports).
#
# Requires in the namespace: the platform's `envoy-config` ConfigMap; the
# sidecar + proxy-init images resolvable from the cluster (README "Prerequisites and configuration").
#
# Structure: read_inputs → preconditions (six, read-only; each returns or exits) →
# note_capture_only → apply (the only cluster writes). gen() is the one bridge
# to the generator.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

read_inputs() {
  DEPLOY="${DEPLOY:?usage: DEPLOY=<deployment> [NAMESPACE=team1] [SELF_ID=<id>] [APP_CONTAINER=<name> [APP_IMAGE=<ref>]] [SIDECAR_IMAGE=<ref> PROXY_INIT_IMAGE=<ref>] [OUTBOUND_PORTS_EXCLUDE=ports] sidecar-patch.sh}"
  NAMESPACE="${NAMESPACE:-team1}"
  SELF_ID="${SELF_ID:-$DEPLOY}"
  APP_CONTAINER="${APP_CONTAINER:-}"
}

require_deployment() {
  kubectl get deploy -n "$NAMESPACE" "$DEPLOY" >/dev/null
}

require_envoy_config() {  # the patch mounts it; missing → the pod never starts
  kubectl get cm -n "$NAMESPACE" envoy-config >/dev/null || {
    echo "error: ConfigMap envoy-config missing in $NAMESPACE (rendered by the platform chart)" >&2
    exit 1
  }
}

refuse_name_collision() {
  # Lists merge by NAME: a target already carrying either name is merged over,
  # not added beside. Init containers included — that is where proxy-init
  # lands. (A native sidecar, an initContainer with restartPolicy Always, is
  # checked here only under these two names; the port check below ranges
  # initContainers too. envoy-proxy is itself emitted as a native-sidecar
  # initContainer, so a re-attach is caught here by name.)
  local names n
  names="$(kubectl get deploy -n "$NAMESPACE" "$DEPLOY" \
    -o jsonpath='{range .spec.template.spec.initContainers[*]}{.name}{" "}{end}{range .spec.template.spec.containers[*]}{.name}{" "}{end}')"
  for n in envoy-proxy proxy-init; do
    case " $names " in
      *" $n "*)
        echo "error: $DEPLOY already has a container named $n — the patch would merge over it, not add beside it" >&2
        echo "  (an operator-injected sidecar, another mesh's init, or an earlier attach) — refusing" >&2
        exit 1 ;;
    esac
  done
}

refuse_port_collision() {
  # An existing sidecar or the app on a sidecar port — see the header. Ports are
  # what a Deployment declares; an undeclared app port cannot be seen from here.
  # initContainers included: a native sidecar (restartPolicy Always) holds its
  # ports at runtime (envoy-proxy, the kit's own, is one — so a re-attach or a
  # foreign native sidecar on these ports is caught here).
  local declared_ports p
  declared_ports="$(kubectl get deploy -n "$NAMESPACE" "$DEPLOY" \
    -o jsonpath='{range .spec.template.spec.initContainers[*].ports[*]}{.containerPort}{" "}{end}{range .spec.template.spec.containers[*].ports[*]}{.containerPort}{" "}{end}')"
  for p in 15124 15123 9090; do
    case " $declared_ports " in
      *" $p "*)
        echo "error: $DEPLOY already declares containerPort $p, which the lineage sidecar binds" >&2
        echo "  (an operator-injected sidecar, or the app itself on that port) — refusing to patch over it" >&2
        exit 1 ;;
    esac
  done
}

refuse_volume_collision() {
  # Volumes merge by NAME too: an existing volume by either name would have
  # its source silently repointed at our ConfigMap (and a different-type
  # volume would make the API server reject the merged object). The owner's
  # volumeMounts keep referencing the name, so the damage would surface only
  # at the next pod start, with nothing in a diff.
  local volumes v
  volumes="$(kubectl get deploy -n "$NAMESPACE" "$DEPLOY" \
    -o jsonpath='{range .spec.template.spec.volumes[*]}{.name}{" "}{end}')"
  for v in envoy-config authbridge-runtime; do
    case " $volumes " in
      *" $v "*)
        echo "error: $DEPLOY already has a volume named $v — the patch would repoint its source, not add beside it" >&2
        echo "  (the owner mounts that volume somewhere; the merge would silently change what the mount serves) — refusing" >&2
        exit 1 ;;
    esac
  done
}

require_app_container() {
  # A strategic merge ADDS a stub container for an unknown name instead of
  # failing — so the name must exist. The generator cannot check this.
  [ -n "$APP_CONTAINER" ] || return 0
  local containers
  containers="$(kubectl get deploy -n "$NAMESPACE" "$DEPLOY" \
    -o jsonpath='{range .spec.template.spec.containers[*]}{.name}{" "}{end}')"
  case " $containers " in
    *" $APP_CONTAINER "*) ;;
    *)
      echo "error: deploy/$DEPLOY has no container named '$APP_CONTAINER' (it has: ${containers% })" >&2
      echo "  — refusing: the patch would ADD a stub container by that name instead of failing" >&2
      exit 1 ;;
  esac
}

note_capture_only() {
  # Whether the app propagates traceparent is a property of the app that
  # nothing here can see — say so once instead of guessing from its env.
  [ -z "$APP_CONTAINER" ] || return 0
  echo "NOTE: the sidecar records every hop; whether $DEPLOY's outbound hops attribute to" >&2
  echo "      their inbound depends on the app carrying the trace context (traceparent +" >&2
  echo "      tracestate) from inbound to outbound itself (its own instrumentation, or the" >&2
  echo "      baked shim + APP_CONTAINER=<name>). Verify pairing" >&2
  echo "      under concurrency before relying on it (DESIGN.md, 'The envelope')." >&2
}

gen() {  # $1 = EMIT mode; the other knobs reach the generator through the environment
  EMIT="$1" NAME="$DEPLOY" SELF_ID="$SELF_ID" NAMESPACE="$NAMESPACE" \
    "${SCRIPT_DIR}/attach-lineage.sh"
}

apply() {
  # All three objects — ConfigMap, patch, and its reverse — are generated
  # before the first write, so a generator refusal stops the script with
  # nothing applied. ConfigMap first — the patch's volume names it.
  local cm patch undo restored_image cm_existed dryrun_err
  cm="$(gen cm)"
  patch="$(gen patch)"
  # The image is the one piece the patch REPLACES rather than adds, so the
  # reverse patch needs a value, not a delete: capture the ref the owner runs
  # now, before the patch swaps it. Every other added piece un-merges by name.
  restored_image=""
  if [ -n "${APP_IMAGE:-}" ]; then
    restored_image="$(kubectl get deploy -n "$NAMESPACE" "$DEPLOY" \
      -o jsonpath="{.spec.template.spec.containers[?(@.name=='$APP_CONTAINER')].image}")"
    [ -n "$restored_image" ] || {
      echo "error: could not read the current image of container '$APP_CONTAINER' — nothing was applied" >&2
      exit 1
    }
  fi
  undo="$(EMIT=undo NAME="$DEPLOY" NAMESPACE="$NAMESPACE" \
          APP_IMAGE= RESTORE_IMAGE="$restored_image" "${SCRIPT_DIR}/attach-lineage.sh")"
  # The server validates the FULLY MERGED object without persisting it, so
  # every rejection class — an invalid merged field, an admission webhook,
  # RBAC missing deployments/patch, and a cluster too old for native sidecars —
  # fails here, before the first write. This is the version guard too: on k8s
  # < 1.29 the server rejects the native-sidecar fields (startupProbe/
  # restartPolicy on an init container), so no version parsing is needed.
  local dryrun_err
  if ! dryrun_err="$(kubectl patch deploy "$DEPLOY" -n "$NAMESPACE" --type strategic \
        --patch "$patch" --dry-run=server -o name 2>&1)"; then
    echo "error: the server rejected the merged patch — nothing was applied:" >&2
    printf '%s\n' "$dryrun_err" >&2
    case "$dryrun_err" in
      *startupProbe*|*restartPolicy*|*"init container"*)
        echo "  hint: rejection of the native-sidecar fields (startupProbe / restartPolicy on an init" >&2
        echo "        container) means the cluster is older than k8s 1.29, which the kit requires." >&2 ;;
    esac
    exit 1
  fi
  # On a re-attach the ConfigMap already exists and running pods project it:
  # the failure compensation below may delete only what THIS run created.
  cm_existed=0
  if kubectl get cm -n "$NAMESPACE" "authbridge-lineage-config-$DEPLOY" >/dev/null 2>&1; then
    cm_existed=1
  fi
  kubectl apply -f - <<<"$cm"
  kubectl patch deploy "$DEPLOY" -n "$NAMESPACE" --type strategic --patch "$patch" || {
    # Only a failure the dry-run could not predict lands here (e.g. a 409
    # from a concurrent write). Nothing else was written this run except,
    # possibly, the ConfigMap — remove it only if this run created it.
    [ "$cm_existed" = "1" ] || kubectl delete cm -n "$NAMESPACE" "authbridge-lineage-config-$DEPLOY"
    exit 1
  }
  # Said before the wait: a rollout that never completes still needs this line.
  # CM after the patch: pods of a revision that still mounts it cannot start.
  echo ">> back out: kubectl -n $NAMESPACE patch deploy/$DEPLOY --type strategic -p '$undo' && kubectl -n $NAMESPACE delete cm authbridge-lineage-config-$DEPLOY"
  [ -z "$restored_image" ] || \
    echo ">>   (the patch restores image $restored_image — drop its \"image\" field if the app is re-imaged after this attach)"
  # A rollout that times out is deliberately left patched (unlike the patch-
  # apply failure above, which auto-cleans): Kubernetes holds the blast —
  # maxUnavailable keeps the old pod serving — and the back-out line printed
  # just above is the clean way out. Undoing here would fight a slow-but-
  # healthy rollout, and the likely cause (an image absent from the node, an
  # SCC/PodSecurity denial creating pods) wants the operator's eyes, not a
  # silent revert.
  kubectl rollout status -n "$NAMESPACE" "deploy/$DEPLOY" --timeout=180s
  echo ">> lineage sidecar attached to deploy/$DEPLOY (self_id=$SELF_ID, ns=$NAMESPACE)"
}

preconditions() {  # read-only: each returns or exits — nothing is applied yet
  require_deployment
  require_envoy_config
  refuse_name_collision
  refuse_port_collision
  refuse_volume_collision
  require_app_container
}

main() {
  read_inputs        # DEPLOY required; the rest defaulted or inherited
  preconditions      # six checks that can only stop the script
  note_capture_only  # no APP_CONTAINER → say what that means, once
  apply              # generate both, then the only cluster writes: cm → patch → rollout
}
main "$@"
