# `token-exchange-aiac` plugin

`token-exchange-aiac` is an outbound token-exchange plugin that needs **no
routes**. It works out a target audience from the destination host, and lets
Keycloak decide whether that destination is a target:

- Keycloak issues a token → the request goes out carrying it.
- Keycloak has no client by that name → the request passes through unchanged.
- Any other answer → the request is denied.

Adding a target is then a Keycloak change only. No `authproxy-routes`
ConfigMap, no `routes.yaml`, no restart:

```yaml
# Not needed with token-exchange-aiac
data:
  routes.yaml: |
    - host: "github-tool-mcp"
      target_audience: "github-tool"
      token_scopes: "openid github-tool-aud github-full-access"
```

It is an **alternative** to [`token-exchange`](./plugin-catalog.md#token-exchange),
not a complement. Both rewrite the outbound `Authorization` header, so put
one or the other on a chain, never both. The two share no exchange code, so a
change to one can never quietly change the other. They read the bearer the same
way, though: both use `core/auth` to take it from `Authorization`, both name the
no-token policies `allow`, `deny` and `client-credentials`, and both default to
`deny`. See [Differences from `token-exchange`](#differences-from-token-exchange).

## What happens to each request

| Destination | Keycloak's answer | Result | Session event |
|---|---|---|---|
| Host outside the cluster (`api.openai.com`, an IP, `github-tool.evil.example`) | not asked | forwarded unchanged | `skip / host_not_in_cluster` |
| Host in another namespace, under a template without `{namespace}`; its name is no client's | asked with the workload's own token: "Audience not found" | forwarded unchanged; remembered for `unknown_audience_ttl` | `skip / audience_unknown` |
| Host in another namespace, under a template without `{namespace}`; its name is a client's | asked with the workload's own token: any answer naming a client | **403**, never exchanged | `deny / foreign_namespace` |
| In-cluster, audience granted to this workload | 200, token for that audience | `Authorization` replaced | `modify / token_exchanged` (or `cache_hit`) |
| In-cluster, no Keycloak client by that name | 400 `invalid_client` "Audience not found" | forwarded unchanged; remembered for `unknown_audience_ttl` | `skip / audience_unknown` |
| In-cluster, client exists but is **not granted** to this workload | 400 `invalid_request` "Requested audience not available" | **403** | `deny / audience_not_granted` |
| In-cluster, the destination's client is **disabled** | 400 `invalid_client` "Client disabled" | **403** | `deny / target_disabled` |
| In-cluster, bearer Keycloak rejects (expired, bad signature, an API key it never issued) | 400 `invalid_request` "Invalid token"; asked again with the workload's own token | that second answer decides: unknown → forwarded unchanged, bearer intact; a granted target → **401**; not granted or disabled → **403**; anything else → **503** | `skip / audience_unknown`, `deny / subject_invalid`, `deny / audience_not_granted`, `deny / target_disabled` or `deny / exchange_failed` |
| Keycloak down, timeout, 5xx, bad credentials, any other error | — | **503** | `deny / exchange_failed` |
| Credentials not yet loaded from `/shared/` | not asked | **503** | `deny / credentials_not_ready` |
| Any host, bearer is a placeholder that can't be resolved (`resolve_placeholders: true`) | not asked | **401** | `deny / placeholder_unresolved` |

Two rules hold throughout:

- **Passthrough is caused by exactly one answer:** "Audience not found". Every
  other failure, including an outage, denies. If a Keycloak upgrade rewords that
  message, unknown destinations start being denied; nothing is ever wrongly
  passed through.
- **A rejected bearer is not a verdict on the destination.** Keycloak checks the
  subject token before the audience, so it answers "Invalid token" for every
  audience alike. The plugin then asks again with the workload's own token,
  which Keycloak accepts, and the destination is judged by that answer. An
  in-cluster model gateway called with its own API key therefore passes through,
  and an expired user token is a 401 at a target only. If the workload's own
  token is rejected too, the request is a 503.
- **The token sent to X is always a token for X.** The plugin checks the `aud`
  claim of every token Keycloak issues and denies if the requested audience is
  missing. It never sends an exchange without an audience: Keycloak answers that
  with the caller's broadest token.

Agents and tools are handled the same way. To Keycloak, an agent you call is
just another client, and the AuthBridge sidecar in front of it validates the
token's audience exactly as a tool's does.

## Which hosts are offered to Keycloak

The audience comes from the Host header, and the workload chooses the host,
possibly because a prompt told it to. If every host were eligible, a request to
`github-tool.evil.example` would ask for the `github-tool` audience, get a real
token, and send it to `evil.example`. So only names that Kubernetes DNS resolves
inside the cluster are eligible:

| Host form | Example | Namespace taken from |
|---|---|---|
| short name | `github-tool`, `github-tool:9090` | the `namespace` setting |
| `service.namespace.svc` | `github-tool.team1.svc` | the host (see below) |
| FQDN | `github-tool.team1.svc.cluster.local` | the host (see below) |

When `audience_template` has no `{namespace}` (the default `{service}` has
none), the audience cannot tell `github-tool.team1.svc` from
`github-tool.attacker.svc`, so a tenant who creates a Service named after a
target in their own namespace would receive that target's token. Under such a
template, a host that names any namespace but this workload's own — the
namespace a short name resolves to — is **never exchanged**, whatever its
bearer. Instead the plugin asks Keycloak, with the workload's own token, whether
the derived audience names a client:

- **No client by that name:** the request is forwarded unchanged, like any
  destination Keycloak doesn't know. A model gateway in another namespace,
  called with its own API key, keeps working.
- **A client by that name** (granted, not granted or disabled alike): **403**
  `deny / foreign_namespace`. The host is claiming a target's name, so it gets
  neither that target's token nor the caller's own bearer.

When this workload's namespace is not known, every namespace counts as another
one. To exchange for targets in other namespaces, use a template with
`{namespace}`, which puts the namespace into the audience; nothing is then
foreign. Operator-registered workloads are already named that way (see
[Choosing `audience_template`](#choosing-audience_template)).

`github-tool.team1` is **not** eligible. Kubernetes resolves it, but it has the
same shape as `example.com`. IP literals, extra labels, a different cluster
domain, and anything that is not an RFC 1123 label are refused too. A refused
host is forwarded unchanged and never reaches Keycloak.

Within this workload's own namespace, anyone who can create a Service can
still pick a name that matches a Keycloak client. That is the trust a caller
already places in its own namespace's DNS when it dials a short name.

## Configuration

```yaml
# authbridge-runtime-config: pipeline.outbound.plugins
pipeline:
  outbound:
    plugins:
      - name: token-exchange-aiac
        config:
          keycloak_url: "${KEYCLOAK_URL}"
          keycloak_realm: "${KEYCLOAK_REALM}"
          # Everything below is optional; these are the defaults.
          client_id_file: /shared/client-id.txt
          client_secret_file: /shared/client-secret.txt
          audience_template: "{service}"
          cluster_domain: cluster.local
          no_token_policy: deny
          unknown_audience_ttl: 5m
          timeout: 5s
          resolve_placeholders: false
```

| Field | Default | Meaning |
|---|---|---|
| `token_url` | derived | Token endpoint. Required unless `keycloak_url` and `keycloak_realm` are set. |
| `keycloak_url`, `keycloak_realm` | — | Keycloak's **internal** service URL and realm; `token_url` becomes `<url>/realms/<realm>/protocol/openid-connect/token`. |
| `client_id` / `client_id_file` | `/shared/client-id.txt` | This workload's client ID. The inline value wins. |
| `client_secret` / `client_secret_file` | `/shared/client-secret.txt` | This workload's client secret. The inline value wins. |
| `audience_template` | `{service}` | Audience requested for a destination. Placeholders: `{service}`, `{namespace}`. Must contain `{service}`. |
| `namespace` | the pod's own | This workload's namespace; must be a DNS label. It is assumed for short names, and under a template without `{namespace}` it is the only namespace a host may name and be exchanged for. Under a template with `{namespace}`, short names without one pass through with `skip / namespace_unknown`. |
| `namespace_file` | see meaning | Read once at start when `namespace` is empty. An absent or blank file, or one that is not a DNS label, refuses at start. When neither field is set, the plugin reads `/var/run/secrets/kubernetes.io/serviceaccount/namespace` if it exists. The kubelet projects that file from the pod's own metadata, so it is right in every copy of a config shared across namespaces. Outside a pod (a laptop) there is no file, and no namespace. |
| `cluster_domain` | `cluster.local` | Cluster DNS domain, for recognising FQDNs. |
| `no_token_policy` | `deny` | The request carries no bearer token: `deny` answers 401, `allow` forwards it unchanged, `client-credentials` exchanges the workload's own token (see below). The same values and default as `token-exchange`. |
| `unknown_audience_ttl` | `5m` | How long a destination Keycloak doesn't know is passed through without asking again. `0` asks on every request. |
| `timeout` | `5s` | Timeout for each call to the token endpoint. |
| `resolve_placeholders` | `false` | Resolve a placeholder bearer from the shared store before the exchange. See [Placeholder bearers](#placeholder-bearers). |

There is no `routes`, `token_scopes`, `default_policy` or `audience_from_host`
field. Configure rejects unknown fields, so a `token-exchange` config pasted in
fails at load instead of half-working.

The credential files work as they do for `token-exchange`. The plugin reads them
when it is configured, and if they aren't there yet it waits for them in the
background, holding the pod not-ready until they land. Plugin config
hot-reloads, so editing it needs no restart.

### Choosing `audience_template`

The audience must be the target's Keycloak **client ID**, which is also what the
target's own sidecar validates as `aud`.

- **Clients named after their Service** (`github-tool` serving at
  `github-tool`): use the default, `{service}`. Targets must then be in the
  caller's own namespace; see [Which hosts are offered to
  Keycloak](#which-hosts-are-offered-to-keycloak).
- **Operator-registered workloads**, whose client ID is a SPIFFE ID:
  `spiffe://<trust-domain>/ns/{namespace}/sa/{service}`. Short names take the
  pod's own namespace, read from its service-account mount by default. This
  form also reaches targets in other namespaces.

The SPIFFE form only works where the **service account has the same name as the
Service**. The Host header carries the Service name, never the service account.
The weather-agent advanced demo breaks this: Service `weather-tool-advanced-mcp`,
service account `weather-tool-advanced`. For such a target the derived audience
names no client, so the request passes through and Keycloak never issues a token
for it. Rename one of the two, or keep `token-exchange` for that target.

## Keycloak requirements

All four are needed. Each one, when missing, breaks the plugin in a different
way.

1. **Standard token exchange enabled on the caller's client.** Set
   `standard.token.exchange.enabled=true` (Keycloak 26.2+). Without it the
   legacy provider answers, and every granted target gets 403 "Client not
   allowed to exchange". Every request to a granted target is then denied.
2. **One client per target**, named exactly as `audience_template` renders it.
   Agents count as targets.
3. **Grant a target with a DEFAULT client scope on the caller's client**: a
   client scope holding an audience mapper for the target, attached to the
   caller as a *default* scope. The plugin sends no `scope` parameter, so
   *optional* scopes are never applied, and a target granted only through one
   looks not granted (403).
4. **Not a realm default scope.** A realm default reaches every client in the
   realm, so every caller is granted every target. "Not granted" then never
   happens, and each target's grant list means nothing. The github-issue
   demo's `agent-team1-git-issue-agent-aud` is set up this way; don't copy it.

Turning off the legacy `token-exchange` preview feature on the server is
recommended. It removes any doubt about which provider is answering.

### What "default scopes" costs

A token issued to the caller **without** an audience carries every target it is
granted, so it is accepted by all of them:
- a `client_credentials` token;
- an exchange with no audience;
- a user logging in directly through the agent's client.

The plugin never forwards such a token. An exchange is always narrowed to one
audience, and with `no_token_policy: client-credentials` the workload's own
token is used only as the subject of a second, narrowing exchange. Anything
*else* that obtains a token for that client gets the broad one.

The `scope` claim of a narrowed token still lists **all** the caller's scope
names, even though `aud` holds one. So each target can see which other targets
the caller can reach, and scope-based authorization at a target (e.g. a
`github-full-access` scope) no longer differs from caller to caller.

## No bearer token on the request

The bearer is taken from `Authorization` exactly as `token-exchange` takes it
(`auth.ExtractBearer`): a case-insensitive `Bearer ` prefix, and everything after
it, spaces included. When there is none, `no_token_policy` decides:

| `no_token_policy` | Result | Session event |
|---|---|---|
| `deny` (default) | a target: **401** `auth.unauthorized`, "missing Authorization header" (a host in another namespace is judged first: see below) | `deny / no_token` |
| `allow` | forwarded unchanged, with no `Authorization` | `skip / no_token` |
| `client-credentials` | exchanged with the workload's own token as the subject | `modify / token_exchanged` |

A host outside the cluster, or a destination remembered as unknown, passes
through before the policy is consulted. `allow` then needs nothing else: it
asks Keycloak nothing and does not wait for the credential files.

`deny` denies a **target** only, so it has to find out whether the destination
is one. It makes the same exchange `client-credentials` does, with the
workload's own token as the subject, and uses only the answer: a token, or a
cached one, means 401; "Audience not found" passes the request through without
an `Authorization` header, as it would with a bearer; any other answer is the
403 or 503 of the table at the top. The issued token is never forwarded.

A host in another namespace, under a template without `{namespace}`, is judged
before `no_token_policy` is looked at, except under `allow`, which asks
Keycloak nothing. Under `deny` and `client-credentials` alike, such a host whose
name is a client's is a **403** `deny / foreign_namespace`, not a 401 or an
exchange; see [Which hosts are offered to
Keycloak](#which-hosts-are-offered-to-keycloak).

With `no_token_policy: client-credentials`, the plugin gets the
workload's own token with `client_credentials` (cached until shortly before it
expires) and exchanges it for the destination's audience. That second step is
required, not an optimisation: Keycloak ignores `audience` on
`client_credentials` and returns a token for every granted target.

Discovery works the same way without a user token. An unknown destination still
passes through, and the workload's own token is never attached to it.

This is where `token-exchange-aiac` deliberately differs from `token-exchange`,
whose `client-credentials` sends the `client_credentials` token itself. Here
the second step is what makes discovery work: only an exchange tells "granted",
"not granted" and "no such client" apart.

## Placeholder bearers

With `resolve_placeholders: true`, a bearer carrying the placeholder prefix is
looked up in the shared store first, before the destination is looked at, just
as `token-exchange` does:

- A handle that resolves: the real token becomes the exchange subject.
- A handle that doesn't (no store, unknown or expired handle): **401**
  `auth.unauthorized`, `deny / placeholder_unresolved`, whatever the
  destination. Keycloak is not asked.
- A request that passes through (outside the cluster, unknown audience, not
  eligible) keeps the placeholder. The real token only ever goes to Keycloak.

With `resolve_placeholders: false` (the default), a placeholder is an ordinary
bearer and is sent to Keycloak as the subject, which rejects it.

## Caching and timing

- **Issued tokens** are cached per (subject token, audience) until 30s before
  they expire. The cache key is a SHA-256 of the subject, so the cache holds no
  usable copy of a caller's token.
- **Unknown destinations** are remembered for `unknown_audience_ttl`. Granting a
  client that already exists takes effect on the next request. A client
  *created* for a destination already remembered as unknown takes effect within
  the TTL.
- Each cache holds at most 10,000 entries. Revoking a grant does not invalidate a
  token already issued; it lives until it expires.

## Policy input (opa)

An exchanged request (`token_exchanged` or `cache_hit`) also appends one hop
to the delegation chain, so an `opa` plugin placed **after** `token-exchange-aiac`
on the outbound chain sees `input.delegation` and an outbound `input.identity`
in the same shape `token-exchange` produces:

| Field | Value |
|---|---|
| `identity.subject` | `sub` of the issued token (the delegating user, or the workload's service account under `no_token_policy: client-credentials`) |
| `identity.client_id` | this workload's own client ID |
| `identity.scopes` | `scope` of the issued token |
| `identity.service_id` | the target audience |

That table holds when the request carries no validated identity, which is the
usual case outbound. When it does carry one (`pctx.Identity`, set by a
validating plugin earlier on the chain), `opa` builds `input.identity` from it
instead: `subject`, `client_id` and `scopes` are that identity's, and
`service_id` is absent. `input.delegation` is unaffected either way, and its
hop still records the issued token's scopes; the hop's `subject_id` is then
that identity's subject.
| `delegation.chain[].strategy` | `"token-exchange-aiac"` |

Subject and scopes are read from the token Keycloak just issued, not from the
caller's bearer, so the workload cannot choose them. A request that is not
exchanged (passed through, not eligible, or denied) records no hop and carries
no outbound `identity`, so a policy keyed on it falls to its default.

## Observability

Every request records one invocation in the session stream. The reasons are in
the table at the top; `details` carries `host`, `target_audience`, and
`cache_hit` or `cached`. `GET /v1/pipeline` shows these counters, which reset on
restart and on config reload:

| Metric | Counts |
|---|---|
| `token-exchange-aiac.exchanged` | tokens Keycloak issued |
| `token-exchange-aiac.cache_hits` | requests served from the token cache |
| `token-exchange-aiac.unknown_passthrough` | requests to destinations Keycloak has no client for |
| `token-exchange-aiac.not_eligible` | requests to hosts never offered to Keycloak |
| `token-exchange-aiac.denied` | requests denied, for any reason |

The WARN log for a failed exchange includes Keycloak's error. The 503 body sent
to the workload deliberately doesn't, because that error can name other
audiences the caller holds.

## Limitations

- **Keycloak only, in practice.** The two answers that mean "unknown" and "not
  granted" are Keycloak's error texts, confirmed against Keycloak 26.5. Against
  another IdP, every failure is a deny, so no destination is ever
  discovered as unknown.
- **Client-secret identity only.** No SPIFFE JWT-SVID client assertion yet.
- **No scopes.** Grants have to be default client scopes, as described above.
- **Plaintext HTTP only**, like every outbound plugin. An opaque HTTPS tunnel
  (a `CONNECT`, or a transparently redirected connection) is neither exchanged
  nor checked: the plugin records `skip / opaque_tunnel` and asks Keycloak
  nothing.
- **The Host header is trusted to name the destination.** The forward proxy
  dials the host it was given, so there the two cannot differ. Under
  `cortex-envoy` the request goes to the address the workload dialed, so a
  workload that sends `Host: github-tool` to another address gets a
  `github-tool` token sent there. `token-exchange`'s routes trust the Host
  header the same way.
- **The service account and the Service must share a name** for SPIFFE-ID
  audiences. See [Choosing `audience_template`](#choosing-audience_template).

## Differences from `token-exchange`

On the bearer, the two plugins behave the same: same extraction, same
`no_token_policy` values and default, same `resolve_placeholders`. These
differences remain on purpose:

| | `token-exchange` | `token-exchange-aiac` | Why |
|---|---|---|---|
| `client-credentials` with no bearer | sends the `client_credentials` token | exchanges it, narrowed to the destination | Only an exchange discovers whether the destination is a target. See [No bearer token on the request](#no-bearer-token-on-the-request). |
| `input.identity.subject` | `sub` of the incoming bearer, unverified | `sub` of the token Keycloak issued | The workload cannot choose it. |
| `input.identity.scopes` | the route's requested scopes | `scope` of the issued token | There are no routes. |
| No-bearer 401 code | `upstream.token-exchange-failed` | `auth.unauthorized` | No exchange failed; the caller sent no token. |
| Token cache key | the subject token | SHA-256 of the subject token | Internal; the cache holds no usable copy of a caller's token. |

## Moving the github-issue demo to it

1. In the agent's runtime config, replace the `token-exchange` entry with the
   `token-exchange-aiac` example above.
   - If the agent calls tools without a user token, set
     `no_token_policy: client-credentials`; the default, `deny`, answers 401.
2. Delete the `authproxy-routes` ConfigMap.
3. In Keycloak, enable standard token exchange on the agent's client.
   - Attach `github-tool-aud` as a **default** scope of the agent's client.
   - Remove it from the realm defaults if `setup_keycloak.py` put it there.
4. Check the target's client ID is the name the agent calls: the agent calls
   `github-tool-mcp`, and the client is `github-tool`. Either name the client
   `github-tool-mcp`, or rename the Service. (Under `token-exchange`, a route
   mapped one name to the other; nothing does that here.)
5. `github-full-access` cannot be requested any more. If the tool should grant it,
   make it a default scope of the agent's client too, accepting that every token
   the agent gets for any target now lists it.

## Verifying against a real Keycloak

The unit tests use a fake Keycloak that copies the probed answers. To check the
real error texts before upgrading Keycloak, run the live test. It creates a
throwaway realm through the admin API and deletes it afterwards:

```sh
cd core
TOKEN_EXCHANGE_AIAC_LIVE_KEYCLOAK=http://keycloak.localtest.me:8080 \
  go test -run TestLiveKeycloak -v ./plugins/tokenexchangeaiac/
```

It logs in as `admin`/`admin` unless `TOKEN_EXCHANGE_AIAC_LIVE_ADMIN` and
`TOKEN_EXCHANGE_AIAC_LIVE_PASSWORD` say otherwise. It is skipped when the variable
is unset, so CI never runs it.
