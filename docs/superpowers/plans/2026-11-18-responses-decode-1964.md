# Responses agent side of the translation hub (#1964) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The inference router can send Codex's OpenAI Responses requests to a server that serves only Chat Completions, and Codex gets valid Responses events back.

**Architecture:** A new package `core/translate/responses` implements `pivot.RequestDecoder`, `pivot.ReplyEncoding` and `EncodeError`, and replaces the stub in `core/translate/registry.go`. The hub contract changes first: the reply encoder gets the decoded request (`Request.Origin` carries the tool-name map), `pivot.Event` gets `Data` for redacted thinking, and a source protocol removes its own headers.

**Tech Stack:** Go 1.26.10, standard library only.

**Spec:** GitHub issue rossoctl/cortex#1964 (`gh issue view 1964 -R rossoctl/cortex`). The issue is the spec. Rule numbers in this plan (R1-R15) are the issue's rule numbers. Parent rules H1-H6 and P1 are in #1963. #1911 rules are in `docs/superpowers/plans/2026-11-17-translation-hub-1911.md` and issue #1911. **Every implementer reads the issue rules that their task names before starting.**

## Global Constraints

- Standard library only. `core/translate/...` non-test code imports only the standard library and other `core/translate` packages. `go mod tidy -diff` in `core` prints nothing.
- Protocol names (exact): `anthropic-messages`, `chat-completions`, `responses`.
- Deterministic encoding (#1911 rule 9a): structs with fixed field order. JSON written with `SetEscapeHTML(false)`, as `chat/protocol.go` `marshal` does.
- Tool-call ids pass byte for byte in both directions (#1911 rule 20a).
- Never copy an unknown field to the target. Unknown top-level fields go to `Extra` under `responses.<field>` (R6).
- Golden expected-output files are written by the test's `-update` flag (`var update = flag.Bool("update", false, "rewrite golden files")`). Input fixtures are written by hand from the issue's "Facts" section. Do not commit a captured Codex request.
- Refusals are `*pivot.RequestError{Status: 400, Type: "invalid_request_error", Message: <exact text from the issue>}`. Copy each message from the issue byte for byte.
- Run every Go command per module with `GOWORK=off`. Commit with `git commit -s`. Comments and commit messages in ASD-STE100 Simplified Technical English. End commit messages with `Assisted-By: Claude (Anthropic AI) <noreply@anthropic.com>` (repo CLAUDE.md attribution policy).

## Decisions made for this plan (with reasons)

1. **Two PRs, not one.** The issue asks for one PR under 2,000 counted lines. The estimate is about 2,600 counted lines (tests and hand-written input fixtures count). Reason: the workflow's 2,000-line limit. P1 holds the hub changes and the request decoder. P2 holds the reply encoder and the registration.
2. **The decoder is registered only in P2 (T5).** Reason: with a decoder and no reply encoder, the router plans a translation (`plan` in `core/plugins/inferencerouter/api.go` checks only `translate.Decodes`) and the listener then refuses every Codex request to a Chat-only server with a 502. P1 changes no runtime behavior.
3. **Source headers:** a new optional interface in `core/translate/pivot/protocol.go`: `type SourceHeaderRemover interface { RemoveSourceHeaders(h http.Header) }`. `translate.Request` calls it on the source protocol, when `h != nil`, before the encoder runs. The chat encoder stops deleting `Anthropic-Version` and `Anthropic-Beta`. The `anthropic` package removes them. Reason: R3, and a prefix rule (`X-Codex-`, `X-OpenAI-`) cannot be a fixed name list.
4. **R2 signatures:** `pivot.ReplyEncoding.NewReplyEncoder(req pivot.Request, stream bool) pivot.ReplyEncoder`. `translate.Converted` gets a field `Req pivot.Request`. `translate.Reply(source, target string, req pivot.Request, status int, upstreamStream, agentStream bool, body []byte) (int, []byte)`. The listener passes `t.conv.Req`.
5. **`Converted.Req` holds no history.** After the encoder runs, `translate.Request` sets `Req` to the decoded request with `System` and `Messages` set to nil. Reason: the reply encoder needs only `Model`, `Stream`, `Tools` and `Origin`, and the listener keeps `Converted` until the reply ends. The history of a 2 MB request must not stay in memory twice.
6. **The Responses-to-Chat request goldens live in `core/translate/responses`** and call `DecodeRequest` and then `chat.Protocol{}.EncodeRequest` (a test-only import). Reason: P1 does not register the decoder (decision 2).
7. **`Origin` type:** `responses` keeps an unexported `type origin struct { tools map[string]toolRef }` with `type toolRef struct { kind, namespace, name string }` (`kind` is `"function"` or `"custom"`). The decoder sets `Request.Origin` to `*origin`. The encoder does a type assertion and treats any other value (nil included) as an empty map.
8. **Message `content` as a string** (Responses allows `"content": "hi"`) → one text block. Reason: the spec is silent, and other Responses agents send it.
9. **Item `id` and `status` fields are ignored and not recorded in `dropped`.** Reason: they are Responses bookkeeping, not content. R6 names top-level fields only.
10. **`output: ""` on a tool output** → a `tool_result` with no content blocks. Reason: the spec is silent. The Chat encoder already handles an empty tool result.
11. **A stream that ends with no `stop` event** gives `response.failed` with code `api_error` and the message "the inference server's stream ended before the reply was complete". Reason: a broken stream must not look complete (#1911 decision 5). The Chat decoder already gives an error event here, so this is a guard in the encoder's `End`.
12. **R12 `context_length_exceeded`:** the pivot error message matches when it contains (ASCII case-insensitive) `prompt is too long`, `context length`, `context_length_exceeded` or `maximum context`. Reason: these are the phrases of Anthropic, OpenAI-compatible and vLLM errors that the Chat and Anthropic decoders pass through.
13. **R15 budget:** checked by a benchmark, not by a timed test. The implementer of T3 puts the `ns/op` of `BenchmarkDecodeRequest` in the T3 commit message. Reason: a timed test flakes on a busy CI host.
14. **The R11 usage round trip test lives in `core/parse/inference`** (external test package `inference_test`), and imports `core/translate/responses`. Reason: `core/translate` must not import `core/parse`.

## Review Focus

1. Several `function_call` items in a row, then several outputs: one assistant message with all `tool_use` blocks, then one user message with all `tool_result` blocks (R7 grouping). Test in T3: `TestDecode_ParallelCallsGroup`.
2. A message whose `content` is a string (decision 8). Test in T3: `TestDecode_StringContent`.
3. A stream that ends with no stop event (decision 11). Test in T4: `TestEncode_StreamEndsEarly`.
4. A `tool_use` name that the model made up and that is not in the map: a `function_call` with that name, no `namespace`. Test in T4: `TestEncode_UnknownToolName`.
5. A tool output whose `call_id` belongs to a dropped `local_shell_call` that comes after a kept call in the same turn: only the dropped one goes. Test in T3: `TestDecode_DropsOutputOfDroppedCallOnly`.

## File structure

| File | Task | Responsibility |
|---|---|---|
| `core/translate/pivot/pivot.go` | T1 | `Request.Origin any` |
| `core/translate/pivot/event.go` | T1 | `Event.Data string` (block_start of `redacted_thinking`) |
| `core/translate/pivot/protocol.go` | T1 | `NewReplyEncoder(req, stream)`, `SourceHeaderRemover` |
| `core/translate/convert.go` | T1 | header removal, `Converted.Req`, `Reply` signature |
| `core/translate/anthropic/{protocol,reply}.go` | T1 | `RemoveSourceHeaders`, new encoder signature, `data` on `redacted_thinking` |
| `core/translate/chat/{request,reply}.go` | T1, T2 | stop deleting Anthropic headers (T1), redacted thinking (T1), effort (T2) |
| `core/listener/forwardproxy/translate.go` | T1 | pass `t.conv.Req` |
| `core/translate/responses/protocol.go` | T3 | `Protocol`, `Name`, `marshal`, `RemoveSourceHeaders` |
| `core/translate/responses/error.go` | T3 | `EncodeError` (moved from the stub) |
| `core/translate/responses/request.go` | T3 | `DecodeRequest`: R5, R6, R7, R10 |
| `core/translate/responses/tools.go` | T3 | R8 tools, R9 flat names, `origin` |
| `core/translate/responses/reasoning.go` | T3 | R14 `cortex1:` codec, both directions |
| `core/translate/responses/reply.go` | T4 | `NewReplyEncoder`: R11, R12, R13 |
| `core/translate/registry.go` | T5 | register `responses.Protocol{}`, remove the stub |

## PR list

| PR | Title | Parent | Tasks | estLines |
|---|---|---|---|---|
| P1 | `feat: Decode OpenAI Responses requests in the translation hub` | "" | T1, T2, T3 | 1500 |
| P2 | `feat: Encode Responses replies, so Codex can use a Chat Completions server` | P1 | T4, T5 | 1100 |

## Gate

From the worktree root:

```sh
(cd core && GOWORK=off go vet ./... && test -z "$(gofmt -l .)" && GOWORK=off go test ./translate/... ./parse/... ./plugins/inferencerouter/... ./listener/... ./pipeline/... && GOWORK=off go test -run '^$' -bench . -benchtime 1x ./translate/... && GOWORK=off go mod tidy -diff) && (cd cmd/cortex && GOWORK=off go vet ./...) && (cd cmd/cortex-envoy && GOWORK=off go vet ./...)
```

---

## PR P1

### Task T1: Hub contract (R2, R2a, R2b, R3)

Depends on: none.

**Files:**
- Modify: `core/translate/pivot/pivot.go`, `core/translate/pivot/event.go`, `core/translate/pivot/protocol.go`
- Modify: `core/translate/convert.go`, `core/translate/reader_test.go`, `core/translate/golden_test.go`
- Modify: `core/translate/anthropic/protocol.go`, `core/translate/anthropic/reply.go`, `core/translate/anthropic/reply_test.go`
- Modify: `core/translate/chat/request.go:95-96`, `core/translate/chat/reply.go:394-397`, `core/translate/chat/reply_test.go`
- Modify: `core/listener/forwardproxy/translate.go`
- Create: `core/translate/testdata/reply/redacted_thinking.json`, `core/translate/testdata/reply/redacted_thinking.sse`
- Test: `core/translate/convert_test.go` (create)

**Interfaces:**
- Produces: `pivot.Request.Origin any` (doc: set by a source decoder for its own reply encoder; other encoders ignore it; never in `dropped`). `pivot.Event.Data string`. `pivot.ReplyEncoding.NewReplyEncoder(req pivot.Request, stream bool) pivot.ReplyEncoder`. `pivot.SourceHeaderRemover` (decision 3). `translate.Converted.Req pivot.Request` (decision 5). `translate.Reply(source, target string, req pivot.Request, status int, upstreamStream, agentStream bool, body []byte) (int, []byte)`.

- [ ] **Step 1: Write the failing tests**
  - `TestRequest_RemovesSourceHeaders` in `convert_test.go`: `Request(Anthropic, Chat, body, h)` with `h` holding `Anthropic-Version`, `Anthropic-Beta` and `X-Keep`. After: only `X-Keep` stays.
  - `TestRequest_ReqHasNoHistory`: `Converted.Req.Model` is the request model, `Req.Messages` and `Req.System` are nil, `Req.Stream` equals `Converted.Stream`.
  - Golden: `testdata/reply/redacted_thinking.{json,sse}`, a Chat reply whose `thinking_blocks` holds `{"type":"thinking",...,"signature":"sig1"}` then `{"type":"redacted_thinking","data":"cmVkYWN0ZWQ="}`, then text. Run `TestReplyGolden -update` and read the golden: the Anthropic reply has a `redacted_thinking` block with `"data":"cmVkYWN0ZWQ="`, streamed (`content_block_start`) and not streamed.
- [ ] **Step 2: Run** `cd core && GOWORK=off go test ./translate/...`. Expected: FAIL (compile errors, missing goldens).
- [ ] **Step 3: Implement.** Change the signatures above and every caller listed in Files. `translate.Request` calls the source's `RemoveSourceHeaders` before `EncodeRequest`. Anthropic `block()` gets a `data` argument for `redacted_thinking`. The Chat decoder emits a `redacted_thinking` entry as `EventBlockStart{Block: BlockRedactedThinking, Data: tb.Data}` then `EventBlockStop`; remove the `shortcut:` comment. The not-streamed Anthropic encoder also keeps `Data` for the block. The `RequestEncoder` doc comment no longer says that the encoder removes source headers.
- [ ] **Step 4: Run** `cd core && GOWORK=off go test ./translate/... ./listener/forwardproxy/ ./plugins/inferencerouter/...`. Expected: PASS. `git status` shows no change to any existing golden file (R2 test: the Anthropic and Chat goldens of #1911 do not change).
- [ ] **Step 5: Commit** `refactor: Give the reply encoder the request, carry redacted thinking data, and let the source remove its headers`.

### Task T2: Effort for Chat targets (R2c)

Depends on: none.

**Files:**
- Modify: `core/translate/chat/request.go:311-318` (`effort`)
- Test: `core/translate/chat/request_test.go`, create `core/translate/testdata/request/r2c_effort_minimal.json`

- [ ] **Step 1: Write the failing test.** `TestEffort` table: `none`→`low`, `minimal`→`low`, `low`→`low`, `medium`→`medium`, `high`→`high`, `xhigh`→`high`, `max`→`high`, `""` with no thinking → `""`. Add the input fixture `r2c_effort_minimal.json`: an Anthropic request with `"output_config":{"effort":"minimal"}` (use the field that `anthropic/request.go` reads for effort; read it first). Run `TestRequestGolden -update` once and check that the golden holds `"reasoning_effort":"low"`.
- [ ] **Step 2: Run** `cd core && GOWORK=off go test ./translate/chat/ -run TestEffort`. Expected: FAIL for `none` and `minimal`.
- [ ] **Step 3: Implement.** Add `case "none", "minimal": return "low"` to `effort`.
- [ ] **Step 4: Run** `cd core && GOWORK=off go test ./translate/...`. Expected: PASS, and no existing golden changed.
- [ ] **Step 5: Commit** `fix: Send low reasoning effort to a Chat server for none and minimal`.

### Task T3: The Responses request decoder (R1 decode side, R3 list, R5-R10, R14 decode, R15 decode benchmark)

Depends on: T1.

**Files:**
- Create: `core/translate/responses/{protocol.go,error.go,request.go,tools.go,reasoning.go}`
- Create: `core/translate/responses/{request_test.go,golden_test.go,reasoning_test.go,bench_test.go}`
- Create: `core/translate/responses/testdata/request/*.json` (inputs) and `*.golden` (written by `-update`)

**Interfaces:**
- Consumes: `pivot.Request.Origin`, `pivot.SourceHeaderRemover` (T1).
- Produces:
  - `responses.Protocol struct{}`, `Name() string` = `"responses"`, `EncodeError(status int, errType, message string) []byte` (same bytes as `chat.Protocol{}.EncodeError` gives today: `{"error":{"message":…,"type":…,"code":null}}`), `DecodeRequest(body []byte) (pivot.Request, error)`, `RemoveSourceHeaders(h http.Header)`.
  - `type origin struct{ tools map[string]toolRef }`, `type toolRef struct{ kind, namespace, name string }` (decision 7).
  - `func encodeReasoning(b pivot.Block) string` and `func decodeReasoning(s string) (pivot.Block, bool)` in `reasoning.go` (R14; `false` for content without the `cortex1:` prefix or with a bad payload). Payload struct field order: `k`, `t`, `s`, `d`, each `omitempty`. `base64.RawURLEncoding`.
  - `func flatName(namespace, name string) string` in `tools.go` (R9).

- [ ] **Step 1: Write the failing tests.**
  - `golden_test.go`: `TestRequestGolden` over `testdata/request/*.json`. Golden form as in `core/translate/golden_test.go` `requestGolden`: line 1 `dropped: <list>`, line 2 the Chat body from `chat.Protocol{}.EncodeRequest(req, nil)`; a refusal gives one `error: <err>` line.
  - Input fixtures, one per rule part: `r5_previous_response_id.json`, `r5_conversation.json`, `r6_input_string.json`, `r6_tool_choice_{auto,none,required,function}.json`, `r6_parallel_false.json`, `r6_text_format_json.json` (refusal), `r6_unknown_fields.json` (every field R6 lists plus `zzz_unknown`), `r7_leading_developer.json`, `r7_later_developer.json` (golden must show a `user` message on Chat), `r7_user_after_outputs.json`, `r7_image_data_url.json`, `r7_image_http_url.json`, `r7_image_file_id.json` (refusal), `r7_input_file.json` (refusal), `r7_refusal_part.json`, `r7_bad_arguments.json` (refusal names the tool), `r7_custom_tool_call.json`, `r7_output_array.json`, `r7_additional_tools.json`, `r7_compaction.json` (refusal), `r7_dropped_items.json` (`web_search_call`, `image_generation_call`, `tool_search_call`, `tool_search_output`, `local_shell_call` with its `function_call_output`, and `zzz_item`), `r8_tools.json` (function with `strict` and `defer_loading`, custom `apply_patch` with a grammar, namespace, `web_search`, `mcp`), `r9_long_name.json`, `r9_dot_name.json`, `r9_duplicate.json` (refusal names both), `r10_compaction_trigger.json` (refusal), `r14_cortex1_reasoning.json`, `r14_openai_reasoning.json` (dropped, `reasoning` in `dropped`), and `full_multiturn.json` with every item that the issue's Tests section lists.
  - `request_test.go`: `TestDeterministic` (#1911 rule 9a: decode `full_multiturn.json`, encode to Chat twice, same bytes; append one `user` message item, and the bytes before the new turn do not change). `TestFlatName` (R9: `functions`/`""` keep the name; `mcp__github` + `create_issue` → `mcp__github__create_issue`; a 70-character name and a name with `.` give 64 or fewer characters from `[a-zA-Z0-9_-]`, the same value on each call, and a different value for a different input). `TestOriginMap` (the decoded `Origin` maps each flat name to kind, namespace and name). `TestRemoveSourceHeaders` (every R3 header and `X-Codex-Turn`, `X-OpenAI-Foo` go; `X-Keep` and `Authorization` stay). Plus the Review Focus tests `TestDecode_ParallelCallsGroup`, `TestDecode_StringContent`, `TestDecode_DropsOutputOfDroppedCallOnly`.
  - `reasoning_test.go`: `TestReasoningRoundTrip` (R14: a `thinking` block with text and signature, and a `redacted_thinking` block with data, are byte-identical after `decodeReasoning(encodeReasoning(b))`; the prefix is `cortex1:`; no `=` padding).
  - `bench_test.go`: `BenchmarkDecodeRequest` on a 2 MB Codex-shaped body made in code (instructions, a developer message, about 400 turns of `function_call`, `function_call_output`, `message` and a `cortex1:` reasoning item, 40 tools). Budget: 15 ms per op (R15, decision 13).
- [ ] **Step 2: Run** `cd core && GOWORK=off go test ./translate/responses/`. Expected: FAIL (package does not exist).
- [ ] **Step 3: Implement** the files above. Decode in one pass: `json.Unmarshal` into a struct with `json.RawMessage` for `input`, `tools`, `tool_choice` and each item, and a `map[string]json.RawMessage` only for the leftover top-level fields that go to `Extra`. Collect the `call_id`s of dropped calls while you walk the items. Because a dropped call always comes before its output, one pass is enough. Each refusal uses the issue's exact message. Do not register the protocol (decision 2).
- [ ] **Step 4: Run** `cd core && GOWORK=off go test ./translate/... && GOWORK=off go test -run '^$' -bench DecodeRequest ./translate/responses/`. Expected: PASS, and `ns/op` ≤ 15,000,000.
- [ ] **Step 5: Commit** `feat: Decode OpenAI Responses requests to the pivot form`, with the benchmark line in the body.

---

## PR P2

### Task T4: The Responses reply encoder (R11, R12, R13, R14 encode)

Depends on: T1, T3.

**Files:**
- Create: `core/translate/responses/reply.go`, `core/translate/responses/reply_test.go`, `core/translate/responses/testdata/reply/*.golden` (written by `-update`)
- Modify: `core/translate/responses/bench_test.go`
- Create: `core/parse/inference/responses_translate_test.go`

**Interfaces:**
- Consumes: `origin`, `toolRef`, `encodeReasoning` (T3). `NewReplyEncoder(req pivot.Request, stream bool)`, `Event.Data` (T1).
- Produces: `func (Protocol) NewReplyEncoder(req pivot.Request, stream bool) pivot.ReplyEncoder`.

- [ ] **Step 1: Write the failing tests.**
  - `reply_test.go`, `TestReplyGolden`: a table of hand-written `[]pivot.Event` cases, each encoded streamed (`<name>.sse.golden`) and not streamed (`<name>.json.golden`): `text_only`, `thinking_signature`, `redacted_thinking`, `function_call`, `namespaced_function_call` (the request's `Origin` comes from decoding a request that has a namespaced tool), `custom_tool_call`, `custom_bad_input` (gives `response.failed` naming the tool), `max_tokens`, `refusal`, `error_event` (status 429 → code `rate_limit_exceeded`), `error_overloaded` (529 → `server_is_overloaded`), `error_context` (message "prompt is too long" → `context_length_exceeded`), `usage_cache`.
  - `TestChatRepliesAsResponses`: each `../testdata/reply/bob_shape*` and `error_*` file, decoded by `chat.Protocol{}.NewReplyDecoder` and encoded by this encoder. Goldens in `testdata/reply/<base>.to-responses-{sse,json}.golden`. A non-2xx case checks the status and the OpenAI error body (R12).
  - `TestSplitAtEveryByte`: for each `bob_shape*.sse`, feed the Chat SSE split at each byte boundary; the output equals the output of one whole feed.
  - `TestEncode_UsageShape` (R11): usage `{Input:100, Output:20, CacheRead:300, CacheCreation:50}` gives `input_tokens: 450`, `cached_tokens: 300`, `cache_write_tokens: 50`, `output_tokens: 20`, `total_tokens: 470`.
  - `TestEncode_ItemIDs`: `msg_0`, `rs_1`, `fc_2`, `ctc_3` for blocks at output index 0-3.
  - `TestEncode_ReasoningRoundTrip` (R14): the `encrypted_content` of a streamed reasoning item, put in a request item and decoded by `DecodeRequest`, gives the same block.
  - Review Focus: `TestEncode_StreamEndsEarly` (decision 11), `TestEncode_UnknownToolName`.
  - `bench_test.go`: `BenchmarkEncodeReplyStream`, 2,000 pivot events (text and tool-input deltas).
  - `core/parse/inference/responses_translate_test.go`, `TestTranslatedResponsesUsage` (R11, decision 14): encode a stream with usage `{Input:100, Output:20, CacheRead:300, CacheCreation:50}`, parse the bytes with the parser that `core/parse/inference/responses.go` defines, and check `Input == 100`, `CacheRead == 300`, `CacheWrite == 50`, `Output == 20`.
- [ ] **Step 2: Run** `cd core && GOWORK=off go test ./translate/responses/ ./parse/inference/`. Expected: FAIL.
- [ ] **Step 3: Implement** `reply.go`. Streamed: each event gives its frames at once (`event: <type>\ndata: <json>\n\n`). Keep only the open item's text, signature, data and tool input. Not streamed (R13): collect, and `End` gives one response object. `End` with no stop event: decision 11.
- [ ] **Step 4: Run** the gate's `core` test and bench steps. Expected: PASS.
- [ ] **Step 5: Commit** `feat: Encode pivot reply events as OpenAI Responses replies`.

### Task T5: Register `responses`, and test the router and the listener (R1, R3, R4, R5, R10 end to end)

Depends on: T3, T4.

**Files:**
- Modify: `core/translate/registry.go` (register `responses.Protocol{}`, delete the stub type and its `chat` import if unused), `core/translate/registry_test.go`
- Modify: `core/plugins/inferencerouter/api_test.go`
- Modify: `core/listener/forwardproxy/translate_test.go`

- [ ] **Step 1: Write the failing tests.**
  - `registry_test.go`: `Decodes("responses")` is true. `Target` cases do not change (`responses` has no request encoder).
  - `api_test.go` `TestPlan`: `{"codex to chat only", {"chat-completions": "/v1/chat/completions"}, "/backend-api/codex/responses"}` → `planTranslate`, source `responses`, target `chat-completions`, path `/v1/chat/completions`. `{"codex to responses", {"responses": "/v1/responses"}, "/backend-api/codex/responses"}` → `planPath` `/v1/responses`. A router-level test, in the style of `TestRouter_APathEntrySetsTheRedirectPath`, checks that the body for the `responses` server is not changed (R4).
  - `translate_test.go`:
    - `TestTranslate_CodexToChat`: a fake Chat upstream that streams text, one tool call and usage with `cached_tokens` and `cache_creation_input_tokens`. A Codex-shaped streamed request to `/backend-api/codex/responses` with `OpenAI-Beta`, `Originator`, `ChatGPT-Account-Id`, `X-Codex-Turn` headers. Check: the upstream got none of those headers (R3) and a Chat body. The agent got `response.created` … `response.completed`. The session row shows the Responses reply and its input, cache-read, cache-write and output tokens.
    - `TestTranslate_CodexStateRefused` (R5) and `TestTranslate_CodexCompactRefused` (R10): status 400, body `{"error":{"message":<exact text>,"type":"invalid_request_error","code":null}}`, and the fake upstream got no request.
- [ ] **Step 2: Run** `cd core && GOWORK=off go test ./translate/ ./plugins/inferencerouter/ ./listener/forwardproxy/ -run 'Registry|Decodes|TestPlan|Codex'`. Expected: FAIL.
- [ ] **Step 3: Implement** the registration.
- [ ] **Step 4: Run** the whole gate. Expected: PASS.
- [ ] **Step 5: Commit** `feat: Translate Codex Responses requests to Chat Completions servers`.

---

## For a human after review

These steps need a live Codex agent and a real server. They need T1-T5 (both PRs merged or checked out).

1. Configure a local Chat server (Ollama or llama-server) with `api: {chat-completions: /v1/chat/completions}` only. Run `agentop server use` to send Codex to it. Run Codex for at least 3 tool-call turns, one of them an `apply_patch`. Put the session id, the turn count and the `dropped` list of one turn in the PR body.
2. If the `dropped` list shows an item type that rule R7 drops but that carries content the model needs, report it. Do not change the rule without a decision.
3. Run `/compact` in Codex on that session. Report what Codex shows for the rule R10 error.
4. Decide if the two-PR split (decision 1) is acceptable, because the issue asked for one PR.
