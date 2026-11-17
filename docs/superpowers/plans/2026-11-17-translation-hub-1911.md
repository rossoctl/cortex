# Translation hub (Anthropic Messages to chat completions) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The inference router can send Claude Code's Anthropic `/v1/messages` requests to a server that serves only OpenAI `/v1/chat/completions`, and Claude Code gets valid Anthropic replies, streamed or not.

**Architecture:** A new stdlib-only hub, `core/translate`: one pivot type (`core/translate/pivot`), one package per protocol (`anthropic`, `chat`), and a compiled-in registry (`core/translate`). The router reads three new server fields (`api`, `auth_header`, `headers`) and asks for translation through a new `pipeline.Context` call. The forward proxy converts the request after the last request plugin and converts the reply after the resend loop, before any reply plugin or recorder reads it.

**Tech Stack:** Go 1.26.10, standard library only. No new module dependency.

**Spec:** GitHub issue rossoctl/cortex#1911 (`gh issue view 1911 -R rossoctl/cortex`). The issue is the spec. Rule numbers in this plan (H1-H6, C1-C4, 4-10, F1, 11-24) are the issue's rule numbers. **Every implementer reads the issue rules that their task names before starting.**

## Global Constraints

- Standard library only. `go mod tidy -diff` must print nothing in `core` and `cmd/agentop`.
- `core/translate/...` imports only the standard library and other `core/translate` packages.
- `core/plugins/inferencerouter/routerconfig` stays stdlib-only (agentop imports it). It must not import `core/translate`.
- Protocol names (exact): `anthropic-messages`, `chat-completions`, `responses`.
- Deterministic encoding (rule 9a): structs with fixed field order; any map is marshalled by `encoding/json` (sorted keys). User, system and tool-message content is always a list of parts, never a plain string.
- Tool-call ids pass byte-exact in both directions (rule 20a). Never check, shorten or rewrite an id.
- Never copy an unknown top-level field into a chat body (rule 11).
- Test fixtures are written by hand from the issue's "Measured Bob stream shape" section. Do not commit a captured Claude Code request.
- Golden expected-output files live under `testdata/` and are written by the test's `-update` flag: `var update = flag.Bool("update", false, "rewrite golden files")`. A reviewer reads each golden file against the issue rules. Input fixtures are hand-written.
- Run every Go command per module with `GOWORK=off`. Commits use `git commit -s`. Comments and commit messages in ASD-STE100 Simplified Technical English.
- The server URL has no path today (`routerconfig.Server.URL`: "no path"). The issue's "path prefix" comes in #1912. In this plan the wire path is exactly `api[<protocol>]`.
- Fallback doc URL (used by the rule 4a error, exact): `https://github.com/rossoctl/cortex/blob/main/docs/translation-server.md`.

## Decisions made for this plan (with reasons)

1. **Package layout:** `core/translate/pivot` (types and interfaces), `core/translate/anthropic`, `core/translate/chat`, `core/translate` (registry, helpers, cross-protocol goldens). Reason: a registry in the same package as the pivot type would import the protocol packages, which import the pivot type: an import cycle.
2. **Directions are optional interfaces** that a protocol value implements; the registry type-asserts them. Reason: rule H4 "a protocol can leave a direction out".
3. **`NewReplyDecoder(status int, stream bool)`** takes the upstream status and whether the upstream body is SSE. Reason: a chat server can answer a stream request with a JSON error; the decoder must know which body it reads. A non-2xx decoder reads the whole body (at most 1 MiB) and gives one `error` event.
4. **Encoder `stream` follows the agent's request** (`stream` in the agent's body), decoder `stream` follows the upstream `Content-Type`. Reason: the agent must get the shape it asked for even when the server ignores `stream`.
5. **Event stream end:** the chat decoder emits `usage` then `stop` at `[DONE]`. When the body ends after `finish_reason` with no `[DONE]`, it emits them at `End()`. When the body ends before any `finish_reason`, it emits an `error` event (502 `api_error`, "the inference server's stream ended before the reply was complete"). Reason: a broken stream must not look like a finished reply.
6. **Tool-call pieces out of order:** an `arguments` piece for a tool-call index whose block is already closed gives an `error` event (502 `api_error`, naming the index). Reason: no measured server does this; buffering it is code for no known case.
7. **Rule 20 invalid tool JSON** is checked only for a not-streamed reply. Reason: a stream cannot be checked without buffering it (rule 10).
8. **Tool messages (rule 14):** `content` is a list of `text` parts, with the `tool_result`'s `cache_control` on its last part. Reason: rule 9a (always parts) and the cache marker. Whether Bob accepts list content on `role: tool` is a check for a human (#1912's list).
9. **Assistant messages (rule 15):** `content` is a string of the joined text blocks, left out when there is no text.
10. **Unsigned `thinking` in assistant history** is not sent and is named in `dropped` as `thinking`.
11. **`dropped`** is a sorted, unique list: each request, message and block `Extra` key of another protocol with its `<protocol>.` prefix removed (for example `top_k`, `metadata`), plus `is_error` when a `tool_result` had `is_error: true`, plus `thinking` (decision 10).
12. **Preference order for the target protocol** (rule 4): `chat-completions`, `anthropic-messages`, `responses`; the first one in the `api` map whose protocol has a request encoder.
13. **The rule 4a message** names every `api` key, sorted, joined with " or ": `Cortex cannot translate anthropic-messages to responses yet. Use a translation server for this pair (see https://github.com/rossoctl/cortex/blob/main/docs/translation-server.md).`
14. **Source protocol** comes from a new exported `inference.ProtocolFor(path string) string` in `core/parse/inference/dialect.go`, which calls `dialectFor`. It gives `anthropic-messages` for the Anthropic dialect, `chat-completions` only when the path ends in `/chat/completions`, `responses` for the Responses dialect, and `""` otherwise (legacy `/v1/completions` is `""`). Reason: rule 4 (one rule for parser and translator) without renaming the unexported type through the parser.
15. **routerconfig keeps its own list** `APIProtocols = []string{"anthropic-messages", "chat-completions", "responses"}`. A test in `core/plugins/inferencerouter` pins it equal to `translate.Names()`. Reason: routerconfig must stay free of `core/translate`; this repeats the `NoAgent` pattern that the package already uses.
16. **Two pipeline calls**, beside `Redirect` and `SetRedirectPath`: `Translate(source, target, path)` and `SetUpstreamHeaders(h)`. Both have the gates of `SetRedirectPath`. Reason: rule 5 (translation target kept privately), and rule C3 (fixed headers "after the plugins run": only the listener can apply them after the last plugin).
17. **The listener records `modify/translated`**, because only the encoder knows `dropped`. It converts the request right after the pipeline run and the reject check, before the request row is recorded, so the invocation is on the request row. All plugins have run at that point (rule 8).
18. **The listener passes `pctx.Headers` to `EncodeRequest`**, which deletes `anthropic-version` and `anthropic-beta` there. The sync to the wire request then removes them.
19. **The refused attempt row of a resend** (`resendIfAsked`) records the Anthropic form of the refused body (`translate.Reply`). The raw head is still what `Resend` reads (rule 8) and what the capture keeps.
20. **Capture field names:** `model.RequestCapture.Translated []byte`; `model.ResponseCapture.Upstream []byte` and `UpstreamTruncated bool`; `model.RawCapture.Translated *string` / `TranslatedBase64 []byte` (`json:"translated"`, `"translatedBase64"`); `model.RawBody.Upstream *string` / `UpstreamBase64 []byte` / `UpstreamTruncated bool` (`json:"upstream"`, `"upstreamBase64"`, `"upstreamTruncated"`).
21. **The hop path (`s.Hop`, praxis) gets no special code.** The request conversion runs before the `hopped` branch in `serveOutbound`, and the reply conversion runs after `hopDo` returns, so both arms convert. When `hopDo` answers its own 503 `gateway_unavailable` (`answered` true), that answer is not converted. No hop test in this plan.
22. **`record.go` does not change.** `recordCapturedRequest` copies the capture struct, so a `Translated` field set before the call is recorded.
23. **Three PRs, stacked.** The total estimate is about 5,000 counted lines, so three PRs is the fewest under the 2,000-line limit. The issue's split (a)/(b) is kept, with (b) cut in two: PR 2 adds the listener layer with its own test plugin; PR 3 adds the config fields with the router that reads them, so no config field merges before its reader.

