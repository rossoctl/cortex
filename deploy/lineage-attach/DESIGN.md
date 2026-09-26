# Design — why the attachment works, and where it stops

The [README](README.md) says what to do. This is why it is built this way, what
it depends on, and the cases that look fine and are not. Nothing here is needed
to attach; all of it is needed to trust the result.

## Two halves, one insight

Every HTTP exchange a workload takes part in crosses its pod's network
boundary, and a sidecar at that boundary sees all of it: the request as it
arrives or leaves, the response as it ends, the protocol, the peer, the
outcome. Those are facts, and they can be recorded without the application
knowing. That is the first half — **capture** — and it is pure configuration:
a plugin entry in the sidecar's pipeline.

What the boundary cannot see is *causality inside the pod*: which inbound
request made the app issue which outbound call. The sidecar lives outside the
app's execution context and has no way to know which coroutine did what. To
attribute an outbound hop to the inbound that caused it, something must carry
a token **with the execution scope through the app** — which is exactly what
the W3C `traceparent` header is for, and only code running *inside* the
request's context can copy it from the inbound request onto the outbound
ones. That is the second half — **propagation** — and it is the only thing the
app must do.

The shim does it for the app: stock OpenTelemetry auto-instrumentation,
exporting nothing, baked as an inert layer. Nothing about what the app *does*
enters into it — the same layer serves an agent, a tool, a relay — and its
activation is one environment variable. Everything else the sidecar records
from the outside.

## How a hop finds its parent

Each sidecar stamps the id of the span it just emitted into the request's
`tracestate` header before forwarding; the next sidecar parents on that stamp
(`lineage.parent.source=tracestate`) and re-stamps its own. When no stamp is
present the hop is parented on the `traceparent` the wire carried and records
`lineage.parent.source=wire`; when the wire carried nothing at all it roots a
trace of its own and records `none`. The forwarded `traceparent` itself is
never modified — when none arrived the sidecar forwards one naming its own
request span, so the next hop has a context to extract and the stamp has a
header to ride on — and nothing guesses: a hop without context is *visibly*
unparented rather than attached to a plausible caller.

So there is exactly one unstamped hop (`wire`, or `none` for a caller that
sent no `traceparent`) per well-propagated trace, at the entry, where
the caller supplied the first `traceparent`. Every other unstamped hop marks a
pod that did not carry the context through — `none` when its app sent no
`traceparent` at all, `wire` when it forwarded one without the stamp. The
subtree beneath it fragments out of its caller's
trace — *visibly absent* lineage rather than wrong lineage — and attribution is
lost for everything downstream of that pod.

> In-process `traceparent` propagation is mandatory, and the sidecar cannot do
> it from outside the app.

## Why the shim is needed, and what it installs

The shim supplies propagation with stock OpenTelemetry auto-instrumentation and
**exports nothing**:

- **server side** (`starlette` / `asgi` / `fastapi`) — extract the inbound
  `traceparent`, make it the active context.
- **client side** (`httpx` / `requests` / `aiohttp-client` / `urllib3`) — inject
  `traceparent` on outbound calls. `urllib3` is what covers `boto3`/`botocore`:
  an S3 read from a tool otherwise escapes into a trace of its own (seen live).
- **`threading`** — carry the active context across `Thread.start` /
  `ThreadPoolExecutor.submit`. **Load-bearing**: frameworks that run the LLM
  call in a worker thread (anything using `loop.run_in_executor`) otherwise
  lose the context at the thread boundary and those legs silently fragment.
- **exporters pinned to `none`, and no `OTEL_EXPORTER_OTLP_ENDPOINT`** — the
  activation hook sets all three signal exporters to `none` as environment
  defaults the moment it wakes, so the shim's instrumentation generates spans
  that go nowhere. Your telemetry backend sees only what the sidecar emits.

### The activation hook

