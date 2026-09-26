# shellcheck shell=bash
# container-runtime.sh — sourced by build-otel-shim.sh: picks the container
# engine (CONTAINER_TOOL) and loads an image into kind the way that engine
# needs (kind_load). Everything else uses $CONTAINER_TOOL directly.
# KIND_CLUSTER_NAME: target cluster (default rossoctl).

# Podman is checked FIRST: on podman hosts `docker` is often a compat client,
# and `kind load docker-image` through it is exactly the breakage to avoid.
# An explicit CONTAINER_TOOL is validated against the engines kind_load knows:
# the override is advertised generally, but kind_load_${CONTAINER_TOOL} only
# resolves to podman/docker — anything else ran the whole bake and then died
# with "command not found" AFTER both attestations. Fail it here instead.
container_tool() {
  if [ -n "${CONTAINER_TOOL:-}" ]; then
    case "$CONTAINER_TOOL" in
      podman|docker) return 0 ;;
      *) echo "error: CONTAINER_TOOL='$CONTAINER_TOOL' is not supported — use podman or docker" >&2
         echo "  (the kind-load step is engine-specific and only implements those two)" >&2
         return 1 ;;
    esac
  fi
  if command -v podman >/dev/null 2>&1; then CONTAINER_TOOL=podman
  elif command -v docker >/dev/null 2>&1; then CONTAINER_TOOL=docker
  else
    echo "error: neither docker nor podman on PATH (set CONTAINER_TOOL)" >&2
    return 1
  fi
}

# `kind load docker-image` misbehaves under podman v5; save + image-archive.
# The archive is this file's only temp file (an app image is easily multiple GB
# under TMPDIR); it is removed explicitly on the normal and failed-save paths,
# and by a signal trap on Ctrl-C / SIGTERM during `podman save`.
kind_load_podman() {
  local ref="$1" tar rc=0
  tar="$(mktemp "${TMPDIR:-/tmp}/kind-load.XXXXXX")"
  # Clean up on Ctrl-C / SIGTERM during `podman save` (a multi-GB archive),
  # then re-raise the default so the interrupt still aborts. The trap is on
  # INT/TERM only and is cleared before return — a RETURN trap would linger
  # and fire again when the one-line kind_load() wrapper returns, where $tar
  # is out of scope and `set -u` would abort. The normal and failed-save paths
  # remove the archive explicitly below.
  trap 'rm -f "${tar:-}"; trap - INT; kill -s INT $$' INT
  trap 'rm -f "${tar:-}"; trap - TERM; kill -s TERM $$' TERM
  podman save -o "$tar" "$ref" \
    && KIND_EXPERIMENTAL_PROVIDER=podman kind load image-archive "$tar" --name "$KIND_CLUSTER_NAME" \
    || rc=$?
  rm -f "$tar"
  trap - INT TERM
  return "$rc"
}

kind_load_docker() {
  kind load docker-image "$1" --name "$KIND_CLUSTER_NAME"
}

kind_load() { "kind_load_${CONTAINER_TOOL}" "$@"; }

# `return`, not `exit`: sourced — the caller's `set -e` handles it.
container_tool || return 1
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-rossoctl}"
