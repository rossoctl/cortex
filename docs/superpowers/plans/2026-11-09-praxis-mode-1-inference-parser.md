# Praxis mode 1/8: Rust workspace and inference parser — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add the Rust workspace `praxis/` with a port of `core/parse/inference` that gives the same `InferenceExtension` JSON as Go on every saved fixture, plus the gates that keep the two in step.

**Architecture:** One library crate, `cortex-praxis-filters`, with a module `parse` (no Praxis filter yet). `parse::inference` is a function-by-function port of `core/parse/inference` and of the dispatch in `core/plugins/inferenceparser/hooks.go`. `parse::sse` and `parse::decode` port the listener's SSE framing and add decoding with caps on decoded bytes. Rust tests read the Go golden files in place, so Go stays the source of truth.

**Tech Stack:** Rust 1.96.0, edition 2024; `serde`, `serde_json` (`raw_value`), `http`, `flate2`, `zstd`. GitHub Actions.

**Spec:** `docs/superpowers/specs/2026-11-07-praxis-mode-design.md`, Section 3 ("The parsers", "The chain" → `cortex_parse`) and Section 4 ("Release and install", the CI lines). Issue: #1718. Tracker: #1714 (decision D6: Cortex owns the Rust parsers).

## Global Constraints

- `praxis/` is outside `go.work`. No `go.mod`, `go.sum` or `go.work` changes. No Go code changes. The only Go-tree changes are new fixture cases under `core/plugins/inferenceparser/testdata/synthetic/` and their README section.
- **The Go parser is the source of truth.** When Rust and Go differ, fix Rust. Never change Go behavior or a Go golden file to suit Rust.
- **No `praxis-proxy` dependency in this PR.** The crate uses no Praxis API yet. #1719 adds `praxis-proxy =0.8.0` with the first filter.
- Toolchain pinned in `praxis/rust-toolchain.toml`: `channel = "1.96.0"`, components `rustfmt`, `clippy`. `rust-version = "1.96"`, `edition = "2024"`, `publish = false`, `Cargo.lock` committed.
- Caps, from `core/listener/forwardproxy/server.go` (`maxBodySize`): `MAX_BODY_SIZE: usize = 10 << 20` (10485760). It is both the buffered decoded-body cap and the per-frame cap.
- Error texts, verbatim (used by #1719 for the row message): buffered `"response body over the 10485760-byte buffer limit"`, frame `"single SSE frame over the 10485760-byte frame cap"`.
- Mislabeled SSE, from `forwardproxy/stream.go:165` and `server.go:926-933`: `status / 100 == 2 && host == "chatgpt.com" && path == "/backend-api/codex/responses"`.
- JSON shape: field names and omit rules exactly as the Go struct tags in `core/model/extensions.go` (`omitempty` → `skip_serializing_if` for zero, empty string, empty `Vec`, `None`, `false`). `InferenceThinking.blocks` has no `omitempty`.
- No logging. Go's `slog` lines are diagnostics only.
- Rust lint gate: `cargo fmt --check`, `cargo clippy --all-targets --locked -- -D warnings`, `cargo test --locked`, all from `praxis/`.
- CI actions pinned by commit SHA with a version comment, as in `ci.yaml`. `Swatinem/rust-cache@d1527c8f67ef9af357290475d4af4b0fab8f12e0  # v2.9.3`.
- Commits: `git commit -s`, message in ASD-STE100, ending with `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>`. No `Co-Authored-By`. PR title `Feat: …`.

## Review Focus

1. **gzip- or zstd-coded responses** (gateways send them, and praxis-ai miscounts them). Expected: the same extension as the plain body. Test: Task 4, parity variant `gzip`, and Task 3 `decode_*` tests.
2. **SSE chunk boundaries that do not match event boundaries** (Praxis hands body chunks of any size). Expected: the same extension. Test: Task 4, parity variants `chunks_1` and `chunks_4096`.
3. **CRLF and CR line endings, and a leading BOM.** Expected: the same frames as LF. Test: Task 3 `frame_reader_*`, Task 4 variant `crlf`.
4. **Invalid UTF-8 inside a JSON string.** `serde_json` refuses the whole body. Go replaces each invalid byte with U+FFFD, parses it, and keeps `contentBytes` as the raw size. Test: Task 4 `invalid_utf8_matches_go`.
5. **A compression bomb or an oversized frame.** Expected: an error at the cap, with memory held under the cap plus one chunk. Test: Task 3 `decode_stops_at_cap`, `frame_reader_cap`.

Not covered, and accepted: the Go plugin tests (`core/plugins/inferenceparser/*_test.go`, about 150 tests) pin more rules than the corpus does. The port follows the Go code function by function, and the reviewer compares them side by side. The row-level parity (the whole `SessionEvent`) comes in #1719.

---

## File Structure

```
praxis/
  Cargo.toml                 workspace: members = ["filters"], resolver = "3"
  Cargo.lock
  rust-toolchain.toml
  .gitignore                 /target
  README.md                  what it is, how to test, the parity rule
  filters/
    Cargo.toml               crate cortex-praxis-filters
    src/lib.rs               pub mod parse;
    src/parse/mod.rs         pub mod model, sse, decode, inference; is_known_mislabeled_sse; MAX_BODY_SIZE
    src/parse/model.rs       InferenceExtension and its parts (serde) + schema drift test
    src/parse/sse.rs         FrameReader (port of core/listener/internal/sseframe ReadFrame), normalize_sse
    src/parse/decode.rs      Decoder (gzip, zstd, identity) with decoded-byte cap; decode_request_body
    src/parse/inference/
      mod.rs                 parse_request, parse_response, read_response, Stream, carries_sse_framing
      observer.rs            InferenceObserver: the hooks.go dispatch
      dialect.rs             Dialect, dialect_for
      anthropic.rs           port of anthropic.go
      openai.rs              port of the chat-completions half of inference.go
      responses.rs           port of responses.go
      usage.rs               TokenUsage, Kind (port of core/internal/parsercommon/tokenusage.go)
      text.rs                go_lossy_utf8, sanitize_control_runes, clean_served_model, truncate
      served_model.rs        note_body_model, settle_served_model, anthropic_beta
      subagent.rs            agent_role
    tests/parity.rs          runs every case under core/plugins/inferenceparser/testdata/*/*
core/plugins/inferenceparser/testdata/synthetic/<case>/   new Go-generated cases (Task 2)
.github/workflows/praxis.yaml
.github/dependabot.yml       + cargo, directory /praxis
CLAUDE.md, .github/CLAUDE.md one row each
```

---

### Task 1: Workspace, model types, schema drift test, CI

**Files:**
- Create: everything under `praxis/` except `src/parse/{sse,decode}.rs`, `src/parse/inference/**` and `tests/parity.rs`
- Create: `.github/workflows/praxis.yaml`
- Modify: `.github/dependabot.yml`, `CLAUDE.md` (Layout table: row `praxis/` — "Rust workspace for praxis mode: `cortex-praxis-filters`. Outside `go.work`. See #1714"), `.github/CLAUDE.md` (one line naming `praxis.yaml`)

**Interfaces:**
- Produces, in `parse::model`: `InferenceExtension`, `InferenceMessage`, `InferenceTool`, `InferenceToolCall`, `InferenceThinking`, `InferenceToolResult`, all `#[derive(Clone, Debug, Default, PartialEq, Serialize)]`, `#[serde(rename_all = "camelCase")]`. Field types: Go `int` → `i64`, `*float64` → `Option<f64>`, `*int` → `Option<i64>`, `any` (`toolChoice`) → `Option<serde_json::Value>`, `RawJSON` (`parameters`) → `Option<Box<serde_json::value::RawValue>>`, `*InferenceThinking` → `Option<InferenceThinking>`, `AgentRole` → `String`, `presentKinds` → `u8`.
- Produces: `pub const MAX_BODY_SIZE: usize = 10 << 20;` in `parse`.

- [ ] **Step 1: Write the failing drift test** in `src/parse/model.rs` `#[cfg(test)] mod tests`:

```rust
const NOT_SET_IN_PRAXIS_MODE: &[&str] = &[
    "inference.requestedModel", // only Context.SetRequestModel sets it; no plugin runs in praxis mode
    "inference.messageCount",   // only sessionapi.summarizeEvent sets it, in Go
    "inference.toolCount",      // the same
];

#[test]
fn every_go_inference_field_is_set_or_listed() {
    // ../../core/pipeline/testdata/parsed-request.fields from CARGO_MANIFEST_DIR
    // Take the lines that start with "inference." ; path = the text before the space.
    // Paths from serde_json::to_value(full_sample()), walked like parsefixture.checkFieldPaths
    // (objects: ".key", arrays: "[]", stop below a field of kind "any").
    // assert: go_paths ⊆ rust_paths ∪ NOT_SET_IN_PRAXIS_MODE, naming each missing path
    // assert: rust_paths ⊆ go_paths, naming each extra path (catches a serde rename typo)
}
```

`full_sample()` is a test helper that sets every field to a non-zero value, with one element in each `Vec`.

- [ ] **Step 2: Run it.** `cargo test -p cortex-praxis-filters every_go_inference_field` → FAIL (types not written).
- [ ] **Step 3: Write the model types** to match `core/model/extensions.go:120-494`, with the omit rules from Global Constraints.
- [ ] **Step 4: Run it.** → PASS. Also run `cargo fmt --check` and `cargo clippy --all-targets --locked -- -D warnings` → clean.
- [ ] **Step 5: Write `.github/workflows/praxis.yaml`.**
  - `on: pull_request` (branches `main`, `release-*`) and `push` to `main`, with `paths`: `praxis/**`, `.github/workflows/praxis.yaml`, `core/parse/**`, `core/internal/parsercommon/**`, `core/model/extensions.go`, `core/pipeline/testdata/**`, `core/plugins/inferenceparser/**`, `core/plugins/mcpparser/**`, `core/plugins/a2aparser/**`, `core/listener/internal/sseframe/**`.
  - One job `rust` (`ubuntu-latest`, `timeout-minutes: 20`, `permissions: contents: read`): checkout (the `ci.yaml` pin, `fetch-depth: 0`), a `changes` step in the shape of `ci.yaml`'s cpex step that sets `blocking=true` when the event is not a PR or `git diff --name-only "$BASE_SHA" HEAD` matches `^(praxis/|\.github/workflows/praxis\.yaml$)`, then `rustup show` in `praxis/` (installs from `rust-toolchain.toml`), `Swatinem/rust-cache` with `workspaces: praxis` and `cache-on-failure: false`, then fmt, clippy and test steps, each with `continue-on-error: ${{ steps.changes.outputs.blocking != 'true' }}`.
  - A last step (`if: always()`) prints `du -sm praxis/target`. When the size is over 2048 MB, it runs `rm -rf praxis/target`, so the cache post-step saves no oversized `target/`. This is the size cap that the spec asks for.
- [ ] **Step 6: Add the dependabot entry:** `package-ecosystem: cargo`, `directory: /praxis`, `schedule: interval: weekly`.
- [ ] **Step 7: Commit.** `git add praxis .github CLAUDE.md` · `git commit -s` "Feat: Add the praxis Rust workspace and the inference model types".

### Task 2: Synthetic fixtures for the OpenAI dialects (Go)

The saved corpus has Claude Code (Anthropic) cases only. The local archive has no Codex traffic. These cases give the other two dialects a parity target. Their text is invented. Go writes their `expected.json`.

**Files:**
- Create: `core/plugins/inferenceparser/testdata/synthetic/{openai-chat-nonstream-tool-calls,openai-chat-stream-tool-calls,responses-nonstream,responses-stream-function-call}/` with `request.headers`, `request.json` (compact), `response.json` or `response.sse` (LF line endings), `response.headers` where needed, and the generated `expected.json`
- Modify: `core/plugins/inferenceparser/testdata/README.md` (a section "Synthetic cases": invented text, no real traffic, one line per case, and "Go generates expected.json with -update")

**Interfaces:** Produces 4 case directories that `tests/parity.rs` (Task 4) and Go `TestFixtures` both read.

Required content, so that each case exercises its dialect's main paths:

| Case | Path | Request | Response |
|---|---|---|---|
| `openai-chat-nonstream-tool-calls` | `POST /v1/chat/completions` | system + user + assistant `tool_calls` + `role: tool` result, 2 `tools`, `temperature`, `tool_choice: "auto"` | JSON, 2 `tool_calls`, `finish_reason: tool_calls`, `usage` with `prompt_tokens_details.cached_tokens` |
| `openai-chat-stream-tool-calls` | `POST /v1/chat/completions` | as above, `stream: true` | SSE: content deltas, index-keyed `delta.tool_calls` fragments for 2 calls, a final usage chunk, `data: [DONE]` |
| `responses-nonstream` | `POST /v1/responses` | `input` with a `message`, a `function_call` and a `function_call_output` item, 1 `tools`, `reasoning.effort` | JSON `response` with output `message` + `function_call`, `usage` with `input_tokens_details.cached_tokens` and `output_tokens_details.reasoning_tokens` |
| `responses-stream-function-call` | `POST /backend-api/codex/responses`, Host `chatgpt.com` | Codex shape: `instructions`, `input`, `tools`, `stream: true` | SSE with `event:` lines: `response.output_text.delta`, `response.output_item.added` (function_call), `response.function_call_arguments.delta`, `response.completed` with usage |

- [ ] **Step 1: Write the case files** (no `expected.json` yet). Take the body shapes from the existing Go tests in `core/plugins/inferenceparser/responses_test.go` and `plugin_test.go`.
- [ ] **Step 2: Run** `GOWORK=off go -C core test ./plugins/inferenceparser -run 'TestFixtures|TestFixtureCasesAreComplete'` → FAIL ("run with -update to write it").
- [ ] **Step 3: Generate** with `GOWORK=off go -C core test ./plugins/inferenceparser -run TestFixtures -update`. Open each `expected.json` and check by eye that it is non-trivial: the model, messages, tools, tool calls, tokens and finish reason are set. A case with an empty completion and no tool calls is wrong. Fix its input.
- [ ] **Step 4: Run** the Step 2 command again → PASS. Then `GOWORK=off go -C core test ./pipeline -run TestParsedRequestFields` → PASS.
- [ ] **Step 5: Commit** "Test: Add synthetic Chat Completions and Responses parser cases".

### Task 3: SSE framing, decoding and the mislabeled-SSE rule

**Files:**
- Create: `praxis/filters/src/parse/sse.rs`, `praxis/filters/src/parse/decode.rs`
- Modify: `praxis/filters/src/parse/mod.rs`

**Interfaces:**
- Produces in `parse::sse`:
  - `pub struct FrameReader` — `new(max_frame: usize) -> Self`, `push(&mut self, chunk: &[u8]) -> Result<Vec<Vec<u8>>, FrameTooLarge>` (the data payloads of the events that this chunk completed), `finish(&mut self) -> Result<Vec<Vec<u8>>, FrameTooLarge>` (an event left open at EOF, as `sseframe.Reader.ReadFrame` gives it at EOF).
  - `pub struct FrameTooLarge;` with `Display` = the frame error text from Global Constraints.
  - `pub fn normalize_sse(body: &[u8]) -> Cow<'_, [u8]>` — port of `parsercommon.NormalizeSSE`.
- Produces in `parse::decode`:
  - `pub enum Coding { Identity, Gzip, Zstd }`, `pub fn coding_of(content_encoding: Option<&str>) -> Option<Coding>` (case-insensitive. `None` for any other coding: the caller then does not parse).
  - `pub struct Decoder` — `new(coding: Coding, cap: usize) -> Self`, `push(&mut self, chunk: &[u8]) -> Result<Vec<u8>, CapExceeded>` (decoded bytes), `finish(&mut self) -> Result<Vec<u8>, CapExceeded>`. The cap counts decoded output for this `Decoder`'s whole life. It is for the buffered path. For SSE, the caller uses `cap = usize::MAX`, and `FrameReader` enforces the per-frame cap.
  - `pub struct CapExceeded;` with `Display` = the buffered error text.
  - `pub fn decode_request_body(headers: &http::HeaderMap, body: &[u8]) -> Cow<'_, [u8]>` — port of `parsercommon.DecodeRequestBody`: zstd only, and the body as it is on any error.
- Produces in `parse`: `pub fn is_known_mislabeled_sse(status: u16, host: &str, path: &str) -> bool`.

- [ ] **Step 1: Write the failing tests.**
  - `frame_reader_*`: port each case of `core/listener/internal/sseframe/reader_test.go` and `bom_test.go` that uses `ReadFrame`, with the same inputs and expected frames. Run each input through `push` in one chunk and again in 1-byte chunks. Both must give the same frames.
  - `frame_reader_cap`: one event with 10485761 data bytes → `Err(FrameTooLarge)`. An event of exactly 10485760 bytes → `Ok`.
  - `decode_round_trip`: gzip and zstd of a 3 MiB text, pushed in 4096-byte chunks → the original bytes.
  - `decode_stops_at_cap`: gzip of 11 MiB of zeros (about 11 KB coded), `cap = MAX_BODY_SIZE` → `Err(CapExceeded)`. Assert that no single `push` returns more than `MAX_BODY_SIZE` bytes in total.
  - `mislabeled`: `(200, "chatgpt.com", "/backend-api/codex/responses")` → true. Status 299 → true. Status 405 → false. Host `api.openai.com` → false. Path `/v1/responses` → false.
- [ ] **Step 2: Run** `cargo test -p cortex-praxis-filters parse::` → FAIL.
- [ ] **Step 3: Implement.** `FrameReader` follows `sseframe/reader.go` (`readLine`, `splitField`, the BOM rule, data lines joined with `\n`, events without data skipped). `Decoder` uses `flate2::write::GzDecoder` and `zstd::stream::write::Decoder` over a `Vec<u8>` sink that refuses to grow past the cap.
- [ ] **Step 4: Run** → PASS, plus fmt and clippy.
- [ ] **Step 5: Commit** "Feat: Port SSE framing and add capped decoding to the praxis parser".

### Task 4: The Anthropic dialect, the dispatch, and the parity harness

**Files:**
- Create: `praxis/filters/src/parse/inference/{mod,observer,dialect,anthropic,usage,text,served_model,subagent}.rs`, `praxis/filters/tests/parity.rs`

**Interfaces:**
- Consumes: Task 1 model types. Task 3 `FrameReader`, `Decoder`, `decode_request_body`, `normalize_sse`.
- Produces in `parse::inference` (Rust names for the Go API in `core/parse/inference/inference.go`):
  - `pub fn parse_request(path: &str, headers: &http::HeaderMap, body: &[u8]) -> Option<InferenceExtension>` (path query-free)
  - `pub fn parse_response(path: &str, body: &[u8]) -> Option<InferenceExtension>`
  - `pub fn read_response(path: &str, sse: bool, body: &[u8], ext: &mut InferenceExtension)`
  - `pub struct Stream` — `new(path: &str)`, `fold(&mut self, frame: &[u8], ext: &mut InferenceExtension)`, `finish(&mut self, ext: &mut InferenceExtension)`
  - `pub fn carries_sse_framing(frame: &[u8]) -> bool`
  - `pub fn settle_served_model(ext: &mut InferenceExtension, resp_headers: &http::HeaderMap)`
  - `pub struct InferenceObserver` — `on_request(path: &str, headers: &http::HeaderMap, body: &[u8]) -> Self` (strips the query, as `endpointPath`), `on_response_frame(&mut self, resp_headers: &http::HeaderMap, frame: &[u8], last: bool)`, `extension(&self) -> Option<&InferenceExtension>`. It ports `OnRequest` and `OnResponseFrame` of `core/plugins/inferenceparser/hooks.go`: not last → fold. Last → reuse the stream, or start one when the frame is not empty, the response Content-Type is `text/event-stream` (`settle.IsEventStream`, `core/cost/settle`) and the frame has no SSE framing. With a stream: fold and finish. Else, an empty frame does nothing. Else `read_response(sse = event-stream || carries_sse_framing)`. On every last frame, `settle_served_model` runs (`cost.go:73`). `IsAction = true` when `parse_request` claims the request.
  - In `text.rs`: `pub(crate) fn go_lossy_utf8(body: &[u8]) -> LossyBody`, where `LossyBody` holds the text with each byte that does not start a valid UTF-8 sequence replaced by U+FFFD (one per byte, as Go's `utf8.DecodeRune`), and `fn raw_len(&self, sub: &str) -> usize`, which maps a substring of the text back to its length in the original bytes. The dialect parsers parse `LossyBody.text` and compute `contentBytes` with `raw_len` on the borrowed `RawValue` of the content. When the body is valid UTF-8, `LossyBody` borrows it and `raw_len` is `sub.len()`.

- [ ] **Step 1: Write the parity harness** `tests/parity.rs`:
  - `cases()` lists `../../core/plugins/inferenceparser/testdata/*/*` dirs that have `request.headers` (read with `std::fs::read_dir`). It parses `request.headers` (the request line, then `Name: value` lines) and the optional `response.headers` into `http::HeaderMap`. It picks the response file and its Content-Type as `parsefixture.responseTypes` does.
  - `run(case, variant) -> serde_json::Value` drives `InferenceObserver` and returns `serde_json::to_value(extension)`, or `Value::Null` when nothing is claimed. Variants for an SSE response: `buffered` (the whole body, one frame, `last = true`), `chunks_4096` and `chunks_1` (`FrameReader` fed in chunks of that size, each frame `last = false`, then `finish`, then one empty frame with `last = true`), `crlf` (LF → CRLF, then `chunks_4096`), `gzip` (gzip the body, `Decoder` in 4096-byte chunks, then `chunks_4096`). For a non-SSE response: `buffered` and `gzip` (decode the whole body, then `buffered`).
  - `same(expected, got, path)` compares two `Value`s and treats numbers as equal when their `as_f64()` values are equal (Go writes `1`, Rust writes `1.0`). On a difference it panics with the JSON path and both values.
  - `#[test] fn claude_code()` runs every case under `claude-code/` in every variant against its `expected.json`.
- [ ] **Step 2: Write the unit test `invalid_utf8_matches_go`** in `anthropic.rs`: `parse_request("/v1/messages", &HeaderMap::new(), b"{\"model\":\"m\",\"max_tokens\":5,\"messages\":[{\"role\":\"user\",\"content\":\"a\xff\xfeb\"}]}")` gives `model "m"`, `max_tokens 5`, one message with `role "user"`, `content "a\u{FFFD}\u{FFFD}b"`, `content_bytes 6`. (Go's result, measured on 2026-11-09.)
- [ ] **Step 3: Run** `cargo test -p cortex-praxis-filters` → FAIL.
- [ ] **Step 4: Implement**, function by function, from `dialect.go`, `anthropic.go`, the shared parts of `inference.go` (`ParseRequest`, `ParseResponse`, `ReadResponse`, `Stream`, `CarriesSSEFraming`, `foldResponseFrame`, the stream state and `finalize`), `subagent.go`, `served_model.go`, `events.go` and `core/internal/parsercommon/{tokenusage,sse,parsercommon}.go`, and `SanitizeControlRunes` from `core/model`. Keep each Rust function's name the snake_case form of the Go name, so a reviewer can put them side by side. The OpenAI and Responses arms of `dialect_for` and `read_response` call `openai::` and `responses::` functions that return `None` and do nothing until Tasks 5 and 6.
- [ ] **Step 5: Run** → PASS for `claude_code` in every variant and for `invalid_utf8_matches_go`. Then fmt and clippy.
- [ ] **Step 6: Commit** "Feat: Port the Anthropic inference parser to Rust, with a parity test".

### Task 5: The OpenAI Chat Completions dialect

**Files:**
- Create or modify: `praxis/filters/src/parse/inference/openai.rs`, `praxis/filters/tests/parity.rs`

**Interfaces:** Consumes Task 4's dispatch. Produces the `openai::` functions for `parseOpenAIRequest`, `parseInferenceJSON`, `parseInferenceSSE` and the OpenAI arm of `foldResponseFrame` in `inference.go`, including the legacy `/completions` shapes that Go reads.

- [ ] **Step 1: Add** `#[test] fn synthetic_openai_chat()` to `parity.rs`: the `synthetic/openai-chat-*` cases, every variant.
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement** from `inference.go`.
- [ ] **Step 4: Run** → PASS. `claude_code` still passes. fmt, clippy.
- [ ] **Step 5: Commit** "Feat: Port the Chat Completions inference parser to Rust".

### Task 6: The Responses dialect, and a check that every case is run

**Files:**
- Create or modify: `praxis/filters/src/parse/inference/responses.rs`, `praxis/filters/tests/parity.rs`

**Interfaces:** Produces the `responses::` functions for `parseResponsesRequest`, `parseResponsesJSON`, `parseResponsesSSE` and the Responses arm of the fold, from `responses.go`.

- [ ] **Step 1: Add** `#[test] fn synthetic_responses()` (the `synthetic/responses-*` cases, every variant) and `#[test] fn every_case_dir_is_run()`, which fails when a directory under `testdata/*/*` is not matched by `claude_code`, `synthetic_openai_chat` or `synthetic_responses`. A new agent directory then needs a new test.
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement** from `responses.go`.
- [ ] **Step 4: Run** the whole suite → PASS. fmt, clippy.
- [ ] **Step 5: Commit** "Feat: Port the Responses inference parser to Rust".

### Task 7: Verify and open the PR

- [ ] **Step 1:** From `praxis/`: `cargo fmt --check && cargo clippy --all-targets --locked -- -D warnings && cargo test --locked` → all pass.
- [ ] **Step 2:** `GOWORK=off go -C core vet ./... && GOWORK=off go -C core test ./plugins/inferenceparser ./pipeline ./parse/...` → pass. `git diff --stat refs/base/praxis-1718 -- '*.go' 'go.mod' 'go.sum' go.work` → empty.
- [ ] **Step 3:** `pre-commit run --all-files` → pass. `cargo build` leaves `praxis/target/` untracked (`git status` clean apart from it).
- [ ] **Step 4:** Write `praxis/README.md`: what the workspace is (#1714, phase A), how to run the tests, the parity rule ("Go is the source of truth; a Go parser change regenerates `expected.json`, and this suite then shows the Rust gap"), and that a Go-only parser PR gets a report, not a block.
- [ ] **Step 5:** Push the branch, then open the PR `Feat: Add the praxis Rust workspace and port the inference parser`, with `Fixes #1718`. The body states: the deviations (no Praxis dependency yet; the drift test covers the `inference` subtree, and #1719 extends it to the whole `SessionEvent`; 4 synthetic cases added because no Codex traffic was available), the Review Focus list, the measured CI time and `target/` size, and the `Assisted-By` line.
