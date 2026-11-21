# Getting started

This page takes you from install to a first prompt that goes through Cortex.
It also tells you how to undo each step.

## Install

Run this command:

```sh
curl -fsSL https://www.rossoctl.dev/cortex/install.sh | sh
```

Setup lists what it will change and asks once (`Continue? [Y/n]`).
It does these steps:

- It installs `agentop` and `cortex` to `~/.local/bin`.
- It adds `~/.local/bin` to `PATH` in your shell profile.
- It writes `~/.cortex/config.yaml`.
- It runs the proxy on `127.0.0.1:47600`, as a launchd user agent on macOS or a systemd user service on Linux.

To skip the question, add `--yes`:

```sh
curl -fsSL https://www.rossoctl.dev/cortex/install.sh | sh -s -- --yes
```

When setup is done, open a new terminal. The new terminal reads the new `PATH`.

### Windows (preview)

`install.sh` and `agentop setup` don't run on Windows yet (#1520). Install by hand:

1. From the [releases page](https://github.com/rossoctl/cortex/releases), download `agentop_<version>_windows_amd64.zip` and `cortex_<version>_windows_amd64.zip` (`arm64` on an Arm PC), and `checksums.txt`.
2. Check them in PowerShell. `checksums.txt` has lower-case hashes, and `Get-FileHash` prints upper case, so compare `(Get-FileHash .\agentop_<version>_windows_amd64.zip -Algorithm SHA256).Hash.ToLower()` with the line for that file, and the same for `cortex`.
3. Unzip both into one folder, for example `$HOME\.local\bin`, and add that folder to your user `PATH`.
4. In one terminal, run the proxy: `cortex --local`. It writes `~/.cortex/config.yaml` on first run. Stop it with `Ctrl-C`.
5. In a second terminal, run `agentop observe`.

Then connect Claude Code with `agentop configure claude-code enable`, or run one session through Cortex with `agentop exec -- claude`.

Not on Windows yet: `agentop service`, `setup`, `uninstall` and `update` (they refuse and say so), so the proxy runs only while its terminal is open. Tools other than Node (`git`, `curl`, Python) inside a session don't trust the TLS bridge yet.

## Connect your agent

Connect each agent that you use. Do this one time per agent.

| Agent | Connect | Start it | Running agent |
|---|---|---|---|
| [Claude Code](agents/claude-code.md) | `agentop configure claude-code enable` | `claude` | Restart a running `claude`. `claude --resume` opens the conversation again. |
| [OpenCode](agents/opencode.md) | `agentop configure opencode enable` | `opencode` | `enable` restarts the OpenCode background service. This stops every OpenCode session that uses the service. |
| [Codex](agents/codex.md) | `agentop configure codex enable` | `codex` | Restart a running `codex`. It reads `~/.codex/.env` only when it starts. `enable` restarts the Codex background app-server. |
| [IBM Bob](agents/bob.md) | `agentop configure bob enable` | The Bob editor, or `bob` in a new terminal | Restart Bob. Bob then reads its settings again. |
| [Pi](agents/pi.md) | `agentop configure pi enable` | `pi` in a new terminal | Start `pi` in a new terminal. |
| [Any other agent](agents/any-agent.md) | Not necessary | `agentop exec -- <command>` | Nothing to restart. |

Each agent has a guide in `agents/<agent>.md`. The first column links to it.

## Check that it works

1. In a second terminal, run `agentop observe`.
2. Send one prompt to your agent.
3. Look for a new row in Sessions.

With two or more agents, `agentop observe` opens the Agents pane first.
Press `Enter` on an agent to show its sessions.

The row shows the session id, the title, the time of the last change, the event count, the tokens and the cost.
The `AGENT` column appears when Sessions lists two or more agents.
Press `Enter` on the row to open the events pane. Its `METHOD` column shows the model name.

When no row appears, run `agentop doctor`.
Each line of its output starts with one of these marks:

- ✓: the item is correct.
- !: advice.
- ✗: a problem. The line gives the command that repairs it.

Then read [Troubleshooting](troubleshooting.md).

## Read the screen

This is the Sessions pane at 100 columns:

```
agentop · http://127.0.0.1:47601
LAST 1H    TODAY   7 DAYS    MONTH
  $4.04   $18.80  $216.44  $703.18
────────────────────────────────────────────────────────────────────────────────────────────────────
 SESSION   TITLE              UPDATED           EVENTS      TOKENS        COST      SAVED~  CTX(1M)
 567-89ab  …src/append-spans  3s ago                42       48.2k       $0.12       $0.01  ████░░░
 890-cdef  weather-agent      18m ago               15        1.2k      <$0.01           —  ▏░░░░░░
 321-0fed                     42m ago                7        2.9k           —           —  ██░░░░░
 default                      1h ago                 8           —           —           —        —

● connected  2.1 events/sec   feedback: https://github.com/rossoctl/cortex/issues/new/choose
[↵] drill  [u] usage  [$] spend  [/] search  [?] keys  [q] quit
```

- The band `LAST 1H`, `TODAY`, `7 DAYS`, `MONTH` shows what your agents spent in each time span.
- `SESSION` shows the session id, cut short.
- `TITLE` shows the conversation title.
- `UPDATED` shows the time since the last event.
- `EVENTS` shows the event count.
- `TOKENS` and `COST` show the totals for the full session.
- `SAVED~` shows what `tool-prune` saved.
- `CTX(1M)` shows how full the context was on the last turn, against a window of one million tokens.
- The footer shows the connection state and the keys.

| Key | Action |
|---|---|
| `↑` / `↓` | Move between rows |
| `Enter` | Open the row |
| `Esc` | Go back |
| `/` | Search the pane |
| `?` | Show every key |
| `$` | Open the spend breakdown, by model, endpoint or agent |
| `u` | Open the usage charts |
| `T` | Open the timeline of the highlighted session |
| `A` | Open the per-agent breakdown |
| `q` | Quit |

Press `?` for every key.
For more detail, read [Keybindings](../cmd/agentop/README.md#keybindings).
More: [Using agentop](using-agentop.md).

## Next steps

- Add the history from before Cortex: `agentop import --dry-run`, then `agentop import`. Read [Importing agent history](../cmd/agentop/README.md#importing-agent-history-agentop-import).
- Change the model server: `agentop server`. Read [inference-servers.md](inference-servers.md).
- See the cost: `agentop cost --window month --by agent`. Read [pricing.md](pricing.md).

## Undo it

To disconnect one agent, run `agentop configure <agent> disable`. Then restart the agent.

To remove Cortex, run `agentop uninstall`. It asks once.
`agentop uninstall --purge` also deletes `~/.cortex`.

Uninstall disconnects Claude Code, OpenCode, IBM Bob and Pi.
It does not disconnect Codex. If you use Codex, run `agentop configure codex disable` first.

For more detail, read [Remove it](laptop-service.md#remove-it).
