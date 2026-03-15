# kagenti-webhook

A Kubernetes admission webhook that automatically injects sidecar containers to enable Keycloak client registration and optional SPIFFE/SPIRE token exchanges for secure service-to-service authentication within the Kagenti platform.

## Overview

This webhook provides security by automatically injecting sidecar containers that handle identity and authentication. It intercepts **Pod CREATE** requests (not Deployments or other workload objects), which eliminates GitOps drift — the workload object in etcd remains exactly as the developer defined it, and sidecars are only visible at the Pod level.

The webhook injects:

1. **`proxy-init`** (init container) - Configures iptables rules for traffic interception
2. **`envoy-proxy`** - Service mesh proxy for traffic management
3. **`spiffe-helper`** - Obtains SPIFFE Verifiable Identity Documents (SVIDs) from the SPIRE agent via the Workload API
4. **`kagenti-client-registration`** - Registers the resource as an OAuth2 client in Keycloak using the SPIFFE identity

All four sidecars are injected by default for eligible agent workloads. Each can be disabled independently via feature gates or per-workload labels.

### Why Sidecar Injection?

The sidecar approach provides a consistent pattern for extending functionality without modifying application code or upstream components.

### Upgrading from the opt-in model

Previous versions required `kagenti.io/inject: enabled` to trigger sidecar injection. This version uses an **opt-out** model: any workload with `kagenti.io/type: agent` is injected by default.

**Impact on upgrade:** Existing agent workloads that relied on the *absence* of `kagenti.io/inject: enabled` to avoid injection will now receive sidecars. To preserve the previous behavior, add `kagenti.io/inject: disabled` to those workloads before upgrading.

### BREAKING CHANGE: Namespace opt-in required

The webhook now requires namespaces to have the `kagenti-enabled: "true"` label to receive sidecar injection. Existing namespaces that previously received injection will **silently stop** unless labeled.

**Migration step:** Label each namespace where agent workloads run:

```bash
kubectl label ns <namespace> kagenti-enabled=true
```

The `webhook-rollout.sh` script automatically labels the AuthBridge demo namespace when `AUTHBRIDGE_DEMO=true` is set.

## Supported Resources

The **AuthBridge webhook** intercepts **Pod CREATE** requests in the core API group (`v1`). It injects sidecars into Pods created by any workload controller — Deployments, StatefulSets, DaemonSets, Jobs, and CronJobs all produce Pods that the webhook can mutate.

This Pod-level targeting follows the same pattern used by Istio, Linkerd, and Vault Agent Injector. Because the webhook mutates Pods (not the parent Deployment), GitOps tools like Argo CD and Flux see no drift on the workload object stored in etcd.

## Injection Control

Injection is evaluated in two stages. Any "no" at Stage 1 skips all injection immediately. Within Stage 2, each sidecar is evaluated independently.

### Stage 1 — Workload pre-filters (PodMutator)

Evaluated in order for every admission request:

| # | Check | How to configure | Skip condition |
| --- | --- | --- | --- |
| 1 | **Workload type** | `kagenti.io/type: agent` or `tool` label on workload | Label absent or value is not `agent`/`tool` |
| 2 | **Global kill switch** | `featureGates.globalEnabled` in Helm values | `false` — disables ALL injection cluster-wide |
| 3 | **Tool gate** | `featureGates.injectTools` in Helm values | Type is `tool` AND gate is `false` (default) |
| 4 | **Whole-workload opt-out** | `kagenti.io/inject: disabled` on workload | Label explicitly set to `disabled` |

No namespace label is required. Agents are injected by default; tools are not (opt-in via `featureGates.injectTools: true`).

### Stage 2 — Per-sidecar precedence chain (PrecedenceEvaluator)

Once past Stage 1, each of the three sidecars runs independently through a two-layer chain. `proxy-init` is not evaluated separately — it always mirrors the `envoy-proxy` decision.

```text
Per-Sidecar Feature Gate → Workload Opt-Out Label → Inject
```

| Layer | Scope | How to configure | Effect |
| --- | --- | --- | --- |
| 1. Per-sidecar feature gate | Cluster-wide, per sidecar | `featureGates.envoyProxy`, `.spiffeHelper`, `.clientRegistration` | Disables a specific sidecar cluster-wide |
| 2. Workload opt-out label | Per-workload, per sidecar | `kagenti.io/<sidecar>-inject: "false"` on pod template | Disables a specific sidecar for that workload |

`proxy-init` always follows the `envoy-proxy` decision and is never independently controlled.

### Feature Gates

Feature gates provide cluster-wide control over sidecar injection. They are configured via Helm values and deployed as a ConfigMap with hot-reload support.

