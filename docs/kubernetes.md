# Running Cortex in Kubernetes

On a laptop Cortex is one binary you install yourself. In a cluster it is a sidecar,
and almost everything else follows from that difference.

## What changes

- **You do not install it per workload.** The
  [operator](https://github.com/rossoctl/operator) injects the sidecar, so a workload
  opts in with a label rather than a command.
- **Identity comes from the cluster.** Workloads get a verifiable identity from
  SPIFFE/SPIRE, and Cortex exchanges it for the credentials each downstream service
  expects — so an agent calling a tool never holds that tool's secret.
- **Keycloak issues and exchanges tokens** (OAuth 2.0 token exchange, RFC 8693).
  Inbound calls are validated against it; outbound calls are re-minted for the target
  audience.
- **Configuration is ConfigMaps**, not `~/.cortex/config.yaml`, and it hot-reloads.

The plugin pipeline, the session API, and `agentop` are the same in both places. If you
learned them on your laptop, they behave identically in a pod — point `agentop` at one
with `kubectl port-forward`, or let it pick a pod from its own Namespaces → Pods
picker.

## Getting a cluster

Cortex needs SPIRE and Keycloak in place. The
[rossoctl](https://github.com/rossoctl/rossoctl) installer sets up both, plus the
[operator](https://github.com/rossoctl/operator) that does the injecting — start from
its [quickstart](https://www.rossoctl.dev/docs/get-started/kubernetes) rather than wiring
them by hand.

## Start here

The **[Weather Agent walkthrough](https://github.com/rossoctl/cortex-demos/blob/main/weather-agent/demo-ui.md)** is the shortest
end-to-end path: a cluster, an agent, inbound validation, and traffic you can watch.
There is also an [`agentop` version](https://github.com/rossoctl/cortex-demos/blob/main/weather-agent/demo-with-agentop.md) that
focuses on the plugin pipeline.

For the full deployment architecture — sidecar shapes, container inventory, the
end-to-end token flow — see [`architecture.md`](architecture.md).

Then, in rough order of how often people need them:

| Topic | Where |
|---|---|
| All the demos, with a recommended order | [demos index](https://github.com/rossoctl/cortex-demos/blob/main/README.md) |
| Deployment modes (proxy-sidecar, envoy-sidecar, lite) | [architecture reference](architecture.md#deployment-modes) |
| Full request flow, what gets verified | [architecture reference](architecture.md) |
| Token exchange per destination | [token-exchange routes](https://github.com/rossoctl/cortex-demos/blob/main/token-exchange-routes/README.md) |
| Writing or configuring a plugin | [plugin reference](./plugin-reference.md) |
| mTLS between workloads | [framework architecture, §8a](framework-architecture.md#8a-mtls-layer) |

## A note on trust boundaries

Two things that are safe in a pod are not safe on a laptop, and the defaults differ
accordingly:

- The **session API** (`:9094` in-cluster, `:47601` locally) is unauthenticated. In a
  pod the network namespace is the boundary. Never expose it through an ingress.
- **Wildcard binds** are correct in a pod and wrong on a laptop, so the laptop config
  sets `listener.bind_loopback_only: true`. Do not set it in a cluster: kubelet probes
  health from outside the container's loopback and would fail.