## Review Focus

1. **A chat stream that breaks before `finish_reason`** must reach the agent as an Anthropic `event: error`, not as a short reply that looks complete. Test: T3 `TestReplyDecoder_EndsEarly`.
2. **A gzip-encoded chat reply** must be decoded before conversion. Test: T8 `TestTranslate_GzipUpstream`.
3. **A server that ignores `stream: true` and answers JSON** (or the reverse) must still give the agent the shape it asked for. Test: T8 `TestTranslate_AgentStreamsServerAnswersJSON`.
4. **A non-JSON error page** (an HTML 502 from a gateway) must become an Anthropic `api_error` with the first 512 bytes of the body text as the message. Test: T3 `TestReplyDecoder_HTMLErrorPage`.
5. **Two tool calls in one stream** (index 0, then index 1) must give two `tool_use` blocks in order; a piece for a closed index gives an error event. Tests: T4 golden `stream_two_tools`, T3 `TestReplyDecoder_ClosedIndexPiece`.

---

## File structure

| Path | Task | Responsibility |
|---|---|---|
| `core/translate/pivot/pivot.go` | T1 | Request, Message, Block, Tool, ToolChoice, Thinking, Usage, Extra |
| `core/translate/pivot/event.go` | T1 | Event, EventKind, DeltaKind |
| `core/translate/pivot/protocol.go` | T1 | Protocol and direction interfaces, RequestError, ErrorType |
| `core/translate/anthropic/{request,reply,error}.go` | T2 | DecodeRequest, NewReplyEncoder, EncodeError |
| `core/translate/chat/{request,reply,error}.go` | T3 | EncodeRequest, NewReplyDecoder, EncodeError |
| `core/translate/{registry,convert,reader}.go` | T4 | Lookup, Names, Target, Request, Reply, ErrorBody, NewReplyReader |
| `core/translate/{golden_test.go,testdata/}` | T4 | Cross-protocol goldens |
| `core/pipeline/destination.go` | T6 | Translate, TranslateTarget, SetUpstreamHeaders, UpstreamHeaders |
| `core/listener/forwardproxy/translate.go` | T8 | Request and reply conversion in the forward proxy |
| `core/model/capture.go`, `core/session/archive/capture.go`, `core/sessionapi/{raw,ingest}.go`, `cmd/agentop/tui/detail_raw.go` | T9 | Rule 8a capture fields |
| `core/plugins/inferencerouter/routerconfig/config.go` | T5 | `api`, `auth_header`, `headers` fields and their checks |
| `core/redact/redact.go` | T5 | Redact the values under a key named exactly `headers` |
| `core/parse/inference/dialect.go`, `core/plugins/inferencerouter/{api.go,plugin.go}` | T7 | ProtocolFor; router rules 4-7, C1-C3 |
| `cmd/agentop/{cmd_server_write.go,cmd_server.go,README.md}` | T10 | Rule C4 flags and display |
| `docs/translation-server.md`, `docs/inference-router-plugin.md`, `docs/README.md` | T11 | F1 and the field reference |

## PR list

| PR | Title | Parent | Tasks | estLines |
|---|---|---|---|---|
| P1 | `feat: Add the core/translate hub with Anthropic Messages and chat completions protocols` | "" | T1, T2, T3 | 1700 |
| P2 | `feat: Translate inference requests and replies in the forward proxy` | P1 | T4, T6, T8, T9 | 1900 |
| P3 | `feat: Add the api, auth_header and headers server fields to the inference router` | P2 | T5, T7, T10, T11 | 1500 |

## Gate

From the worktree root:

```sh
(cd core && GOWORK=off go vet ./... && test -z "$(gofmt -l .)" && GOWORK=off go test ./translate/... ./pipeline/... ./redact/... ./parse/... ./plugins/inferencerouter/... ./listener/... ./model/... ./session/... ./sessionapi/... && GOWORK=off go mod tidy -diff) && (cd cmd/cortex && GOWORK=off go vet ./...) && (cd cmd/agentop && GOWORK=off go vet ./... && test -z "$(gofmt -l .)" && GOWORK=off go mod tidy -diff && GOWORK=off go test $(GOWORK=off go list ./... | grep -v 'agentop$')) && scripts/dev/go-test-shards.sh -short cmd/agentop
```

On the base commit 8695f5f0d, the format and vet steps of this gate were run one by one from the worktree root (`core` vet and gofmt, `cmd/cortex` vet, `cmd/agentop` vet and gofmt). All passed. The test, tidy and shard steps were not run.

---

## PR P1

### Task T1: The pivot type

**Depends on:** none. **Estimate:** 230 lines. **Rules:** H2, H3, H4.

**Files:**
- Create: `core/translate/pivot/pivot.go`, `core/translate/pivot/event.go`, `core/translate/pivot/protocol.go`, `core/translate/doc.go` (package comment only for `translate`; T4 adds code)
- Test: `core/translate/pivot/protocol_test.go`

**Interfaces (Produces):**

