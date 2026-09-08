#!/usr/bin/env bash
# attach-lineage.sh — the one generator. Given a Deployment's NAME (and,
# optionally, which of its containers to switch propagation on and with which
# image), print ONE of the two objects that attach lineage to it:
#
#   EMIT=patch (default)  strategic-merge patch adding the sidecar pieces —
#                         proxy-init + envoy-proxy as initContainers (envoy a
#                         NATIVE sidecar: restartPolicy Always, so it is up
#                         before the app container starts — needs k8s >= 1.29),
#                         two config volumes — and, with APP_CONTAINER, the
#                         propagation switch on the app's own container.
#   EMIT=cm               the per-app plugin ConfigMap the sidecar mounts
#                         (parser chain + lineage-telemetry entry).
#   EMIT=undo             the reverse of the patch: one line of strategic-merge
#                         JSON (JSON is YAML) deleting, by name, exactly what
#                         the patch adds — `$patch: delete` on the merge-by-name
#                         lists (proxy-init + envoy-proxy initContainers, the
#                         volumes, the app's LINEAGE_PROPAGATE env).
#                         The image is restored via RESTORE_IMAGE (a replaced
#                         image has no delete directive, only another value);
#                         APP_IMAGE is refused in this mode — it is the ref to
#                         INSTALL, and reusing the attach line verbatim would
#                         otherwise "restore" the -otel image.
#
# Apply both and the app's traffic flows through the lineage plugin. The app
# itself is never described: no Deployment, no Service, no app config — lists
# merge by name, so nothing the owner wrote changes. Stdout only; this script
# never touches the cluster.
#
# Consumed by sidecar-patch.sh (live: checks, applies both, waits) or by your
# own kustomization (the cm under resources:, the patch under patches: with
# target kind Deployment / name NAME) — see README.md "How to attach".
#
# Propagation: capture alone cannot attribute an app's outbound calls to the
# inbound that caused them. The sidecar forwards the trace context (a
# traceparent when none arrived, plus its own tracestate stamp), but only code
# running inside the request can carry it from the inbound to the outbound
# calls it causes — the app must do that itself. For an
# uninstrumented Python app, bake the shim first (build-otel-shim.sh), then
# pass APP_CONTAINER (+ APP_IMAGE): the patch sets LINEAGE_PROPAGATE=1 on that
# container, which wakes the baked hook. Without APP_CONTAINER the patch is
# capture-only and the app container is not touched.
#
# Usage:
#   NAME=echo-upstream ./attach-lineage.sh                     # the patch
#   NAME=echo-upstream EMIT=cm ./attach-lineage.sh             # the ConfigMap
#   NAME=my-agent APP_CONTAINER=agent \
#     APP_IMAGE=docker.io/library/my-agent-otel:latest ./attach-lineage.sh
#
# Variables:
#   NAME            (required) the target Deployment; also names the ConfigMap
#                   (authbridge-lineage-config-NAME) and defaults SELF_ID
#   NAMESPACE       default team1
#   SELF_ID         lineage identity on every span (default NAME)
#   APP_CONTAINER   the app container to set LINEAGE_PROPAGATE=1 on. MUST name
#                   an existing container: a strategic merge ADDS a stub for an
#                   unknown name. Checked by sidecar-patch.sh, not here.
#   APP_IMAGE       needs APP_CONTAINER: the -otel image to set on it
#                   (EMIT=patch only; EMIT=undo refuses it — see RESTORE_IMAGE)
#   RESTORE_IMAGE   EMIT=undo only, needs APP_CONTAINER: the pre-attach image
#                   ref the reverse patch sets back on the app container
#   OTEL_ENDPOINT   OTLP/gRPC target for the plugin's spans (default: the
#                   platform collector). Any OTLP consumer works. Plain gRPC
#                   unless it starts with https://.
#   CAPTURE_IO      false (default, the plugin's own) | true — whether the spans
#                   carry the parsed content (input.value / output.value:
#                   prompts, tool arguments, messages). PII-bearing: opt in.
#   MAX_PAYLOAD_BYTES  cap on a captured value (plugin default 4096 when unset;
#                   -1 = attach whole). Only meaningful with CAPTURE_IO=true.
#   OUTBOUND_PORTS_EXCLUDE  ports proxy-init must not intercept — an app's OWN
#                   telemetry export port (e.g. 4317), or a plaintext non-HTTP
#                   store (Postgres 5432, SMTP 1025: the outbound HTTP codec
#                   would close them). Never LLM/tool/S3 ports.
#   SIDECAR_IMAGE   default ghcr.io/rossoctl/cortex/authbridge-envoy:latest —
#                   UNTIL A RELEASE CARRIES lineage-telemetry (cortex #761) it
#                   boots without the plugin; build from a tree that has it and
#                   point this at your tag (RECIPE.md step 1)
#   PROXY_INIT_IMAGE  default ghcr.io/rossoctl/cortex/proxy-init:latest
#   NO_EMIT=1       omit the plugin entry: the sidecar proxies, emits nothing
#                   (parsers alone are legal). The A/B baseline.
#   EMIT            patch (default) | cm | undo
#
# Structure: parse_inputs validates EVERY knob (all refusals live there);
# build_* each assemble one optional YAML fragment into a global; the three
# fragment functions are the single source of the sidecar YAML; emit()
# dispatches to the emitters.
set -euo pipefail

