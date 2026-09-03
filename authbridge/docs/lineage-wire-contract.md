# Lineage wire contract — two-span sidecar lineage (v1.6.1)

What the AuthBridge `lineage-telemetry` plugin emits, what it writes onto the wire, and what the
data-governance `sidecar` interactions algorithm (ADR-0030) commits to when consuming it.

- Producer: `cortex/authbridge/authlib/plugins/lineage/` (repo `rossoctl/cortex`).
- Consumer: `data_governance/processors/interactions/sidecar.py`; vocabulary in
  `data_governance/sidecar_facts.py` (repo `rossoctl/lab-data-governance`).

This document is kept **byte-identical in both repositories** — `cortex/authbridge/docs/lineage-wire-contract.md`
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
- A lone request span means one of three things: the sidecar died mid-exchange; the plugin
  recovered a panic while emitting the response span (a WARN is logged); or the response span was
  emitted but lost — the two halves enter a batching exporter an exchange apart, so a response can
  be lost after its request has flushed. The consumer renders it as in-flight, never as a wrong
  pairing. A response span whose `lineage.outcome` is absent derives with `error` NULL (honest
  unknown), never `false`.
- **Scope of `denied`.** The lineage plugin runs after the gate plugins and the pipeline
  short-circuits on a request-phase denial, so an exchange a gate rejects **before** the request
  span exists emits **no spans at all** and is invisible to lineage. `denied` appears only for
  denials after the request span exists: response-phase denials, or gates ordered after lineage.
  Spans for gate-denied traffic are a named follow-up, not current behaviour.
- **Bypass.** Requests whose path starts with a `bypass_paths` prefix, or whose host contains a
  `bypass_hosts` substring, produce no spans (defaults in §6).

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
| `tracestate` | every exchange, both directions, whenever a valid context exists after §3.3 | the caller's members with `lineage-parent` set to this request span id |
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

## 4. Attributes

Resource attributes: `service.name=authbridge`, `authbridge.component=lineage-telemetry`.

| key | on | example | notes |
|---|---|---|---|
| `lineage.exchange.id` | both | `00f067aa0ba902b7` | the request span id, hex |
| `lineage.role` | both | `request` \| `response` | which half this span is |
| `lineage.direction` | both | `inbound` \| `outbound` | |
| `lineage.self.id` | both | `weather-service` | this workload's identity, from `self_id` or `self_id_file`, **reduced to its last non-empty `/`-segment**: a SPIFFE ID `spiffe://td/ns/team1/sa/agent` emits `agent`, and two identities that differ only above that segment emit the same value — the consumer keys entity identity on it (§7). The producer refuses to start without an identity |
| `lineage.peer.host` | both, when present | `weather-tool-mcp.team1.svc:8000` | the Host/authority header. Outbound: the service being called. Inbound: the address this workload was reached on |
| `lineage.protocol` | both | `a2a` \| `mcp` \| `inference` \| `http` | which parser matched; `http` = none |
| `lineage.parent.source` | request | `tracestate` \| `wire` \| `none` | which precedence in §3.2 chose the parent. An audit fact; the consumer derives nothing from it |
| `http.method` | request, when the listener supplies it | `POST` | all listeners do |
| `url.path` | request, when present | `/mcp` | |
| `url.scheme` | request, when present | `http` | the listener's observed scheme. Optional: the consumer composes `scheme://peer.host + url.path` only when all three exist |
| `a2a.method`, `a2a.session_id` | request, a2a | `message/send` | parsed facts |
| `mcp.method`, `mcp.tool` | request, mcp | `tools/call`, `get_weather` | `mcp.tool` only for `tools/call` |
| `inference.model` | request, inference | `qwen2.5:7b` | from the parsed request body |
| `lineage.principal.sub`, `lineage.principal.client` | request, inbound, only when a gate plugin validated a JWT | `alice` | raw identity facts, never inferred from a network address |
| `input.value` | request, with `capture_io` | `{"city":"Tokyo"}` | see §5 |
| `output.value` | response, with `capture_io` | `{...}` | see §5; absent when unparsed or streamed |
| `http.status_code` | response, when a status was produced | `200` | |
| `lineage.outcome` | response | `ok` \| `denied` \| `error` \| `abandoned` | how the exchange ended as the proxy saw it; `abandoned` = no status was ever produced |
| `lineage.denied_by` | response, denials | `jwt-validation` | the plugin that denied |

`http.method` and `http.status_code` are the pre-1.21 OpenTelemetry semantic-convention keys, kept
deliberately: this producer's vocabulary is `lineage.*` plus these two well-known keys, and
interoperability with generic OpenTelemetry tooling is not a goal.

Span names: request = `{self.id} {protocol} {op}`, where op is `mcp.tool` (else `mcp.method`),
`a2a.method`, or `inference.model`, falling back to `url.path`, and is omitted when empty;
response = the request name + ` response`.

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
- TLS-passthrough connections bypass the HTTP pipeline entirely and produce no exchange. That is a
  capture gap, not a derivation rule: once such a connection is seen, the same rules apply.

## 6. Producer configuration

| key | default | meaning |
|---|---|---|
| `otel_endpoint` | `localhost:4317` | OTLP gRPC target; `host:port`, `http://host:port` or `https://host:port`; any other scheme is refused |
| `otel_tls` | `false` | TLS to the collector, verified against the system roots or `otel_ca_file`; an `https://` endpoint implies it. Refused contradictions: `https://` with `otel_tls: false`, `http://` with `otel_tls: true` or `otel_ca_file` |
| `otel_ca_file` | — | PEM bundle to verify the collector's certificate against, for a private CA; implies `otel_tls`, and `otel_ca_file` with `otel_tls: false` is refused. An unreadable file, or one with no certificate, refuses to start |
| `capture_io` | `false` | attach `input.value` / `output.value` |
| `max_payload_bytes` | `4096` | producer-side cap on those two values; `-1` attaches whole |
| `mint_traceparent` | `true` | §3.3; `false` = a pure observer that never writes a `traceparent` |
| `bypass_paths` | `/.well-known/`, `/healthz`, `/readyz`, `/health` | path prefixes that produce no spans |
| `bypass_hosts` | `otel-collector`, `jaeger`, `zipkin`, `prometheus` | host substrings that produce no spans |
| `self_id` | — | this workload's identity (§4: reduced to its last `/`-segment) |
| `self_id_file` | `/shared/client-id.txt` | read when `self_id` is empty; the producer refuses to start if neither yields an identity |

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

- **v1.6.1** — prose and configuration only; spans and wire unchanged. `lineage.self.id` is documented
  as reduced to its last `/`-segment before emission, which the producer has always done;
  `otel_ca_file` added for a collector under a private CA.
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