```yaml
# values.yaml
featureGates:
  globalEnabled: true        # Kill switch — set to false to disable ALL injection
  envoyProxy: true           # Set to false to disable envoy-proxy cluster-wide
  spiffeHelper: true         # Set to false to disable spiffe-helper cluster-wide
  clientRegistration: true   # Set to false to disable client-registration cluster-wide
  injectTools: false         # Set to true to enable injection for tool workloads
```

### Workload-Level Control

Workloads must have the `kagenti.io/type` label set to `agent` or `tool` to be eligible for injection. Without this label, injection is always skipped. No namespace label is required.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-agent
  namespace: my-apps
spec:
  template:
    metadata:
      labels:
        app: my-agent
        kagenti.io/type: agent   # Required: identifies workload as an agent (injected by default)
    spec:
      containers:
      - name: app
        image: my-app:latest
```

To opt a workload out of all injection:

```yaml
labels:
  kagenti.io/type: agent
  kagenti.io/inject: disabled   # Skips all sidecar injection for this workload
```

### Per-Sidecar Workload Labels

Individual sidecars can be disabled per-workload using these labels on the pod template. Setting a label to `"false"` opts that workload out of the corresponding sidecar.

| Label | Controls |
| --- | --- |
| `kagenti.io/envoy-proxy-inject: "false"` | Disables envoy-proxy (and proxy-init) |
| `kagenti.io/spiffe-helper-inject: "false"` | Disables spiffe-helper (and SPIRE volumes/SA) |
| `kagenti.io/client-registration-inject: "false"` | Disables client-registration |

Example — inject envoy and spiffe-helper, but not client-registration:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
  namespace: my-apps
spec:
  template:
    metadata:
      labels:
        kagenti.io/type: agent
        kagenti.io/client-registration-inject: "false"
    spec:
      containers:
      - name: app
        image: my-app:latest
```

### Platform Configuration

Container images, resource limits, proxy settings, and other parameters are externalized into a ConfigMap managed through Helm values. The webhook loads this configuration at startup and supports hot-reload via file watching.

```yaml
# values.yaml — defaults section
defaults:
  images:
    envoyProxy: ghcr.io/kagenti/kagenti-extensions/envoy-with-processor:latest
    proxyInit: ghcr.io/kagenti/kagenti-extensions/proxy-init:latest
    spiffeHelper: ghcr.io/spiffe/spiffe-helper:0.11.0
    clientRegistration: ghcr.io/kagenti/kagenti-extensions/client-registration:latest
    pullPolicy: IfNotPresent
  proxy:
    port: 15123
    uid: 1337
    adminPort: 9901
    inboundProxyPort: 15124
  resources:
    envoyProxy:
      requests: { cpu: 50m, memory: 64Mi }
      limits: { cpu: 200m, memory: 256Mi }
    # ... (proxyInit, spiffeHelper, clientRegistration)
  sidecars:
    envoyProxy: { enabled: true }
    spiffeHelper: { enabled: true }
    clientRegistration: { enabled: true }
```

If the ConfigMap is not available, compiled defaults are used as a fallback.

## Architecture

### AuthBridge Architecture

The AuthBridge webhook supports two modes of operation:

#### With SPIRE Integration

```text
┌────────────────────────────────────────────────────────────────┐
│                    Kubernetes Workload Pod                     │
│                                                                │
│  ┌──────────────┐  ┌────────────┐  ┌─────────────────────────┐│
│  │  proxy-init  │  │ envoy-proxy│  │   spiffe-helper         ││
│  │ (init)       │  │            │  │                         ││
│  │ - Setup      │  │ - Service  │  │ 1. Connects to SPIRE    ││
│  │   iptables   │  │   mesh     │  │ 2. Gets JWT-SVID        ││
│  │ - Redirect   │  │   proxy    │  │ 3. Writes jwt_svid.token││
│  │   traffic    │  │            │  │                         ││
│  └──────────────┘  └────────────┘  └─────────────────────────┘│
│                                              │                 │
│                          ┌───────────────────▼──────────────┐  │
│  ┌─────────────────────┐ │    Shared Volume: /opt          │  │
│  │ client-registration │─│                                  │  │
│  │                     │ └──────────────────────────────────┘  │
│  │ 1. Waits for token  │                                       │
│  │ 2. Registers with   │                                       │
│  │    Keycloak         │                                       │
│  └─────────────────────┘                                       │
│                                                                │
│  ┌──────────────────────────────────────────────────────────┐ │
│  │              Your Application Container                   │ │
│  └──────────────────────────────────────────────────────────┘ │
└────────────────────────────────────────────────────────────────┘
         │                    │                    │
         ▼                    ▼                    ▼
  SPIRE Agent Socket   Keycloak Server      Application Traffic
                       (OAuth2/OIDC)        (via Envoy proxy)
```