# Every free-form value lands in a double-quoted YAML or JSON scalar, where
# only '"' and '\' are special, and — via EMIT=undo — inside the single-quoted
# shell line sidecar-patch.sh prints, where "'" is. Refusing those three is
# exactly sufficient. Whitespace and control characters are refused as well:
# never part of an endpoint or image ref, and a control character is illegal
# in YAML even inside quotes.
yaml_safe() {  # $1 = what it is (for the error), $2 = the value
  local unsafe=$'"\'\\'
  case "$2" in
    *["$unsafe"]*|*[[:space:][:cntrl:]]*)
      printf "error: %s '%s' contains whitespace, a control character or one of %s, which this script cannot quote safely\n" "$1" "$2" "$unsafe" >&2
      exit 2 ;;
  esac
}

parse_inputs() {
  # Knobs of the removed app-deployment mode (EMIT=manifest): a caller passing
  # one is running a stale recipe — refuse loudly rather than ignore.
  local stale
  for stale in APP_PORT SVC_PORT ENV_VARS APP_COMMAND APP_RESOURCES \
               PVC_NAME PVC_MOUNT WORKLOAD_TYPE WORKLOAD_PROTOCOL LABEL_PREFIX \
               NO_PROPAGATE APP_ENTRYPOINT; do
    if [ -n "${!stale:-}" ]; then
      echo "error: $stale is not a knob — it was removed with the app-deployment (EMIT=manifest) mode." >&2
      echo "       This script only attaches lineage to an EXISTING Deployment; the app itself" >&2
      echo "       is deployed by its owner. See the header and README.md." >&2
      exit 2
    fi
  done

  EMIT="${EMIT:-patch}"
  NAME="${NAME:?set NAME}"
  NAMESPACE="${NAMESPACE:-team1}"
  # NAME (RFC 1123 subdomain) and NAMESPACE (DNS label) are interpolated bare.
  # 227 = 253 minus the ConfigMap name's prefix (authbridge-lineage-config-).
  if [ "${#NAME}" -gt 227 ] \
     || ! [[ "$NAME" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$ ]]; then
    echo "error: NAME='$NAME' must be a lowercase RFC 1123 subdomain of at most 227 chars (253 minus the ConfigMap prefix)" >&2; exit 2
  fi
  if ! [[ "$NAMESPACE" =~ ^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$ ]]; then
    echo "error: NAMESPACE='$NAMESPACE' is not a DNS label (lowercase alphanumerics and '-', max 63)" >&2; exit 2
  fi
  case "$EMIT" in
    patch|cm|undo) ;;
    *) echo "error: EMIT must be patch|cm|undo (got '$EMIT')" >&2; exit 2 ;;
  esac
  SELF_ID="${SELF_ID:-$NAME}"
  APP_CONTAINER="${APP_CONTAINER:-}"
  APP_IMAGE="${APP_IMAGE:-}"
  RESTORE_IMAGE="${RESTORE_IMAGE:-}"
  # The two image knobs are mode-bound, and confusing them is destructive:
  # regenerating an undo with the attach line's APP_IMAGE would "restore" the
  # -otel image. Refuse the wrong knob loudly rather than reinterpret it.
  if [ "$EMIT" = "undo" ] && [ -n "$APP_IMAGE" ]; then
    echo "error: APP_IMAGE is the image to INSTALL (EMIT=patch) — under EMIT=undo pass the" >&2
    echo "       pre-attach ref to restore as RESTORE_IMAGE instead" >&2
    exit 2
  fi
  if [ "$EMIT" != "undo" ] && [ -n "$RESTORE_IMAGE" ]; then
    echo "error: RESTORE_IMAGE is an EMIT=undo knob (the pre-attach ref the reverse patch restores)" >&2
    exit 2
  fi
  OUTBOUND_PORTS_EXCLUDE="${OUTBOUND_PORTS_EXCLUDE:-}"
  OTEL_ENDPOINT="${OTEL_ENDPOINT:-otel-collector.rossoctl-system.svc.cluster.local:4317}"
  CAPTURE_IO="${CAPTURE_IO:-false}"
  case "$CAPTURE_IO" in
    true|false) ;;
    *) echo "error: CAPTURE_IO must be true|false (got '$CAPTURE_IO')" >&2; exit 2 ;;
  esac
  # Capture carries PII (prompts, tool arguments, messages). The plugin sends
  # it plain gRPC unless OTEL_ENDPOINT starts with https://. In-cluster to the
  # platform collector that is the norm, so this warns rather than refuses —
  # but say it, once, on stderr (the YAML on stdout is unaffected).
  if [ "$CAPTURE_IO" = "true" ]; then
    case "$OTEL_ENDPOINT" in
      https://*) ;;
      *) echo "NOTE: CAPTURE_IO=true sends parsed content (PII) to ${OTEL_ENDPOINT} over plain gRPC." >&2
         echo "      Use an https:// OTEL_ENDPOINT for TLS if that endpoint leaves the cluster." >&2 ;;
    esac
  fi
  MAX_PAYLOAD_BYTES="${MAX_PAYLOAD_BYTES:-}"
  [[ "$MAX_PAYLOAD_BYTES" =~ ^(-1|[1-9][0-9]*)?$ ]] \
    || { echo "error: MAX_PAYLOAD_BYTES must be a positive integer or -1 (got '$MAX_PAYLOAD_BYTES')" >&2; exit 2; }
  # Published images by default; point at local tags when building from source.
  SIDECAR_IMAGE="${SIDECAR_IMAGE:-ghcr.io/rossoctl/cortex/authbridge-envoy:latest}"
  PROXY_INIT_IMAGE="${PROXY_INIT_IMAGE:-ghcr.io/rossoctl/cortex/proxy-init:latest}"
  NO_EMIT="${NO_EMIT:-0}"
  case "$NO_EMIT" in
    0|1) ;;
    *) echo "error: NO_EMIT must be 0|1 (got '$NO_EMIT')" >&2; exit 2 ;;
  esac

  local v
  for v in SELF_ID OTEL_ENDPOINT APP_IMAGE RESTORE_IMAGE SIDECAR_IMAGE PROXY_INIT_IMAGE; do
    yaml_safe "$v" "${!v}"
  done
  # A container name is a DNS label; it is interpolated bare.
  if [ -n "$APP_CONTAINER" ] && ! [[ "$APP_CONTAINER" =~ ^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$ ]]; then
    echo "error: APP_CONTAINER='$APP_CONTAINER' is not a valid container name (DNS label)" >&2; exit 2
  fi
  if [ -n "$APP_IMAGE" ] && [ -z "$APP_CONTAINER" ]; then
    echo "error: APP_IMAGE needs APP_CONTAINER — the image lands on a container the patch must name" >&2; exit 2
  fi
  if [ -n "$RESTORE_IMAGE" ] && [ -z "$APP_CONTAINER" ]; then
    echo "error: RESTORE_IMAGE needs APP_CONTAINER — the ref lands on a container the patch must name" >&2; exit 2
  fi
  # Ports exactly as proxy-init hands them to iptables: no leading zero (iptables
  # reads `010` as octal and refuses `0080`), at most five digits (a longer
  # number overflows the `-gt` test below), then the range.
  [[ "$OUTBOUND_PORTS_EXCLUDE" =~ ^([1-9][0-9]{0,4}(,[1-9][0-9]{0,4})*)?$ ]] \
    || { echo "error: OUTBOUND_PORTS_EXCLUDE='$OUTBOUND_PORTS_EXCLUDE' is not a comma-separated list of ports (1-65535, no leading zeros)" >&2; exit 2; }
  local port
  for port in ${OUTBOUND_PORTS_EXCLUDE//,/ }; do
    if [ "$port" -gt 65535 ]; then
      echo "error: OUTBOUND_PORTS_EXCLUDE has '$port', which is not a port (1-65535)" >&2; exit 2
    fi
  done
}

