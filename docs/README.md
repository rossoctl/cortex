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
| See the rates actually in effect | `agentop pricing [--host <gateway>]` |
| Write a plugin | [`docs/plugin-reference.md`](../docs/plugin-reference.md) and [`plugin-tutorial.md`](../docs/plugin-tutorial.md) |
| Understand the pipeline internals and hot-reload | [`docs/framework-architecture.md`](../docs/framework-architecture.md) |
| Run a demo | [`demos/README.md`](../demos/README.md) |
| Use the `agentop` TUI | [`cmd/agentop/README.md`](../cmd/agentop/README.md) |
| Export sessions to files | [`session-dump.md`](session-dump.md) |

## Configuration reference

The runtime config has ten top-level sections. They are documented next to the
subsystem each one drives rather than in one file, so this table is the index.

| Section | What it configures | Documented in |
|---|---|---|
| `mode:` | Which deployment shape the binary serves; must match the binary | [`cmd/README.md`](../cmd/README.md) |
| `pipeline:` | The plugin composition, and each plugin's own `config:` | [`plugin-catalog.md`](plugin-catalog.md), [`plugin-reference.md`](plugin-reference.md) |
| `pricing:` | Model rates, gateway discounts, resolution order | [`pricing.md`](pricing.md) |
| `cost_ledger:` | The durable per-minute cost ledger | [`laptop-service.md`](laptop-service.md) |
| `session:` | Session store TTL, event/session caps, id headers | [`framework-architecture.md`](framework-architecture.md) |
| `session.archive:` | The laptop's on-disk session history, its bounds, and clearing it | [`laptop-service.md`](laptop-service.md#session-history-is-kept-in-cortexsessions) |
| `stats:` | The diagnostic listener (`/stats`, `/config`, `/reload/status`, `/pricing/table`, `/tls-bridge/unread`), default `:9093` | [`framework-architecture.md`](framework-architecture.md) |
| `spiffe:` | SVID sourcing over the Workload API and the `/opt` file mirror | [`architecture.md`](architecture.md) |
| `listener:` | Listener addresses, `skip_hosts`, interception mode | [`framework-architecture.md`](framework-architecture.md) (reload rules), [`kubernetes.md`](kubernetes.md) (`bind_loopback_only`), and `CLAUDE.md` for `skip_hosts` |
| `mtls:` | Transport mTLS on the listeners, both deployment shapes | [`framework-architecture.md`](framework-architecture.md#8a-mtls-layer) and `CLAUDE.md` |
| `tls_bridge:` | The laptop TLS bridge and its CA | [`laptop-service.md`](laptop-service.md), partially — only `passthrough_hosts` |

The remaining gaps: `listener.skip_hosts` is documented only in `CLAUDE.md`, which
is AI-assistant context rather than operator documentation, and `tls_bridge:` has
one field described and the rest undocumented. Recorded here so the gap is nameable rather than absent.

Two things no config file can answer, because the effective values come from the
file *plus* compiled-in defaults: what rates are in effect (`agentop pricing --host
<gateway>`) and what pipeline is running (`agentop pipeline get`, or `GET /v1/pipeline`).
`GET /config` on the diagnostic listener reports the config **as written**, not as
resolved.

## In this directory

- [`proposals/`](proposals/) — design records for work that has since shipped.
  They are kept for the reasoning, not as current documentation; each carries a
  status line pointing at the doc that describes present behaviour.
- `assets/` — images used by the root README.

Dated implementation plans and design specs live in
[`docs/superpowers/`](../docs/superpowers/).