Activation is a baked, env-gated site hook (`lineage-propagate-hook.py`,
installed into site-packages as a `.pth` + module pair): every Python process
of the image checks one variable at startup and either initializes stock
auto-instrumentation (`LINEAGE_PROPAGATE=1`) or does nothing at all. The
image's ENTRYPOINT/CMD is never rewritten, there is no launcher process, and
an unactivated `-otel` image runs the app exactly as its base does, with no
OpenTelemetry module loaded. `build-otel-shim.sh` attests this on every bake,
before the image is ever loaded: `verify_inert` starts a bare interpreter with
the gate off and asserts no `opentelemetry` module loads, and
`verify_propagates` starts one with the gate on and asserts the hook ran and a
`traceparent` is injected. (Both run the interpreter directly, not the image's
ENTRYPOINT/CMD — they prove the hook's behaviour, not that the app's own
command is unchanged; that half rests on the Dockerfile never touching
ENTRYPOINT/CMD.) It is the same attach shape as the Java agent
(`JAVA_TOOL_OPTIONS`) and Node (`NODE_OPTIONS`): the switch lives in the
Deployment, next to the image reference, and nowhere in the app.

One thing the bake *does* rewrite is `USER`: the final stage resets it to
`${APP_UID}:${APP_GID}` (the detected values), so a base image that named its
user — `USER app` — comes out numeric, which drops that user's supplementary
groups. The primary uid/gid are preserved; only supplementary group
memberships are lost. Detected automatically by `build-otel-shim.sh`; a direct
Dockerfile build must pass both build-args (they have no default) or the build
fails rather than silently running as `1001:0`.

A hook failure cannot take the app down: the module catches everything and
logs; propagation is then off and the trace shows it (`none` on the pod's
outbound hops), which is the
honest failure mode. Read the hook's docstring for the full contract.

### An app that already configures OpenTelemetry itself

That app is outside the shim's envelope, and the bake interlock cannot see it
(it probes for instrumentors, not for SDK use). The hook's `initialize()`
installs the global `TracerProvider` first, so a provider the app sets in code
is rejected (`Overriding of current TracerProvider is not allowed`) and the
app's own exporter goes silent. An `OTEL_*_EXPORTER` the app sets in its
environment wins over the hook's `none` (`setdefault`), so the shim's own
server and client spans then flow to the app's backend — and `otlp` needs an
exporter package the shim does not install, in which case the hook fails at
startup and propagation stays off. For such an app attach capture only (no
`APP_CONTAINER`) and let its own instrumentation carry `traceparent`.

## The envelope

The shim is generic across the mainstream Python stack, not universal:

- **Python**, in any of the common layouts — a venv (declared via
  `$VIRTUAL_ENV` or at the usual paths) or a plain pip/system python;
  `build-otel-shim.sh` probes the image for the interpreter the app runs and
  refuses when it finds none (an explicit argument overrides the probe).
- Server is **ASGI / Starlette / FastAPI**; HTTP client is **httpx**,
  **requests**, **aiohttp** or **urllib3** (which is how `boto3` talks).
- A caller that sends no `traceparent` gets one minted by the entry sidecar
  (`parent.source=none` on that hop, contract v1.6); the shim itself seeds
  nothing.
- **Not covered:** work handed to a `multiprocessing` child or a `subprocess`,
  threads started at app boot rather than inside a request, and an app that
  configures the OpenTelemetry SDK itself — in code or through `OTEL_*`
  environment variables (above).
- The base image has a shell and coreutils (`Dockerfile.otel-shim` runs one
  `RUN` step to place the hook); a distroless base fails that step.

Outside the envelope, attach capture only (no `APP_CONTAINER`): every hop is
still recorded. Whether those hops are *correctly attributed* depends entirely
on the app carrying the trace context (`traceparent`, and the stamp in
`tracestate`) from its inbound to its outbound calls itself — and that is a
stronger condition than it sounds.

### Half-instrumented apps: the case that looks fine and is not

Propagation needs **two** halves: something that extracts the inbound
`traceparent` into an active context (`starlette` / `asgi` / `fastapi`), and
something that injects it on the way out (`httpx` / `requests` / `aiohttp` /
`urllib3`). An app carrying only the client half can inject, but has nothing
to inject *from*.

That app is in a corner this kit cannot get you out of:

- **The shim refuses it**, correctly — its client instrumentor is one this shim
  installs, so wrapping would stack a second one on the same library.