build_proxy_env() {
  # proxy-init OUTBOUND_PORTS_EXCLUDE env (only if set)
  exclude_env=""
  if [ -n "$OUTBOUND_PORTS_EXCLUDE" ]; then
    exclude_env="
            - name: OUTBOUND_PORTS_EXCLUDE
              value: \"${OUTBOUND_PORTS_EXCLUDE}\""
  fi
}

build_plugin_entry() {
  # The plugin entry, one variable so NO_EMIT is a single point. Empty → the
  # parsers stay and the sidecar is a pure proxy that emits nothing.
  lineage_plugin=""
  if [ "$NO_EMIT" != "1" ]; then
    lineage_plugin='
          - name: lineage-telemetry
            config:
              otel_endpoint: "'"${OTEL_ENDPOINT}"'"
              capture_io: '"${CAPTURE_IO}"'
              self_id: "'"${SELF_ID}"'"'
    # The plugin's own default applies when unset; an explicit value is emitted.
    [ -z "$MAX_PAYLOAD_BYTES" ] || lineage_plugin="${lineage_plugin}
              max_payload_bytes: ${MAX_PAYLOAD_BYTES}"
  fi
}

build_app_patch() {
  # The propagation switch. `containers` and `env` both merge by name, so this
  # sets exactly LINEAGE_PROPAGATE (and the image, if given) on the owner's
  # container and nothing else. One env var is enough: it wakes the baked hook,
  # which sets the propagate-only posture itself as env defaults. This is the
  # ONLY thing the patch adds under `containers:` — the sidecar is a native
  # initContainer now, so the app stays the pod's sole regular container (and
  # its default for `kubectl logs/exec`); no default-container annotation.
  app_patch=""
  if [ -n "$APP_CONTAINER" ]; then
    app_patch="
        - name: ${APP_CONTAINER}"
    if [ -n "$APP_IMAGE" ]; then
      app_patch="${app_patch}
          image: \"${APP_IMAGE}\""
    fi
    app_patch="${app_patch}
          env:
            - { name: LINEAGE_PROPAGATE, value: \"1\" }"
  fi
}

# ---- the sidecar fragments (the single source of every sidecar YAML byte) ----

sidecar_container() {  # the envoy-proxy NATIVE sidecar (8-space list-item indent)
  cat <<EOF
        # Envoy + authbridge-envoy (ext_proc + the lineage plugin). MUST run as
        # UID 1337: proxy-init exempts that uid from the outbound redirect.
        # A NATIVE sidecar: an initContainer with restartPolicy Always, so the
        # kubelet holds the app container until this one's startupProbe passes —
        # proxy-init has already redirected egress, and without this the app can
        # boot and make its first outbound call before the proxy is listening
        # (connection refused). The startupProbe watches the OUTBOUND listener
        # (15123), which is the one an app's startup egress actually hits.
        # readinessProbe stays on the inbound listener: it gates the pod into
        # Service endpoints, a separate concern. Admin binds loopback.
        - name: envoy-proxy
          image: "${SIDECAR_IMAGE}"
          imagePullPolicy: IfNotPresent
          restartPolicy: Always
          args: ["--config", "/etc/authbridge/config.yaml"]
          securityContext:
            runAsNonRoot: true
            runAsUser: 1337
            runAsGroup: 1337
            allowPrivilegeEscalation: false
            seccompProfile: { type: RuntimeDefault }
            capabilities:
              drop: ["ALL"]
          ports:
            - { containerPort: 15123, name: envoy-out }
            - { containerPort: 15124, name: envoy-in }
            - { containerPort: 9090,  name: ext-proc }
          startupProbe:
            tcpSocket: { port: 15123 }
            periodSeconds: 1
            failureThreshold: 60
          readinessProbe:
            tcpSocket: { port: 15124 }
            periodSeconds: 5
          resources:
            requests: { cpu: 50m, memory: 64Mi }
            limits: { cpu: 500m, memory: 512Mi }
          volumeMounts:
            - { name: envoy-config,       mountPath: /etc/envoy,      readOnly: true }
            - { name: authbridge-runtime, mountPath: /etc/authbridge, readOnly: true }
EOF
}

proxy_init_container() {  # the iptables init container
  cat <<EOF
        # Programs the pod's iptables once and exits. Root + exactly
        # NET_ADMIN/NET_RAW, nothing else. POD_IP + POD_IPS (Downward API):
        # proxy-init prefers the plural for full dual-stack redirect and falls
        # back to the singular otherwise.
        - name: proxy-init
          image: "${PROXY_INIT_IMAGE}"
          imagePullPolicy: IfNotPresent
          securityContext:
            runAsNonRoot: false
            runAsUser: 0
            allowPrivilegeEscalation: false
            seccompProfile: { type: RuntimeDefault }
            capabilities:
              drop: ["ALL"]
              add: ["NET_ADMIN", "NET_RAW"]
          resources:
            requests: { cpu: 10m, memory: 16Mi }
            limits: { cpu: 100m, memory: 64Mi }
          env:
            - name: POD_IP
              valueFrom:
                fieldRef:
                  fieldPath: status.podIP
            - name: POD_IPS
              valueFrom:
                fieldRef:
                  fieldPath: status.podIPs${exclude_env}
EOF
}

sidecar_volumes() {  # envoy-config + the per-app runtime ConfigMap
  cat <<EOF
        - name: envoy-config
          configMap:
            name: envoy-config
        - name: authbridge-runtime
          configMap:
            name: authbridge-lineage-config-${NAME}
            items:
              - { key: config.yaml, path: config.yaml }
EOF
}

# ---- the two emittable objects ----

emit_configmap() {
  cat <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: authbridge-lineage-config-${NAME}
  namespace: ${NAMESPACE}
data:
  config.yaml: |
    mode: envoy-sidecar
    # The same parser chain in both directions: an app's entry protocol is
    # not knowable from the attach side, and a direction-specific chain would
    # silently mislabel what it was not given as plain http. Uniform is safe —
    # parsers are content-gated and not mutually exclusive (mcp-parser attaches
    # to any JSON-RPC body), and the plugin reads payloads only through the
    # protocol fact it stamped (precedence a2a > mcp > inference).
    pipeline:
      inbound:
        plugins:
          - name: a2a-parser
          - name: mcp-parser
          - name: inference-parser${lineage_plugin}
      outbound:
        plugins:
          - name: a2a-parser
          - name: mcp-parser
          - name: inference-parser${lineage_plugin}
EOF
}

emit_patch() {  # strategic merge: lists merge by name — the owner's spec is untouched
  # apiVersion/kind/metadata make it a complete resource, which kustomize
  # requires of a patch file; kubectl patch merges them harmlessly.
  # proxy-init (to completion) then envoy-proxy (native sidecar) are both
  # initContainers; the app container is touched only to switch propagation on.
  # Emit `containers:` ONLY when there is an app fragment — a bare `containers:`
  # key is `containers: null` in a strategic merge and would clobber the owner's
  # container list.
  local containers_block=""
  [ -z "$app_patch" ] || containers_block="      containers:${app_patch}
"
  cat <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${NAME}
  namespace: ${NAMESPACE}
spec:
  template:
    spec:
      initContainers:
$(proxy_init_container)
$(sidecar_container)
${containers_block}      volumes:
$(sidecar_volumes)
EOF
}

emit_undo() {
  # The exact inverse of emit_patch, in one line of compact JSON so it can ride
  # inside the single-quoted back-out line sidecar-patch.sh prints. Lists that
  # merge by name un-merge by name: `$patch: delete` removes exactly the items
  # the patch added and touches nothing else, at ANY later time — unlike a
  # `rollout undo`, which restores a whole earlier pod template and silently
  # takes with it whatever the owner changed since the attach. proxy-init AND
  # envoy-proxy are both initContainers now (the native sidecar), so both are
  # deleted from that list. The image is the one non-additive field — a replaced
  # value has no delete, only another value — so RESTORE_IMAGE is the pre-attach
  # ref to restore, captured by the caller (APP_IMAGE is refused in this mode;
  # see parse_inputs). `containers` appears only when the attach touched one.
  local app_containers=""
  if [ -n "$APP_CONTAINER" ]; then
    local app='{"name":"'"${APP_CONTAINER}"'"'
    [ -z "$RESTORE_IMAGE" ] || app="${app},\"image\":\"${RESTORE_IMAGE}\""
    app="${app},\"env\":[{\"name\":\"LINEAGE_PROPAGATE\",\"\$patch\":\"delete\"}]}"
    app_containers=',"containers":['"${app}"']'
  fi
  printf '{"spec":{"template":{"spec":{"initContainers":[{"name":"proxy-init","$patch":"delete"},{"name":"envoy-proxy","$patch":"delete"}]%s,"volumes":[{"name":"envoy-config","$patch":"delete"},{"name":"authbridge-runtime","$patch":"delete"}]}}}}\n' \
    "$app_containers"
}

emit() {
  case "$EMIT" in
    cm)    emit_configmap ;;
    patch) emit_patch ;;
    undo)  emit_undo ;;
  esac
}

main() {
  [ $# -eq 0 ] || { echo "error: attach-lineage.sh takes no arguments — every input is an environment variable (see the header)" >&2; exit 2; }
  parse_inputs        # every knob: read, default, validate — all refusals live here
  build_proxy_env     # optional OUTBOUND_PORTS_EXCLUDE env for proxy-init
  build_plugin_entry  # the lineage-telemetry pipeline entry (empty under NO_EMIT=1)
  build_app_patch     # optional propagation switch on the app's own container
  emit                # dispatch: patch | cm
}
main "$@"
