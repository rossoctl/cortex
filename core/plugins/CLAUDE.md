# core/plugins — agent notes: the auth paths

Loaded by Claude Code when you read a file under `core/plugins/`. It covers the
inbound JWT validation and outbound token exchange paths, which the
`jwt-validation` and `token-exchange` plugins implement. The repo-wide rules,
including the plugin capability and classification rules, are in the root
[`CLAUDE.md`](../../CLAUDE.md). Paths in backticks are written from the repo root.

## Inbound and outbound auth paths

**Inbound path** (`x-authbridge-direction: inbound`):
- Validates JWT signature via JWKS (auto-refreshing cache; `jwks_url` derivation is below)
- Validates issuer claim against `ISSUER` env var
- Validates audience: the token's `aud` must match one configured audience. The set is `allowed_audiences` plus `audience`, or plus the value in `audience_file` when `audience` is unset. With none of them set, `audience_file` defaults to `/shared/client-id.txt`
- Returns 401 with JSON error body for invalid/missing tokens
- Removes `x-authbridge-direction` header before forwarding to app

**Outbound path** (no direction header):
- Default policy is **passthrough** -- outbound requests pass through unchanged unless a route matches
- Uses a **route resolver** to match the request's `Host` header against patterns in `authproxy-routes` ConfigMap
- If a route matches: reads `target_audience` and `token_scopes` from the route, exchanges the incoming token for one with that audience (RFC 8693), and injects it as `Authorization: Bearer <token>`. A request with no token gets a `client_credentials` token only when the plugin's no-token policy says so
- If no route matches: applies the default outbound policy (`passthrough` or `exchange`)
- Returns 503 if exchange fails for a routed host (prevents unauthenticated calls)
- The plugin's `default_policy` controls the fallback (default: `passthrough`)

**Route resolver (outbound):**
- Reads `/etc/authproxy/routes.yaml` by default; the plugin's `routes.file` sets the path, and `routes.rules` adds inline rules
- Each route entry has: `host` (glob pattern), `target_audience`, `token_scopes`
- Host matching uses `gobwas/glob` with `.` as the separator (`core/internal/hostglob`): `*` matches one DNS label, `**` crosses labels
- Most commonly, `host` is a plain Kubernetes service name (e.g., `github-tool-mcp`) because the HTTP client sets the Host header from the URL hostname
- The routes file is read when `token-exchange` is configured — at startup and on a config hot-reload; nothing watches the routes file itself

**Configuration loading:** (the config format is in the root `CLAUDE.md`, AuthBridge Binaries section.)
- Plugin settings are local to each plugin under `pipeline.*.plugins[].config`; the runtime YAML's other top-level blocks are `mode`, `listener`, `session`, `stats`, `mtls`, `spiffe`, `tls_bridge`, `memory`, `pricing` and `cost_ledger`. See [`docs/plugin-reference.md`](../../docs/plugin-reference.md) for the per-plugin decode pattern.
- The operator-supplied env vars (`KEYCLOAK_URL`, `KEYCLOAK_REALM`, `TOKEN_URL`, `ISSUER`, `DEFAULT_OUTBOUND_POLICY`, `CLIENT_ID`) are consumed by the default `authbridge-runtime-config` via `${VAR}` expansion — they land inside the appropriate plugin's `config:` block rather than a top-level section.
- `jwt-validation` derives `jwks_url` when omitted: from `keycloak_url` + `keycloak_realm` if both are set, else from `issuer` (appends `/protocol/openid-connect/certs`).
- `token-exchange` derives `token_url` when omitted from `provider` + `provider_url` + `provider_realm`. `keycloak_url` / `keycloak_realm` are deprecated aliases and log a warning.
- Credential files: the **operator** registers each workload with Keycloak and creates a Secret containing `client-id.txt` + `client-secret.txt`; the operator's webhook mounts that Secret at `/shared/client-id.txt` and `/shared/client-secret.txt` in containers that share the `shared-data` volume. SPIRE-issued credentials are sourced in-process via the `spiffe.Provider` (built from the top-level `spiffe:` block in `authbridge-runtime-config`) — authbridge's hot path reads X.509 SVIDs from an in-memory `spiffe.X509Source` (no per-handshake file I/O), and `token-exchange` consumes a JWT-SVID from the injected Provider via `plugins.BuildWithSPIFFE`. The Provider also mirrors the SVIDs under `/opt/` (see the Shared Volume Contract in [`demos/CLAUDE.md`](../../demos/CLAUDE.md#shared-volume-contract)). `jwt-validation` reads the audience from `/shared/client-id.txt` via `audience_file`; `token-exchange` reads client credentials via `client_id_file` / `client_secret_file` (polling: see the credential-file gotcha below). The `rossoctl.io/client-registration-inject: "true"` label is **no longer functional** — the operator's `ClientRegistrationReconciler` still treats it as a "skip operator-managed registration" signal (`SkipReason` in `operator/internal/clientreg/names.go:58`), but the legacy sidecar that the label deferred to is gone. Setting it today silently breaks registration; do not add it to new manifests.
- Outbound `default_policy`: `passthrough` (default) or `exchange`, configured per-plugin (there is no top-level field; `authbridge-runtime-config` expands the `DEFAULT_OUTBOUND_POLICY` env var into the plugin config).

## Modifying Token Exchange Logic
- Edit `core/plugins/tokenexchange/exchange/` -- the RFC 8693 token exchange client
- The token exchange POST parameters follow RFC 8693 exactly
- Test by rebuilding the affected image. `GO_BUILD_TAGS` is required — every
  plugin is opt-in, so a build without it registers none and rejects every
  config it is handed:
  `podman build -f cmd/cortex-envoy/Dockerfile
  --build-arg GO_BUILD_TAGS="$(go -C scripts/profile-tags run . envoy)"
  -t authbridge-envoy:latest .`, then load it into Kind as
  [`demos/CLAUDE.md`](../../demos/CLAUDE.md#building-everything-locally) shows.

## Modifying Inbound JWT Validation
- Edit `core/plugins/jwtvalidation/validation/` -- the JWKS-backed JWT verifier
- JWKS cache auto-refreshes
- Direction detection: `x-authbridge-direction: inbound` header (injected by Envoy inbound listener config)

## Gotcha

- **Credential file race condition**: Each plugin that reads a credential file (jwt-validation's `audience_file`, token-exchange's `client_id_file` / `client_secret_file`; the JWT-SVID comes from the spiffe Provider, not a file) tries a synchronous read at Configure time and, on miss, spawns an Init goroutine that polls indefinitely — emitting a WARN every ~60s while the file is still missing. Until the file arrives, OnRequest returns 503. For jwt-validation, only an inbound request with a bearer token gets the 503: a request with no `Authorization` header gets 401 first. Exception: when `allowed_audiences` already gives jwt-validation an audience, it does not poll and does not return 503. So an `audience_file` that becomes readable only after Configure is not loaded until the next restart or config reload, and tokens for that audience are rejected until then. If the file never shows up (wrong path, missing volume mount), the plugin keeps refusing traffic; follow the WARN lines to the misconfigured path.
