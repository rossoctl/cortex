# Inference parser cases from real traffic

`TestFixtures` (`../fixtures_test.go`) runs every `<agent>/<case>/` directory here
through the parser hooks. It compares the result with `expected.json`.

Each case has these files:

| File | What it holds |
|---|---|
| `request.headers` | The HTTP request line and the headers the parser reads. The test reads it with `http.ReadRequest` |
| `request.json` | The request body, compact, as the client sent it |
| `response.sse`, `response.json` or `response.html` | The response body. The extension sets the Content-Type: `text/event-stream`, `application/json` or `text/html` |
| `response.headers` | Optional. The response headers that the parser reads: `Content-Type` and `X-Litellm-Model-Name`. Without it, the test sets only the Content-Type from the body file's extension |
| `expected.json` | The `pipeline.InferenceExtension` that the parser makes, as indented JSON |

The test parses each case two times: with the whole response body (`OnResponse`), and
frame by frame (`OnResponseFrame`, one call for each SSE event). The two results must be
equal.

## Capture and scrub procedure

The captures come from a laptop Cortex with the session archive on. Only the archive
keeps raw bytes, so only events after #1494 have them.

1. List the sessions, with the archived ones:

   ```sh
   API=http://127.0.0.1:47601
   curl -s --noproxy '*' "$API/v1/sessions?archived=true&limit=500" \
     | jq -r '.sessions[] | [.id, .agent, .updatedAt] | @tsv'
   ```

2. List the events of one session. Pair a request and its response by `requestId`:

   ```sh
   curl -s --noproxy '*' "$API/v1/sessions/$SID?view=summary&limit=500" \
     | jq -r '.events[] | [.seq, .phase, .requestId, .statusCode, .httpPath,
         .inference.model, .inference.stream, .inference.finishReason,
         .inference.messageCount, .inference.toolCount] | @tsv'
   ```

3. Get the two events with their raw bytes. Keep them outside the repository:

   ```sh
   curl -s --noproxy '*' "$API/v1/sessions/$SID/events/$REQ?raw=1" > /tmp/req.json
   curl -s --noproxy '*' "$API/v1/sessions/$SID/events/$RESP?raw=1" > /tmp/resp.json
   jq -r '.raw.request' /tmp/req.json > /tmp/request.raw.json     # not .raw.forwarded
   jq '.raw.requestHeaders, .raw.query' /tmp/req.json
   jq -r '.raw.response.body' /tmp/resp.json > /tmp/response.raw
   jq '.raw.response | {status, ct: .headers["Content-Type"], incomplete}' /tmp/resp.json
   ```

   Use `raw.request`, not `raw.forwarded`. The parser reads the body before a plugin
   changes it.

4. Before the scrub, write down the facts of the live response event:

   ```sh
   jq '.inference | {model, stream, streamedResponse, finishReason, agentRole,
         messages: (.messages | length), tools: (.tools | length),
         toolCalls: [.toolCalls[]?.name], promptTokens, completionTokens,
         totalTokens, inputTokens, cacheReadTokens, cacheWriteTokens,
         outputTokens, reasoningTokens, presentKinds}' /tmp/resp.json
   ```

5. Scrub the request body. Keep the JSON structure, field names, block `type`s, roles,
   model ids, `cache_control` markers and the numbers that are not tool input.
   Replace these:
   - Every text block, string content, thinking text and tool result: `<role> text N`,
     `thinking text`, `tool result text N`. An empty thinking text stays empty.
   - Thinking `signature`: `c2lnbmF0dXJl`.
   - The first system block is Claude Code's billing line. Keep only
     `x-anthropic-billing-header: cc_version=2.1.295.000; cc_entrypoint=cli;` and add
     ` cc_is_subagent=true;` when the original has it.
   - Tool use `id` and `tool_use_id`: `toolu_fixture_NN`. Use the same map for the
     request and the response.
   - Tool `input`: keep the keys. A string becomes `value`, a number becomes `1`.
   - Tool definitions: keep built-in names. An `mcp__*` name becomes
     `mcp__example__tool_N`. The description becomes `Tool description.`. In
     `input_schema`, keep the keys and types. Replace `description` and `title` with
     `Parameter description.`, `$schema` with `https://json-schema.example.com/schema`,
     the `default`, `examples`, `const` and `pattern` values as tool input, and each
     string in `enum` with `option_N`.
   - `metadata.user_id`: a fake JSON string with zero UUIDs.
   - `stop_sequences`: `</stop>`.

