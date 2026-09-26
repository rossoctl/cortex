# Lineage wire contract — two-span sidecar lineage (v1.7.0)

What the AuthBridge `lineage-telemetry` plugin emits, what it writes onto the wire, and what the
data-governance `sidecar` interactions algorithm (ADR-0030) commits to when consuming it.

- Producer: `cortex/core/plugins/lineage/` (repo `rossoctl/cortex`).
- Consumer: `data_governance/processors/interactions/sidecar.py`; vocabulary in
  `data_governance/sidecar_facts.py` (repo `rossoctl/lab-data-governance`).

This document is kept **byte-identical in both repositories** — `cortex/docs/lineage-wire-contract.md`
and `lab-data-governance/docs/sidecar-wire-contract.md`. The version in the title is the pin: a
minor bump means producer behaviour or vocabulary changed; a patch bump means prose only. A change
is a pull request to both repositories.

## 1. Principles

- **Facts, not meaning.** The producer emits what it observed on the wire plus parsed protocol
  facts. All vocabulary — hop kinds, entity kinds, caller/callee — lives in the consumer's
  `classify()`.
- **Emit on sight.** Two spans per exchange, each emitted and ended as soon as its half has been
  seen. No span is held open across the wait and no body is buffered for the exchange's lifetime.
- **One channel for the sidecar chain.** The sidecar parent chain lives entirely in one W3C
  `tracestate` member, `lineage-parent`. A valid forwarded `traceparent` is never modified. The
  sidecar's spans and an application's own spans land in different backends, so a chain that
  pointed across the two would always dangle somewhere; the member keeps the sidecar chain
  self-consistent in this store while an instrumented application keeps its own `traceparent` chain
  intact toward its own backend.
- **No mechanism may guess.** A mechanism whose correctness depends on a precondition it cannot
  verify at runtime does not belong in the producer. When attribution is unknown the producer says
  so — `lineage.parent.source` is `wire` or `none` — and the edge is visibly absent. A missing edge
  is recoverable downstream; a confidently wrong one is not, because it is indistinguishable from a
  true one.
- **Parsers reduce payloads; interactions do not depend on them.** `input.value` and
  `output.value` are semantic content produced by the protocol parsers, not raw bytes, and they are
  enrichment only. Every exchange the sidecar saw becomes a complete interaction — kind, endpoints,
  timing, status — whether or not a body could be read.

## 2. Span model

One HTTP exchange through the sidecar produces two OTLP spans.

| | request span | response span |
|---|---|---|
| emitted | when the request (headers and body) has been seen and forwarded | when the response has been fully relayed, or the stream ends or errors |
| SpanKind | SERVER for inbound, CLIENT for outbound | same as its request span |
| parent | see §3.2 | its request span |
| carries | caller-side facts and `input.value` | outcome and status facts and `output.value` |

- `lineage.exchange.id` is the request span's own span id, echoed on both spans. No new identifier
  is minted; the response span names its request twin. Exchange duration is computed downstream as
  response end minus request start.
- The response span is emitted at stream end **even when no response was produced** — client
  disconnect, upstream reset, plugin denial. It then carries `lineage.outcome` and whatever status
  exists, so the row completes as failed instead of dangling.
- The producer samples unconditionally: a valid caller `traceparent` with the sampled-out flag
  (`…-00`) does not suppress the spans — lineage is an audit record, and a caller-chosen flag is
  not an opt-out from being graphed. The forwarded `traceparent` keeps the caller's flags (§3.3: a
  valid one is never modified); only what this producer exports ignores them.
- A lone request span means one of four things: the sidecar died mid-exchange; a panic while
  emitting the response span was recovered by the pipeline (a WARN is logged); the response span
  was emitted but lost — the two halves enter a batching exporter an exchange apart, so a response
  can be lost after its request has flushed; or the exchange outlived a config hot-reload — old
  pipelines stop a drain window (default 30 s) after the swap, and a response span emitted into
  the old, already-shut-down provider is dropped, which is routine for SSE or LLM exchanges
  longer than the window. The consumer renders it as in-flight, never as a wrong
  pairing. A response span whose `lineage.outcome` is absent derives with `error` NULL (honest
  unknown), never `false`.
- **Scope of `denied`.** The lineage plugin runs after the gate plugins and the pipeline
  short-circuits on a request-phase denial, so an exchange a gate rejects **before** the request
  span exists emits **no spans at all** and is invisible to lineage. `denied` appears only for
  denials after the request span exists: response-phase denials, or gates ordered after lineage.
  Spans for gate-denied traffic are a named follow-up, not current behaviour.
