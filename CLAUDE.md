# CLAUDE.md - Kagenti Extensions

This file provides context for Claude (AI assistant) when working with the `kagenti-extensions` monorepo.

## AI Assistant Instructions

- **Use `Assisted-By` for attribution** — never add `Co-Authored-By`, `Generated with Claude Code`, or similar trailers. See [Commit Attribution Policy](#commit-attribution-policy) below.

## Repository Overview

**kagenti-extensions** contains Kubernetes security extensions for the [Kagenti](https://github.com/kagenti/kagenti) ecosystem. It provides **zero-trust authentication** for Kubernetes workloads through transparent token exchange and dynamic Keycloak client registration using SPIFFE/SPIRE identities.

The sidecar injection webhook lives in a separate repo: [kagenti/kagenti-operator](https://github.com/kagenti/kagenti-operator).

**GitHub:** `github.com/kagenti/kagenti-extensions`
**Container registry:** `ghcr.io/kagenti/kagenti-extensions/<image-name>`
**License:** Apache 2.0

## Top-Level Directory Structure

```
kagenti-extensions/
├── authbridge/               # Authentication bridge components
│   ├── authlib/              #   Shared auth building blocks (Go module)
│   │   ├── validation/       #     JWKS-backed JWT verifier
│   │   ├── exchange/         #     RFC 8693 token exchange client
│   │   ├── cache/            #     SHA-256 keyed token cache
│   │   ├── bypass/           #     Path pattern matcher
│   │   ├── spiffe/           #     SPIFFE credential sources
│   │   ├── routing/          #     Host-to-audience router
│   │   ├── auth/             #     HandleInbound + HandleOutbound composition
│   │   └── config/           #     Mode presets, YAML config, validation
│   ├── cmd/authbridge-proxy/ #   proxy-sidecar mode (default): HTTP forward + reverse
│   │   │                     #   proxies, full plugin set including parsers
│   │   ├── main.go
│   │   ├── Dockerfile        #     proxy-sidecar combined image
│   │   └── entrypoint.sh
│   ├── cmd/authbridge-envoy/ #   envoy-sidecar mode: ext_proc gRPC server, full plugin set
│   │   ├── main.go
│   │   ├── Dockerfile        #     envoy-sidecar combined image
│   │   └── entrypoint.sh
│   ├── cmd/authbridge-lite/  #   proxy-sidecar mode, lite plugin set (no parsers)
│   │   │                     #   for size-optimized deployments
│   │   ├── main.go
│   │   ├── Dockerfile        #     proxy-sidecar lite combined image
│   │   └── entrypoint.sh
│   ├── proxy-init/           #   iptables init container (envoy-sidecar mode only)
│   │   ├── init-iptables.sh
│   │   ├── Dockerfile.init
│   │   ├── Makefile
│   │   └── README.md
│   ├── demos/                #   Demo scenarios (weather-agent, github-issue, token-exchange-routes, mcp-parser)
│   └── keycloak_sync.py      #   Declarative Keycloak sync tool
├── tests/                    # Python tests (keycloak_sync)
├── .github/
│   ├── workflows/            # CI/CD (ci.yaml, build.yaml, security-scans, scorecard, spellcheck)
│   └── ISSUE_TEMPLATE/       # Bug report, feature request, epic templates
├── .pre-commit-config.yaml   # Pre-commit hooks (trailing whitespace, go fmt/vet, ruff)
└── CLAUDE.md                 # This file
```

## Major Components

### 1. AuthBridge Binaries (Go)

**Three mode-specific binaries** providing transparent traffic interception for both inbound JWT validation and outbound OAuth 2.0 token exchange (RFC 8693). Each binary is hardcoded to its deployment shape; mode is no longer selected at runtime.

**Library:** `authbridge/authlib/` (shared)
**Language:** Go 1.24
**Detailed guide:** [`authbridge/CLAUDE.md`](authbridge/CLAUDE.md)

**Binaries:**
- `cmd/authbridge-proxy/` — proxy-sidecar mode (default): HTTP forward + reverse proxies, full plugin set (jwt-validation, token-exchange, a2a-parser, mcp-parser, inference-parser). No Envoy / no gRPC.
- `cmd/authbridge-envoy/` — envoy-sidecar mode: ext_proc gRPC server hooked into Envoy, full plugin set.
- `cmd/authbridge-lite/` — proxy-sidecar mode, lite plugin set (auth gates only, parsers dropped) for size-optimized deployments.

**Common:**
- `authlib/` — shared auth library (JWT validation, token exchange, caching, routing, all listener implementations, all plugins).
- `proxy-init/init-iptables.sh` — traffic interception setup (Istio ambient mesh compatible). Used by envoy-sidecar mode only.
- `proxy-init/Dockerfile.init` — proxy-init container image.

**Ports (envoy-sidecar):** 15123 (outbound), 15124 (inbound), 9090 (ext-proc), 9901 (admin)
**Ports (proxy-sidecar / lite):** 8080 (reverse proxy), 8081 (forward proxy), 9091 (health), 9093 (stats), 9094 (session API)

### 2. Client Registration

Keycloak client registration for workloads is handled by the
**kagenti-operator** (separate repo) — see `kagenti-operator/docs/operator-managed-client-registration.md`.
The operator creates a Secret with `client-id.txt` + `client-secret.txt`
and the webhook mounts it at `/shared/` in the workload pod. The
in-pod `client-registration` sidecar that previously lived in this
repo has been removed.

## How the Components Work Together

The kagenti-operator (in a separate repo) injects AuthBridge sidecars
into workload pods. Default deployment shape (proxy-sidecar mode):

```
         ┌────────────────────────────────────┐
         │            WORKLOAD POD            │
         │                                    │
         │  spiffe-helper ──► SPIRE Agent     │  (in-container,
         │       │ writes JWT SVID            │   conditional on
         │       ▼                            │   SPIRE_ENABLED)
         │  authbridge-proxy                  │
         │    - Reverse proxy: inbound JWT    │
         │    - Forward proxy: outbound       │
         │      token exchange                │
         │       │                            │
         │  Your Application                  │
         │    (HTTP_PROXY → forward proxy)    │
         └────────────────────────────────────┘

         The operator also creates a Secret with client-id +
         client-secret and mounts it at /shared/.

         For envoy-sidecar mode, replace authbridge-proxy with
         the authbridge-envoy image (Envoy + ext_proc + spiffe-helper)
         and add a proxy-init container for iptables.
```

## AuthBridge Binaries

Three mode-specific binaries, one Dockerfile per binary:

| Binary | Mode | Listeners | Plugins |
|--------|------|-----------|---------|
| `cmd/authbridge-proxy/` | proxy-sidecar (default) | HTTP forward + reverse proxies | full (incl. parsers) |
| `cmd/authbridge-envoy/` | envoy-sidecar | gRPC ext_proc on :9090 | full (incl. parsers) |
| `cmd/authbridge-lite/` | proxy-sidecar | HTTP forward + reverse proxies | auth-only (jwt-validation + token-exchange, no parsers) |

**Go modules:**
- `authbridge/authlib/` — pure library: validation, exchange, cache, bypass, spiffe, routing, auth, config, all listener implementations, all plugins.
- `authbridge/cmd/authbridge-{proxy,envoy,lite}/` — thin main packages that import authlib and start the listeners they need.
- `authbridge/go.work` — workspace linking authlib + the binaries for local development.

**Config format:** YAML with `${ENV_VAR}` expansion, mode presets, and startup validation. Supports `keycloak_url` + `keycloak_realm` derivation for operator compatibility. The `mode` field in YAML must match the binary (each binary rejects mismatched modes at boot).

## CI/CD Workflows

| Workflow | Trigger | Purpose |
|----------|---------|---------|
| `ci.yaml` | PR to main/release-* | Pre-commit, Go fmt/vet/build/test for authlib and the cmd/authbridge-* binaries; Python tests |
| `build.yaml` | Tag push (`v*`) or manual | Multi-arch Docker builds for: proxy-init, authbridge (proxy-sidecar combined), authbridge-envoy (envoy-sidecar combined), authbridge-lite (proxy-sidecar lite combined) |
| `security-scans.yaml` | PR to main | Dependency review, shellcheck, YAML lint, Hadolint, Bandit, Trivy, CodeQL |
| `scorecard.yaml` | Weekly / push to main | OpenSSF Scorecard security health metrics |
| `spellcheck_action.yml` | PR | Spellcheck on markdown files |

### PR Title Convention

PRs must follow **conventional commits** format:

```
<type>: <Subject starting with uppercase>
```

Types: `feat`, `fix`, `docs`, `style`, `refactor`, `perf`, `test`, `build`, `ci`, `chore`, `revert`

## Container Images

All images are pushed to `ghcr.io/kagenti/kagenti-extensions/` from
`.github/workflows/build.yaml`:

| Image | Source | Description |
|-------|--------|-------------|
| **`authbridge`** | **`authbridge/cmd/authbridge-proxy/Dockerfile`** | **proxy-sidecar combined image (default mode): authbridge-proxy (full plugin set incl. parsers) + spiffe-helper. No Envoy.** |
| `authbridge-envoy` | `authbridge/cmd/authbridge-envoy/Dockerfile` | envoy-sidecar combined image: Envoy + authbridge-envoy (ext_proc, full plugin set) + spiffe-helper |
| `authbridge-lite` | `authbridge/cmd/authbridge-lite/Dockerfile` | proxy-sidecar lite combined image: authbridge-lite (auth gates only, parsers dropped) + spiffe-helper. Same listener layout as `authbridge`; not yet referenced by the operator's default config |
| `proxy-init` | `authbridge/proxy-init/Dockerfile.init` | Alpine + iptables init container (envoy-sidecar mode only) |

In all three combined images, `spiffe-helper` is started conditionally
based on the `SPIRE_ENABLED` env var (set by the operator when SPIRE
identity is enabled for the workload).

The legacy `authbridge-unified`, `authbridge-light`, `client-registration`,
`spiffe-helper`, `auth-proxy`, and `demo-app` standalone images have
been removed from CI (the auth-proxy / demo-app source is still in-tree
for the standalone quickstart). Older release tags continue to publish
the old images.

## Pre-commit Hooks

Install: `pre-commit install`

Hooks:
- `trailing-whitespace`, `end-of-file-fixer`, `check-added-large-files` (max 1024KB), `check-yaml`, `check-json`, `check-merge-conflict`, `mixed-line-ending`
- `ai-assisted-by-trailer` — Rewrites `Co-Authored-By` to `Assisted-By` (commit-msg stage)
- `ruff`, `ruff-format` — Python linting/formatting on `authbridge/` files
- `go-fmt`, `go-vet` — Runs on `authbridge/proxy-init/` Go files

## Languages and Tech Stack

| Area | Technology |
|------|------------|
| AuthBridge unified binary | Go 1.24, envoy-control-plane, lestrrat-go/jwx |
| Client Registration | Python 3.12, python-keycloak, PyJWT |
| Proxy | Envoy 1.28 |
| Traffic interception | iptables (via init container) |
| Identity | SPIFFE/SPIRE (JWT-SVIDs) |
| Auth provider | Keycloak (OAuth2/OIDC, token exchange RFC 8693) |
| Packaging | Docker |
| CI | GitHub Actions |

## External Dependencies and Services

| Service | Required | Purpose |
|---------|----------|---------|
| Kubernetes | Yes | Target platform (v1.25+ recommended) |
| [kagenti-operator](https://github.com/kagenti/kagenti-operator) | Yes | Injects AuthBridge sidecars into workload pods |
| Keycloak | Yes | OAuth2/OIDC provider, token exchange |
| SPIRE | Optional | SPIFFE identity (JWT-SVIDs) for workloads |

## ConfigMaps and Secrets Expected at Runtime

When the operator injects sidecars, the target namespace needs these resources:

| Resource | Kind | Used by | Keys |
|----------|------|---------|------|
| `authbridge-config` | ConfigMap | client-registration, authbridge | `KEYCLOAK_URL`, `KEYCLOAK_REALM`, `PLATFORM_CLIENT_IDS` (optional), `TOKEN_URL` (optional, derived from KEYCLOAK_URL+KEYCLOAK_REALM), `ISSUER` (optional, derived or explicit for split-horizon DNS), `DEFAULT_OUTBOUND_POLICY` (optional, defaults to `passthrough`). Inbound audience validation uses `CLIENT_ID` from `/shared/client-id.txt`. Target audience and scopes are configured per-route in `authproxy-routes`. |
| `keycloak-admin-secret` | Secret | client-registration | `KEYCLOAK_ADMIN_USERNAME`, `KEYCLOAK_ADMIN_PASSWORD` |
| `authproxy-routes` | ConfigMap (optional) | authbridge | `routes.yaml` -- per-host token exchange rules (see authbridge/CLAUDE.md for format) |
| `spiffe-helper-config` | ConfigMap | spiffe-helper | SPIFFE helper configuration file |
| `envoy-config` | ConfigMap | envoy-proxy | Envoy YAML configuration |

**Note:** `authproxy-routes` is optional. Without it, all outbound traffic passes through unchanged (the default policy is `passthrough`). Only create it when the agent needs to call services that require token exchange. Set `DEFAULT_OUTBOUND_POLICY: "exchange"` in `authbridge-config` to restore the legacy behavior.

## Common Development Tasks

### Building Everything Locally

The repo-root `local-build-and-test.sh` orchestrates every image
the platform needs (`spiffe-idp-setup` from kagenti, plus
`authbridge`, `authbridge-envoy`, `authbridge-lite`, `proxy-init`
from this repo) and loads them into a Kind cluster:

```bash
KAGENTI_DIR=../kagenti ./local-build-and-test.sh
```

To build a single image directly:

```bash
# proxy-init (iptables init container, envoy-sidecar mode)
cd authbridge/proxy-init && make docker-build-init

# Combined sidecars (proxy-sidecar default / envoy-sidecar / lite)
cd authbridge && podman build -f cmd/authbridge-proxy/Dockerfile -t authbridge:latest .
cd authbridge && podman build -f cmd/authbridge-envoy/Dockerfile -t authbridge-envoy:latest .
cd authbridge && podman build -f cmd/authbridge-lite/Dockerfile  -t authbridge-lite:latest  .
```

### Running the Full Demo

1. Set up a Kind cluster with SPIRE + Keycloak (use [Kagenti Ansible installer](https://github.com/kagenti/kagenti/blob/main/docs/install.md))
2. Deploy the webhook via [kagenti-operator](https://github.com/kagenti/kagenti-operator)
3. See the [AuthBridge demos index](authbridge/demos/README.md) for a recommended learning path:
   - **Getting started**: `authbridge/demos/weather-agent/demo-ui.md` (inbound validation, UI deployment)
   - **Full flow**: `authbridge/demos/github-issue/demo-ui.md` (token exchange + scope-based access)
   - **Routes config reference**: `authbridge/demos/token-exchange-routes/README.md` (single + multi-target route patterns)

### Adding a New Component Image to CI

1. Add entry to `.github/workflows/build.yaml` matrix (`image_config` array)
2. Provide `name`, `context`, and `dockerfile` fields
3. Image will be pushed to `ghcr.io/kagenti/kagenti-extensions/<name>`

## Code Style and Conventions

### Go Code (AuthProxy)
- Use `go fmt` (enforced by pre-commit and CI)
- Use `go vet` (enforced by pre-commit and CI)

### Python Code (client-registration)
- Python 3.12+ syntax (type hints with `str | None`)
- Dependencies in `requirements.txt` (version-pinned, e.g. `python-keycloak==5.3.1`)

### Kubernetes Manifests
- Example deployment YAMLs in `authbridge/demos/*/k8s/`

### Shell Scripts
- `set -euo pipefail` (strict mode)
- Extensive inline documentation (especially `init-iptables.sh`)

## Important Cross-Component Relationships

1. **UID/GID Sync:** The `client-registration` Dockerfile creates a user with UID/GID 1000. The operator's webhook sets `runAsUser: 1000` / `runAsGroup: 1000` when injecting the client-registration container. These MUST match. In combined mode (`authbridge` container), everything runs as UID 1337 instead.

2. **Envoy Proxy UID:** Envoy runs as UID 1337. The `proxy-init` iptables rules exclude this UID from redirection to prevent loops. The combined `authbridge` container also runs as UID 1337.

3. **Shared Volume Contract:** The sidecars communicate through shared volumes:
   - `/opt/jwt_svid.token` — spiffe-helper writes, client-registration reads
   - `/shared/client-id.txt` — client-registration writes, envoy-proxy reads
   - `/shared/client-secret.txt` — client-registration writes, envoy-proxy reads

4. **Port Coordination:** Envoy listens on 15123 (outbound) and 15124 (inbound). The ext-proc listens on 9090. The `proxy-init` iptables rules redirect to these ports.

## Gotchas and Known Issues

1. **One Go module:** The repo has a single Go module at `authbridge/proxy-init/go.mod` (Go 1.24).

2. **Avoid committing venvs:** Virtual environment directories (e.g. `authbridge/proxy-init/quickstart/venv/`) should be gitignored (the repo's `.gitignore` has a `venv` pattern). Do not create and commit new virtual environments under version control.

3. **Envoy config not embedded:** The envoy-proxy sidecar mounts `envoy-config` ConfigMap at `/etc/envoy`. This ConfigMap must exist in the target namespace before workloads are created.

4. **Outbound policy is passthrough by default:** AuthBridge defaults to passing outbound traffic through unchanged. Token exchange only happens for hosts explicitly listed in the `authproxy-routes` ConfigMap. Target audience and scopes are configured per-route in `authproxy-routes`.

5. **Route host patterns use short service names:** The `host` field in `authproxy-routes` matches against the HTTP `Host` header, which is typically just the short Kubernetes service name (e.g., `github-tool-mcp`), not the FQDN. Glob patterns (`*`) are supported but the most common case is a plain service name.

## DCO Sign-Off (Mandatory)

All commits **must** include a `Signed-off-by` trailer (Developer Certificate of Origin).
Always use the `-s` flag when committing:

```sh
git commit -s -m "feat: Add new feature"
```

This adds a line like `Signed-off-by: Your Name <your@email.com>` to the commit message.
PRs without DCO sign-off will fail CI checks. To retroactively sign-off existing commits:

```sh
git rebase --signoff main
```

## Orchestration

This repo includes orchestrate skills for enhancing related repositories.
Run `/orchestrate <repo-url>` to start.

| Skill | Description |
|-------|-------------|
| `orchestrate` | Entry point — scan, plan, execute phases |
| `orchestrate:scan` | Assess repo structure, CI, security gaps |
| `orchestrate:plan` | Create phased enhancement plan |
| `orchestrate:precommit` | Add pre-commit hooks and linting |
| `orchestrate:tests` | Add test infrastructure |
| `orchestrate:ci` | Add CI/CD workflows |
| `orchestrate:security` | Add security governance files |
| `orchestrate:replicate` | Bootstrap skills into target repo |
| `orchestrate:review` | Review all orchestration PRs before merge |

Skills management:

| Skill | Description |
|-------|-------------|
| `skills` | Skills router — create, validate, scan |
| `skills:write` | Create or edit skills with proper structure |
| `skills:validate` | Validate skill format and naming |
| `skills:scan` | Audit repo for skill gaps |

## Commit Attribution Policy

When creating git commits, do NOT use `Co-Authored-By` trailers for AI attribution.
Instead, use `Assisted-By` to acknowledge AI assistance without inflating contributor stats:

    Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>

Never add `Co-authored-by`, `Made-with`, or similar trailers that GitHub parses as co-authorship.

### PR Bodies

PR descriptions should end with the same `Assisted-By` trailer:

    Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>

Do not use `🤖 Generated with [Claude Code](https://claude.com/claude-code)` or similar.

A `commit-msg` hook in `scripts/hooks/commit-msg` enforces this automatically for commits.
Install it via pre-commit:

```sh
pre-commit install --hook-type pre-commit --hook-type commit-msg
```
