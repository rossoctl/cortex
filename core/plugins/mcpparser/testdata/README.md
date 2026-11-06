# MCP parser cases, hand-built

`TestFixtures` (`../fixtures_test.go`) runs every `<case>/` directory here through the
parser hooks. It compares the result with `expected.json`.

These cases are **hand-built**, not captured. The session archive keeps no raw MCP
bodies. The shapes follow the MCP specification, revision 2025-06-18 (Streamable HTTP).
The parser has its default config, so `/mcp` is the MCP path.

Each case has the files of `../../inferenceparser/testdata/README.md`, with these changes:

- `request.json` is indented, not compact. An empty `request.json` or response file
  is a body with no bytes.
- There is no `response.html`.
- `response.headers` holds `Content-Type` and `Mcp-Session-Id`. The parser does not
  read response headers.
- `expected.json` is the `pipeline.MCPExtension`. `null` means that the parser does not
  claim the request. The JSON omits `isAction` when it is false, so the byte
  comparison checks `isAction` in every case.

The test parses each case with the whole response body (`OnResponse`) and frame by
frame (`OnResponseFrame`). The two results must be equal. A case in the test's
`divergent` map gives two different results. The test skips it and shows the two
results. The case has no `expected.json`.

To write the goldens, then read each one:

```sh
cd core && GOWORK=off go test ./plugins/mcpparser/ -run Fixtures -count=1 -update
```

## Cases

| Case | What it covers | isAction |
|---|---|---|
| `initialize` | `initialize` request, JSON result. `response.headers` gives the `Mcp-Session-Id` that the server assigns | false |
| `notification-initialized` | `notifications/initialized`: no `id`, and an empty 202 response | false |
| `tools-list` | `tools/list`, JSON result with one tool | false |
| `tools-call-json` | `tools/call`, JSON result | true |
| `tools-call-sse` | `tools/call`, SSE response: a `notifications/message` log event, then the result | true |
| `tools-call-error` | `tools/call` for an unknown tool, JSON-RPC error response | true |
| `tools-call-sse-result-then-error` | SSE response with a result event and then an error event for the same `id`. Divergent: `OnResponse` keeps the first event. `OnResponseFrame` keeps the result and sets the error | true |
| `delete-terminate` | `DELETE /mcp` with `Mcp-Session-Id` and no body: `$transport/terminate` | false |
| `get-stream` | `GET /mcp` with no body: `$transport/stream`. The SSE stream has only a server notification | false |
| `not-jsonrpc` | An inference body with no `method`. Not claimed | no extension |
| `non-mcp-method` | The A2A method `tasks/get` on `/mcp`. Not claimed | no extension |
