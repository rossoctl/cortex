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
learned them on your laptop, they behave identically in a pod.

## The journey

Do these steps in this order.

| Step | What to do | Where |
|---|---|---|
| 1. Install the platform | Install SPIRE, Keycloak and the [operator](https://github.com/rossoctl/operator). The rossoctl installer installs all three. Do not connect them manually. | [quickstart](https://www.rossoctl.dev/docs/get-started/kubernetes/), [install reference](https://github.com/rossoctl/rossoctl/blob/main/docs/operate/install-kubernetes.md) |
| 2. Deploy an agent | Deploy the Weather Agent. It is the shortest end-to-end path: an agent, inbound validation, and traffic that you can watch. | [Weather Agent walkthrough](https://github.com/rossoctl/cortex-demos/blob/main/weather-agent/demo-ui.md) |
| 3. Opt a workload in | Label the namespace and the workload. | [Opt a workload in](#opt-a-workload-in) |
| 4. Verify | Make sure that the pod has the sidecar and that the sidecar serves. | [Verify the sidecar](#verify-the-sidecar) |
| 5. Configure plugins | Edit the ConfigMap `authbridge-config-<agent>` with `kubectl edit configmap authbridge-config-<agent> -n <ns>`. The pod mounts it at `/etc/authbridge/config.yaml`. Kubelet syncs the mount in about 60 s, and the sidecar then reloads it. | [config shape](framework-architecture.md#example), [hot reload](framework-architecture.md#9-config-hot-reload), [plugin catalog](plugin-catalog.md) |
| 6. Troubleshoot | Find the symptom and do the fix. | [Troubleshooting](#troubleshooting) |

More topics:

| Topic | Where |
|---|---|
| All the demos, with a recommended order | [demos index](https://github.com/rossoctl/cortex-demos/blob/main/README.md) |
| The Weather Agent with `agentop`, about the plugin pipeline | [`agentop` walkthrough](https://github.com/rossoctl/cortex-demos/blob/main/weather-agent/demo-with-agentop.md) |
| Deployment modes (proxy-sidecar, envoy-sidecar) | [binaries and images](../cmd/README.md) |
| Sidecar shapes, container inventory, full request flow | [architecture reference](architecture.md) |
| Token exchange per destination | [token-exchange routes](https://github.com/rossoctl/cortex-demos/blob/main/token-exchange-routes/README.md) |
| Writing or configuring a plugin | [plugin reference](./plugin-reference.md) |
| mTLS between workloads | [framework architecture, §8a](framework-architecture.md#8a-mtls-layer) |

## Opt a workload in

<!-- Checked against rossoctl/operator e9a2c225: operator/internal/webhook/injector/pod_mutator.go (InjectAuthBridge), constants.go, charts/operator/templates/webhook/webhooks.yaml. -->

The operator's webhook `inject.rossoctl.io` adds the sidecar. It adds it only when all
of these conditions are true:

- The namespace has the label `rossoctl-enabled: "true"`. The webhook ignores
  `kube-system`, `kube-public`, `kube-node-lease` and the operator's own namespace.
- The pod has the label `rossoctl.io/type: agent`. The value `tool` also works, but
  only when the feature gate `injectTools` is on. Its default is off.
- The pod does not have the label `rossoctl.io/inject: disabled`. This label opts the
  workload out. Other values of this label have no effect.
- The feature gate `globalEnabled` is on. This is the default. When it is off, the
  operator injects into no workload.

The feature gates are in the ConfigMap `rossoctl-feature-gates`, key
`feature-gates.yaml`, in the operator's release namespace.

The webhook acts only when a pod is created. A pod that runs before you add the labels
gets no sidecar. Restart the rollout to create new pods:
`kubectl rollout restart deploy/<name> -n <ns>`.

This is a minimal example:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: team1
  labels:
    rossoctl-enabled: "true"
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-agent
  namespace: team1
spec:
  selector:
    matchLabels: {app: my-agent}
  template:
    metadata:
      labels:
        app: my-agent
        rossoctl.io/type: agent      # opt in
        # rossoctl.io/inject: disabled   # opt out
    spec:
      containers:
        - name: agent
          image: <your-agent-image>
```

The operator gives the sidecar one of two shapes: `proxy-sidecar` (the default) or
`envoy-sidecar`. It uses the first value that it finds in this list:

1. `mode:` in the ConfigMap `authbridge-runtime-config` in the namespace.
2. The deprecated annotation `rossoctl.io/authbridge-mode`.
3. `proxy-sidecar`.

The field `AgentRuntime.Spec.AuthBridgeMode` is validated, but the injector does not
use it yet. The operator also accepts two other mode values. `lite` is the
proxy-sidecar shape with the `authbridge-lite` image. `waypoint` is a standalone
deployment, and the operator does not inject it.

## Verify the sidecar

1. List the containers of the pod:

   ```sh
   kubectl get pod <pod> -n <ns> -o jsonpath='{.spec.containers[*].name}'
   ```

   The list shows `authbridge-proxy` in proxy-sidecar mode and in lite mode. It shows
   `envoy-proxy` in envoy-sidecar mode.
2. Read the sidecar logs:

   ```sh
   kubectl logs <pod> -n <ns> -c authbridge-proxy
   ```

3. Read the session API on `:9094`. The port-forward commands are in
   [session API quick examples](session-api.md#quick-examples).
4. Read the reload status on `:9093`. The commands are in
   [config hot reload](framework-architecture.md#9-config-hot-reload).
5. Point `agentop` at the pod with `kubectl port-forward`, or let it select a pod from
   its own Namespaces → Pods picker.

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| The pod has no sidecar | The namespace does not have the label `rossoctl-enabled: "true"` | Add the label to the namespace, then restart the rollout |
| The pod has no sidecar | The pod does not have the label `rossoctl.io/type: agent`, or it has `rossoctl.io/inject: disabled` | Correct the labels in the pod template |
| The pod has no sidecar | The pod has `rossoctl.io/type: tool` and the feature gate `injectTools` is off | Set `injectTools: true` in `rossoctl-feature-gates` |
| The pod has no sidecar | The feature gate `globalEnabled` is off | Set `globalEnabled: true` in `rossoctl-feature-gates` |
| The pod has no sidecar | The pod was created before you added the labels | `kubectl rollout restart deploy/<name> -n <ns>` |
| The sidecar refuses a config reload | You changed a field that needs a restart, for example `mode` or a `listener` field | `/reload/status` names the error and `reloads_failed` increments. The old pipeline continues to serve. Restart the pod |
| Lineage data is missing | See the lineage kit | [lineage troubleshooting](../deploy/lineage-attach/README.md#troubleshooting) |
| SPIRE, Keycloak or the operator has a problem | A platform fault | [platform troubleshooting](https://www.rossoctl.dev/docs/operate/troubleshooting/) |

## A note on trust boundaries

Two things that are safe in a pod are not safe on a laptop, and the defaults differ
accordingly:

- The **session API** (`:9094` in-cluster, `:47601` locally) is unauthenticated. In a
  pod the network namespace is the boundary. Never expose it through an ingress.
- **Wildcard binds** are correct in a pod and wrong on a laptop, so the laptop config
  sets `listener.bind_loopback_only: true`. Do not set it in a cluster: kubelet probes
  health from outside the container's loopback and would fail.
