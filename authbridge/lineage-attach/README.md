# Lineage — per-request data lineage from the AuthBridge sidecar

Every HTTP exchange a workload takes part in crosses its pod's network
boundary. A sidecar at that boundary sees all of it — who called whom, over
which protocol, with what outcome, and — if you switch it on — what was said.
Attach the AuthBridge sidecar to a Deployment that is **already running**,
switch on the `lineage-telemetry` plugin, and each exchange becomes **two
OTLP spans**, request and response, sent to any OTLP consumer. The application
is not asked to do anything, and nothing about what it does enters into it.

One thing the boundary cannot see is which inbound request caused which
outbound call. Only code running inside the request can carry that, and it
does so by forwarding two headers, `traceparent` and `tracestate`. So the app image gets one
inert layer — stock OpenTelemetry auto-instrumentation with every exporter
off — that wakes on a single environment variable and forwards the header.
The app's source, command and manifests stay as its owner wrote them.

That is the whole attachment: a ConfigMap and a strategic-merge patch on the
cluster side, an image reference and one variable on the app side. It is
additive, it is reversible with one printed reverse patch, and it is the same
for every workload in the fleet — an agent, a tool, a relay, a service nobody
remembers writing.

> **Until a release carries the plugin, build the sidecar yourself.** The
> published `ghcr.io/rossoctl/cortex/authbridge-envoy` image boots without
> `lineage-telemetry` and logs `unknown plugin`. The plugin is cortex #761;
> [RECIPE.md](RECIPE.md) step 1 builds and loads `authbridge-envoy` + `proxy-init`
> from a tree that carries it.

**Start here:** [RECIPE.md](RECIPE.md) — six steps, expected output, back
out. **Why it works and where it stops:** [DESIGN.md](DESIGN.md).

---

## What you get