```go
package pivot

type Extra map[string]json.RawMessage // keys "<protocol>.<field>"

type BlockKind string
const (
	BlockText BlockKind = "text"; BlockImage = "image"; BlockDocument = "document"
	BlockToolUse = "tool_use"; BlockToolResult = "tool_result"
	BlockThinking = "thinking"; BlockRedactedThinking = "redacted_thinking"
)

type Request struct {
	Model       string
	System      []Block // text blocks
	Messages    []Message
	Tools       []Tool
	ToolChoice  *ToolChoice
	MaxTokens   int
	Stop        []string
	Temperature *float64
	TopP        *float64
	Stream      bool
	Effort      string    // "" when none
	Thinking    *Thinking // nil when none
	Extra       Extra
}
type Message struct { Role string; Content []Block; Extra Extra } // Role: user, assistant, system
type Block struct {
	Kind         BlockKind
	Text         string          // text; thinking text
	SourceType   string          // image/document: "base64" or "url"
	MediaType    string
	Data         string          // base64 data; redacted_thinking data
	URL          string
	ID, Name     string          // tool_use
	Input        json.RawMessage // tool_use input, raw
	ToolUseID    string          // tool_result
	Content      []Block         // tool_result content
	IsError      bool            // tool_result
	Signature    string          // thinking
	CacheControl json.RawMessage // nil when none
	Extra        Extra
}
type Tool struct { Name, Description string; InputSchema json.RawMessage; Type string /* "" for a function tool; else a server tool type */; CacheControl json.RawMessage; Extra Extra }
type ToolChoice struct { Type string /* auto, any, tool, none */; Name string; DisableParallel bool }
type Thinking struct { Type string /* enabled, adaptive, disabled */; BudgetTokens int }
type Usage struct { Input, Output, CacheRead, CacheCreation int } // Anthropic meaning: Input excludes cache tokens

type EventKind int
const ( EventStart EventKind = iota; EventBlockStart; EventDelta; EventBlockStop; EventStop; EventUsage; EventError )
type DeltaKind int
const ( DeltaText DeltaKind = iota; DeltaThinking; DeltaSignature; DeltaInputJSON )
type Event struct {
	Kind              EventKind
	ID, Model         string    // start
	Index             int       // block_start, delta, block_stop
	Block             BlockKind // block_start
	ToolID, ToolName  string    // block_start of tool_use
	Delta             DeltaKind // delta
	Value             string    // delta payload
	StopReason        string    // stop: Anthropic vocabulary (end_turn, max_tokens, tool_use, refusal)
	Usage             Usage     // usage
	Status            int       // error
	ErrorType         string    // error: Anthropic error type vocabulary
	Message           string    // error
}

type RequestError struct { Status int; Type, Message string } // implements error; a refusal to send to the agent
func (e *RequestError) Error() string

// ErrorType maps an HTTP status to the Anthropic error type of rule 24:
// 400,422 invalid_request_error; 401 authentication_error; 402,403 permission_error;
// 404 not_found_error; 429 rate_limit_error; 5xx and every other status api_error.
func ErrorType(status int) string

type Protocol interface { Name() string; EncodeError(status int, errType, message string) []byte }
type RequestDecoder interface { DecodeRequest(body []byte) (Request, error) }
type RequestEncoder interface { EncodeRequest(req Request, h http.Header) (body []byte, dropped []string, err error) }
type ReplyDecoding interface { NewReplyDecoder(status int, stream bool) ReplyDecoder }
type ReplyEncoding interface { NewReplyEncoder(stream bool) ReplyEncoder }
type ReplyDecoder interface { Feed(p []byte) []Event; End() []Event } // keeps state between calls; never holds a whole 2xx stream
type ReplyEncoder interface { Encode(ev Event) []byte; End() []byte } // stream: bytes now; not stream: nil until End
```

- [ ] **Step 1:** Write `TestErrorType` (table: 400, 422, 401, 402, 403, 404, 429, 500, 503, 418 → the types above; 418 → `api_error`). Run `cd core && GOWORK=off go test ./translate/pivot/` → FAIL (no package).
- [ ] **Step 2:** Write the three files with the declarations above and doc comments that state the rule each one carries.
- [ ] **Step 3:** Run the test → PASS. `GOWORK=off go vet ./translate/...`.
- [ ] **Step 4:** Commit `feat: Add the translation pivot type`.

### Task T2: The anthropic protocol

**Depends on:** T1. **Estimate:** 600 lines. **Rules:** H2 (decoder side), H3, H4, H6, 13, 13a (keeps `system` role), 14-20a (Anthropic side), 23 (frame shapes), 24.

**Files:**
- Create: `core/translate/anthropic/protocol.go` (`type Protocol struct{}`, `Name()` = `"anthropic-messages"`), `request.go`, `reply.go`, `error.go`
- Test: `core/translate/anthropic/request_test.go`, `reply_test.go`

**Interfaces:** Consumes T1. Produces `anthropic.Protocol` implementing `pivot.Protocol`, `pivot.RequestDecoder`, `pivot.ReplyEncoding`.

- `DecodeRequest`: reads `model`, `system` (string → one text block; array → text blocks with `cache_control`), `messages` (string content → one text block), `tools` (`type` set → `Tool.Type`), `tool_choice` (`disable_parallel_tool_use` → `DisableParallel`), `max_tokens`, `stop_sequences`, `temperature`, `top_p`, `stream`, `output_config.effort` → `Effort`, `thinking` → `Thinking`. Every other top-level field goes to `Request.Extra` as `anthropic.<field>`, every unknown message or block field to that `Extra` (same key form), and other `output_config` fields as `anthropic.output_config`. Image: `source.type` `base64` (media_type, data) or `url`. Document: kind `document`, same source fields. Bad JSON → `*pivot.RequestError{400, "invalid_request_error", …}`.
- `NewReplyEncoder(true)`: SSE frames `event: <type>\ndata: <json>\n\n`. `message_start` at `EventStart` with `{"id","type":"message","role":"assistant","model","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`. `content_block_start` per kind: text `{"type":"text","text":""}`, thinking `{"type":"thinking","thinking":"","signature":""}`, tool_use `{"type":"tool_use","id","name","input":{}}`. Deltas `text_delta`/`thinking_delta`/`signature_delta`/`input_json_delta` (`partial_json`). `EventUsage` is kept; `EventStop` writes `message_delta` `{"type":"message_delta","delta":{"stop_reason":…,"stop_sequence":null},"usage":{"input_tokens","cache_creation_input_tokens","cache_read_input_tokens","output_tokens"}}` then `message_stop`. `EventError` writes `event: error` with `{"type":"error","error":{"type","message"}}`.
- `NewReplyEncoder(false)`: collects events; `End()` writes one message `{"id","type":"message","role":"assistant","model","content":[…],"stop_reason","stop_sequence":null,"usage":{the four fields}}`. Tool input is the joined `input_json_delta` pieces, written as raw JSON (`{}` when empty). An `EventError` makes `End()` write only the error body.
- `EncodeError(status, t, m)` → `{"type":"error","error":{"type":t,"message":m}}`.

