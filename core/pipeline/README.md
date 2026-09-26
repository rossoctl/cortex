# pipeline

Plugin pipeline types, lifecycle interfaces, and the `Context` / `Extensions`
wire shape. The full framework reference — mental model, dispatch order,
SessionEvent shape, versioning changelog — lives in
[`docs/framework-architecture.md`](../../docs/framework-architecture.md).

## Plugin developer docs

- Tutorial: [`docs/plugin-tutorial.md`](../../docs/plugin-tutorial.md)
- Reference: [`docs/plugin-reference.md`](../../docs/plugin-reference.md)
- Framework architecture: [`docs/framework-architecture.md`](../../docs/framework-architecture.md)

## Package contents

| File | Purpose |
|---|---|
| `plugin.go` | `Plugin` interface + optional `Configurable` / `Initializer` / `Shutdowner` / `Readier` |
| `context.go` | Per-request `Context` (the `pctx` each plugin sees) + `Record` / `Allow` / `Skip` / `Observe` / `Modify` / `DenyAndRecord` helpers |
| `extensions.go` | Named protocol slots (A2A / MCP / Inference), `Custom` map, `Invocations`, `/event` suffix |
| `session.go` | `SessionEvent` wire shape; the `:9094` API payload |
| `pipeline.go` | `Pipeline.Run` / `RunResponse` dispatch loop |
| `action.go` | `Action` + helpers (`Deny` / `DenyStatus` / `Challenge` / `RateLimited`) |
