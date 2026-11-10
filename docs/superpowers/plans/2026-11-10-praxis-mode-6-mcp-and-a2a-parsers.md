# Praxis mode 6/8: the MCP and A2A parsers — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Port `core/parse/mcp` and `core/parse/a2a` to Rust, each behind its own parity gate, and put the `mcp` and `a2a` extensions on the praxis-mode rows (#1724).

**Architecture:** Two new modules in `cortex_parse`: `parse::mcp` and `parse::a2a`. Each one has a port of the Go functions (`parse_request`, `parse_response`) and an observer that ports the plugin dispatch (`OnRequest`, `OnResponseFrame` in `core/plugins/{mcpparser,a2aparser}/hooks.go`), as `InferenceObserver` does for inference. Two new parity tests read the #1503 fixtures under `core/plugins/{mcpparser,a2aparser}/testdata/<case>/` and compare with the Go `expected.json`. Then `record::row::Exchange` runs the three observers on each exchange, and the request and response rows carry `mcp` and `a2a`.

**Tech Stack:** Rust (`praxis/filters`, toolchain from `praxis/rust-toolchain.toml`), serde_json. No Go change.

**Spec:** `docs/superpowers/specs/2026-11-07-praxis-mode-design.md` (Section 3, "The parsers" and "The parity gate"; Section 5, "Parser parity"; Section 6 step 6). Issue: #1724. Earlier plan: `2026-11-09-praxis-mode-1-inference-parser.md` (the inference port and the parity gate).

## Global Constraints

