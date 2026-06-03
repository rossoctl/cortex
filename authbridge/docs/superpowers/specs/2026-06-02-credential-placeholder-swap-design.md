# Design: Credential placeholder swap (hide real tokens from the agent)

**Date:** 2026-06-02
**Status:** Draft (design approved, pending spec review)
**Scope (v1):** authbridge — `jwt-validation` plugin, `token-exchange` plugin, a new shared store, and inbound-header propagation in the **`reverseproxy` and `extproc`** listeners (the two single-process topologies that ship today). `extauthz`/waypoint and an external store are **deferred** — see "Out of scope". This scope statement is authoritative; the per-listener and files-touched sections below mark `extauthz` as deferred to match.

## Problem

Today the agent workload sees real credentials. On the inbound path, `reverseproxy`
forwards the user's `Authorization: Bearer <token>` straight through to the agent
(it never strips it). The agent then either forwards that real token outbound (where
`token-exchange` swaps it for a downstream-scoped token) or sends nothing and relies
on client-credentials injection.

This means a compromised or prompt-injected agent holds the real user token — exactly
the secret we'd like to keep out of its reach.

**Goal:** the agent never receives the real `Authorization` value. Instead it receives
an opaque random **placeholder**. When the agent forwards that placeholder on an
outbound call, authbridge swaps it back to the real token (then runs its normal token
exchange) before the request leaves the sidecar.

This escapes the current either/or: it preserves user-delegated identity (unlike
client-credentials injection) **and** keeps the secret away from the agent (unlike
forwarding the real token).

## Why not the existing config?

