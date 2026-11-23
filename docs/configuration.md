# Configuration reference

The runtime config has the top-level sections below (`Config` in `core/config/config.go`).
Each one is documented next to the subsystem it drives rather than in one file, so this
table is the index.

**Plane** says what a section shapes. A "data plane" section shapes how the proxy
handles requests. A "control plane" section shapes what Cortex records or serves
about them. "Both" means the section holds settings of each kind.
**On reload** says what a hot-reload does with an edit (`validateReloadable` in
`core/reloader/reloader.go`).
"Restart" means a hot-reload refuses the edit, and the running config stays in
effect. The full contract is in [`proxy-contract.md`](proxy-contract.md#hot-reload).

| Section | What it configures | Plane | On reload | Documented in |
|---|---|---|---|---|
| `mode:` | Which deployment shape the binary serves. It must match the binary | data plane | restart | [`cmd/README.md`](../cmd/README.md) |
| `pipeline:` | The plugin composition, and each plugin's own `config:` | data plane | reloads | [`plugin-catalog.md`](plugin-catalog.md), [`plugin-reference.md`](plugin-reference.md) |
| `pricing:` | Model rates, gateway discounts, resolution order | control plane | reloads | [`pricing.md`](pricing.md) |
| `cost_ledger:` | The durable per-minute cost ledger | control plane | restart | [`laptop-service.md`](laptop-service.md) |
| `session:` | Session store TTL, event/session caps, id headers | both | restart | [`framework-architecture.md`](framework-architecture.md#operator-configuration-session) |
| `session.archive:` | The laptop's on-disk session history, its bounds, and clearing it | control plane | restart | [`laptop-service.md`](laptop-service.md#session-history-is-kept-in-cortexsessions) |
| `stats:` | The diagnostic listener (`/stats`, `/config`, `/reload/status`, `/pricing/table`, `/tls-bridge/unread`), default `:9093` | control plane | restart | [`framework-architecture.md`](framework-architecture.md) |
| `spiffe:` | SVID sourcing over the Workload API and the `/opt` file mirror | data plane | restart | [`architecture.md`](architecture.md#spiffe-configuration) |
| `listener:` | Listener addresses, `roles`, interception mode, `skip_hosts`, `responses_websockets` | both | restart | [`cmd/README.md`](../cmd/README.md) (ports, roles), [`framework-architecture.md`](framework-architecture.md#skipping-infrastructure-hosts-listenerskip_hosts) (reload rules, `skip_hosts`), [`framework-architecture.md`](framework-architecture.md#responses-api-websockets-listenerresponses_websockets) (`responses_websockets`) |
| `mtls:` | Transport mTLS on the proxy-sidecar listeners. In envoy-sidecar mode it has no effect, because Envoy does mTLS | data plane | restart | [`framework-architecture.md`](framework-architecture.md#8a-mtls-layer) |
| `tls_bridge:` | The laptop TLS bridge and its CA | data plane | restart | [`laptop-service.md`](laptop-service.md#the-tls_bridge-configuration) |
| `memory:` | The Go runtime's soft memory limit for the whole process | both | restart | [`laptop-service.md`](laptop-service.md#the-memory-configuration) |
| `praxis:` | Praxis mode: the `cortex-gateway` process and its `policy` | data plane | restart | [`praxis-policy.md`](praxis-policy.md) |
| `accounting:` | Whether the process hosts the accountant that prices the context operators' savings | control plane | `enabled`: turning it on needs a restart if the proxy started with it off; turning it off reloads | [`proxy-contract.md`](proxy-contract.md#hot-reload) |
| `update:` | Automatic updates: `auto`, `wait`, `check_every` | control plane | `check_every`: restart. agentop reads `auto` and `wait` each time it runs | [`laptop-service.md`](laptop-service.md#automatic-updates) |

Two things no config file can answer, because the effective values come from the
file *plus* compiled-in defaults: what rates are in effect (`agentop pricing --host
<gateway>`) and what pipeline is running (`agentop pipeline get`, or `GET /v1/pipeline`).
`GET /config` on the diagnostic listener reports the config in effect, not the file
as written. That is the file after `${ENV_VAR}` expansion, with the defaults that
`config.Load` fills in and the mode preset applied. On a local install it also shows
the default script slot that `cortex` adds. It masks secret values. Defaults that are
worked out at runtime do not show:
- plugin defaults, which each plugin sets in its own `Configure`
- the soft memory limit of a local install (`MemoryLimitBytes`)
- whether an unset `cost_ledger.enabled` turns the ledger on (`ledgerDefaultOn` in
  `cmd/cortex/main.go`)
- the session store limits and whether the session archive runs

A refused reload does not change `/config`. For example, a reload that changes
`stats.*` is refused, and `/reload/status` reports the error. `/config` then still
shows the address the stats server listens on, which it keeps until a restart
([hot-reload](proxy-contract.md#hot-reload)).
