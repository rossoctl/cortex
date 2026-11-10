# Praxis mode 4/8: the hop and the egress listener — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** In `praxis.enabled` mode, send each recorded request from the forward proxy through `cortex-gateway` and back out through a Cortex egress listener, with no change to direct mode (#1721).

**Architecture:** A dial-only hop (`core/listener/forwardproxy/hop.go`) moves the dial of a non-skipped request to `praxis.listen` over HTTP/1, with five branch points in `serveOutbound`. The gateway records the rows and forwards to the egress listener (`egress.go`) on `praxis.egress`, which dials the origin with the TLS bridge's upstream options. Ingestion (`core/sessionapi/ingest.go`) joins the gateway's rows to Cortex state (tunnel pairing, Cortex's own cuts) through a `HopJoins` interface that the hop implements.

**Tech Stack:** Go 1.26.10 (`core`, `cmd/cortex`), Rust (`praxis/`, Praxis 0.8.0 API).

**Spec:** `docs/superpowers/specs/2026-11-07-praxis-mode-design.md` (Sections 1 "Joins", 2, 4 "The hop", "The egress listener", "Fail closed", "Config", 5). Issue: #1721. Earlier plans: `2026-11-09-praxis-mode-2-gateway-and-ingest.md`, `2026-11-09-praxis-mode-3-supervise-gateway.md`. The W1 spike `5ee3950b` (`spike/hairpin-w1`) is a reference only.

## Global Constraints

- Direct mode does not change. Every existing Go test passes with no change to its expectations.
- `serveOutbound` gets exactly five branch points for the hop, and no other. Each one tests `s.Hop != nil` or the local `hopped`.
- The hop speaks HTTP/1, reads no proxy env (`Proxy: nil`) and keeps Go's default compression (no `DisableCompression`).
- The hop sets only `r.URL.Scheme`, `r.URL.Host` and the transport. `r.Host` and the path do not change. It is not `pctx.Redirect`.
- Header names, verbatim: `X-Cortex-Attribution`, `X-Cortex-Dest`, `X-Cortex-Token`, `X-Cortex-Request-Id`, `X-Cortex-Probe`, `X-Cortex-Egress-Error`.
- Join entries live at most 60 s (`hopJoinTTL = 60 * time.Second`).
- New error kind: `gateway_unavailable`, status 503.
- Run per-module Go commands with `GOWORK=off`. Log with `log/slog`.
- Comments and commit messages in ASD-STE100 style. Commits use `git commit -s` and end with `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. No `Co-Authored-By`.
- Prose says Cortex for the product (CLAUDE.md naming rule).

## Decisions (made for this plan, with reasons)

1. **`X-Cortex-Dest` is `<scheme>://<host>:<port>`**, port always present (80 for http, 443 for https when the URL has none). Reason: the egress listener must know the scheme, and the spike used this form.
2. **The probe is `GET /` to the gateway with `X-Cortex-Token`, `X-Cortex-Probe: 1` and `X-Cortex-Dest: cortex-probe`.** The egress listener answers `204` to a valid token with that Dest and dials nothing. Reason: the probe then passes through the whole chain (Cortex → gateway → egress), and `cortex-probe` has no `://`, so it can never name an origin.
3. **The probe runs every 1 s until one passes, then stops.** After that a failed dial to the gateway gives 503 `gateway_unavailable` per request. Reason: the spec asks for fail closed at startup and while the gateway is down. A per-request dial check covers "down" with no second state machine.
4. **The hop records `gateway_unavailable` itself: the request row and a 503 response row**, through the normal tunnel pairing path. Reason: Praxis never saw the request, so no other row exists (spec, "Who records which rows").
5. **Branch point 5 lives in `recordOutboundResponseEvent`**, the one function every response-row site calls. When the hop owns `pctx`, it records no row. When `fail.Kind == "proxy_error"` (Cortex's own idle cut), it keeps that error as a cut join for the request id. Reason: one branch for all response sites, and the cut is known exactly there.
6. **The hop marks the requests it owns in a `sync.Map` keyed by `*pipeline.Context`**, set at branch point 2 and removed when `serveOutbound` returns. Reason: no change to `pipeline.Context`, which other repositories consume.
7. **The request id on the wire is `pctx.RequestID()`.** Reason: the gateway uses `X-Cortex-Request-Id` as the line's `requestId`, so both sides use one id.
8. **`attribution.Wire` is filled from `pctx`** after hydration: `Proc`, `ClientProc` (`AttributedProc`, `AttributedClientProc`), `Session` (`pctx.SessionID`), `Source`, `SessionHeader`, `ChainSession`, `RecordSession` (the recording sid), and `Agent` from `attribution.AgentOf(r.Header)` read at branch point 2. Reason: the hop must not resolve attribution a second time.
9. **The gateway keeps `X-Cortex-Dest` and `X-Cortex-Token` for its upstream**, and its request capture still removes all twelve hop headers. Reason: the interim upstream is always the egress listener, which removes both. The token must never be in a capture.
10. **The egress listener answers an egress-guard refusal with 403 and `X-Cortex-Egress-Error: destination_private`**, and the Rust `egress_kind` learns `destination_private`. Reason: direct mode records that refusal as `destination_private`, so both modes give the same row.
11. **The egress listener uses `bridge.Upstream.Transport` when the TLS bridge is on, else a `tlsbridge.NewUpstreamClient(nil, false, upOpts...)` transport.** Reason: the spec's "same upstream options as the TLS bridge" (`egress_guard`, CA bundle, `upstream_insecure`).
12. **`HopJoins` is an interface in `core/sessionapi`.** `forwardproxy` does not import `sessionapi`. `cmd/cortex` has the compile-time check. Reason: no new package and no import cycle.
13. **Release of an erased line's join marks the tunnel answered**, so the tunnel writes neither its open row nor a close row. Reason: spec Section 1, "A dropped line also releases its tunnel join with no write".
14. **A join that expires** removes one "awaiting" count and settles the tunnel as today (open row plus a "carried nothing" close row). Reason: spec, known edge case in #1721.
15. **The end-to-end tests run in process in `cmd/cortex`**, behind the `praxis_e2e` build tag and `CORTEX_GATEWAY_BIN`, with the real `cortex-gateway` binary. The client sends plain HTTP through the forward proxy, and a test dialer maps every origin host to the mock origin. Reason: real names such as `chatgpt.com` must not reach DNS, and the TLS bridge needs no new test setup here. Tunnel pairing is a forwardproxy contract test (Task 6). The supervisor is covered by #1720's tests.
16. **The e2e test uses the same wiring function as `main`** (`startPraxis`, Task 7). Reason: the test must not drift from the real wiring.
17. **CI:** the `Praxis` workflow builds `cortex-gateway` and runs the e2e tests after `cargo test`. Reason: spec, "only on `praxis/**` changes and before a release". The workflow's blocking rule stays as it is.

## Review Focus

1. A client that sends its own `X-Cortex-Token` or `X-Cortex-Dest` in praxis mode: the hop must remove them at entry and set its own. Pinned in Task 4 (`TestHop_ClientHopHeadersNeverReachGateway`).
2. The gateway goes down after the probe passed: each request must get 503 `gateway_unavailable` with both rows, not a 502 `upstream_refused`. Pinned in Task 4 (`TestHop_DialFailureIsGatewayUnavailable`).
3. A bridged tunnel whose request row arrives after the client closed the tunnel: one pair, and no "carried nothing" close row. Pinned in Task 6 (`TestHopJoin_TunnelClosedBeforeRowGetsOnePair`).
4. An origin that sends its own `X-Cortex-Egress-Error`: the header must not reach the gateway. Pinned in Task 3 (`TestEgress_OriginEgressErrorRemoved`).
5. A reload that adds an outbound plugin in praxis mode: no pipeline is built, and the skip is logged. Pinned in Task 7 (`TestOutboundEntries_PraxisSkipsPlugins`).

---

## File map

| File | Task | Change |
|---|---|---|
| `praxis/filters/src/record/attribution.rs`, `praxis/filters/src/filters/attribution.rs`, `praxis/filters/src/record/row.rs`, `praxis/server/tests/gateway.rs`, `praxis/README.md` | 1 | keep Dest and Token for the upstream, `destination_private` kind |
| `core/attribution/wire.go`, `core/attribution/wire_test.go`, `core/attribution/testdata/wire.golden`, `praxis/filters/tests/attribution_wire.rs` | 2 | `attribution.Wire` encoder and its cross-language golden |
| `core/listener/forwardproxy/hopheaders.go`, `egress.go`, `egress_test.go` | 3 | header constants, the egress listener |
| `core/listener/forwardproxy/hop.go`, `hop_test.go`, `hop_direct_test.go`, `server.go`, `record.go`, `core/model/session.go`, `core/pipeline/model_alias.go` | 4 | the hop, five branch points, `gateway_unavailable`, the probe |
| `core/sessionapi/ingest.go`, `core/sessionapi/ingest_joins_test.go` | 5 | `HopJoins` and its use in `fileIngest` |
| `core/listener/forwardproxy/hopjoins.go`, `hopjoins_test.go`, `tunnel.go`, `hop.go` | 6 | the join registry and the tunnel hold |
| `cmd/cortex/praxis.go`, `praxis_test.go`, `ingest.go`, `ingest_test.go`, `main.go`, `gateway.go`, `core/config/listenerplan.go`, `core/config/listenerplan_test.go`, `core/config/config.go`, `docs/session-api.md`, `praxis/README.md` | 7 | wiring, config rules, docs |
| `cmd/cortex/praxis_e2e_test.go`, `.github/workflows/praxis.yaml` | 8 | end-to-end tests and CI |

## PRs

| PR | Title | Parent | Tasks | estLines |
|---|---|---|---|---|
| P1 | `Feat: Add the praxis hop and the egress listener to the forward proxy` | "" | 1, 2, 3, 4 | 1250 |
| P2 | `Feat: Join gateway rows to tunnels and turn on the hop in praxis mode` | P1 | 5, 6, 7 | 1050 |
| P3 | `Test: Run the praxis-mode end-to-end checks through cortex-gateway` | P2 | 8 | 850 |

P1 is inert alone: nothing sets `Server.Hop` until P2. P3 needs the wiring of P2.

---

### Task 1: The gateway keeps `X-Cortex-Dest` and `X-Cortex-Token` for the egress listener

Depends on: none. Estimated lines: 80.

**Files:**
- Modify: `praxis/filters/src/record/attribution.rs` (`HOP_HEADERS` stays 12 entries for the capture; add `UPSTREAM_STRIP_HEADERS: [&str; 10]`, the ten without `X-Cortex-Dest` and `TOKEN_HEADER`)
- Modify: `praxis/filters/src/filters/attribution.rs` (`request_headers_to_remove` uses `UPSTREAM_STRIP_HEADERS`)
- Modify: `praxis/filters/src/record/row.rs` (`egress_kind`: `"destination_private" => "destination_private"`)
- Modify: `praxis/server/tests/gateway.rs` (`hop_headers_removed_custom_kept`)
- Modify: `praxis/README.md` (gateway decision 20)

- [ ] **Step 1: Change the tests first.**
  - In `hop_headers_removed_custom_kept`: assert that `x-cortex-dest` and `x-cortex-token` now reach the upstream with the values sent, and that the other ten hop headers do not. `X-Cortex-Custom` still reaches it. The request capture in the ingested line still has none of the twelve.
  - In `row.rs` tests: `egress_kind("destination_private") == "destination_private"`.
- [ ] **Step 2:** `cargo test --locked --manifest-path praxis/Cargo.toml` fails on both.
- [ ] **Step 3:** Implement. Doc comment on `UPSTREAM_STRIP_HEADERS`: the interim upstream is always the egress listener, which removes Dest and Token. At phase B both go on this list.
- [ ] **Step 4:** Update README decision 20: the gateway keeps Dest and Token for its upstream, the egress listener (#1721), and removes them before any other upstream.
- [ ] **Step 5:** `cd praxis && cargo fmt --check && cargo clippy --all-targets --locked -- -D warnings && cargo test --locked` passes.
- [ ] **Step 6:** Commit `feat: Keep X-Cortex-Dest and X-Cortex-Token for the egress listener`.

### Task 2: `attribution.Wire`, the Go encoder of `X-Cortex-Attribution`

Depends on: none. Estimated lines: 150.

**Files:**
- Modify: `core/attribution/wire.go`
- Create: `core/attribution/wire_test.go`, `core/attribution/testdata/wire.golden`
- Create: `praxis/filters/tests/attribution_wire.rs`

**Interfaces:**
- Produces:
  ```go
  // Wire is the attribution result that the hop sends in X-Cortex-Attribution.
  type Wire struct {
      Agent         string `json:"agent,omitempty"`
      Proc          string `json:"proc,omitempty"`
      ClientProc    string `json:"clientProc,omitempty"`
      Session       string `json:"session,omitempty"`
      Source        string `json:"source,omitempty"`
      SessionHeader string `json:"sessionHeader,omitempty"`
      ChainSession  string `json:"chainSession,omitempty"`
      RecordSession string `json:"recordSession,omitempty"`
  }
  const HeaderAttribution = "X-Cortex-Attribution"
  func (w Wire) Encode() string // base64.RawURLEncoding of the JSON
  ```

- [ ] **Step 1: Write the failing tests.**
  - `TestWire_EncodeGolden`: encode `Wire{Agent:"claude-code", Proc:"p1", ClientProc:"p2", Session:"s1", Source:"header", SessionHeader:"X-Claude-Code-Session-Id", ChainSession:"s0", RecordSession:"s1"}`. It must equal `testdata/wire.golden` (trimmed). `-update` rewrites the file.
  - `TestWire_KeysMatchRust`: the JSON keys of a full Wire are exactly `agent, proc, clientProc, session, source, sessionHeader, chainSession, recordSession`.
  - `TestWire_EncodeIsUnpadded`: no `=` in the output.
  - Rust `decodes_go_golden` (`praxis/filters/tests/attribution_wire.rs`): read `../../core/attribution/testdata/wire.golden` from `CARGO_MANIFEST_DIR`, `decode(Some(..))`, assert every field equals the Go values.
- [ ] **Step 2:** Run `GOWORK=off go -C core test ./attribution/ -run Wire`. Expect FAIL (undefined).
- [ ] **Step 3:** Implement. Update the package comment "The encoder and decoder land with that hop": the encoder is `Wire.Encode`, the decoder is the gateway's.
- [ ] **Step 4:** Run with `-update` once, then without. Run the Rust test. Both PASS.
- [ ] **Step 5:** Commit `feat: Add attribution.Wire, the encoder of X-Cortex-Attribution`.

### Task 3: The egress listener

Depends on: none. Estimated lines: 300.

**Files:**
- Create: `core/listener/forwardproxy/hopheaders.go`, `core/listener/forwardproxy/egress.go`, `core/listener/forwardproxy/egress_test.go`

**Interfaces:**
- Produces (in `hopheaders.go`):
  ```go
  const (
      headerDest        = "X-Cortex-Dest"
      headerToken       = "X-Cortex-Token"
      headerRequestID   = "X-Cortex-Request-Id"
      headerProbe       = "X-Cortex-Probe"
      headerEgressError = "X-Cortex-Egress-Error"
      probeDest         = "cortex-probe" // Decision 2
  )
  // clientHopHeaders are the five hop headers a client must not send in praxis mode.
  var clientHopHeaders = []string{attribution.HeaderAttribution, headerDest, headerToken, headerRequestID, headerProbe}
  func stripHopHeaders(h http.Header)
  func tokenMatches(given, token string) bool // constant time; empty token matches nothing
  ```
- Produces (in `egress.go`):
  ```go
  // NewEgressHandler is the praxis-mode egress listener (spec Section 4, throwaway at phase B).
  func NewEgressHandler(token string, upstream http.RoundTripper) http.Handler
  ```

Behavior, in order:
1. A token that does not match: 403, no dial, nothing recorded.
2. `X-Cortex-Dest: cortex-probe`: 204, no dial.
3. A Dest that is not `http://host:port` or `https://host:port`: 400.
4. Else an `httputil.ReverseProxy` with `Rewrite` (set `Out.URL.Scheme`, `Out.URL.Host` from Dest, `Out.Host = In.Host`, delete Dest, Token and Egress-Error from `Out.Header`), `FlushInterval: -1`, `ModifyResponse` that deletes `X-Cortex-Egress-Error`, and an `ErrorHandler`:
   - `*egressguard.Refusal` (errors.As): 403, `X-Cortex-Egress-Error: destination_private` (Decision 10).
   - Else when the client is still there: 502, `X-Cortex-Egress-Error: <model.TransportError(err).Kind>`.
   - Log with `pipeline.SanitizeLogValue` on every client-controlled value.

- [ ] **Step 1: Write the failing tests** (`egress_test.go`, with an `httptest` origin):
  - `TestEgress_TokenRequired`: no token, a wrong token, and an empty configured token each give 403. The origin sees no request.
  - `TestEgress_ProbeAnswers204`: valid token, Dest `cortex-probe` gives 204. The origin sees nothing.
  - `TestEgress_BadDest400`: `""`, `ftp://h:1`, `h:1`, `http://`.
  - `TestEgress_RelaysAndStripsHeaders`: the origin gets the path, the query, the client's `Host`, `X-Cortex-Custom`, and no Dest, Token or Egress-Error.
  - `TestEgress_GzipPassesUnchanged`: the client sends `Accept-Encoding: gzip`. The origin answers gzip. The bytes arrive still gzip, with `Content-Encoding: gzip`.
  - `TestEgress_OriginEgressErrorRemoved`: the origin sets `X-Cortex-Egress-Error: upstream_dns`. The client does not get it.
  - `TestEgress_DialFailure502WithKind`: Dest on a closed port gives 502 and `X-Cortex-Egress-Error: upstream_refused`.
  - `TestEgress_GuardRefusal403`: a transport whose `DialContext` returns `&egressguard.Refusal{...}` gives 403 and `destination_private`.
  - `TestEgress_StreamsFrames`: an SSE origin that flushes one frame, then waits on a channel. The client reads the first frame before the test releases the channel.
- [ ] **Step 2:** `GOWORK=off go -C core test ./listener/forwardproxy/ -run Egress` FAILS.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Same command PASSES. Also `-race`.
- [ ] **Step 5:** Commit `feat: Add the praxis-mode egress listener`.

### Task 4: The hop, its five branch points, `gateway_unavailable` and the probe

Depends on: 2, 3. Estimated lines: 720.

**Files:**
- Create: `core/listener/forwardproxy/hop.go`, `hop_test.go`, `hop_direct_test.go`
- Modify: `core/listener/forwardproxy/server.go` (`Server.Hop *Hop`, five branch points in `serveOutbound`)
- Modify: `core/listener/forwardproxy/record.go` (branch point 5 in `recordOutboundResponseEvent`)
- Modify: `core/model/session.go` (`ErrorKindGatewayUnavailable = "gateway_unavailable"`, doc comment), `core/pipeline/model_alias.go` (alias)

**Interfaces:**
- Consumes: Task 2 `attribution.Wire`, `HeaderAttribution`. Task 3 header constants, `stripHopHeaders`, `probeDest`.
- Produces:
  ```go
  type Hop struct { /* gateway addr, token, client *http.Client, ready atomic.Bool, owned sync.Map */ }
  func NewHop(gatewayAddr, token string) *Hop
  func (h *Hop) Ready() bool
  // Probe sends a probe every `every` until one gets 204 or ctx ends. Logs once when ready.
  func (h *Hop) Probe(ctx context.Context, every time.Duration)
  // Server field:
  Hop *Hop // nil in direct mode
  // Internal, used by Task 6:
  func (h *Hop) own(pctx *pipeline.Context, w attribution.Wire)   // branch point 2
  func (h *Hop) release(pctx *pipeline.Context)                    // deferred at branch point 2
  func (h *Hop) owns(pctx *pipeline.Context) (attribution.Wire, bool)
  func (s *Server) hopDo(w http.ResponseWriter, r *http.Request, pctx *pipeline.Context, tl *tunnelLog, sid string) (resp *http.Response, err error, answered bool)
  ```

The hop client: `http.Transport{Proxy: nil, ForceAttemptHTTP2: false, MaxIdleConnsPerHost: 100, IdleConnTimeout: 90 * time.Second, DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext}`, `CheckRedirect` returns `http.ErrUseLastResponse`.

The five branch points in `serveOutbound`, in this order:
1. Right after `attribution.Strip(r.Header)` in `serveOutbound` (`server.go`, the call near the top of the function; not the one in `handleConnect`): `if s.Hop != nil { stripHopHeaders(r.Header) }`.
2. At the request row (`if !skipped && s.Sessions != nil` block): `hopped := s.Hop != nil && !skipped && !forwardUpgrade`. When `hopped`, call `s.Hop.own(pctx, wire)` and `defer s.Hop.release(pctx)` in place of `s.recordCapturedRequest`. `defer s.touch` stays (spec: keep `touch`). Build `wire` here (Decision 8).
3. `ownEncoding := hopped || (!skipped && (...))`.
4. At `client.Do(r)`: `if hopped { resp, err, answered = s.hopDo(w, r, pctx, tl, sid); if answered { return } } else { resp, err = client.Do(r) }`. `hopDo` runs after the redirect re-check, so the order is the spec's.
5. In `recordOutboundResponseEvent`, first line after the `s.Sessions == nil` check: `if s.Hop != nil { if _, ok := s.Hop.owns(pctx); ok { return } }`. Task 6 adds the cut note here.

`hopDo`:
- When `!s.Hop.Ready()`: answer 503 `gateway_unavailable` (below) and return `answered = true`.
- Set `X-Cortex-Dest` (Decision 1, from `r.URL` before the swap), `X-Cortex-Token`, `X-Cortex-Request-Id: pctx.RequestID()`, `X-Cortex-Attribution: wire.Encode()`. Then `r.URL.Scheme, r.URL.Host = "http", gatewayAddr`.
- `resp, err := h.client.Do(r)`. A dial failure (`*net.OpError` with `Op == "dial"`, via errors.As) answers 503 `gateway_unavailable`, `answered = true`. Any other result returns to the caller.
- The 503 path: `s.Hop.release(pctx)` first, so branch point 5 records. Then `s.recordCapturedRequest(tl, pctx, sid, nil)`, `http.Error(w, `{"error":"gateway unavailable"}`, 503)`, and `s.recordOutboundResponseEvent(pctx, 503, &pipeline.EventError{Kind: pipeline.ErrorKindGatewayUnavailable, Message: "cortex-gateway is unavailable: " + cause})`. `cause` is `"not ready"` or `model.TransportError(err).Message`.

`Probe`: `GET http://<gateway>/` with `X-Cortex-Token`, `X-Cortex-Probe: 1`, `X-Cortex-Dest: cortex-probe`, 2 s timeout per try. Set ready on 204. Log at Info once when ready, and at Warn at most every 30 s while not ready.

- [ ] **Step 1: Write the failing tests** (`hop_test.go`). A fake gateway is an `httptest.Server` that records each request and answers as the test says. Build the Server as the existing tests do (`support_test.go` helpers), with an empty pipeline and a store.
  - `TestHop_DialsGatewayKeepsHostAndPath`: a plain proxied `GET http://api.example.com/v1/x?q=1`. The gateway sees path `/v1/x?q=1`, `Host: api.example.com`, `X-Cortex-Dest: http://api.example.com:80`, the token, a non-empty request id, and an attribution header that decodes to the request's session.
  - `TestHop_OnlyTheDialTargetMoves` (rewrite of the spike test): a plugin that writes `pctx.Scheme`, `pctx.Host` and a `X-Cortex-Dest` header in `pctx.Headers`. The gateway still sees the hop's Dest, and the real destination stays in Dest.
  - `TestHop_ClientHopHeadersNeverReachGateway`: the client sends all five with forged values, plus `X-Cortex-Custom: v`. The gateway sees only the hop's values for Dest, Token, Request-Id and Attribution, no `X-Cortex-Probe`, and `X-Cortex-Custom: v`.
  - `TestHop_OwnsEncoding`: the client sends `Accept-Encoding: gzip, deflate, br`. The gateway sees `Accept-Encoding: gzip` only. A gzip answer reaches the client decoded.
  - `TestHop_NoRowsForHoppedRequest`: after a 200 and after an SSE stream, the store holds no row for the request.
  - `TestHop_TouchStillRuns`: with process attribution on (the existing test helper for a chain), the session is touched.
  - `TestHop_NotReadyIsGatewayUnavailable`: before any probe: 503 to the client, the gateway sees nothing, the store has the request row and a response row with status 503 and kind `gateway_unavailable`.
  - `TestHop_DialFailureIsGatewayUnavailable`: ready, gateway on a closed port: the same two rows.
  - `TestHop_ProbeSetsReady`: the fake gateway answers 503 twice, then 204. `Probe` returns, `Ready()` is true, and each probe carried the token, `X-Cortex-Probe` and `X-Cortex-Dest: cortex-probe`.
  - `TestHop_SkipHostsAndUpgradesDoNotHop`: a `skip_hosts` match and a forwarded WebSocket upgrade dial the origin, not the gateway.
- [ ] **Step 2: Write the direct-mode test** (`hop_direct_test.go`): `TestServeOutbound_DirectModeBranchPoints`. `Hop` nil, inference-parser-like test plugin that reads bodies (use the existing test plugin helpers). One request each for JSON and SSE. Assert: the request row and its capture exist, the response row and its capture exist, a client `X-Cortex-Dest`, `-Token`, `-Attribution`, `-Request-Id`, `-Probe` reach the origin unchanged, `Accept-Encoding` at the origin is as before (Go's `gzip`), and the origin (not a gateway) got the dial.
- [ ] **Step 3:** `GOWORK=off go -C core test ./listener/forwardproxy/ -run 'Hop|DirectModeBranchPoints'` FAILS.
- [ ] **Step 4:** Implement. Add the error kind to `docs/session-api.md`'s error-kind list in Task 7, not here.
- [ ] **Step 5:** `GOWORK=off go -C core test -race ./listener/forwardproxy/ ./model/ ./pipeline/` PASSES. The whole existing forwardproxy suite passes with no change.
- [ ] **Step 6:** Commit `feat: Hop praxis-mode requests to cortex-gateway at the dial`.

### Task 5: Ingestion uses the hop's joins

Depends on: none. Estimated lines: 230.

**Files:**
- Modify: `core/sessionapi/ingest.go`
- Create: `core/sessionapi/ingest_joins_test.go`

**Interfaces:**
- Produces:
  ```go
  // HopJoins is the forward proxy's join state in praxis mode (spec Section 1, "Joins").
  type HopJoins interface {
      // AppendRequest writes a request row through its bridged tunnel's pairing path and
      // reports whether it did. False: the caller appends the row.
      AppendRequest(requestID, sid string, ev model.SessionEvent, c *model.RequestCapture) bool
      // Release drops the join of requestID with no write.
      Release(requestID string)
      // TakeCut returns the error of a cut that Cortex made on requestID, or nil.
      TakeCut(requestID string) *model.EventError
  }
  // Ingest gets: Joins HopJoins // nil: no joins
  ```

`fileIngest` changes:
- The `erased` branch: `if j := in.cfg.Joins; j != nil { j.Release(l.RequestID) }` before `refuse`. Remove the `#1721` comment.
- A response row: `if cut := joins.TakeCut(id); cut != nil` then set `ev.Error.Kind, ev.Error.Message = cut.Kind, cut.Message` when `ev.Error != nil`, else `ev.Error = cut`. Before `settleIngested`.
- A request row (`ev.Phase == model.SessionRequest`): keep the `holdRequest` call as it is. Then, in place of the last two lines (`s.store.AppendCapture(sid, ev, in.capture(l, &ev))` and `in.lines.Add(1)`): `c := in.capture(l, &ev)`; when `joins != nil && joins.AppendRequest(l.RequestID, sid, ev, c)`, only `in.lines.Add(1)` and return. Else `s.store.AppendCapture(sid, ev, c)` and `in.lines.Add(1)` as now. Response rows always go to `AppendCapture`.
- `joins` is `in.cfg.Joins`. Add the field `Joins HopJoins` to the `Ingest` struct with the doc comment `// Joins is the forward proxy's join state in praxis mode. nil: no joins.`

- [ ] **Step 1: Write the failing tests** with a fake `HopJoins` that records calls:
  - `TestIngestJoins_ErasedLineReleasesJoin`: erase the session, then send a line with an earlier `startedAt`. `Release(id)` is called once, nothing is appended, `erased` is counted.
  - `TestIngestJoins_RequestRowGoesThroughJoin`: the fake returns true. The store gets no row from ingestion, and `ingest_lines_total` is 1.
  - `TestIngestJoins_RequestRowFallsBack`: the fake returns false. The row is appended as today.
  - `TestIngestJoins_CutReplacesClientCanceled`: a response row with `client_canceled`. The fake cut is `proxy_error` with a message. The stored row has kind `proxy_error` and that message, and keeps its `code`.
  - `TestIngestJoins_NilJoinsUnchanged`: the existing behavior with `Joins == nil`.
- [ ] **Step 2:** `GOWORK=off go -C core test ./sessionapi/ -run IngestJoins` FAILS.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** PASS, and the whole `./sessionapi/` suite passes.
- [ ] **Step 5:** Commit `feat: Join ingested rows to Cortex's tunnels and cuts`.

### Task 6: The join registry and the tunnel hold

Depends on: 4. Estimated lines: 450.

**Files:**
- Create: `core/listener/forwardproxy/hopjoins.go`, `hopjoins_test.go`
- Modify: `core/listener/forwardproxy/tunnel.go` (`awaiting int` on `tunnelLog`, `settleLocked` waits while `awaiting > 0`, new methods below)
- Modify: `core/listener/forwardproxy/hop.go` (register a join in `hopDo`, the cut note at branch point 5, `Hop` methods that implement `HopJoins`)

**Interfaces:**
- Consumes: Task 4 `Hop`, `hopDo`, `owns`.
- Produces (method set of `*Hop`, matching Task 5's `HopJoins` exactly):
  ```go
  func (h *Hop) AppendRequest(requestID, sid string, ev model.SessionEvent, c *model.RequestCapture) bool
  func (h *Hop) Release(requestID string)
  func (h *Hop) TakeCut(requestID string) *model.EventError
  const hopJoinTTL = 60 * time.Second
  // on tunnelLog:
  func (t *tunnelLog) await()                // awaiting++
  func (t *tunnelLog) joined(sid string, ev pipeline.SessionEvent, c *pipeline.RequestCapture) bool // recordWith, awaiting--, settleLocked
  func (t *tunnelLog) unawait(answered bool) // awaiting--; when answered, t.answered = true; settleLocked
  ```

Rules:
- `hopDo` registers `requestID → {tl, timer}` for a bridged request (`tl != nil`) before it sends, and calls `tl.await()`. It does not mark the tunnel answered.
- The 503 path of `hopDo` records its rows first, then drops the join with `unawait(false)`.
- `AppendRequest`: remove the entry and stop its timer. No entry, or `tl == nil`: return false. Else return `tl.joined(...)`. When `joined` returns false (the open row was already written), the caller appends.
- `Release`: remove the entry, `tl.unawait(true)` (Decision 13).
- Expiry (`time.AfterFunc(h.ttl, ...)`): remove the entry, `tl.unawait(false)` (Decision 14).
- Branch point 5: when the hop owns `pctx` and `fail != nil && fail.Kind == "proxy_error"`, keep `requestID → copy of fail` in `cuts` with its time. `TakeCut` removes and returns it. Entries older than `h.ttl` are swept on each insert.
- `h.ttl` is a field set to `hopJoinTTL` by `NewHop`, so tests can set it short.

- [ ] **Step 1: Write the failing tests** (`hopjoins_test.go`). Use the existing bridged-tunnel test setup (`tlsbridge_integration_test.go` / `bridgedopen_test.go` helpers) with a fake gateway that answers 200 and a test that calls `hop.AppendRequest` as ingestion would.
  - `TestHopJoin_TunnelClosedBeforeRowGetsOnePair`: the client closes the tunnel after its answer. No row is in the store. Then `AppendRequest` with the request row. The store holds exactly the open row and the request row, in that order, and no close row.
  - `TestHopJoin_ExpiredJoinSettlesAsToday`: ttl 50 ms, no `AppendRequest`. After the ttl the store holds the open row and a "carried nothing" close row.
  - `TestHopJoin_ReleaseWritesNothing`: `Release` after the tunnel closed. The store holds no row for that tunnel, also after twice the ttl.
  - `TestHopJoin_SecondRequestAppendsItself`: two requests on one tunnel. The first `AppendRequest` returns true, the second returns false.
  - `TestHopJoin_PlainRequestHasNoPairJoin`: a plain proxied request: `AppendRequest` returns false.
  - `TestHopJoin_IdleCutIsTaken`: a hopped SSE stream cut by the idle cutoff (set `streamReadIdleTimeout` short the way `maxidle_test.go` does). `TakeCut(id)` returns kind `proxy_error` once, then nil.
  - `TestHopJoin_GatewayUnavailablePairsAndReleases`: not ready, bridged. The open row, the request row and the 503 row are written once, and no close row follows.
- [ ] **Step 2:** `GOWORK=off go -C core test ./listener/forwardproxy/ -run HopJoin` FAILS.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** `GOWORK=off go -C core test -race ./listener/forwardproxy/` PASSES, all existing tunnel tests included.
- [ ] **Step 5:** Commit `feat: Hold a bridged tunnel until its gateway row joins`.

### Task 7: Turn on the hop and the egress listener in praxis mode

Depends on: 3, 4, 5, 6. Estimated lines: 370.

**Files:**
- Create: `cmd/cortex/praxis.go`, `cmd/cortex/praxis_test.go`
- Modify: `cmd/cortex/ingest.go` (`readGatewayToken`, `ingestOptions` takes `joins sessionapi.HopJoins`), `cmd/cortex/ingest_test.go`
- Modify: `cmd/cortex/main.go` (`buildPipelines`, the forward-role block, the session API options, `upOpts` moves out of the TLS bridge block)
- Modify: `cmd/cortex/gateway.go` (the comment "Until the egress listener lands (#1721)")
- Modify: `core/config/listenerplan.go`, `core/config/listenerplan_test.go`, `core/config/config.go` (`PraxisConfig` doc comment)
- Modify: `docs/session-api.md` (`gateway_unavailable` in the error kinds, the joins under "What Cortex does with a line"), `praxis/README.md` (a "Hop decisions (#1721)" list: Decisions 1-17 in one line each)

**Interfaces:**
- Consumes: `forwardproxy.NewHop`, `Hop.Probe`, `forwardproxy.NewEgressHandler`, `sessionapi.HopJoins`, `Ingest.Joins`.
- Produces:
  ```go
  // outboundEntries is the outbound plugin list to build: none in praxis mode, and it logs the skipped names.
  func outboundEntries(c *config.Config, log *slog.Logger) []config.PluginEntry
  // readGatewayToken reads the token file that CORTEX_GATEWAY_TOKEN_FILE names.
  func readGatewayToken(getenv func(string) string, readFile func(string) ([]byte, error)) (string, error)
  // startPraxis sets fpSrv.Hop and decline mode, serves the egress listener on ln with upstream,
  // starts the probe, and returns the hop for Ingest.Joins.
  func startPraxis(ctx context.Context, cfg *config.Config, fpSrv *forwardproxy.Server, token string, upstream http.RoundTripper, ln net.Listener) *forwardproxy.Hop
  var _ sessionapi.HopJoins = (*forwardproxy.Hop)(nil)
  ```

Rules:
- `buildPipelines` is the closure in `cmd/cortex/main.go`. Change its call `plugins.BuildWithDeps(c.Pipeline.Outbound.Plugins, outDeps)` to `plugins.BuildWithDeps(outboundEntries(c, slog.Default()), outDeps)`. Leave `spiffeProviderNeeded` as it is. The log line: `slog.Info("praxis mode: outbound plugins not built; cortex-gateway processes the traffic", "plugins", names)`, only when the list is not empty. Startup and each reload go through it. The config keeps the plugins.
- `ListenerPlan`: in the forward role, when `cfg.PraxisEnabled()`, add `PlannedListener{Name: "praxis-egress", Addr: cfg.Praxis.EgressAddr()}`.
- `startPraxis` sets `fpSrv.ResponsesWebSockets = config.ResponsesWebSocketsDecline`. Call it before the forward proxy serves (the same rule as `startVMControl`). The egress server goes through `bootstrap.ServeHTTPServer("praxis-egress", ...)` into `httpServers`, so shutdown closes it.
- `upstream` is Decision 11.
- `mtls` and `bind_loopback_only` refusals exist (`validatePraxis`). Do not add them again.

- [ ] **Step 1: Write the failing tests.**
  - `TestOutboundEntries_PraxisSkipsPlugins`: a config with two outbound plugins and `praxis.enabled`. The result is empty, and the log (a `slog` handler into a buffer) names both. With `praxis.enabled: false` the same two come back, in order. The config's list does not change.
  - `TestOutboundEntries_PraxisEmptyListLogsNothing`.
  - `TestListenerPlan_PraxisEgress` (core/config): praxis on adds `praxis-egress` at `127.0.0.1:47606`. Praxis off, or no forward role, does not.
  - `TestStartPraxis_DeclinesResponsesWebSocket`: after `startPraxis`, `fpSrv.ResponsesWebSockets == decline` and `fpSrv.Hop != nil`.
  - `TestStartPraxis_EgressServesProbe`: a probe from the hop through a fake gateway (an `httputil.ReverseProxy` to `ln`) makes `Hop.Ready()` true within 3 s.
  - `TestReadGatewayToken_*`: the three errors that `ingestOptions` has today move here, with the same messages.
- [ ] **Step 2:** Run (gate step for `cmd/cortex` and `core/config`). FAIL.
- [ ] **Step 3:** Implement. Update the comments that name #1721 as future work (`config.go`, `gateway.go`, `attribution.rs` was Task 1).
- [ ] **Step 4:** `TAGS=$(GOWORK=off go -C scripts/profile-tags run . local) && GOWORK=off go -C cmd/cortex test -tags "$TAGS" ./... && GOWORK=off go -C core test ./config/ ./sessionapi/` PASSES.
- [ ] **Step 5:** Commit `feat: Turn on the hop and the egress listener in praxis mode`.

### Task 8: End-to-end tests through the real gateway

Depends on: 1, 7. Estimated lines: 850.

**Files:**
- Create: `cmd/cortex/praxis_e2e_test.go` (`//go:build praxis_e2e`)
- Modify: `.github/workflows/praxis.yaml`

**Harness** (`newPraxisChain(t, opts)`):
- Skip unless `CORTEX_GATEWAY_BIN` names a file.
- A mock origin (`httptest.NewServer`) that records each request's headers and serves the handlers below.
- A `session.Store`, a `sessionapi.Server` with `WithIngest` (token, rates, `Joins: hop`) on a loopback listener, and a forward proxy built as `main` builds it with an empty outbound chain.
- `startPraxis` with an `upstream` transport whose `DialContext` sends every address to the mock origin (Decision 15).
- `gatewayYAML(cfg, tokenPath)` written to a temp dir. Start `CORTEX_GATEWAY_BIN -c <yaml> --exit-on-stdin-eof` with an empty env and a stdin pipe. `t.Cleanup` closes the pipe and waits.
- A direct-mode twin (`newDirectChain`): the same mock origin and dialer, praxis off, outbound `inference-parser` from the local profile, so the two runs can be compared.
- The client: `http.Client` with `Proxy` = the forward proxy, plain `http://` URLs.

- [ ] **Step 1: Write the tests.** Each one names the W1 item or the issue's "Done when" line.
  - `TestPraxisE2E_HostAndCostMatchDirect` (W1 1): one chat-completions JSON request with `usage`. The response row's `host` and `plugins.cost` equal the direct run's.
  - `TestPraxisE2E_GzipSSEArrivesAsFrames` (W1 3): the origin sends gzip SSE, one frame, then waits. The client reads frame 1 before the test releases frame 2.
  - `TestPraxisE2E_HangUpIsClientCanceled` (W1 5): the client cancels mid-stream. The response row's kind is `client_canceled`, and the origin's request context ends within 5 s.
  - `TestPraxisE2E_FailClosedAtStartup` (W1 7): start the chain without the gateway. A request gets 503 and a `gateway_unavailable` row. Start the gateway. Within 10 s a request gets 200.
  - `TestPraxisE2E_SilenceMidStream` (W1 8): the origin sends one frame, is silent for 65 s, then sends the rest. The client gets every frame and the row has no error.
  - `TestPraxisE2E_OverTenMiBEachWay` (W1 9): an 11 MiB request body (not inference) and an 11 MiB streamed response. `bytesUp` and `bytesDown` are the sizes.
  - `TestPraxisE2E_CodexMislabeledSSE`: host `chatgpt.com`, path `/backend-api/codex/responses`, `Content-Type: application/json`, 2xx SSE body. The client reads frame 1 before frame 2 is released. A 405 with `{"detail":"Method Not Allowed"}`: the client gets that body, and the row carries the error detail.
  - `TestPraxisE2E_AcceptEncodingDecoded`: the client sends `Accept-Encoding: gzip, deflate, br`. The origin sees only `gzip` and answers gzip JSON. The client gets plain JSON, and the row has the tokens.
  - `TestPraxisE2E_NoHopHeaderReachesOrigin`: the client sends the five hop headers and `X-Cortex-Custom`. The origin sees none of the twelve hop headers and sees `X-Cortex-Custom`.
  - `TestPraxisE2E_TokenlessRequestTo47605`: a request straight to `praxis.listen` with no token gets 403. `/v1/sessions` shows no new row.
  - `TestPraxisE2E_SameEventsAsDirect`: the JSON and SSE requests through both chains. Compare `/v1/sessions/{id}` events field by field after removing `seq`, `at`, `sessionId`, `requestId`. `ttfbMs` and `durationMs` agree within 50 ms. Any other difference fails the test. Do not widen the list in the test: fix the cause, or add it to the README's known differences with its reason and a pinned assertion.
- [ ] **Step 2:** Build the gateway and run:
  `cargo build --locked -p cortex-gateway --manifest-path praxis/Cargo.toml && TAGS=$(GOWORK=off go -C scripts/profile-tags run . local) && CORTEX_GATEWAY_BIN=$PWD/praxis/target/debug/cortex-gateway GOWORK=off go -C cmd/cortex test -count=1 -timeout 15m -tags "praxis_e2e,$TAGS" -run TestPraxisE2E .`
  Fix what fails in the task that owns the code, in this PR.
- [ ] **Step 3: CI.** In `praxis.yaml`, after `cargo test`, add `actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e  # v7.0.0` (`go-version-file: cmd/cortex/go.mod`) and a step `End-to-end (praxis mode)` with the Step 2 command and `continue-on-error: ${{ steps.changes.outputs.blocking != 'true' }}`. Give the step `id: e2e` and no `working-directory`, so it runs from the repo root as the command expects. Add its outcome to the report step as the other three are (`E2E: ${{ steps.e2e.outcome }}` in `env`, and a `::warning::` line). Add to both `paths` lists: `core/listener/forwardproxy/hop*.go`, `core/listener/forwardproxy/egress*.go`, `cmd/cortex/praxis*.go`, `cmd/cortex/gateway.go`.
- [ ] **Step 4:** Commit `test: Run the praxis-mode end-to-end checks through cortex-gateway`.

---

## Gate

From the worktree root:

```sh
test -z "$(gofmt -l core cmd/cortex)" && GOWORK=off go -C core vet ./... && GOWORK=off go -C core test -race ./attribution/... ./listener/... ./sessionapi/... ./config/... ./model/... ./pipeline/... && TAGS=$(GOWORK=off go -C scripts/profile-tags run . local) && GOWORK=off go -C cmd/cortex vet -tags "$TAGS" ./... && GOWORK=off go -C cmd/cortex test -tags "$TAGS" ./... && cargo fmt --check --manifest-path praxis/Cargo.toml && cargo clippy --manifest-path praxis/Cargo.toml --all-targets --locked -- -D warnings && cargo test --manifest-path praxis/Cargo.toml --locked && cargo build --locked -p cortex-gateway --manifest-path praxis/Cargo.toml && CORTEX_GATEWAY_BIN=$PWD/praxis/target/debug/cortex-gateway GOWORK=off go -C cmd/cortex test -count=1 -timeout 15m -tags "praxis_e2e,$TAGS" -run TestPraxisE2E .
```

Before P1 and P2 there is no `praxis_e2e` file, so the last step runs no test and passes.

## Self-review notes

- Spec coverage: the hop (Task 4), the entry strip (Task 4, point 1), `ownEncoding` (Task 4, point 3), the egress listener (Task 3), joins (Tasks 5, 6), the probe and `gateway_unavailable` (Task 4), config skip and decline (Task 7), the tunnel hold (Task 6), the end-to-end list (Task 8), the direct-mode branch test (Task 4), the gateway's kept headers (Task 1).
- Out of scope: agentop and install (step 5), MCP and A2A (step 6), the release job (step 7), phase B absolute form.
