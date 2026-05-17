.PHONY: docker-build-init load-image

KIND_CLUSTER_NAME ?= kagenti

# Container runtime — defaults to podman (the repo's primary toolchain)
# but auto-falls-through to docker if podman isn't installed. Override
# explicitly with `make CONTAINER_RUNTIME=docker docker-build-init`.
CONTAINER_RUNTIME ?= $(shell command -v podman >/dev/null 2>&1 && echo podman || echo docker)

# Build the proxy-init image (iptables init container, used by
# AuthBridge envoy-sidecar mode for transparent traffic interception).
docker-build-init:
	$(CONTAINER_RUNTIME) build -f Dockerfile.init -t proxy-init:latest .

# Load the locally-built proxy-init image into a Kind cluster. Podman
# tags images under localhost/, but kubelet looks under
# docker.io/library/ for unqualified names — so on podman we need an
# extra `ctr images tag` inside the kind node. Docker doesn't have
# this problem; the second step is a no-op skipped by the `ifeq`.
KIND_NODE ?= $(KIND_CLUSTER_NAME)-control-plane
load-image:
	kind load docker-image proxy-init:latest --name $(KIND_CLUSTER_NAME)
ifeq ($(CONTAINER_RUNTIME),podman)
	$(CONTAINER_RUNTIME) exec $(KIND_NODE) ctr --namespace=k8s.io images tag \
	  localhost/proxy-init:latest docker.io/library/proxy-init:latest
endif