- **Bypass.** Requests whose path matches a `bypass_paths` glob, and outbound requests whose
  host matches a `bypass_hosts` glob (both `path.Match`; hosts port-stripped and case folded,
  paths query-stripped and normalized), produce no spans
  (defaults in §6). `bypass_hosts` is outbound-only: an inbound `Host` is the caller's own header,
  so honouring it there would let a caller suppress the record of its own request.

## 3. Trace context on the wire

### 3.1 The stamp

Each lineage element — inbound and outbound alike — re-stamps one W3C `tracestate` member on the
request it forwards:

```
tracestate: lineage-parent=<this element's request span id>
```

Inbound stamps toward its own application: an application that propagates W3C context carries
`tracestate` through its per-request causal chain, so the member surfaces on exactly the outbound
calls that inbound caused. Outbound stamps toward the peer, whose inbound sidecar reads it as its
parent. Foreign `tracestate` members are preserved. The key is producer-owned and names the lineage
domain; it never lands in stored data, so it is wire-only, and every sidecar on a hop must run the
same key.

The stamp needs a valid `traceparent` to ride on: a W3C reader takes `tracestate` only alongside a
valid `traceparent`. That is why §3.3 exists.

### 3.2 Parent selection

The request span's parent is chosen by the first rule that applies, in both directions:

| precedence | parent | `lineage.parent.source` |
|---|---|---|
| 1 | the `lineage-parent` stamp, if the wire context is valid and the member parses as a span id in that trace | `tracestate` |
| 2 | the wire `traceparent`'s parent span, if the wire context is valid | `wire` |
| 3 | none — the request span roots a new trace | `none` |

