# `authlib/llmclient`

Helper for authbridge plugins that call OpenAI-compatible chat-completions endpoints (OpenAI, Anthropic-via-proxy, ollama, vLLM, llama.cpp, and friends).

The package exists so plugin authors don't rewrite ~150 LOC of HTTP plumbing, JSON-from-prose extraction, and error categorization every time they need an LLM in the loop. It deliberately stays small: no streaming, no multi-turn helpers, no retry, no template engine. Plugins keep ownership of prompts, response schemas, and pipeline-action mapping.

## Quick start

```go
import "github.com/rossoctl/rossocortex/authbridge/authlib/llmclient"

c := llmclient.New(llmclient.Options{
    Endpoint:           "http://host.docker.internal:11434",
    Model:              "llama3.2:3b",
    Timeout:            15 * time.Second,
    SentinelHeaderName: "X-MyPlugin-Reentrancy",
})

type verdict struct {
    Verdict string `json:"verdict"`
    Reason  string `json:"reason"`
}

v, err := llmclient.CallStructured[verdict](ctx, c,
    "You are a security policy judge ...",
    "USER_INTENT: ...\nPROPOSED_ACTION: ...")
```

## API surface

| Symbol | Use |
|---|---|
| `New(Options) *Client` | Construct a Client. |
| `(*Client).Call(ctx, system, user) (string, error)` | Single-turn prompt → raw content. |
| `(*Client).CallRaw(ctx, *ChatRequest) (*ChatResponse, error)` | Caller-built request (multi-turn, custom params). |
| `CallStructured[T](ctx, c, system, user) (T, error)` | Call + ExtractJSON in one step. |
| `ExtractJSON[T](content) (T, error)` | Parse the first `{...}` from prose / code-fenced text. |
| `ErrUncertain` | Sentinel for "responded but unparseable." |

## Error model

| Failure | Wraps `ErrUncertain`? | Plugin should typically map to … |
|---|---|---|
| Transport / connection refused / DNS | no | 503 (LLM unavailable) |
| Per-call timeout tripped | no | 503 |
| HTTP non-2xx from upstream | no | 503 |
| HTTP 200 with empty `Choices` | yes | 403 (fail-closed deny) |
| `ExtractJSON` finds no `{...}` | yes | 403 |
| `ExtractJSON` JSON malformed / wrong shape | yes | 403 |

Plugins SHOULD wrap `ErrUncertain` in their own named sentinel so `errors.Is` works at both layers:

```go
var ErrJudgeUncertain = fmt.Errorf("%w: judge produced bad output", llmclient.ErrUncertain)
```

## Reentrancy

When a plugin's outbound LLM call passes through the same authbridge listener that hosts the plugin, the plugin's own `OnRequest` will fire on its own request — infinite loop. Two safeguards together solve this:

1. Make the LLM call via this package's `Client`, which uses a standalone `http.Client` and does not route through the local listener.
2. Set `Options.SentinelHeaderName` (e.g. `X-IBAC-Judge`). At the top of `OnRequest`, the plugin checks for that header and returns immediately when present, so even a misconfiguration that sent the call back through the proxy would short-circuit.

Pick a header name unique to the plugin to avoid collisions if multiple LLM-calling plugins are stacked.

## Out of scope

- **Streaming responses** (SSE, partial JSON). Add when the first plugin needs them.
- **Anthropic-native `/v1/messages`** shape. Use `CallRaw` with a custom request, or add a sibling `anthropicclient` package.
- **Retry / circuit-breaker.** Per-request observability and resilience are plugin concerns.
- **Schema validation.** `CallStructured[T]` does JSON unmarshal only; semantic checks are the caller's responsibility.

## Reference plugin

The IBAC plugin (`authlib/plugins/ibac/`) is the in-tree consumer — see `judge.go` for a minimal `Judge` impl built on this package.
