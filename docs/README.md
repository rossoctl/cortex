# Cortex documentation

Cortex's documentation lives next to the code it describes. This directory holds
only the repo-level pieces.

## Start here

| If you want to… | Read |
|---|---|
| Install and run Cortex on a laptop | [root README](../README.md) |
| Understand the sidecar shapes and deployment | [`architecture.md`](architecture.md) |
| Configure a plugin | [`docs/plugin-catalog.md`](../docs/plugin-catalog.md) |
| Set up model pricing and gateway discounts, or read the `cost` record | [`docs/pricing.md`](../docs/pricing.md) |
| See the rates actually in effect | `abctl pricing [--host <gateway>]` |
| Write a plugin | [`docs/plugin-reference.md`](../docs/plugin-reference.md) and [`plugin-tutorial.md`](../docs/plugin-tutorial.md) |
| Understand the pipeline internals and hot-reload | [`docs/framework-architecture.md`](../docs/framework-architecture.md) |
| Run a demo | [`demos/README.md`](../demos/README.md) |
| Use the `abctl` TUI | [`cmd/abctl/README.md`](../cmd/abctl/README.md) |

## In this directory

- [`proposals/`](proposals/) — design records for work that has since shipped.
  They are kept for the reasoning, not as current documentation; each carries a
  status line pointing at the doc that describes present behaviour.
- `assets/` — images used by the root README.

Dated implementation plans and design specs live in
[`docs/superpowers/`](../docs/superpowers/).
