# CLAUDE.md - AuthBridge

This file provides context for Claude (AI assistant) when working with the `AuthBridge` codebase.
For repo-level context (CI/CD, cross-component relationships), see [`../CLAUDE.md`](../CLAUDE.md).
The sidecar injection webhook lives in [kagenti-operator](https://github.com/kagenti/kagenti-operator).

## Binaries

The unified `cmd/authbridge/` binary has been split into three mode-specific
binaries with shared auth logic in `authlib/`:

- `cmd/authbridge-proxy/` — proxy-sidecar mode (default). HTTP forward + reverse
  proxies. Compiles in every plugin by default (jwt-validation, token-exchange,
  a2a-parser, mcp-parser, inference-parser, opa, sparc, ibac, token-broker).
  **Every** plugin is excludable via `-tags exclude_plugin_<name>` — one
  `plugins_<name>.go` file per plugin, gated by `//go:build !exclude_plugin_<name>`;
  `main.go` imports no plugin package directly. **Exception:** `context-guru` is
  **opt-IN** (`//go:build include_plugin_contextguru`, not compiled by default),
  because its embedded engine pulls a large transitive dependency set; build with
  `-tags include_plugin_contextguru` to link it in.
- `cmd/authbridge-envoy/` — envoy-sidecar mode. ext_proc gRPC server hooked
  into Envoy. Full plugin set. The `exclude_plugin_ibac` tag applies here too.
- `cmd/authbridge-cpex/` — proxy-sidecar mode, full plugin set plus the
  `cpex` plugin. Built with `-tags cpex` and requires cgo (CGO_ENABLED=1):
  it links `libcpex_ffi.a` from a pinned CPEX release to route hooks through
  the CPEX framework (APL DSL + named CPEX policy plugins). The FFI ABI
  version lives in `cmd/authbridge-cpex/CPEX_FFI_VERSION`. The other
  binaries are pure-Go (CGO_ENABLED=0) and do not import the cpex package.
- `authbridge-lite` (**image, not a separate binary**) — `cmd/authbridge-proxy`
  built with `exclude_plugin_*` tags so only jwt-validation + token-exchange
  compile in (OPA + parsers dropped). For size-optimized deployments that
  don't need protocol-aware session events.

Each binary is hardcoded to its deployment shape; mode is no longer selected
at runtime. The YAML `mode:` field must match the binary or boot fails.

See [`authlib/README.md`](authlib/README.md) for the library reference.

## What AuthBridge Does

AuthBridge provides **zero-trust, transparent token management** for Kubernetes workloads. It combines three capabilities:

1. **Automatic Identity** -- Workloads obtain SPIFFE IDs from SPIRE and auto-register as Keycloak clients
2. **Inbound JWT Validation** -- Incoming requests are validated (signature, issuer, audience) by the authbridge binary
3. **Outbound Token Exchange** -- Outgoing requests get their tokens automatically exchanged for the correct target audience (OAuth 2.0 RFC 8693)

All of this happens transparently via sidecar injection -- no application code changes required.

## Directory Structure

```
authbridge/
├── authlib/                          # Shared auth library (Go module)
│   ├── validation/                   #   JWKS-backed JWT verifier
│   ├── exchange/                     #   RFC 8693 token exchange client
│   ├── cache/                        #   SHA-256 keyed token cache
│   ├── bypass/                       #   Path pattern matcher
│   ├── spiffe/                       #   SPIFFE credential sources
│   ├── routing/                      #   Host-to-audience router
│   ├── auth/                         #   HandleInbound + HandleOutbound composition
│   └── config/                       #   Mode presets, YAML config, validation
│
├── cmd/authbridge-proxy/             # proxy-sidecar mode (default). Full plugin set.
│   ├── main.go
│   ├── Dockerfile                    #   proxy-sidecar combined image (authbridge-proxy)
│   └── entrypoint.sh
│
├── cmd/authbridge-envoy/             # envoy-sidecar mode. Full plugin set.
│   ├── main.go
│   ├── Dockerfile                    #   envoy-sidecar combined image (Envoy + authbridge-envoy)
│   └── entrypoint.sh
│
├── cmd/authbridge-cpex/              # proxy-sidecar mode + cpex plugin. -tags cpex, cgo required.
│   ├── main.go
│   ├── Dockerfile                    #   proxy-sidecar build linking libcpex_ffi.a
│   ├── CPEX_FFI_VERSION              #   pinned CPEX FFI ABI version (build-arg source of truth)
│   └── entrypoint.sh
│
├── proxy-init/                       # iptables init container (envoy-sidecar + proxy-sidecar enforce-redirect modes)
│   ├── init-iptables.sh              #   iptables setup script
│   ├── Dockerfile.init               #   proxy-init container image
│   ├── Makefile                      #   docker-build-init + load-image targets
│   └── README.md
│
├── demos/                            # Demo scenarios with full setup
│   ├── README.md                     #   Demo index (recommended starting order)
│   ├── weather-agent/                #   Getting-started demo (inbound validation only)
│   │   ├── demo-ui.md
│   │   ├── demo-ui-advanced.md       #   With token exchange + tool-side AuthBridge
│   │   └── demo-with-abctl.md        #   Plugin-pipeline TUI walkthrough
│   ├── token-exchange-routes/        #   Routes config reference (single + multi-target)
│   │   ├── README.md
│   │   └── routes.yaml
│   ├── github-issue/                 #   GitHub integration demo
│   │   ├── demo.md, demo-ui.md, demo-manual.md
│   │   ├── setup_keycloak.py
│   │   └── k8s/
│   └── webhook/                      #   Webhook-based injection demo
│       ├── README.md                 #     Webhook injection walkthrough
│       ├── setup_keycloak.py
│       └── k8s/                      #     Manifests including configmaps-webhook.yaml
│
└── keycloak_sync.py                  # Declarative Keycloak sync tool (routes.yaml driven)
```

## Component Details

### AuthBridge Binaries (cmd/authbridge-{proxy,envoy}/)

The mode-specific authbridge binaries handle both traffic directions. Auth logic
and all listener implementations live in `authlib/` (under `authlib/listener/`);
each binary's `main.go` just imports the listeners it needs and the plugins it
wants to register.

**Inbound path** (`x-authbridge-direction: inbound`):
- Validates JWT signature via JWKS (auto-refreshing cache from `TOKEN_URL`-derived JWKS endpoint)
- Validates issuer claim against `ISSUER` env var
- Validates audience against `CLIENT_ID` (from `/shared/client-id.txt` or env var)
- Returns 401 with JSON error body for invalid/missing tokens
- Removes `x-authbridge-direction` header before forwarding to app

**Outbound path** (no direction header):
- Default policy is **passthrough** -- outbound requests pass through unchanged unless a route matches
- Uses a **route resolver** to match the request's `Host` header against patterns in `authproxy-routes` ConfigMap
- If a route matches: reads `target_audience` and `token_scopes` from the route, obtains a token via `client_credentials` grant, and injects it as `Authorization: Bearer <token>`
- If no route matches: applies the default outbound policy (`passthrough` or `exchange`)
- Returns 503 if exchange fails for a routed host (prevents unauthenticated calls)
- The `DEFAULT_OUTBOUND_POLICY` env var controls the fallback behavior (default: `passthrough`)

**Route resolver (outbound):**
- Reads `/etc/authproxy/routes.yaml` (default path; override with `ROUTES_CONFIG_PATH` env var in standalone deployments)
- Each route entry has: `host` (glob pattern), `target_audience`, `token_scopes`
- Host matching uses `filepath.Match` semantics (supports `*`, `?`, `[...]` patterns)
- Most commonly, `host` is a plain Kubernetes service name (e.g., `github-tool-mcp`) because the HTTP client sets the Host header from the URL hostname
- Routes file is loaded once at startup; restart the pod to pick up changes

**Configuration loading:**
- YAML config with `${ENV_VAR}` expansion, mode presets, and startup validation.
- Plugin settings are local to each plugin under `pipeline.*.plugins[].config`; the runtime YAML itself only carries `mode`, `listener`, `session`, `stats`, and the pipeline composition. See [`docs/plugin-reference.md`](docs/plugin-reference.md) for the per-plugin decode pattern.
- The operator-supplied env vars (`KEYCLOAK_URL`, `KEYCLOAK_REALM`, `TOKEN_URL`, `ISSUER`, `DEFAULT_OUTBOUND_POLICY`, `CLIENT_ID`) are consumed by the default `authbridge-combined.yaml` via `${VAR}` expansion — they land inside the appropriate plugin's `config:` block rather than a top-level section.
- `jwt-validation` derives `jwks_url` from `issuer` when omitted (appends `/protocol/openid-connect/certs`).
- `token-exchange` derives `token_url` from `keycloak_url + keycloak_realm` when omitted (Keycloak convention).
- Credential files: the **kagenti-operator** registers each workload with Keycloak and creates a Secret containing `client-id.txt` + `client-secret.txt`; the operator's webhook mounts that Secret at `/shared/client-id.txt` and `/shared/client-secret.txt` in containers that share the `shared-data` volume. SPIRE-issued credentials are sourced in-process via the `spiffe.Provider` (built from the top-level `spiffe:` block in `authbridge-runtime`) — authbridge's hot path reads X.509 SVIDs from an in-memory `spiffe.X509Source` (no per-handshake file I/O), and `token-exchange` consumes a JWT-SVID from the injected Provider via `plugins.BuildWithSPIFFE`. The Provider also mirrors `/opt/jwt_svid.token`, `/opt/svid.pem`, `/opt/svid_key.pem`, and `/opt/svid_bundle.pem` for external readers (e2e probes, debugging, future Envoy filesystem SDS). The `spiffe-helper` binary is no longer bundled in any combined image, and the `SPIRE_ENABLED` env var no longer gates anything — presence/absence of the `spiffe:` block in YAML drives behavior. `jwt-validation` reads the audience from `/shared/client-id.txt` via `audience_file`; `token-exchange` reads client credentials via `client_id_file` / `client_secret_file`. Each plugin attempts a synchronous read at Configure time and falls back to a background poll from its `Init` goroutine if the file isn't yet readable. The legacy in-pod `client-registration` sidecar has been removed entirely; the `kagenti.io/client-registration-inject: "true"` label is **no longer functional** — the operator's `ClientRegistrationReconciler` still treats it as a "skip operator-managed registration" signal (`SkipReason` in `kagenti-operator/internal/clientreg/names.go:58`), but the legacy sidecar that the label deferred to is gone. Setting it today silently breaks registration; do not add it to new manifests.
- Outbound route config: `token-exchange` reads `/etc/authproxy/routes.yaml` by default (path is per-plugin, configured via `routes.file` in its config block); inline rules can be declared under `routes.rules`.
- Outbound `default_policy`: `passthrough` (default) or `exchange`, configured per-plugin (no top-level `DEFAULT_OUTBOUND_POLICY` field anymore; the env var is still expanded into the plugin config by `authbridge-combined.yaml`).

**Key library packages (authlib/):**
- `authlib/validation/` -- JWKS-backed JWT verifier (used internally by `jwt-validation` plugin)
- `authlib/exchange/` -- RFC 8693 token exchange client (used internally by `token-exchange` plugin)
- `authlib/cache/` -- SHA-256 keyed token cache
- `authlib/routing/` -- Host-to-audience route resolver (used internally by `token-exchange` plugin)
- `authlib/auth/` -- `HandleInbound` + `HandleOutbound` composition; each plugin instance constructs its own `auth.Auth` from its own local config
- `authlib/config/` -- Mode presets, YAML config loader, credential-file waiters, top-level (mode + listener + session) validation
- `authlib/pipeline/` -- Plugin interface + lifecycle (`Configurable`, `Initializer`, `Shutdowner`); see [`docs/framework-architecture.md`](docs/framework-architecture.md)
- `authlib/plugins/` -- The concrete plugins + registry; see [`docs/plugin-reference.md`](docs/plugin-reference.md) for the per-plugin config convention

**Plugin classification.** Protocol parsers (`mcp-parser`, `a2a-parser`, `inference-parser`) populate an `IsAction bool` field on their respective extensions to classify each request as either a user-meaningful action or protocol mechanics. Default-false means "not classified as action" — guardrails treat it as bypass. Parsers explicitly set `IsAction = true` for the small set of action methods (`tools/call` / `prompts/get` / `resources/read` for MCP; `message/send` / `message/stream` for A2A; every populated case for inference). Guardrails (`ibac` today; future rate limiters, audit loggers, etc.) read the aggregated verdict via `pctx.Classification()` which returns `(anyAction, anyBypass)`. A defense-in-depth guardrail skips on `anyBypass`, passes through on `!anyAction` (no parser claimed this traffic), and judges only when `anyAction && !anyBypass`. This puts the protocol-specific bypass-vs-action vocabulary in each parser — adding a new guardrail or new protocol does not multiply work at the guardrail layer. See [`docs/plugin-reference.md` "Classifying requests"](docs/plugin-reference.md#classifying-requests-as-actions-vs-protocol-mechanics) for the contract.

### init-iptables.sh

Extensively documented shell script that sets up iptables for transparent traffic interception. Key features:

- **Outbound**: `PROXY_OUTPUT` chain in `nat OUTPUT`, redirects to Envoy port 15123
- **Inbound**: `PROXY_INBOUND` chain in `nat PREROUTING`, redirects to Envoy port 15124
- **Istio ambient mesh coexistence**: Handles ztunnel fwmark (0x539), HBONE port (15008), DNAT to POD_IP for inbound interception
- **Exclusions**: SSH (22), loopback, configurable `OUTBOUND_PORTS_EXCLUDE` and `INBOUND_PORTS_EXCLUDE`
- **Envoy UID 1337**: Excluded from outbound redirect to prevent loops
- **Mangle rule**: Sets fwmark on Envoy's local delivery to prevent ISTIO_OUTPUT redirect loop
- Uses `-I 1` (insert first) for chain ordering stability with Istio CNI

**Environment variables:**
| Variable | Default | Description |
|----------|---------|-------------|
| `PROXY_PORT` | 15123 | Envoy outbound listener |
| `INBOUND_PROXY_PORT` | 15124 | Envoy inbound listener |
| `PROXY_UID` | 1337 | Envoy process UID (excluded from redirect) |
| `OUTBOUND_PORTS_EXCLUDE` | (empty) | Comma-separated ports to exclude |
| `INBOUND_PORTS_EXCLUDE` | (empty) | Comma-separated ports to exclude |
| `POD_IP` | (required) | Pod IP via Downward API; used as DNAT target for ambient mesh inbound interception |

### client_registration.py

Idempotent Python script that:
1. Reads SPIFFE ID from `/opt/jwt_svid.token` JWT `sub` claim (when authbridge's `spiffe.Provider` mirror is writing the file — i.e., `spiffe:` is configured in `authbridge-runtime`)
2. Falls back to `CLIENT_NAME` env var as client ID (if no SPIRE-issued JWT-SVID is mirrored)
3. Creates or reuses a Keycloak client with token exchange enabled
4. Retrieves the client secret and writes to `SECRET_FILE_PATH` (in cluster deployments, the webhook sets `SECRET_FILE_PATH=/shared/client-secret.txt` to match the shared-volume contract)

**Keycloak client configuration created:**
- `publicClient: False` (confidential/authenticated)
- `serviceAccountsEnabled: True` (allows `client_credentials` grant)
- `standardFlowEnabled: True`
- `directAccessGrantsEnabled: True`
- `standard.token.exchange.enabled: True`

**Dependencies:** `python-keycloak==5.3.1`, `pyjwt==2.10.1`

### keycloak_sync.py

Declarative Keycloak synchronization tool that maintains client scope mappings based on `routes.yaml`. Idempotent, used in multi-target demos for dynamic scope assignments.

### Envoy Configuration

Envoy config lives in the `envoy-config` ConfigMap rendered by the [kagenti Helm chart](https://github.com/kagenti/kagenti) at install time (template: `charts/kagenti/templates/agent-namespaces.yaml` / `authbridge-template-configmaps.yaml`). Key listeners: `outbound_listener` (15123), `inbound_listener` (15124). Inbound listener injects `x-authbridge-direction: inbound` header. Both use ext_proc cluster pointing to the authbridge binary on localhost:9090.

## Demo Scenarios

The `demos/` directory contains the following scenarios (see `demos/README.md` for a recommended learning path):

- **weather-agent/** -- Getting-started demo: inbound JWT validation with outbound passthrough. Simplest way to see AuthBridge in action (UI deployment). `demo-ui-advanced.md` extends this with outbound token exchange and tool-side AuthBridge; `demo-with-abctl.md` is a plugin-pipeline tooling walkthrough.
- **webhook/** -- Shows how to use the webhook (now part of [kagenti-operator](https://github.com/kagenti/kagenti-operator)) to automatically inject AuthBridge sidecars. Recommended starting point for webhook-based deployments.
- **github-issue/** -- External API integration (GitHub) with inbound validation, outbound token exchange, and scope-based access control. Available as UI or manual deployment.
- **token-exchange-routes/** -- Configuration reference for the `authproxy-routes` ConfigMap. Covers single-target (one route) and multi-target (one agent → many tools) patterns. Pairs with one of the deployment demos for a full stack.
- **mcp-parser/** -- Configuration reference for enabling the outbound `mcp-parser` plugin.

## Keycloak Setup Scripts

There are **two** setup scripts for different demo scenarios:

| Script | Location | Use Case |
|--------|----------|----------|
| `setup_keycloak_weather_advanced.py` | `authbridge/demos/weather-agent/` | Weather agent (advanced) demo: realm setup, scopes for token exchange to the weather tool's audience, alice user. Drives the CI verify script `deploy_and_verify_advanced.sh`. |
| `setup_keycloak.py` | `authbridge/demos/github-issue/` | GitHub issue integration demo (creates github-tool client, github-tool-aud + github-full-access scopes, alice + bob users) |

**Common Keycloak defaults across all scripts:**
- URL: `http://keycloak.localtest.me:8080`
- Realm: `kagenti`
- Admin: `admin` / `admin`

**Note:** All scripts share the same helper function patterns (`get_or_create_realm`, `get_or_create_client`, `get_or_create_client_scope`, etc.) and are idempotent.

## Required ConfigMaps for Webhook Injection

When the webhook injects sidecars (via [kagenti-operator](https://github.com/kagenti/kagenti-operator)), these ConfigMaps must exist in the target namespace. The kagenti Helm chart's `agent-namespaces.yaml` and `authbridge-template-configmaps.yaml` templates render them; the operator copies them into agent namespaces that don't already have them:

| Resource | Kind | Consumer | Key Fields |
|----------|------|----------|------------|
| `authbridge-config` | ConfigMap | authbridge | `KEYCLOAK_URL`, `KEYCLOAK_REALM`, `PLATFORM_CLIENT_IDS` (optional), `TOKEN_URL` (optional, derived), `ISSUER` (optional, derived or explicit), `DEFAULT_OUTBOUND_POLICY` (optional). Inbound audience validation uses `CLIENT_ID` from `/shared/client-id.txt`. Target audience and scopes are configured per-route in `authproxy-routes`. |
| `keycloak-admin-secret` | Secret | kagenti-operator (ClientRegistrationReconciler) | `KEYCLOAK_ADMIN_USERNAME`, `KEYCLOAK_ADMIN_PASSWORD` |
| `authproxy-routes` | ConfigMap (optional) | authbridge | `routes.yaml` with per-host token exchange rules |
| `spiffe-helper-config` | ConfigMap (legacy, unused by authbridge) | (none — retained only for compatibility with older deployments) | Previously held `helper.conf` for the bundled `spiffe-helper` binary. Authbridge now drives SPIRE configuration via the top-level `spiffe:` block in `authbridge-runtime` and no longer reads this ConfigMap. |
| `envoy-config` | ConfigMap | Envoy (inside the `authbridge-envoy` combined image, envoy-sidecar mode only) | `envoy.yaml` (full Envoy configuration) |

**`authproxy-routes` format** (`routes.yaml`):
```yaml
routes:
  - host: "github-tool-mcp"
    target_audience: "github-tool"
    token_scopes: "openid github-tool-aud github-full-access"
  - host: "auth-target-*"
    target_audience: "auth-target"
    token_scopes: "openid auth-target-aud"
```

Authbridge defaults to **passthrough** for outbound requests that don't match any route. Token exchange only happens for hosts with explicit entries in `authproxy-routes`, where target audience and scopes are configured per-route.

## Shared Volume Contract

Sidecars communicate through files on shared volumes:

| Path | Writer | Reader | Content |
|------|--------|--------|---------|
| `/opt/jwt_svid.token` | spiffe.Provider mirror | authbridge (token-exchange) + external readers | JWT SVID, audience from `spiffe.jwt_audience` |
| `/opt/svid.pem` | spiffe.Provider mirror | external readers (debugging, future Envoy SDS) | X.509 SVID leaf cert (PEM) |
| `/opt/svid_key.pem` | spiffe.Provider mirror | external readers | X.509 SVID private key (PEM) |
| `/opt/svid_bundle.pem` | spiffe.Provider mirror | external readers | SPIRE trust bundle (PEM, may concatenate multiple CAs) |
| `/shared/client-id.txt` | operator (Secret mount) | authbridge | SPIFFE ID or workload name |
| `/shared/client-secret.txt` | operator (Secret mount) | authbridge | Keycloak client secret |

The X.509 SVID files are mirrored to disk by the in-process
`spiffe.Provider` whenever the runtime config carries a top-level
`spiffe:` block; the files exist for external readers (e2e probes,
debugging, future Envoy filesystem SDS) and are kept fresh on every
rotation. The listener itself reads SVIDs in-memory via
`spiffe.X509Source` and never re-reads the files. authbridge enables
mTLS only when `mtls:` is configured at the top level of
`authbridge-runtime`; absent that block, today's plaintext behavior is
preserved.

## Top-level `mtls:` configuration

When the runtime config carries an `mtls:` block, authbridge enables
transport-level mTLS on the proxy-sidecar listeners (forward + reverse
proxy). envoy-sidecar mode handles mTLS at the Envoy data-plane level
instead — see the **envoy-sidecar mTLS** subsection below.

```yaml
# authbridge-runtime ConfigMap (top-level)
mtls:
  mode: strict          # permissive | strict (omit block entirely for off)
  # cert_file / key_file / bundle_file optional —
  # default to /opt/svid.pem, /opt/svid_key.pem, /opt/svid_bundle.pem
```

| Mode | Inbound (reverse proxy `:8080`) | Outbound (forward proxy) |
|---|---|---|
| (no `mtls` block) | Plaintext only. | Plaintext only. |
| `permissive` (default when block present) | Byte-peek listener: TLS handshakes verified against the SPIRE trust bundle; plaintext callers served on the same port. ⚠️ Plaintext requests carry their full headers and bodies in the clear — including any `Authorization: Bearer ...` token already injected by `token-exchange`. Use only during rollout with cluster-network trust. | Plaintext — no TLS-wrap attempt. Matches envoy-sidecar's permissive and Istio's PeerAuthentication semantics (permissive is inbound-only). A permissive caller cannot reach a strict peer; mixed-mode deployments need both ends compatible. |
| `strict` | TLS only — non-TLS callers get the connection closed. | TLS or fail: handshake failure is a hard error, no fallback. |

In both modes, a successful TLS handshake that fails certificate
verification is always a hard error.

**Trust model:** any peer with a valid cert from the SPIRE trust bundle
can talk to this authbridge. Per-caller policy / SPIFFE allowlists are
out of scope; the trust bundle IS the policy. Plugins that want
per-caller decisions read `pctx.PeerCert` and check the URI SAN.

**Hot-reload boundary:** mTLS config (`mtls.mode`, cert paths) requires a
pod restart to apply, matching the existing rule for `listener.*`
addresses. Plugin-pipeline config keeps its own hot-reload behavior.

### envoy-sidecar mTLS

In envoy-sidecar mode the listeners live in Envoy, not authbridge,
so the top-level `mtls:` block is a no-op there. Equivalent semantics
are configured at the Envoy data-plane level by extending the
`envoy-config` ConfigMap with TLS blocks:

| Mode | Inbound (`:15124`) | Outbound (`:15123`) |
|---|---|---|
| `disabled` | plaintext (today's behavior) | plaintext (today's behavior) |
| `permissive` | `tls_inspector` listener filter + two filter chains: `transport_protocol: tls` chain terminates mTLS, `transport_protocol: raw_buffer` chain accepts plaintext | **plaintext** — no TLS-wrap attempt |
| `strict` | `tls_inspector` + single TLS filter chain; plaintext drops at filter chain match | `UpstreamTlsContext` on the `original_destination` cluster: TLS-or-fail, blanket |

X.509 SVIDs are read by Envoy directly from `/opt/svid.pem`,
`/opt/svid_key.pem`, `/opt/svid_bundle.pem` — the same paths
proxy-sidecar's mTLS uses. The spiffe Provider's file-mirror in
the `authbridge-envoy` binary keeps these fresh on rotation.

**Inbound parity with proxy-sidecar:** byte-identical observable
semantics — TLS handshakes terminate against the SPIRE trust bundle,
plaintext is served (permissive) or rejected (strict). Same outcome
as proxy-sidecar's `tlssniff.Listener`, just expressed as Envoy
filter chains. This matches Istio's PERMISSIVE/STRICT inbound exactly.

**Outbound is Istio-shaped:** Envoy has no native primitive for
"try TLS, fall back to plaintext on handshake failure" within an
`ORIGINAL_DST` cluster (and Istio itself doesn't do it — Pilot
pre-decides mesh membership). Permissive keeps outbound plaintext;
strict does blanket TLS-or-fail to everything the listener sees.
This works in practice because outbound calls that need plaintext —
Keycloak, JWKS, external HTTPS — never reach the listener: plugin
outbound uses Go `net/http` directly, and `proxy-init`'s iptables
doesn't redirect arbitrary HTTPS egress. **Proxy-sidecar matches
this**: its forward proxy now also dials plaintext in permissive
mode, so the two deployment shapes share one outbound semantics.

**Behavioral note:** a *permissive* caller cannot reach a *strict*
peer regardless of mode (its outbound is plaintext; the peer's
strict inbound rejects it). Mixed-mode deployments need both ends
compatible — both strict, both permissive, or one strict + the
other permissive on inbound only.

The kagenti-operator's AgentRuntime CR's `Spec.MTLSMode` flows
through to a per-agent rendered envoy-config with the matching TLS
blocks (operator companion PR). The `authbridge/demos/mtls/`
envoy-sidecar variant (`make demo-mtls-envoy*`) ships a hand-crafted
demo that proves the same Envoy YAML design at the data-plane level
without needing a CR.

## Build and Deploy

### Build images locally

```bash
# Build the proxy-init iptables init container (envoy-sidecar + proxy-sidecar enforce-redirect modes)
cd authbridge/proxy-init
make docker-build-init
make load-image                     # Uses KIND_CLUSTER_NAME env var (default: kagenti)

# Build the combined sidecar images from the authbridge/ context.
# Pick whichever you need; the operator selects the image per workload
# from the resolved AuthBridge mode (see kagenti-operator#361).
cd ..
podman build -f cmd/authbridge-proxy/Dockerfile -t authbridge:latest .       # proxy-sidecar (default)
podman build -f cmd/authbridge-envoy/Dockerfile -t authbridge-envoy:latest . # envoy-sidecar
# authbridge-lite: the proxy Dockerfile built with exclude_plugin_* tags (auth-only)
podman build -f cmd/authbridge-proxy/Dockerfile \
  --build-arg GO_BUILD_TAGS="exclude_plugin_a2aparser,exclude_plugin_ibac,exclude_plugin_inferenceparser,exclude_plugin_mcpparser,exclude_plugin_opa,exclude_plugin_sparc,exclude_plugin_tokenbroker" \
  -t authbridge-lite:latest .
kind load docker-image authbridge:latest       --name kagenti
kind load docker-image authbridge-envoy:latest --name kagenti
kind load docker-image authbridge-lite:latest  --name kagenti
```

For the repo-level "build everything" path, the root `local-build-and-test.sh` orchestrates all four images plus the kagenti-side `spiffe-idp-setup`.

### Full demo with webhook injection

The recommended end-to-end flow uses the weather-agent advanced demo,
which exercises the post-#411 combined sidecar shape with token
exchange to a tool's audience:

```bash
# Apply manifests, run Keycloak setup, verify end-to-end
authbridge/demos/weather-agent/deploy_and_verify_advanced.sh
```

For an interactive walkthrough see
`authbridge/demos/weather-agent/demo-ui-advanced.md`. For route
configuration see `authbridge/demos/token-exchange-routes/README.md`.

## Important Port Mapping

| Port | Component | Protocol | Purpose |
|------|-----------|----------|---------|
| 15123 | Envoy | TCP | Outbound listener (iptables redirects app traffic here) |
| 15124 | Envoy | TCP | Inbound listener (iptables redirects incoming traffic here) |
| 9090 | authbridge | gRPC | Ext-proc server (called by Envoy) |
| 9093 | authbridge | HTTP | Stats + config inspection (`/stats`, `/config`, `/reload/status`) |
| 9094 | authbridge | HTTP | Session events API (JSON snapshots + SSE stream) |
| 9901 | Envoy | HTTP | Admin interface (bound to 127.0.0.1) |

## Session Events API (`:9094`)

When `session.enabled` is true (default) and `listener.session_api_addr` is non-empty (default `:9094`), the authbridge binary exposes the captured session store over HTTP. Intended for operators debugging the plugin pipeline via `kubectl port-forward` and for the `abctl` TUI.

**Trust model:** no authentication. Bind only on in-cluster addresses, never behind ingress. Payloads may contain raw user messages, LLM completions, and tool results.

### Endpoints

| Method & Path | Format | Purpose |
|---|---|---|
| `GET /v1/sessions` | `application/json` | List active sessions: `{sessions: [{id, createdAt, updatedAt, eventCount, active}]}`. |
| `GET /v1/sessions/{id}` | `application/json` | Full snapshot of one session's events. 404 if unknown/expired. |
| `GET /v1/events` | `text/event-stream` | SSE stream of new events. Optional `?session=<id>` filters to one session. Heartbeat every 30s. |
| `GET /v1/pipeline` | `application/json` | Active pipeline composition: `{inbound: [...], outbound: [...]}`. Each plugin entry carries `name`, `direction`, `position`, `readsBody`, plus the static metadata (`requires`, `requiresAny`, `description`) and runtime `config` when present. abctl renders this as the Pipeline pane. |
| `GET /v1/plugins` | `application/json` | Catalog of every registered plugin (whether or not in the active pipeline): `{plugins: [{name, requires, requiresAny, description, ...}]}`. abctl renders this as the Catalog pane (`P` key). 404s when the binary's session API was constructed without `WithCatalog`. |
| `GET /healthz` | text | Liveness probe. |

### Quick examples

The `abctl` TUI handles port-forward + connection automatically — pick a
pod from the Namespaces → Pods picker. For raw HTTP exploration, set up
your own port-forward first:

```sh
POD=$(kubectl get pod -n team1 -l app.kubernetes.io/name=weather-agent \
  -o jsonpath='{.items[0].metadata.name}')
kubectl port-forward -n team1 $POD 9094:9094 &

# List sessions
curl -s http://localhost:9094/v1/sessions | jq

# Snapshot the most recently updated session
SID=$(curl -s http://localhost:9094/v1/sessions | jq -r '.sessions[0].id')
curl -s "http://localhost:9094/v1/sessions/$SID" | jq

# Live tail every event
curl -N http://localhost:9094/v1/events

# Live tail a single session
curl -N "http://localhost:9094/v1/events?session=$SID"
```

### Event schema

Every event on `/v1/sessions/{id}` and `/v1/events` carries:

- `at`, `direction`, `phase` — when, which side, what stage. `phase` is one of `"request"`, `"response"`, or `"denied"` (terminal denial from a pipeline plugin — typically a jwt-validation failure).
- `a2a` / `mcp` / `inference` — protocol parser payloads (one at most).
- `invocations` — per-plugin invocation records for every plugin that ran on the pipeline pass. Structured as `{inbound: [...], outbound: [...]}`; each entry carries `plugin`, `action` (one of 5 values — see below), `reason` (machine-stable code), and optional plugin-specific context (expected issuer, target audience, cache-hit flag, path, etc.). abctl renders one row per invocation, so operators see an explicit per-plugin timeline.
- `plugins` — escape-hatch map for plugin-specific observability. Keys are plugin names; values are the raw JSON each plugin emitted. Unknown plugins render as opaque JSON in abctl. See [`docs/plugin-reference.md`](docs/plugin-reference.md#emitting-session-events) for the producer contract.
- `identity`, `host`, `statusCode`, `error`, `durationMs` — request-level context.

### Invocation action vocabulary

Every plugin emits one of these 5 action values per invocation, so operators can scan a timeline without memorizing plugin-specific verbs:

| `action` | Meaning | Example |
|---|---|---|
| `allow` | Gate plugin permitted the request | jwt-validation on valid token |
| `deny` | Gate plugin rejected the request; pipeline stops | jwt-validation on bad token, token-exchange on IdP failure |
| `skip` | Plugin ran but didn't act on this message | jwt-validation on a bypass path; parser whose body didn't match |
| `modify` | Plugin mutated the message | token-exchange replaced the Authorization header |
| `observe` | Plugin attached diagnostic data without changing flow | a2a-parser, mcp-parser, inference-parser when they match |

Use `reason` to discriminate within an action — e.g. `skip/path_bypass` vs `skip/no_matching_route` tell different stories at the detail-pane level but both scan as "skip" in the at-a-glance timeline.

> **Producer-side contract:** the authoritative definition of the 5-value vocabulary, the `Invocation` struct fields, and which diagnostic fields each plugin type populates lives in [`docs/plugin-reference.md`](docs/plugin-reference.md#emitting-session-events). Edit that file when the vocabulary changes; this table is the consumer-side summary.

### Gotcha: denied requests

Rejected requests (401 / 503) land as `phase: "denied"` events in `/v1/sessions` when at least one pipeline plugin appended an Invocation before rejecting. If you're debugging an unauthorized-access pattern, the default-session bucket (`GET /v1/sessions/default`) is where denial events aggregate.

### Disabling

Set `session.enabled: false` in the runtime config to turn off the store (and implicitly the API). Setting `listener.session_api_addr: ""` alone is not currently supported as a selective disable — the preset refills it; if you need store-on-API-off, raise an issue.

## Config Hot-Reload (`:9093/reload/status`)

The authbridge binary watches its config file (`/etc/authbridge/config.yaml`) via `authlib/reloader`. When the ConfigMap changes, kubelet syncs the new content into the mount (~60s), the watcher detects it, and the binary rebuilds + atomically swaps the plugin pipelines without a pod restart. In-flight requests finish on the previous pipeline; new requests go to the new one.

**What reloads:** any plugin list change (add/remove/reorder) and any plugin `config:` subtree edit.

**What doesn't reload (pod restart required):** `mode`, any `listener.*` address, and the session store parameters (`session.ttl`, `session.max_events`, `session.max_sessions`).

**Bad YAML stays safe:** if Load/Validate/Build/Start fails, the active pipeline keeps serving and the error is exposed on `/reload/status` with `reloads_failed` incremented. The pod never goes unhealthy from a bad edit.

Operator workflow:

```sh
kubectl edit configmap authbridge-config-<agent> -n <ns>
kubectl port-forward -n <ns> deploy/<agent> 9093:9093 &
curl http://localhost:9093/reload/status  # last_success, reloads_ok, active_config_sha256
curl http://localhost:9093/config         # now-active config (ConfigProvider closure)
```

See [`docs/framework-architecture.md`](docs/framework-architecture.md#9-config-hot-reload) §9 for the reload lifecycle, debounce / symlink-swap handling, and the drain-window behavior.

## Code Conventions

### Go (authlib, cmd/authbridge-{proxy,envoy}, demo-app)
- Go 1.25
- Modules: `authbridge/authlib/` (pure library — all listeners, all plugins) and `authbridge/cmd/authbridge-{proxy,envoy}/` (mode-specific binaries that wire listeners + plugins together). The `authbridge-lite` image is the proxy binary built with `exclude_plugin_*` tags, not a separate module.
- `authbridge/go.work` workspace links the modules for local development
- Logging with `log/slog`; the binaries log under their own name (`authbridge-proxy`, `authbridge-envoy`). Note the `authbridge-lite` image runs the `authbridge-proxy` binary, so it logs as `authbridge-proxy`.
- gRPC ext-proc using `envoyproxy/go-control-plane` types (in `authlib/listener/extproc`)
- JWT validation with `lestrrat-go/jwx/v2` (in `authlib/plugins/jwtvalidation/validation`)

### Python (client-registration, setup scripts)
- Python 3.12 syntax (type hints: `str | None`)
- `python-keycloak` library for all Keycloak admin API calls
- `PyJWT` for JWT decoding (signature verification disabled -- uses `verify_signature: False`)
- Idempotent: all `get_or_create_*` helper functions check existence before creating
- UID/GID 1000 in Dockerfile **must match** the `runAsUser`/`runAsGroup` values set by the operator's webhook when injecting the client-registration container (see [kagenti-operator](https://github.com/kagenti/kagenti-operator))

### Shell (init-iptables.sh)
- `set -e` (exit on error)
- Extensive inline documentation explaining iptables chain ordering, Istio interactions, and debugging tips
- Idempotent: uses `iptables -N ... 2>/dev/null || true` and `iptables -F` before adding rules

## Common Tasks for Code Changes

### Modifying Token Exchange Logic
- Edit `authlib/exchange/` -- the RFC 8693 token exchange client
- The token exchange POST parameters follow RFC 8693 exactly
- Test by rebuilding the affected combined image (e.g.,
  `cd authbridge && podman build -f cmd/authbridge-envoy/Dockerfile
  -t authbridge-envoy:latest .` then `kind load docker-image
  authbridge-envoy:latest --name kagenti`).

### Modifying Inbound JWT Validation
- Edit `authlib/validation/` -- the JWKS-backed JWT verifier
- JWKS cache auto-refreshes
- Direction detection: `x-authbridge-direction: inbound` header (injected by Envoy inbound listener config)

### Adding New iptables Rules
- Edit `init-iptables.sh`
- Follow the existing pattern: document the rule's purpose, Istio interaction, and chain ordering
- Test with and without Istio ambient mesh if possible
- Rebuild: `make docker-build-init && make load-images`

### Modifying Client Registration
- Edit `client-registration/client_registration.py`
- The `register_client()` function is idempotent
- Keycloak client payload is the main configuration point
- Test: `kubectl delete pod <pod> -n <ns>` to trigger re-registration

### Adding New Keycloak Resources to Setup
- Edit the appropriate `setup_keycloak*.py` script
- Use the `get_or_create_*` helper pattern for idempotency
- All scripts use `python-keycloak` library (KeycloakAdmin class)

### Changing Envoy Configuration
- Edit the `envoy.yaml` template in the [kagenti Helm chart](https://github.com/kagenti/kagenti)
  (`charts/kagenti/templates/agent-namespaces.yaml` or
  `authbridge-template-configmaps.yaml`) and `helm upgrade`
- Key listener/cluster names: `outbound_listener`, `inbound_listener`, `original_destination`, `ext_proc_cluster`
- After changes, restart the affected pods so they pick up the new ConfigMap content

## Gotchas and Known Issues

1. **Credential file race condition**: Each plugin that reads a credential file (jwt-validation's `audience_file`, token-exchange's `client_id_file` / `client_secret_file` / `jwt_svid_path`) tries a synchronous read at Configure time and, on miss, spawns an Init goroutine that polls indefinitely — emitting a WARN every ~60s while the file is still missing. OnRequest returns 503 until the file arrives. If the file never shows up (wrong path, missing volume mount), the pod stays unready for outbound traffic; follow the WARN lines to the misconfigured path.

2. **ISSUER vs TOKEN_URL**: `ISSUER` must be the Keycloak **frontend URL** (what appears in the `iss` claim of tokens), while `TOKEN_URL` is the **internal service URL**. These are often different in Kubernetes (e.g., `http://keycloak.localtest.me:8080` vs `http://keycloak-service.keycloak.svc:8080`).

3. **Keycloak port exclusion**: When using iptables interception, Keycloak's port (8080) must be excluded from redirect via `OUTBOUND_PORTS_EXCLUDE=8080`. Otherwise, token exchange requests from authbridge get redirected back to Envoy, creating a loop.

4. **TLS passthrough is one-way**: Outbound HTTPS traffic passes through Envoy without token exchange via the TLS passthrough filter chain. Only plaintext HTTP outbound traffic reaches authbridge. With the default outbound policy of `"passthrough"`, even plaintext HTTP traffic is forwarded unchanged unless it matches an explicit route in `authproxy-routes`.

5. **Admin credentials in ConfigMap**: the kagenti Helm chart's
   `agent-namespaces.yaml` template stores Keycloak admin credentials
   in `authbridge-config` (a ConfigMap, not a Secret). This is for
   demo / dev clusters only — production should use a Kubernetes
   Secret and mount via SecretKeyRef.

6. **Envoy Lua filter required for inbound**: The `x-authbridge-direction: inbound` header MUST be injected via a Lua filter before the ext_proc filter in the inbound listener. Route-level `request_headers_to_add` does NOT work because the router filter runs after ext_proc.

7. **iptables backend auto-detection**: `init-iptables.sh` auto-detects `iptables-legacy` vs `iptables-nft`. Override with `IPTABLES_CMD` env var if needed. Always verify with proxy-init logs after deployment.

8. **Route host patterns must match HTTP Host header**: The `host` field in `authproxy-routes` is matched against the HTTP `Host` header, which is set by the HTTP client from the URL hostname. For in-cluster calls, this is the **short Kubernetes service name** from `MCP_URL` (e.g., `github-tool-mcp`), not the FQDN. Using the wrong pattern (e.g., `*.github-issue-tool*.svc.cluster.local`) will silently fall through to the default passthrough policy.

9. **Keycloak scope assignment for dynamically registered clients**: When the operator's `ClientRegistrationReconciler` auto-registers an agent as a Keycloak client, the client may not inherit all necessary scopes. The agent's own audience scope (e.g., `agent-team1-git-issue-agent-aud`) must be a **default** client scope for inbound JWT audience validation to work. Token exchange scopes (e.g., `github-tool-aud`, `github-full-access`) must be **optional** client scopes for `client_credentials` grants with explicit `scope=` to succeed. Re-run the demo's `setup_keycloak.py` after the agent is deployed to assign these scopes to the registered client.

10. **Outbound passthrough is the safe default**: The `DEFAULT_OUTBOUND_POLICY` defaults to `passthrough`, which means outbound traffic to LLM inference endpoints (e.g., Ollama via `host.docker.internal`) passes through without token exchange. If this were set to `exchange`, all outbound HTTP calls would attempt token exchange and fail for non-Keycloak destinations.

11. **Chatty observability traffic and IBAC user intent**: The session store is FIFO with a default cap of 100 events per session. Two layered defenses keep the inbound A2A user intent visible to IBAC even when an agent generates dozens of outbound events per turn:

    - **Primary fix — `listener.skip_hosts`**: list infrastructure destinations (OTel collectors, metrics endpoints, log shippers) whose traffic should bypass the pipeline AND session recording entirely. Matched requests are forwarded as a transparent proxy: no plugin runs, no event is appended. Patterns use the same `.`-delimited glob semantics as `authproxy-routes`; ports are stripped before matching. Example:
      ```yaml
      listener:
        skip_hosts:
          - "otel-collector.*.svc.cluster.local"
          - "*.metrics.local"
      ```
      Any change to `listener.skip_hosts` requires a pod restart (same rule as other `listener.*` fields). Do NOT add hosts here that need IBAC / token-exchange policy applied — bypass means bypass.

    - **Backstop — intent pin in the eviction policy**: even with `skip_hosts` empty, the session store now pins the most-recent inbound A2A request event against FIFO eviction. If the buffer overflows, every other event evicts in normal chronological order; the protected intent stays at its original timestamp, leaving a visible time gap in the timeline. Older intents from earlier turns are NOT pinned — only the latest one — so a multi-turn conversation with huge fan-out can't pile up stale intents and starve the buffer. The pin protects against FIFO eviction only: IBAC's `LastIntent()` survives buffer overflow as long as the session is still alive and an inbound A2A request has landed in it, but can still return nil after session expiry, explicit deletion, or before the first inbound request arrives. The pin is defense-in-depth; reach for `skip_hosts` first when the offending traffic is identifiable infrastructure.

## DCO Sign-Off (Mandatory)

All commits **must** include a `Signed-off-by` trailer (Developer Certificate of Origin).
Always use the `-s` flag when committing:

```sh
git commit -s -m "fix: Fix token exchange"
```

PRs without DCO sign-off will fail CI checks.

## Commit Attribution Policy

Do NOT use `Co-Authored-By` trailers for AI attribution. Use `Assisted-By` instead:

    Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>

Never add `Co-authored-by`, `Made-with`, or similar trailers that GitHub parses as co-authorship.
See the [root CLAUDE.md](../CLAUDE.md) for full commit policy details.
