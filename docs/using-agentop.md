# Using agentop

`agentop` shows the traffic that Cortex sees: sessions, events, tokens and cost.
This guide is a first tour for a laptop user.
For every flag and key, read the [agentop reference](../cmd/agentop/README.md).

## Open it

Run this command:

```sh
agentop observe
```

On a laptop, `agentop observe` connects to the local Cortex.
To watch a cluster, read [Choosing between a cluster and a local Cortex](../cmd/agentop/README.md#choosing-between-a-cluster-and-a-local-cortex---kubernetes) (cluster only).

## The screen

`agentop` opens on the Sessions pane. This is the Sessions pane at 100 columns:

```
agentop · http://localhost:9094
LAST 1H    TODAY   7 DAYS    MONTH
  $4.04   $18.80  $216.44  $703.18
────────────────────────────────────────────────────────────────────────────────────────────────────
 SESSION         TITLE        UPDATED           EVENTS      TOKENS        COST      SAVED~  CTX(1M)
 ctx-abc-123-4…  …pend-spans  3s ago                42       48.2k       $0.12       $0.01  ▕████ ▏
 ctx-def-567-4…  weather-ag…  18m ago               15        1.2k      <$0.01           —  ▕▏    ▏
 ctx-ghi-901-4…               42m ago                7        2.9k           —           —  ▕██   ▏
 default                      1h ago                 8           —           —           —        —

● connected  2.1 events/sec   feedback: https://github.com/rossoctl/cortex/issues/new/choose
[↑↓] nav  [↵] drill  [u] usage  [$] spend  [/] search  [p] pause  [P] plugins  [?] keys  [q] quit
```

The band at the top shows what your agents spent in each period:

- `LAST 1H` shows the spend in the last hour.
- `TODAY` shows the spend since local midnight.
- `7 DAYS` shows the spend in the last seven days.
- `MONTH` shows the spend since the first day of the month.
- A `—` in the band means that this deployment cannot answer for that period.

The table has one row for each session:

- `SESSION` shows the session id, cut short.
- `TITLE` shows the conversation title.
- `UPDATED` shows the time since the last event, as a relative time (for example `3s ago`).
- `EVENTS` shows the count of events.
- `TOKENS` shows the count of tokens.
- `COST` shows the cost in US dollars.
- `SAVED~` shows an estimate of what Cortex plugins saved this session, in US dollars.
- `CTX(1M)` shows how full the context was on the last turn, as a bar against a window of one million tokens.
- `SERVER` shows the inference server of the session. It shows only when the inference router routes to more than one server.

Each figure in the table is a total for the full session.

An imported session has `(*)` before its title.
When the terminal is too narrow for `TITLE`, `(*)` is before its id.

For more detail, read [Panes](../cmd/agentop/README.md#panes) and [How CTX is derived](../cmd/agentop/README.md#how-ctx-is-derived).

## Panes

Each pane opens with a key. `Esc` goes back to the pane before it.

- **Sessions**: the start pane. It lists the sessions.
- **Events**: `Enter` on a session. It lists the events of that session.
- **Detail**: `Enter` on an event. It shows the full event.
- **Timeline**: `T`. It shows the turns of the highlighted session in time order.
- **Usage**: `u`. It shows the usage charts.
- **Agents**: `A`. It shows the cost of each agent.
- **Pipeline**: `P`. It shows the plugin pipeline that runs.
- **Plugin detail**: `Enter` on a Pipeline or Catalog row. It shows one plugin.
- **Catalog**: `C`. It lists the plugins that Cortex has.
- **Key help**: `?`. It lists every key.
- **Namespaces** and **Pods**: cluster only. They let you select an AuthBridge sidecar.

## Keys

| Key | Action |
|---|---|
| `↑` / `↓` | Move between rows |
| `Enter` | Open the row |
| `Esc` | Go back |
| `/` | Search the pane |
| `?` | Show every key |
| `$` | Open the spend breakdown |
| `u` | Open the Usage pane |
| `T` | Open the timeline of the highlighted session |
| `A` | Open the Agents pane |
| `S` | On Agents, select the server for the new sessions of the agent. On Sessions, move the session to a different server |
| `p` | Pause or resume the live stream |
| `P` | Open the Pipeline pane |
| `s` | Hide or show the skip rows |
| `q` / `Ctrl+C` | Quit |

Press `?` for the full list.
For more detail, read [Keybindings](../cmd/agentop/README.md#keybindings).

## Everyday commands

| Command | What it does | Example | More |
|---|---|---|---|
| `observe` | Opens the traffic viewer (TUI) | `agentop observe` | [Panes](../cmd/agentop/README.md#panes) |
| `setup` | Installs or repairs Cortex on this machine | `agentop setup` | [Laptop service](laptop-service.md) |
| `doctor` | Checks the Cortex install on this machine | `agentop doctor` | [Laptop service](laptop-service.md) |
| `update` | Installs the newest Cortex release | `agentop update` | [Updating](../cmd/agentop/README.md#updating-agentop-update) |
| `uninstall` | Removes Cortex from this machine | `agentop uninstall` | [Laptop service](laptop-service.md) |
| `service` | Runs Cortex as a service: install, uninstall, status, stop, start, restart | `agentop service status` | [Laptop service](laptop-service.md) |
| `configure` | Points a coding agent at Cortex: enable, disable, status | `agentop configure claude-code enable` | [configure status](../cmd/agentop/README.md#which-harnesses-are-wired-into-cortex-agentop-configure-status) |
| `exec` | Runs a command with the proxy and CA of Cortex in its environment | `agentop exec -- curl https://example.com` | [exec](../cmd/agentop/README.md#running-one-command-through-cortex-agentop-exec) |
| `server` | Lists or changes the inference server for the new sessions of each agent | `agentop server` | [server](../cmd/agentop/README.md#choosing-an-inference-server-agentop-server) |
| `script` | Lists, installs or removes the script plugins that Cortex ships | `agentop script list` | [script](../cmd/agentop/README.md#installing-a-bundled-script-plugin-agentop-script) |
| `pipeline` | Shows the plugin pipeline that runs | `agentop pipeline get` | [pipeline](../cmd/agentop/README.md#the-pipeline-that-runs-agentop-pipeline-get) |
| `pricing` | Shows the model rates that Cortex uses | `agentop pricing` | [Pricing](pricing.md) |
| `cost` | Shows what your agents spent | `agentop cost --window month --by agent` | [cost](../cmd/agentop/README.md#what-your-agents-have-spent-agentop-cost) |
| `export` | Writes the archive as NDJSON with no content | `agentop export --out sessions.ndjson` | [Session export](session-export.md) |
| `import` | Imports agent transcripts into Cortex | `agentop import --dry-run` | [import](../cmd/agentop/README.md#importing-agent-history-agentop-import) |

Run a command with `--help` for its full usage.
