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
HTTP: when the request's scheme is `http` and the resolved host has a
credential, the plugin denies with `403 plaintext_credential` before
any header is written, so nothing is injected and nothing is sent
upstream. The code is the MOCA egress contract's identifier verbatim
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