`jwt-validation` is a validator (it can't mint/strip), and `token-exchange` treats the
inbound bearer as an RFC 8693 *subject token* — a random placeholder is not a valid JWT,
so Keycloak would reject it. The placeholder pattern needs a server-side
handle→token store, a resolver step, and inbound mint/strip — none of which exist.
See the conversation history for the full walk-through.

## Approach

Two existing plugins gain an opt-in mode; one listener gains a small propagation step;
one new tiny store is added. **No new plugin, no new framework abstraction.**

```
User → [reverseproxy: inbound] → Agent → [forwardproxy: outbound] → Upstream
        jwt-validation (mint)            token-exchange (resolve + exchange)
                    \                    /
                     \                  /
                  shared.Store (process-scoped, keyed by handle)
```

### Decisions (resolved during brainstorming)

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Store topology | **Sidecar-only, in-memory, TTL** | Agent + sidecar share one process; no new infra; extend to external store only when waypoint/shared-Envoy needs it |
| Handle format | **Opaque random `abph_` token** (CSPRNG ≥256-bit) | Simplest; prefix gives the resolver a cheap fast-path |
| Redemption scope | **Existing token-exchange routes** (no handle-level audience binding in v1) | Route-gating already prevents off-route leakage; multi-configured-upstream blast radius equals today's; per-session least-privilege is a future hardening |
| State sharing | **`pctx.Shared` injected by the listener** (not a package global) | Mirrors the existing `sessions` store wiring; explicit, testable, no global mutable state |

## Components

### Layer 1 — shared store (reusable)

A generic, semantics-free, process-scoped TTL key→value store. It is **not**
credential-aware; the placeholder logic lives in the plugins.

New package `authlib/shared`:

```go
package shared

type entry struct{ val any; expires time.Time }

type Store struct {
    mu    sync.RWMutex
    items map[string]entry
    now   func() time.Time // injectable for tests
}

func New() *Store { return &Store{items: map[string]entry{}, now: time.Now} }
func (s *Store) Put(key string, val any, ttl time.Duration) // lock; set expires
func (s *Store) Get(key string) (any, bool)                 // RLock; lazy-evict if expired
func (s *Store) Delete(key string)                          // lock; delete
```

- Eviction: lazy on `Get` (v1). A periodic background sweep to reclaim
  minted-but-never-resolved handles is a future enhancement; for a sidecar with
  bounded handle volume, lazy eviction plus TTL is sufficient. The store mirrors
  `tokenexchange/cache`'s thread-safe TTL shape.
- Correctness under concurrency comes from the unique, unguessable handle key —
  concurrent users never collide.
- Justified as reusable (not speculative) because `tokenexchange/cache` is already a
  sibling TTL `string→token` map. Future consumers (idempotency keys, counters, other
  brokers) call `Put`/`Get` with namespaced keys; no new infra.
- **Guardrail:** to avoid the "junk drawer" problem (`session.Store` carries a comment
  warning of exactly this), keep the API to three methods and namespace keys by feature
  (e.g. `placeholder/<handle>`).

The pipeline depends only on a small interface (defined in `pipeline`, so no import
cycle and tests can inject a fake):

```go
// authlib/pipeline/context.go
type SharedStore interface {
    Put(key string, val any, ttl time.Duration)
    Get(key string) (any, bool)
    Delete(key string)
}
type Context struct {
    // ...
    Shared SharedStore // process-scoped; set by the listener; may be nil
}
```

### Layer 2 — placeholder logic (in the plugins)

Mint, the `abph_` prefix convention, fail-closed resolve, and the configured
`placeholder_ttl` (default `1h`) live in the two plugins — not in the store.

### Wiring (mirrors the existing `sessions` injection)

In `cmd/authbridge-proxy/main.go`, both listeners are already built in one `main()` and
already share a process-scoped store (`sessions`, `config.go`/`main.go:199,246,250`).
The shared store follows the same pattern:

```go
sh := shared.New()                                                       // next to sessions
rpSrv, _ := reverseproxy.NewServer(inboundH, sessions, sh, backend, rpMTLS)
fpSrv, _ := forwardproxy.NewServer(outboundH, sessions, sh, fpMTLS)
```

Each server stores it and sets `pctx.Shared` when building the context (reverseproxy
`server.go:~170`, forwardproxy `handleRequest:~159`). For the **extproc/extauthz**
single-server topologies (`cmd/authbridge-envoy`), one server owns the store and sets it
on both inbound and outbound contexts.

### Inbound header propagation (per-listener — the one real blocker)

Mint requires the minted placeholder to actually reach the agent, i.e. the **inbound**
pipeline's `Authorization` mutation must be propagated to the request forwarded to the
agent. Before this change no listener did this — they propagated only the *outbound*
`Authorization` swap. **v1 implements the inbound mirror for `reverseproxy` and `extproc`**;
`extauthz` is deferred (see Out of scope). Each needs it in its own idiom:

| Listener | v1 | Today | Change |
|----------|----|-------|--------|
| `reverseproxy` | ✅ done | clones headers into `pctx`, copies only **body** mutations back (`server.go:171,220-225,253`); inbound header mutations dropped | after `Run`, if `pctx.Headers.Get("Authorization")` changed, copy it to `r.Header` before `ServeHTTP` |
| `extproc` | ✅ done | `handleOutbound` emits `replaceTokenResponse` on auth change; `handleInbound`/`handleInboundBody` emit **no** auth mutation | capture `originalAuth`/`newAuth` in the inbound handlers and emit the `replaceTokenResponse` HeaderMutation (with `RemoveHeaders: x-authbridge-direction`) when changed |
| `extauthz` | ⛔ deferred | inbound `Check` validates but returns no request-header injection for the agent | (future) add the placeholder header to the inbound `OkResponse` (request-header mutation) |

This generalizes cleanly to "inbound plugins may rewrite the request to the agent," a
capability the listeners arguably should have regardless.

## Configuration

Plugins decode config via `Configure(json.RawMessage)`; each pipeline `PluginEntry`
(`config.go:199`) carries an optional `config:` subtree. New modes are new struct
fields, **off by default** (per the feature-flag mandate).

`jwt-validation` config additions:

```go
PlaceholderMode bool   `json:"placeholder_mode" default:"false" description:"After validating the inbound token, replace it with an opaque placeholder before forwarding to the agent; the real token is held in the shared store for the outbound path to resolve."`
PlaceholderTTL  string `json:"placeholder_ttl" default:"1h" description:"How long the real token is retained for outbound resolution (Go duration). Default 1h. Binding the TTL to the token's own exp is a future enhancement."`
```

`token-exchange` config addition:

```go
ResolvePlaceholders bool `json:"resolve_placeholders" default:"false" description:"Resolve an inbound bearer carrying the placeholder prefix from the shared store to the real token before exchange. Unresolvable placeholders are denied."`
```

Operator YAML:

```yaml
pipeline:
  inbound:
    plugins:
      - name: jwt-validation
        config:
          issuer: https://keycloak/...
          audience: agent
          placeholder_mode: true        # NEW
  outbound:
    plugins:
      - name: token-exchange
        config:
          keycloak_url: https://keycloak
          routes: { ... }
          resolve_placeholders: true    # NEW
```

Free wins: abctl forms pick up the new fields automatically via `SchemaOf` reflection;
the framework `on_error: observe` wrapper gives a shadow-mode rollout.

**Matched pair caveat:** the two flags must both be on to be coherent. Mint on + resolve
off → outbound gets an `abph_` subject → fail-closed deny (safe, visible). Resolve on +
mint off → no `abph_` tokens appear → no-op. We can't express "requires token-exchange
with resolve on" via the `Requires` relationship (that's by plugin *name*, not config
*state*), so v1 relies on the fail-closed deny + documentation rather than new
build-time cross-validation.