- [ ] **Step 1:** Write `TestDecodeRequest_Fields` (one request with system string, a mid-conversation `system` message, image base64 and url, document, tool_use, tool_result with `is_error`, thinking with signature, redacted_thinking, `cache_control` on system and on a block, `top_k`, `metadata`, a server tool `web_search_20250305`, `output_config.effort: "xhigh"`; assert the pivot fields with `reflect.DeepEqual` on a hand-built `pivot.Request`, and `Extra` keys `anthropic.top_k`, `anthropic.metadata`). Write `TestDecodeRequest_BadJSON` (→ `*pivot.RequestError` status 400).
- [ ] **Step 2:** Write `TestReplyEncoder_Stream` (events for: start, thinking block with text and signature, text block, tool_use block with two JSON pieces, usage, stop `tool_use`; assert the exact SSE bytes as a string literal) and `TestReplyEncoder_NotStream` (same events → exact JSON body) and `TestReplyEncoder_Error` (stream: `event: error` frame; not stream: error body).
- [ ] **Step 3:** Run `cd core && GOWORK=off go test ./translate/anthropic/` → FAIL.
- [ ] **Step 4:** Implement with structs of fixed field order (no `map[string]any` for output).
- [ ] **Step 5:** Run → PASS. Commit `feat: Add the anthropic-messages translation protocol`.

### Task T3: The chat protocol

**Depends on:** T1. **Estimate:** 870 lines. **Rules:** 9a, 11-24.

**Files:**
- Create: `core/translate/chat/protocol.go` (`type Protocol struct{}`, `Name()` = `"chat-completions"`), `request.go`, `reply.go`, `error.go`
- Test: `core/translate/chat/request_test.go`, `reply_test.go`

**Interfaces:** Consumes T1. Produces `chat.Protocol` implementing `pivot.Protocol`, `pivot.RequestEncoder`, `pivot.ReplyDecoding`.

- `EncodeRequest(req, h)`: deletes `Anthropic-Version` and `Anthropic-Beta` from `h` (nil `h` is valid). Top-level fields only from rule 11, in that order: `model`, `messages`, `max_tokens`, `stream`, `stream_options`, `tools`, `tool_choice`, `parallel_tool_calls`, `stop`, `temperature`, `top_p`, `reasoning_effort` (all `omitempty` except `model` and `messages`). Rules 12-19 and decisions 8-11 of this plan. Refusals return `*pivot.RequestError{400, "invalid_request_error", msg}`: document → `this inference server cannot read documents (PDF)`; server tool → `this inference server cannot run the server tool "<name>" (type <type>)`. Effort map: `low`, `medium`, `high` stay; any other non-empty value → `high`; else `Thinking.Type` `enabled`/`adaptive` → `medium`; else none.
- `NewReplyDecoder(status, stream)`:
  - 2xx, `stream`: SSE line parser that keeps a partial line between `Feed` calls; skips lines that start with `:`; reads `data:` payloads; rule 23 and decisions 5-6. `EventStart` at the first chunk (chunk `id`, `model`). Block index counts from 0 in emit order. `finish_reason` maps by rule 21 (unknown value → `end_turn`) and closes the open block. Usage by rule 22. A chunk `{"error":{…}}` → `EventError` (status 500 when the chunk has none, type from `error.type` when it is an Anthropic type, else `api_error`). `provider_specific_fields` is never read.
  - 2xx, not `stream`: buffers the JSON body (a not-streamed body is one value) and at `End()` emits the same events as a stream would, by rule 20; invalid tool `arguments` → only `EventError{502, "api_error", "the inference server sent tool <name> arguments that are not valid JSON"}`.
  - non-2xx: buffers at most 1 MiB; `End()` emits one `EventError{status, pivot.ErrorType(status), message}`. Message: `error.message` when the body is JSON with one; else the body text, trimmed, first 512 bytes; else `http.StatusText(status)`.
- `EncodeError` → `{"error":{"message":m,"type":t,"code":null}}`.