- **The Go parser is the source of truth.** Do not change Go code or a Go `expected.json` to suit Rust. When the two differ, fix the Rust code.
- Decode with the rules of Go's `encoding/json`: use `crate::parse::inference::gojson` (case-insensitive keys, a later duplicate key wins, `null` is "no change", a type error fails the whole decode). Do not use plain serde derives to decode a body.
- Invalid UTF-8 in a body becomes U+FFFD, as in Go: use `crate::parse::inference::text::go_lossy_utf8` before `gojson::parse`.
- A Go `any` decodes with `gojson::any` (numbers become f64).
- Each Rust function has the snake_case name of the Go function it ports. Each module starts with a `//! Port of ...` line that names the Go file.
- Log nothing. The Go `slog` lines are not ported (the inference port does the same).
- Comments in ASD-STE100 style. Commits use `git commit -s`, a `<prefix>: <Subject>` title, and end with `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. No `Co-Authored-By`.
- Prose says Cortex for the product (CLAUDE.md naming rule).
- All commands run from `praxis/` unless a step says otherwise. rustup reads `rust-toolchain.toml` only below `praxis/`.

## Decisions (made for this plan, with reasons)

1. **The MCP path matcher is fixed to the Go default, `["/mcp"]`.** Reason: praxis mode runs no plugin config, so there is no `paths` to read. The fixtures use the default too. Port `bypass.Matcher.Match` for this one pattern: strip the query, apply a port of Go's `path.Clean`, and compare with `/mcp`. `/mcp` has no glob characters, so `path.Match` is a string compare. Add the comment `shortcut: only the default "/mcp" pattern, no globs; port path.Match when praxis mode reads mcp-parser config`.
2. **The response row carries the whole extension, request fields included.** Reason: direct mode does this (`core/listener/internal/sessionevent/sessionevent.go`, `SnapshotMCP`/`SnapshotA2A` on both rows). MCP params and A2A parts are small, so line format v1 does not need to move them to the request row only. Ingestion needs no change: `ingestLine.Event` is a `model.SessionEvent`, which already decodes `mcp` and `a2a`.
3. **The request row carries the extension as it is after `on_request`:** for MCP, no `result` and no `error`; for A2A, no response fields. Reason: direct mode snapshots the extension at request time.
4. **No capture for MCP or A2A rows.** Reason: `request_row` keeps a capture only for inference ("the body of any other request can be a credential"). That rule does not change.
5. **The observers are separate structs, one per parser, as `InferenceObserver` is.** `Exchange` holds all three. Reason: the Go plugins run one after the other on the same exchange. A request that two parsers claim is not possible (the method namespaces do not overlap, and inference has no `method`), but the code does not depend on it.
6. **Parity variants:** `buffered` (the whole body as one `last=true` frame) for every case. For an SSE case also `chunks_4096`, `chunks_1` and `crlf` (through `FrameReader`, as `parity.rs` does). No `gzip` variant. Reason: the parser never sees coded bytes. The `Decoder` is shared, and `parity.rs` already tests it.
7. **A2A `data` parts need a port of Go's `json.Marshal` for a decoded `any`:** `parse::a2a::go_marshal(&Value) -> String`. Rules: object keys sorted by bytes (serde_json `Map` is a `BTreeMap` in this workspace: `preserve_order` is not enabled; keep it so), no spaces, the five characters `<` `>` `&` U+2028 U+2029 written as the six-byte escapes `\u003c` `\u003e` `\u0026` `\u2028` `\u2029`, other escapes as Go 1.22+ `appendString` writes them (`\"`, `\\`, `\b`, `\f`, `\n`, `\r`, `\t`; every other byte below 0x20 as `\u00XX` with lowercase hex; 0x7F and other non-ASCII runes as raw UTF-8), and numbers as Go's `floatEncoder` writes a float64: `'f'` format with the shortest digits, but `'e'` format when `abs != 0 && (abs < 1e-6 || abs >= 1e21)`, with `e-07` cleaned to `e-7`, and a `+` on a positive exponent (`1e+21`). Reason: the part `content` is a string, so the parity test compares it byte for byte.
8. **`gojson` and `text` stay in `parse/inference/`.** T1 makes `gojson` `pub(crate)`. Reason: moving them is churn with no gain in this step. Phase D can move them.
9. **The parity tests reuse `tests/common/mod.rs`.** T1 adds `case_dirs(root: &Path) -> Vec<PathBuf>` (every sorted subdirectory of `root` with a `request.headers` file). `cases(agent)` calls it. The MCP and A2A testdata have no `<agent>` level. Reason: the loader, the header parser and `same` are already correct for these fixtures (`response.headers` replaces the default headers, a missing `request.json` is an empty body).
10. **The A2A session id does not change the row's session.** Reason: the row's session comes from attribution (`Line.session`), as for inference. Direct-mode outbound rows are keyed the same way. A change belongs to the store, not the parser.
11. **One PR.** The estimate is about 1,700 counted lines, under the 2,000 limit. T2 and T3 share no file, so they run at the same time.
12. **The shared JSON-RPC code is in T1, in `praxis/filters/src/parse/jsonrpc.rs`** (`//! Port of core/internal/parsercommon/parsercommon.go ...`). Reason: T2 and T3 run at the same time and both need the `JSONRPCRequest` decode and the `data:` line scan. Go shares them in `parsercommon` too. Without this, each task writes its own copy. Reuse the existing helpers: `crate::parse::decode::decode_request_body`, `crate::parse::sse::normalize_sse`, `crate::parse::inference::text::{go_lossy_utf8, go_trim_space}`.
13. **The A2A `path` argument is not ported.** Reason: Go's `a2a.ParseRequest` uses `path` only in `slog` lines, and this plan logs nothing.

## Review Focus

1. MCP `tools-call-sse-result-then-error`: the first response wins on both paths, and a later error frame does not set `error`. Pinned by `mcp_parity` and `later_response_frame_is_ignored` (T2).
2. MCP `ParseResponse` tries a whole-body JSON decode first, then scans `data:` lines. A buffered SSE body given as one `last=true` frame must still give the result. Pinned by `mcp_parity` (`buffered` variant of `tools-call-sse`).
3. A2A `message-stream-task-artifacts`: a Task event replaces the joined `artifact-update` text. Pinned by `a2a_parity`.
4. A2A `message-send-task-id-number`: a number `id` gives no `taskId`, and the decode does not fail. Pinned by `a2a_parity`.
5. A2A `data` part content is byte-equal to Go's `json.Marshal`. Pinned by `go_marshal_matches_go` (T3).
6. A request row does not show the response's MCP `result`. Pinned by `mcp_request_row_has_no_result` (T4).

