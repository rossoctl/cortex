# AuthBridge Egress TLS Bridge — Design

**Date:** 2026-06-12 (revised after adversarial review; renamed 2026-06-17)
**Status:** Converged design — ready for implementation plan.
**Repos touched:** `kagenti-extensions/AuthBridge` (proxy), `kagenti-operator` (CA coordination)

> **Terminology:** this feature was originally called "MITM". It is now the
> **TLS bridge** — terminate one TLS connection, run the pipeline on plaintext,
> originate a fresh verified TLS connection (literally a bridge between two TLS
> connections); package `tlsbridge`, config block `tls_bridge:`. The body below
> still uses "MITM" in places as a synonym for the same terminate-and-re-originate
> mechanism. The implementation plan is
> `plans/2026-06-17-authbridge-tlsbridge-phase1.md`.

## Problem

AuthBridge's default Go proxy (`proxy-sidecar` mode) **never decrypts TLS**. For HTTPS it blind-tunnels:
`io.Copy` after CONNECT (`authlib/listener/forwardproxy/server.go`) or transparent intercept
(`authlib/listener/forwardproxy/transparent.go`). Every content plugin (token-exchange, A2A/MCP parsing,
inference parsing, IBAC, guardrails) runs **only on plaintext** (the pipeline `Run` in `forwardproxy`). So
wherever the agent speaks TLS to an origin and nothing else terminates it (standalone, or an OpenShell
sandbox with OpenShell's own proxy suppressed), AuthBridge's headline features silently do nothing on
HTTPS egress.

This design adds **MITM (TLS-terminating interception)** to the Go proxy so the **existing outbound
pipeline sees the decrypted request, unchanged**. MITM = *forging an identity*: minting a leaf to
impersonate the remote origin so the agent's TLS terminates at AuthBridge. That only applies to
**agent-initiated outbound HTTPS**.

## Goals / Non-goals

**Goals**
- Decrypt agent **outbound** HTTPS (transparent + CONNECT paths) and feed the **existing, unchanged**
  outbound pipeline. No plugin changes — plugins already self-gate (route-matched, body-matched, opt-in).
- General AuthBridge capability (mesh / standalone / sandbox), **validated first** in the OpenShell
  suppressed-proxy scenario where trust injection is easiest.
- Operator-coordinated trust so the agent trusts AuthBridge's CA with no manual steps, **on supported
  runtimes** (see Trust distribution).
- Never turn a currently-working agent call into a broken one: re-origination preserves the agent's trust
  semantics; un-MITMable traffic fails *open* (passthrough) and self-heals.

**Non-goals (this iteration)**
- **Inbound TLS termination** — that presents the agent's *own real* SVID (not forging) and is already the
  existing `mtls:` reverse-proxy block's job; not MITM.
- Replacing SPIRE mTLS for sidecar-to-sidecar (orthogonal).
- Forcing cert-pinned origins through inspection (they fail open to passthrough + auto-skip).

> **HTTP/2 is in scope, not deferred.** h1.1-only ALPN silently breaks h2-only origins (much of modern
> HTTPS). The Terminator must offer `h2` + `http/1.1` and serve h2 via `http2.ConfigureServer`. (An h1.1-only
> first cut may exist transiently during implementation, but h2 is a prerequisite for enabling MITM by
> default — it is not a fast-follow.)

## Protocol & port coverage (scope — what the bridge does NOT see)

The TLS bridge is an **HTTPS-inspection** mechanism, not a general egress-inspection guarantee. It yields
decrypted, pipeline-visible traffic only when **all** of these hold: the connection is **TCP**, on a
**configured bridge port** (default `{443, 8443}`), the first bytes are a **TLS ClientHello** record, and the
agent **trusts the forged CA**. Everything outside that envelope is either tunneled opaquely (still egresses;
the pipeline sees only `host:port` via the per-connection egress gate) or dropped. The categories with **no
content visibility**:

| Category | Examples | Why blind | Outcome |
|---|---|---|---|
| **Non-TLS TCP** | SSH (22); plaintext DB (Postgres 5432, MySQL 3306, Redis 6379, Mongo 27017); SMTP/IMAP/FTP/LDAP; raw/custom TCP; h2c | first bytes ≠ TLS ClientHello → `non-tls`; usually non-bridge port → `port` | tunneled |
| **TLS on non-standard ports** | LDAPS 636, SMTPS 465, IMAPS 993, AMQPS 5671, MQTTS 8883, DB-over-TLS, custom HTTPS on `:9443` etc. | port not in the bridge set → `port` (configurable) | tunneled |
| **STARTTLS** | SMTP/IMAP/POP3/LDAP/XMPP/Postgres plaintext→TLS upgrade | connection opens plaintext; the 5-byte peek isn't a ClientHello → `non-tls` | tunneled |
| **Non-TCP** | QUIC / HTTP-3 (UDP 443), DTLS, WireGuard/IPsec/VPN | `enforce-redirect` DROPs external non-TCP | **dropped** (forces TCP fallback for QUIC; a no-fallback client fails) |
| **Un-MITM-able TLS** | cert-pinned clients; client-cert/mTLS to the origin; ECH / encrypted-SNI | leaf rejected / upstream-verify fails / SNI unreadable → fail open | tunneled (auto-skip) |

**Why the port gate is load-bearing (not just perf).** The downstream serving layer (`ServeConn`) parses the
decrypted bytes as **HTTP/1.1 or h2**. TLS on `443`/`8443` is ~always HTTP(S); TLS on `636`/`465`/`5671`/DB
ports is **not** HTTP. Bridging those would terminate the connection and then fail to serve non-HTTP wire bytes
as HTTP → a **broken** agent connection (the upstream-verify `HEAD` probe usually rejects a non-HTTP origin and
makes us fall open, but that is a fragile net that also sprays `HEAD /` at non-HTTP services). The port gate
keeps interception **HTTP-only by operator intent**. To inspect HTTPS on an extra port, **widen the configured
port set** (`Decision.Ports` is arbitrary; expose a `ports:` config key) — do **not** bridge all ports.

**Non-goals (coverage):**
- **Non-HTTP protocols** (SSH, databases, SMTP/LDAP/AMQP/MQTT, raw TCP) — out of scope; inspecting/controlling
  them needs a protocol-aware proxy/bastion or, for *control without decryption*, connection-level egress
  policy.
- **STARTTLS upgrade detection** — out of scope (the first-byte heuristic intentionally does not track
  mid-connection upgrades).
- **envoy-sidecar bridging** — out of scope; it would need a separate Envoy-data-plane mechanism (dynamic
  per-SNI leaf issuance via SDS/filter + terminate/originate config), not this Go-proxy bridge. The operator
  rejects `tlsBridgeMode=enabled` with `envoy-sidecar` at admission. A future "Phase 3" if a real need arises.

**The backstop is connection-level egress policy.** Allow/deny by `host:port` *without* decryption, enforced at
the per-connection egress gate that already runs on every captured connection (it sees the destination even
when it cannot read the payload). That layer covers SSH, databases, STARTTLS, odd-port TLS, and pinned/mTLS
traffic, and composes with the bridge: the bridge decrypts what it can (HTTPS), policy gates the rest.

## Design decisions

| Topic | Decision |
|-------|----------|
| Direction | **Outbound only** (agent-initiated HTTPS). Inbound TLS is the existing `mtls:` concern, not MITM. |
| Plugins | **Unchanged.** MITM produces plaintext; the existing pipeline runs as-is. Plugins self-gate, so all stay enabled (token-exchange route-gated/passthrough-default; mcp/a2a parsers body-matched + observe-only; IBAC opt-in). The plugin layer is genuinely "no new code." |
| Interception scope | A **configurable host/CIDR gate** in `Decision`. Default: **external-egress only** (perf; avoids redundant in-cluster double-decrypt where a mesh already gives the receiving sidecar L7-able plaintext). Overridable to include in-cluster for **no-mesh / standalone / OpenShell**, where MITM is the *only* way to inspect in-cluster HTTPS. |
| Upstream verification | A **dedicated upstream `http.Client`** whose `RootCAs` = system roots **+** the agent's injected trust bundle. Re-origination NEVER uses the SPIRE-mTLS dialer and NEVER sets `InsecureSkipVerify`. |
| Reversibility | The MITM decision is **reversible until the upstream handshake succeeds**: verify upstream first, then mint+terminate downstream; otherwise tunnel the buffered ClientHello. Plus **auto-skip on handshake-fail** so a pinned client's retry passes through. |
| CA model | **Operator-provisioned per-agent CA via cert-manager** (default); per-sidecar **ephemeral** CA as standalone/no-cert-manager fallback. Same `CASource` interface. |
| Build vs adopt | **Hand-roll** on stdlib `crypto/tls`/`crypto/x509`; reference martian's cert logic, not its framework. |
| HTTP/2 | **In scope** (prerequisite for default-on). ALPN offers `h2` + `http/1.1`. |

## Architecture

### New package `authlib/mitm/`

| Unit | Responsibility | Depends on |
|------|----------------|------------|
| `CASource` (interface) | Supply the signing CA; expose `CACertPEM()`. Impls: `FileSource` (mounted cert-manager Secret — default), `EphemeralSource` (in-memory self-signed — fallback). | `crypto/x509` |
| `Minter` | Mint per-SNI leaf signed by the CA (SAN incl. IP literals for no-SNI/IP-dialed origins); LRU+TTL cache keyed by **SNI** (h1.1+h2 share). Exposes `GetCertificate(*tls.ClientHelloInfo)`. Leaf validity ≥ cache TTL. | `CASource` |
| `Terminator` | Wrap a sniffed client `net.Conn` as `tls.Server` with `GetCertificate=Minter`; ALPN `h2`,`http/1.1`. | `Minter` |
| `Decision` | `classify(host, port, firstBytes) → {terminate, passthrough}`: validates the 5-byte TLS record header (not just `0x16`), port gate, **internal/mesh host gate**, skip-list. | config |

### Plugin integration — no new code in the pipeline

Extract the core of `forwardproxy`'s request handler into a shared `serveOutbound(w, r)`: build `pctx`, run
`OutboundPipeline.Run/RunResponse/RunResponseFrame`, re-originate, write response. The plaintext
forward-proxy path and the MITM path both call it. MITM just produces decrypted `*http.Request`s; the
pipeline is identical. (This is a *refactor* of the proxy, not a plugin change.)

`serveOutbound` needs an `isMITM`/scheme discriminator: the MITM path must **not** strip proxy-only headers
(`Proxy-Authorization`/`Proxy-Connection`) and the request URL is origin-form, not absolute-form. After the
`Terminator` returns the decrypted conn, serve it with a one-connection `http.Server` over a one-conn
`net.Listener` so HTTP **keep-alive** (multiple requests on the kept-alive TLS conn) works.

### Upstream re-origination (the core correctness fix)

Re-origination uses a **dedicated upstream `http.Client`**, never the proxy's mesh-mTLS dialer:
- `Transport.TLSClientConfig.RootCAs` = `x509.SystemCertPool()` **+** the agent's injected upstream trust
  bundle (a configurable/mounted path; empty by default = system roots only, which covers public origins).
- No `InsecureSkipVerify`. This preserves the agent's trust semantics: an origin the agent would have
  trusted directly is still verified; a private-CA origin works iff its CA is in the injected bundle.
- The SPIRE-mTLS dialer (which presents the agent SVID + verifies against the SPIRE bundle with
  skip-verify) is for **mesh peers only** and is never used for MITM'd external origins.

### Why outbound is the only MITM surface

- **Outbound = forge identity:** mint a leaf per origin SNI; the agent must trust our CA. The only path that
  needs a minting CA.
- **Inbound ≠ MITM:** terminating inbound TLS presents the agent's *own real* SVID (no forging) — already
  the `mtls:` reverse-proxy block's job (mesh: ztunnel delivers plaintext; standalone: real SVID). No
  PREROUTING / `InboundPipeline` changes here.

## Data flow — outbound (agent → HTTPS origin)

```
1. Agent opens TLS (captured at transparent listener, or CONNECT on the forward proxy)
2. Peek ClientHello → SNI, ALPN, validate TLS record header (sniff.go peekedConn, replayable)
3. Decision.classify(host, port, recordHeader):
     port not in {443,8443,…}            → tunnel + log reason=port
     not a valid TLS ClientHello          → tunnel + log reason=non-tls
     host ∈ skip_hosts                    → tunnel + log reason=skip
     host internal/mesh & scope=external  → tunnel + log reason=in-cluster
     ECH / encrypted-SNI present          → tunnel + log reason=ech
     else                                 → candidate for MITM ↓
4. Dial + complete VERIFIED upstream handshake to the origin via the upstream client
     upstream verify FAILS  → tunnel the buffered ClientHello (blind passthrough) + log reason=upstream-verify
     upstream verify OK     ↓   (decision still reversible up to here)
5. Terminator: tls.Server(clientConn, {GetCertificate: Minter}); ALPN h2/http1.1
     agent rejects minted leaf (pinned) → record host→auto-skip; this call fails, retry passes through
     agent accepts                      ↓
6. one-conn http.Server → serveOutbound(w, r)  [Scheme=https, Host=SNI, isMITM=true]
     → OutboundPipeline.Run/RunResponse  (UNCHANGED — full plugin set)
     → relay over the already-verified upstream connection from step 4
     → response back through the terminated client TLS
```

The reversibility in steps 4–5 is what keeps "we tried to MITM" from breaking working calls: upstream is
proven before we forge anything; pinned clients self-heal on retry via auto-skip.

## Interception scope (config gate)

`Decision` takes an explicit scope: a set of host globs / CIDRs that are **internal** (don't MITM by
default) and the policy for them. Default policy = **external-egress only** — internal/mesh destinations
tunnel (the mesh + receiving sidecar already handle them, and MITM-ing them is a redundant double-decrypt).
Operators override to `all` for no-mesh / standalone / OpenShell, where MITM is the only L7 inspection
point. This is a *traffic* gate, never a plugin gate.

## Trust distribution (operator-coordinated)

**CA model: operator-provisioned per-agent CA via cert-manager** (the operator has no startup-ordering
primitive, so an ephemeral CA created after boot would race first egress; a mounted Secret gates pod start).

Flow:
1. Operator creates a per-agent CA `Certificate` (cert-manager) → Secret (`tls.crt`/`tls.key`/`ca.crt`).
   The CA carries **X.509 Name Constraints** scoping it to the agent's allowed egress hosts (so a per-agent
   CA can't mint for arbitrary origins).
2. Operator mounts the Secret as a **hard (`Optional:false`) volume** so the pod blocks until it exists:
   - into the **sidecar**: `tls.crt`+`tls.key` (signing) — mode **`0400`**, owned by the proxy UID. The
     signing key never goes to the agent container.
   - into the **agent**: `ca.crt` only (trust) + trust env (see below).
3. **Ordering** is a 3-actor liveness dependency (webhook mutates pod → reconciler creates the `Certificate`
   → cert-manager issues the Secret). The hard mount makes the pod wait; a *soft* mount would degrade to
   silent unverified egress and is disallowed.

**Trust injection is per-runtime — scope the promise.** Setting `NODE_EXTRA_CA_CERTS` / `SSL_CERT_FILE` /
`REQUESTS_CA_BUNDLE` / `CURL_CA_BUNDLE` / `GIT_SSL_CAINFO` / **`AWS_CA_BUNDLE`** /
**`GRPC_DEFAULT_SSL_ROOTS_FILE_PATH`** covers Python-requests, curl, git, Node, Go-on-Linux, boto3, and
gRPC. It is **silently ignored** by Go-on-macOS and `certifi.where()`-pinned / custom-`SSLContext` clients.
So: document **supported runtimes**; emit a **startup self-check** (probe whether the injected CA is
actually honored) so a trust-miss is a loud signal, not an opaque in-agent handshake error.

Pluggability: operator path = `FileSource`; standalone / no-cert-manager = `EphemeralSource` (writes CA cert
to a shared `emptyDir` consumed by an initContainer or baked trust). One-line `CASource` swap in `main.go`.

### Operator: reused vs net-new (verified against source)

*Reused:* sidecar injection webhook; agent **env** mutation (`pod_mutator.go`); per-agent `config.yaml`
generation (layering `mode`/`listener`/`mtls` blocks — `mitm` is the same pattern); `AuthBridgeMode`/
`MTLSMode` CRD home for a `MITMMode` field; cluster-wide Secrets CRUD.

*Net-new (do not assume reuse):*
- **Agent-side Secret mount.** Today the webhook only injects *env* into the agent; only the sidecar mounts
  `shared-data`. Mounting `ca.crt` into the agent is new plumbing and deliberately punctures agent/sidecar
  mount-namespace isolation.
- **Per-agent cert-manager reconciler.** The `SharedTrust` controller is a *hardcoded mesh-root shuttle*,
  not a per-agent template — a new reconciler is needed.
- **cert-manager write RBAC**, including **namespaced `Issuer` create** (today only `get;list;watch` on
  certs + `clusterissuers`).
- **CA rotation:** PoC reads `FileSource` once at start (restart to rotate; long-lived CA). Zero-downtime
  rotation via cert-manager overlapping `ca.crt` bundle + file-watch is a tracked follow-up.

## Fail-open / self-healing

`Decision` defaults to passthrough on anything it can't safely MITM; every passthrough is logged with a
reason (`port|non-tls|skip|in-cluster|ech|upstream-verify|handshake-fail`). Beyond the static `skip_hosts`:
- **upstream-verify**: handled *before* forging (step 4) → clean blind tunnel.
- **handshake-fail** (pinned client): the proxy **auto-adds the host to an in-memory skip set** on first
  failure, so the agent's retry passes through — no hand-edited config, no persistent outage.

## Security hardening (in scope)

- **CA signing key**: mode `0400`, dedicated UID, sidecar-only (never the agent volume); **Name
  Constraints** scope each per-agent CA to that agent's egress hosts → a leaked key is a keyring, not a
  cluster-wide skeleton key.
- **`:9094` session API** is unauthenticated and would now capture **decrypted HTTPS** bodies (tokens, PII).
  Treat "MITM on" and "unauthenticated raw-body store" as mutually exclusive: localhost-only bind +
  redact/disable raw-body capture when MITM is enabled.

## Config

A pointer block on `Config` mirroring the `MTLS`/`SPIFFE` idiom:

```yaml
mitm:
  enabled: true
  scope: external            # external | all   (which traffic to intercept)
  internal_cidrs: []         # treated as in-cluster when scope=external (else discovered)
  ca_source: file            # file | ephemeral
  ca_cert_path: /etc/authbridge/mitm-ca/tls.crt
  ca_key_path:  /etc/authbridge/mitm-ca/tls.key
  ca_export_path: /var/run/authbridge/mitm-ca.pem   # ephemeral mode only
  upstream_ca_bundle: ""     # extra roots for re-origination (agent's private CAs); empty = system roots
  skip_hosts: []             # static passthrough; auto-skip augments this at runtime
  leaf_cache: { max: 1024, ttl: 24h }
```

Read in `main.go` beside the `fpMTLS` block; construct `CASource` + `Minter` + `Terminator` + the upstream
client; pass into `forwardproxy.NewServer` and the transparent listener.

## Testing & success criteria

**Unit (`authlib/mitm/`):** CASource (ephemeral gen; file load + error paths); Minter (SAN incl. IP
literals, chain-to-CA, validity ≥ TTL, cache hit/TTL/LRU); Decision (table-driven incl. internal-host gate,
record-header validation, ECH → passthrough).

**Integration (in-process):**
- `httptest` TLS origin + client trusting ephemeral CA → MITM → probe plugin recorded decrypted
  method/path/headers/body; response byte-intact; **h2 and h1.1 origins both**.
- **Custom-root origin**: origin signed by a private CA placed in `upstream_ca_bundle` → re-origination
  **succeeds** (proves injected-roots fix); same origin with empty bundle → upstream-verify fails →
  passthrough (no skip-verify regression, no broken call).
- Pinned client (rejects minted leaf) → first call fails, host **auto-skipped**, retry tunnels & succeeds.
- Skip-list / internal-host (scope=external) → tunneled, pipeline not run, logged.

**E2E (OpenShell suppressed-proxy):** operator provisions CA, mounts both ways (hard volume), sets trust
env, injects `mitm` block; agent HTTPS call → AuthBridge logs decrypted request + a plugin acts on it;
trust-injection self-check passes; pinned/skip host tunneled, agent unbroken.

**Success criteria:**
1. Agent HTTPS egress decrypted → full pipeline runs (logged) → real origin, response byte-intact.
2. Agent trusts the CA with no manual steps on a **supported runtime**; self-check confirms it.
3. No startup race (hard Secret-mount gate).
4. A private-CA origin the agent trusts still works (injected upstream roots).
5. Un-MITMable / pinned traffic fails open and **self-heals** (auto-skip); no working call is broken.

## Implementation surface (summary)

**AuthBridge (all TLS/transport layer — the pipeline is untouched):** new `authlib/mitm/` package; extract
`serveOutbound` + `isMITM` discriminator; reversible decision (upstream-verify before forge) at the two
capture sites (transparent + CONNECT-with-peek); dedicated upstream `http.Client` (system + injected
roots); auto-skip set; one-conn keep-alive server; h2 via `http2.ConfigureServer`; `MITMConfig` + `main.go`
wiring; trust-injection self-check.

**Operator:** per-agent cert-manager `Certificate`/`Issuer` (with Name Constraints) + write RBAC (incl.
namespaced `Issuer`); **net-new** agent-side `ca.crt` hard mount + trust env (incl. `AWS_CA_BUNDLE`,
`GRPC_DEFAULT_SSL_ROOTS_FILE_PATH`); sidecar signing-key mount `0400`; `mitm` config block; `MITMMode` CRD
field + resolution.

## Phasing (for the implementation plan)

- **Phase 1 — AuthBridge, in-process:** `authlib/mitm/` + `serveOutbound` refactor + reversible decision +
  upstream client + auto-skip + h2, with `EphemeralSource` CA. Fully unit/integration-testable, **no
  operator dependency**. Proves the decrypt→pipeline→re-originate loop and the no-broken-calls guarantees.
- **Phase 2 — Operator trust coordination:** per-agent cert-manager CA (`FileSource`), hard mounts, trust
  env + self-check, RBAC, CRD field. Enables the zero-manual-steps E2E on OpenShell.
- **Follow-ups:** CA rotation (overlapping bundle + file-watch); leaf-cache tuning.

## Design evolution (record)

This spec was reshaped by a four-pass source-grounded adversarial review (control-flow, TLS/PKI,
trust/operator, pipeline-value). Resolved conclusions now folded into the body:
- The hard problems are all in the **TLS/transport layer**, not the pipeline. The original
  "drop token-exchange/MCP/A2A" recommendation was **rejected**: plugins self-gate, so the full pipeline
  runs on the plaintext and interception **scope** (not plugin selection) is the real knob.
- Re-origination must use a dedicated upstream client with **system + injected roots** (the mesh-mTLS
  dialer + skip-verify was a fail-closed / SVID-leak blocker, and system-roots-only would break private-CA
  origins).
- The MITM decision must be **reversible** (upstream-verify first) and **self-healing** (auto-skip), so no
  working call is broken.
- Trust injection is a **per-runtime** mechanism (scope + self-check), not a universal guarantee.
- CA-key handling (`0400` + Name Constraints) and `:9094` body-capture gating are **in scope**, not
  deferred. h2 is a **prerequisite**, not a fast-follow.
