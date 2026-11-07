# pipeline

Plugin pipeline types, lifecycle interfaces, and the per-request `Context`.
The `Extensions` and `SessionEvent` wire shapes are in [`core/model`](../model).
The full framework reference — mental model, dispatch order,
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
| [`../model/extensions.go`](../model/extensions.go) | Named protocol slots (A2A / MCP / Inference), `Custom` map, `Invocations`, `/event` suffix. Moved to `core/model` |
| [`../model/session.go`](../model/session.go) | `SessionEvent` wire shape; the `:9094` API payload. Moved to `core/model` |
| `state.go` | `SetState` / `GetState`: plugin-private state in the `Custom` map |
| `model_alias.go` | Aliases for the data types that moved to [`core/model`](../model): `Extensions`, `SessionEvent`, `EventClient` and the others. The old `pipeline.X` names still work |
| `pipeline.go` | `Pipeline.Run` / `RunResponse` dispatch loop |
| `action.go` | `Action` + helpers (`Deny` / `DenyStatus` / `Challenge` / `RateLimited`) |
