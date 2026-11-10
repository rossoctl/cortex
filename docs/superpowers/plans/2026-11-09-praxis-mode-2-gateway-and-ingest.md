# Praxis mode 2/8: cortex-gateway and the Cortex ingestion endpoint — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Praxis records each request and response row of a gateway request and streams it to Cortex, and Cortex accepts, settles and stores the rows through `POST /v1/ingest`. The endpoint lands with its producer.

**Architecture:** In `praxis/filters`, a pure module `record` builds rows and lines from plain values (no Praxis types), so its tests need no `HttpFilterContext`. Two thin Praxis filters (`cortex_attribution`, `cortex_observe`) adapt the Praxis hooks to `record` and `parse`. One emitter thread per process streams NDJSON to Cortex. A new binary crate `praxis/server` (`cortex-gateway`) registers the filters with `register_filters!`. In Go, `core/sessionapi/ingest.go` serves `POST /v1/ingest`, and `cmd/cortex` turns it on only when `praxis.enabled` and `listener.bind_loopback_only` are both set.

**Tech Stack:** Rust 1.96.0 (edition 2024), `praxis-proxy =0.8.0` and `praxis-proxy-filter =0.8.0`, `tokio`, `hyper` 1 (client, http1), `http-body-util`, `base64`, `chrono`, `metrics`, `yaml_serde`. Go 1.26.10.

**Spec:** `docs/superpowers/specs/2026-11-07-praxis-mode-design.md`, Sections 1, 2, 3 and 5. Issue: #1719. Tracker: #1714. Previous step: #1718 (plan `docs/superpowers/plans/2026-11-09-praxis-mode-1-inference-parser.md`, merged as PR #1741).

## Decisions this plan takes

Each one closes a question that the spec or the issue leaves open.