- **Capture-only does not save it** — with no server-side extraction, every
  outbound call starts a *fresh* trace.

Measured on exactly such an app: it served requests, called its LLM
successfully, and produced 26 outbound exchanges — **every one of them alone
in its own trace, none sharing a trace with the inbound that caused it.** No
correlation survives at all, and every per-trace consistency check stays green
while it happens.

**Check the shape, not just the count.** A concurrency check that only asks "is
each trace internally consistent?" will pass this app perfectly: a trace holding
one lonely inbound is clean by every structural measure. What exposes it is
expecting a *number of hops* per trace and finding one. If your app should make
three outbound calls per request, assert that three appear in the same trace —
otherwise a total attribution failure reads as a clean run. The same test
caught the shim's own gap once: a tool reading S3 through `boto3` escaped into
its own trace on a run whose every functional check passed, because the
recipe lacked the `urllib3` instrumentor. One unstamped hop in the wrong place
is the whole signal.

If you own the image, the fix is to add the missing server-side instrumentor
(or activate stock auto-instrumentation, which brings both halves — that is
what the shim's hook does). If you do not, the image reference is the only
lever left (`SELF_ACTIVATE=1`, README "The propagation half") — and only if
the shim's interlock lets the image through.

### Baking is not purely additive

The shim installs one pinned OpenTelemetry contrib release
(`OTEL_CONTRIB_VERSION` in `Dockerfile.otel-shim`), and the resolver brings
the SDK that release requires — on an image that already carries an older SDK,
the bake *upgrades* it (observed: `opentelemetry-sdk 1.42.1 → 1.44.0`, semconv
`0.63b1 → 0.65b0`). Harmless for an app that does not pin, but an app pinning
an older SDK can be affected by being wrapped. Check the build output if your
app is sensitive to those versions.

The static `uv` used to install the shim is copied into the image and left
there — a `rm` in a later layer would not reclaim the copy layer's space, so
removing it cleanly would need a build-stage split the kit does not do. It is a
small self-contained binary and inert at runtime; noted here rather than hidden.

### Refuse rather than guess

`build-otel-shim.sh` probes the image and stops if it finds no runnable Python,
or if any of the eight instrumentors the shim installs is already present
(wrapping would stack a second instrumentor on the same library), or if the
image already carries the shim's own hook. It does **not** refuse on the mere
presence of the `opentelemetry.instrumentation` namespace or of a dormant SDK
— an image can carry those as transitive dependencies and still need the shim.
`FORCE_BAKE=1` overrides the **instrumentation interlock only** — the
already-instrumented and already-has-hook refusals — and makes that human
judgment explicit and greppable. The no-runnable-Python refusal is not
overridable by it: pass the interpreter explicitly (arg 3) instead.

## Why the sidecar is a native sidecar

`proxy-init` programs the pod's iptables and exits; from that instant every
non-1337 outbound TCP connection is redirected to the envoy outbound listener.
If `envoy-proxy` were a plain container, the kubelet would start it and the app
container together and wait for neither — and an app that makes its first
outbound call *at process start* (peer discovery, a config fetch, a license
ping) would reach the redirect before envoy is listening: connection refused,
surfaced as an app error, the pod "running" and the failure silent. Observed
live on an agent mesh resolving its A2A peers at boot — every agent came up
with `0 peers` and delegations quietly no-oped.

So `envoy-proxy` is emitted as a **native sidecar**: an `initContainers` entry
with `restartPolicy: Always` and a `startupProbe` on the outbound listener,
and the kubelet holds every container that starts after it until that probe
passes, keeping it running for the pod's life. The app container therefore
*cannot* start before its proxy is accepting, with no cooperation from the app.

Where the pair lands among the owner's own init containers is strategic-merge
placement behaviour the patch does not pin (no `$setElementOrder`), and the
safety of the owner's inits deliberately does not rest on it. What the kit
does control is that **`proxy-init` and `envoy-proxy` travel adjacent in one
patch**, and both possible placements are safe: prepended (what the apiserver
does today), the owner's inits run after the proxy is up, against a working
redirect; appended, they run before any redirect exists, on ordinary pod
networking. The only arrangement that breaks — an owner init container landing
*between* the two — cannot arise from a merge, because getting there would
need the target to already own one of those two names, which the
`refuse_name_collision` precondition refuses outright.

This needs **Kubernetes ≥ 1.29** (native sidecars are on by default from 1.29,
GA in 1.33), and an older cluster fails loud: with the SidecarContainers gate
off, the apiserver drops `restartPolicy: Always` and then rejects the
now-orphaned `startupProbe` on the init container (`startupProbe: Forbidden:
may not be set for init containers without restartPolicy=Always`) — verified
on a real 1.28 cluster. So there is no version parsing and no silent wedge:
the adopt route hits that rejection at `sidecar-patch.sh`'s server-side
dry-run (which recognises the native-sidecar error and adds a needs-1.29
hint), before anything is persisted; the bring-your-own-manifests route hits
it at the operator's own `kubectl apply` of the Deployment — the ConfigMap
from that same apply does persist, inert on its own (no pod references it).

## What the sidecar can and cannot see

- **Plaintext HTTP only.** An HTTPS destination is TLS passthrough — no hop is
  recorded for it. Inside a cluster that is usually every hop that matters;
  an external HTTPS LLM is the notable exception, and it is invisible.
- **Non-HTTP protocols cannot pass through the outbound listener at all.**
  `proxy-init` redirects every outbound TCP connection to the sidecar, whose
  outbound listener hands each non-TLS connection to its HTTP codec. A
  Postgres or SMTP connection is closed on arrival (seen live as `psycopg`
  "server terminated abnormally"). `OUTBOUND_PORTS_EXCLUDE` keeps such ports
  out of the redirect; those hops carry no HTTP facts and are invisible to
  lineage either way. S3 is HTTP and stays in. The redirect is symmetric:
  every inbound TCP port of the pod is sent to the inbound listener too, and
  this kit exposes no inbound exclusion (`proxy-init` supports
  `INBOUND_PORTS_EXCLUDE`; the generator does not offer it). An app that
  *serves* a plaintext non-HTTP protocol cannot be adopted as is.
- **Content is captured through parsers, not off the wire.** With
  `capture_io: true` the A2A, MCP and inference parsers attach the parsed
  message as `input.value` / `output.value`; a plain HTTP exchange has no
  parser and is recorded bodyless — who called whom, with what outcome, but
  not what was said. Payloads are capped (`max_payload_bytes`, 4096 by
  default) with a visible truncation marker.

## Where you see the spans, on the rossoctl platform

The platform's `rossoctl-deps` chart
([`charts/rossoctl-deps/values.yaml`](https://github.com/rossoctl/rossoctl/blob/main/charts/rossoctl-deps/values.yaml),
`otel.collector`) runs an OTel collector (`deploy/otel-collector` in
`rossoctl-system`) whose base `traces` pipeline exports to `debug` at
`verbosity: detailed` — span attributes included — and does **not** install
Phoenix (`components.phoenix.enabled` defaults to `false`). Spans arriving at
the default endpoint are printed to the collector's log and stored nowhere
queryable:

```sh
kubectl logs -n rossoctl-system deploy/otel-collector | grep lineage.exchange.id
```

That is enough to prove the plugin works. For a store, add an exporter and a
pipeline to the collector's ConfigMap (Phoenix with
`--set components.phoenix.enabled=true`, Jaeger, any OTLP store of your own), or point
`OTEL_ENDPOINT` straight at a sink of your own. Nothing in this kit depends
on which you choose.

## Limits, in one place

- **The sidecar sees plaintext HTTP only.**
- **Trace-context propagation is the app's job** — the shim does it for the
  in-envelope stack, and nothing else can do it from outside the process.
- **The patch path is not durable**: the owner still owns the Deployment, and
  a platform-side rewrite (an operator reconcile, a chart upgrade, a UI
  redeploy) silently drops the sidecar. Keep the attachment in the manifests
  when you own them (README "Bring your own manifests").
- **A workload whose image *and* env you cannot influence stays capture-only.**