#### Without SPIRE Integration (opt-out via `kagenti.io/spiffe-helper-inject: "false"`)

```text
┌────────────────────────────────────────────────────────────────┐
│                    Kubernetes Workload Pod                     │
│                                                                │
│  ┌──────────────┐  ┌────────────────────────────────────────┐ │
│  │  proxy-init  │  │           envoy-proxy                  │ │
│  │ (init)       │  │                                        │ │
│  │ - Setup      │  │ - Service mesh proxy                   │ │
│  │   iptables   │  │ - Traffic management                   │ │
│  │ - Redirect   │  │ - Authentication (non-SPIFFE methods)  │ │
│  │   traffic    │  │                                        │ │
│  └──────────────┘  └────────────────────────────────────────┘ │
│                                                                │
│  ┌──────────────────────────────────────────────────────────┐ │
│  │              Your Application Container                   │ │
│  └──────────────────────────────────────────────────────────┘ │
└────────────────────────────────────────────────────────────────┘
                               │
                               ▼
                       Application Traffic
                        (via Envoy proxy)
```

For detailed architecture diagrams, see [`ARCHITECTURE.md`](ARCHITECTURE.md).

## Features

### Automatic Container Injection

#### AuthBridge Containers

The AuthBridge webhook injects the following containers into Kubernetes workloads:

**Always injected:**

##### 1. Proxy Init (`proxy-init`) - Init Container

- **Image**: `ghcr.io/kagenti/kagenti-extensions/proxy-init:latest` (configurable)
- **Purpose**: Sets up iptables rules to redirect traffic through the Envoy proxy
- **Resources**: 10m CPU / 64Mi memory (request/limit)
- **Capabilities**: NET_ADMIN, NET_RAW (required for iptables modification)

##### 2. Envoy Proxy (`envoy-proxy`) - Sidecar Container

- **Image**: `ghcr.io/kagenti/kagenti-extensions/envoy-with-processor:latest` (configurable)
- **Purpose**: Service mesh proxy for traffic management and authentication
- **Resources**: 50m CPU / 64Mi memory (request), 200m CPU / 256Mi memory (limit)
- **Ports**: 15123 (envoy-outbound), 9901 (admin), 9090 (ext-proc)

**Injected by default (opt out with `kagenti.io/spiffe-helper-inject: "false"`):**

##### 3. SPIFFE Helper (`spiffe-helper`) - Sidecar Container

- **Image**: `ghcr.io/spiffe/spiffe-helper:nightly`
- **Purpose**: Obtains and refreshes JWT-SVIDs from SPIRE
- **Resources**: 50m CPU / 64Mi memory (request), 100m CPU / 128Mi memory (limit)
- **Volumes**:
  - `/spiffe-workload-api` - SPIRE agent socket
  - `/etc/spiffe-helper` - Configuration
  - `/opt` - SVID token output

##### 4. Client Registration (`kagenti-client-registration`) - Sidecar Container

- **Image**: `ghcr.io/kagenti/kagenti-extensions/client-registration:latest`
- **Purpose**: Registers resource as Keycloak OAuth2 client using SPIFFE identity
- **Resources**: 50m CPU / 64Mi memory (request), 100m CPU / 128Mi memory (limit)
- **Behavior**: Waits for `/opt/jwt_svid.token`, then registers with Keycloak
- **Volumes**:
  - `/opt` - Reads SVID token from spiffe-helper

### Automatic Volume Configuration

The webhook automatically adds these volumes:

- **`spire-agent-socket`** - CSI volume (`csi.spiffe.io` driver) providing the SPIRE Workload API socket for SPIRE agent access (when SPIRE enabled)
- **`spiffe-helper-config`** - ConfigMap containing SPIFFE helper configuration (when SPIRE enabled)
- **`svid-output`** - EmptyDir for SVID token exchange between sidecars (when SPIRE enabled)
- **`envoy-config`** - ConfigMap containing Envoy configuration

## Getting Started

### Prerequisites

- Kubernetes v1.11.3+ cluster
- Go v1.22+ (for development)
- Docker v17.03+ (for building images)
- kubectl v1.11.3+
- cert-manager v1.0+ (for webhook TLS certificates)
- SPIRE agent deployed on cluster nodes
- Keycloak server accessible from the cluster

### Quick Start with Helm

```bash

# Install the webhook using Helm
helm install kagenti-webhook oci://ghcr.io/kagenti/kagenti-extensions/kagenti-webhook-chart \
  --version <version> \
  --namespace kagenti-webhook-system \
  --create-namespace
```

### Local Development with Kind

```bash
cd kagenti-webhook

# Build and deploy to local Kind cluster in one command
make local-dev CLUSTER=<your-kind-cluster-name>

# Or step by step:
make ko-local-build                    # Build with ko
make kind-load-image CLUSTER=<name>    # Load into Kind
make install-local-chart CLUSTER=<name> # Deploy with Helm

# Reinstall after changes
make reinstall-local-chart CLUSTER=<name>
```