## Data flow

**Happy path**

1. User → reverseproxy with `Authorization: Bearer T`.
2. jwt-validation validates T → `pctx.Identity`. Mint: `H = "abph_"+CSPRNG`,
   `pctx.Shared.Put("placeholder/"+H, T, placeholder_ttl)`, swap header to `Bearer H`,
   record `Modify{reason:"placeholder_minted"}` (hash only, never cleartext).
3. reverseproxy copies the mutated `Authorization` back to `r.Header`, forwards to agent.
4. Agent holds only `Bearer H`; makes an outbound call forwarding `Bearer H`.
5. token-exchange: on a **matched route**, prefix `abph_` + `Get` → T; exchange T→DT;
   set `Bearer DT`.
6. forwardproxy forwards `Bearer DT` upstream. Agent never saw T or DT.

**Branches**

| # | Condition | Behavior |
|---|-----------|----------|
| A | T invalid | jwt-validation denies (existing); no mint |
| B | No inbound token, mint on | nothing to mint; existing no-token handling |
| C | H sent to unmatched host (incl. evil.com) | route miss → no resolve/exchange → `Bearer H` passes through; opaque/useless off-box, **T never leaks** |
| D | H sent to matched route, lookup miss (expired/forged/restart) | **deny, fail-closed**, `Deny{reason:"placeholder_unresolved"}`; never exchange the opaque string |
| E | `pctx.Shared == nil` (store not wired) | mint: fail fast at Init (deploy error); resolve + prefix: deny |
| F | Sidecar restart between mint & resolve | in-memory lost → branch D; user retries → new mint (documented v1 limitation) |
| G | Multiple outbound calls, one inbound | handle is **multi-use** until TTL; each matched call resolves independently |
| H | Resolve on, normal token (no `abph_`) | skip resolve, normal exchange — backward compatible |
| I | CONNECT / end-to-end TLS egress | placeholder unreachable inside TLS → **out of scope**, documented |
| J | Response path | no-op (`token-exchange.OnResponse` already no-op) |

**The route-gating invariant:** the `H → T` lookup MUST be gated by the same route match
as the exchange — never resolve before confirming a matched route. Otherwise the real
token could end up in a header bound for an unmatched host.

## Listener / deployment compatibility

The design holds across listeners **provided inbound mint and outbound resolve run in
the same process** (so the in-memory store bridges them). Each listener also needs the
inbound header-propagation change above.

| Deployment | Mint + resolve same process? | Works with in-memory store? |
|------------|------------------------------|------------------------------|
| `authbridge-proxy` (reverseproxy + forwardproxy sidecar) | Yes — both built in one `main()` | ✅ |
| `authbridge-envoy` (extproc), per-pod sidecar, 1 replica | Yes — one `extproc.Server` holds both pipelines (`main.go:301-304`) | ✅ |
| extproc/extauthz scaled to >1 replica | No — mint and resolve can hit different replicas | ❌ needs external store |
| Istio **ambient waypoint** (shared/scaled) | Often no — inbound-to-agent and egress-from-agent may be enforced by different waypoint instances | ❌ needs external store |