---

## File map

| File | Task | Change |
|---|---|---|
| `praxis/filters/src/parse/model.rs` | 1 | `McpExtension`, `McpError`, `A2aExtension`, `A2aPart`; field-drift test for `mcp.` and `a2a.` |
| `praxis/filters/src/parse/mod.rs` | 1 | `pub mod jsonrpc; pub mod mcp; pub mod a2a;` |
| `praxis/filters/src/parse/jsonrpc.rs` (new) | 1 | `JsonRpcRequest`, `decode_request`, `for_each_data_payload` (Decision 12) |
| `praxis/filters/src/parse/mcp/mod.rs` (new, stub) | 1 → 2 | MCP port |
| `praxis/filters/src/parse/mcp/observer.rs` (new) | 2 | `McpObserver` |
| `praxis/filters/src/parse/a2a/mod.rs` (new, stub) | 1 → 3 | A2A port |
| `praxis/filters/src/parse/a2a/marshal.rs` (new) | 3 | `go_marshal` |
| `praxis/filters/src/parse/a2a/observer.rs` (new) | 3 | `A2aObserver` |
| `praxis/filters/src/parse/inference/mod.rs` | 1 | `pub(crate) mod gojson;` |
| `praxis/filters/tests/common/mod.rs` | 1 | `case_dirs` |
| `praxis/filters/tests/mcp_parity.rs` (new) | 2 | MCP parity gate |
| `praxis/filters/tests/a2a_parity.rs` (new) | 3 | A2A parity gate |
| `praxis/filters/src/record/row.rs` | 4 | run the MCP and A2A observers; `mcp` and `a2a` on both rows |
| `praxis/filters/src/record/line.rs` | 4 | `Event.mcp`, `Event.a2a`; remove both from `NOT_SET_IN_PRAXIS_MODE` |
| `praxis/README.md` | 4 | the parity rule names the three parsers and their regenerate commands |

## PRs

| PR | Title | Parent | Tasks | estLines |
|---|---|---|---|---|
| P1 | `feat: Port the MCP and A2A parsers to praxis mode` | "" | T1, T2, T3, T4 | 1700 |

---

### Task 1: The shared types, module stubs and fixture loader

Depends on: none. Estimate: 280 lines.

**Files:**
- Modify: `praxis/filters/src/parse/model.rs`
- Modify: `praxis/filters/src/parse/mod.rs`
- Create: `praxis/filters/src/parse/jsonrpc.rs` (Decision 12)
- Modify: `praxis/filters/src/parse/inference/mod.rs` (`mod gojson;` → `pub(crate) mod gojson;`)
- Create: `praxis/filters/src/parse/mcp/mod.rs`, `praxis/filters/src/parse/a2a/mod.rs` (each only its `//! Port of core/parse/<x>/<x>.go ...` doc line)
- Modify: `praxis/filters/tests/common/mod.rs`

**Interfaces (produces):**

