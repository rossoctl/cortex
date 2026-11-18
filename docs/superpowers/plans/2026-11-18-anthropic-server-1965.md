# Anthropic server side of the translation hub (#1965) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The router can send a translated request (Codex's Responses request) to a server that serves only Anthropic Messages, natively, with thinking and prompt caching. The existing Anthropic request decoder decodes in one pass.

**Architecture:** `anthropic.Protocol` gets `pivot.RequestEncoder` (new file `anthropic/encode.go`) and `pivot.ReplyDecoding` (new file `anthropic/decode_reply.go`). Because `translate.Target` checks for a `RequestEncoder`, the registry needs no new entry. `anthropic/request.go` is rewritten to decode in one pass, in a separate PR.

**Tech Stack:** Go 1.26.10, standard library only.

**Spec:** GitHub issue rossoctl/cortex#1965 (`gh issue view 1965 -R rossoctl/cortex`). The issue is the spec. Rule numbers S1-S17 in this plan are the issue's rule numbers. Parent rules H1-H6 and P1 are in #1963. #1911 rules are in `docs/superpowers/plans/2026-11-17-translation-hub-1911.md`. #1964 rules (R2, R2b, R9, R14) are in `docs/superpowers/plans/2026-11-18-responses-decode-1964.md` and issue #1964. **Every implementer reads the issue rules that their task names before they start.**

## Global Constraints