- [ ] **Step 1:** Write `TestEncodeRequest_Shape` (a hand-built `pivot.Request` with one of each block kind except document; assert exact bytes as a literal), `TestEncodeRequest_RefusesDocument`, `TestEncodeRequest_RefusesServerTool`, `TestEncodeRequest_DeletesAnthropicHeaders`, `TestEncodeRequest_Effort` (table: `low`→`low`, `max`→`high`, none + `enabled`→`medium`, none + `disabled`→ field absent).
- [ ] **Step 2:** Write `TestReplyDecoder_BobShape1` (the issue's first stream, as `data:` lines with a `: ping` comment line; assert the event list), `TestReplyDecoder_EndsEarly` (no `finish_reason` → last event is `EventError` 502), `TestReplyDecoder_ClosedIndexPiece`, `TestReplyDecoder_HTMLErrorPage` (status 502, body `<html>bad gateway</html>` → message `<html>bad gateway</html>`, type `api_error`), `TestReplyDecoder_InvalidArgumentsNotStream`.
- [ ] **Step 3:** Run `cd core && GOWORK=off go test ./translate/chat/` → FAIL. Implement. Run → PASS.
- [ ] **Step 4:** Commit `feat: Add the chat-completions translation protocol`.

---

## PR P2

### Task T4: Registry, helpers and cross-protocol goldens

**Depends on:** T2, T3. **Estimate:** 550 lines. **Rules:** H5, H6, 9a, 10, 11-24, and the issue's "Golden request tests" and "Golden reply tests".

**Files:**
- Create: `core/translate/registry.go`, `convert.go`, `reader.go`
- Test: `core/translate/golden_test.go`, `core/translate/reader_test.go`, `core/translate/testdata/request/*.json` (hand-written Anthropic inputs), `testdata/request/*.golden`, `testdata/reply/*.sse|*.json` (hand-written chat inputs), `testdata/reply/*.golden`

**Interfaces (Produces):**

```go
package translate
const ( Anthropic = "anthropic-messages"; Chat = "chat-completions"; Responses = "responses" )
func Lookup(name string) (pivot.Protocol, bool) // compiled-in map; responses is an unexported type with only Name and EncodeError (OpenAI error shape {"error":{"message","type","code":null}})
func Names() []string                            // sorted
func Decodes(name string) bool                   // has pivot.RequestDecoder
func Target(served []string) (string, bool)      // decision 12
type Converted struct { Body []byte; Dropped []string; Stream bool } // Stream: the agent asked for a stream
func Request(source, target string, body []byte, h http.Header) (Converted, error) // *pivot.RequestError for a refusal
func Reply(source, target string, status int, upstreamStream, agentStream bool, body []byte) []byte // whole-body conversion
func ErrorBody(protocol string, status int, errType, message string) []byte
func NewReplyReader(body io.ReadCloser, dec pivot.ReplyDecoder, enc pivot.ReplyEncoder) io.ReadCloser
```

`NewReplyReader`: each `Read` reads at most 32 KiB from `body`, feeds it, encodes each event, and returns the bytes; it returns as soon as it has bytes (never waits for the whole stream). At upstream EOF it calls `dec.End()` and `enc.End()`. `Close` closes `body`. An upstream read error other than EOF is returned after the bytes already converted.

- [ ] **Step 1:** Write `TestNames` (`[anthropic-messages chat-completions responses]`), `TestTarget` (`[responses]` → false; `[responses chat-completions]` → `chat-completions`; `[anthropic-messages responses]` → false), `TestDecodes`.
- [ ] **Step 2:** Write the request goldens: one input per rule 11, 12, 13, 13a, 14 (each bullet, with a `tool_result` that holds an image), 15, 16, 17, 18, 19 and 20a (`call_3833__thought__AY89a1+8zd/x=` id in a `tool_use` and its `tool_result`), and `full_multiturn.json` (text, image, tool_use, tool_result, thinking with signature, `cache_control` on system and a message, a mid-conversation `system` message). The test runs `Request(Anthropic, Chat, in, nil)` and compares the body to `<name>.golden` byte for byte, and `Dropped` to the golden's first line. With `-update` it writes the golden.
- [ ] **Step 3:** Write `TestDeterministic` (rule 9a): encode `full_multiturn.json` twice → equal bytes; append one user turn → old output is a byte prefix up to the end of the old last message; move `cache_control` from the last message to the new message → the two outputs differ only in the marker bytes (compare after removing `,"cache_control":{"type":"ephemeral"}` from both).
- [ ] **Step 4:** Write the reply goldens for the issue's three Bob shapes, `stream_two_tools`, and errors (403 LiteLLM `team_model_access_denied`, 404 Bob `not_found_error`, 429, an in-stream `{"error":…}` chunk), each streamed and not streamed: `Reply(Anthropic, Chat, …)` vs golden. Shape 3: `output_tokens` 296 and no thinking block. Shape 2: a thinking block with empty text and its `signature_delta`.
- [ ] **Step 5:** Write `TestReplyReader_EveryByteSplit`: for each streamed reply input, feed it through `NewReplyReader` over a reader that returns one byte per `Read`; the output equals the golden. Write `TestReplyReader_Incremental`: an `io.Pipe` upstream; after writing the first chunk only, a `Read` returns the `message_start` frame.
- [ ] **Step 6:** Run `cd core && GOWORK=off go test ./translate/... -update`, read every golden against the issue rules, then run without `-update` → PASS. Commit `feat: Add the translation registry and golden tests`.

### Task T6: The pipeline calls

**Depends on:** none. **Estimate:** 250 lines. **Rules:** 5, C3 (apply-after-plugins half).

**Files:**
- Modify: `core/pipeline/destination.go` (after `RedirectPath`), `core/pipeline/context.go` (private fields beside `redirectPath`; reset them wherever `redirectPath` is reset)
- Test: `core/pipeline/translate_test.go`

**Interfaces (Produces):**

```go
// Translate asks the listener to convert this redirected request from source to
// target and send it to path, with no query string. It does not change Path.
// Gates: as SetRedirectPath (OnRequest only, WritesDestination, a Redirect took
// effect, path absolute with no query, fragment or ".." segment), plus source and
// target non-empty and different. Records nothing: the listener records
// modify/translated. A second call replaces the first.
func (c *Context) Translate(source, target, path string) error
func (c *Context) TranslateTarget() (source, target, path string, ok bool) // ok only when Redirected and Translate was accepted
// SetUpstreamHeaders gives headers that the listener sets on the upstream request
// after every plugin has run, replacing a header of the same name. Same gates as
// SetRedirectPath without the path. Keeps a clone.
func (c *Context) SetUpstreamHeaders(h http.Header) error
func (c *Context) UpstreamHeaders() http.Header // nil unless Redirected
```

- [ ] **Step 1:** Write tests, following the setup of the existing `SetRedirectPath` tests in `core/pipeline/resend_test.go`: accepted after a Redirect (Path unchanged, `TranslateTarget` ok); refused before a Redirect, without WritesDestination, outside OnRequest, under observe (no Redirect took effect), with `path` `v1/x`, `/a?b`, `/a/../b`, with `source == target`; `SetUpstreamHeaders` stores a clone (a later change to the caller's map does not show).
- [ ] **Step 2:** Run `cd core && GOWORK=off go test ./pipeline/ -run 'Translate|UpstreamHeaders'` → FAIL. Implement. Run → PASS. Commit `feat: Add pipeline calls for translation and fixed upstream headers`.

### Task T8: The forward-proxy layer

**Depends on:** T4, T6. **Estimate:** 660 lines. **Rules:** 5, 8, 9, 10, C3 (apply), decisions 4, 17, 18, 19.

**Files:**
- Create: `core/listener/forwardproxy/translate.go`
- Modify: `core/listener/forwardproxy/server.go` (`serveOutbound`, `resendIfAsked`)
- Test: `core/listener/forwardproxy/translate_test.go`

**Behavior (in `serveOutbound`, in this order):**
1. After the redirect-apply block and the `Reject` return: if `pctx.TranslateTarget()` is ok, call `translate.Request(src, tgt, pctx.Body, pctx.Headers)`. `pctx.Body == nil` → treat as a 502 `api_error` "the request body was not buffered, so Cortex cannot translate it". A `*pivot.RequestError` or any other error (502 `api_error`) → a `pipeline.Reject` action with `Violation{Code: "inference.translation_refused", Status, Body: translate.ErrorBody(src, …), BodyType: "application/json"}`, recorded and written the way the existing reject branch does (`recordOutboundRejectIn` + `httpx.WriteRejectionForRequest`); nothing goes upstream. On success record `pipeline.Invocation{Plugin: "forward-proxy", Action: ActionModify, Reason: "translated", Details: {"from": src, "to": tgt, "path": path}}`, plus `"dropped": strings.Join(dropped, ",")` when not empty. Keep `Converted` in a local.
2. After `httpx.SyncRequestHeaders`: set each `pctx.UpstreamHeaders()` header on `r.Header` (replace). This applies to translated and not-translated requests.
3. After the `BodyMutated` block, for a translated request: `httpx.SetRequestBody(r, conv.Body)`; `r.URL.Path = path`; `r.URL.RawPath, r.URL.RawQuery, r.URL.ForceQuery = "", "", false`. `pctx.Path` is not changed.
4. `ownEncoding` is true for a translated request, so the transport decodes gzip.
5. In `resendIfAsked`, for a translated request: re-run `translate.Request` on `pctx.Body` and send `conv.Body` (on error, return the current answer unchanged); re-apply upstream headers after `SyncRequestHeaders`; the refused attempt row's `pctx.ResponseBody` is `translate.Reply(...)` of `head` (decision 19). Pass the translation through a small struct parameter or a field on a per-request value, not through `pctx`.
6. After the resend loop and the error branch, before `pctx.StatusCode = resp.StatusCode`, for a translated request: `dec := target.NewReplyDecoder(resp.StatusCode, httpx.IsEventStream(ct))`, `enc := source.NewReplyEncoder(conv.Stream && resp.StatusCode/100 == 2)`, `resp.Body = translate.NewReplyReader(resp.Body, dec, enc)`; set `Content-Type` to `text/event-stream` (stream) or `application/json`; delete `Content-Length` and `Content-Encoding`; `resp.ContentLength = -1`. Every later arm (buffered, streaming responders, relay) then reads Anthropic bytes.

**Tests** (use the setup of `redirect_test.go`: a test plugin with `WritesDestination` that calls `Redirect` to an `httptest` server, then `Translate("anthropic-messages", "chat-completions", "/v1/chat/completions")`, and optionally `SetUpstreamHeaders`; include the inference-parser plugin where a test checks the session store):
- `TestTranslate_StreamedReplyRecordedAndIncremental`: upstream streams the issue's shape 1 and blocks after the first two chunks; the client reads `message_start` before the upstream finishes; after the end, the session store's response event has the Anthropic inference (text `391 is not prime`, tool `Bash`), and the request event has a `modify/translated` invocation with `from`, `to`, `path`.
- `TestTranslate_NoQueryString`: client `POST /v1/messages?beta=true` → upstream sees path `/v1/chat/completions` and empty `RawQuery`; the recorded request row path is `/v1/messages`.
- `TestTranslate_UpstreamHeadersReplace`: client sends `User-Agent: claude-cli/2`; `SetUpstreamHeaders({"User-Agent": ["bob-shell"]})` → upstream sees only `bob-shell`.
- `TestTranslate_GzipUpstream`: upstream gzips when the request has `Accept-Encoding: gzip`; client gets valid Anthropic JSON.
- `TestTranslate_AgentStreamsServerAnswersJSON`: agent `stream: true`, upstream answers `application/json` → client gets SSE.
- `TestTranslate_ErrorStatus`: upstream 429 → client 429 `rate_limit_error` body.
- `TestTranslate_DocumentRefused`: a request with a document → client 400 `invalid_request_error`; the upstream got no request.
- `TestTranslate_DroppedRecorded`: `top_k` and `metadata` → `dropped` `metadata,top_k`.
- `TestTranslate_ResendSeesRawReply`: a test `Resender` records the head it got; upstream answers 403 LiteLLM `team_model_access_denied` first, then 200; the Resender saw `team_model_access_denied`; the second upstream body is chat-shaped.
- `TestTranslate_NotTranslatedPassesBytes`: Redirect only, no Translate → upstream body equals the client body byte for byte.

- [ ] **Step 1:** Write the tests. Run `cd core && GOWORK=off go test ./listener/forwardproxy/ -run TestTranslate` → FAIL.
- [ ] **Step 2:** Implement `translate.go` helpers and the `server.go` edits. Run → PASS. Run the whole `./listener/...` → PASS. Commit `feat: Translate requests and replies in the forward proxy`.

### Task T9: Raw capture of the translated hop

**Depends on:** T8. **Estimate:** 400 lines. **Rules:** 8a, decision 20.

**Files:**
- Modify: `core/model/capture.go` (fields of decision 20), `core/listener/forwardproxy/server.go` and `translate.go` (fill them), `core/session/archive/capture.go` (write and read the new bodies the way `Forwarded` and the response `Body` are written, with `splitBody`/`rebuild`), `core/session/archive/archive.go` (`captureSize`: count the new bodies in the size), `core/sessionapi/raw.go` (`textOr64` for both), `core/sessionapi/ingest.go` (accept `translated` and the response `upstream` and carry them to `model.RequestCapture`), `cmd/agentop/tui/detail_raw.go` (labels below)
- Test: `core/listener/forwardproxy/translate_test.go` (`TestTranslate_CaptureHoldsFourBodies`), `core/session/archive/capture_test.go`, `core/sessionapi/archive_test.go` (beside `TestHandleGetEvent_RawAddsTheCapture`), `core/sessionapi/ingest_test.go`, `cmd/agentop/tui/detail_raw_test.go`

Behavior: `capture.Translated = conv.Body` before the request row is recorded. For the reply, wrap the upstream body **before** `NewReplyReader` with `bodycount.WrapCapture(body, &unused, &rc.Upstream, &rc.UpstreamTruncated, &ended, pipeline.MaxResponseCapture)`; the present `rc.Body` stays the converted reply as the agent got it. agentop labels: `translated` → `sent upstream, translated by Cortex: <size>` with body key `translatedBody`; `upstream` → `server's reply before translation: <size>` with body key `upstreamBody`. The `forwarded` label ("rewritten by a plugin") does not change.

- [ ] **Step 1:** Write `TestTranslate_CaptureHoldsFourBodies` (a plugin that changes the body, so `Forwarded` is set; assert `Original`, `Forwarded`, `Translated` chat body, `Response.Body` Anthropic, `Response.Upstream` chat SSE), an archive round-trip test with both new fields, a raw GET test (`translated` text; a non-UTF-8 `upstream` → `upstreamBase64`), and an agentop render test for both labels.
- [ ] **Step 2:** Run the four tests → FAIL. Implement. Run `cd core && GOWORK=off go test ./listener/... ./session/... ./sessionapi/... ./model/...` and `cd cmd/agentop && GOWORK=off go test ./tui/` → PASS. Commit `feat: Capture the translated request and the raw server reply`.

---

## PR P3

### Task T5: Server config fields and header redaction

**Depends on:** none. **Estimate:** 450 lines. **Rules:** C1 (validation), C2 (values), C3 (validation, redaction), C2a.

**Files:**
- Modify: `core/plugins/inferencerouter/routerconfig/config.go`, `core/redact/redact.go`
- Test: `core/plugins/inferencerouter/routerconfig/config_test.go`, `core/redact/redact_test.go`

**Interfaces (Produces):**

```go
// in routerconfig.Server, after Helper:
API        map[string]string `json:"api" description:"Protocols the server serves, by protocol name, to the path it serves each at. Empty: requests go in the agent's protocol, to the agent's path."`
AuthHeader string            `json:"auth_header" description:"Header that carries key. authorization sends Bearer <key>; any other name sends the raw key. Empty: the agent's header, or the target protocol's usual one for a translated request."`
Headers    map[string]string `json:"headers" description:"Fixed headers set on every request to this server after the plugins run. Values may use ${ENV_VAR}."`

var APIProtocols = []string{"anthropic-messages", "chat-completions", "responses"}
func CheckAPI(api map[string]string) error        // keys sorted for a stable first error
func CheckAuthHeader(name string) error
func CheckHeaders(h map[string]string, authHeader string) error
```

Errors (prefix `servers.<name>.<field>` added by `Validate`):
- unknown key: `unknown protocol "<k>"; valid protocols: anthropic-messages, chat-completions, responses`
- bad path: `api.<k>: path "<p>" must be absolute, with no query, fragment or ".." segment`
- `auth_header`: must be a valid header token; refused names (case-insensitive): `Host`, `Content-Length`, and the hop-by-hop set `Connection`, `Keep-Alive`, `Proxy-Connection`, `Transfer-Encoding`, `Upgrade`, `TE`, `Trailer`, `Proxy-Authenticate`, `Proxy-Authorization`.
- `headers`: refused names = the `auth_header` refused set plus `Authorization`, `X-Api-Key`, `Api-Key`, and the server's own `auth_header`; a value with CR, LF or NUL is refused; names must be valid tokens. Error: `headers: "<name>" cannot be set here: <reason>` (reason "Cortex sets it" for auth headers, "it is a hop-by-hop or framing header" otherwise).

`redact.redactMap`: when a key equals `headers` exactly (case-sensitive) and the value is an object, replace each value in it with `"[REDACTED]"` and keep the names. Do not add `headers` to `sensitiveKeys`.

- [ ] **Step 1:** Write tests: `api` valid (`{chat-completions: /v1/chat/completions}`, DeepSeek's two entries, `{responses: /v1/responses}`), unknown key error lists the valid keys, relative path refused, `?` refused; `auth_header` `authorization`, `x-api-key`, `api-key` valid, `Host` and `Connection` refused; `headers` `User-Agent: bob-shell` valid, `Authorization`, `X-Api-Key`, `Content-Length`, `Transfer-Encoding` refused, a value with `\n` refused; a strict decode of a config with all three fields succeeds. Redact, through `redact.JSON` (the function that `/config` in `core/observe/statserver.go` and `/v1/pipeline` in `core/sessionapi/server.go` call): `{"servers":{"bob":{"headers":{"User-Agent":"bob-shell"}}},"session":{"id_headers":["X-Session"]}}` → `User-Agent` value `[REDACTED]`, `id_headers` unchanged; `delegated_credential_headers` and `remove_headers` unchanged.
- [ ] **Step 2:** Run `cd core && GOWORK=off go test ./plugins/inferencerouter/routerconfig/ ./redact/` → FAIL. Implement (the path rule is the same as `SetRedirectPath`'s). Run → PASS. Commit `feat: Add the api, auth_header and headers server fields`.

### Task T7: Router rules

**Depends on:** T4, T5, T6, T8. **Estimate:** 600 lines. **Rules:** 4, 4a, 5, 6, 7, 9, C1 (routing), C2, C3 (handing over), decisions 12-15.

**Files:**
- Modify: `core/parse/inference/dialect.go` (add `ProtocolFor`), `core/plugins/inferencerouter/plugin.go` (`route` gets `api map[string]string`, `authHeader string`, `headers http.Header`; `Configure` fills them; `OnRequest` uses the plan; `setKey` call replaced by `setAuth`)
- Create: `core/plugins/inferencerouter/api.go`
- Test: `core/parse/inference/dialect_test.go`, `core/plugins/inferencerouter/api_test.go`, `core/listener/forwardproxy/inferencerouter_test.go` (end-to-end cases)

**Interfaces:**

```go
// core/parse/inference
func ProtocolFor(path string) string // decision 14

// core/plugins/inferencerouter/api.go
type planKind int
const ( planAsIs planKind = iota; planPath; planTranslate; planRefuse; planCountTokens )
type apiPlan struct { kind planKind; source, target, path string }
func (r route) plan(path string) apiPlan
func setAuth(pctx *pipeline.Context, r route, translatedTo string) // translatedTo "" when not translated
```

`plan` rules, in this order: (1) empty `api` → `planAsIs`; (2) path ends in `/v1/messages/count_tokens` and `api` has no `anthropic-messages` → `planCountTokens`; (3) `src := ProtocolFor(path)`; `api[src]` set → `planPath` with that path; (4) `translate.Decodes(src)` → `translate.Target(keys)`: ok → `planTranslate{src, tgt, api[tgt]}`, else `planRefuse{src}`; (5) else `planAsIs`.

`OnRequest`, once `redirected` is true: compute the plan before `chooseModel`. `planPath` → `SetRedirectPath(plan.path)` in place of `SetRedirectPath(captured)`. `planTranslate` → `pctx.Translate(...)`, and no `SetRedirectPath`. `planRefuse` → record `pipeline.Invocation{Action: ActionDeny, Reason: "cannot_translate", Details: details()+{"from": src}}` and return `pipeline.Action{Type: pipeline.Reject, Violation: &pipeline.Violation{Code: "inference.translation_unavailable", Reason: msg, Status: 502, Body: translate.ErrorBody(src, 502, "api_error", msg), BodyType: "application/json"}}` with the decision 13 message. `planCountTokens` → record `ActionDeny` reason `count_tokens_local`, reject with status 404 and body `translate.ErrorBody("anthropic-messages", 404, "not_found_error", "this inference server does not count tokens")`. An error from `Translate` or `SetRedirectPath` takes the existing `redirect_failed` branch. Then `setAuth`: `authHeader` set → delete `Authorization`, `X-Api-Key`, `Api-Key`, then `authorization` → `Authorization: Bearer <key>`, any other name → that header = key. `authHeader` empty and translated → delete the three, then `chat-completions` → `Authorization: Bearer <key>`, `anthropic-messages` → `X-Api-Key: <key>`. Else `setKey` as today. Last: when `len(headers) > 0`, `pctx.SetUpstreamHeaders(headers)`.

- [ ] **Step 1:** Write `TestProtocolFor` (`/v1/messages` → anthropic-messages, `/anthropic/v1/messages` → anthropic-messages, `/v1/chat/completions` → chat-completions, `/v1/completions` → "", `/v1/responses` → responses, `/v1/messages/count_tokens` → "").
- [ ] **Step 2:** Write `TestAPIProtocolsMatchRegistry` (`routerconfig.APIProtocols` equals `translate.Names()`) and `TestPlan` table: empty map; `{chat-completions}` + `/v1/messages` → translate to `/v1/chat/completions`; `{anthropic-messages: /anthropic/v1/messages}` + `/v1/messages` → path; `{responses}` + `/v1/messages` → refuse; `{anthropic-messages, responses}` + `/v1/chat/completions` → as is (rule 7); `{chat-completions}` + `/v1/messages/count_tokens` → count tokens; `{anthropic-messages}` + `/v1/messages/count_tokens` → as is.
- [ ] **Step 3:** Write plugin tests in `api_test.go` with the existing `plugin_test.go` setup: `setAuth` cases (rule C2 table: `X-Api-Key` agent + empty `auth_header` + not translated → `setKey` behavior unchanged; translated to chat → only `Authorization: Bearer`; `auth_header: x-api-key` → only `X-Api-Key`; `auth_header: api-key` → only `Api-Key`); translated request leaves `pctx.Path` unchanged and `RedirectPath` not ok; `headers` reach `UpstreamHeaders`.
- [ ] **Step 4:** Write end-to-end tests in `core/listener/forwardproxy/inferencerouter_test.go` (its existing router setup): Claude Code `X-Api-Key` request to a `{chat-completions}` server → upstream has only `Authorization: Bearer <key>` and a chat body; `{responses: /v1/responses}` → client 502 Anthropic `api_error` with the decision 13 text, upstream got nothing; `{anthropic-messages: /anthropic/v1/messages}` → upstream path `/anthropic/v1/messages`, body unchanged; a chat-completions request to a `{anthropic-messages: …}` server → not translated, no 502; `count_tokens` routed to a chat server → client 404 `not_found_error`, upstream got nothing; translated request + LiteLLM 403 `team_model_access_denied` → the router resends with its substitute model; Bob 404 `model "claude-opus-5-5" not found or not accessible` → resend as today.
- [ ] **Step 5:** Run `cd core && GOWORK=off go test ./parse/... ./plugins/inferencerouter/... ./listener/forwardproxy/` → FAIL. Implement. Run → PASS. Commit `feat: Route requests by the server api map and translate them`.

### Task T10: agentop flags and display

**Depends on:** T5. **Estimate:** 340 lines. **Rules:** C4.

**Files:**
- Modify: `cmd/agentop/cmd_server_write.go` (`serverAdd`), `cmd/agentop/cmd_server.go` (`serverList`), `cmd/agentop/README.md`
- Test: `cmd/agentop/cmd_server_write_test.go`, `cmd/agentop/cmd_server_test.go`

Flags on `server add`: `--api <protocol>=<path>` (repeatable, a `flag.Value` that fills a map; a second value for the same protocol replaces the first), `--auth-header <name>`, `--header <name>=<value>` (repeatable). Check them with `routerconfig.CheckAPI`, `CheckAuthHeader`, `CheckHeaders` before the key is read, like `checkWordFlags`. `--header` with a `$` in the value is refused with: `--header <name>: the value contains $, which Cortex reads as an environment variable when it loads the config; write headers: {<name>: ${NAME}} by hand`. A flag value with no `=` is refused. Write the fields into the server. `agentop server` shows, for each server that has them, lines `api: chat-completions=/v1/chat/completions` (sorted by protocol), `auth header: x-api-key`, `headers: User-Agent` (names only, sorted). The README lists the three flags.

- [ ] **Step 1:** Write tests: `--api chat-completions=/v1/chat/completions --auth-header x-api-key --header User-Agent=bob-shell` writes the three fields; `--api bogus=/x` fails with exit 2 and the valid-keys text, and reads no key; `--header X-A=$HOME` fails; `--header Authorization=x` fails; `server` list shows the names and not the value `bob-shell`.
- [ ] **Step 2:** Run `scripts/dev/go-test-shards.sh -short cmd/agentop` → FAIL. Implement. Run → PASS. Commit `feat: Add --api, --auth-header and --header to agentop server add`.

### Task T11: Docs

**Depends on:** none. **Estimate:** 120 lines. **Rules:** F1, C1-C3.

**Files:**
- Create: `docs/translation-server.md`
- Modify: `docs/inference-router-plugin.md` (a section "Servers that serve another protocol"), `docs/README.md` (index row for the new page)

Facts for `docs/translation-server.md`:
- When: Cortex translates only Anthropic Messages to chat completions. For any other pair, Cortex answers 502 "Cortex cannot translate <source> to <target> yet". Run a translation server on loopback for that pair.
- LiteLLM config that worked (2026-11-16):
  ```yaml
  model_list:
    - model_name: <model>
      litellm_params:
        model: hosted_vllm/<model>
        api_base: <server base URL, for example https://host/v1>
        api_key: os.environ/UPSTREAM_API_KEY
        additional_drop_params: ["prompt_cache_key"]
  general_settings:
    master_key: os.environ/LITELLM_MASTER_KEY
  ```
  Use `hosted_vllm/`, not `openai/`: LiteLLM's `openai/` provider sent requests to `/v1/responses`, which gave 403. Run `litellm --config litellm.yaml --host 127.0.0.1 --port 4000`. Bind to loopback only and always set a master key.
- Add it: `agentop server add --main <word> --helper <word> --api anthropic-messages=/v1/messages <name> http://127.0.0.1:4000`, and give the master key as the server key (prompt or `--key-stdin`). With `anthropic-messages` in `api`, Cortex sends Claude Code's requests to LiteLLM untranslated.
- Cortex records only the agent's side of the hop to the translation server. The translation server's own cost and errors are its own, and Cortex does not see them.

Facts for `docs/inference-router-plugin.md`: the three fields, their descriptions from T5, the valid `api` keys, the DeepSeek example `{chat-completions: /chat/completions, anthropic-messages: /anthropic/v1/messages}`, rules 4 (when Cortex translates), 4a (502), 6 (`count_tokens` 404), 7, C2's header choice table, C3's refused names and that `/config` and `/v1/pipeline` hide the header values, C2a (the model-list fetch still sends Bearer only), and the new `modify/translated` record with `from`, `to`, `path`, `dropped`. Link `translation-server.md`.

- [ ] **Step 1:** Write the pages. Run `pre-commit run --files docs/translation-server.md docs/inference-router-plugin.md docs/README.md` → PASS. Commit `docs: Add the translation server page and the router's api fields`.

---

## For a human after review

These steps need a live agent, a real server or a person's decision. They are not tasks in this workflow.

1. **Needs T1-T11 (all PRs).** Run Claude Code (`claude -p`, with tools) against a chat-completions server through the router for at least 3 tool-call turns. A local Ollama or llama-server works when its `api` map has only `chat-completions` (`agentop server add --api chat-completions=/v1/chat/completions …`). Put the session id and its turn count in the PR P3 body.
2. **Needs T7.** Check that Claude Code's `/context` still works with the local 404 for `count_tokens` (rule 6). If it does not, report what Claude Code does. Do not guess a token count.
3. **Needs T3, T8 (and #1912 for Bob).** Check that Claude Code accepts a `__thought__` tool id with `+` or `/` and sends it back unchanged. If Claude Code refuses or changes it, report what it does. Do not encode the id without a decision on that report.
4. **Needs T3.** On Bob (#1912's list), check the rule 11 fields not yet tested there (`tool_choice`, `parallel_tool_calls`, `stop`, `temperature`, `top_p`), and that Bob accepts `role: tool` messages whose `content` is a list of text parts (decision 8).
5. **Needs P1-P3 opened.** Post nothing outside the repository except the PRs themselves; issue comments on #1911 or #1910 are a person's call.
