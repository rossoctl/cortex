# Praxis mode: Cortex with no plugins, Praxis does the processing — design

Date: 2026-11-07. Revised the same day after three expert reviews: no regressions, development speed and throwaway.
Issue: rossoctl/cortex#1714 (switchover tracker)
Branch: `docs/praxis-mode-spec`
Background: #1437 (assessment, closed), #1491 (hairpin spec and W1 spike results)

Revision marks: **[R n]** = from the no-regressions review, **[D n]** = from the development-speed review, **[T n]** = from the throwaway review.

Second revision (2026-11-07): phase D added (Section 8). The end state is Cortex as a Praxis distribution, in the shape of praxis-ai: one Rust binary, with no Go service and no `/v1/attribute`.

## Problem

The end state of #1714 is one network proxy on the laptop: Praxis. Cortex then is not a proxy. It keeps the attribution logic, the session store and archive, the ledger, and the session API that agentop uses.

Praxis cannot be that proxy today:
- no CONNECT runtime in a release (praxis#1479).
- no TLS interception (praxis#782).
- no client port for filters (praxis#1447).

We want an interim state now that:
- turns off every plugin in the Cortex pipeline, so that Praxis does all request processing.
- keeps agentop working as it does today: sessions, timeline, tokens, cost, tool calls and titles.
- is built from parts that survive into the end state. Only the parts that upstream Praxis replaces are throwaway.
- changes nothing for users who do not turn it on.

## Goals

- A config switch, `praxis.enabled`, that sends all bridged and plain proxied HTTP traffic through a Praxis binary that we build and ship (`cortex-gateway`).
- In this mode the Cortex outbound pipeline is empty. Cortex refuses plugins at startup and at reload.
- Praxis parses the traffic (inference, MCP, A2A) with Rust ports of Cortex's parsers, and records the rows.
- Cortex receives those rows through an ingestion endpoint. It persists them in its archive, caches them in its store and prices them. It serves them to agentop through the session API.
- The rows match direct mode, field by field, on the same traffic (exceptions in Section 5).
- **Direct mode (`praxis.enabled: false`, the default) behaves exactly as before.** No new endpoint, file, error kind or code path runs in direct mode. **[R 10]**

## Non-goals

- Any other plugin in `praxis` mode: inference-router (`agentop server`), tool-prune, script, context-guru, ibac, sparc, opa, jwt-validation, static-inject and cpex are off. Each one is a later port.
- Praxis checks (guardrails, limits). This design moves the processing, not the policy.
- Phase D (Cortex as a Praxis distribution) and Praxis-only CONNECT. Both are specified here (Sections 7 and 8) but not built in v1.
- WebSockets through Praxis. In `praxis` mode, Codex's Responses WebSocket is declined, so Codex uses HTTPS through Praxis (Section 4). **[R 4] [T F4]**
- Showing Praxis metrics in agentop.
- The MOCA VM control channel (`trustedSessionOf`) and mesh mTLS. `praxis.enabled` is refused with `mtls` set. **[R 5]**
- Kubernetes shapes (envoy-sidecar, proxy-sidecar in a pod).

## Decisions this design takes

These were open on #1714:
- **D1:** the session store, archive and ledger stay in the Go Cortex in v1, and Praxis feeds them. In phase D they move into the binary (Section 8). agentop stays a Go client and does not change.
- **D3:** attribution stays Go logic in Cortex in v1. In phase D it moves into the binary together with the store, because the store's events feed its state (Section 8). There is no attribution service at any phase.
- **D4:** Cortex's processing code runs in Praxis as Rust filters, in our own binary built on the published `praxis-proxy` crates. It is not the Go pipeline linked into Praxis as a C library (#1437's work breakdown).
- **D0:** the hairpin is built, but for processing and recording in Praxis, not for new checks.

Also decided:
- **D2:** in v1, Cortex prices each row at ingestion with its existing Go code. Later, Cortex's pricing logic is ported into a Rust filter in `cortex-gateway`, so that Praxis prices each request itself. The ledger stays in Cortex (D1). When Praxis Tokenomics is on par with it, we switch to Tokenomics and remove the port.
- **D5:** no stop-loss date for the interim. The owner of `praxis-proxy` version bumps is decided later, as we go.

## Architecture

```
                 launchd / systemd ─► cortex -supervise                     [T F1]
                                        ├─ cortex            (child)
                                        └─ cortex-gateway    (child, sibling of cortex)

INTERIM (this design)
agent ─► cortex :47600 (forward proxy)   CONNECT, TLS bridge, attribution (in process)
           │  no plugins. Hop (hop.go): dial the gateway over HTTP/1, inject
           │  X-Cortex-Attribution, X-Cortex-Request-Id, X-Cortex-Dest, X-Cortex-Token
           ▼
         cortex-gateway :47605           cortex_attribution → cortex_parse → cortex_record → access_log
           │                             listener has a forward_proxy block (end-state shape)
           ▼
         cortex egress listener :47606 ─► origin                    [throwaway at phase B]

PHASE B (#1714: praxis#1479 + #1480 released)
agent ─► cortex :47600 ─► hop sends absolute form ─► cortex-gateway ─► origin   (egress listener removed)

PHASE D (no upstream trigger; Section 8)
agent ─► cortex :47600 (front door only) ─► hop + X-Cortex-Client ─► cortex binary ─► …
           CONNECT, TLS bridge                cortex_attribution (Rust, own lookup)
                                              cortex_record ─► store (SQLite, in process)
                                              session API :47601 ─► agentop

END STATE (phase C after phase D: #782, #1447)
agent ─► cortex :47600  — one Rust binary on Praxis, like praxis-ai —  ─► origin
           Praxis core: CONNECT, decryption, dial
           Cortex filters: attribution, parse, record, pricing
           Cortex store + session API :47601 ─► agentop

V1 AND PHASE B:  cortex_record ──NDJSON──► Go cortex /v1/ingest ─► store / archive / usage / ledger
                                                                   ─► session API :47601 ─► agentop
```

**Survives into the end state:**
- the filter crate (parsers, row builder, attribution filter).
- the line format, which becomes the store's record type in phase D.
- the attribution JSON type **[T F6]**, which becomes the Rust filter's own type in phase D.
- the session API contract (`docs/session-api.md`, the golden fixtures, `schemaVersion`). agentop sees no change at any phase.
- the supervisor layout (two siblings) and `gateway.yaml` in the `forward_proxy` listener shape **[T F1, F3]**.
- packaging: the `cortex-gateway` binary, the release job and the version pin.

**Throwaway:**
- the egress listener and `X-Cortex-Dest` (at phase B).
- the ingestion transport (`POST /v1/ingest`, the emitter's HTTP stream) and the `X-Cortex-Attribution` header (at phase D). Their types survive.
- the supervisor's second child and the token (at phase C, when one binary is left).
- the hop, at phase C. At phase B it only changes to absolute form. **[T F7]**
- the readiness probe and `gateway_unavailable` (at phase C).
- the Cortex forward proxy, the TLS bridge and Cortex's own tunnel rows (at phase C). These exist today, so they cost nothing new.

### Who records which rows

- **Praxis (`cortex_record`) records** each request and response row of a hopped request: bytes, timing, status, errors, `client_canceled`, the parsed facts and the capture.
- **Cortex records only** what Praxis cannot see in the interim, with `store.Append` in process, as now:
  - its own refusals before the hop (egress allow list, misdirected Host).
  - `gateway_unavailable`, only when the dial to the gateway fails, so Praxis never saw the request. If Praxis accepted the request, Praxis's rows cover it, and Cortex writes nothing. **[R 17]**
  - CONNECT tunnel rows. The open row of a bridged tunnel still pairs with the tunnel's first request row (Section 1, "Tunnel pairing"). **[R 3]**
  - WebSocket rows other than the Responses WebSocket (which is declined).
- In the end state Praxis records all of these too.

## 1. Event ingestion contract

### Endpoint

- `POST /v1/ingest` on the session API listener (loopback, `:47601` on a local install). The body is NDJSON, one line per write. `GET /v1/events` stays the SSE stream.
- **The route and the token file exist only when `praxis.enabled` and `listener.bind_loopback_only` are both set.** Cortex registers the route. The supervisor creates the token file (Section 4). In direct mode, and in a sidecar, neither exists. **[R 10]**
- Each `cortex-gateway` process keeps one long-lived streaming connection open, so lines arrive in order. It reconnects with backoff.
- **Auth:** a bearer token. The supervisor creates `~/.cortex/gateway.token` (mode 0600) at start and passes its path to both children. Without a valid token the answer is 401.
- **Guards** from `DELETE /v1/sessions` also apply: a loopback `Host` and no `Origin`.
- **Why POST is safe here:** `clear.go` uses DELETE because a simple cross-site POST needs no preflight. This route needs an `Authorization` header, which a browser cannot add to a simple cross-site request without a preflight. The token is the protection, and the guards are a second layer. **[R 10]**

### Line format, version 1

```json
{"v":1, "schemaVersion":1, "op":"append",
 "requestId":"<X-Cortex-Request-Id>",
 "session":"<recording session, from X-Cortex-Attribution>",
 "event":   { "...": "SessionEvent wire JSON (core/pipeline/testdata/session-event.golden.json)" },
 "capture": { "original":"<base64>", "forwarded":null, "header":{}, "query":"",
              "response":{ "status":200, "header":{}, "body":"<base64>", "decoded":true,
                           "truncated":false, "ended":true, "incomplete":false } },
 "cost":    { "headers": {"X-Litellm-Response-Cost":"…", "X-Litellm-Model-Name":"…", "Content-Type":"…"} }}
```

- `v` is the line format version. `schemaVersion` is the session API's (`sessionapi.SchemaVersion`). It is a top-level field, because events do not carry it. **[R 14]** Cortex refuses a line with an unknown `v` or another `schemaVersion`.
- **Version 1 has only `op: "append"`.** `pair` and `rekey` come in the end state, when Praxis does CONNECT and they have a caller.
- The store files the event under `session` and assigns `seq` and `sessionId`, as a listener's `Append(sid, ev)` does. Producer values for `seq` and `sessionId` are ignored.
- `session` is the **recording** session (`Attribution.RecordSession`, which can be a `pending:…` id or the `ActiveSession()` fallback). It is a field of its own, not `X-Cortex-Session`. **[R 12]**
- The request and response rows share `requestId`. The response row names the same `session` as its request.
- `capture` is a wire struct defined for this line (`model.RequestCapture` has no JSON tags) that maps one to one to `model.RequestCapture` and `model.ResponseCapture`. **[R 15]**
- `cost.headers` holds only the cost-relevant response headers: `X-Litellm-Response-Cost`, `-Original`, `-Discount-Amount`, `-Margin-Amount`, `X-Litellm-Model-Name` (for `servedModel`) and `Content-Type`. Response rows only. **[R 16]**

### Captures

- **Cortex keeps a capture only when the event has `inference`**, the same rule as `recordCapturedRequest` (`forwardproxy/record.go:21-25`). The body of any other request, such as an OAuth token refresh, can be a credential. **[R 11]**
- The response part keeps `MaxResponseCapture` (10 MiB) and the meaning of `Truncated`, `Ended` and `Incomplete`.
- **The producer removes credentials** by the rules of `core/redact` (`redact.Headers`, `redact.Query`), which the forward proxy applies today (`forwardproxy/server.go:592`, `:907`). The Rust redaction is a port of `core/redact`, tested on the same cases.
- As a second layer, ingestion runs `redact.Headers` and `redact.Query` again.

### Cost

- Cortex settles at ingestion: `settle.SettleInput(Input{Host, Path, Inference, ResponseHeaders: cost.headers}, rates)` and `SettleServedModel`. It writes the result to `plugins["cost"]` as `inference-parser` does today.
- **Cortex does not trust a producer's cost record.** It removes `plugins["cost"]` and the legacy `plugins["litellm-budget-track"]` from each ingested event before it settles. This answers #1497's design question 2.
- When the pricing port lands (D2), this rule changes. Cortex then accepts the cost record of a gateway that has the token. It settles only rows without one.

### Joins with Cortex's own state

Two cases need Cortex state that Praxis does not have. Both are keyed by `requestId`, set at the hop, and kept for at most 60 s.

- **Tunnel pairing.** **[R 3]** Today the open row of a bridged tunnel waits for the tunnel's first request row. `AppendPairCapture` then writes both at once, because agentop folds a tunnel row only into the row that follows it (`forwardproxy/tunnel.go:834-858`). In `praxis` mode the hop registers `requestId → tunnel`. When ingestion receives that request row, it writes it through the tunnel's existing pairing path. Without this, each CONNECT gets an extra unfolded row.
- **Cuts made by Cortex.** **[R 6]** When Cortex itself ends a hopped stream (the 5-minute idle cut, the per-frame cap), Praxis sees the downstream go away and would record `client_canceled`. The hop records `requestId → kind` (`proxy_error`). Ingestion replaces the response row's error kind with it.

### Failure rules

- **Recording never holds up traffic.** The emitter has a bounded queue. When the queue is full or Cortex is unreachable, it drops the line and counts it.
- Cortex skips a refused line (bad JSON, unknown `v`, wrong `schemaVersion`, over the 64 MiB line cap) and counts it by reason. The connection stays open.
- Counters: Praxis `/metrics` (`cortex_rows_emitted_total`, `cortex_rows_dropped_total`, `cortex_parse_errors_total`) and Cortex `/stats` (`ingest_lines_total`, `ingest_refused_total{reason}`).
- A request row without its response row (the gateway restarted mid-stream) shows the same way an in-flight row shows in agentop today.
- **Restarts are not made longer.** Lines still in the emitter's queue when Cortex shuts down are lost and counted. Cortex does not wait for them. (Restarts stay quick, not graceful.) **[R 18]**
- Rows arrive a few milliseconds after the bytes. Ingested rows take the `at` time the producer set, so agentop's order stays correct. **[R 18]**
- The readiness probe's requests carry `X-Cortex-Probe` with the token. `cortex_record` records nothing for them. **[R 17]**

## 2. Attribution contract

### One type, two transports **[T F6]**

- The attribution result is one Go type, `attribution.Wire`, with a JSON form:
  - the seven fields of `core/attribution/wire.go` (agent, proc, client proc, session, source, session header, chain session).
  - `recordSession`, the bucket a row is filed under (`Attribution.RecordSession`) **[R 12]**.
- **Interim transport:** the forward proxy puts it in one header, `X-Cortex-Attribution: <base64url JSON>`, next to the hop token.
- **Phase D:** the type becomes the Rust filter's own. The filter fills it from its own lookup and store, and the header goes away (Section 8).
- `cortex_attribution` has one decoder and one validator in v1. In phase D, the decoder goes and the validator stays.

### Interim: Cortex resolves, the header carries the result

- The forward proxy resolves attribution as today: the process chain from the socket (`Attributor.Chain`), then `Attributor.Resolve`.
- **At entry, it removes every hop header from the client's request:** the seven `wire.go` headers (as `attribution.Strip` does now), plus `X-Cortex-Attribution`, `X-Cortex-Dest`, `X-Cortex-Token`, `X-Cortex-Request-Id` and `X-Cortex-Probe`. This also closes the W1 finding M5. In direct mode the only effect is that those headers no longer reach an origin. **[R 17]**
- `cortex_attribution` accepts `X-Cortex-Attribution` **only when the token is valid**. Without it (any other local process that talks to `:47605`), it treats the request as unattributed.
- It passes the result to `cortex_record` as filter metadata, and removes every `X-Cortex-*` header before the request leaves Praxis.
- **Touch and claims stay in Cortex's request path.** `Attributor.Record` only marks a session live (`attribution.go:224`). The touch is a separate call (`defer s.touch`, `identity.go:242`). The hop keeps it. **[R 2]**

### Self-traffic

- Interim: Cortex's self-traffic rule and `skip_hosts` run before the hop (`forwardproxy/server.go:520-547`), so that traffic never reaches Praxis.
- The gateway's own outbound traffic is exempt by pid and start time. The supervisor starts it and passes both to `cortex` (an env var at spawn, and `~/.cortex/gateway.pid` after a gateway restart). A match on the executable name is not used. **[T F1]**

## 3. The filter crate and the binary

### Where the code lives

A new Rust workspace `praxis/` in this repository, **outside `go.work`**. The 11 Go modules, `go vet` and `go mod tidy -diff` do not change. **[D 3]**
- `praxis/filters`: crate `cortex-praxis-filters`, with the three filters and the parser modules.
- `praxis/server`: binary **`cortex-gateway`**. It depends on the published `praxis-proxy` crates at exactly `=0.8.0` and registers our filters with `register_filters!` (Praxis's "manual registration", `docs/filters/extensions.md`). No Praxis fork.
- **The crate uses only a small part of the Praxis API:** `HttpFilter`, `register_filters!`, `emit_deferred_record` and filter metadata. No subrequests and no `Protocol`. This keeps the cost of a `praxis-proxy` bump low. Praxis shipped 8 pre-releases from 2026-09-20 to 2026-11-07, some with breaking Rust API changes. **[D 4]**
- Rust tests build `HttpFilterContext` with Praxis's own test helpers, never with a struct literal, because praxis#1447 adds a field to it. **[T F10]**

The name keeps clear of `cmd/cortex-praxis`, the paused Go converter, which stays (CLAUDE.md).

### The chain, in order

1. **`cortex_attribution`** (request head): checks the token, decodes `X-Cortex-Attribution`, removes every `X-Cortex-*` header, and sets filter metadata (Section 2).
2. **`cortex_parse`** (read-only body access):
   - It reads the request body buffered, up to the body limit. Claude Code requests reach about 5 MiB.
   - It reads `text/event-stream` responses in stream mode, frame by frame, so streaming latency and memory stay as in direct mode. It reads other responses buffered, up to the limit.
   - **It decodes gzip and zstd itself, for parsing only.** The bytes on the wire do not change. The W1 spike found that praxis-ai's `token_count` misses gzip-coded JSON. This rule removes that gap without upstream ask U6.
   - It writes the parsed request and response into filter metadata in Cortex's types (`inference`, `mcp`, `a2a`).
3. **`cortex_record`:**
   - It builds the request row when the request is parsed, and hands it to the emitter at once, so agentop shows it live.
   - It builds the response row at the end of the stream: status, bytes, timing (`ttfbMs`, `durationMs`), the parsed response and the capture.
   - **It records a stream that ends early, and a request rejected before the upstream, from `emit_deferred_record`.** Kinds: `client_canceled` (the downstream went away), `upstream_error` (the upstream did), `gateway_refused` (for example a body limit).
   - It reads the egress listener's `X-Cortex-Upstream-Error` response header into the row's error kind (`upstream_dns`, `upstream_tls`, `upstream_timeout`, `upstream_refused`), and removes the header. **[R 6]**
4. **`access_log`** (Praxis built-in). **Required:** Praxis calls `emit_deferred_record` only when the chain contains `access_log` (`protocol/src/http/pingora/handler/logging_util.rs:31`), and only for the first filter that claims the record (`filter/src/pipeline/mod.rs:561`). `cortex_record` comes first, so it claims the record. Upstream ask: praxis#1228 removes this coupling. **[T F2]**

### Row fields keep their documented meaning

- Every field that `cortex_record` writes has the meaning that `docs/session-api.md` gives it today. Where Praxis sees other bytes than Cortex did, `cortex_record` computes the documented value. Example: when Cortex decodes a gzip body today, Praxis sees the encoded body, so `cortex_record` computes the byte counts the same way Cortex does.
- Timing fields (`ttfbMs`, `durationMs`) are measured on the Cortex → Praxis leg in the interim. The W1 spike measured the whole loop at about 0.4 ms (p50) above direct mode, in batches, not interleaved. The end-to-end tests compare timing within a tolerance (Section 5).

### The parsers

- They are ports of `core/parse/inference`, `core/parse/mcp` and `core/parse/a2a`, moved one parser at a time, inference first.
- **The Go parser is the source of truth.** The Go parser changes rarely, so the parity cost is small. But a direct-mode fix must never wait for Rust. **[D 1]**
- **The parity gate:**
  - A Go test writes the expected event JSON next to each saved real-traffic fixture from #1503. A Rust test parses the same fixture and must give the same JSON.
  - It **blocks** PRs that touch `praxis/`, the `cortex-gateway` release, and the change of the default.
  - It **reports, without blocking,** on PRs that touch only `core/parse/` or the Go plugin wrappers. The release check then shows the open gap.
- **The schema drift test:** a Go test writes the list of `SessionEvent` wire fields to a testdata file (the same list that `TestSessionEventWire_HasEveryDomainField` checks). A Rust CI test fails when that list has a field that `cortex_record` neither sets nor lists as "not set in praxis mode". Without it, a new Go field would be empty in `praxis` mode, and `schemaVersion` would not show it, because adding a field does not raise it. **[D 2]**

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

- **With `praxis.enabled`, Cortex refuses a non-empty outbound pipeline at startup and at reload.** The reloader refuses it like a `listener` change, and keeps the running pipeline (`core/reloader`, `validateReloadable`). Plugin lists normally reload without a restart, so a startup check alone would let a hand edit or `agentop tools scan --write` bring plugins back. **[R 7]**
- `praxis.enabled` with `mtls` set is refused at startup. **[R 5]**
- The `praxis` block needs a restart to change, like `listener`.
- Unknown YAML keys are ignored, so an older Cortex loads a config with a `praxis` block and runs in direct mode. (Verified by the regression review.)

### What goes through Praxis

| Traffic | In `praxis` mode |
|---|---|
| Bridged HTTPS (decrypted by Cortex) | through Praxis |
| Plain proxied HTTP | through Praxis (#1491 H8) |
| Opaque tunnels (passthrough hosts, programs that refuse the CA) | direct from Cortex, as today |
| `skip_hosts`, self-traffic | direct, no row, as today |
| The gateway's own traffic | direct (exempt by pid and start time) |
| Codex Responses WebSocket | **declined** (`responses_websockets` acts as `decline`). Codex then uses HTTPS, through Praxis. **[R 4] [T F4]** |
| Other WebSocket upgrades | direct from Cortex, recorded by Cortex (#1491 M1) |

### The hop **[R 1] [D 5]**

- **It runs at the dial, as in the W1 spike.** It comes after the pipeline step (empty), the header sync, `stripForUpstream` and the redirect re-check of the egress allow list. It sets only `r.URL.Scheme`, `r.URL.Host` and the transport. It leaves `r.Host` and the path unchanged. It is not `pctx.Redirect` (#1491 W2).
- **It lives in its own file, `core/listener/forwardproxy/hop.go`.** `serveOutbound` gets exactly three branch points, and review refuses others:
  1. skip the request row and its capture (Praxis records them), but keep `touch`.
  2. swap the dial target and the transport.
  3. skip the response row and its capture.
- The hop speaks **HTTP/1**. At phase B it sends absolute-form requests, which #1480 detects only on HTTP/1. Go's `Transport` sends CONNECT for an `https://` URL through a proxy, so the phase B hop writes absolute form itself (`Request.WriteProxy` on its own connection). **[T F7]**
- Its transport reads no proxy env.

### The egress listener (throwaway)

- Token check, then `X-Cortex-Dest`. It removes both headers and relays the stream.
- **It dials with the same upstream options as the TLS bridge's upstream client:** `egress_guard`, `upstream_ca_bundle` and `upstream_insecure` (`upOpts`, `cmd/cortex/main.go:963-968`). **[R 5]**
- On a failed dial it answers 502 with `X-Cortex-Upstream-Error: <kind>`, the kind that Cortex builds from the Go dial error today (`docs/session-api.md:370-376`). **[R 6]**
- It records nothing. The egress allow list already ran before the hop.

### Supervision **[T F1] [R 8]**

- **`cortex -supervise` starts two siblings: `cortex` and `cortex-gateway`.** The gateway is not Cortex's child. In the end state the gateway is the front proxy, so a Cortex restart must not cut traffic. This layout survives to the end state. Only the ports and the fallback address change.
- The supervisor:
  - creates the token file.
  - writes `~/.cortex/gateway.yaml` (below) from Cortex's config at each start.
  - starts the gateway with a clean env (no proxy variables).
  - passes the gateway's pid and start time to `cortex`.
  - restarts either child with the existing backoff rules (`cmd/cortex/supervise.go`).
- **No orphans:** the gateway exits when the supervisor goes away (a parent-death signal on Linux, a closed pipe on macOS). At start, the supervisor stops a stale gateway that holds `:47605`.
- `cortex-gateway` is added to `cortexBinaries` (`cmd/cortex/portconflict.go:49`), `install.sh --stop` and `agentop uninstall`. In `praxis` mode, `cortexPorts` (`cmd/agentop/setup_probe.go:18`) also probes `47605` and `47606`.

### `gateway.yaml`, in the end-state shape **[T F3]**

- A listener on `praxis.listen` with a `forward_proxy: {enabled: true}` block. This needs Praxis's `forward-proxy` cargo feature, which `cortex-gateway` turns on.
- `ip_acl` that allows loopback only. Praxis requires an unconditioned `ip_acl` or `allow_destinations` for an enabled forward proxy (`config/validate/forward_proxy.rs:43-58`).
- The chain from Section 3, ending with `access_log`.
- Interim only: a catch-all router to the egress cluster on `praxis.egress`. Phase B removes this entry.
- `insecure_options.allow_private_endpoints: true` and `allow_private_upstreams: true` (local model servers are private destinations).
- `body_limits` of 64 MiB each way, the admin metrics port, and the token file path.
- Users do not edit it.

### Fail closed

- At startup Cortex sends a probe through the whole chain before it uses the hop (#1491 H1, W1 item 7).
- Until the probe passes, and while the gateway is down, each request that would go through Praxis gets **503**. Cortex records it with the new error kind **`gateway_unavailable`**. There is no fallback to direct mode. A silent fallback would skip what Praxis is meant to do (#1491's recorded decision).

### agentop

- Sessions, the timeline, tokens, cost and `/v1/usage` do not change.
- agentop reads `praxis.enabled` from `GET /config`. This adds a fourth field to the fields that agentop decodes (`docs/proxy-contract.md`, #1686).
- In `praxis` mode these commands refuse, with a message that says why: `agentop server …`, the script commands, and `agentop tools scan --write`. **[R 7]** One helper decides it for all of them. **[D 8]**
- The plugin pane is empty in this mode.

### Release and install **[R 9] [R 13] [D 3]**

- **`cortex-gateway` builds in its own release job on native runners,** as `build-cpex` does. Targets: darwin/arm64, linux/amd64, linux/arm64. There is no darwin/amd64 build.
- **A gateway build failure does not block the Go release.** The release then ships without the gateway tarball, and `install.sh --praxis` refuses that version with a clear message.
- **Rust CI jobs run only on changes under `praxis/**`** (and the non-blocking parity report on `core/parse/**`). They cache `~/.cargo` and `target/`, with a size cap. A Go-only PR does not pay for Rust.
- A weekly job builds against the latest Praxis release. It does not block, and it reports a failure. **[D 4]**
- `make dev-install` and `agentop setup` work without `cortex-gateway` while `praxis.enabled` is false.
- The interim is opt-in: `install.sh --praxis`, in the way `--cpex` works. `install.sh --praxis` refuses darwin/amd64, as `--cpex` refuses targets it has no build for. `agentop setup` installs the binary next to `cortex`.
- With `--praxis`, setup writes the `praxis` block and an empty outbound pipeline. The default local config has plugins, and the startup check would refuse it.
- **`agentop update` keeps praxis mode.** It detects it from the config, as it re-passes `--cpex` today (`cmd/agentop/cmd_update.go:120-122`). The supervisor refuses to start a gateway whose version differs from `cortex`.
- `praxis.enabled` stays `false` by default until the parity gates pass and the live check (Section 5) is done.

## 5. Testing

### Direct mode stays the same

- The existing Go test suites pass with no change to their expectations. This is the main check for "nothing breaks".
- With `praxis.enabled: false`: `/v1/ingest` answers 404, no token file is created, and no new error kind appears in any row.

### Parser parity

- Every #1503 fixture runs through the Go and the Rust parser. The event JSON must be the same. Blocking and reporting rules: Section 3.
- The schema drift test (Section 3).

### Contract tests (Go)

- Ingestion:
  - 401 without the token, and a refused `Origin`.
  - Each refusal reason is counted, and the connection stays open.
  - A producer's `plugins["cost"]` is removed, and the cost is settled again from `cost.headers`.
  - The store assigns `seq` and ignores a producer's.
  - Credentials in a capture are removed again. A capture on a row without `inference` is dropped.
  - Tunnel pairing: the first ingested request row of a bridged tunnel is written with its open row.
  - A Cortex cut replaces `client_canceled` with `proxy_error`.
- **The replay test from #1497:** an NDJSON capture through `/v1/ingest` gives the same `/v1/sessions` and `/v1/usage` as the live run.
- Config: `praxis.enabled` with a non-empty outbound pipeline refuses to start, and a reload that adds a plugin is refused. `praxis.enabled` with `mtls` refuses to start.
- The hop: a plugin cannot steer it (the W1 test `TestSpikeHairpin_OnlyTheDialTargetMoves`, rewritten), a client's `X-Cortex-*` headers never reach Praxis or an origin, and `touch` still runs.

### Filter tests (Rust)

- `X-Cortex-Attribution` is accepted only with a valid token, and every `X-Cortex-*` header is removed before the upstream.
- Credential removal matches `core/redact` on the same cases.
- With `access_log` in the chain: a mid-body abort gives `client_canceled`, and a body-limit refusal gives `gateway_refused`. Without `access_log`, the test documents that no deferred row appears (it guards the upstream coupling).
- gzip and zstd responses parse, and the wire bytes do not change.
- `X-Cortex-Upstream-Error` becomes the row's error kind.

### End-to-end (CI, Linux, only on `praxis/**` changes and before a release)

- The chain: the supervisor, Cortex, `cortex-gateway`, the egress listener and a mock origin.
- **The W1 checks become regression tests:**
  - item 1: host and cost match direct mode.
  - item 3: gzip-coded SSE arrives as frames.
  - item 5: a hang-up gives `client_canceled` and closes the origin connection.
  - item 7: fail closed at startup.
  - item 8: a 65 s silence mid-stream.
  - item 9: more than 10 MiB each way.
- A Cortex restart does not restart the gateway, and a supervisor exit stops it.
- **Direct mode against `praxis` mode** on the same mock traffic: the `/v1/sessions` events must be the same, field by field. Two exceptions: timing fields agree within 50 ms, and the `gateway_*` error kinds exist only in `praxis` mode.

### Live check (manual, before the default changes)

- Claude Code and Codex through `praxis` mode, with a scratch HOME and a scratch Cortex, never the shared proxy.
- Check in agentop: sessions, the timeline, tokens, cost, tool calls and titles.

## 6. Delivery

Each step is one PR and is useful when it merges. Nothing changes for a user until `praxis.enabled` is set. **[D 6]**

1. **The Rust workspace and the inference parser:** `praxis/filters` with `cortex_parse` for inference, the parity gate, the schema drift test, and the path-scoped Rust CI job.
2. **The gateway and ingestion together:** `cortex_record`, `cortex_attribution`, the emitter, the `cortex-gateway` binary, and `/v1/ingest` with its replay test. The endpoint lands with its producer.
3. **Supervision** (`cmd/cortex`): two siblings, the token file, `gateway.yaml`, the probe, no orphans, the port-conflict names.
4. **The hop and the egress listener** (`core/listener/forwardproxy`): `hop.go`, the three branch points, the joins, `gateway_unavailable`, the config checks (startup and reload), and the end-to-end tests.
5. **agentop and install:** the `praxis.enabled` contract field, the command refusals, `install.sh --praxis`, setup, update and uninstall.
6. **Release:** the `cortex-gateway` release job and the weekly Praxis build.
7. **The MCP and A2A parsers**, each behind its own parity gate.

Then the live check, then a decision to change the default.

After v1: phase B when praxis#1479 and #1480 ship, then phase D (Section 8) when praxis mode is stable, then phase C.

## 7. End state: what changes when upstream lands

This section is not built now. It records what the interim parts must allow.

### Phase B (praxis#1479 and #1480 in a release)

- The hop sends absolute-form requests over HTTP/1 (Section 4). The egress listener, `X-Cortex-Dest` and the catch-all router entry go away.
- **Needed upstream first:** a CA and verify option for forward-proxy origination. #1480 uses the system roots only, so users of `upstream_ca_bundle` or `upstream_insecure` would break. **[T F3]**

### Phase C (#782 and #1447, after phase D)

- **Phase C comes after phase D.** If upstream lands first, phase C waits for phase D. Then no attribution service is ever built.
- The binary takes `:47600`. The Go front door, the supervisor's second child and the token go away. One process is left.
- `cortex_attribution` reads the client address from the connection (praxis#1447) instead of `X-Cortex-Client`.
- **The CONNECT-time decisions move into Rust filters too.** **[T F5]** Today the Go front door makes them:
  - the self-traffic decision (`attribution.SelfTraffic`).
  - the passthrough decision (the per-program skip set, `tlsbridge/program.go`).
  - the client-trust prediction and the unread report (`clienttrust.go`, `unread.go`).
- The process-chain lookup is cached per downstream connection, as `attribution.Memo` does today. The request part runs per request.
- `cortex_record` adds CONNECT rows. #1479 runs `on_request` for the CONNECT and the end-of-stream body hook with the tunnel byte counts. It does not expose the close reason or the dial address, so `tunnelReason` and `dialAddr` stay empty until it does. HTTP/2 CONNECT gets 501. **[T F8]**
- Rules that move from Cortex into `cortex_attribution`: self-traffic, `skip_hosts`, and the misdirected-Host check. The egress allow list maps to `allow_destinations`, which refuses IP literals, while Cortex accepts `[2001:db8::1]:443`. **[T F9]**
- The store's record type adds `pair` and `rekey` (in process, no wire format).
- `gateway_unavailable`, the probe and the hop go away.

## 8. Phase D: Cortex as a Praxis distribution

### Goal

Cortex ends in the shape of praxis-ai today: **one Rust binary built on the Praxis crates**. It has its own filters, its own store, and its own APIs served by filters. No Go service. agentop stays a Go client and does not change.

| Today (Go) | After phase D |
|---|---|
| attribution and peerproc (about 1.3k lines) | `cortex_attribution` with its own process lookup (Rust) |
| session store, archive and timeline (about 5k lines) | a Cortex store crate on SQLite, in the pattern of praxis-ai's `store`, `store-lifecycle` and `store-backends` |
| cost and the ledger (about 6.2k lines) | a Rust pricing filter (D2), and Praxis Tokenomics when it is on par |
| session API (about 1.2k lines) | filters on `:47601` that serve the same API |
| `POST /v1/ingest` and the emitter's HTTP stream | `cortex_record` writes the same record type to the store in process |

Line counts are non-test, non-comment Go lines on `main` @ `92604f4c`. The forward proxy and the TLS bridge (about 8k lines) are not ported: phase C replaces them with Praxis core.

### Trigger and order

- **Phase D has no upstream trigger.** It starts when praxis mode is the default and stable.
- **It comes before phase C.** Attribution keeps state that the store's events feed: `Attributor` is a store `Recorder`, `Clearer`, `Deleter`, `Evicter` and `Rekeyer` (`core/attribution/attribution.go:214-262`). So attribution and the store move together. If attribution moved first, it would need a new interface to the Go store, and that interface would be throwaway.
- **The client address in the interim.** `Attributor.Lookup` takes only the client and server socket addresses (`core/attribution/lookup.go:23`), so any process on the host can ask it. In phase D the Go front door passes them in a token-protected header, `X-Cortex-Client: <client addr> <server addr>`. The binary runs the lookup itself. In phase C the address comes from the connection (praxis#1447), and the header goes away.

### What stays the same

- **The session API contract** (`docs/session-api.md`, the golden fixtures, `schemaVersion`) is the boundary. The phase D test is the same agentop session against both stores, with the same `/v1/sessions`, `/v1/events`, `/v1/usage` and timeline answers.
- The Go tests of the store, the archive, the timeline, pricing and the session API become parity cases for the Rust port. The #1503 fixtures do the same for the parsers.
- **Serving SSE from a filter:** `GET /v1/events` is a long-lived stream. Praxis has `StreamingTerminalResponse`, a pull-based body for a filter's own streaming answer (`crates/filter/src/actions.rs:53`). Read in the code. Not tested with a long-lived stream.

### What phase D must decide

1. **Existing archives.** The Go segment format moves to SQLite. Options: a one-time importer, or keep only the retention window (30 days by default) and import nothing.
2. **The store crates.** praxis-ai's store crates are `publish = false`, so Cortex follows their pattern and does not depend on them.
3. **The plugins.** Each remaining plugin (inference-router, tool-prune, script and the rest) is ported as a Rust filter or retired. Each is its own decision.

## Upstream asks (add to #1714)

- **praxis#1228:** `emit_deferred_record` should not depend on `access_log`. **[T F2]**
- A CA and verify option for forward-proxy origination (#1480). **[T F3]**
- For #782 and #1447: **[T F5]**
  - a filter decides interception per CONNECT.
  - a filter learns of a TLS handshake failure (the client refused the minted certificate).
  - inner requests carry the outer connection's socket address, so the CONNECT and its requests share one connection id.
- Per-message hooks on upgraded connections (WebSockets). **[T F4]**
- #1479: expose the tunnel's close reason and dial address to filters. **[T F8]**

## Open questions

1. **Who bumps `praxis-proxy`** (D5): decided as we go. Until then, a failed weekly build has no named owner.
2. **The pricing port** (D2): when to start it, and the test that shows Tokenomics is on par.
3. **Rust CI and release cost:** the Praxis lock file has 609 crates, some with native code (`aws-lc-sys`, `openssl-sys`, `zstd-sys`). The W1 build took about 3 minutes on an M-series Mac, and `target/` was 1.3 GB. CI time on GitHub runners is not measured yet.
4. **Phase D archive migration:** an importer, or the retention window only (Section 8).
5. **Interim memory:** in `praxis` mode the capture lives in `cortex-gateway` until it is sent, then in Cortex. Peak memory per in-flight request may be about twice that of direct mode. Not measured.

## References

- #1714: the switchover tracker (phases, upstream asks, decisions).
- #1491: the hairpin spec and the W1 results comment.
- #1497: the ingestion items, closed and moved to #1714.
- #1503: the saved parser fixtures.
- praxis-proxy/praxis#1228, #1447, #1479, #1480, #782.
- Praxis `docs/filters/extensions.md` (manual registration), `crates/filter/src/filter.rs` (`emit_deferred_record`), `crates/protocol/src/http/pingora/handler/logging_util.rs` (the `access_log` gate).
