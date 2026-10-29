# `jwt-validation`

Validates inbound JWTs: signature via JWKS, issuer, and audience.

- `issuer` (string) — expected `iss` claim; required.
- `jwks_url` (string) — JWKS endpoint; derived from Keycloak URL/realm or issuer when omitted.
- `keycloak_url` / `keycloak_realm` (string) — used to derive `jwks_url` when omitted.
- `audience` (string) — expected `aud` claim; one of `audience` / `audience_file` / `allowed_audiences` required.
- `audience_file` (string) — file to read expected audience from. Default `/shared/client-id.txt`.
- `audience_mode` (string) — `static` only. `per-host` was removed: it derived the expected audience from the client-controlled `Host` header, so a token minted for any host was accepted for this agent (confused deputy). A config still naming it fails validation at load.
- `allowed_audiences` (`[]string`) — extra audience values accepted (OR semantics).
- `bypass_paths` (`[]string`) — path globs skipped. Default `/healthz`, `/readyz`, `/livez`, `/metrics`, `/.well-known/*`.
- `placeholder_mode` (bool) — replace the validated inbound token with an opaque placeholder before forwarding, for later outbound resolution. Default `false`.
- `placeholder_ttl` (string) — how long the real token is retained. Default `1h`.