Per HTTP exchange, two spans. The request span is named
`{self_id} {protocol} {operation}`, the response span appends ` response`; the
pair is joined by `lineage.exchange.id` (the request span's own id):

| attribute | what it records |
|---|---|
| `lineage.exchange.id` · `lineage.role` | pairs the two spans of one exchange; `request` / `response` |
| `lineage.direction` · `lineage.self.id` · `lineage.peer.host` | `inbound` / `outbound`; this workload's stable id; the other end |
| `lineage.protocol` | `a2a` / `mcp` / `inference` / `http` |
| `lineage.outcome`, `lineage.denied_by` | how the exchange ended |
| `lineage.principal.sub`, `lineage.principal.client` | the caller's identity when a validated token carried one (the generated pipeline runs no `jwt-validation`, so not with the generated ConfigMap) |
| `lineage.parent.source` | `tracestate` — parented on the previous sidecar's stamp; `wire` — on a `traceparent` that arrived without a stamp; `none` — nothing valid arrived, so this hop roots a trace and a `traceparent` is minted for the next |
| `input.value` / `output.value` | with `capture_io: true`: the parsed A2A message, MCP arguments or LLM prompt, cut at `max_payload_bytes` (4096 unless `MAX_PAYLOAD_BYTES` says otherwise) with a visible marker |

The attributes are the plugin's: [`plugin-catalog.md`](../docs/plugin-catalog.md#lineage-telemetry)
lists its knobs, [`lineage-wire-contract.md`](../docs/lineage-wire-contract.md) the wire format.

A well-propagated trace has exactly one unstamped hop, at the entry: `wire`
when the caller sent a `traceparent`, `none` when it sent nothing. Every other
unstamped hop marks a pod that did not carry the context through — `none` when
its app sent no `traceparent` at all, `wire` when it forwarded one without the
stamp — and the subtree beneath it is visibly its own trace rather than
silently misattributed. The spans say
what happened on the wire; whatever consumes them decides what it means.

---

## How to attach

The target is a Deployment that already exists. `attach-lineage.sh` generates
exactly two things — the per-app plugin ConfigMap (`EMIT=cm`) and a
strategic-merge patch with the sidecar pieces (`EMIT=patch`, the default) —
and there are two ways to consume them.

### Adopt a live Deployment

```sh
DEPLOY=<deployment> [APP_CONTAINER=<container> APP_IMAGE=docker.io/library/<app>-otel:latest] ./sidecar-patch.sh
```

`sidecar-patch.sh` checks the preconditions (the Deployment exists, the
platform-rendered `envoy-config` ConfigMap is in the namespace, no container
already named `envoy-proxy`/`proxy-init`, no port collision, no volume
already named `envoy-config`/`authbridge-runtime` — volumes merge by name
too, so an existing one would have its source silently repointed —
`APP_CONTAINER` names a real container), applies the ConfigMap,
patches the Deployment, and waits for the rollout. The patch only *adds*:
lists merge by name, so everything the owner wrote stays as written. To back
out, run the reverse-patch line the script printed — a strategic merge that
`$patch: delete`s exactly what the attach added and restores the app image it
replaced, leaving every later change of the owner's in place — then delete
`authbridge-lineage-config-<name>`. (A `rollout undo` is not a back-out: it
restores a whole earlier pod template, silently taking with it anything the
owner changed since the attach.) The line is reconstructible without
scrollback: `EMIT=undo` with the attach's `NAME`/`NAMESPACE`/`APP_CONTAINER`
plus `RESTORE_IMAGE=<pre-attach ref>` regenerates the patch — not `APP_IMAGE`,
which is the ref to *install* and is refused in undo mode (RECIPE step 5).

Two limits. **The target must not already carry an AuthBridge sidecar** — a
platform-enrolled workload (an `AgentRuntime` CR) has an injected one, also
named `envoy-proxy`, so the script refuses rather than merge into it; see the
namespace route below. **A patch is not durable** — the owner still owns the
Deployment, and a platform-side rewrite (operator reconcile, chart upgrade, UI
redeploy) silently drops the sidecar. Re-run after such a change, or keep the
attachment in the manifests.

### Bring your own manifests

If you own the app's manifests, make lineage part of them instead — the
attachment then survives every re-deploy:

```sh
NAME=my-agent EMIT=cm    ./attach-lineage.sh > lineage-cm.yaml
NAME=my-agent EMIT=patch APP_CONTAINER=agent APP_IMAGE=docker.io/library/my-agent-otel:latest \
  ./attach-lineage.sh > lineage-patch.yaml
```

```yaml
# kustomization.yaml
resources: [deployment.yaml, service.yaml, lineage-cm.yaml]   # yours untouched, plus the generated cm
patches:
  - path: lineage-patch.yaml
    target: { kind: Deployment, name: my-agent }
```

### The propagation half

Both routes attach **capture**. For *attribution* — outbound hops landing in
the trace of the inbound that caused them — the app must forward
`traceparent`: its own instrumentation, or the shim:

```sh
./build-otel-shim.sh <your-app>:latest    # -> <your-app>-otel:latest, attested, kind-loaded
```

then hand the patch the container to switch on: `APP_CONTAINER=<name>` adds
`LINEAGE_PROPAGATE=1` to that container's env (merged by name — nothing else
in the container changes) and `APP_IMAGE=…-otel:latest` points it at the baked
image. Without `APP_CONTAINER` the app container is not touched at all.

The `-otel` image must be resolvable the way the base is: the patch swaps
`image` and leaves `imagePullPolicy` alone, so a kind-loaded image needs
`IfNotPresent` and a registry-pulled base needs the `-otel` tag pushed beside
it. A Deployment whose env you cannot touch at all has one lever left, the
image reference: `SELF_ACTIVATE=1 ./build-otel-shim.sh` bakes the switch in.

### Enrolled workloads: the namespace-ConfigMap route

When the platform injects its own AuthBridge sidecar (an `AgentRuntime` CR),
lineage is enabled for the whole namespace by adding the three parsers +
`lineage-telemetry` to both directions of the operator-rendered
`authbridge-runtime-config` ConfigMap. Leave `self_id` unset — each pod
resolves its identity from the operator-mounted credential. The propagation
half is unchanged, and a platform upgrade re-renders the ConfigMap; re-apply
after one. This route is described here, not exercised: nothing in this kit
generates that edit.

---

## What it costs, per workload

Deploying the app is your work and stays your work. Attaching lineage adds
two commands and no YAML:

| step | command | per | done for you |
|---|---|---|---|
| bake | `./build-otel-shim.sh <image>` | image | interpreter and uid detection, the interlock, the attestation, the kind load |
| attach | `DEPLOY=<name> APP_CONTAINER=<c> APP_IMAGE=…-otel:latest ./sidecar-patch.sh` | Deployment | the ConfigMap, the patch, six preconditions, the rollout wait |

Capture only is one command, the attach without `APP_CONTAINER`. A fleet is
two loops (RECIPE "A fleet"). Then read the *shape* of one trace: one root per
turn, unstamped only at the entry.

---

## Prerequisites and configuration

- A cluster with the platform installed and the platform-rendered
  **`envoy-config` ConfigMap** in the target namespace (the sidecar mounts it).
- Sidecar images resolvable from the cluster: `SIDECAR_IMAGE` /
  `PROXY_INIT_IMAGE`, defaulting to the published
  `ghcr.io/rossoctl/cortex/{authbridge-envoy,proxy-init}:latest` — see the
  caveat at the top.
- An **OTLP/gRPC endpoint**, `OTEL_ENDPOINT`, default
  `otel-collector.rossoctl-system.svc.cluster.local:4317`. Nothing downstream
  is assumed; DESIGN "Where you see the spans" covers the platform collector.
- **Know what leaves the pod.** Content capture is off by default, as in the
  plugin. `CAPTURE_IO=true` makes the spans carry parsed content: LLM prompts,
  MCP arguments, A2A messages, cut at 4,096 bytes (`MAX_PAYLOAD_BYTES`;
  `-1` keeps them whole). That content is
  PII-bearing. It travels to `OTEL_ENDPOINT` as plain gRPC unless the endpoint
  starts with `https://`, and on the stock platform the collector prints every
  attribute into its own pod log.

The generated ConfigMap's plugin entry:

```yaml
- name: lineage-telemetry
  config:
    otel_endpoint: "otel-collector.rossoctl-system.svc.cluster.local:4317"   # host:port; https:// prefix turns on TLS
    capture_io: false     # the plugin's default; CAPTURE_IO=true attaches the parsed content — PII lives in it
    self_id: "<deploy>"   # falls back to self_id_file (the operator-mounted credential)
    # max_payload_bytes: 4096 — the plugin's default cap on a captured value (MAX_PAYLOAD_BYTES); bypass_paths / bypass_hosts as below
```

