# CLAUDE.md - Kagenti Extensions

This file provides context for Claude (AI assistant) when working with the `kagenti-extensions` monorepo.

## Repository Overview

**kagenti-extensions** is a monorepo containing Kubernetes security extensions for the [Kagenti](https://github.com/kagenti/kagenti) ecosystem. It provides **zero-trust authentication** for Kubernetes workloads through automatic sidecar injection, transparent token exchange, and dynamic Keycloak client registration using SPIFFE/SPIRE identities.

**GitHub:** `github.com/kagenti/kagenti-extensions`
**Container registry:** `ghcr.io/kagenti/kagenti-extensions/<image-name>`
**License:** Apache 2.0

## Top-Level Directory Structure

```
kagenti-extensions/
├── kagenti-webhook/          # Kubernetes admission webhook (Go, Kubebuilder)
├── AuthBridge/               # Authentication bridge components
│   ├── AuthProxy/            #   Envoy + ext-proc sidecar (Go) — token validation & exchange
│   │   ├── go-processor/     #     gRPC ext-proc server (inbound JWT validation, outbound token exchange)
│   │   └── quickstart/       #     Standalone demo (no SPIFFE)
│   ├── client-registration/  #   Keycloak auto-registration (Python)
│   ├── demos/                #   Demo scenarios (webhook, single-target, multi-target, github-issue)
│   └── keycloak_sync.py      #   Declarative Keycloak sync tool
├── charts/
│   └── kagenti-webhook/      # Helm chart for the webhook
├── .github/
│   ├── workflows/            # CI/CD (ci.yaml, build.yaml, goreleaser.yml, pr-verifier.yaml, spellcheck)
│   └── ISSUE_TEMPLATE/       # Bug report, feature request, epic templates
├── .goreleaser.yaml          # GoReleaser config (webhook binary + ko image + Helm chart)
├── .pre-commit-config.yaml   # Pre-commit hooks (trailing whitespace, go fmt/vet, helmlint)
└── CLAUDE.md                 # This file
```

## The Three Major Components

### 1. kagenti-webhook (Go / Kubebuilder)

A Kubernetes **mutating admission webhook** that intercepts workload creation (Deployments, StatefulSets, DaemonSets, Jobs, CronJobs) and automatically injects AuthBridge sidecar containers.

**Location:** `kagenti-webhook/`
**Language:** Go 1.24, controller-runtime v0.22, Kubebuilder v4
**Detailed guide:** [`kagenti-webhook/CLAUDE.md`](kagenti-webhook/CLAUDE.md)

**Key facts:**
- Webhook: **AuthBridge** at `/mutate-workloads-authbridge`
- Injection controlled via pod labels (`kagenti.io/type`, `kagenti.io/inject`, `kagenti.io/spire`) and namespace labels (`kagenti-enabled: "true"`)
- Shared `PodMutator` instance (in `internal/webhook/injector/`)
- Injects: `proxy-init` (init), `envoy-proxy`, `spiffe-helper` (gated by `kagenti.io/spire` label), `kagenti-client-registration` (gated by `--enable-client-registration` flag)
- Build: `cd kagenti-webhook && make build` / `make test` / `make docker-build`
- Local dev: `cd kagenti-webhook && make local-dev CLUSTER=<kind-cluster>`

### 2. AuthProxy (Go)

An **Envoy proxy with a gRPC external processor** that provides transparent traffic interception for both inbound JWT validation and outbound OAuth 2.0 token exchange (RFC 8693).

**Location:** `AuthBridge/AuthProxy/`
**Language:** Go 1.23
**Detailed guide:** [`AuthBridge/CLAUDE.md`](AuthBridge/CLAUDE.md)

**Core components:**
- `go-processor/main.go` — gRPC ext-proc server (inbound JWT validation, outbound token exchange)
- `init-iptables.sh` — Traffic interception setup (Istio ambient mesh compatible)
- `Dockerfile.{envoy,init}` — Container images

**Ports:** 15123 (outbound), 15124 (inbound), 9090 (ext-proc), 9901 (admin)

### 3. Client Registration (Python)

A Python script that **automatically registers Kubernetes workloads as Keycloak OAuth2 clients** using their SPIFFE identity.

**Location:** `AuthBridge/client-registration/`
**Language:** Python 3.12
**Detailed guide:** [`AuthBridge/CLAUDE.md`](AuthBridge/CLAUDE.md)

**Flow:** Reads SPIFFE ID from JWT, registers client in Keycloak, writes secret to `/shared/client-secret.txt`

## How the Components Work Together

```
                    Workload Creation
                          │
                          ▼
               ┌─────────────────────┐
               │  kagenti-webhook    │  Intercepts CREATE/UPDATE
               │  (admission webhook)│  via MutatingWebhookConfiguration
               └──────────┬──────────┘
                          │ Injects sidecars
                          ▼
         ┌────────────────────────────────────┐
         │            WORKLOAD POD            │
         │                                    │
         │  proxy-init (init) ─► iptables     │
         │                                    │
         │  spiffe-helper ──► SPIRE Agent     │
         │       │ writes JWT SVID            │
         │       ▼                            │
         │  client-registration ──► Keycloak  │
         │       │ writes client secret       │
         │       ▼                            │
         │  envoy-proxy (+ go-processor)      │
         │    - Inbound: JWT validation       │
         │    - Outbound: token exchange       │
         │       │                            │
         │  Your Application                  │
         └────────────────────────────────────┘
```

## CI/CD Workflows

| Workflow | Trigger | Purpose |
|----------|---------|---------|
| `ci.yaml` | PR to main/release-* | Go fmt, vet, build across all Go modules |
| `build.yaml` | Tag push (`v*`) or manual | Multi-arch Docker builds for: client-registration, auth-proxy, proxy-init, envoy-with-processor, demo-app |
| `goreleaser.yml` | Tag push (`v*`) | GoReleaser binary + ko image for webhook, Helm chart package + push |
| `pr-verifier.yaml` | PR to main/release-* | Semantic PR title validation (conventional commits) |
| `spellcheck_action.yml` | PR | Spellcheck on markdown files |

### PR Title Convention

PRs must follow **conventional commits** format:

```
<type>: <Subject starting with uppercase>
```

Types: `feat`, `fix`, `docs`, `style`, `refactor`, `perf`, `test`, `build`, `ci`, `chore`, `revert`

## Container Images

All images are pushed to `ghcr.io/kagenti/kagenti-extensions/`:

| Image | Source | Description |
|-------|--------|-------------|
| `kagenti-webhook` | `kagenti-webhook/Dockerfile` | Admission webhook manager (Go binary in distroless) |
| `envoy-with-processor` | `AuthBridge/AuthProxy/Dockerfile.envoy` | Envoy 1.28 + go-processor ext-proc |
| `proxy-init` | `AuthBridge/AuthProxy/Dockerfile.init` | Alpine + iptables init container |
| `client-registration` | `AuthBridge/client-registration/Dockerfile` | Python Keycloak client registrar |
| `auth-proxy` | `AuthBridge/AuthProxy/Dockerfile` | Example pass-through proxy (for demos) |
| `demo-app` | `AuthBridge/AuthProxy/quickstart/demo-app/Dockerfile` | Demo target service |

## Helm Chart

**Location:** `charts/kagenti-webhook/`
**Published to:** `oci://ghcr.io/kagenti/kagenti-extensions/kagenti-webhook-chart`

Key values:
- `image.repository` / `image.tag` — Webhook manager image (tag is `__PLACEHOLDER__` in source, replaced at release time)
- `webhook.enableClientRegistration` — Controls `--enable-client-registration` flag
- `certManager.enabled` — Uses cert-manager for webhook TLS certificates
- Templates include: Deployment, Service, ServiceAccount, RBAC, CertManager Certificate/Issuer, MutatingWebhookConfigurations (authbridge, agent)

Install:
```bash
helm install kagenti-webhook oci://ghcr.io/kagenti/kagenti-extensions/kagenti-webhook-chart \
  --version <version> \
  --namespace kagenti-webhook-system \
  --create-namespace
```

## Pre-commit Hooks

Install: `pre-commit install`

Hooks:
- `trailing-whitespace`, `end-of-file-fixer`, `check-added-large-files` (max 1024KB), `check-yaml`, `check-json`, `check-merge-conflict`, `mixed-line-ending`
- `helmlint` — Runs on `charts/` directory
- `go-fmt`, `go-vet`, `go-mod-tidy` — Runs on `kagenti-webhook/` Go files

## Languages and Tech Stack

| Area | Technology |
|------|------------|
| Webhook | Go 1.24, controller-runtime v0.22, Kubebuilder v4 |
| AuthProxy ext-proc | Go 1.23, envoy-control-plane, lestrrat-go/jwx |
| Client Registration | Python 3.12, python-keycloak, PyJWT |
| Proxy | Envoy 1.28 |
| Traffic interception | iptables (via init container) |
| Identity | SPIFFE/SPIRE (JWT-SVIDs) |
| Auth provider | Keycloak (OAuth2/OIDC, token exchange RFC 8693) |
| Packaging | Docker, ko, GoReleaser, Helm 3 |
| Testing | Ginkgo/Gomega (Go), envtest (controller-runtime) |
| CI | GitHub Actions |

## External Dependencies and Services

| Service | Required | Purpose |
|---------|----------|---------|
| Kubernetes | Yes | Target platform (v1.25+ recommended) |
| cert-manager | Yes (for webhook) | TLS certificates for webhook server |
| Keycloak | Yes (for AuthBridge) | OAuth2/OIDC provider, token exchange |
| SPIRE | Optional | SPIFFE identity (JWT-SVIDs) for workloads |
| Prometheus | Optional | Metrics collection (ServiceMonitor) |

## ConfigMaps Expected at Runtime

When the webhook injects sidecars, the target namespace needs these ConfigMaps:

| Resource | Kind | Used by | Keys |
|----------|------|---------|------|
| `environments` | ConfigMap | client-registration | `KEYCLOAK_URL`, `KEYCLOAK_REALM` |
| `keycloak-admin-secret` | Secret | client-registration | `KEYCLOAK_ADMIN_USERNAME`, `KEYCLOAK_ADMIN_PASSWORD` |
| `authbridge-config` | ConfigMap | envoy-proxy (ext-proc) | `TOKEN_URL`, `ISSUER`, `TARGET_AUDIENCE`, `TARGET_SCOPES` |
| `spiffe-helper-config` | ConfigMap | spiffe-helper | SPIFFE helper configuration file |
| `envoy-config` | ConfigMap | envoy-proxy | Envoy YAML configuration |

## Common Development Tasks

### Building Everything Locally

```bash
# Webhook
cd kagenti-webhook && make build && make test

# AuthProxy images
cd AuthBridge/AuthProxy && make build-images

# Client registration (no separate build needed, uses Dockerfile directly)
```

### Running the Full Demo

1. Set up a Kind cluster with SPIRE + Keycloak (use [Kagenti Ansible installer](https://github.com/kagenti/kagenti/blob/main/docs/install.md))
2. Deploy the webhook: `cd kagenti-webhook && make local-dev CLUSTER=<name>`
3. Follow `AuthBridge/demos/webhook/README.md` for the webhook-based AuthBridge demo (recommended), or `AuthBridge/demos/single-target/demo.md` for manual deployment

### Quick Webhook Iteration

```bash
cd kagenti-webhook
./scripts/webhook-rollout.sh           # Build + deploy to Kind
# or with AuthBridge demo setup:
AUTHBRIDGE_DEMO=true ./scripts/webhook-rollout.sh
```

### Adding a New Component Image to CI

1. Add entry to `.github/workflows/build.yaml` matrix (`image_config` array)
2. Provide `name`, `context`, and `dockerfile` fields
3. Image will be pushed to `ghcr.io/kagenti/kagenti-extensions/<name>`

## Code Style and Conventions

### Go Code (webhook, AuthProxy)
- Use `go fmt` (enforced by pre-commit and CI)
- Use `go vet` (enforced by pre-commit and CI)
- Kubebuilder markers (`+kubebuilder:webhook:...`) generate webhook manifests -- run `make manifests` after changes
- Logger names: lowercase-hyphenated (e.g., `logf.Log.WithName("pod-mutator")`)
- Apache 2.0 license header in all Go files (template at `kagenti-webhook/hack/boilerplate.go.txt`)

### Python Code (client-registration)
- Python 3.12+ syntax (type hints with `str | None`)
- Dependencies in `requirements.txt` (version-pinned, e.g. `python-keycloak==5.3.1`)
- UID/GID 1000 in Dockerfile must match `ClientRegistrationUID`/`ClientRegistrationGID` in webhook's `container_builder.go`

### Kubernetes Manifests
- Example deployment YAMLs in `AuthBridge/demos/*/k8s/`
- Helm templates in `charts/kagenti-webhook/templates/`
- Helm templates excluded from YAML check in pre-commit (they contain Go template syntax)

### Shell Scripts
- `set -euo pipefail` (strict mode)
- Extensive inline documentation (especially `init-iptables.sh`)

## Important Cross-Component Relationships

1. **UID/GID Sync:** The `client-registration` Dockerfile creates a user with UID/GID 1000. The webhook's `container_builder.go` sets `runAsUser: 1000` / `runAsGroup: 1000`. These MUST match.

2. **Envoy Proxy UID:** Envoy runs as UID 1337. The `proxy-init` iptables rules exclude this UID from redirection to prevent loops. Both `container_builder.go` and `init-iptables.sh` use this value.

3. **Shared Volume Contract:** The sidecars communicate through shared volumes:
   - `/opt/jwt_svid.token` — spiffe-helper writes, client-registration reads
   - `/shared/client-id.txt` — client-registration writes, envoy-proxy reads
   - `/shared/client-secret.txt` — client-registration writes, envoy-proxy reads

4. **Port Coordination:** Envoy listens on 15123 (outbound) and 15124 (inbound). The ext-proc listens on 9090. The `proxy-init` iptables rules redirect to these ports. The webhook's `container_builder.go` exposes these ports on the container spec.

5. **Image References:** Default image tags are hardcoded in `kagenti-webhook/internal/webhook/injector/container_builder.go` and paralleled in `kagenti-webhook/internal/webhook/config/defaults.go`. The CI in `build.yaml` builds the images. All three must stay in sync.

## Gotchas and Known Issues

1. **Config system not wired in:** `kagenti-webhook/internal/webhook/config/` (PlatformConfig, FeatureGates, loaders) exists but is NOT used by the injector. Container builder uses hardcoded constants. This is a known gap.

2. **Two Go modules:** The repo has two independent Go modules (`kagenti-webhook/go.mod` and `AuthBridge/AuthProxy/go.mod`) with different Go versions (1.24 vs 1.23). They do not share code.

3. **Helm chart tag placeholder:** `charts/kagenti-webhook/values.yaml` uses `tag: "__PLACEHOLDER__"`. The goreleaser workflow replaces this at release time. For local dev, override with `--set image.tag=<tag>`.

4. **Avoid committing venvs:** Virtual environment directories (e.g. `AuthBridge/AuthProxy/quickstart/venv/`) should be gitignored (the repo's `.gitignore` has a `venv` pattern). Do not create and commit new virtual environments under version control.

5. **CI Go version alignment:** Ensure the Go version in `ci.yaml` matches the highest Go version required across all modules (currently Go 1.24, matching `kagenti-webhook/go.mod`).

6. **Envoy config not embedded:** The envoy-proxy sidecar mounts `envoy-config` ConfigMap at `/etc/envoy`. This ConfigMap must exist in the target namespace before workloads are created.
