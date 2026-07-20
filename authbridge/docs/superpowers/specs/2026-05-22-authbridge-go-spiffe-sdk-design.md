# Replace spiffe-helper with go-spiffe SDK in authbridge

**Status:** Design — pending user review
**Date:** 2026-05-22
**Issue:** [cortex#332](https://github.com/rossoctl/cortex/issues/332)
**Reference:** [klaviger's SDK usage](https://github.com/grs/klaviger/blob/main/internal/spiffe/jwt_source.go)

## Goal

Replace the `spiffe-helper` binary bundled inside authbridge images with an
in-process `go-spiffe` SDK integration. Preserve today's external behavior:
authbridge keeps reading rotated SVIDs, mTLS keeps working, JWT-SVIDs keep
flowing into token-exchange, and external readers of `/opt/svid*.pem` /
`/opt/jwt_svid.token` (Envoy filesystem SDS in upcoming work, e2e probes,
debugging shells) keep finding fresh files.

## Non-goals

- Operator changes (webhook, CRDs, e2e fixtures). Follow-up PR.
- Helm chart changes (`spiffe-helper-config` ConfigMap, helper.conf
  templates). Follow-up PR.
- Backend (`rossoctl/backend`) changes (DEFAULT_SPIFFE_HELPER_CONF). Follow-up.
- Implementing gRPC SDS for envoy-sidecar mode mTLS. Future PR; this design
  preserves the file-mirror so that work can use filesystem SDS instead.
- Changing the `X509Source` / `JWTSource` interfaces consumed by `authlib/tls`
  and `authlib/plugins/tokenexchange`. Both remain unchanged — only their
  concrete implementations swap.

## Architecture

One `spiffe.Provider` per authbridge container owns all SVID concerns. It
wraps a single `workloadapi.Client` that talks to the SPIRE agent socket
(unchanged: `/spiffe-workload-api/spire-agent.sock`). The Provider exposes:

- An in-memory `X509Source` consumed directly by `authlib/tls`,
  `reverseproxy`, and `forwardproxy` listeners (per-handshake hot path; no
  file I/O).
- An in-memory `JWTSource` consumed by the `tokenexchange` plugin
  (per-exchange hot path).
- An optional, default-on file-mirror goroutine that writes `/opt/svid.pem`,
  `/opt/svid_key.pem`, `/opt/svid_bundle.pem`, and `/opt/jwt_svid.token`
  atomically on every rotation. Mirror failures are logged but never affect
  the hot path.

```
┌─── authbridge container ───────────────────────────────────────────┐
│  /spiffe-workload-api/spire-agent.sock                             │
│            │                                                       │
│            ▼                                                       │
│  ┌──────────────────────────────────────────────────────────────┐  │
│  │ spiffe.Provider                                              │  │
│  │   workloadapi.X509Source  +  workloadapi.JWTSource           │  │
│  │   exposes spiffe.X509Source, spiffe.JWTSource                │  │
│  │   optional file-mirror goroutine → /opt/svid*.pem,           │  │
│  │                                    /opt/jwt_svid.token       │  │
│  └──────────────────────────────────────────────────────────────┘  │
│            │                          │                            │
│            ▼                          ▼                            │
│   listeners (mTLS)             tokenexchange plugin                │
└────────────────────────────────────────────────────────────────────┘
```

`spiffe-helper` binary, `helper.conf`, and the helper-launch logic in all
three `entrypoint.sh` files are removed in this PR.

## Components

### New files in `authlib/spiffe/`

- `provider.go` — `Provider` type, `NewProvider(ctx, ProviderConfig)`,
  `X509Source()`, `JWTSource(audience string)`, `Close()`.
- `workload_x509.go` — `workloadX509` (implements `spiffe.X509Source`) wraps
  `workloadapi.X509Source.GetX509SVID` / `GetX509BundleForTrustDomain`.
- `workload_jwt.go` — `workloadJWT` (implements `spiffe.JWTSource`) wraps
  `workloadapi.JWTSource.FetchJWTSVID` for a configured audience.
- `mirror.go` — file-mirror goroutine: subscribes to X.509 `Updated()`
  channel, runs JWT refresh loop at 90% lifetime, writes via tmp+rename.
- `provider_test.go`, `workload_x509_test.go`, `workload_jwt_test.go`,
  `mirror_test.go` — table-driven tests using a fake `workloadapi.Client`
  (go-spiffe's `spiffetest` or equivalent).

### Public surface

```go
package spiffe

type ProviderConfig struct {
    SocketPath   string   // default "unix:///spiffe-workload-api/spire-agent.sock"
    JWTAudience  string   // empty = no JWTSource constructed; mirror skipped
    MirrorFiles  bool     // default true
    MirrorDir    string   // default "/opt"
}

type Provider struct { /* unexported */ }

func NewProvider(ctx context.Context, cfg ProviderConfig) (*Provider, error)
func (p *Provider) X509Source() X509Source           // never nil after NewProvider
func (p *Provider) JWTSource() JWTSource             // nil if JWTAudience == ""
func (p *Provider) Close() error                     // closes both SDK sources, stops mirror
```

The `X509Source` and `JWTSource` interfaces (defined respectively in
`authlib/spiffe/source.go` and `authlib/plugins/tokenexchange/spiffe/source.go`)
are unchanged. `tokenexchange` keeps its own interface package because the
plugin remains the only consumer of `JWTSource` — the framework Provider
just constructs an implementation that satisfies it.

### Files deleted

- `authlib/spiffe/x509source.go` (FileX509Source) + `x509source_test.go`
- `authlib/plugins/tokenexchange/spiffe/file.go` (FileJWTSource) + `file_test.go`

Per rossoctl CLAUDE.md "delete completely if unused" — no runtime caller, no
need to keep dead code in-tree.

### Files modified

- `authlib/config/config.go` —
  - `MTLSConfig`: drop `CertFile`, `KeyFile`, `BundleFile`. Keep `Mode`.
  - New top-level `SPIFFEConfig` struct: `Socket`, `JWTAudience`,
    `MirrorFiles`, `MirrorDir`, with defaults applied in `Load`.
  - `tokenexchangeIdentity`: drop `JWTSVIDPath`. Keep `Type`, `ClientID`,
    `ClientIDFile`, `ClientSecret`, `ClientSecretFile`.
  - Cross-block validation: when any plugin sets
    `tokenexchange.identity.type: spiffe`, top-level `spiffe.jwt_audience`
    must be non-empty.
- `cmd/authbridge-{proxy,envoy,lite}/main.go` — replace
  `spiffe.NewFileX509Source(...)` with `spiffe.NewProvider(...)`. Pass
  `provider.X509Source()` to `MTLSOptions`. Pass `provider.JWTSource()` to
  the `tokenexchange` plugin's Configure. `defer provider.Close()`. Remove
  the file-existence wait loop (lines around `cmd/authbridge-proxy/main.go:209`)
  — `NewProvider` blocks on first SVID arrival.
- `cmd/authbridge-{proxy,envoy,lite}/entrypoint.sh` — remove the Phase 1
  `spiffe-helper` launch block (lines 27-32 in each).
- `cmd/authbridge-{proxy,envoy,lite}/Dockerfile` — remove `spiffe-helper`
  binary install, remove `helper.conf` copy if any.
- `authbridge/CLAUDE.md` — update credential-files table (lines 247-250)
  to say "written by authbridge's spiffe.Provider mirror."
- `authbridge/demos/mtls/README.md` — update line 6 spiffe-helper reference.

## Configuration schema

### Before / after diffs

```yaml
# Before
mtls:
  mode: permissive
  cert_file: /opt/svid.pem
  key_file: /opt/svid_key.pem
  bundle_file: /opt/svid_bundle.pem

plugins:
  - name: tokenexchange
    config:
      identity:
        type: spiffe
        client_id_file: /shared/client-id.txt
        jwt_svid_path: /opt/jwt_svid.token

# After
mtls:
  mode: permissive

spiffe:
  socket: unix:///spiffe-workload-api/spire-agent.sock   # default
  jwt_audience: "http://keycloak.localtest.me:8080/realms/rossoctl"
  mirror_files: true                                     # default
  mirror_dir: /opt                                       # default

plugins:
  - name: tokenexchange
    config:
      identity:
        type: spiffe
        client_id_file: /shared/client-id.txt
```

### Defaults preserve today's behavior

A config with only `spiffe.jwt_audience` set produces identical on-disk
artifacts to today's helper.conf-driven setup.

### Old fields are silently ignored

Removing `CertFile`, `KeyFile`, `BundleFile`, `JWTSVIDPath` from the structs
means the YAML decoder will discard them as unknown keys (assuming the
existing decoder does not have `KnownFields(true)` / `DisallowUnknownFields`
set). **Pre-flight verification:** confirm the loader's decoder does not
reject unknown fields before merge. A pinning test is added that loads a
config containing the old fields and asserts it succeeds — pins this
assumption against future tightening.

### Why a top-level `spiffe:` block

Both X.509 and JWT come from one Provider, one socket, one client. Splitting
SPIFFE configuration across `mtls:` and `tokenexchange.identity:` would force
operators to keep two `socket` values in sync. A single block matches the
runtime architecture.

## Data flow

### Startup

```
1. config.Load → cfg.SPIFFE populated with defaults
2. NewProvider(ctx, cfg.SPIFFE):
     workloadapi.NewX509Source(ctx, WithAddr(socket))
       — blocks on first SVID; ctx is uncancelled (background) so kubelet
         restart pattern matches today's "wait for /opt/svid.pem"
     if JWTAudience != "":
       workloadapi.NewJWTSource(ctx, WithAddr(socket))
     if MirrorFiles:
       fileMirror goroutine spawned; first X.509 + JWT writes are
       synchronous before NewProvider returns
3. Wire sources into consumers
4. defer provider.Close()
```

### Hot paths

**Inbound mTLS handshake:**
```
GetCertificate         → provider.X509Source().Certificate()
                       → workloadapi.X509Source.GetX509SVID()  (in-memory)
VerifyPeerCertificate  → provider.X509Source().TrustBundle()
                       → workloadapi.X509Source.GetX509BundleForTrustDomain
```
No file I/O.

**Outbound token exchange:**
```
JWTAssertionAuth.TokenSource(ctx)
  → provider.JWTSource().FetchToken(ctx)
  → workloadapi.JWTSource.FetchJWTSVID(ctx, jwtsvid.Params{Audience: aud})
  → return svid.Marshal()
```
SDK caches internally; repeated calls within token validity skip the agent.

### Rotation

**X.509:** SPIRE pushes new SVID over the gRPC stream → SDK updates
internal state atomically → next handshake reads new SVID transparently →
`Updated()` fires → mirror goroutine writes new files via tmp+rename.

**JWT:** mirror loop calls `FetchJWTSVID`, writes file, sleeps until
`expiry * 0.9`, repeats. The hot path doesn't depend on this loop — it
calls `FetchJWTSVID` directly, which the SDK refreshes on demand.

### Shutdown

`provider.Close()` closes both SDK sources (gRPC streams) and stops the
mirror goroutine. `/opt/*` files are left in place (last-good values).

### Edge cases

- **No token-exchange:** `spiffe.jwt_audience` empty → no JWTSource, no JWT
  mirror. `provider.JWTSource()` returns nil. The cross-block validation
  above already errors if `type: spiffe` is set without an audience, so any
  caller that reaches `provider.JWTSource()` and sees nil represents a
  config-validation gap and is handled with a startup error.
- **SPIRE agent restart:** SDK auto-reconnects with backoff. Cached SVID
  stays valid until expiry; next push restores. No app-level handling.
- **Cold start:** `NewProvider` blocks until first SVID arrives. With
  `MirrorFiles=true`, files are written before `NewProvider` returns. This
  is a tighter guarantee than today (spiffe-helper writes asynchronously).

## Error handling

### Fatal at startup

| Condition | Behavior |
|---|---|
| `spiffe.socket` not `unix://...` | Config validation fails. |
| `spiffe.jwt_audience` empty AND any plugin uses `type: spiffe` | Config validation fails (cross-block check). |
| `workloadapi.NewX509Source` fails | `NewProvider` returns wrapped error; `main` exits non-zero; kubelet restarts. |

### Non-fatal at startup

| Condition | Behavior |
|---|---|
| `spiffe.mirror_dir` not writable when `MirrorFiles=true` | Pre-flight probe (write+delete a `.probe` file). On failure, log WARN and disable mirroring for this run. Hot path is the source of truth. |

### Hot-path errors

`X509Source.Certificate()` / `TrustBundle()` are effectively infallible
post-startup (SDK source has cached SVID). Errors propagate to TLS callbacks
where today's behavior (handshake fails, peer reconnects) is correct.

`JWTSource.FetchToken()` errors propagate to tokenexchange. Real failures:
agent unreachable (SDK retries; eventual error → 503 to caller), audience
misconfigured (`PermissionDenied`, surfaced with audience name in error).

### Mirror errors

Logged at WARN. Last-good file remains readable. Mirror retries on next
rotation signal (X.509) or after 30s sleep (JWT). Mirror errors must never
affect the hot path — this is the key invariant.

## Testing

### Unit tests (in `authlib/spiffe/`)

Use go-spiffe's `spiffetest` (or fake workloadapi server) to drive the SDK:

- `provider_test.go` — construction success, agent unreachable returns
  wrapped error, JWT audience empty skips JWT source, defaults applied,
  Close() idempotent.
- `workload_x509_test.go` — `Certificate()` and `TrustBundle()` pass-through;
  federated bundles included (parity with `include_federated_domains=true`).
- `workload_jwt_test.go` — `FetchToken()` returns marshaled JWT; audience
  forwarded; SDK error wrapped with audience name.
- `mirror_test.go` — atomic write semantics (tmp+rename, no torn writes);
  `MirrorFiles=false` produces no writes; mirror failure logged but no
  crash; JWT refresh schedule respects 90% lifetime.

### Integration tests (existing)

`authlib/tls/server_test.go`, `client_test.go`, `plugin_test.go` use stubs
that implement `X509Source` and `JWTSource` directly — **unchanged**.
`testhelpers_test.go:49` ("in the shape spiffe-helper would write") is
mechanically rewritten to use a fake or the new SDK source.

### End-to-end

`rossoctl/tests/e2e/token_exchange/test_token_exchange.py` does
`kubectl exec ... cat /opt/jwt_svid.token`. With `mirror_files=true`
default, this **continues to work** — the file still exists and contains a
valid token. No e2e edits required.

### Old-config pinning test

A new test boots the config loader with old fields (`mtls.cert_file`,
`tokenexchange.identity.jwt_svid_path`) present and asserts no error. Pins
the silently-ignore-unknown-keys behavior so a future "tighten unknown
fields" change can't silently break in-flight chart configs.

### Manual verification before merge

- Build all three images, confirm no `spiffe-helper` binary in
  `/usr/local/bin/`.
- Deploy to Kind via `kind-full-test.sh`. Verify:
  - Container starts with no Phase 1 entry in entrypoint logs.
  - `/opt/svid.pem`, `/opt/svid_key.pem`, `/opt/svid_bundle.pem`,
    `/opt/jwt_svid.token` exist within ~10s of pod ready.
  - mTLS handshake between two authbridge-proxy pods succeeds.
  - Token exchange against Keycloak returns a valid token.
  - After ~3min, `/opt/svid.pem` mtime updates (rotation works).
- Run `verify-spire-keycloak.sh` once before merge.

### Coverage gaps accepted

- No CI job against a real SPIRE agent — `spiffetest` + manual verification
  is the bar. Same gap exists today against `FileX509Source`.
- No load test for "thousand handshakes during rotation" — atomic SVID swap
  is well-tested upstream in go-spiffe.

## Rollout

### What ships in this PR

The bullet list under "Files modified" / "Files deleted" / "New files"
above. PR description includes the manual-verification checklist.

### Sequencing risk

Old fields ignored silently → this PR can merge before chart/operator
follow-ups. **One concrete failure mode:** chart sets
`tokenexchange.identity.type: spiffe` but doesn't yet render
`spiffe.jwt_audience` → authbridge fails Validate at startup.

**Mitigation:** before this PR merges, the rossoctl chart PR that adds
`spiffe.jwt_audience` rendering must merge first. Realistic order:

1. Chart PR (adds `spiffe.jwt_audience` rendering, leaves
   `spiffe-helper-config` ConfigMap intact for now).
2. **This PR.** Deployments boot fine; old fields ignored.
3. Operator + backend cleanups land independently afterward, no urgency.
4. Future PR cleans up the now-unused `spiffe-helper-config` ConfigMap and
   `JWT_AUDIENCE` ConfigMap key from the operator webhook.

### Image size

Each combined image drops ~15-20MB (compressed) after `spiffe-helper`
binary removal. Mentioned in PR description.

### Reversibility

Rollback = pin previous authbridge image tag in the chart. Configs are
forward- and backward-compatible (new image ignores old fields; old image
ignores new fields, assuming neither rejects unknown keys). The new
`spiffe.jwt_audience` field is the one rollback hazard if added to a config
the old image's loader rejects.

## Follow-ups (separate PRs, not this design)

| Repo | What |
|---|---|
| `rossoctl/charts/rossoctl/templates/` | Add `spiffe.jwt_audience` rendering. **Must precede this PR.** Later: remove `spiffe-helper-config` ConfigMap, stop emitting old fields. |
| `rossoctl/operator` | Webhook injector: stop creating `spiffe-helper-config` Volume; surface `JWT_AUDIENCE` into authbridge YAML's `spiffe.jwt_audience`. Update e2e fixtures. |
| `rossoctl/rossoctl/backend` | Delete `DEFAULT_SPIFFE_HELPER_CONF`; stop creating spiffe-helper-config ConfigMap on agent runtime creation. |
| `cortex/authbridge` (envoy-sidecar mTLS) | When envoy-sidecar mode adds mTLS, Envoy's filesystem SDS can read the same `/opt/svid*.pem` paths the mirror writes. No additional auth bridge change needed — payoff of this design's option-B choice. |