There is no fourth option. A malformed stamp falls through to the wire parent silently. Under
precedence 1 the parent is the previous lineage element: the caller sidecar's outbound request span
for an inbound, this pod's inbound request span for an outbound. Under precedence 2 the parent is
usually a span this pipeline never exported (an application-internal span, or an un-sidecared
caller's); the exchange still derives as a complete interaction, but as a trace entry rather than a
child.

### 3.3 The `traceparent` rule

- **Valid → never modified.** A `traceparent` the W3C propagator accepts is forwarded byte for
  byte.
- **Invalid → restarted.** When the request carries no valid `traceparent` — absent, empty or
  malformed, as the propagator judges it, the same verdict that yields `parent.source=none` — the
  producer forwards a new one naming its own request span, and the caller's `tracestate` is
  dropped; the stamp is then written alone. This is W3C Trace Context's processing model for an
  unparseable `traceparent`. Without it the next element has nothing to extract: a propagating
  application roots a trace of its own and discards `tracestate`, the stamp never leaves the pod,
  and every call the application makes derives as a separate root.
- Controlled by `mint_traceparent` (default on). Off, the producer writes no `traceparent` at all
  and an entry without a valid one fragments as described.

Consequences for the stored trace: a trace entered through a sidecar with no valid `traceparent`
has a real, exported root (the entry request span, `parent.source=none`). A trace entered with a
foreign valid `traceparent` — an un-sidecared driver or UI that propagates — keeps one dangling
parent at the trace edge, by design.

### 3.4 What the producer writes onto a forwarded request

| header | when | value |
|---|---|---|
| `tracestate` | both directions, whenever a valid context exists after §3.3 — except a bypassed exchange (§6) or a not-yet-ready producer. A malformed or over-long (more than 32 members) inbound `tracestate` is dropped whole by the extractor before the stamp, per W3C, and the stamp is written as the only member; a list at exactly 32 members loses its right-most member to make room | the caller's members with `lineage-parent` set to this request span id |
| `traceparent` | only when the request carried no valid one and `mint_traceparent` is on | `00-<trace id>-<this request span id>-<flags>` |

Nothing else is written. The listener is responsible for carrying these header mutations to the
wire.

### 3.5 Un-stamped traffic

An application with no context propagation, one that strips `tracestate`, or a caller with no
sidecar yields precedence 2 or 3 at the next element. The trace fragments at that pod, visibly,
instead of being welded by a guess. The consequences per case:

| case | inbound entry | that pod's outbound calls |
|---|---|---|
| caller propagates, application propagates | stamp or wire | stamp |
| caller propagates, application does not | wire | wire: each call a trace of its own, restarted by its outbound sidecar |
| caller sends no valid context, application propagates | none (restarted) | stamp: one tree under the entry |
| caller sends no valid context, application does not | none (restarted) | wire: each call a trace of its own |

A bypassed exchange (§6) is a fifth case: no spans at that element and the stamp passes through
unchanged, so the next element parents on the last element that did stamp — the bypassed hop is
simply absent from the chain.

## 4. Attributes

Resource attributes: `service.name=authbridge`, `authbridge.component=lineage-telemetry`. Both are
constant on every pod, deliberately: the resource says what produced the span, and the span says
which workload it was beside (`lineage.self.id`). A backend that groups by `service.name` —
Phoenix and Jaeger both do — therefore shows one merged service. Operators wanting per-workload
grouping map `lineage.self.id` onto `service.name` in a collector transform, the same way §8
handles `openinference.span.kind`.

| key | on | example | notes |
|---|---|---|---|
| `lineage.exchange.id` | both | `00f067aa0ba902b7` | the request span id, hex |
| `lineage.role` | both | `request` \| `response` | which half this span is |
| `lineage.direction` | both | `inbound` \| `outbound` | |
| `lineage.self.id` | both | `weather-service` | this workload's identity, from `self_id` or `self_id_file`, **reduced to its last non-empty `/`-segment**: a SPIFFE ID `spiffe://td/ns/team1/sa/agent` emits `agent`, and two identities that differ only above that segment emit the same value — the consumer keys entity identity on it (§7). The producer emits nothing without one: with no identity source configured it refuses to start, and while `self_id_file` is not yet readable it is not ready and skips every exchange (no span, no header) until the file resolves. Not capped by `max_attr_bytes` (an identity fact is never truncated; the value is operator configuration, not caller input) |
| `lineage.self.namespace` | both | `team1` | the Kubernetes namespace this workload runs in, from the `namespace` key or `namespace_file` (§6), trimmed of surrounding whitespace and otherwise verbatim; always an RFC 1123 DNS label (lowercase letters, digits and `-`, 1–63 chars), since the producer refuses any other shape. The other half of its identity: the consumer keys an entity on the (namespace, `self.id`) pair (§7), since the same `self.id` in two namespaces is two workloads. Never derived from the SPIFFE ID's path (a registrar convention, and the kit path has no SPIFFE ID); the producer refuses to start without one. Not capped by `max_attr_bytes`: an identity fact is never truncated |
| `lineage.peer.host` | both, when present | `weather-tool-mcp.team1.svc:8000` | the Host/authority header. Outbound: the service being called. Inbound: the address this workload was reached on |
| `lineage.protocol` | both | `a2a` \| `mcp` \| `inference` \| `http` | which parser matched, at fixed precedence `a2a` > `mcp` > `inference`; `http` = none. The precedence is load-bearing: the parsers are not mutually exclusive — `mcp-parser` attaches to any JSON-RPC body, including every a2a exchange — so an a2a hop is labeled `a2a`, never `mcp`. The payload reduction (§5) is keyed by this label, reading the same protocol's parser |
| `lineage.parent.source` | request | `tracestate` \| `wire` \| `none` | which precedence in §3.2 chose the parent. An audit fact; the consumer derives nothing from it |
| `http.method` | request, when the listener supplies it | `POST` | all listeners do |
| `url.path` | request, when present | `/mcp` | query-free: anything from `?` on is stripped before emission (per OTel semconv; the query can carry secrets and is never captured) |
| `url.scheme` | request, when present | `http` | the listener's observed scheme; `tcp` marks a proxy-sidecar CONNECT tunnel (§5 Limits). Optional: the consumer composes `scheme://peer.host + url.path` only when all three exist |
| `a2a.method`, `a2a.session_id` | request, a2a | `message/send` | parsed facts |
| `mcp.method`, `mcp.tool` | request, mcp | `tools/call`, `get_weather` | `mcp.tool` only for `tools/call` |
| `inference.model` | request, inference | `qwen2.5:7b` | from the parsed request body |
| `lineage.principal.sub`, `lineage.principal.client` | request, inbound, only when a gate plugin validated a JWT | `alice` | raw identity facts, never inferred from a network address |
| `input.value` | request, with `capture_io` | `{"city":"Tokyo"}` | see §5 |
| `output.value` | response, with `capture_io` | `{...}` | see §5; absent when unparsed or streamed |
| `http.status_code` | response, when a status was produced | `200` | |
| `lineage.outcome` | response | `ok` \| `denied` \| `error` \| `abandoned` | how the exchange ended as the proxy saw it; `ok` and `denied` are the pipeline's verdicts, with or without a status; `abandoned` is a nil outcome, or an error that never produced a status |
| `lineage.denied_by` | response, denials | `jwt-validation` | the plugin that denied |

`http.method` and `http.status_code` are the pre-1.21 OpenTelemetry semantic-convention keys, kept
deliberately: this producer's vocabulary is `lineage.*` plus these two well-known keys, and
interoperability with generic OpenTelemetry tooling is not a goal.

Span names: request = `{self.id} {protocol} {op}`, where op is `mcp.tool` (else `mcp.method`),
`a2a.method`, or `inference.model`, and is omitted when the protocol's parser yielded none — so an
exchange no parser claimed is named `{self.id} http`; response = the request name + ` response`.
The name is a bounded vocabulary by construction (backends build operation lists and group on it);
it never carries `url.path`, which is its own attribute on the request span.

Every variable-content string attribute above, and the request span name, is capped at
`max_attr_bytes` (default 256), cut on a UTF-8 boundary and suffixed `…[truncated]` — several of
these values are caller-controlled (`url.path`, `lineage.peer.host`, `mcp.tool`,
`a2a.session_id`) and the SDK never truncates on its own. `input.value` / `output.value` carry
their own `max_payload_bytes` cap (§5); the fixed-vocabulary facts and the hex ids are bounded by
construction.

## 5. Payloads

- `input.value` and `output.value` are the parsers' semantic reduction of the request and
  response: for a2a the message or artifact text, for mcp the tool arguments and the text content
  of a result, for inference the messages and the completion or tool calls.
- Two heuristics live in that reduction, affecting payloads only, never interactions: the a2a
  parser falls back to the status-message text when a result carries no artifact; and the lineage
  plugin suppresses an A2A protocol event from `output.value` when the reduced value is a JSON
  object whose `kind` is exactly one of `status-update`, `task-status-update`, `artifact-update`,
  `working`, `canceled`. Either can mislabel an unusual payload; since payload absence is legal, the
  failure mode is a missing or imprecise `output.value`, never a wrong interaction.
- A value longer than `max_payload_bytes` is cut on a UTF-8 boundary and suffixed `…[truncated]`,
  so the loss is visible in the span. A truncated value no longer parses as JSON; the consumer then
  stores it as a string. Deployments that want whole prompts set `max_payload_bytes: -1` or raise it
  (LLM chat prompts on the reference fleet reach 14 KB; a third exceed the 4096 default).
- In envoy-sidecar mode, TLS-passthrough connections bypass Envoy's HTTP filter chain entirely and
  produce no exchange — a capture gap. In proxy-sidecar mode an HTTPS destination is a CONNECT
  tunnel, which IS an exchange: an ordinary span pair with `http.method=CONNECT`,
  `url.scheme=tcp`, `lineage.peer.host` naming the dial target, no path and no payload — the bytes
  inside the tunnel are opaque. The producer does not filter tunnel exchanges; what they mean is
  the consumer's call, like every other fact.

## 6. Producer configuration

| key | default | meaning |
|---|---|---|
| `otel_endpoint` | `localhost:4317` | OTLP gRPC target; `host:port`, `http://host:port` or `https://host:port`; any other scheme is refused |
| `otel_tls` | `false` | TLS to the collector, verified against the system roots or `otel_ca_file`; an `https://` endpoint implies it. Refused contradictions: `https://` with `otel_tls: false`, `http://` with `otel_tls: true` or `otel_ca_file`. Plaintext to a non-loopback collector is allowed and logged as a WARN at start — spans carry principal facts, and payloads under `capture_io` |
| `otel_ca_file` | — | PEM bundle to verify the collector's certificate against, for a private CA; implies `otel_tls`, and `otel_ca_file` with `otel_tls: false` is refused. An unreadable file, or one with no certificate, refuses to start |
| `capture_io` | `false` | attach `input.value` / `output.value` |
| `max_payload_bytes` | `4096` | producer-side cap on those two values; `0` or unset takes the default, `-1` attaches whole, any other negative is refused at start |
| `max_attr_bytes` | `256` | cap on every variable-content string attribute and the span name (§4), except the two identity facts `lineage.self.id` and `lineage.self.namespace`, which are operator configuration and never truncated; same `0` / `-1` / negative semantics as `max_payload_bytes` |
| `mint_traceparent` | `true` | §3.3; `false` = a pure observer that never writes a `traceparent` |
| `bypass_paths` | `/.well-known/*`, `/healthz`, `/readyz`, `/health` | path globs (`path.Match`; `*` does not cross `/`) that produce no spans, matched by the shared bypass package (query stripped, path normalized) — the same key and semantics as `jwt-validation` and `sparc` |
| `bypass_hosts` | `otel-collector`, `otel-collector.*`, `jaeger`, `jaeger.*`, `zipkin`, `zipkin.*`, `prometheus`, `prometheus.*` | outbound host globs that produce no spans |
| `self_id` | — | this workload's identity (§4: reduced to its last `/`-segment); a blank value, or one with no non-empty `/`-segment (`/`), is refused at start — no name, no subject |
| `self_id_file` | `/shared/client-id.txt` | read when `self_id` is empty. Until it is readable and carries an identity the producer is not ready — every exchange skipped, nothing written to the wire — and it re-reads the file in the background, the sidecar's `/readyz` naming it meanwhile (a pod whose readiness probe uses `/readyz` stays out of rotation until the file lands — the `Readier` contract for a mounted credential, and in the stock chain `jwt-validation` already holds readiness on this same file); the process starts regardless, so the plugin never takes the sidecar's other plugins down over a late mount. Refused at start only when `self_id` is also empty |
| `namespace` | — | this workload's Kubernetes namespace, emitted as `lineage.self.namespace` (§4). Required (or `namespace_file`), and shape-checked: an absent or blank value, or one that is not an RFC 1123 DNS label, is refused at start — a `/` would make the consumer's `{kind}:{namespace}/{self.id}` key ambiguous. Resolved before anything else in the producer's start, so a refusal leaves nothing behind. The attach kit writes its `NAMESPACE`. A producer older than this key rejects a configuration that carries it (unknown keys are a boot error), so image and configuration change together |
| `namespace_file` | — | read once at start when `namespace` is empty; meant for `/var/run/secrets/kubernetes.io/serviceaccount/namespace`, which the kubelet projects from the pod's own metadata — the one source that is correct in every copy of a configuration shared across namespaces (the platform's per-namespace ConfigMap is rendered from one template and copied). Absent, blank or not a DNS label refuses at start; no default, no poller |

