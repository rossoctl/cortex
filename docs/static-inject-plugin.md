# `static-inject`

An outbound plugin (`core/plugins/staticinject`, registered name
`static-inject`) that swaps a placeholder credential on the outbound
`Authorization` header for a real static credential, so a
model-influenced workload never holds the real secret. Unlike
[Credential placeholder swap](token-exchange-plugin.md#credential-placeholder-swap) —
which mints and resolves a per-request `abph_` handle for a real user
token across two cooperating plugins — `static-inject` is a single
self-contained plugin that injects one operator-provisioned static
credential (an API key, a shared service credential) keyed by
destination host or a fixed key. The two mechanisms solve different
problems and do not interact.

The workload authenticates outbound calls with an agreed-upon
placeholder string; `static-inject` verifies that placeholder (if
configured) and replaces it with the real credential resolved from
either a secret-mounted file or an inline map (tests/dev only). The
real credential is never present in the workload's own config or
environment.

### Config

| Field | Type | Required | Default | Notes |
|---|---|---|---|---|
| `source` | string | yes | — | `secret_dir` or `mappings`. |
| `secret_dir` | string | when `source: secret_dir` | — | Directory containing one file per credential key; read via the same whitespace-trimmed `config.ReadCredentialFile` convention other plugins use for file-sourced values (see [File-sourced values](plugin-reference.md#file-sourced-values)). |
| `mappings` | map[string]string | when `source: mappings` | — | Inline key→credential map. Tests/dev only — do not put real secrets in YAML. |
| `key_by` | string | no | `host` | `host` (resolve by the outbound request's destination host) or `static` (always use `key`). |
| `key` | string | when `key_by: static` | — | The single lookup key used when `key_by` is `static`. |
| `placeholder` | string | no | — | When set, injection only proceeds if the inbound bearer equals this exact string. |

### Fail-closed

`static-inject` denies (`401`, `static-inject.<reason>`) and leaves the
`Authorization` header unchanged whenever any step of the swap can't be
completed cleanly:

- missing or unparsable bearer token (`static-inject.missing-auth`);
- `placeholder` is set and the inbound bearer doesn't match
  (`static-inject.placeholder-mismatch`);
- the resolved key has no credential in the configured source
  (`static-inject.unresolved-key`);
- the resolved value would be unsafe to place in a header — contains
  `\r`, `\n`, or `\x00` — a CWE-113 header-splitting guard
  (`static-inject.unsafe-value`);
- the plugin was never successfully `Configure`d
  (`static-inject.unconfigured`).

There is no partial-injection path: a denied request never forwards
the placeholder, and a successful swap only ever writes the fully
resolved real credential.

### Plaintext refusal (`403 plaintext_credential`)

A credential that is about to be injected is never sent over plain
HTTP: when the request's scheme is `http` and resolution reached a
credential — the bearer and placeholder checks above passed — the
plugin denies with `403 plaintext_credential` before any header is
written, so nothing is injected and nothing is sent upstream. A
plaintext request that fails an earlier check keeps that check's 401;
either way nothing is sent. The code is the MOCA egress contract's identifier verbatim
(MI1 §8.5) rather than the usual `static-inject.<reason>` form, so the
conformance suite Cortex shares with `moca-egress` matches it without
a translation table.

The refusal names the credential, not the scheme: plain HTTP to a host
*without* a credential is not refused here — it keeps its normal
fail-closed treatment above, and allow-list/SSRF policy judges the
request as it would any other. An empty scheme (the listener couldn't
determine one) is likewise not treated as plaintext. The check runs
after resolution on purpose: only the resolver knows whether this host
has a credential, which is why the gate lives in the injector rather
than in a pre-pipeline check.

**Outbound only.** The gate fires on outbound requests — the forward
proxy's egress, which is what the contract is about. It deliberately
does not fire on an inbound (reverse-proxy) request, because there
`pctx.Scheme` describes the **wrong hop**: it is the scheme the client
used to reach Cortex, `http` for any non-TLS inbound listener, while
the credential leaves on the Cortex→backend hop whose scheme comes
from `reverse_proxy_backend` and is not visible to the plugin. An
inbound static-inject behind a plaintext listener in front of an
`https://` backend is a supported shape — MOCA's AB1 is exactly that —
and refusing it would block every request while protecting nothing.

Covering the inbound hop properly would mean teaching the reverse
proxy to expose its backend scheme; that is not implemented. Until it
is, an operator running static-inject inbound is responsible for
pointing `reverse_proxy_backend` at an `https://` backend (or one on a
trusted network). That is the hop the credential actually travels on:
an inbound static-inject in front of a plaintext backend still injects
the real key, which then crosses that hop in cleartext. Cortex does
not refuse an `http://` backend, and `mtls:` on the front listener
does not change it.

### Deployments this refuses

The refusal is the intended contract, but it is a behaviour change for
anything that was relying on plaintext egress with a credential:

| Shape | Effect |
|---|---|
| Forward-proxy egress, `http://` target **with** a credential | **Now 403.** The request must move to `https://`, or the host must stop having a credential. |
| Forward-proxy egress, `http://` target without a credential | Unchanged (401 `unresolved-key`, as before). |
| Forward-proxy egress, `http://` target with a credential, under `mtls: mode: strict` | **Now 403**, even though the hop is TLS on the wire. Strict mode dials every upstream TLS-or-fail (`mtlsDialer`), but the gate reads the request's scheme, not the dialer, so it cannot see that. Fail-closed, and rare (static-inject plus strict mesh mTLS); move the target to `https://` to inject. |
| Inbound static-inject, plaintext front, any backend | Unchanged — the gate does not fire inbound (above). |

Known downstream: **MOCA's AB2 leaf smoke** (`deploy/knative/leaf-smoke.sh`,
the `H2-inject` allow-path claim) sends
`curl -s http://echo-target/ -H "Authorization: Bearer $ECHO_CRED"`
through the forward proxy to a host that has a credential. That request
is refused by this gate. AB2 is pinned to an image predating it, so
nothing breaks today, but the next image bump fails that check unless
`echo-target` moves to TLS or the smoke is updated to expect the 403.

One boundary worth naming: the gate judges the scheme the request
*arrived* with. A `WritesDestination` plugin later in the chain can
`Redirect` to an `http://` target (an operator-pinned server), and the
listener applies the redirect after the pipeline has run, headers
already injected. The shipped inference-router clears that path
itself, dropping credential-sounding headers and overwriting
`Authorization` / `X-Api-Key` with the routed server's own key — so
what can survive is a custom `inject_header` whose name avoids every
credential word. If static-inject shares a pipeline with a redirecting
plugin and a plaintext pin, give the injected header a name containing
one of those words so the router's drop covers it.