1. **No Praxis test helper exists outside Praxis** (`praxis_filter::test_utils` is `pub(crate)`), and the spec forbids an `HttpFilterContext` struct literal [T F10]. So the filters stay thin, and all logic is in `record` and `parse`, which take plain values. The filter behavior is tested end to end: the tests start the real `cortex-gateway` binary with a mock origin and a mock ingest endpoint.
2. **The token file path.** Cortex reads it from the env var `CORTEX_GATEWAY_TOKEN_FILE`. The gateway reads it from the `token_file` key of its filter config. The supervisor (#1720) creates the file, sets the env var and writes `gateway.yaml`. In this PR, tests create the file. Cortex never creates it.
3. **The `praxis` config block has only `enabled` in this PR.** `listen` and `egress` land with their callers (#1720, #1721). This PR also adds the two startup refusals (`praxis.enabled` with `mtls`, or without `bind_loopback_only`) and the restart rule for `praxis.*`, because the field lands here.
4. **The ingest counters** are served at `GET /v1/ingest/stats` on the session API, in praxis mode only. The spec names `/stats`, but `/stats` serves `auth.Stats` from plugins and is built before the session API. The route uses the loopback guards of the session API.
5. **`X-Cortex-Attribution` JSON**, base64url without padding. Keys: `agent`, `proc`, `clientProc`, `session`, `source`, `sessionHeader`, `chainSession`, `recordSession`. All are strings, all optional. The Go encoder (`attribution.Wire`) lands with the hop (#1721). A missing or bad header with a valid token gives `Attribution::default()`. Then `session` is empty, and ingestion files the row with `attribution.FiledSession` (the active session, then the default bucket), as a listener does.
6. **Fields the gateway cannot know in v1** stay unset: `identity` (no workload identity on a laptop), `tls` (the TLS bridge state is in Cortex; #1721 decides if the hop carries it), `invocations` (no plugins run), `requestedHost` (no router), `upgradeStripped` (Cortex strips upgrades before the hop), `tunnel`, `tunnelReason`, `dialAddr` (Cortex's own tunnel rows), `a2a`, `mcp` (step 6). The drift test lists each one with this reason. `plugins` is set by ingestion (cost).
7. **`client`, `subagent` and `parentSubagent` are ported to Rust** (`model.ParseUserAgent`, `subagentHeader`), because the end state needs them in Rust. A Go test writes a golden table that the Rust test reads.
8. **Messages of `client_canceled` and `upstream_error` rows** are Rust text (`"client closed the connection"`, `"upstream closed the connection"`). Go puts the Go error text there. Only the message differs. This is a known difference, as the 413 text is not one: the 413 text is copied exactly.
9. **The response-capture guard at ingestion** keeps the request ids of kept request captures until the response line arrives, and removes entries older than 1 hour. A 60 s window would drop the response capture of every long stream.
10. **Tunnel joins** (`requestId → tunnel`, Section 1 "Joins") land with the hop (#1721). This PR has no join to release when a line is dropped as erased. #1721 adds the release at that drop point (`ingest.go`, the `erased` branch).
11. **The replay test.** The "live run" is a direct-mode forward proxy with `inference-parser` and a rate table, over every `claude-code` fixture. A Go test converts its rows and captures to NDJSON lines, as the producer writes them, and commits the file. The same file is the golden for the Rust row builder (Task 10). So one file pins both sides.
12. **The emitter has its own thread** with a current-thread Tokio runtime, started once per process. It does not depend on the Praxis runtime, and `from_config` can run outside a runtime.
13. **`praxis-proxy` without default features**: `features = ["pingora", "admin-api", "forward-proxy"]`. This drops the policy engine and `aws-lc-sys` from the build. OpenSSL is vendored now (spec Section 4, "A standalone binary"), so CI needs no system OpenSSL.
14. **The measurement** (Done-when, last item) is a script and numbers in the PR body, not a CI gate.
15. **The Praxis 0.8.0 API names.** The filter hooks and context members that Task 6 names (`request_headers_to_remove`, `insert_filter_state`, `upstream_reached`, `stream_termination()`, `emit_deferred_record`, `Rejection::status`) were read in 0.7.5. After Task 1 fetches 0.8.0, read `~/.cargo/registry/src/*/praxis-proxy-filter-0.8.0/src`. When a name differs, use the 0.8.0 member with the same meaning. Do not add a Praxis fork or a patch.
16. **`emit_deferred_record` gets `&HttpFilterContext`, not `&mut`.** So `CortexObserve` keeps its exchange in filter state as `Arc<std::sync::Mutex<Exchange>>`, and each hook locks it.
17. **The ingest stream ends on shutdown** (spec [R 18], restarts stay quick). `New` registers `s.server.RegisterOnShutdown`. It sets a read deadline of now on each open ingest stream (`http.NewResponseController(w).SetReadDeadline`), so a blocked read returns and the handler ends.

## Global Constraints

- **Direct mode does not change.** With `praxis.enabled: false` (the default): `/v1/ingest` and `/v1/ingest/stats` answer 404, no erase mark is kept, no token file is read or created, no new error kind appears. Every existing Go test passes with no change to its expectations.
- Line format v1 (spec Section 1). Exact JSON keys: `v` (1), `schemaVersion` (1, `sessionapi.SchemaVersion`), `op` (`"append"`), `requestId`, `startedAt` (RFC 3339 with nanoseconds, UTC), `session`, `event` (SessionEvent wire JSON), `capture`, `cost`.
- `capture` JSON: `{"original": <base64 std>, "forwarded": null, "header": {"Name": ["v"]}, "query": "", "response": {"status", "header", "body", "decoded", "truncated", "ended", "incomplete"}, "dropped": false}`. A request row carries `original`, `header`, `query`. A response row carries `response` only. A dropped capture is exactly `{"dropped": true}`. Header names are canonical (Go `http.CanonicalHeaderKey`).
- `cost` JSON: `{"headers": {...}}`, response rows only, with only these headers when present: `X-Litellm-Response-Cost`, `X-Litellm-Response-Cost-Original`, `X-Litellm-Response-Cost-Discount-Amount`, `X-Litellm-Response-Cost-Margin-Amount`, `X-Litellm-Model-Name`, `Content-Type`.
- Caps: line 64 MiB (`64 << 20`); emitter queue 4096 lines and 256 MiB (`256 << 20`); request body 32 MiB (`32 << 20` = 33554432, `maxRequestBodySize`); buffered decoded response and SSE frame 10 MiB (`parse::MAX_BODY_SIZE`); response capture 10 MiB (`model.MaxResponseCapture`); erase mark 10 minutes.
- Error texts, exact: 413 `"request body of %d bytes is over the 33554432-byte buffer limit"` when `Content-Length` is known, else `"request body over the 33554432-byte buffer limit"`; buffered `"response body over the 10485760-byte buffer limit"`; frame `"single SSE frame over the 10485760-byte frame cap"`.
- Hop headers. The seven `wire.go` headers: `X-Cortex-Agent`, `X-Cortex-Proc`, `X-Cortex-Client-Proc`, `X-Cortex-Session`, `X-Cortex-Source`, `X-Cortex-Session-Header`, `X-Cortex-Chain-Session`. Plus `X-Cortex-Attribution`, `X-Cortex-Request-Id`, `X-Cortex-Probe`, `X-Cortex-Dest`, `X-Cortex-Token`. Before the upstream, remove the first ten and keep `X-Cortex-Dest` and `X-Cortex-Token`. The request capture removes all twelve. Any other `X-Cortex-…` header goes through.
- Error kinds from `X-Cortex-Egress-Error`: `upstream_dns`, `upstream_tls`, `upstream_timeout`, `upstream_refused`. Any other value becomes `upstream_error`.
- Rust lint gate, from `praxis/`: `cargo fmt --check`, `cargo clippy --all-targets --locked -- -D warnings`, `cargo test --locked`. Use the rustup `cargo` (`$HOME/.cargo/bin`), not Homebrew's, so `rust-toolchain.toml` applies.
- Go gate: `GOWORK=off go vet ./...` and `gofmt -l <path>` in each module touched (`core`, `cmd/cortex`).
- The full gate, from the worktree root: `export PATH="$HOME/.cargo/bin:$PATH" && (cd praxis && cargo fmt --check && cargo clippy --all-targets --locked -- -D warnings && cargo test --locked) && (cd core && GOWORK=off go vet ./... && GOWORK=off go test ./config/... ./reloader/... ./model/... ./pipeline/... ./redact/... ./session/... ./sessionapi/... ./cost/... ./listener/forwardproxy/...) && (cd cmd/cortex && GOWORK=off go vet ./... && GOWORK=off go test ./...) && test -z "$(gofmt -l core cmd/cortex)"`.
- Tasks 1 and 7 both edit `CLAUDE.md` and can run at the same time. Each edits only its own line (Task 1 the Layout row `praxis/`, Task 7 the "Needs a restart" list), so the merge is clean. Do not reflow other lines.
- Go is the source of truth for every port. When Rust and Go differ, fix Rust.
- Commits: `git commit -s`, message in ASD-STE100, ending with `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. No `Co-Authored-By`. PR title `Feat: Build cortex-gateway and the Cortex ingestion endpoint`, body with `Fixes #1719`.

## Review Focus

1. **A gzip response whose capture and byte counts must look like direct mode.** Expected: capture `decoded: true`, without `Content-Encoding` and `Content-Length`, `bytesDown` = decoded bytes; a `br` or uncoded response is captured as it came. Test: Task 5 `gzip_response_capture_is_decoded`, `identity_response_capture_is_raw`.
2. **A slow or dead Cortex.** Expected: traffic is never held up; the queue stays under 256 MiB, captures go first. Test: Task 4 `queue_drops_captures_before_rows`, `submit_never_blocks_when_ingest_is_down`.
3. **A client that sends its own hop headers or `X-Cortex-Custom`.** Expected: hop headers never reach the upstream (except Dest and Token), the custom header does. Test: Task 6 `hop_headers_removed_custom_kept`.
4. **An erase while lines are queued.** Expected: the erased session does not come back; a request that started after the erase starts a new session. Test: Task 8 `TestIngest_EraseDropsOlderLines`, `TestIngest_ClearDropsOlderLines`.
5. **A producer that sends its own cost record or an unredacted credential.** Expected: Cortex removes the cost record and settles again, and removes the credential again. Test: Task 8 `TestIngest_ProducerCostIsReplaced`, `TestIngest_CaptureRedactedAgain`.

---

## File Structure

```
praxis/
  Cargo.toml                    members = ["filters", "server"]
  Cargo.lock
  README.md                     + the gateway, how to run it, the decisions above in one line each
  filters/
    Cargo.toml                  + praxis-proxy-filter, async-trait, bytes, tokio, hyper, http-body-util,
                                  base64, chrono, metrics, yaml_serde (as serde_yaml)
    src/lib.rs                  pub mod parse; pub mod record; pub mod filters;
    src/record/mod.rs           pub mod attribution, client, emitter, line, redact, row;
    src/record/redact.rs        port of core/redact headers.go (Task 2)
    src/record/client.rs        port of model.ParseUserAgent and subagentHeader (Task 3)
    src/record/emitter.rs       Emitter (Task 4)
    src/record/line.rs          Line, Event, Capture, Cost wire types; drift test (Task 5)
    src/record/attribution.rs   Attribution, decode, token_matches, hop header lists (Task 5)
    src/record/row.rs           Exchange: the row builder (Task 5)
    src/filters/mod.rs          pub mod attribution, observe;
    src/filters/attribution.rs  CortexAttribution (Task 6)
    src/filters/observe.rs      CortexObserve (Task 6)
    tests/row_parity.rs         the replay NDJSON against the Rust builder (Task 10)
  server/
    Cargo.toml                  package cortex-gateway, bin cortex-gateway
    src/main.rs                 register_filters!, run the server (Task 1, filled in Task 6)
    tests/gateway.rs            end-to-end tests with the real binary (Task 6)
core/config/config.go, validate.go       Praxis block and refusals (Task 7)
core/reloader/reloader.go                praxis.* needs a restart (Task 7)
core/pipeline/client_golden_test.go      writes core/pipeline/testdata/user-agent.golden.json (Task 3)
core/sessionapi/ingest.go, ingest_test.go, erase_marks.go   (Task 8)
cmd/cortex/ingest.go, ingest_test.go, main.go               (Task 9)
core/listener/forwardproxy/ingest_replay_test.go            (Task 10)
core/sessionapi/testdata/ingest/replay.ndjson               (Task 10, generated)
scripts/dev/praxis-measure.sh                               (Task 11)
.github/workflows/praxis.yaml, docs/session-api.md, CLAUDE.md
```

---

### Task 1: Scaffold the workspace, the server crate and CI

Depends on: none

All later Rust tasks only fill files that this task creates, so parallel tasks do not touch `Cargo.toml`, `Cargo.lock` or a `mod` list.

**Files:**
- Modify: `praxis/Cargo.toml`, `praxis/filters/Cargo.toml`, `praxis/filters/src/lib.rs`, `praxis/filters/src/parse/inference/mod.rs`, `praxis/Cargo.lock`, `praxis/README.md`, `.github/workflows/praxis.yaml`, `CLAUDE.md` (Layout row `praxis/`: add "and the `cortex-gateway` binary")
- Create: `praxis/filters/src/record/{mod,attribution,client,emitter,line,redact,row}.rs`, `praxis/filters/src/filters/{mod,attribution,observe}.rs` (each a one-line `//!` doc comment; `emitter.rs` also holds `Queued`, below), `praxis/server/Cargo.toml`, `praxis/server/src/main.rs`

**Interfaces:**
- Produces: the module tree in File Structure. `praxis/server` is package `cortex-gateway` (`publish = false`, edition 2024, `rust-version = "1.96"`), with `[[bin]] name = "cortex-gateway"`.
- Produces, in `record/emitter.rs`, the one type that Tasks 4 and 5 share, so they can run at the same time: `pub struct Queued { pub row: Vec<u8>, pub with_capture: Option<Vec<u8>> }`. These are two serializations of one line, without and with its capture, each ending in `\n`. `row` holds `"capture":{"dropped":true}` when the line has a capture, and no `capture` key otherwise.

- [ ] **Step 1: Add the dependencies.** Filters crate: `praxis-proxy-filter = { version = "=0.8.0", default-features = false }`, `async-trait = "0.1"`, `bytes = "1"`, `tokio = { version = "1", features = ["rt", "net", "sync", "time", "io-util", "macros"] }`, `hyper = { version = "1", features = ["client", "http1"] }`, `http-body-util = "0.1"`, `base64 = "0.22"`, `chrono = { version = "0.4", default-features = false, features = ["clock", "std"] }`, `metrics = "0.24"`, `serde_yaml = { package = "yaml_serde", version = "0.10.7" }`. Server crate: `praxis-proxy = { version = "=0.8.0", default-features = false, features = ["pingora", "admin-api", "forward-proxy"] }`, `cortex-praxis-filters = { path = "../filters" }`, `openssl = { version = "0.10", features = ["vendored"] }`. If cargo reports a `links = "zstd"` conflict, raise the filters crate's `zstd` to the version Praxis uses (`0.14`); the API it uses does not change.
- [ ] **Step 2: Write `server/src/main.rs`**: install the crypto provider (`praxis::install_crypto_provider()`), read the config path from `-c <path>` or `PRAXIS_CONFIG`, load it (`praxis::load_config`), init tracing, and call `praxis::run_server_with_registry(config, custom_registry(), …)` with an empty `praxis_filter::register_filters! {}` for now. Copy the shape of Praxis's own `crates/server/src/main.rs` at v0.8.0.
- [ ] **Step 2b: Open `text` to the crate.** In `praxis/filters/src/parse/inference/mod.rs`, change `mod text;` to `pub(crate) mod text;`. Task 3 calls `is_control_rune` from `record::client`, and a private module is not visible there.
- [ ] **Step 3: Build.** `cargo build --locked` after `cargo generate-lockfile` → OK. Record the cold build time and `du -sh target`.
- [ ] **Step 4: Update `praxis.yaml`.** `timeout-minutes: 45`; target cap 3072 MB in place of 2048; add `core/sessionapi/ingest*.go`, `core/sessionapi/testdata/ingest/**`, `core/redact/**`, `core/pipeline/subagent.go`, `core/listener/forwardproxy/ingest_replay_test.go` to both `paths` lists (`core/pipeline/testdata/**` is already there, and it holds the Task 3 golden). Keep the blocking rule.
- [ ] **Step 5: Lint.** fmt, clippy, test → clean.
- [ ] **Step 6: Commit.** "Feat: Add the cortex-gateway crate and the record module scaffold".

### Task 2: Port credential removal (Rust)

Depends on: Task 1

**Files:**
- Modify: `praxis/filters/src/record/redact.rs`

**Interfaces:**
- Produces: `pub fn credential_name(name: &str) -> bool`, `pub fn headers(h: &http::HeaderMap) -> http::HeaderMap` (a copy without credential headers), `pub fn query(raw: &str) -> String`. Ports of `core/redact/headers.go`, same word list, same split on `-` and `_`, same rule that a query that does not parse is dropped whole.

- [ ] **Step 1: Write the failing tests**, one per Go test in `core/redact/headers_test.go`, with the same inputs and expected outputs: `headers_drops_credentials_and_copies`, `query_drops_credential_parameters`, `headers_drops_cloud_and_proxy_credentials`, `headers_and_query_drop_names_with_a_credential_word`, `credential_name_documented_examples`. Copy each Go case table verbatim.
- [ ] **Step 2: Run.** `cargo test -p cortex-praxis-filters redact` → FAIL.
- [ ] **Step 3: Implement.** For `query`, "does not parse" means what Go's `url.ParseQuery` refuses: a bad `%` escape or a `;`. Keep kept pairs in order and in their original escaping.
- [ ] **Step 4: Run** → PASS; fmt and clippy clean.
- [ ] **Step 5: Commit.** "Feat: Port core/redact header and query credential removal to Rust".

### Task 3: Port the user-agent and subagent rules (Rust, with a Go golden)

Depends on: Task 1

**Files:**
- Create: `core/pipeline/client_golden_test.go` (package `pipeline`, because `subagentHeader` is unexported there), `core/pipeline/testdata/user-agent.golden.json`
- Modify: `praxis/filters/src/record/client.rs`

**Interfaces:**
- Produces: `pub struct Client` (serde, camelCase, the JSON of `model.EventClient` with its omit rules), `pub fn parse_user_agent(ua: &str) -> Option<Client>` (port of `model.ParseUserAgent`, `core/model/client.go:234-590` with `capUA`, `productVersion`, `trailingKnownClient`), `pub fn subagent_header(v: Option<&str>) -> String` (port of `pipeline.subagentHeader`: trim SP and HTAB only, then refuse an id with control runes or longer than `maxClientLen`). Header names `X-Claude-Code-Agent-Id`, `X-Claude-Code-Parent-Agent-Id`.
- Consumes: `parse::inference::text::is_control_rune` (from #1718, made `pub(crate)` in Task 1).

- [ ] **Step 1: Write the Go golden test** `TestParseUserAgent_Golden` in package `pipeline`: a list of user-agent strings → `ParseUserAgent` JSON, written when the existing `-update` flag is set (it is declared in `fields_test.go`; do not declare a second one) and compared otherwise. Inputs: copy every user-agent string literal from the cases in `core/pipeline/client_test.go` into the test's own list; add every `User-Agent` read at test time from `../plugins/inferenceparser/testdata/*/*/request.headers`; add `""`, a 600-byte string and one with a control rune. Also a `subagent` section: header value → `subagentHeader` result, with the header values copied from `core/pipeline/subagent_test.go`. Run `GOWORK=off go test ./pipeline/ -run Golden -update` from `core`, then without `-update` → PASS.
- [ ] **Step 2: Write the failing Rust test** `user_agent_golden_matches_go`: read `../../core/pipeline/testdata/user-agent.golden.json`; for each case, `serde_json::to_value(parse_user_agent(ua))` equals the Go JSON; for each subagent case, `subagent_header` equals the Go string. Run → FAIL.
- [ ] **Step 3: Implement** the port, function by function.
- [ ] **Step 4: Run** → PASS; Go `GOWORK=off go test ./pipeline/ -run Golden` from `core` → PASS.
- [ ] **Step 5: Commit.** "Feat: Port the user-agent and subagent rules to Rust".

### Task 4: The emitter (Rust)

Depends on: Task 1

**Files:**
- Modify: `praxis/filters/src/record/emitter.rs`

**Interfaces:**
- Produces:
  - Consumes `Queued` (Task 1). Task 5 builds it.
  - `pub struct Emitter`; `pub fn Emitter::start(ingest_addr: std::net::SocketAddr, token: String) -> Arc<Emitter>` starts the thread; `pub fn submit(&self, q: Queued)` never blocks and never fails; `pub fn stats(&self) -> EmitterStats`.
  - `pub struct EmitterStats { pub rows_emitted: u64, pub rows_dropped: u64, pub captures_dropped_line_cap: u64, pub captures_dropped_queue_bytes: u64, pub queued_lines: usize, pub queued_bytes: usize }`.
  - Constants `MAX_QUEUE_LINES = 4096`, `MAX_QUEUE_BYTES = 256 << 20`, `MAX_LINE = 64 << 20`.
  - `pub fn global(ingest_addr, token) -> Arc<Emitter>`: one emitter per process (a `OnceLock`); a second call with other arguments returns the first one and logs a warning.

`submit` rules, in this order:
1. `with_capture` longer than `MAX_LINE` → use `row`, count `captures_dropped_line_cap`.
2. `queued_lines >= MAX_QUEUE_LINES` → drop, count `rows_dropped`.
3. Chosen bytes would take `queued_bytes` over `MAX_QUEUE_BYTES` and the chosen bytes are `with_capture` → use `row`, count `captures_dropped_queue_bytes`.
4. Still over → drop, count `rows_dropped`.
5. Else queue it and wake the sender.

The sender: a thread with a current-thread Tokio runtime. It connects first, and takes a line from the queue only on an open connection, so a failed connect drops no line (the no-server tests rely on this). It connects with `TcpStream`, `hyper::client::conn::http1::handshake`, and sends `POST /v1/ingest` with `Authorization: Bearer <token>`, `Content-Type: application/x-ndjson` and a streaming body (`http_body_util::StreamBody` over a `tokio::sync::mpsc` channel of `Bytes`). It moves lines from the queue to the body. On any error, it counts the line in flight as `rows_dropped`, waits (100 ms, doubling to 5 s, reset on success) and reconnects. `rows_emitted` counts lines written to the body. Each counter also increments the `metrics` counters `cortex_rows_emitted_total`, `cortex_rows_dropped_total`, `cortex_captures_dropped_total{reason="line_cap"|"queue_bytes"}`.

- [ ] **Step 1: Write the failing tests** (a mock ingest server on `127.0.0.1:0` in the test, reading lines):
  - `lines_arrive_in_order_with_token`: 3 lines → the server reads them in order, with `Authorization: Bearer t`.
  - `capture_over_line_cap_is_dropped_row_kept`: `with_capture` of `MAX_LINE + 1` bytes → the server reads `row`; `captures_dropped_line_cap == 1`.
  - `queue_drops_captures_before_rows`: no server listening (a closed port); submit 30 lines with a 10 MiB `with_capture` each → `queued_bytes <= MAX_QUEUE_BYTES`, `captures_dropped_queue_bytes > 0`, `rows_dropped == 0`.
  - `line_limit_drops_whole_lines`: no server; submit 4097 small lines → `rows_dropped == 1`.
  - `submit_never_blocks_when_ingest_is_down`: no server; 1000 submits finish in under 1 s.
  - `reconnects_after_ingest_restarts`: the server closes the connection after line 1, then accepts again → line 3 arrives.
- [ ] **Step 2: Run** `cargo test -p cortex-praxis-filters emitter` → FAIL.
- [ ] **Step 3: Implement** with a `std::sync::Mutex<VecDeque<Bytes>>` plus a `tokio::sync::Notify`, so `submit` takes only a short lock.
- [ ] **Step 4: Run** → PASS; fmt and clippy clean.
- [ ] **Step 5: Commit.** "Feat: Add the bounded NDJSON emitter for cortex-gateway".

### Task 5: The row builder (Rust)

Depends on: Task 2, Task 3

**Files:**
- Modify: `praxis/filters/src/record/{line,attribution,row}.rs`

**Interfaces:**
- Consumes: `record::emitter::Queued` (Task 1), `record::redact::{headers, query}` (Task 2), `record::client::{parse_user_agent, subagent_header, Client}` (Task 3), `parse::inference::InferenceObserver`, `parse::decode::{Coding, Decoder, decode_request_body}`, `parse::sse::FrameReader`, `parse::MAX_BODY_SIZE` (#1718). The mislabeled-SSE rule (`status / 100 == 2 && host == "chatgpt.com" && path == "/backend-api/codex/responses"`) moves here as `is_known_mislabeled_sse(status: u16, host: &str, path: &str) -> bool`, with the `mislabeled` test that #1741 removed (spec Section 3).
- Produces, in `record::attribution`:
  - `pub struct Attribution` (Decision 5 keys, `#[serde(default, rename_all = "camelCase")]`).
  - `pub fn decode(header: Option<&str>) -> Attribution` (bad base64url or bad JSON → default).
  - `pub fn token_matches(given: Option<&[u8]>, token: &[u8]) -> bool` (constant time; false on empty token).
  - `pub const UPSTREAM_REMOVE: [&str; 10]`, `pub const CAPTURE_REMOVE: [&str; 12]` (Global Constraints).
- Produces, in `record::line`: `Line`, `Event`, `EventError { kind, code, message }`, `Capture`, `ResponseCapture`, `Cost`, all `Serialize`, with the JSON of Global Constraints and of `core/pipeline/testdata/session-event.golden.json` (camelCase, Go's omit rules, `durationMs` and `ttfbMs` as integer milliseconds). `pub fn Line::queued(&self) -> emitter::Queued` writes the two serializations.
- Produces, in `record::row`:
  - `pub struct Exchange`, one per request.
  - `pub fn Exchange::new(method: &str, host: &str, path_and_query: &str, headers: &http::HeaderMap, attribution: Attribution, request_id: String, started: std::time::SystemTime, start: std::time::Instant) -> Exchange`.
  - `pub fn on_request_body(&mut self, body: &[u8]) -> Line`: the request row. It parses with `InferenceObserver::on_request` on the raw body (`on_request` calls `decode_request_body` itself, so do not decode twice), sets `bytesUp = body.len()`, `client`, `subagent`, `parentSubagent`, `proc`, `clientProc`, `sessionHeader`, and carries the capture only when `inference` is set: `original` = body, `header` = `redact::headers` of the request headers without `CAPTURE_REMOVE`, `query` = `redact::query`.
  - `pub fn on_response_head(&mut self, status: u16, headers: &mut http::HeaderMap) -> Result<(), Abort>`. It records the TTFB, takes and removes `X-Cortex-Egress-Error` (Global Constraints mapping), and picks the SSE mode (`text/event-stream`, or `is_known_mislabeled_sse`). It returns `Err(Abort::BufferedOverCap)` for a non-SSE response with no `Content-Encoding` and a `Content-Length` over `MAX_BODY_SIZE`.
  - `pub fn on_response_chunk(&mut self, chunk: &[u8]) -> Result<(), Abort>`: decodes for parsing, counts and captures, and returns `Err(Abort::FrameOverCap)` or `Err(Abort::BufferedOverCap)` at a cap.
  - `pub fn on_response_end(&mut self) -> Line`: the response row (`ended: true`).
  - `pub fn on_abort(&mut self, a: Abort) -> Line`: the cap rows.
  - `pub fn deferred(&mut self, status: u16, downstream_gone: bool, upstream_gone: bool) -> Vec<Line>`: the deferred rows. It first gives the request row when none was built.
  - `pub enum Abort { BufferedOverCap, FrameOverCap }`.

Response row rules (each from direct mode, cited in spec Sections 1 and 3):
- `inference` = `InferenceObserver::extension()` (the request and response facts together), read after the last `on_response_frame(headers, …, true)` call. That call already runs `settle_served_model`, so do not call it again.
- Capture only when the request capture was kept. When `Content-Encoding` is `gzip` alone: body = the decoder output, `decoded: true`, without `Content-Encoding` and `Content-Length` in the header. Otherwise: wire bytes, `decoded: false`, headers kept. Header = `redact::headers`. Up to 10 MiB, `truncated` past it. `incomplete = !ended`.
- `bytesDown` = decoded bytes for gzip, wire bytes otherwise.
- `cost` = the six headers when present.
- Kinds: `Abort::BufferedOverCap` → `statusCode` 502, `proxy_error`, `code` = the upstream status as a decimal string, the buffered text. `Abort::FrameOverCap` → `statusCode` = the upstream status, `proxy_error`, the frame text. `deferred`: status 413 → `statusCode` 413, `proxy_error`, the 413 text with the request's `Content-Length`; `downstream_gone` or status 499 → `statusCode` 499, `client_canceled`, Decision 8 text; `upstream_gone` → `statusCode` = status, `upstream_error`, Decision 8 text; otherwise `statusCode` = status, `gateway_refused`.

- [ ] **Step 1: Write the failing drift test** in `line.rs`: `every_session_event_field_is_set_or_listed`. Take the top-level keys of `../../core/pipeline/testdata/session-event.golden.json`, and the keys of `serde_json::to_value(full_event())`, where `full_event()` is a test helper with every field set. Assert golden ⊆ rust ∪ `NOT_SET_IN_PRAXIS_MODE`, and rust ⊆ golden. The list is `pub const NOT_SET_IN_PRAXIS_MODE: &[&str]` (public, because Task 10's integration test reads it): `sessionId`, `seq`, `at` ("the store sets it") and the Decision 6 fields, with each reason as a comment.
- [ ] **Step 2: Write the failing row tests** in `row.rs`, on the Claude Code fixture `core/plugins/inferenceparser/testdata/claude-code/main-stream-thinking-tool-use` (fed whole and in 4096-byte chunks). The gzip tests gzip that response in the test with `flate2` and set `Content-Encoding: gzip`. The `br` test sets `Content-Encoding: br` on the raw bytes:
  - `request_row_has_inference_and_capture`
  - `capture_headers_have_no_hop_or_credential_headers`: `Authorization` and `X-Cortex-Token` absent, `X-Cortex-Custom` present.
  - `non_inference_request_has_no_capture`: `POST /oauth/token`.
  - `gzip_response_capture_is_decoded`
  - `identity_response_capture_is_raw`
  - `br_response_capture_is_raw`: `decoded: false`, headers kept.
  - `egress_error_header_becomes_kind_and_is_removed`
  - `unknown_egress_error_is_upstream_error`
  - `buffered_over_cap_by_content_length_aborts_at_head`
  - `chunked_over_cap_row_is_502_proxy_error`: `code: "200"`.
  - `frame_over_cap_row_keeps_upstream_status`
  - `deferred_413_gives_request_and_response_rows`: the exact text with `Content-Length` 34603008.
  - `deferred_499_is_client_canceled`
  - `deferred_upstream_gone_is_upstream_error`
  - `deferred_other_is_gateway_refused`
  - `mislabeled_sse_streams_only_on_2xx`
  - `attribution_decode_bad_input_is_default`
  - `token_matches_rules`
  - `line_with_capture_over_cap_has_dropped_row`: a request with 30 MiB of ASCII message text. `Line::queued().with_capture` is over 64 MiB, and `row` parses with `capture == {"dropped": true}` and keeps `inference`.
- [ ] **Step 3: Run** `cargo test -p cortex-praxis-filters record` → FAIL.
- [ ] **Step 4: Implement** `line.rs`, `attribution.rs`, `row.rs`.
- [ ] **Step 5: Run** → PASS; fmt and clippy clean.
- [ ] **Step 6: Commit.** "Feat: Add the row builder for praxis-mode rows".

### Task 6: The filters, the binary and its end-to-end tests (Rust)

Depends on: Task 4, Task 5

**Files:**
- Modify: `praxis/filters/src/filters/{attribution,observe}.rs`, `praxis/server/src/main.rs`
- Create: `praxis/server/tests/gateway.rs`, `praxis/server/tests/testdata/gateway.yaml.tmpl`

**Interfaces:**
- Consumes: `record::attribution`, `record::row::Exchange`, `record::emitter::global` (Tasks 4, 5).
- Produces: filter names `cortex_attribution` (config `token_file`) and `cortex_observe` (config `token_file`, `ingest` as `host:port`), registered in `main.rs` with `register_filters! { http "cortex_attribution" => CortexAttribution::from_config, http "cortex_observe" => CortexObserve::from_config }`. A newtype `pub(crate) struct TokenValid(pub Attribution)`, defined in `filters/attribution.rs`, goes into `ctx.extensions`. The exchange is kept as Decision 16 says.

Filter behavior:
- `CortexAttribution::on_request`: when `token_matches(X-Cortex-Token)` is false → `Reject(Rejection::status(403))`. Else insert `TokenValid(decode(X-Cortex-Attribution))` (unless `X-Cortex-Probe` is present: then insert nothing and continue), and push `UPSTREAM_REMOVE` to `ctx.request_headers_to_remove`.
- `CortexObserve`: does nothing when `TokenValid` is absent. `request_body_access` → `ReadOnly`, `request_body_mode` → `StreamBuffer`, `response_body_access` → `ReadOnly`, `response_body_mode` → `Stream`. It keeps its `Exchange` in `ctx.insert_filter_state`. `on_request` builds the `Exchange` (request id from `X-Cortex-Request-Id`, else `ctx.request_id()`). `on_request_body` at end of stream submits the request row. `on_response` calls `on_response_head` on `ctx.response_header` (an `Abort` there → `Reject(Rejection::status(502))`, and the row is submitted). `on_response_body` feeds chunks (an `Abort` → submit `on_abort` and return `Err`, which aborts the connection), and at end of stream submits `on_response_end`. `emit_deferred_record(ctx, status)` submits `deferred(status, …)` and returns true, unless the exchange already submitted its response row (then false). `downstream_gone` and `upstream_gone` come from `ctx.upstream_reached` and `ctx.stream_termination()`; Step 3 sets the exact mapping from what the tests see.

The test harness: each test writes `gateway.yaml` from the template into a temp dir and starts `env!("CARGO_BIN_EXE_cortex-gateway")` with it. The YAML has one listener on a free port; the chain `cortex_attribution`, `cortex_observe`, `access_log`, router to a cluster on a mock origin; `body_limits.max_request_bytes: 33554432`, `max_response_bytes: null`; `insecure_options.allow_unbounded_body: true`, `allow_private_upstreams: true`, `allow_private_endpoints: true`. A mock origin (std `TcpListener`, one thread per connection) and a mock ingest (reads NDJSON lines into a channel) run in the test. A test waits for readiness by polling the port.

- [ ] **Step 1: Write the failing tests** in `server/tests/gateway.rs`:
  - `tokenless_request_gets_403_and_no_line`: the mock origin sees nothing, and the ingest sees no line in 1 s.
  - `probe_request_records_nothing`
  - `hop_headers_removed_custom_kept`: the origin sees `X-Cortex-Dest`, `X-Cortex-Token` and `X-Cortex-Custom`, and none of the other ten.
  - `inference_request_gives_request_and_response_lines`: the fixture `claude-code/main-stream-thinking-tool-use`, with the mock origin adding `X-Litellm-Response-Cost: 0.01` → two lines with one `requestId`, `session` = `recordSession`, response row with tokens and `cost.headers`.
  - `gzip_sse_streams_as_frames_and_wire_bytes_unchanged`: the client receives the gzip bytes unchanged, and receives the first frame before the origin sends the last one.
  - `mid_body_abort_is_client_canceled`: the client sends half of a body with `Content-Length`, then closes → a response row with `client_canceled`, 499.
  - `body_over_limit_is_413_proxy_error`: a 33 MiB body → the client gets 413; lines: request row and response row with `proxy_error` and the 413 text.
  - `buffered_over_cap_with_content_length_is_502`: an uncoded 12 MB JSON response → the client gets 502; the row is 502 `proxy_error`.
  - `frame_over_cap_aborts_stream`
  - `egress_error_header_becomes_kind`: the origin answers 502 with `X-Cortex-Egress-Error: upstream_dns` → the client does not get the header; the row kind is `upstream_dns`. An origin `X-Cortex-Upstream-Error` header reaches the client.
  - `without_access_log_no_deferred_row`: the same chain without `access_log` and a mid-body abort → no response line. This guards the upstream coupling (praxis#1228).
- [ ] **Step 2: Run** `cargo test -p cortex-gateway` → FAIL.
- [ ] **Step 3: Implement** the two filters and register them.
- [ ] **Step 4: Run** → PASS; fmt and clippy clean. When Praxis gives a status or termination cause that the `deferred` mapping does not expect, change the adapter in `observe.rs`, not the kinds in Task 5.
- [ ] **Step 5: Update `praxis/README.md`**: the binary, how to run the tests, and decisions 1, 2, 5 and 12 in one line each.
- [ ] **Step 6: Commit.** "Feat: Add the cortex_attribution and cortex_observe filters and the cortex-gateway binary".

### Task 7: The `praxis` config block (Go)

Depends on: none

**Files:**
- Modify: `core/config/config.go`, `core/config/validate.go`, `core/reloader/reloader.go`, `CLAUDE.md` (the "Needs a restart" list: add `praxis`)
- Test: `core/config/praxis_test.go`, `core/reloader/reloader_test.go`

**Interfaces:**
- Produces: `type PraxisConfig struct { Enabled bool \`yaml:"enabled" json:"enabled"\` }`; field `Praxis *PraxisConfig \`yaml:"praxis,omitempty" json:"praxis,omitempty"\`` on `Config`; `func (c *Config) PraxisEnabled() bool` (false on a nil block).

- [ ] **Step 1: Write the failing tests:**
  - `TestPraxis_DefaultOff`: no block → `PraxisEnabled()` false.
  - `TestPraxis_NeedsLoopbackOnly`: `praxis.enabled` without `listener.bind_loopback_only` → `Validate` error naming `listener.bind_loopback_only`.
  - `TestPraxis_RefusedWithMTLS`: with `mtls` set → error naming `mtls`.
  - `TestValidateReloadable_PraxisNeedsRestart`: a change of `praxis.enabled` → the diff names `praxis.*`.
- [ ] **Step 2: Run** `GOWORK=off go test ./config/ ./reloader/ -run 'Praxis'` from `core` → FAIL.
- [ ] **Step 3: Implement**: a `validatePraxis(cfg)` called from `Validate`, and one `reflect.DeepEqual(active.Praxis, next.Praxis)` branch in `validateReloadable`.
- [ ] **Step 4: Run** → PASS; `go vet ./...` and `gofmt -l config reloader` clean.
- [ ] **Step 5: Commit.** "Feat: Add the praxis.enabled config switch".

### Task 8: `POST /v1/ingest` (Go)

Depends on: none

**Files:**
- Create: `core/sessionapi/ingest.go`, `core/sessionapi/erase_marks.go`, `core/sessionapi/ingest_test.go`
- Modify: `core/sessionapi/server.go` (register the routes when ingest is configured), `docs/session-api.md` (a section for `POST /v1/ingest` and `GET /v1/ingest/stats`: auth, guards, line format v1, refusal reasons, the counters)

**Interfaces:**
- Produces:
  - `type Ingest struct { Token string; Rates pricing.Resolver }`
  - `func WithIngest(in Ingest) Option`
  - `POST /v1/ingest` and `GET /v1/ingest/stats`, registered only when `WithIngest` was given a non-empty `Token`.
  - `type IngestStats struct { Lines uint64 \`json:"ingest_lines_total"\`; Refused map[string]uint64 \`json:"ingest_refused_total"\`; CapturesDropped uint64 \`json:"ingest_captures_dropped_total"\` }`.
  - Refusal reasons: `bad_json`, `version`, `schema`, `op`, `line_cap`, `erased`.
- Consumes: `session.Store.AppendCapture`, `session.Store.AddRecorder`, `attribution.FiledSession`, `settle.SettleInput`, `settle.NewRecord`, `inference.SettleServedModel`, `event.Key`, `event.PluginName`, `redact.Headers`, `redact.Query`, `httpx.LoopbackHost`.

Handler rules, in order:
1. A non-loopback `Host` (`httpx.LoopbackHost`), or an `Origin` header → 403, with the texts `"ingest is accepted only for a loopback Host (localhost, 127.0.0.1 or [::1])"` and `"ingest is refused from a browser: the request carries an Origin header"`. Do not call `clearRefusal`: its texts say "erasing", and its third check is `allowClear`.
2. No `Authorization: Bearer <Token>` (`subtle.ConstantTimeCompare`) → 401.
3. Read the body line by line with a `bufio.Reader` and a 64 MiB cap per line. A line over the cap is discarded up to its `\n` and refused as `line_cap`. The connection stays open after any refusal. The answer, when the body ends, is 204.
4. For each line: unmarshal (`bad_json`); `v != 1` → `version`; `schemaVersion != SchemaVersion` → `schema`; `op != "append"` → `op`.
5. `sid := attribution.FiledSession(line.Session, store.ActiveSession)`. When the erase marks hold a mark for `sid` (or a clear) later than `startedAt` → `erased`. (#1721 releases the tunnel join here: Decision 10.)
6. Remove `Plugins["cost"]` and `Plugins["litellm-budget-track"]`. Zero `Seq` and `SessionID`. Set `At = time.Now()`.
7. A response row (`Phase == SessionResponse`): `inference.SettleServedModel(ev.Inference, hdr)`, then `settle.SettleInput(settle.Input{Host: ev.Host, Path: ev.HTTPPath, Inference: ev.Inference, ResponseHeaders: hdr}, Rates)`, with `hdr` built from `cost.headers`. When the result is `Priced`, `HasPrompt` or has a `RejectedReason`, write `json.Marshal(settle.NewRecord(s, nil))` to `Plugins[event.Key]` and `Plugins[event.PluginName]`. This is the gate of `inferenceparser/cost.go`.
8. Capture: `dropped` → count `CapturesDropped`, no capture. No `ev.Inference` → no capture. A request capture: redact `Header` and `Query` again, keep it, and remember `requestId`. A response capture: keep it only when `requestId` is remembered (then forget it), with `Header` redacted again. Entries older than 1 hour are removed when a response line is read (Decision 9).
9. `store.AppendCapture(sid, ev, capture)`; count `Lines`.

`erase_marks.go`: `type eraseMarks struct` implements `session.Recorder` (a no-op `Record`), `session.Deleter` and `session.Clearer`. It keeps the id → time and the last clear time for 10 minutes. `func (m *eraseMarks) erasedAfter(sid string, startedAt time.Time) bool`. It has an injectable clock. `Deleted` and `Cleared` run under the store's write lock, so they take only the marks' own mutex and never call the store. It is registered with `store.AddRecorder` in `New`, only when `WithIngest` was given. Tests reach the clock through an unexported field set in the test (package `sessionapi`), not through a new exported option.

- [ ] **Step 1: Write the failing tests** (a `session.Store` with a capture recorder, as in `forwardproxy/capture_test.go`):
  - `TestIngest_NotRegisteredWithoutOption`: `POST /v1/ingest` and `GET /v1/ingest/stats` → 404, and the store has no `eraseMarks` recorder.
  - `TestIngest_Unauthorized`: no token, wrong token → 401.
  - `TestIngest_RefusesOriginAndForeignHost` → 403.
  - `TestIngest_RefusalsCountedConnectionStays`: one body with a bad JSON line, `v: 2`, `schemaVersion: 2`, `op: "pair"`, a line over 64 MiB, then a good line → the good row is stored; `/v1/ingest/stats` counts each reason once.
  - `TestIngest_StoreAssignsSeqAndAt`: a producer's `seq: 99` and an old `at` → `seq` 1, `at` within 1 s of now.
  - `TestIngest_ProducerCostIsReplaced`: a response row with `plugins.cost` of 999 USD and `cost.headers` `X-Litellm-Response-Cost: 0.01` → `plugins.cost` is the settled 0.01 record, in both keys.
  - `TestIngest_CaptureRedactedAgain`: an `Authorization` header and `?api_key=` in a capture → both removed.
  - `TestIngest_CaptureOnlyForInference`
  - `TestIngest_DroppedCaptureKeepsRow`
  - `TestIngest_NoResponseCaptureWithoutRequestCapture`
  - `TestIngest_EraseDropsOlderLines`: append, `DELETE /v1/sessions/{id}`, a line with `startedAt` before the erase → refused `erased` and the session does not come back; a line with `startedAt` after → a new session.
  - `TestIngest_ClearDropsOlderLines`: the same with `DELETE /v1/sessions`.
  - `TestIngest_EraseMarkExpires`: with the clock 11 minutes on, the older line is kept.
  - `TestIngest_EmptySessionIsFiled`: `session: ""` → filed under the active session.
  - `TestIngest_ShutdownEndsStream`: an open ingest stream with no line in flight; `Server().Shutdown` with a 2 s context returns nil within 1 s (Decision 17).
- [ ] **Step 2: Run** `GOWORK=off go test ./sessionapi/ -run Ingest` from `core` → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** → PASS; also the whole `./sessionapi/ ./session/...` suite; `go vet ./...` and `gofmt -l sessionapi` clean.
- [ ] **Step 5: Commit.** "Feat: Add the praxis-mode ingestion endpoint to the session API".

### Task 9: Turn ingestion on in `cmd/cortex` (Go)

Depends on: Task 7, Task 8

**Files:**
- Create: `cmd/cortex/ingest.go`, `cmd/cortex/ingest_test.go`
- Modify: `cmd/cortex/main.go` (the `sessionapi.New` call at about line 1194)

**Interfaces:**
- Consumes: `config.Config.PraxisEnabled` (Task 7), `sessionapi.WithIngest`, `sessionapi.Ingest` (Task 8).
- Produces: `func ingestOptions(cfg *config.Config, rates pricing.Resolver, getenv func(string) string, readFile func(string) ([]byte, error)) ([]sessionapi.Option, error)`. It returns nil options when `!cfg.PraxisEnabled() || !cfg.Listener.BindLoopbackOnly`. Otherwise it reads the token from the file named by `CORTEX_GATEWAY_TOKEN_FILE`, trims white space, and returns `WithIngest{Token, rates}`. It returns an error when the variable is unset, the file cannot be read, or the token is empty. `main.go` calls `fatalf` on the error and appends the options to the `sessionapi.New` call, with `pricingRegistry` as `rates`.

- [ ] **Step 1: Write the failing tests:**
  - `TestIngestOptions_DirectModeNone`: praxis off → no options, and neither `getenv` nor `readFile` is called.
  - `TestIngestOptions_NeedsTokenFile`: praxis on, variable unset → error naming `CORTEX_GATEWAY_TOKEN_FILE`.
  - `TestIngestOptions_EmptyToken` → error.
  - `TestIngestOptions_ReadsToken`: `"abc\n"` → one option. A server built with it accepts `Bearer abc`.
- [ ] **Step 2: Run** `GOWORK=off go test -run IngestOptions .` from `cmd/cortex` → FAIL.
- [ ] **Step 3: Implement** and wire it into `main.go`.
- [ ] **Step 4: Run** → PASS; `GOWORK=off go test ./...` from `cmd/cortex` → PASS; vet and gofmt clean.
- [ ] **Step 5: Commit.** "Feat: Turn on ingestion in cortex when praxis mode is on".

### Task 10: The replay test and the row parity test

Depends on: Task 5, Task 8

**Files:**
- Create: `core/listener/forwardproxy/ingest_replay_test.go` (package `forwardproxy`, the same as `capture_test.go`. `sessionapi` does not import `forwardproxy`, so there is no cycle. Reuse `captures` and `proxyWithCaptures` from `capture_test.go`), `core/sessionapi/testdata/ingest/replay.ndjson`, `praxis/filters/tests/row_parity.rs`, `praxis/filters/tests/common/mod.rs`
- Modify: `praxis/filters/tests/parity.rs` (move its fixture loader, `Case`, `cases`, `load`, `header_map`, `read_body`, into `tests/common/mod.rs` and use `mod common;` from both tests)

**Interfaces:**
- Consumes: `sessionapi.WithIngest` (Task 8); `record::row::Exchange`, `record::line::Line` (Task 5).

- [ ] **Step 1: Write the Go test** `TestIngestReplay_MatchesLiveRun`:
  - **Live run:** a forward proxy with `inference-parser` and a fixed `pricing.Table`, a store with a usage aggregator and a capture recorder, and a mock origin that answers each `claude-code` fixture with its response file and headers (the `parsefixture` rules: `response.headers` when present, else the Content-Type of the response file's type; status 502 for `error-502-html`, else 200), plus `X-Litellm-Response-Cost` on half the cases. One request per fixture, in fixture order, all with one session header.
  - **Lines:** convert each stored event, with its capture, into a v1 line (`seq` and `sessionId` zeroed as a producer sends them, `startedAt` = the event's `At`, `cost.headers` = the cost headers the origin sent). With `-update`, write them to `testdata/ingest/replay.ndjson`. Otherwise read that file.
  - **Replay:** post the file to a second session API with `WithIngest` and an empty store with its own aggregator.
  - **Compare:** `/v1/sessions/{id}` events field by field without `at`; the `/v1/sessions` summaries without their time fields; the `/v1/usage` totals (tokens, cost, requests) without the buckets (spec Section 5, [DR 4]).
- [ ] **Step 2: Run** with `-update`, then without → PASS. If they differ, fix ingestion (Task 8 code), not the comparison.
- [ ] **Step 3: Write the Rust test** `row_parity_matches_replay`: for each request and response line in `replay.ndjson`, build the same exchange from the same fixture files with `Exchange` (the headers from `request.headers`, the response fed in 4096-byte chunks), and compare `event` and `capture` JSON. Leave out `durationMs`, `ttfbMs`, `requestId`, `plugins` (ingestion owns it) and every key in `NOT_SET_IN_PRAXIS_MODE` (direct mode sets `invocations`, and the store sets `at`). A known difference is allowed only for the captured request headers: Go captures the agent's headers at entry (spec Section 1, "Captures", [F 2]).
- [ ] **Step 4: Run** `cargo test -p cortex-praxis-filters --test row_parity` → PASS. If it fails, fix Rust.
- [ ] **Step 5: Commit.** "Test: Pin praxis-mode rows against a direct-mode replay".

### Task 11: Measure both processes

Depends on: Task 6, Task 9

**Files:**
- Create: `scripts/dev/praxis-measure.sh`

- [ ] **Step 1: Write the script.** It starts a scratch Cortex (a temp `HOME`, never the shared `:47600` proxy: other ports, `praxis.enabled: true`, `bind_loopback_only: true`, and `CORTEX_GATEWAY_TOKEN_FILE`), `cortex-gateway` with a generated `gateway.yaml`, and a Python 3 stdlib mock origin that answers with the largest `claude-code` fixture response. It sends N requests (default 200, alternating 7 MiB and 12 MiB request bodies, two `X-Claude-Code-Agent-Id` values for subagents) to the gateway with the token. Then it runs the same traffic through a direct-mode scratch Cortex. It prints, for each process: CPU time (`ps -o time`), peak RSS, and the heap from pprof where it is served (`listener.bind_loopback_only` serves `/debug/pprof/heap`).
- [ ] **Step 2: Run it** on macOS. Put the table in the PR body.
- [ ] **Step 3: Commit.** "Chore: Add a script that measures praxis mode against direct mode".

---

## Self-review notes

- Spec coverage: Section 1 → Tasks 4, 5, 8, 10. Section 2 → Tasks 5, 6. Section 3 (chain, record, emitter, drift test, binary) → Tasks 1, 4, 5, 6. Section 4 parts in scope (config refusals, vendored OpenSSL) → Tasks 1, 7. Section 5 Rust filter tests and Go contract tests in scope → Tasks 6, 8, 9, 10. Out of scope, by delivery step: tunnel pairing, Cortex cuts, the hop, the egress listener (#1721); supervision, the token file, `gateway.yaml` (#1720); agentop and install (step 5); MCP and A2A (step 6); the release job (step 7).
- Known edge cases from the PR #1717 review are accepted as the issue lists them: an in-memory erase mark, a check that is not atomic with the append, streams longer than 10 minutes, and `At` set at append.