### Local Development with webhook-rollout.sh

When the webhook is deployed as a **subchart** (e.g., as part of a parent Helm chart), `helm upgrade` on the subchart alone can fail with an immutable `spec.selector` error because the parent chart may use different label selectors. The `webhook-rollout.sh` script works around this by using `kubectl set image` and `kubectl patch` instead of a full Helm upgrade.

The script handles the full build-and-deploy cycle:

1. Builds the Docker image locally
2. Loads the image into the Kind cluster
3. Deploys the platform defaults ConfigMap (`kagenti-webhook-defaults`)
4. Deploys the feature gates ConfigMap (`kagenti-webhook-feature-gates`)
5. Applies the AuthBridge `MutatingWebhookConfiguration` (before image rollout to avoid race conditions)
6. Updates the deployment image and patches in config volume mounts
7. Waits for rollout to complete

```bash
cd kagenti-webhook

# Basic usage (uses CLUSTER=kagenti by default)
./scripts/webhook-rollout.sh

# Specify cluster and container runtime
CLUSTER=my-cluster ./scripts/webhook-rollout.sh
DOCKER_IMPL=podman ./scripts/webhook-rollout.sh

# Include AuthBridge demo setup (namespace + ConfigMaps)
AUTHBRIDGE_DEMO=true ./scripts/webhook-rollout.sh
AUTHBRIDGE_DEMO=true AUTHBRIDGE_NAMESPACE=myns ./scripts/webhook-rollout.sh
```

Environment variables:

| Variable | Default | Description |
| --- | --- | --- |
| `CLUSTER` | `kagenti` | Kind cluster name |
| `NAMESPACE` | `kagenti-webhook-system` | Webhook deployment namespace |
| `DOCKER_IMPL` | auto-detected | Container runtime (`docker` or `podman`) |
| `AUTHBRIDGE_DEMO` | `false` | Set to `true` to create demo namespace + ConfigMaps |
| `AUTHBRIDGE_NAMESPACE` | `team1` | Namespace for AuthBridge demo workloads |

### Testing the Precedence System

`scripts/test-precedence.sh` is an automated end-to-end test runner for the two-stage sidecar injection precedence system. It deploys workloads into a Kind cluster and validates that each layer of the decision chain behaves correctly.

```bash
cd kagenti-webhook

# Run against the default Kind cluster (cluster name: kagenti, namespace: team1)
./scripts/test-precedence.sh

# Override namespace or cluster
NS=my-ns CLUSTER=my-cluster ./scripts/test-precedence.sh
```

The script accelerates ConfigMap volume propagation by patching the kubelet's `syncFrequency` to `10s` before the tests run, restoring it automatically via an `EXIT` trap when the tests finish.

### Webhook Configuration

The webhook can be configured via Helm values or command-line flags:

```yaml
# values.yaml
webhook:
  enabled: true
  certPath: /tmp/k8s-webhook-server/serving-certs
  certName: tls.crt
  certKey: tls.key
  port: 9443
```

## Development

### Pod-Mutator Architecture

The webhook uses a shared pod mutation engine:

```bash
internal/webhook/
├── config/                          # Configuration layer
│   ├── types.go                     # PlatformConfig, SidecarDefaults types
│   ├── defaults.go                  # Compiled default values
│   ├── loader.go                    # ConfigMap-based config loader with hot-reload
│   ├── feature_gates.go             # FeatureGates type
│   └── feature_gate_loader.go       # Feature gates loader with hot-reload
├── injector/                        # Shared mutation logic
│   ├── pod_mutator.go               # Core mutation engine + InjectAuthBridge
│   ├── precedence.go                # Per-sidecar 2-layer precedence evaluator
│   ├── precedence_test.go           # Table-driven precedence tests
│   ├── injection_decision.go        # SidecarDecision, InjectionDecision types
│   ├── constants.go                 # Label constants
│   ├── container_builder.go         # Build sidecars & init containers
│   └── volume_builder.go            # Build volumes
└── v1alpha1/
    └── authbridge_webhook.go        # AuthBridge webhook handler
```

The `InjectAuthBridge()` method supports:

- Init container injection (proxy-init)
- Sidecar container injection (envoy-proxy, spiffe-helper, client-registration)
- Optional SPIRE integration via pod labels and feature gates
- Pod-level mutation at CREATE time (works with any workload controller)

## Uninstallation

### Using Helm

```bash
helm uninstall kagenti-webhook -n kagenti-webhook-system
```

## License

Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

```text
http://www.apache.org/licenses/LICENSE-2.0
```

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
