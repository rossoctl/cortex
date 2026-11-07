# Praxis mode: Cortex with no plugins, Praxis does the processing — design

Date: 2026-11-07
Issue: rossoctl/cortex#1714 (switchover tracker)
Branch: `docs/praxis-mode-spec`
Background: #1437 (assessment, closed), #1491 (hairpin spec and W1 spike results)

## Problem

The end state of #1714 is one network proxy on the laptop: Praxis. Cortex then is not a proxy. It keeps the attribution logic, the session store and archive, the ledger and the session API that agentop uses.

Praxis cannot be that proxy today. It has no CONNECT runtime in a release (praxis#1479), no TLS interception (praxis#782) and no client port for filters (praxis#1447).

We want an interim state now that:
- turns off every plugin in the Cortex pipeline, so that Praxis does all request processing.
- keeps agentop working as it does today: sessions, timeline, tokens, cost, tool calls and titles.
- is built from parts that survive into the end state. Only the parts that upstream Praxis replaces are throwaway.

## Goals

- A config switch, `praxis.enabled`, that sends all bridged and plain proxied HTTP traffic through a Praxis binary that we build and ship (`cortex-gateway`).
- In this mode, the Cortex outbound pipeline is empty. Cortex refuses to start with plugins configured.
- Praxis parses the traffic (inference, MCP, A2A) with Rust ports of Cortex's parsers, and records the rows.
- Cortex receives those rows through an ingestion endpoint. It persists them in its archive, caches them in its store and prices them. It serves them to agentop through the session API.
- The parsed rows match direct mode, field by field, on the saved real-traffic fixtures.

## Non-goals

- Any other plugin in `praxis` mode: inference-router (`agentop server`), tool-prune, script, context-guru, ibac, sparc, opa, jwt-validation, static-inject and cpex are off. Each one is a later port.
- Praxis checks (guardrails, limits). This design moves the processing, not the policy.
- The end-state attribution service and Praxis-only CONNECT. Both are specified here (Section 2) but built only when praxis#1447, #1479 and #782 land.
- WebSockets through Praxis. In the interim they stay direct from Cortex (#1491 M1).
- Showing Praxis metrics in agentop.
- The MOCA VM control channel (`trustedSessionOf`). It is not a laptop feature.
- Kubernetes shapes (envoy-sidecar, proxy-sidecar in a pod).

## Decisions this design takes

These were open on #1714:
- **D1:** the session store, archive, agentop and the ledger stay in Cortex. Praxis feeds them.
- **D3:** attribution stays Go logic in Cortex. In the end state Praxis calls it as a local service.
- **D4:** Cortex's processing code runs in Praxis as Rust filters, in our own binary built on the published `praxis-proxy` crates. It is not the Go pipeline linked into Praxis as a C library (#1437's work breakdown).
- **D0:** the hairpin is built, but for processing and recording in Praxis, not for new checks.

Still open:
- D2: who owns prices long term. In this design Cortex settles at ingestion.
- D5: the stop-loss date and the owner of version bumps.

## Architecture

```
INTERIM (this design)
agent ─► Cortex forward proxy :47600   CONNECT, TLS bridge, attribution (in process)
           │  no plugins. Hop: dial Praxis, inject X-Cortex-* attribution, request id, token
           ▼
         cortex-gateway :47605         cortex_attribution → cortex_parse → cortex_record
           │                           (input: headers)     (Rust parsers) (emits rows)
           ▼
         Cortex egress listener :47606 ─► origin                     [throwaway]

END STATE (#1714 phase C)
agent ─► cortex-gateway :47600         CONNECT (#1479), decryption (#782), client port (#1447)
           │                           cortex_attribution → cortex_parse → cortex_record
           │                           (input: Cortex attribution service)
           ▼
         origin                        (Praxis dials, #1480)

BOTH:  cortex_record ──NDJSON──► Cortex /v1/ingest ─► store / archive / usage / ledger
                                                       ─► session API :47601 ─► agentop
```

**Survives into the end state:**
- the filter crate (parsers, row builder, emitter, attribution filter).
- the ingestion endpoint and the event schema as the contract.
- the attribution code, used in process now and as a service later.
- packaging: the `cortex-gateway` binary, supervision and the release pin.

**Throwaway:**
- the hop and the egress listener (replaced by praxis#1480).
- the Cortex forward proxy, the TLS bridge and Cortex's tunnel rows (replaced by #1479, #782 and #1447). These exist today, so they cost nothing new.

### Who records which rows

- **Praxis (`cortex_record`) records** each request and response row: bytes, timing, status, errors, `client_canceled`, the parsed facts and the capture. The W1 spike measured that an agent hang-up reaches Praxis in about 2.5 ms. So in the interim, Praxis sees the agent side accurately.
- **Cortex records only** what Praxis cannot see in the interim:
  - its own refusals before the hop (egress allow list, misdirected Host).
  - `gateway_unavailable` when Praxis is down or not ready.
  - CONNECT tunnel rows.
  - WebSocket rows.

  It writes these rows with `store.Append` in process, as it does now.
- In the end state Praxis records all of these too.

## 1. Event ingestion contract

### Endpoint

- `POST /v1/ingest` on the session API listener (loopback, `:47601` on a local install). The body is NDJSON, one line per write. `GET /v1/events` stays the SSE stream.
- Each `cortex-gateway` process keeps one long-lived streaming connection open, so lines arrive in order and with low latency. It reconnects with backoff.
- **Auth:** a bearer token. Cortex creates `~/.cortex/gateway.token` (mode 0600) at start and passes its path to the child. Without a valid token the answer is 401.
- **Guards** from `DELETE /v1/sessions` also apply: a loopback `Host` and no `Origin`.
- Reason: the session API has no authentication, but this endpoint writes the ledger (#1027).

### Line format, version 1

```json
{"v":1, "op":"append", "session":"<attributed session id>",
 "event":   { "...": "SessionEvent wire JSON, as in core/pipeline/testdata/session-event.golden.json" },
 "capture": { "original":"<base64>", "forwarded":null, "header":{}, "query":"", "response":{} },
 "cost":    { "headers": {"X-Litellm-Response-Cost":"…", "Content-Type":"…"} }}
```

- `v` is the line format version. `event` carries the session API's `schemaVersion`. Cortex refuses a line with an unknown `v` or another `schemaVersion`.
- **Version 1 has only `op: "append"`.** `pair` (tunnel open and close as one write) and `rekey` come in the end state, when Praxis does CONNECT and they have a caller.
- The store files the event under `session` and assigns `seq` and `sessionId`, as a listener's `Append(sid, ev)` does. Producer values for `seq` and `sessionId` are ignored. Numbering cannot collide with the archive.
- The request row and the response row share one `requestId`. The response row names the same `session` as its request.
- `capture` has the shape of `model.RequestCapture` (bodies as base64). It is optional. Cortex calls `AppendCapture` when it is present and the store takes captures.
- `cost.headers` holds only the cost-relevant response headers: `X-Litellm-Response-Cost`, `-Original`, `-Discount-Amount`, `-Margin-Amount` and `Content-Type`. It is present on response rows only.

### Cost

- Cortex settles at ingestion: `settle.SettleInput(Input{Host, Path, Inference, ResponseHeaders: cost.headers}, rates)`. It writes the result to `plugins["cost"]` as `inference-parser` does today.
- **Cortex does not trust a producer's cost record.** It removes `plugins["cost"]` and the legacy `plugins["litellm-budget-track"]` from each ingested event before it settles. This answers #1497's design question 2.
- The avoided-cost estimate does not apply: tool-prune is off in `praxis` mode.

### Credentials in the capture

- **The producer removes credentials** before it sends a capture, by the rules of `core/redact` (`redact.Headers`, `redact.Query`), which the forward proxy applies today (`forwardproxy/server.go:592`, `:907`). The archive stores what it gets (`model.RequestCapture`'s contract).
- The Rust redaction is a port of `core/redact`, tested against the same cases (Section 5).
- As a second line of defense, ingestion runs `redact.Headers` and `redact.Query` again on each capture. The cost is one header scan per row.

### Failure rules

- **Recording never holds up traffic.** The emitter has a bounded queue. When the queue is full or Cortex is unreachable, it drops the line and counts it.
- Cortex skips a refused line (bad JSON, unknown `v`, wrong `schemaVersion`, over the 64 MiB line cap) and counts it by reason. The connection stays open.
- The counts are on Praxis `/metrics` (`cortex_rows_emitted_total`, `cortex_rows_dropped_total`, `cortex_parse_errors_total`) and on Cortex `/stats` (`ingest_lines_total`, `ingest_refused_total{reason}`).
- A request row without its response row (Praxis restarted mid-stream) shows the same way an in-flight row shows in agentop today.

## 2. Attribution contract

### Interim: Cortex resolves, the headers carry the result

- The forward proxy resolves attribution as today: the process chain from the socket (`Attributor.Chain`), then `Attributor.Resolve` with the session-id headers.
- On the hop it strips any client copy (`attribution.Strip`, already at entry), then injects:
  - the seven `core/attribution/wire.go` headers: `X-Cortex-Agent`, `X-Cortex-Proc`, `X-Cortex-Client-Proc`, `X-Cortex-Session`, `X-Cortex-Source`, `X-Cortex-Session-Header`, `X-Cortex-Chain-Session`.
  - `X-Cortex-Request-Id`, so that both rows and Cortex logs share an id.
  - `X-Cortex-Dest` and `X-Cortex-Token` (the hop, from #1491).
- `cortex_attribution` accepts these headers **only when the token is valid**. Without it (any other local process that talks to `:47605`), it removes them and treats the request as unattributed.
- It passes the result to `cortex_record` as filter metadata. It removes every `X-Cortex-*` header before the request leaves Praxis.
- Claims and touches need no new code: `Attributor` is a store Recorder, so ingested rows update it as listener rows do.
- The `session` that a row is filed under follows today's rule (`Attribution.RecordSession`), computed in Cortex and carried in `X-Cortex-Session`.

### End state: Praxis asks Cortex

- `cortex_attribution` calls `POST /v1/attribute` on loopback with the client and server address (praxis#1447) and the request's session-id headers. Cortex runs the same `Lookup` and `Resolve`, and answers with the same seven fields as JSON.
- The process-chain part is cached per downstream connection, as `attribution.Memo` does today. `Resolve` runs per request, because it depends on per-request headers and on Cortex's claim state.
- Same token and guards as `/v1/ingest`.
- **This endpoint is built only when praxis#1447 exists.** Before that nothing can call it ("an API lands with its caller").

### Self-traffic

- Interim: Cortex's self-traffic rule and `skip_hosts` run before the hop, so that traffic never reaches Praxis.
- `cortex-gateway`'s own outbound traffic is exempt by pid and start time. Cortex starts the child, so it knows both (#1491 W6). A match on the executable name is not used: any binary named `praxis` would skip the checks.
- End state: these rules move into `cortex_attribution`, with the decision from the service.

## 3. The filter crate and the binary

### Where the code lives

A new Rust workspace `praxis/` in this repository:
- `praxis/filters`: crate `cortex-praxis-filters`, with the three filters and the parser modules.
- `praxis/server`: binary **`cortex-gateway`**. It depends on the published `praxis-proxy` crates at exactly `=0.8.0` and registers our filters with `register_filters!` (Praxis's "manual registration", `docs/filters/extensions.md`). No Praxis fork.

The name keeps clear of `cmd/cortex-praxis`, the paused Go converter, which stays (CLAUDE.md).

### The chain, in order

1. **`cortex_attribution`** (request head): checks the token, reads and removes the attribution headers, and sets filter metadata (Section 2).
2. **`cortex_parse`** (read-only body access):
   - It reads the request body buffered, up to the body limit. Claude Code requests reach about 5 MiB.
   - It reads `text/event-stream` responses in stream mode, frame by frame, so streaming latency and memory stay as in direct mode. It reads other responses buffered, up to the limit.
   - **It decodes gzip and zstd itself, for parsing only.** The bytes on the wire do not change. The W1 spike found that praxis-ai's `token_count` misses gzip-coded JSON. This rule removes that gap without upstream ask U6.
   - It writes the parsed request and response into filter metadata in Cortex's types (`inference`, `mcp`, `a2a`).
3. **`cortex_record`:**
   - It builds the request row when the request is parsed, and hands it to the emitter at once, so agentop shows it live.
   - It builds the response row at the end of the stream, with status, bytes, timing (`ttfbMs`, `durationMs`), the parsed response and the capture.
   - It uses `emit_deferred_record` for two cases:
     - a stream that ends early: `error.kind: client_canceled` when the downstream went away, `upstream_error` when the upstream did.
     - a request rejected before the upstream, for example by a body limit: `gateway_refused`.

### Row fields keep their documented meaning

- Every field that `cortex_record` writes has the meaning that `docs/session-api.md` gives it today. Where Praxis sees other bytes than Cortex did, `cortex_record` computes the documented value. Example: when Cortex decodes a gzip body today, Praxis sees the encoded body, so `cortex_record` computes the byte counts the same way Cortex does.
- Timing fields (`ttfbMs`, `durationMs`) are measured on the Cortex → Praxis leg in the interim. The W1 spike measured the whole loop at about 0.4 ms (p50) above direct mode. The end-to-end tests compare timing within a tolerance (Section 5).

### The parsers

- They are ports of `core/parse/inference`, `core/parse/mcp` and `core/parse/a2a`, moved one parser at a time, inference first.
- **The parity gate:**
  - A Go test writes the expected event JSON next to each saved real-traffic fixture from #1503.
  - A Rust test parses the same fixture and must give the same JSON.
- The Go parser stays the reference until its row on #1714 moves to "Praxis only". While both exist, a change to one without the other fails CI.

### The emitter

- One background task per process, with a bounded channel (default 4096 lines) and one streaming `POST /v1/ingest` with the token.
- It exposes the counters from Section 1 on Praxis `/metrics`.

## 4. Packaging, the mode switch and failure modes

### Config

```yaml
praxis:
  enabled: true            # default false
  listen: 127.0.0.1:47605
  egress: 127.0.0.1:47606
```

- **With `praxis.enabled`, Cortex refuses to start when `pipeline.outbound.plugins` is not empty**, with a message that names the plugins. Two pipelines would parse twice and record twice.
- The `praxis` block needs a restart to change, like `listener` (`validateReloadable`).
- In this mode, Cortex's own request capture is off for hopped requests, because `cortex_record` captures them.

### What goes through Praxis

| Traffic | In `praxis` mode |
|---|---|
| Bridged HTTPS (decrypted by Cortex) | through Praxis |
| Plain proxied HTTP | through Praxis (#1491 H8) |
| Opaque tunnels (passthrough hosts, programs that refuse the CA) | direct from Cortex, as today |
| `skip_hosts`, self-traffic | direct, no row, as today |
| `cortex-gateway`'s own traffic | direct (exempt by pid and start time) |
| WebSocket upgrades | direct from Cortex, recorded by Cortex (#1491 M1) |

### The hop

- After the egress allow list, and in place of the pipeline: Cortex sets only `r.URL.Scheme` and `r.URL.Host` to the gateway. It leaves `r.Host` and the path unchanged, and injects the headers from Section 2. This is the W1 spike's dial-only rewrite, not `pctx.Redirect` (#1491 W2).
- The hop's transport reads no proxy env. Cortex → Praxis can use h2c, which Praxis accepts (W1 item 4). Connection reuse comes first. h2c is an option if the connection count matters.

### The egress listener (throwaway)

- Token check, then `X-Cortex-Dest`. It strips both headers and relays the stream through `tlsbridge.NewUpstreamClient`, which keeps `upstream_ca_bundle` and `upstream_insecure` (#1491 H7).
- It records nothing. The egress allow list already ran before the hop.

### Supervision

- Cortex starts `cortex-gateway` as its child, with the restart and backoff rules of `cmd/cortex/supervise.go`.
- The child gets a clean env (no proxy variables).
- Cortex generates the child's config at `~/.cortex/gateway.yaml`. It holds:
  - the listener and the chain from Section 3.
  - the catch-all route to the egress listener.
  - `insecure_options.allow_private_endpoints: true`.
  - `body_limits` of 64 MiB each way.
  - the admin metrics port and the token file path.
- Users do not edit `gateway.yaml`. Cortex writes it again at each start.

### Fail closed

- At startup Cortex sends a probe through the whole chain before it uses the hop (#1491 H1, W1 item 7).
- Until the probe passes, and while the child is down, each request that would go through Praxis gets **503**. Cortex records it with the new error kind **`gateway_unavailable`**. There is no fallback to direct mode. A silent fallback would skip what Praxis is meant to do (#1491's recorded decision).

### agentop

- Sessions, the timeline, tokens, cost and `/v1/usage` do not change.
- `agentop server …` and the script commands refuse in `praxis` mode, with a message that says why.
- The plugin pane is empty in this mode.

### Release

- `release-binaries` builds `cortex-gateway` for darwin/arm64 and linux/amd64 and arm64.
- The interim is opt-in: `install.sh --praxis`, in the way `--cpex` works. `agentop setup` installs the binary next to `cortex`.
- With `--praxis`, setup writes the `praxis` block and an empty outbound pipeline. The default local config has plugins, and the startup check would refuse it.
- `praxis.enabled` stays `false` by default until the parity gates pass and the live check (Section 5) is done.

## 5. Testing

### Parser parity (the gate for each parser)

- Every #1503 fixture runs through the Go and the Rust parser. The event JSON must be the same.
- CI runs it on each PR that touches `core/parse/` or `praxis/filters/`.

### Contract tests (Go)

- Ingestion:
  - 401 without the token, and a refused `Origin`.
  - each refusal reason is counted, and the connection stays open.
  - a producer's `plugins["cost"]` is removed, and the cost is settled again from `cost.headers`.
  - the store assigns `seq` and ignores a producer's.
  - credentials in a capture are removed again.
- **The replay test from #1497:** an NDJSON capture through `/v1/ingest` gives the same `/v1/sessions` and `/v1/usage` as the live run.
- Config: `praxis.enabled` with a non-empty outbound pipeline refuses to start.
- The hop: a plugin cannot steer it (the W1 test `TestSpikeHairpin_OnlyTheDialTargetMoves`, rewritten), and a client's `X-Cortex-*` headers never reach Praxis.

### Filter tests (Rust)

- Attribution headers are accepted only with a valid token, and always removed before the upstream.
- Credential removal matches `core/redact` on the same cases.
- A mid-body abort gives `client_canceled`. A body-limit refusal gives `gateway_refused`.
- gzip and zstd responses parse, and the wire bytes do not change.

### End-to-end (CI, Linux)

- The chain: Cortex, `cortex-gateway`, the egress listener and a mock origin.
- **The W1 checks become regression tests:**
  - item 1: host and cost match direct mode.
  - item 3: gzip-coded SSE arrives as frames.
  - item 5: a hang-up gives `client_canceled` and closes the origin connection.
  - item 7: fail closed at startup.
  - item 8: a 65 s silence mid-stream.
  - item 9: more than 10 MiB each way.
- **Direct mode against `praxis` mode** on the same mock traffic: the `/v1/sessions` events must be the same, field by field. Two exceptions: timing fields agree within 50 ms, and the `gateway_*` error kinds exist only in `praxis` mode.

### Live check (manual, before the default changes)

- Claude Code through `praxis` mode, with a scratch HOME and a scratch Cortex, never the shared proxy.
- Check in agentop: sessions, the timeline, tokens, cost, tool calls and titles.

## 6. Delivery

Each step is one PR and is useful when it merges. Steps 1–3 change nothing for a user until `praxis.enabled` is set.

1. **Ingestion endpoint** (Go): `/v1/ingest`, the token file, settle at ingestion, the replay test. The replay test is its first caller.
2. **The Rust workspace and the inference parser:** `praxis/filters` with `cortex_parse` for inference, and the parity gate in CI.
3. **`cortex_record` and `cortex_attribution`, and the `cortex-gateway` binary,** with the Rust filter tests.
4. **`praxis` mode in Cortex:** the config block, the startup check, the hop, the egress listener, supervision, the probe, `gateway_unavailable`, and the agentop refusals. The end-to-end tests come with it.
5. **Release:** `cortex-gateway` in `release-binaries`, `install.sh --praxis`, and `agentop setup`.
6. **The MCP and A2A parsers**, each behind its own parity gate.

Then the live check, then a decision to change the default.

## Open questions

1. **D2:** pricing stays in Cortex at ingestion in this design. Does it stay there in the end state?
2. **D5:** the stop-loss date for the interim, and the owner of `praxis-proxy` version bumps.
3. **Rust in CI and release:** this adds the first Rust toolchain to the repository's CI and release builds. The release must build `cortex-gateway` for three targets. CI time and cache size are not measured yet.
4. **Interim memory:** in `praxis` mode the capture lives in `cortex-gateway` until it is sent, then in Cortex. Peak memory per in-flight request may be about twice that of direct mode. Not measured.

## References

- #1714: the switchover tracker (phases, upstream asks, decisions).
- #1491: the hairpin spec and the W1 results comment.
- #1497: the ingestion items, closed and moved to #1714.
- #1503: the saved parser fixtures.
- praxis-proxy/praxis#1447, #1479, #1480, #782.
- Praxis `docs/filters/extensions.md` (manual registration), `crates/filter/src/filter.rs` (`emit_deferred_record`).