So Envoy mode is supported in the **single-process** case (per-pod extproc/extauthz, one
replica); the shared/scaled waypoint case is the external-store follow-on (see Out of
scope). The plugin and store code is identical across all of them — only the per-listener
inbound-propagation idiom and the store backend differ.

## Security properties

- The agent never receives T; T never persists to disk (in-memory store).
- H is unguessable (CSPRNG ≥256-bit) and meaningless off-box — only resolvable in the
  minting sidecar's store.
- Logs/records never emit T or H in cleartext (hash/prefix only).
- Redemption is bounded by token-exchange's existing routes + Keycloak exchange policy +
  per-destination output scoping. A leaked handle cannot be redeemed off-route.
- Fail-closed on every resolve failure (branches D, E, F).

## Out of scope (v1) / future

- External/shared store for any deployment where mint and resolve can land in
  **different processes** — i.e. multiple authbridge replicas (HA) or a shared waypoint
  scaled past one replica. The in-memory store is correct only when both the inbound
  mint and the outbound resolve run in the same process (the single-replica sidecar, and
  single-replica extproc/extauthz). Swap in an external store behind the same
  `SharedStore` interface when that no longer holds.
- Per-session least-privilege redemption (handle bound to the token's scopes / an
  allowlist) — add only when a "subset of configured upstreams per session" requirement
  appears.
- CONNECT / end-to-end-TLS egress.
- Build-time cross-validation of the mint/resolve flag pair.

## Testing

- `shared.Store` unit tests: `Put`/`Get`/`Delete`, TTL expiry with injected clock,
  concurrent access under `-race`.
- jwt-validation mint: success → header swapped to `abph_`, store entry keyed by handle
  with T and `ttl≈placeholder_ttl`; validation failure → no mint, deny; no inbound token → no mint;
  `nil` Shared → Init/Configure error.
- **Inbound propagation** (riskiest change, per listener): inbound plugin sets
  `pctx.Headers` Authorization → assert it reaches the agent. reverseproxy: forwarded
  `r.Header` carries it. extproc: `handleInbound`/`handleInboundBody` emit a
  `replaceTokenResponse` HeaderMutation. extauthz: inbound `OkResponse` carries the
  request-header injection.
- token-exchange resolve: seeded store → matched route resolves + exchanges; unmatched
  route passes through; lookup miss denies; non-placeholder bearer → normal exchange
  (regression).
- One end-to-end test sharing a real store: agent sees only H, upstream sees DT.

## Files touched

| File | Change |
|------|--------|
| `authlib/shared/store.go` (new) | generic process-scoped TTL store |
| `authlib/pipeline/context.go` | add `SharedStore` interface + `Context.Shared` field |
| `authlib/listener/reverseproxy/server.go` | inbound Authorization propagation (copy `pctx.Headers` → `r.Header`); accept + set shared store |
| `authlib/listener/forwardproxy/server.go` | accept + set shared store on pctx (outbound resolve already propagates) |
| `authlib/listener/extproc/server.go` | inbound Authorization propagation in `handleInbound`/`handleInboundBody` (emit `replaceTokenResponse` on change); accept + set shared store on both pctx |
| `authlib/listener/extauthz/server.go` | ⛔ **deferred (not in v1)** — future inbound request-header injection in `Check` `OkResponse` |
| `authlib/plugins/jwtvalidation/plugin.go` | `placeholder_mode` / `placeholder_ttl`; mint logic |
| `authlib/plugins/tokenexchange/plugin.go` | `resolve_placeholders`; route-gated resolve step |
| `cmd/authbridge-proxy/main.go`, `cmd/authbridge-lite/main.go`, `cmd/authbridge-envoy/main.go` | create store, inject into listeners, `Close()` on shutdown |
| docs | plugin-reference updates for the new mode |

## Attribution

Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>