```rust
// parse/model.rs. Mirrors model.MCPExtension and model.MCPError (core/model/extensions.go).
#[derive(Clone, Debug, Default, PartialEq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct McpExtension {
    #[serde(skip_serializing_if = "String::is_empty")] pub method: String,
    #[serde(skip_serializing_if = "Option::is_none")] pub rpc_id: Option<Value>,
    #[serde(skip_serializing_if = "Option::is_none")] pub params: Option<serde_json::Map<String, Value>>,
    #[serde(skip_serializing_if = "Option::is_none")] pub result: Option<serde_json::Map<String, Value>>,
    #[serde(rename = "error", skip_serializing_if = "Option::is_none")] pub err: Option<McpError>,
    #[serde(skip_serializing_if = "is_false")] pub is_action: bool,
}
#[derive(Clone, Debug, Default, PartialEq, Serialize)]
pub struct McpError {
    pub code: i64,
    pub message: String,
    #[serde(skip_serializing_if = "Option::is_none")] pub data: Option<Value>,
}
// Mirrors model.A2AExtension and model.A2APart.
#[derive(Clone, Debug, Default, PartialEq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct A2aExtension {
    method, rpc_id: Option<Value>, session_id, message_id, task_id, role: String (omit empty),
    parts: Vec<A2aPart> (omit empty),
    final_status, artifact, error_message: String (omit empty),
    is_action: bool (omit false),
}
#[derive(Clone, Debug, Default, PartialEq, Serialize)]
pub struct A2aPart { pub kind: String, #[serde(skip_serializing_if = "String::is_empty")] pub content: String }

// parse/jsonrpc.rs. Port of parsercommon.JSONRPCRequest.
pub(crate) struct JsonRpcRequest { pub method: String, pub id: Option<Value>, pub params: Option<serde_json::Map<String, Value>> }
impl JsonRpcRequest {
    /// Port of StringParam: params[key] when it is a string, else "".
    pub(crate) fn string_param(&self, key: &str) -> String;
    /// Port of MapParam: params[key] when it is an object, else None.
    pub(crate) fn map_param(&self, key: &str) -> Option<&serde_json::Map<String, Value>>;
}
/// `body` is already through decode_request_body and is not empty. None when the first
/// byte after ` \t\r\n` is not `{`, or when the gojson decode fails (a non-object
/// `params` is a type error; a second `params` key merges into the first map, later key wins;
/// `null` leaves it None; each value through `gojson::any`).
pub(crate) fn decode_request(body: &[u8]) -> Option<JsonRpcRequest>;
/// The Go `data:` scan: normalize_sse, split on b'\n', go_trim_space each line, keep lines
/// that start with `data:`, go_trim_space the rest, skip empty. Calls f on each payload in
/// order and stops when f returns true.
pub(crate) fn for_each_data_payload(body: &[u8], f: impl FnMut(&[u8]) -> bool);

// tests/common/mod.rs
/// Every subdirectory of `root` that has a request.headers file, sorted.
pub fn case_dirs(root: &Path) -> Vec<PathBuf>;
```

Go's `omitempty` on `Params map[string]any` omits an empty map too. So `params: Some(empty)` must serialize as absent: use `skip_serializing_if = "map_is_empty"` with `fn map_is_empty(m: &Option<Map<..>>) -> bool { m.as_ref().is_none_or(|m| m.is_empty()) }`. The same for `result`. Note the difference from the parser's view: Go's `ParseResponse` checks `rpc.Result != nil`, and an empty `{}` result is not nil. So the parser keeps `Some(empty)`, and only the JSON omits it.

- [ ] **Step 1: Write the failing tests** in `model.rs` `mod tests`:
  - `mcp_a2a_zero_values_follow_go_omitempty`: `McpExtension::default()` → `{}`; `McpExtension { params: Some(Map::new()), ..}` → `{}`; `McpError::default()` → `{"code":0,"message":""}`; `A2aExtension::default()` → `{}`; `A2aPart::default()` → `{"kind":""}`.
  - In `jsonrpc.rs` `mod tests`: `decode_request_rules` (`[1]` and ` x` give None; `{"method":"m","params":1}` gives None; `{"method":"m","params":{"a":1},"params":{"b":2}}` gives params with both `a` and `b`; `{"METHOD":"m"}` gives method `m`) and `data_payloads_follow_go` (`"\u{feff}data: a\r\n\r\ndata:\ndata:b\n: c"` gives `["a", "b"]`; a `true` return stops after the first).
  - `every_go_mcp_field_is_set_or_listed` and `every_go_a2a_field_is_set_or_listed`: the same check as `every_go_inference_field_is_set_or_listed`, for the `mcp.` and `a2a.` lines of `core/pipeline/testdata/parsed-request.fields`. Move the body of the inference test into `fn check_fields(prefix: &str, sample: Value, not_set: &[&str])` and call it three times. `mcp` and `a2a` have no not-set fields. `kind == "map"` paths use `{}` (see `parsefixture.checkFieldPaths`): extend `walk` so that the children of a `map` path go under `<path>{}`.
- [ ] **Step 2: Run** `cargo test -p cortex-praxis-filters --lib model` and then `cargo test -p cortex-praxis-filters --lib jsonrpc` Expected: both FAIL (types missing).
- [ ] **Step 3: Implement** the types, `jsonrpc.rs`, the module declarations, the `pub(crate)` change and `case_dirs`. Add `#[allow(dead_code)]` on the `jsonrpc` items only if clippy reports them unused before T2 and T3 use them; T4's step 4 removes any such allow that is no longer needed. Change `cases(agent)` to call `case_dirs`.
- [ ] **Step 4: Run** `cargo test -p cortex-praxis-filters && cargo clippy --all-targets --locked -- -D warnings` Expected: PASS (an unused `case_dirs` is allowed by the module's `#![allow(dead_code)]`).
- [ ] **Step 5: Commit** `feat: Add the MCP and A2A extension types to the praxis filters`.

### Task 2: Port the MCP parser, behind its parity gate

Depends on: T1. Estimate: 550 lines.

**Files:**
- Modify: `praxis/filters/src/parse/mcp/mod.rs`
- Create: `praxis/filters/src/parse/mcp/observer.rs`
- Create: `praxis/filters/tests/mcp_parity.rs`

**Interfaces (produces):**

```rust
pub use observer::McpObserver;
/// Port of mcp.ParseRequest with the default paths matcher ["/mcp"]. `path` may have a query.
pub fn parse_request(method: &str, path: &str, headers: &HeaderMap, body: &[u8]) -> Option<McpExtension>;
/// Port of mcp.ParseResponse.
pub fn parse_response(body: &[u8], ext: &mut McpExtension) -> bool;

pub struct McpObserver { /* ext: Option<McpExtension> */ }
impl McpObserver {
    /// Port of MCPParser.OnRequest.
    pub fn on_request(method: &str, path: &str, headers: &HeaderMap, body: &[u8]) -> Self;
    /// Port of MCPParser.OnResponseFrame (the Observe/Skip calls are not ported).
    pub fn on_response_frame(&mut self, frame: &[u8], last: bool);
    pub fn extension(&self) -> Option<&McpExtension>;
}
```

**Port notes (read `core/parse/mcp/mcp.go` and `core/plugins/mcpparser/hooks.go` in full first):**
- `parse_request`: `decode_request_body` first. An empty body: `DELETE` with a non-empty `Mcp-Session-Id` on the MCP path gives `$transport/terminate`; `GET` on the MCP path gives `$transport/stream`; else `None`. The method compare is exact (`"DELETE"`, `"GET"`), as in Go.
- A non-empty body goes to `jsonrpc::decode_request` (T1). `None` is not claimed. A method outside `is_mcp_method` is not claimed.
- `parse_response` / `parseResponse`: first a whole-body decode of `jsonRPCResponse` (`id` any, `result` `map[string]any`, `error` `*jsonRPCError` with `code` int, `message` string, `data` any). It counts only when there is no decode error and `result` or `error` is non-nil. Else `normalize_sse` the body, split on `\n`, trim each line (Go `bytes.TrimSpace`: ASCII space, `\t\n\v\f\r`, plus U+0085 and U+00A0 as UTF-8), take lines that start with `data:`, trim the rest, skip empty, decode each; the first with a result or error wins. Use `jsonrpc::for_each_data_payload` (T1) for this scan. `code` is a Go `int`: a fraction or an out-of-range number is a type error (use `gojson::int`).
- On an error response: set `err`, and do not touch `result`. On a result: set `result`.
- Observer `on_response_frame`: no extension → nothing. Empty frame → nothing (the Skip/Observe is not ported). `result` or `err` already set → ignore the frame. Else `parse_response(frame, ext)`.

- [ ] **Step 1: Write the failing parity test** `tests/mcp_parity.rs`. For each dir in `common::case_dirs(<CARGO_MANIFEST_DIR>/../../core/plugins/mcpparser/testdata)`, `load` it, run each variant (Decision 6), serialize `observer.extension()` (`Value::Null` for `None`), and `same(&expected, &got, name)`. Assert that at least 11 cases ran. Variants:
  - `buffered`: `McpObserver::on_request(&case.method, &case.target, &case.req_headers, &case.req_body)`, then `on_response_frame(&case.resp_body, true)`.
  - SSE only, `chunks_4096` / `chunks_1` / `crlf`: as `parity.rs` `chunks`, through `FrameReader::new(MAX_BODY_SIZE)`, each frame with `last=false`, then `on_response_frame(b"", true)`.
- [ ] **Step 2: Write the failing unit tests** in `mcp/mod.rs`: `later_response_frame_is_ignored` (a result frame, then an error frame: `err` stays `None`); `path_clean_matches_go` (`/mcp/` `//mcp` `/a/../mcp` `/mcp?x=1` match; `/mcpx` `/mcp/x` do not); `non_object_params_is_not_claimed`; `empty_result_object_counts` (`{"result":{}}` returns true, and the JSON omits `result`); `error_code_fraction_is_not_a_response` (`{"error":{"code":1.5,"message":"m"}}` returns false).
- [ ] **Step 3: Run** `cargo test -p cortex-praxis-filters --test mcp_parity --lib mcp` Expected: FAIL.
- [ ] **Step 4: Implement** `mcp/mod.rs` and `mcp/observer.rs`.
- [ ] **Step 5: Run** `cargo test -p cortex-praxis-filters && cargo clippy --all-targets --locked -- -D warnings && cargo fmt --check` Expected: PASS.
- [ ] **Step 6: Commit** `feat: Port the MCP parser to praxis mode`.

### Task 3: Port the A2A parser, behind its parity gate

Depends on: T1. Estimate: 700 lines.

**Files:**
- Modify: `praxis/filters/src/parse/a2a/mod.rs`
- Create: `praxis/filters/src/parse/a2a/marshal.rs`
- Create: `praxis/filters/src/parse/a2a/observer.rs`
- Create: `praxis/filters/tests/a2a_parity.rs`

**Interfaces (produces):**

```rust
pub use observer::A2aObserver;
/// Port of a2a.ParseRequest.
pub fn parse_request(headers: &HeaderMap, body: &[u8]) -> Option<A2aExtension>;
/// Port of a2a.ParseResponse: folds one body or one SSE frame into ext.
pub fn parse_response(body: &[u8], ext: &mut A2aExtension);
/// Go's json.Marshal of a decoded `any` (Decision 7).
pub(crate) fn go_marshal(v: &Value) -> String;

pub struct A2aObserver { /* ext: Option<A2aExtension> */ }
impl A2aObserver {
    /// Port of A2AParser.OnRequest.
    pub fn on_request(headers: &HeaderMap, body: &[u8]) -> Self;
    /// Port of A2AParser.OnResponseFrame: a non-empty frame goes to parse_response.
    pub fn on_response_frame(&mut self, frame: &[u8], last: bool);
    pub fn extension(&self) -> Option<&A2aExtension>;
}
```

**Port notes (read `core/parse/a2a/a2a.go` and `core/plugins/a2aparser/hooks.go` in full first):**
- `parse_request`: `decode_request_body`; an empty body is not claimed; else `jsonrpc::decode_request` (T1), and `None` or a method outside `is_a2a_method` is not claimed. Then the field order of the Go code: `contextId`, else `sessionId` from params (`string_param`); `params.message` (`map_param`): `taskId`, `contextId` (only when no session id yet), `role`, `messageId` (string values only), `parts` (an array only) through `parse_a2a_parts`. Then `params.id` when `task_id_param_is_id(method)` and no task id, then `params.taskId`.
- `parse_a2a_parts`: skip a non-object part, and a part whose `kind` is not a non-empty string. `text` → `text` string; `file` → `data` string, else `uri` string; `data` → `go_marshal(data)` when `data` is present and not null. Other kinds → empty content.
- `parse_response`: `extract_response_summary`, then `extract_session_id` only when `session_id` is empty.
- `extract_response_summary`: when the whole body is valid JSON (`gojson::parse` succeeds on the lossy text), `fold_response(body)`. Else `jsonrpc::for_each_data_payload` (T1), `fold_response` on each payload, never stopping early.
- `extract_send_response`: decode the Go struct with `gojson`. Any decode error, or an empty `result.status.state`, returns false. `kind` and `id` are `any` (never a type error). Note: `status.message.parts[].kind` and `.text` are strings, so a number there is a type error and the whole decode fails. Artifacts: the last non-empty `text` part of all artifacts wins (assign, not append). `errorMessage` only for state `failed`: the first non-empty text part.
- `extract_stream_event`: kind `artifact-update` or `artifact`: append each non-empty text part with `append_capped` (64 KiB cap, marker `…(truncated)` once). `kind` is a `string` here (not `any`), so a non-string kind fails this decode. `append_capped` cuts at a byte index, as Go does. A Go string can hold a cut UTF-8 sequence, and Go's `json.Marshal` writes U+FFFD for it. A Rust `String` cannot hold it. So keep `artifact` as a `String`, and append `String::from_utf8_lossy(&s.as_bytes()[..remaining])` then the marker. The JSON is then the same as Go's. Measure the cap on the Rust `String`'s byte length. Add a unit test with a multi-byte rune at the cut.
- `result_task_id`: kind `"task"` (a string compare on the `any`): the `id` when it is a string, else empty. Other kinds: `taskId`.
- `session_id_from_json`: decode `result.contextId` and `result.sessionId` (strings). An error gives empty. contextId first.
- `extract_session_id`: `session_id_from_json(body)`; when empty, `jsonrpc::for_each_data_payload` and stop at the first payload with a non-empty id. (Go does not skip an empty payload here, but an empty payload gives an empty id, so the shared scan is equal.)
- `go_marshal` (`marshal.rs`): Decision 7. The input is a `gojson::any` value, so every number is an f64.

- [ ] **Step 1: Write the failing parity test** `tests/a2a_parity.rs`, as `mcp_parity.rs`, for `core/plugins/a2aparser/testdata` (at least 13 cases). `A2aObserver::on_request(&case.req_headers, &case.req_body)`.
- [ ] **Step 2: Write the failing unit tests:** `go_marshal_matches_go` with these exact outputs, written as Rust raw strings (`r#"..."#`) so that each backslash is one byte: input `{"b":1,"a":[true,null,"<&>"]}` gives `{"a":[true,null,"\u003c\u0026\u003e"],"b":1}`; `1e21` gives `1e+21`; `1e-7` gives `1e-07` cleaned to `1e-7`; `0.000001` gives `0.000001`; `123456789012345680000` gives `123456789012345680000`; `1.5` gives `1.5`; `-0.0` gives `-0`; the JSON string `" \u0001\b\u2028"` (decoded: space, U+0001, U+0008, U+2028) gives `" \u0001\b\u2028"`. Plus `append_capped_cuts_once`, `append_capped_cut_rune_is_replaced`, `data_part_content_is_go_json`, `number_task_id_gives_no_task_id`.
- [ ] **Step 3: Run** `cargo test -p cortex-praxis-filters --test a2a_parity --lib a2a` Expected: FAIL.
- [ ] **Step 4: Implement** `a2a/mod.rs`, `a2a/marshal.rs`, `a2a/observer.rs`.
- [ ] **Step 5: Run** `cargo test -p cortex-praxis-filters && cargo clippy --all-targets --locked -- -D warnings && cargo fmt --check` Expected: PASS.
- [ ] **Step 6: Commit** `feat: Port the A2A parser to praxis mode`.

### Task 4: Put `mcp` and `a2a` on the praxis-mode rows

Depends on: T2, T3. Estimate: 170 lines.

**Files:**
- Modify: `praxis/filters/src/record/line.rs` (`Event` gets `a2a: Option<A2aExtension>` and `mcp: Option<McpExtension>`, both `skip_serializing_if = "Option::is_none"`; remove `"a2a"` and `"mcp"` and their comment from `NOT_SET_IN_PRAXIS_MODE`; set both in `full_event()` of the drift test)
- Modify: `praxis/filters/src/record/row.rs`
- Modify: `praxis/README.md` (first bullet: "ports of the Go inference, MCP and A2A parsers"; "The parity rule": name `parity.rs`, `mcp_parity.rs`, `a2a_parity.rs` and the two new regenerate commands `GOWORK=off go -C ../core test ./plugins/mcpparser -run TestFixtures -update` and the same for `a2aparser`)

**Changes in `row.rs`:**
- `Exchange` gets `mcp: Option<McpObserver>` and `a2a: Option<A2aObserver>`. `on_request_body` builds both next to the inference observer: `McpObserver::on_request(&self.method, &target, &self.headers, body)` and `A2aObserver::on_request(&self.headers, body)`.
- `Response::feed` takes the three observers. Group them in one private struct `Observers { inference, mcp, a2a }` in `row.rs`, with `fn frame(&mut self, headers: &HeaderMap, f: &[u8], last: bool)` that calls each one. Replace every `self.observer` call site (`feed`, `on_response_end`'s flush and last frame) with it. Reason: one call site per frame, so a later parser cannot be missed.
- `request_row`: `mcp` and `a2a` are clones of the extensions after `on_request` (Decision 3). `response_row`: clones of the extensions at the end (Decision 2). The deferred rows (`deferred`, `on_abort`) go through `response_row`, so they get them too.

- [ ] **Step 1: Write the failing tests** in `row.rs` `mod tests`, with the fixture loader of the existing tests pointed at `core/plugins/mcpparser/testdata/tools-call-sse` and `core/plugins/a2aparser/testdata/message-stream`:
  - `mcp_request_row_has_no_result`: the request row has `event.mcp.method == "tools/call"`, `params`, and no `result`. The response row has `result`, and `event.mcp` equals the case's `expected.json` (use `same`).
  - `a2a_rows_carry_the_extension`: the response row's `event.a2a` equals `expected.json`. The request row has no `finalStatus`.
  - `mcp_row_has_no_capture`: both lines have `capture == None`.
- [ ] **Step 2: Run** `cargo test -p cortex-praxis-filters --lib row` Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `cargo test -p cortex-praxis-filters && cargo clippy --all-targets --locked -- -D warnings && cargo fmt --check` Expected: PASS. Remove any `#[allow(dead_code)]` that T1 put on `jsonrpc` items. `row_parity` still passes: the replay rows are inference rows with no `mcp` or `a2a`.
- [ ] **Step 5: Run the Go goldens once, to show they did not change:** from the repo root, `GOWORK=off go -C core test ./plugins/mcpparser/ ./plugins/a2aparser/ -run Fixtures -count=1` Expected: PASS.
- [ ] **Step 6: Commit** `feat: Record MCP and A2A facts on praxis-mode rows`.

## Gate

From the worktree root:

```sh
(cd praxis && cargo fmt --check && cargo clippy --all-targets --locked -- -D warnings && cargo test --locked) && GOWORK=off go -C core test ./plugins/mcpparser/ ./plugins/a2aparser/ -run Fixtures -count=1
```

## Self-review

- Issue scope: MCP port → T2. A2A port → T3. Each behind its own parity gate → `mcp_parity.rs` (T2), `a2a_parity.rs` (T3). "Every MCP and A2A fixture from #1503 gives the same event JSON in Go and in Rust" → both parity tests run every case directory and assert a minimum count.
- Spec Section 3 "writes the parsed request and response into filter metadata in Cortex's types (`inference`, `mcp`, `a2a`)": the row builder holds the parsed facts and writes them on the rows (T4). The `line.rs` drift list said "The a2a and mcp parsers move at step 6"; T4 removes that entry, so the schema drift test now checks both fields.
- CI: `.github/workflows/praxis.yaml` already runs on `core/plugins/{mcpparser,a2aparser}/**` and reports without blocking there. No workflow change.
- Gate check (plan review, 11-10): `cargo fmt --check` from `praxis/` passes on the base. The gate has no Go format or vet step: this plan changes no Go code.
- Out of scope: MCP `paths` config (Decision 1), the A2A session rekey (Decision 10), the release job (step 7).
