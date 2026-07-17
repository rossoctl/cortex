# mTLS demo — agent-to-agent encryption via SPIRE X.509 SVIDs

> **Status.** Both variants are verified end-to-end on a kind +
> kagenti + SPIRE cluster — `make demo-mtls` (proxy-sidecar) and
> `make demo-mtls-envoy*` (envoy-sidecar; see the
> [envoy-sidecar variant](#envoy-sidecar-variant) section below).
> An earlier revision of this README flagged the proxy-sidecar demo
> as broken because `patch-mtls-config.sh` was patching the per-agent
> `authbridge-config-<name>` ConfigMap, which the operator's
> pod-mutating webhook re-renders (and scrubs `mtls:` from) on each
> pod admission. The script now patches the namespace's
> `authbridge-runtime-config` ConfigMap instead, where the
> operator's resolution chain reads `mtls.mode` and propagates it
> into every per-agent CM at admission time — one patch, all
> workloads in the namespace pick it up on next pod restart.

Two pods that talk to each other through their authbridge sidecars,
configured with `mtls.mode: strict`. Confirms end-to-end:

- authbridge's `spiffe.Provider` mirror writes `/opt/svid.pem` /
  `_key.pem` / `_bundle.pem` on both pods on every rotation; the files
  exist for external readers (e2e probes, debugging, future Envoy
  filesystem SDS).
- For the listener itself, authbridge reads X.509 SVIDs in-memory via
  `spiffe.X509Source` (no per-handshake file I/O) and uses them for
  both inbound (reverse proxy) and outbound (forward proxy) mTLS.
- The wire between the two pods is encrypted: a `tcpdump` on the
  cluster network shows no plaintext HTTP request lines on TCP port
  8080.
- abctl shows the negotiated TLS version + peer SPIFFE ID per session
  event in the detail pane.

This is intentionally minimal. It doesn't exercise IBAC, weather, or
the token-exchange flows — it just proves the mTLS layer is real.

## Prerequisites

1. **A kagenti install on a kind cluster** with SPIRE enabled.
2. **kubectl, kind, podman (or docker)** on PATH.

## Quick start

```sh
cd authbridge/demos/mtls

make demo-mtls          # deploys caller + callee, both with mtls.mode: strict
                        # waits for ready, runs an end-to-end request,
                        # asserts the wire was encrypted via tcpdump

make demo-mtls-permissive
                        # same shape but with mtls.mode: permissive
                        # sends a plaintext curl from outside the mesh,
                        # asserts the request is served (proving permissive
                        # serves both)

make demo-mtls-strict-rejects-plain
                        # hits a strict callee with plain HTTP and asserts
                        # the connection is closed (proving strict actually
                        # enforces)

make undeploy           # cleanup
```

## Verification recipe

The `make demo-mtls` target does:

1. Deploys caller + callee Pods to `team1` with operator-injected
   sidecars and SPIRE.
2. Patches the namespace's `authbridge-runtime-config` ConfigMap to
   add `mtls: { mode: strict }`. The operator's resolution chain
   reads `mtls.mode` from this CM and bakes it into every per-agent
   CM in the namespace at the next pod admission — one patch, all
   workloads pick it up.
3. Restarts the pods so the new config takes effect (mTLS config
   requires pod restart per the framework hot-reload boundary).
4. Triggers a request from caller → callee through the kagenti UI
   flow (caller's outbound forward proxy → callee's inbound reverse
   proxy).
5. Captures cluster traffic via `tcpdump -i any -n -A 'tcp port 8080'`
   inside the kind cluster's network namespace. Asserts no plaintext
   HTTP/1.1 request lines appear in captured packets.
6. Greps both pods' logs for the startup line confirming mTLS is
   enabled (`"mTLS enabled" mode=strict`).
7. Reports the negotiated TLS version + peer SPIFFE ID from the
   session API.

## What this demo doesn't cover

- **Permissive vs strict rollout**: covered by `make demo-mtls-permissive`
  and `make demo-mtls-strict-rejects-plain`.
- **Cert rotation under load**: tested via unit tests in
  `authlib/spiffe/x509source_test.go`.
- **kagenti UI integration**: this demo is intentionally pre-UI —
  if you want to chat with an mTLS-protected agent, run any other
  kagenti demo with `mtls: { mode: strict }` added to its config.
- **Ambient mesh interaction**: when ambient is on, ztunnel handles
  the cross-pod hop and authbridge's mTLS is redundant. This demo
  doesn't run ambient. Phase 5 will add ambient detection so the
  redundancy is auto-dropped.

## envoy-sidecar variant

The targets above (`make demo-mtls`, `make demo-mtls-permissive`, etc.)
exercise mTLS in **proxy-sidecar** mode, where authbridge itself is
the listener and the byte-peek + dial logic lives in Go
(`authlib/listener/internal/tlssniff` for inbound,
`authlib/listener/forwardproxy` for outbound).

A parallel set of targets exercises **envoy-sidecar** mode, where
Envoy is the listener and mTLS is configured at the data-plane level
via `DownstreamTlsContext` / `UpstreamTlsContext` reading
`/opt/svid*.pem`:

```sh
make demo-mtls-envoy                          # strict: deploy + verify
make demo-mtls-envoy-permissive               # permissive: deploy + verify plaintext callers served
make demo-mtls-envoy-strict-rejects-plain     # strict: verify plaintext rejected
make undeploy-envoy                           # tear down + restore namespace envoy-config
```

### Design

| `mtlsMode` | Inbound (peer-facing :15124) | Outbound (app-egress :15123) |
| --- | --- | --- |
| `disabled` (default) | plaintext | plaintext |
| `permissive` | `tls_inspector` + two filter chains: TLS chain terminates mTLS, raw_buffer chain accepts plaintext | **plaintext** — no TLS-wrap attempt |
| `strict` | `tls_inspector` + single TLS chain; plaintext rejected at chain match | `UpstreamTlsContext` on the `original_destination` cluster: TLS-or-fail, blanket |

Inbound is byte-identical to proxy-sidecar's `tlssniff.Listener`
(same protocol semantics, just expressed as Envoy filter chains —
matches Istio's PERMISSIVE/STRICT inbound exactly).

Outbound is **Istio-shaped**: there is no per-connection
try-then-fallback (Envoy has no native primitive for it, and Istio
itself doesn't do it — it relies on Pilot pre-deciding mesh
membership). Blanket TLS in strict mode is practical because outbound
calls that need plaintext (Keycloak / JWKS / external HTTPS) never
hit the listener:

1. Plugin outbound (token-exchange, jwt-validation talking to
   Keycloak / JWKS) uses Go's `net/http` directly — bypasses Envoy.
2. External HTTPS from the app — `proxy-init`'s iptables redirects
   only plaintext HTTP to the outbound listener; HTTPS bypasses it.
3. HTTP from the app to peer agents — what gets TLS-wrapped. Peer's
   inbound listener terminates. Works.

**Proxy-sidecar matches the same outbound semantics now**: its
forward proxy dials plaintext in permissive mode and TLS-or-fail in
strict, with no per-connection fallback. The two deployment shapes
share one outbound model. See `authbridge/CLAUDE.md`'s
"Top-level mtls: configuration" for the full table.

### Mixed-mode caveat

A *permissive* caller cannot reach a *strict* peer regardless of
mode (its outbound is plaintext; the peer's strict inbound rejects
it). Mixed-mode deployments need both ends compatible — both strict,
both permissive, or one strict + the other permissive on inbound
only.

### How the demo wires mTLS

The demo is fully **hand-crafted** — no `kagenti.io/inject` label,
no operator pod injection. This sidesteps both
[operator-side notes](#operator-side-notes): no RO `/opt` mount because
we control the volumes, no per-agent CM reconciliation because there
is no per-agent CM (the demo brings its own `envoy-config-mtls-active`
and `authbridge-runtime-mtls` CMs that the operator doesn't touch).

The demo pods bring their own:

- ServiceAccount for SPIFFE identity (auto-registered via the
  cluster's default `ClusterSPIFFEID` template
  `spiffe://<trust-domain>/ns/<ns>/sa/<sa>`).
- `authbridge-runtime-mtls` ConfigMap — minimal authbridge config
  with a no-op pipeline (jwt-validation + token-exchange registered
  but never invoked, since our Envoy config has no ext_proc filter).
- `envoy-config-mtls-active` ConfigMap — populated by
  `swap-envoy-config.sh` from one of the per-mode variants.
- emptyDir for `/opt` (RW), CSI mount for `/spiffe-workload-api`.

The demo's Envoy config uses **STATIC clusters** with explicit
upstream addresses (`callee_cluster` points at
`mtls-callee-envoy.team1.svc.cluster.local:8080`) rather than
`ORIGINAL_DST` + iptables. That lets the demo skip the proxy-init
container and HTTP_PROXY-route from the demo-app to the local
Envoy outbound listener — same pattern the proxy-sidecar mTLS
demo uses for its forward proxy on `:8081`.

Production traffic uses ORIGINAL_DST + iptables — that's a
deployment-shape concern, not a mTLS-design concern. Reusing the
same TLS blocks (`tls_inspector`, `DownstreamTlsContext`, filter
chain match, `UpstreamTlsContext`) in the operator's
`envoy.yaml.tmpl` with ORIGINAL_DST is what the kagenti-operator
follow-up PR does. The follow-up also fixes the RO `/opt` mount
and per-agent CM rendering; once it lands, the user sets
`mtlsMode: strict` on the AgentRuntime CR (or in the kagenti UI)
and the operator wires up the same Envoy YAML this demo ships by
hand.

## Operator-side notes

Running both variants against a real cluster surfaced two
kagenti-operator items worth knowing about. Both are addressed in
the kagenti-operator companion PR. A third issue (Pod-spec selector
mismatch) was a demo-side oversight already fixed by the manifest
updates that landed alongside the envoy-sidecar variant.

### `/opt` mount asymmetry on envoy-sidecar (operator bug, fixed in companion PR)

The kagenti-operator's pod mutator was mounting `svid-output` at
`/opt` with `readOnly: true` on envoy-sidecar's `envoy-proxy`
container while the proxy-sidecar branch correctly mounted it RW.
The in-process spiffe Provider mirror writes `/opt/svid.pem`,
`/opt/svid_key.pem`, `/opt/svid_bundle.pem` there on every SPIRE
rotation; under the RO mount the mirror failed:

```text
spiffe.mirror: initial x509 write
  err="write svid.pem: atomicWrite: create temp in /opt: open /opt/.tmp-svid.pem.4183688747: read-only file system"
```

Envoy then refused to boot with `Invalid path: /opt/svid_bundle.pem`
because the file-based `DownstreamTlsContext` / `UpstreamTlsContext`
references couldn't resolve. **Affects: envoy-sidecar mode only.**
The fix landed in the companion PR — flips the readOnly bit on the
`svid-output` volumeMount in `BuildEnvoyProxyContainerWithSpireOption`
(`kagenti-operator/internal/webhook/injector/container_builder.go`).

### Per-agent CMs are operator-owned (by-design; reflected in this PR's script)

The kagenti-operator's pod-mutating webhook builds each per-agent
`authbridge-config-<name>` ConfigMap by reading the namespace
`authbridge-runtime-config` as baseYAML and overlaying values
resolved from the AgentRuntime CR (or defaults when no CR exists).
The webhook's `ensurePerAgentConfigMap` actively scrubs the `mtls:`
block when the CR's `Spec.MTLSMode` is unset — that scrub is
intentional, ensuring the per-agent CM's contents track the
authoritative source (the resolution chain) rather than picking up
stale fields. Earlier revisions of this demo's `patch-mtls-config.sh`
patched the per-agent CM directly, so the patch got reverted on the
next pod admission. The current script patches the namespace
`authbridge-runtime-config` CM, which is exactly where the operator's
resolution chain reads it; one patch propagates to every per-agent
CM in the namespace.

The longer-term path is to set `Spec.MTLSMode: strict` on an
AgentRuntime CR — the operator's companion PR makes that work
cleanly for envoy-sidecar too. The namespace-CM patch this demo uses
is a lighter-weight knob that avoids requiring a CR for every demo
pod.

### Demo pod enrollment via AgentRuntime

The kagenti-operator's mutating-webhook `objectSelector` requires
`kagenti.io/type` in `[agent, tool]`. The demo manifests include
AgentRuntime CRs that enroll each workload — the operator applies
the label automatically. The `demo-app` containers set an explicit
`runAsUser: 100` (the `curlimages/curl:latest` image runs as the
non-numeric `curl_user`, which trips the kubelet's `runAsNonRoot`
check without an explicit numeric UID).
