# `jwt-validation`

Validates inbound JWTs: signature via JWKS, issuer, and audience.

- `issuer` (string) — expected `iss` claim; required.
- `jwks_url` (string) — JWKS endpoint; derived from Keycloak URL/realm or issuer when omitted.
- `keycloak_url` / `keycloak_realm` (string) — used to derive `jwks_url` when omitted.
- `audience` (string) — expected `aud` claim; one of `audience` / `audience_file` / `allowed_audiences` required.
- `audience_file` (string) — file to read expected audience from. Default `/shared/client-id.txt`.
- `audience_mode` (string) — `static` only. `per-host` was removed: it derived the expected audience from the client-controlled `Host` header, so a token minted for any host was accepted for this agent (confused deputy). A config still naming it fails validation at load.
- `allowed_audiences` (`[]string`) — extra audience values accepted (OR semantics).
- `bypass_paths` (`[]string`) — path globs skipped. Default `/healthz`, `/readyz`, `/livez`, `/metrics`, `/.well-known/*`. Only a canonical path matches: the app gets the path as sent, so `/mcp/../healthz`, `//healthz` and any path with `;`, `\`, `%` or `?` after decoding are validated, and a trailing slash matches only a glob that ends in one.
- `placeholder_mode` (bool) — replace the validated inbound token with an opaque placeholder before forwarding, for later outbound resolution. Default `false`.
- `placeholder_ttl` (string) — how long the real token is retained. Default `1h`.

## `jwt-validation`: `allowed_audiences` (optional, transitional)

The `jwt-validation` plugin accepts an optional JSON array field
`allowed_audiences` (YAML under `config:`). Each entry is an additional
**expected** inbound JWT `aud` value. The plugin **union**s these with
the primary audience from `audience` / `audience_file` (deduplicated,
order preserved). Inbound verification succeeds if the token's `aud`
claim (RFC 7519 — string **or** array) contains **any** configured value.

Use this as a **short-term bridge** when legitimate tokens carry
multiple audiences (for example a public UI client plus `account`) and
the workload must accept more than the SPIFFE / client ID read from
`audience_file` alone. Prefer aligning IdP audience policy and
application token exchange long-term; see team design discussions linked
from release notes.

When `allowed_audiences` is non-empty, the default
`/shared/client-id.txt` `audience_file` is **not** auto-added unless you
still omit `audience`, `audience_file`, and `allowed_audiences` entirely
— i.e. you can run with **only** `allowed_audiences` for static mode.

**Session / log migration:** jwt-validation deny-path `Details` used a
single key `expected_audience` (string). It now emits
`expected_audiences` (comma-joined list of configured inbound audiences).
Update saved queries and dashboards that filtered on the old
key.

## `jwt-validation`: `placeholder_mode` / `placeholder_ttl` (optional, off by default)

Two fields enable the **mint** half of [credential placeholder
swap](token-exchange-plugin.md#credential-placeholder-swap) (read that section for the full
model — these are the field-level reference).

- **`placeholder_mode`** — bool, default `false`. After validating the
  inbound token, replace it with an opaque `abph_`-prefixed placeholder
  before forwarding to the agent. The real token is held in the
  process-scoped shared store for the outbound path to resolve. Requires
  `token-exchange` with `resolve_placeholders: true` on the outbound
  chain to be coherent (see
  [the matched pair](token-exchange-plugin.md#the-two-flags-are-a-matched-pair)).
- **`placeholder_ttl`** — Go duration string (e.g. `30m`, `1h`), default
  `1h`. How long the real token is retained for outbound resolution.

Mint requires `pctx.Shared` to be wired by the listener; if it is nil
when `placeholder_mode` is on, the plugin fails fast at init (a deploy
error) rather than silently forwarding the real token.