- Standard library only in `core/translate/...` non-test code. `GOWORK=off go mod tidy -diff` in `core` prints nothing.
- Protocol names (exact): `anthropic-messages`, `chat-completions`, `responses`.
- Deterministic encoding (#1911 rule 9a): structs with a fixed field order. Write JSON with `marshal` from `core/translate/anthropic/error.go` (no HTML escapes).
- Never copy an unknown field to the target (#1911 rule 11). Each field that the encoder does not send is named in `dropped`, sorted, no duplicates.
- Golden expected-output files are written by the test's `-update` flag. Input fixtures are written by hand. Do not commit a captured agent request.
- Do not change the bytes of any existing file under `core/translate/testdata/request/`, `core/translate/testdata/reply/` or `core/translate/responses/testdata/`.
- Run Go commands per module with `GOWORK=off`. Commit with `git commit -s`. Write comments and commit messages in ASD-STE100 Simplified Technical English. End each commit message with `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>` (repo CLAUDE.md policy, no `Co-Authored-By`).

## Decisions made for this plan (with reasons)

1. **Three PRs, not one.** The issue asks for one PR under 2,000 counted lines. The estimate is about 3,200 counted lines: the S16 rewrite counts its removed lines too, and tests and input fixtures count. Per task: T1 800, T2 800, T3 950, T4 500, T5 80, T6 50. (For scale: the #1964 Responses decoder and the Responses reply encoder were each about 1,000 counted lines with their tests.) The workflow limit is 2,000 per PR, and T2-T6 (about 2,400) do not fit in one PR. The S16 decoder fix shares no code with the server side, so it goes in its own PR, P1, with no parent. P2 has T3 (the reply decoder) and no parent. P3 has T2, T4, T5 and T6, with parent P2. Reason for this order: `translate.Target` (`core/translate/registry.go`) picks a protocol when it has a `RequestEncoder`, and does not check for a reply decoder. So the encoder (T2) must not merge before the reply decoder (T3): with the encoder alone, the router sends Codex to an Anthropic server and cannot read the reply. The reply decoder alone changes no route. T5 goes in P3 because its S4 test needs the encoder. P1 and P2 run and are reviewed at the same time.
2. **P1 must not collide with P2 and P3 in `core/translate/anthropic/`.** P1 changes only `anthropic/request.go`, `anthropic/request_test.go` and adds `core/translate/bench_test.go`. P2 and P3 must not call any helper that `anthropic/request.go` defines today (`object`, `take`, `takeRaw`, `extra`, `isNull`, `orNull`, `takeEffort`, `takeSource`, `decodeContent`, ...), because P1 can remove or rename them. P2 and P3 can use `marshal` (`error.go`), `errorBody`, `errorDetail` (`error.go`), and `Name`, `extraPrefix` (`protocol.go`). P1 must not change `error.go`, `protocol.go`, `reply.go` or `reply_test.go`.
3. **S16 budget applies to `anthropic.Protocol{}.DecodeRequest`**, measured by `BenchmarkAnthropicDecodeRequest` in `core/translate/bench_test.go`. The same file also has `BenchmarkRequestAnthropicToChat` (the whole `translate.Request(Anthropic, Chat, …)`), which reports a number but has no budget. Reason: S16 is the decoder fix, and part 1 put its budget on its decoder in the same way (#1964 plan decision 13). The 1963 table shows about 22 ms of the 65 ms outside the decoder, so the full conversion can stay over 15 ms after the fix. The P1 PR body gives both `ns/op` values. If the full conversion is over 15 ms, the PR body says so. A benchmark, not a timed test: a timed test fails at random on a busy CI host.
4. **S16 technique (recommended, not required):** walk the JSON with one `json.Decoder` and its `Token` method, as `members` in `core/translate/responses/request.go` does. Decode each known member straight into its Go field with `dec.Decode`. Decode each unknown member into a `json.RawMessage` for `Extra`. Decode `input`, `input_schema` and `cache_control` into `json.RawMessage` so that their bytes do not change. Reason: a nested `json.RawMessage` layer scans the same bytes again at each level, and that is the measured cost. Any technique is good if the tests of T1 pass and the benchmark meets the budget.
5. **Usage comes before stop in the pivot events.** The issue says `message_delta` gives `stop` and `message_stop` gives `usage`. But every reply encoder (`anthropic/reply.go`, `responses/reply.go`) ends the reply at `EventStop` and reads the usage that came before it. So the decoder keeps the stop reason from `message_delta`, and at `message_stop` it emits `EventUsage`, then `EventStop`. The Chat decoder uses the same order (`chat/reply.go` `finish`).
6. **Block indexes are renumbered.** A skipped block (server-tool block) uses no pivot index. Each kept block gets the next pivot index, from 0. Reason: the Anthropic encoder sends `ev.Index` to the agent, and a gap there is not valid.
7. **Reply-side skips are not reported by the decoder.** S13 says that a skipped block is "recorded". `pivot.ReplyDecoder` has no channel for that, and a new channel changes the hub contract for one case. The raw capture (#1911 rule 8a) keeps the server's reply, with the skipped block. The P2 PR body says so.
8. **A stream that ends with no `message_stop`** gives `EventError{Status: 502, ErrorType: "api_error", Message: "the inference server's stream ended before the reply was complete"}`. Reason: a broken stream must not look complete (#1911 decision 5). This is the same text as `msgEndedEarly` in `chat/reply.go`.
9. **The status of a stream `error` event** comes from its type: `invalid_request_error` 400, `authentication_error` 401, `permission_error` 403, `not_found_error` 404, `request_too_large` 413, `rate_limit_error` 429, `overloaded_error` 529, any other type 500. Reason: these are the statuses that Anthropic gives with each type.
10. **Buffer limits** are the Chat decoder's: at most 1 MiB of a non-2xx body, at most 10 MiB of a not-streamed 2xx body or of one stream line. Over the limit: `EventError{Status: 502, ErrorType: "api_error", Message: "the inference server's reply is over the 10485760-byte buffer limit"}`.
11. **SSE parsing:** read only `data:` lines, and use the JSON `type` member. Ignore `event:` lines, comment lines and blank lines. Accept `\r\n` line ends.
12. **A start block with text:** when `content_block_start` carries a non-empty `text` (text block) or `thinking` (thinking block), the decoder emits it as the first delta of that block. The `input` of a streamed `tool_use` start (always `{}`) is ignored.
13. **`message_delta.usage`:** each member that is present replaces the kept value. Missing members keep the `message_start` value.
14. **`cache_control` on blocks.** The encoder keeps `cache_control` on text, image, document, tool_use and tool_result blocks and on tools. It drops it on thinking and redacted_thinking blocks and names `cache_control` in `dropped`. Reason: Anthropic accepts the marker on content blocks and refuses it on thinking blocks.
15. **`tool_result`:** `content` is left out when it has no blocks. `is_error` is sent only when it is true. Content blocks of a kind other than text and image are dropped and named by their kind. Reason: these are Anthropic's defaults, and a shorter body is still deterministic.
16. **`tool_use` input** that is empty is sent as `{}`. **A function tool** with no `input_schema` gets `{"type":"object"}`. Reason: Anthropic needs both fields.
17. **A message whose content is empty after the drops** (for example an assistant message with only an unsigned thinking block) is left out. Reason: Anthropic refuses an empty content list. Consecutive messages with the same role are allowed, so the next messages stay valid.
18. **An empty `messages` list** goes as `"messages":[]`, and the server's answer goes to the agent. Rule S6 refuses only a first message that is not `user`. The refusal text is: `an Anthropic Messages server needs a user message first, but the first message of this request is an assistant message`.
19. **An effort outside the S11 table** (for example `auto`) gives no `output_config`, and `effort` is named in `dropped`.
20. **A server tool** (S8) is named in `dropped` by its `Type`, for example `web_search_20250305`.
21. **`Extra` keys of every protocol** are named in `dropped`, also the `anthropic.` keys (the part after the first `.`). Reason: S3 sends only the fixed field list, so nothing in `Extra` is sent.
22. **S15 Anthropic 404 match:** `refusesModel` returns true when the status is 404, `error.type` is `not_found_error`, and `errorMessage(body)` is exactly `"model: " + model`. Reason: today's `modelNotFound` phrases need quotes around the name, and Anthropic's message has none. An exact match cannot match a longer name: `model: claude-opus-5-5` is not a refusal of `claude`.
23. **S4 and the router's output cap.** `capOutput` (`core/plugins/inferencerouter/fit.go:207`) leaves an absent output field alone, and `resendForLimit` (`models.go:184`) records `limit_not_fitted` and does not resend when `applyLimits` changes nothing. Codex usually sends no `max_output_tokens`. So for a translated Codex request, the encoder fills 64,000, and when the server refuses it, the refusal goes to Codex (inferred from the code, checked by the T5 test). The issue asks only for a report: the P3 PR body says this. The plan does not change `fit.go`.
24. **Test layout (H6):** the Responses → Anthropic pair has its own directory, `core/translate/testdata/responses-anthropic/` with `request/` and `reply/`. Its golden tests live in package `translate` (the protocols are registered, so `translate.Request` and `translate.Reply` work). The S17 round-trip test also lives in package `translate` and reads `testdata/request/*.json`.

## Review Focus

1. A stream that ends with no `message_stop` (decision 8). Test in T3: `TestDecodeReply_StreamEndsEarly`.
2. An assistant message whose only block is an unsigned thinking block (decision 17). Test in T2: `TestEncodeRequest_EmptyMessageLeftOut`.
3. A server-tool block between two text blocks in a stream: the next block gets index 1, not 2 (decision 6). Test in T3: `TestDecodeReply_SkippedBlockUsesNoIndex`.
4. A non-2xx body that is HTML, longer than 200 bytes and with a multi-byte character at byte 200: the message is cut at a rune start. Test in T3: `TestDecodeReply_NonJSONErrorCut`.
5. A model name that is a prefix of the refused name in Anthropic's 404 (decision 22). Test in T5: a negative case in `TestRefusesModel_OtherAnswersAreNotRefusals`.

## File structure

| File | Task | Responsibility |
|---|---|---|
| `core/translate/anthropic/request.go` | T1 | `DecodeRequest` in one pass (S16) |
| `core/translate/anthropic/request_test.go` | T1 | more decoder cases |
| `core/translate/bench_test.go` | T1 | `BenchmarkAnthropicDecodeRequest`, `BenchmarkRequestAnthropicToChat` |
| `core/translate/anthropic/protocol.go` | T3, T2 | interface assertions, package doc |
| `core/translate/anthropic/encode.go` | T2 | `EncodeRequest` (S2-S12) |
| `core/translate/anthropic/encode_test.go` | T2 | encoder unit tests |
| `core/translate/anthropic/decode_reply.go` | T3 | `NewReplyDecoder` (S13-S15) |
| `core/translate/anthropic/decode_reply_test.go` | T3 | reply decoder unit tests |
| `core/translate/testdata/responses-anthropic/reply/*.{sse,json}` | T3 | reply input fixtures |
| `core/translate/testdata/responses-anthropic/request/*.json` | T4 | request input fixtures |
| `core/translate/responses_anthropic_test.go` | T4 | goldens, rule 9a, S12 |
| `core/translate/roundtrip_test.go` | T4 | S17 |
| `core/translate/golden_test.go` | T4 | `replyCases` takes a directory |
| `core/translate/registry_test.go` | T2 | `TestTarget` |
| `core/listener/forwardproxy/inferencerouter_test.go` | T4 | the listener test |
| `core/plugins/inferencerouter/models.go`, `models_test.go`, `api_test.go` | T5 | Anthropic 404, S4 check |
| `docs/translation-server.md`, `docs/inference-router-plugin.md`, `docs/agents/codex.md` | T6 | docs |

## PR list

| PR | Title | Parent | Tasks | estLines |
|---|---|---|---|---|
| P1 | `perf: Decode Anthropic Messages requests in one pass` | "" | T1 | 800 |
| P2 | `feat: Decode Anthropic Messages replies to pivot events` | "" | T3 | 950 |
| P3 | `feat: Encode Anthropic Messages requests, so Codex can use the Anthropic API` | P2 | T2, T4, T5, T6 | 1430 |

## Gate

From the worktree root:

```sh
(cd core && GOWORK=off go vet ./... && test -z "$(gofmt -l .)" && GOWORK=off go test ./translate/... ./parse/... ./plugins/inferencerouter/... ./listener/... ./pipeline/... && GOWORK=off go test -run '^$' -bench . -benchtime 1x ./translate/... && GOWORK=off go mod tidy -diff) && (cd cmd/cortex && GOWORK=off go vet ./...) && (cd cmd/cortex-envoy && GOWORK=off go vet ./...) && pre-commit run --from-ref 30e147bfcc491fcf98fdfdb6122221af093b9ce5 --to-ref HEAD
```

---

## PR P1

### Task T1: Decode Anthropic requests in one pass (S16)

Depends on: none.

**Files:**
- Modify: `core/translate/anthropic/request.go` (rewrite the body of `decodeRequest` and its helpers)
- Modify: `core/translate/anthropic/request_test.go`
- Create: `core/translate/bench_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces: no new API. `func (Protocol) DecodeRequest(body []byte) (pivot.Request, error)` keeps its signature, its error type (`*pivot.RequestError`, 400, `invalid_request_error`, message prefix `the request body is not a valid Anthropic Messages request: `) and its exact pivot output.

- [ ] **Step 1: Write the tests.**
  - `bench_test.go` (package `translate`): `func claudeCodeBody() []byte` builds a Claude-Code-shaped request of at least 2 MB in code: a `system` list of 2 text blocks (the last with `cache_control`), 40 tools with `input_schema`, `"thinking":{"type":"adaptive"}`, `"output_config":{"effort":"high"}`, `"metadata":{"user_id":"u"}`, `"stream":true`, and 400 turns. Each turn: a user message with a text block, an assistant message with a `thinking` block (text and signature), a text block and a `tool_use` block, then a user message with a `tool_result` whose `content` is a list with one text block of about 4 KB with quotes and backslashes. `BenchmarkAnthropicDecodeRequest` fails with `b.Fatalf` when the body is under `2<<20` bytes, calls `b.SetBytes`, and runs `anthropic.Protocol{}.DecodeRequest` in `b.Loop()`. `BenchmarkRequestAnthropicToChat` runs `Request(Anthropic, Chat, body, nil)` on the same body.
  - `request_test.go`: add `TestDecodeRequest_KeepsRawBytes`: `input`, `input_schema`, `cache_control` and an unknown member with spaces and unsorted keys (`{ "b":1, "a":[1, 2] }`) come out byte for byte. Add `TestDecodeRequest_Errors`: each case of `TestDecodeRequest_BadJSON` plus `{"messages":[{"role":"user","content":[{"type":"text","text":5}]}]}`, `{"messages":{}}`, `{"model":"m"} x` (data after the object) and `{"tools":[5]}` gives a `*pivot.RequestError` with status 400. `TestDecodeRequest_Fields` does not change and must still pass.
- [ ] **Step 2: Run** `cd core && GOWORK=off go test ./translate/... && GOWORK=off go test -run '^$' -bench 'AnthropicDecodeRequest|RequestAnthropicToChat' ./translate/`. Expected: the tests PASS on the old decoder. Write down both `ns/op` values (the "before" numbers).
- [ ] **Step 3: Implement** the one-pass decoder (decision 4). Keep the `Extra` rules: unknown members go to the `Extra` of their request, message or block under `anthropic.<field>`. The leftover members of `output_config` and of an image or document `source` go to `anthropic.output_config` and `anthropic.source` as one JSON object, as today. A tool with `"type":"custom"` gets `Type` `""`. Content that is a string gives one text block. `null` members are the same as missing members.
- [ ] **Step 4: Run** `cd core && GOWORK=off go test ./translate/... ./listener/forwardproxy/ ./plugins/inferencerouter/... && GOWORK=off go test -run '^$' -bench 'AnthropicDecodeRequest|RequestAnthropicToChat' ./translate/`. Expected: PASS. `git status` shows no change to any golden file. `BenchmarkAnthropicDecodeRequest` `ns/op` is at most 15,000,000.
- [ ] **Step 5: Commit** `perf: Decode Anthropic Messages requests in one pass`, with the before and after `ns/op` of both benchmarks in the body. The P1 PR body gives the same numbers (decision 3).

---

## PR P2

### Task T3: The Anthropic reply decoder (S13-S15)

Depends on: none.

**Files:**
- Create: `core/translate/anthropic/decode_reply.go`, `core/translate/anthropic/decode_reply_test.go`
- Modify: `core/translate/anthropic/protocol.go` (add `_ pivot.ReplyDecoding = Protocol{}`, and say in the package doc that it also decodes replies)
- Create the reply input fixtures in `core/translate/testdata/responses-anthropic/reply/`: `text.sse`, `text.json`, `thinking_signature.sse`, `thinking_signature.json` (thinking with `""` text and a signature, then text), `redacted_thinking.sse`, `redacted_thinking.json`, `two_tools.sse`, `two_tools.json`, `server_tool.sse`, `server_tool.json` (a `server_tool_use` block and a `web_search_tool_result` block between two text blocks), `max_tokens.sse`, `max_tokens.json`, `refusal.sse`, `refusal.json`, `stream_error.sse` (a `message_start`, one text delta, then an `error` event with `overloaded_error`), `error_429_rate_limit.json`, `error_529_overloaded.json`. Each `.sse` uses Anthropic's real frame shape: `event: <type>\ndata: <json>\n\n`, with one `ping` event after `message_start`. `message_start` usage has `input_tokens`, `cache_creation_input_tokens`, `cache_read_input_tokens` and `output_tokens`. `message_delta` usage has `output_tokens`.

**Interfaces:**
- Consumes: `pivot.Event`, `pivot.ReplyDecoder`, `pivot.ErrorType` (`core/translate/pivot`).
- Produces: `func (Protocol) NewReplyDecoder(status int, stream bool) pivot.ReplyDecoder`. The event order is: `EventStart`, then for each kept block `EventBlockStart`, its `EventDelta`s, `EventBlockStop`, then `EventUsage`, then `EventStop` (decision 5). An error gives one `EventError` and nothing after it.

- [ ] **Step 1: Write the failing tests** in `decode_reply_test.go` (package `anthropic`). Add `decodeAll` and `decodeBytewise` helpers in the style of `chat/reply_test.go`.
  - `TestDecodeReply_Stream`: `two_tools.sse` gives exactly: start (`id`, `model`), text block 0 with its text delta, tool_use block 1 (`ToolID`, `ToolName`) with its `DeltaInputJSON` pieces, tool_use block 2, usage (`Input`, `CacheRead`, `CacheCreation` from `message_start`, `Output` from `message_delta`), stop `tool_use`.
  - `TestDecodeReply_ThinkingSignature`: a thinking block with no thinking delta and one `DeltaSignature` is passed on (S13 last bullet).
  - `TestDecodeReply_Redacted`: the `redacted_thinking` start gives `EventBlockStart{Block: BlockRedactedThinking, Data: <data>}`.
  - `TestDecodeReply_SkippedBlockUsesNoIndex` (Review Focus 3): `server_tool.sse` gives text blocks with pivot index 0 and 1, and no event for the server-tool blocks or their deltas.
  - `TestDecodeReply_Whole`: each `.json` fixture gives the same events as its `.sse` twin, except the split of the deltas: one delta for each block (thinking: one `DeltaThinking` when the text is not empty, then one `DeltaSignature`; tool_use: one `DeltaInputJSON` with the raw `input` bytes).
  - `TestDecodeReply_EveryByteBoundary`: for each `.sse` fixture and each `i` from 0 to `len(body)`, `Feed(body[:i])`, `Feed(body[i:])`, `End()` gives the same events as one `Feed`. `decodeBytewise` also gives the same events.
  - `TestDecodeReply_StreamError`: `stream_error.sse` gives start, text block start, delta, then `EventError{Status: 529, ErrorType: "overloaded_error", Message: <message>}` (decision 9), and nothing after it.
  - `TestDecodeReply_HTTPError`: status 429 with `error_429_rate_limit.json` → `EventError{Status: 429, ErrorType: "rate_limit_error", Message: <message>}`. Status 529 → `overloaded_error`. Status 404 with `{"type":"error","error":{"type":"not_found_error","message":"model: claude-nope"}}` → `not_found_error` and `model: claude-nope`.
  - `TestDecodeReply_NonJSONErrorCut` (Review Focus 4): status 502 with an HTML body of 300 bytes with a 3-byte character at bytes 199-201 → `ErrorType` `api_error`, message is the first 199 bytes. An empty body → `http.StatusText(502)`.
  - `TestDecodeReply_StreamEndsEarly` (Review Focus 1): `two_tools.sse` cut before `message_stop` → the last event is the error of decision 8.
  - `TestDecodeReply_OverLimit`: a not-streamed 2xx body over 10 MiB → the error of decision 10.
  - `TestDecodeReply_StopReasons`: `max_tokens.sse` → stop `max_tokens`. `refusal.sse` → stop `refusal`.
- [ ] **Step 2: Run** `cd core && GOWORK=off go test ./translate/anthropic/ -run DecodeReply`. Expected: FAIL (`NewReplyDecoder` is not defined).
- [ ] **Step 3: Implement** `decode_reply.go`. Keep only the partial line between `Feed` calls, in a new array, so that the stream read so far is not held (#1911 rule 10). Use decisions 5-13. Do not call any helper of `request.go` (decision 2).
- [ ] **Step 4: Run** `cd core && GOWORK=off go test ./translate/anthropic/`. Expected: PASS.
- [ ] **Step 5: Commit** `feat: Decode Anthropic Messages replies to pivot events`.

---

## PR P3

### Task T2: The Anthropic request encoder (S1, S2-S12)

Depends on: none.

**Files:**
- Create: `core/translate/anthropic/encode.go`, `core/translate/anthropic/encode_test.go`
- Modify: `core/translate/anthropic/protocol.go` (add `_ pivot.RequestEncoder = Protocol{}`, and say in the package doc that it also encodes requests. P2 (T3) already added the reply decoder to the doc)
- Modify: `core/translate/registry_test.go` (`TestTarget`: `{"anthropic-messages","responses"}` → `"anthropic-messages", true`. Add `{"anthropic-messages"}` → `"anthropic-messages", true` and `{"anthropic-messages","chat-completions"}` → `"chat-completions", true`.)

**Interfaces:**
- Consumes: `pivot.Request` and its types (`core/translate/pivot/pivot.go`), `marshal` (`anthropic/error.go`).
- Produces: `func (Protocol) EncodeRequest(req pivot.Request, h http.Header) ([]byte, []string, error)`. When `h != nil`, it sets `Anthropic-Version: 2023-06-01` and deletes `Anthropic-Beta`. It does not set an auth header. A refusal is `*pivot.RequestError{Status: 400, Type: "invalid_request_error"}`. `func toolID(id string) string` (S7). `func effortOf(e string) (string, bool)` (S11, `false` for an effort outside the table).

- [ ] **Step 1: Write the failing tests** in `encode_test.go`. Build each input as a `pivot.Request` in code. Compare the exact body string and the `dropped` slice.
  - `TestEncodeRequest_TopLevelOrder`: a request with every field gives the keys in this order: `model`, `max_tokens`, `system`, `messages`, `tools`, `tool_choice`, `stop_sequences`, `stream`, `output_config`, `cache_control` (S3). `Extra{"responses.store":…, "anthropic.metadata":…}` gives `dropped` `["metadata","store"]` and neither key is in the body (decision 21).
  - `TestEncodeRequest_MaxTokens`: 0 with `Stream` true → 64000. 0 with `Stream` false → 16000. 500 → 500 (S4).
  - `TestEncodeRequest_System`: system blocks keep `cache_control`. A non-text system block is dropped by kind. A message with role `system` at index 2 becomes a `user` message with the same blocks at index 2 (S5).
  - `TestEncodeRequest_Blocks`: each S6 block shape, byte for byte. `image` base64 → `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBO"}}`. `image` url → `{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}}`. `document` in the same two shapes. `tool_use` keeps `input` raw and unchanged. `tool_result` with text and image content, `is_error` only when true, no `content` when empty (decision 15). A tool_result content block of kind `document` is dropped as `document`. Signed `thinking` → `{"type":"thinking","thinking":…,"signature":…}`. Unsigned `thinking` → dropped, `dropped` has `thinking`. `redacted_thinking` → `{"type":"redacted_thinking","data":…}`. A thinking block with `CacheControl` loses it and `dropped` has `cache_control` (decision 14).
  - `TestEncodeRequest_FirstMessageNotUser`: the first message is `assistant` → the error of decision 18, with that exact message.
  - `TestEncodeRequest_EmptyMessageLeftOut` (Review Focus 2): user, then an assistant message with only an unsigned thinking block, then user → two user messages in the body.
  - `TestToolID` (S7): `toolu_01A-b_c` → unchanged. `call_3833__thought__AY89a1+8zd/x=` → `"cx_" + base64.RawURLEncoding.EncodeToString([]byte(id))`. The same input gives the same output on each call. `TestEncodeRequest_ToolIDPairs`: a `tool_use` and its `tool_result` with that id get the same `cx_…` value.
  - `TestEncodeRequest_Tools` (S8): a function tool → `{"name":…,"description":…,"input_schema":…}` with `cache_control` kept. No `input_schema` → `{"type":"object"}` (decision 16). A tool with `Type` `web_search_20250305` → not sent, `dropped` has `web_search_20250305` (decision 20).
  - `TestEncodeRequest_ToolChoice` (S9): `auto`, `any`, `tool` (with `name`), `none`. `DisableParallel` adds `"disable_parallel_tool_use":true` to `auto`, `any` and `tool`, and not to `none`.
  - `TestEncodeRequest_Sampling` (S10): `Stop` → `stop_sequences`. `Temperature` and `TopP` → not sent, `dropped` has `temperature` and `top_p`.
  - `TestEffortOf` (S11): `none`, `minimal`, `low` → `low`. `medium` → `medium`. `high` → `high`. `xhigh` → `xhigh`. `max` → `max`. `auto` → `false`. `TestEncodeRequest_Effort`: no effort → no `output_config`. `Thinking` set → no `thinking` key, `dropped` has `thinking`. Effort `auto` → no `output_config`, `dropped` has `effort` (decision 19).
  - `TestEncodeRequest_CacheControl` (S12): no marker anywhere → the body ends with `,"cache_control":{"type":"ephemeral"}}`. One marker on a tool, or on a tool_result content block, or on a system block → no top-level `cache_control`, and the marker is sent where it was.
  - `TestEncodeRequest_Headers` (S2): `h` with `Authorization`, `Anthropic-Beta` → after: `Anthropic-Version` is `2023-06-01`, `Anthropic-Beta` is gone, `Authorization` is not changed. `h == nil` does not panic.
- [ ] **Step 2: Run** `cd core && GOWORK=off go test ./translate/anthropic/`. Expected: FAIL (`EncodeRequest` is not defined).
- [ ] **Step 3: Implement** `encode.go` with output structs in a fixed field order and `omitempty` on every optional field. Collect `dropped` in a `map[string]bool`, then sort it. Do not call any helper of `request.go` (decision 2).
- [ ] **Step 4: Run** `cd core && GOWORK=off go test ./translate/... ./plugins/inferencerouter/...`. Expected: PASS, with the `TestTarget` change above.
- [ ] **Step 5: Commit** `feat: Encode Anthropic Messages requests from the pivot form`.

### Task T4: Goldens, round trip and the listener test (S12, S17, tests section)

Depends on: T2, T3.

**Files:**
- Create the request input fixtures in `core/translate/testdata/responses-anthropic/request/` (Responses bodies, as Codex sends them):
  `s3_unknown_fields.json` (`store`, `include`, `prompt_cache_key`, `service_tier`, `zzz_unknown`), `s4_max_tokens_stream.json`, `s4_max_tokens_not_streamed.json`, `s4_max_tokens_set.json` (`max_output_tokens` 2000), `s5_developer_messages.json` (`instructions`, a leading developer message, a developer message after a tool output), `s6_blocks.json` (text, an image as a data URL, an image as an https URL, a function call and its output, a custom tool call and its output), `s6_unsigned_thinking.json` (a `cortex1:` reasoning item with text and no signature), `s6_redacted.json` (a `cortex1:` redacted reasoning item), `s6_first_assistant.json` (refusal, decision 18), `s7_tool_ids.json` (one call id with `+`, `/` and `=`, one valid id), `s8_tools.json` (a function tool, a custom tool, a namespaced tool, `web_search`), `s9_tool_choice_auto.json`, `s9_tool_choice_required.json`, `s9_tool_choice_function.json`, `s9_tool_choice_none.json`, `s9_parallel_false.json`, `s10_sampling.json` (`temperature`, `top_p`), `s11_effort_minimal.json`, `s11_effort_xhigh.json`, `s11_no_effort.json`, `s12_no_marker.json`, and `full_multiturn.json` (a system message, a mid-conversation developer message, an image, function and custom tool calls with results, a `cortex1:` reasoning payload with a signature, and a tool id with `+`, `/` and `=`). Make the `cortex1:` payloads with `encodeReasoning` in `core/translate/responses/reasoning.go`, as the existing fixtures in `core/translate/responses/testdata/request/` were made.
- Create: `core/translate/responses_anthropic_test.go`, `core/translate/roundtrip_test.go`
- Modify: `core/translate/golden_test.go` (`replyCases(t, dir string)`; the old caller passes `"testdata/reply"`)
- Modify: `core/listener/forwardproxy/inferencerouter_test.go` (`routedProxy` agents: add `"codex": "s"`, and add codex to its doc comment)
- Modify: `core/plugins/inferencerouter/api_test.go` (the comment of `TestSetAuth_TranslatedToAnthropic` says that no protocol can be translated to Anthropic. Change it: Responses can now be translated to it.)

**Interfaces:**
- Consumes: `anthropic.Protocol{}.EncodeRequest`, `NewReplyDecoder` (T2, T3), `translate.Request`, `translate.Reply`, `checkGolden`, `replyCases` (`golden_test.go`).
- Produces: tests only.

- [ ] **Step 1: Write the tests.**
  - `TestResponsesToAnthropicRequestGolden`: for each `testdata/responses-anthropic/request/*.json`, the golden `<name>.golden` is `dropped: <list>\n<body>\n` from `Request(Responses, Anthropic, body, nil)`, or one `error: <err>` line. Run with `-update`, then read each golden and check it against its rule: S4 gives 64000, 16000 and 2000. S5 gives `user` messages for the developer messages and a `system` list for `instructions`. S7 gives `cx_…` for the id with `+`, `/`, `=` on the `tool_use` and on its `tool_result`, and the valid id unchanged. S10 `dropped` has `temperature,top_p`. S11 gives `"output_config":{"effort":"low"}`, `{"effort":"xhigh"}`, and none. S12: every golden ends with the top-level `cache_control`.
  - `TestResponsesToAnthropicReplyGolden`: for each case of `replyCases(t, "testdata/responses-anthropic/reply")` and each `agentForms()` value, `Reply(Responses, Anthropic, pivot.Request{}, c.status, c.stream, agentStream, body)` against the golden that `goldenName(c, agentStream)` in `golden_test.go` gives (`<input>.to-sse.golden` or `<input>.to-json.golden`; the separate directory keeps these names apart from the #1911 goldens). Read the goldens: the thinking with empty text and a signature gives a reasoning item, the server-tool blocks are not in the output, 429 and 529 give Responses errors.
  - `TestResponsesToAnthropicDeterministic` (#1911 rule 9a, S12): encode `full_multiturn.json` two times: same bytes. Append one user message item to `input`: the output bytes up to the end of the old last message do not change, and both outputs end with the same top-level `cache_control` bytes.
  - `TestHeaders`: `Request(Responses, Anthropic, body, h)` with `OpenAI-Beta`, `Anthropic-Beta` and `X-Keep` in `h` → after: `Anthropic-Version: 2023-06-01`, `X-Keep`, and nothing else.
  - `TestAnthropicRoundTrip` (S17, H5) in `roundtrip_test.go`: for each `testdata/request/*.json`, `want := DecodeRequest(f)`, `body, dropped := EncodeRequest(want)`, `got := DecodeRequest(body)`. A refusal from `EncodeRequest` fails the test. Normalize `want` by only the allowed differences: remove what `dropped` names (Extra keys, `temperature`, `top_p`, `thinking`, unsigned thinking blocks, server tools, other dropped kinds); fill `MaxTokens` by S4; change role `system` to `user` (S5); map each tool id with `toolID` rule S7 (use the same pattern and `cx_` + base64url in the test, the function is not exported); map `Effort` by S11; set `Thinking` to nil. Remove `anthropic.cache_control` from `got.Extra`. Remove `Extra` maps that are empty after that (set them to nil). Then `reflect.DeepEqual(got, want)`. On a failure, print both as indented JSON.
  - `TestRouterAPI_CodexToAnthropicServer` in `core/listener/forwardproxy/inferencerouter_test.go`: `routedProxy(t, up, `, "api": {"anthropic-messages": "/v1/messages"}`)`. `up := routedServer(t, …)` (it also answers `/v1/models`) answers `testdata/responses-anthropic/reply/two_tools.sse` with `Content-Type: text/event-stream` (read it with a path relative to the package, `../../translate/testdata/responses-anthropic/reply/two_tools.sse`). Send `codexRequest` to `http://chatgpt.example/backend-api/codex/responses?x=1`. `postCodex` (`translate_test.go`) has a fixed URL and sets no `Authorization`, so build the request in the test with the same lines as `postCodex` (the `Content-Type`, the Codex `User-Agent`, each header of `codexHeaders`), plus `Authorization: Bearer client-key`, and send it with `proxyClient(proxy, nil).Do`. Check with `sent(up)`: one upstream request, path `/v1/messages`, `rawQuery` empty, `X-Api-Key` is exactly `routedKey`, `Anthropic-Version` is `2023-06-01`, no `Authorization` header, no header of `codexHeaders`. The upstream body has `"messages":[{"role":"user"` and the top-level `cache_control`. The reply to the agent has status 200, starts with `event: response.created\n` and has `event: response.completed\n`.
- [ ] **Step 2: Run** `cd core && GOWORK=off go test ./translate/ -update -run 'ResponsesToAnthropic'` once, read every new golden as Step 1 says, then run `cd core && GOWORK=off go test ./translate/... ./listener/forwardproxy/ ./plugins/inferencerouter/...`. Expected: PASS. `git status` shows no change to an existing golden.
- [ ] **Step 3: Commit** `test: Pin the Responses to Anthropic translation with goldens, a round trip and a listener test`.

### Task T5: Anthropic's 404 for the resend, and the S4 check (S15, S4)

Depends on: T2 (the S4 test sends a Codex request that the router translates to Anthropic, and `translate.Target` gives `anthropic-messages` only when `EncodeRequest` exists).

**Files:**
- Modify: `core/plugins/inferencerouter/models.go` (`refusesModel`, its doc comment)
- Modify: `core/plugins/inferencerouter/models_test.go`

**Interfaces:**
- Consumes: `refusesModel(status int, body []byte, model string) bool`, `errorMessage` (`limits.go`), the `resend` helper and `TestResend_TreatsALocalServersMissingModelAsARefusal` (`models_test.go:955`) as a template.
- Produces: no new API.

- [ ] **Step 1: Write the failing tests.**
  - `TestRefusesModel_ReadsEachLocalServersWording`: add `{"anthropic messages api", 404, "claude-nope", `{"type":"error","error":{"type":"not_found_error","message":"model: claude-nope"}}`}`.
  - `TestRefusesModel_OtherAnswersAreNotRefusals` (Review Focus 5): add `{"anthropic, a name the refused one starts with", 404, "claude", `{"type":"error","error":{"type":"not_found_error","message":"model: claude-opus-5-5"}}`}` and `{"anthropic's message at 400", 400, "claude-nope", <the same body with claude-nope>}`.
  - `TestResend_TreatsAnthropicsMissingModelAsARefusal`: the template test with Anthropic's 404 body. `Resend` gives true, and the resent model is the substitute.
  - `TestResend_ALimitRefusalOfATranslatedCodexRequestWithNoOutputField` (decision 23): a Codex request routed to a server with `"api": {"anthropic-messages": "/v1/messages"}`, with no `max_output_tokens`, and `Resend` with status 400 and `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: 64000 > 32000, which is the maximum allowed number of output tokens for claude-sonnet-5"}}`. Assert what the code does. Decision 23 expects `Resend` false and a `limit_not_fitted` record. If the code does something else, assert what it does, and change decision 23's text in the P3 PR body to match.
- [ ] **Step 2: Run** `cd core && GOWORK=off go test ./plugins/inferencerouter/ -run 'RefusesModel|Resend_TreatsAnthropics|Resend_ALimitRefusalOfATranslated'`. Expected: FAIL for the Anthropic cases.
- [ ] **Step 3: Implement** decision 22 in `refusesModel`: before the `modelNotFound` loop, return true when `status == http.StatusNotFound`, `gjson.GetBytes(body, "error.type").String() == "not_found_error"` and `errorMessage(body) == "model: "+model`. Add one line to the doc comment.
- [ ] **Step 4: Run** `cd core && GOWORK=off go test ./plugins/inferencerouter/...`. Expected: PASS.
- [ ] **Step 5: Commit** `fix: Resend a request when Anthropic does not have the model`.

### Task T6: Docs

Depends on: none.

**Files:**
- Modify: `docs/translation-server.md` (lines 3-7)
- Modify: `docs/inference-router-plugin.md` (item 4 of the list, lines 291-296)
- Modify: `docs/agents/codex.md` (add a section after the section with the anchor `on-a-chat-completions-server`)

Facts for the docs (from this plan and the base code):
- Cortex translates three pairs: Anthropic Messages (Claude Code) to Chat Completions, Responses (Codex) to Chat Completions, and Responses (Codex) to Anthropic Messages.
- The router picks the target in this order: `chat-completions`, then `anthropic-messages`. A server whose `api` map has both gets Chat Completions. Only a server whose map has `anthropic-messages` and no `chat-completions` gets Anthropic Messages.
- To an Anthropic server, the router sends the key as `X-Api-Key` (when `auth_header` is empty), the header `Anthropic-Version: 2023-06-01`, and no `Anthropic-Beta`.
- When Codex sends no `max_output_tokens`, `max_tokens` is 64,000 for a stream and 16,000 for a request that is not streamed.
- `temperature`, `top_p` and Codex's thinking setting are not sent. They are named in `dropped`. Codex's reasoning effort goes as `output_config.effort`: `none`, `minimal` and `low` give `low`. `medium`, `high`, `xhigh` and `max` keep their value.
- When the request has no `cache_control` marker (Codex sends none), the router adds the top-level `cache_control: {"type": "ephemeral"}`, so Anthropic caches the conversation as it grows.
- A tool call id with characters outside `a-z`, `A-Z`, `0-9`, `_` and `-` goes to Anthropic as `cx_` and the id's base64url form.
- Server tools (for example `web_search`) are not sent. Server-tool blocks in the reply do not reach Codex. The raw capture keeps them.
- Example command: `agentop server add anthropic https://api.anthropic.com --main <model> --helper <model> --api anthropic-messages=/v1/messages`, then `agentop server use anthropic --agent codex`. (The `--api` flag and `server use --agent` are shown in `docs/agents/codex.md` today.)

- [ ] **Step 1: Write** the changes. In `inference-router-plugin.md`, replace "(only `chat-completions` at this time)" with the two targets and their order. Keep the rest of item 4.
- [ ] **Step 2: Run** `pre-commit run --files docs/translation-server.md docs/inference-router-plugin.md docs/agents/codex.md`. Expected: PASS.
- [ ] **Step 3: Commit** `docs: Say that Codex can use an Anthropic Messages server`.

---

## For a human after review

These steps need a real Anthropic API key and a live Codex session. They need P2 and P3 (all of T2-T6) merged or checked out and installed. Do not run them in this workflow.

1. Configure a server with only `api: {anthropic-messages: /v1/messages}` for `https://api.anthropic.com`, and route Codex to it (`agentop server use <name> --agent codex`).
2. Run Codex for at least 3 turns with tool calls. Put the session id and the turn count in the P3 PR body.
3. In that session, check that `cache_read_input_tokens` is above 0 from turn 2 (S12). If it stays at 0, report it in the PR.
4. Check that turn 2 and later get no 400 about thinking blocks (#1964 R14 and S6). If they do, report the error message in the PR.