Setting `bypass_paths` or `bypass_hosts` **replaces** the default list rather than extending it —
the convention the `ibac`, `sparc` and `cpex` plugins use for their keys of the same name. An
operator who adds one entry must restate the defaults they want kept. An entry that would match
everything by its literal shape — empty, whitespace-only, `*` for a host, `*` or `/*` for a
path — is refused at start, as is an entry of either kind that is not valid `path.Match` syntax.
(The check is by shape, not by semantics: an exotic glob that happens to match every value, such
as `?*`, is the operator's own deliberate choice and is accepted — the same behaviour as the
sibling plugins' keys.)

Unknown keys are a boot error.

## 7. Consumer commitments

- Interaction id = `uuid5(NS_INTERACTION, f"{trace_id}/{exchange.id}")`. The request half fills
  caller, callee, request payload hash and started-at; the response half fills response payload
  hash, ended-at and error.
- **Whole-trace reconcile, not per-half upsert.** Every arriving span re-derives its entire trace
  from all stored spans of that trace: idempotent, order-independent, authoritative. Rows no longer
  justified by the current span set are deleted, trace-scoped; `entities` is global and never
  deleted. A half arriving alone still produces its row, so in-flight stays visible. The wanted set
  can shrink — an inbound request is a real interaction until its outbound ancestor arrives, then it
  demotes to the callee-side echo and its row is removed — which a per-half upsert cannot express.
  See ADR-0030.
- Anchors: `role=request` and either `direction=outbound`, or `direction=inbound` with no stored
  anchor ancestor (the trace entry). Entry detection tolerates both a NULL parent and a dangling
  wire parent, since an un-sidecared caller's root span is never exported.
- Response spans are never anchors; they attach by `exchange.id`.
- Kinds and entity identity come from the facts only. `classify()` never requires `input.value` or
  `output.value`; bodyless exchanges produce complete, first-class interaction rows with NULL payload
  hashes, and the UI renders them like any other row.
- A pod's entity identity is the pair (`lineage.self.namespace`, `lineage.self.id`), read from the
  request span (the response half is never consulted for identity): its natural key is
  `{kind}:{namespace}/{self.id}` (so the row id, uuid5 of the natural key, splits with it), and the
  namespace is stored again in `entities.namespace` for reading without parsing the key. The key
  is parseable because a namespace is a DNS label and never contains `/`; a present value that is
  not a DNS label (empty, padded, or any other shape) is a producer contract violation and halts
  the derivation loudly, as a missing `self.id` does. The callee of an outbound hop takes both facts
  from the callee's own echo span when it exists; the `peer.host` fallback, an LLM endpoint, a user
  and an anonymous client have no namespace. A span with no `lineage.self.namespace` at all — one a
  pre-v1.7 producer emitted, stored and replayable — keys its pod without a namespace, as v1.6 did:
  absence is recorded, never filled in from `peer.host`, a SPIFFE path or the trace. Two
  consequences follow and are accepted: traces recorded before the producer carried the fact keep
  their un-namespaced identities for good (a re-derivation reproduces them), so a pod has one
  entity row for its history and another from the cutover on unless the pre-v1.7 spans are dropped
  before a replay; and during a mixed rollout a v1.6 callee's echo under a v1.7 caller keys that
  callee without a namespace while its own v1.7 spans key it with one — one pod, two rows, for as
  long as both trace sets exist.
- The natural key is a consumed vocabulary, not only a column: `lineage_metadata.data_sources` /
  `.entities` store natural keys as text, the data-lineage traversal seeds on them, and any policy
  matching an entity by name must match the namespaced form.
- The consumer never welds a fragmented trace: an anchor whose parent is not a stored anchor derives
  as a root.
- `content_kind` vocabulary stays ADR-0014-compatible; the classification processor consumes
  `interaction_payloads` as a stream.
- Drain loop = the shared `data_governance/processors/_driver.py` StreamSpec.

## 8. Retired names

The producer must not emit these, and the consumer reads nothing from them.

| retired | replaced by |
|---|---|
| `lineage.hop.kind`, `trust.hop_kind` | consumer `classify()` over (direction, protocol, mcp.method) |
| `lineage.source.id`, `lineage.target.id`, `trust.source_id`, `trust.target_id` | `lineage.self.id` + `lineage.peer.host` + `lineage.direction`; caller and callee computed downstream |
| `enduser.id`, `trust.principal_id` | `lineage.principal.*` |
| `source=sidecar` | the resource `service.name=authbridge` |
| `openinference.span.kind` | not a producer attribute; a display backend derives it from `lineage.protocol` in a collector transform |
| `lineage.peer.addr` | nothing; it was never producible in ext_proc mode. Anonymous inbound callers derive as `client:(unknown)` |
| config `is_principal`, `emit_body_hash` | nothing |
| tracestate keys `kglin`, `dg-parent` | `lineage-parent` |

## 9. History

Version ladder, newest first. Each line is what changed on the wire or in the vocabulary; the
mechanisms named as removed are not to be reintroduced.

- **v1.7.0** — `lineage.self.namespace` on both spans, from a new **required** `namespace` config
  key or a `namespace_file` (absent, blank, or not a DNS label refuses at start, as a blank
  `self_id` does; resolved first, so a refusal leaves nothing behind); a `self_id` made only of
  separators (`/`), which the reduction would emit as-is, is now refused too, and a `self_id_file`
  carrying one keeps the producer not-ready like a blank file. Neither identity fact is capped by
  `max_attr_bytes` any more (`self.id` was): the pair the consumer keys on is never truncated. The reduction itself, its §4 clause
  and the by-design collision of same-named workloads across namespaces on `self.id` alone are
  unchanged — that clause is the documentation half (v1.6.1); the namespace is the identity half; the consumer keys a pod's
  entity on (namespace, `self.id`) — natural key `{kind}:{namespace}/{self.id}` — and stores the
  namespace in a new nullable `entities.namespace` column. Motivation: `self.id` is
  the last segment of the SPIFFE ID, so `team1/weather-service` and `team2/weather-service` derived
  as one entity, and every interaction of both pods pointed at one row. The namespace is a
  configured fact, not parsed out of the SPIFFE path — the `ns/…/sa/…` layout is a registrar
  convention, and a kit-attached pod has no SPIFFE ID at all. `lineage.self.id`, its reduction, and
  span names are unchanged. Additive on the wire, breaking in configuration: every existing
  `lineage-telemetry` block needs the key.
- **v1.6.3** — an unreadable or blank `self_id_file` no longer refuses to start: the producer starts
  not-ready, skips every exchange (no span, no header) and re-reads the file until an identity
  appears. Until now the refusal failed the whole sidecar — every plugin in its chain — over a
  Secret that the platform mounts after the pod starts. Refusal remains for a missing identity
  source (neither `self_id` nor `self_id_file`) and for a blank `self_id`. And the span name no longer falls back to `url.path`
  when no parser named an operation: an unparsed exchange is `{self.id} http`, so the set of
  span names stays bounded (a `/tasks/{id}` surface minted one name per request); `url.path` is
  unchanged as an attribute. Prose: §3.4 no longer describes a refused stamp on a malformed inbound
  `tracestate` — the extractor drops such a list before the stamp, which is then written alone.
  Nothing else on the wire changes.
- **v1.6.2** — `url.path` and the span-name fallback derived from it are query-free: the producer
  strips anything from `?` on before emission. Until now the envoy-sidecar listener's raw `:path`
  pseudo-header put the query string on the wire regardless of `capture_io`; the proxy listeners
  never delivered it. And every variable-content string attribute plus the span name is capped at
  `max_attr_bytes` (default 256) — until now only the two payload values were bounded, so one
  request could put a 100 KB span name into the backend. And the producer samples unconditionally
  (§2): under the SDK-default ParentBased sampler a caller's sampled-out `traceparent` (`…-00`)
  exported zero spans for the whole chain. And `bypass_paths` becomes a `path.Match` glob list via
  the shared bypass matcher (it was a prefix match): the `/health` default no longer swallows
  `/health-records/...`, and a pattern copied from a sibling plugin means the same thing here.
  Prose: the `lineage.protocol` precedence (`a2a` > `mcp` > `inference`), always the producer's
  behaviour, is stated in §4, and §2's lone-request-span causes gain config hot-reload.
- **v1.6.1** — prose and configuration only; spans and wire unchanged. `lineage.self.id` is documented
  as reduced to its last `/`-segment before emission, which the producer has always done;
  `otel_ca_file` added for a collector under a private CA; `bypass_hosts` becomes an outbound-only
  `path.Match` glob list (it was an unanchored substring match on both directions) and both bypass
  lists are validated at start; `-1` is stated as the only `max_payload_bytes` opt-out, with any
  other negative refused rather than silently unbounded.
- **v1.6** — an invalid or absent `traceparent` is restarted per W3C (`mint_traceparent`);
  `lineage.parent.source` gains `none`; the stamp key becomes `lineage-parent`; `otel_tls` and
  `max_payload_bytes` added; the document is vendored into the producer repository. Motivation:
  one traceparent-less turn through a four-pod fleet derived nine roots instead of one — the
  application's shim minted the trace, but with no `traceparent` to ride on the entry's stamp never
  reached the application's outbound calls.
- **v1.5** — the single channel: both directions parent stamp-first; the outbound `traceparent`
  rewrite ("the splice", v1.2–v1.4) removed; `lineage.peer.host` read on both directions;
  `url.scheme` added.
- **v1.4** — `lineage.peer.addr` removed; the listener header diff made live on all handler
  paths (until then the stamp never reached the wire in ext_proc mode, the mechanical cause of the
  phantom-rooted per-pod trees stored before it).
- **v1.3** — the trace-keyed inbound map removed: it answered "the last inbound seen for this
  trace", correct only while exactly one inbound of that trace is in flight, a precondition it could
  not verify, and under same-trace concurrency it produced a real, exported, untrue parent with no
  signal. A census before removal found zero spans attributed by it. `lineage.parent.source` added.
- **v1.2** — the `tracestate` stamp introduced (then keyed `kglin`), proven under same-trace
  fan-in: six concurrent same-trace turns through a mid-chain agent paired 1/6 by the map and 6/6
  by the stamp.

Naming decisions, not to be re-litigated: span attributes stay `lineage.*` — they are this
producer's own telemetry and name the domain, not a product. The `tracestate` key is a shared
channel every intermediary must preserve, so it carries a producer-owned, consumer-neutral name.

Open item: the response-span name suffix (` response`) is cosmetic, for trace-viewer legibility
only.