`bypass_paths` / `bypass_hosts` keep infrastructure noise out (agent-card
discovery, health probes, telemetry backends). `OUTBOUND_PORTS_EXCLUDE` keeps
a port out of the iptables redirect: an app's own telemetry export port, or a
**plaintext non-HTTP store** it talks to (Postgres 5432, SMTP 1025, Redis 6379
— the outbound listener's HTTP codec would close them; DESIGN "What the
sidecar can and cannot see"). Never exclude LLM, tool, peer or S3 ports.
`NO_EMIT=1` keeps the sidecar as a pure proxy — a clean A/B baseline.
Script knobs: `NAME`/`DEPLOY`, `NAMESPACE`, `SELF_ID`, `OTEL_ENDPOINT`,
`CAPTURE_IO`, `MAX_PAYLOAD_BYTES`, `APP_CONTAINER`, `APP_IMAGE`, `OUTBOUND_PORTS_EXCLUDE`, `SIDECAR_IMAGE`,
`PROXY_INIT_IMAGE`, `NO_EMIT`, `EMIT`; each script's header documents its own.

---

## Files

Two moments, eight files. The bake happens once per image, on a laptop; the
attach once per Deployment, against the cluster. The only thing that crosses
between them is an image reference.

```
BAKE — once per app image                 ATTACH — once per Deployment
  build-otel-shim.sh <app>:latest           sidecar-patch.sh DEPLOY=<name> [APP_CONTAINER= APP_IMAGE=]
    ├─ container-runtime.sh   podman/docker, kind load     ├─ checks    Deployment · envoy-config · names · ports · container
    ├─ Dockerfile.otel-shim   + lineage-propagate-hook.py  ├─ attach-lineage.sh EMIT=cm    → kubectl apply
    └─ <app>-otel:latest      inert until LINEAGE_PROPAGATE=1 ├─ attach-lineage.sh EMIT=patch → kubectl patch
                                                            └─ kubectl rollout status
```

| file | what it is |
|---|---|
| `RECIPE.md` · `DESIGN.md` | the step-by-step; the reasoning, envelope and limits |
| `attach-lineage.sh` | **the one generator** — every YAML byte of the attachment, `EMIT=patch` / `EMIT=cm`, env-driven, stdout only, every input validated or refused |
| `sidecar-patch.sh` | the live applier: preconditions, then ConfigMap + patch + rollout wait; owns no YAML |
| `Dockerfile.otel-shim` | the propagate-only layer, one recipe for every in-envelope app, instrumentors pinned to one contrib release |
| `build-otel-shim.sh` | bakes, attests (gate off: nothing OTel-shaped loads; gate on: a `traceparent` is injected), kind-loads; refuses images it cannot safely wrap |
| `lineage-propagate-hook.py` | the env-gated site hook the Dockerfile installs (`.pth` + module); read its docstring for the contract |
| `container-runtime.sh` | sourced helper: docker vs podman, kind load either way |

---

## Troubleshooting

| symptom | cause |
|---|---|
| No spans at all | Wrong `OTEL_ENDPOINT`, or the sidecar image predates the plugin — read the `envoy-proxy` container's log. |
| `envoy-proxy` restarts with `unknown plugin "lineage-telemetry"` | The published image, until a release carries the plugin. Build from this repo (RECIPE step 1); the printed back-out line meanwhile. The patch pulls `IfNotPresent`, so a node that cached an older `:latest` keeps it. |
| Only inbound hops, never outbound | `proxy-init` did not install its iptables rules — its log. |
| Outbound hops fragment (`lineage.parent.source=none` on the pod's outbound hops) | `traceparent` not propagating: the app container lacks `LINEAGE_PROPAGATE=1` (the patch sets it with `APP_CONTAINER`; an operator-owned Deployment needs `SELF_ACTIVATE=1`), or the call runs in a worker thread (the `threading` instrumentor is bundled), or the client library is outside the envelope. Only the entry hop dangling is expected. |
| The app cannot reach its database / mail server after the patch | A plaintext non-HTTP port went through the outbound HTTP codec — `OUTBOUND_PORTS_EXCLUDE` it. |
| A non-HTTP port the app *serves* stops answering after the patch | Inbound is redirected too, and there is no inbound exclusion knob; the app cannot be adopted as is (DESIGN "What the sidecar can and cannot see"). Run the printed back-out line. |
| Nothing captured when testing | `kubectl port-forward` reaches the app on loopback and bypasses the sidecar. Drive from inside the cluster. |
| `kind load` fails under podman | `container-runtime.sh` saves + loads an archive for podman v5; `CONTAINER_TOOL` forces a runtime, `KIND_CLUSTER_NAME` the cluster. |
| Sidecar `ImagePullBackOff` | `SIDECAR_IMAGE` / `PROXY_INIT_IMAGE` unresolvable from the cluster. |
| App container `ErrImagePull` after the patch | `APP_IMAGE` unresolvable under the container's own `imagePullPolicy` ("The propagation half"). Run the printed back-out line. |
| Pod stuck `ContainerCreating`, `configmap "envoy-config" not found` | Not a platform-set-up namespace (`sidecar-patch.sh` checks; the manifests route cannot). |
| `sidecar-patch.sh` refuses: "already has a container named …" | The target carries an `envoy-proxy` container or a `proxy-init` init container — enrolled workload, another mesh, or an earlier attach. Namespace route for the first; the printed back-out line for the last. |
| `sidecar-patch.sh` refuses: "already declares containerPort …" | An undeclared sidecar, or the app itself listens on 9090/15123/15124; the latter cannot be adopted. |
| `sidecar-patch.sh` refuses: "already has a volume named …" | The target owns a volume named `envoy-config` or `authbridge-runtime`. Volumes merge by name, so the patch would silently repoint that volume's source under the owner's own mounts; the target cannot be adopted until the owner renames their volume. |
| `sidecar-patch.sh` refuses: "has no container named …" | `APP_CONTAINER` matches nothing; a strategic merge would otherwise *add* a stub container by that name. |
