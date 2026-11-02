# `token-exchange`

RFC 8693 outbound token exchange per route. Supports Keycloak, Entra
ID, Okta, and any RFC 8693-compliant IdP. For the `IdPProvider`
interface each IdP implements, see
[idp-plugin-contract.md](./idp-plugin-contract.md).

- `token_url` (string) — OAuth token endpoint; required unless derived from `provider` + `provider_url`(+`provider_realm`), or the deprecated `keycloak_url`/`keycloak_realm`.
- `provider` (string) — IdP selector for endpoint derivation and client auth: `keycloak`, `generic`.
- `provider_url` / `provider_realm` (string) — IdP base URL and realm/tenant, meaning varies by provider.
- `keycloak_url` / `keycloak_realm` (string) — deprecated aliases for `provider_url`/`provider_realm` with `provider=keycloak`.
- `default_policy` (string) — behavior when no route matches: `passthrough` (default) or `exchange`. With `exchange`, an unrouted host still needs a resolved audience (from `audience_from_host` or a matching route): an exchange with no audience returns a token valid for **every** audience the caller holds, so it is refused with 503 `no_audience` rather than sent.
- `no_token_policy` (string) — behavior for outbound requests with no bearer token: `client-credentials`, `allow`, or `deny` (default). `client-credentials` with no resolved audience is refused the same way.
- `identity.type` (string) — `spiffe` (JWT-SVID assertion) or `client-secret`; required.
- `identity.client_id` / `identity.client_id_file` — OAuth client ID, inline or from file (default `/shared/client-id.txt`).
- `identity.client_secret` / `identity.client_secret_file` — client secret, inline or from file (default `/shared/client-secret.txt`).
- `identity.jwt_audience` (string) — audience claim minted on the JWT-SVID assertion; required when `type=spiffe`.
- `identity.assertion_type` (string) — client-assertion URN: `jwt-spiffe` (default) or `jwt-bearer` (Okta).
- `routes.file` (string) — path to `routes.yaml`. Default `/etc/authproxy/routes.yaml`.
- `routes.rules` (list) — inline route entries (`host`, `target_audience`, `token_scopes`, `token_url`, `action`), combined with file-loaded routes.
- `audience_from_host` (bool) — derive audience from host for unrouted requests. Default `false`.
- `resolve_placeholders` (bool) — resolve an inbound placeholder-prefixed bearer to its real token before exchange; unresolvable placeholders are denied. Default `false`.

## Credential placeholder swap

An opt-in mode that keeps the **real** user token out of the agent. With
it on, `jwt-validation` validates the inbound token and then replaces it
with an opaque `abph_` placeholder before forwarding to the agent; the
real token is held in a process-scoped shared store. On the way out,
`token-exchange` resolves the placeholder back to the real token (on a
matched route) before its normal RFC 8693 exchange. The agent thus holds
only an opaque handle, never a usable credential.

For the design rationale, data flow, and branch table, see
[`docs/superpowers/specs/2026-06-02-credential-placeholder-swap-design.md`](./superpowers/specs/2026-06-02-credential-placeholder-swap-design.md).

### The two flags are a matched pair

| Flag | Plugin | Chain | Role |
|---|---|---|---|
| `placeholder_mode` | `jwt-validation` | inbound | **mint** — swap real token → `abph_` handle |
| `resolve_placeholders` | `token-exchange` | outbound | **resolve** — swap `abph_` handle → real token, then exchange |

Both must be on to be coherent:

- **Mint on, resolve off** → the outbound side sees an `abph_` subject it
  can't use and **fails closed (deny)**. Safe and visible, but broken.
- **Resolve on, mint off** → no `abph_` tokens are ever produced, so
  resolve is a **no-op**; normal exchange is unaffected.

The pairing can't be expressed via `Requires` (that checks plugin
*name*, not config *state*), so v1 relies on the fail-closed deny plus
this documentation rather than build-time cross-validation.

### `token-exchange`: `resolve_placeholders`

Bool, default `false`. When the outbound bearer carries the `abph_`
prefix, resolve it from the shared store to the real token before the
normal exchange. An unresolvable placeholder (unknown or expired —
e.g. after a sidecar restart) is **denied, fail-closed**; the opaque
string is never sent to an upstream. A non-placeholder bearer skips
resolve and runs the normal exchange (backward compatible). The lookup
is the first step of the outbound pass, before the route match — a
stale handle is denied even when the host matches no route. The
resolved token is used only by the exchange: it is never written back
into the request's headers, so a passthrough host still receives the
opaque handle (next section).

### Passthrough hosts receive the placeholder, not the real token

With mint on, the agent never holds the real token, so any non-exchange
(passthrough) egress forwards the opaque `abph_` handle as-is. It is
useless off-box. Any host that needs a real credential MUST be
configured as a `token-exchange` route — passthrough egress cannot
produce one.

### The store is in-memory and process-scoped

The handle→token store lives in memory and is shared only within a
single process. The mode therefore works only where inbound mint and
outbound resolve run in the **same** process:

- the reverse+forward proxy sidecar (`cortex`, and its
  `authbridge-lite` image variant);
- a **single-replica** extproc (`cortex-envoy`).

Multi-replica (HA) deployments can land mint and resolve on different
processes, which the in-memory store cannot bridge. Those need an external store behind the same
interface — a **current limitation**, tracked as a future enhancement.

### Security

- The handle is a random `abph_`-prefixed token (CSPRNG ≥256-bit),
  meaningless outside the minting process's store.
- The real token never reaches the agent and never persists to disk
  (in-memory store only).
- Neither the real token nor the handle is logged in cleartext —
  records carry a hash or the prefix only (see the "NEVER put raw
  tokens" rules under [Emitting session events](plugin-reference.md#emitting-session-events)).