6. Scrub the response the same way. In SSE, keep every event, its name and its order.
   - `message.id`: `msg_fixture`.
   - `thinking_delta`: `thinking text`. `signature_delta`: `c2lnbmF0dXJl`.
     `text_delta`: `word `. Empty values stay empty.
   - `input_json_delta`: join the fragments of one block, scrub the JSON as tool input,
     and cut it into the same number of fragments.

7. Cut a long history to the first message and the last 8 or so, from an assistant
   message on. The request then stays valid: each `tool_result` follows its `tool_use`.

8. Write `request.headers` with the request line and only these headers:
   `Host: api.example.com`, `Content-Type`, `Anthropic-Beta`, `Anthropic-Version`,
   `User-Agent`, and `X-Claude-Code-Session-Id: 00000000-0000-4000-8000-000000000000`.
   To cover `servedModel`, also write `response.headers` with `Content-Type` and
   `X-Litellm-Model-Name: gateway/<model>`. Never keep the real value: it names the
   upstream provider.

   Keep the full tool manifest in `main-stream-thinking-tool-use` only. In each other
   case, cut `tools` to the tools that the history or the response calls, and one
   `mcp__example__tool_N`.

9. Write the goldens, then read each one:

   ```sh
   cd core && GOWORK=off go test ./plugins/inferenceparser/ -run Fixture -count=1 -update
   ```

10. Compare `expected.json` with the facts from step 4. Only these facts can differ:
    `contentBytes`, thinking `signatureBytes` and `textBytes` (the placeholders are
    shorter), `servedModel` where the case has no `response.headers` (the live value
    comes from that header), the tool count of a case with tools cut, and the message,
    tool result and history counts of a case with history cut.

11. Search the case for leaks before you commit it: paths under `/Users`, host names,
    e-mail addresses, `sk-`, `Bearer`, and long natural-language strings.

## Cases

All cases are Claude Code 2.1.295 on the Anthropic Messages dialect, through a LiteLLM
gateway, captured on 2026-11-06.

| Case | What it covers | Cut |
|---|---|---|
| `claude-code/main-stream-thinking-tool-use` | Main loop, streamed. Adaptive thinking, one `Bash` call. The history has thinking, `tool_use`, `tool_result` and mid-conversation system messages. Full tool manifest. `response.headers` gives `servedModel` | No |
| `claude-code/main-first-turn-parallel-tool-use` | First main-loop turn, streamed. Two parallel `Skill` calls | Tools cut |
| `claude-code/main-nonstream-tool-use` | Main loop, NOT streamed (`stream: false`). JSON body with thinking, text and `tool_use` | 547 to 10 messages, tools cut |
| `claude-code/main-stream-client-canceled` | Main loop. The client canceled the stream after the thinking block started: no `message_delta`, no stop reason | 394 to 9 messages, tools cut |
| `claude-code/subagent-stream-end-turn` | Subagent (`cc_is_subagent=true`), streamed text, `end_turn`. History has `tool_use` and `tool_result` | Tools cut |
| `claude-code/subagent-stream-canceled-in-tool-call` | Subagent. Canceled after a `SubagentHandback` tool block started, with empty input | 185 to 10 messages, tools cut |
| `claude-code/side-classifier-nonstream` | Side call: a classifier on `claude-sonnet-5`. Not streamed, no tools, thinking disabled, `stop_sequence`. `response.headers` gives `servedModel` | No |
| `claude-code/side-nonstream-no-tools` | Side call on `claude-opus-5-5`. One string message, not streamed, `end_turn` | No |
| `claude-code/error-502-html` | Classifier request answered by a gateway 502 with an HTML body. The parser keeps the request fields and adds no response fields | No |

## Not in the archive

The archive had no raw capture of these when the cases were made. Add them when a
capture exists. Do not write one by hand and call it real.

- OpenAI Chat Completions (`/v1/chat/completions`). Bob and OpenCode sessions exist, but
  they are older than raw capture.
- OpenAI Responses (`/v1/responses`, Codex).
- Agents other than Claude Code.
- A request with an `image` or `document` block.
- A JSON error body (4xx or 5xx). The only error with a capture is the HTML 502 above.
- A non-Claude model on the Anthropic dialect (GLM, DeepSeek). Sessions exist, but they
  are older than raw capture.
