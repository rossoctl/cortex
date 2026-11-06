# A2A parser cases, hand-built

`TestFixtures` (`../fixtures_test.go`) runs every `<case>/` directory here through the
parser hooks. It compares the result with `expected.json`.

These cases are **hand-built**, not captured. The session archive keeps no raw A2A
bodies. The shapes follow the A2A specification v0.3.0 (JSON-RPC transport).

Each case has the files of `../../inferenceparser/testdata/README.md`, with these changes:

- `request.json` is indented, not compact. An empty `request.json` or response file
  is a body with no bytes.
- There is no `response.html`.
- There is no `response.headers`.
- `expected.json` is the `pipeline.A2AExtension`. `null` means that the parser does not
  claim the request. The JSON omits `isAction` when it is false, so the byte
  comparison checks `isAction` in every case.

The test parses each case with the whole response body (`OnResponse`) and frame by
frame (`OnResponseFrame`). The two results must be equal.

To write the goldens, then read each one:

```sh
cd core && GOWORK=off go test ./plugins/a2aparser/ -run Fixtures -count=1 -update
```

## Cases

| Case | What it covers | isAction |
|---|---|---|
| `message-send` | `message/send` with no `contextId`. The JSON response is a completed Task, which gives `sessionId`, `artifact`, and `taskId` from the Task `id` | true |
| `message-stream` | `message/stream` with `contextId` in the message. SSE: a Task, an `artifact-update`, and a final `status-update` | true |
| `message-stream-no-final` | `message/stream`. SSE: a Task and a status-update that is not final, then the stream ends. `finalStatus` is the last state, `working` | true |
| `tasks-get` | `tasks/get`. The JSON response is a completed Task. The Task `id` gives `taskId` | false |
| `tasks-get-error` | `tasks/get` for an unknown task, JSON-RPC error response. `params.id` gives `taskId`. The extension has no field for a JSON-RPC error | false |
| `message-send-task-not-found` | `message/send` for a task that the server does not know, JSON-RPC error response. `params.message.taskId` gives `taskId` | true |
| `push-config-set-unsupported` | `tasks/pushNotificationConfig/set`, error response. The older `params.taskId` location gives `taskId` | false |
| `not-jsonrpc` | An inference body with no `method`. Not claimed | no extension |
| `non-a2a-method` | The MCP method `tools/list`. Not claimed | no extension |

Gap: `A2AExtension` has no field for a JSON-RPC error. An error response keeps only the request fields (`tasks-get-error`).
