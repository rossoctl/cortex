# abctl

Interactive terminal UI for inspecting AuthBridge's in-memory session store.
`abctl` connects to the session API exposed by an AuthBridge sidecar
(default `http://localhost:9094`, typically reached via `kubectl port-forward`)
and lets you browse active sessions, follow a session's event stream live,
and read individual events as pretty-printed JSON.

## Install

Download a prebuilt `abctl` for your platform (linux/macOS, amd64/arm64) from the
[Releases page](https://github.com/rossoctl/cortex/releases) — see
[Download prebuilt binaries](../../README.md#download-prebuilt-binaries) for the download,
checksum-verify, and macOS quarantine steps — and drop it on your PATH.

Or build from source:

```sh
cd authbridge/cmd/abctl
go build .
```

Either way you get a single binary (~10 MB; the linux build is fully static).

## Run

`abctl observe` discovers AuthBridge agents in your current `kubectl`
context and lets you pick one:

```sh
./abctl observe
```

Bare `./abctl` does the same thing, but is **deprecated** and will stop
opening the viewer in a future release. It printed no hint that the
other subcommands existed, so anyone who never ran `--help` reasonably
concluded the TUI was all abctl did — `abctl service` least visible of
all, and that is what you need when Cortex is not running. Run
`abctl --help` for the full list.

You'll see a Namespaces pane listing each namespace that contains an
AuthBridge agent. Enter drills into the Pods pane for that namespace;
Enter on a pod starts a `kubectl port-forward` automatically and drops
you into the session-events view. Esc backs out. `q` (or Ctrl+C) quits
and tears the port-forward down.

The picker shells out to `kubectl` — whatever context you're in is the
context abctl uses. There's no separate auth.

### Connecting to an existing port-forward

Press `l` on the Namespaces pane to skip the cluster entirely and connect to a
session API on this host. Where that is depends on whether you have a Cortex
installed: it goes to the address in `~/.cortex/config.yaml` when one answered
there (47601 by default), and otherwise to `http://localhost:9094`, the
in-cluster default port. The second case is the useful one when you already have
your own `kubectl port-forward` running, when abctl runs inside the mesh, or
when your kubeconfig can't list pods but a tunnel is up.

abctl probes `/v1/sessions` before switching panes, so an endpoint with
nothing listening surfaces as a footer error and leaves you in the
picker rather than dropping you into a silently empty session view.
`Esc` from a session entered this way returns to the Namespaces pane
(there's no pod to go back to).

Pipeline editing (`e`) follows the same split. Connected to this machine's
Cortex, `e` edits its config file — see [Editing the pipeline](#editing-the-pipeline).
Connected to `:9094` through somebody else's port-forward, there is neither a pod
identity to resolve a ConfigMap from nor a local config file to write, so `e`
flashes a hint instead.

### Power-user / scripting bypass

Pass `--endpoint` to skip the picker entirely:

```sh
kubectl port-forward -n team1 pod/weather-agent-xxxx 9094:9094 &
./abctl observe --endpoint http://localhost:9094
```

This preserves the pre-picker behavior for scripts, CI, or remote
session APIs that aren't in your kube context.

### Choosing between a cluster and a local Cortex (`--kubernetes`)

With no `--endpoint`, abctl decides between the cluster picker and the Cortex
running on this machine (read from `~/.cortex/config.yaml`, and probed first —
a stale config from an install that is no longer running is ignored).

`--kubernetes` controls that choice and **defaults to false**, so a local Cortex
that is answering wins and the picker appears only when none is:

```sh
./abctl observe                # the local Cortex, when one is running
./abctl observe --kubernetes   # the picker, even with a local Cortex running
```

The default favours the local one because that is the quickstart, and it should
need no flag: install Cortex on your laptop, run `abctl observe`, watch traffic.
`--kubernetes` is for the machine that has both — a local install AND cluster
work — where the probe would otherwise win every time and `--endpoint` could
only substitute for the picker by naming a namespace, a pod and a port-forward
by hand.

A machine with no local install needs no flag either: with nothing answering,
`abctl observe` opens the picker on its own.

The flag is ignored when `--endpoint` is given — an explicit address always
wins. Resolution in full:

| `--endpoint` | Local Cortex answering | `--kubernetes` | Result |
|---|---|---|---|
| given | — | — | that endpoint |
| — | yes | absent (default) | the local Cortex |
| — | yes | passed | Namespaces picker |
| — | no | either | Namespaces picker |

## Running one command through Cortex (`abctl exec`)

`abctl configure claude-code enable` works because Claude Code has a settings
file: the variables can be written once and reach every session on the machine,
background agents included. Nothing else has that. `curl`, `python`,
`node`, `gh` and your test suite read the process environment and nothing
else, and the usual workaround — exporting `HTTPS_PROXY` in your shell —
leaks into every unrelated command in that terminal until you remember to
unset it.

`abctl exec` scopes the routing to a single child process:

```sh
abctl exec -- curl -sv https://api.anthropic.com/v1/messages
abctl exec -- claude --dangerously-skip-permissions
abctl exec -- bob
```

Everything after `--` is passed through exactly as typed. abctl never
parses it, so the command's own flags need no escaping — even ones abctl
also has, like `--print`. The child inherits your whole environment plus
these nine variables:

| Variable | Value |
|---|---|
| `HTTP_PROXY` `HTTPS_PROXY` `http_proxy` `https_proxy` | the forward proxy URL |
| `NODE_EXTRA_CA_CERTS` | `ca.crt`, *added* to the runtime's own roots |
| `CURL_CA_BUNDLE` `REQUESTS_CA_BUNDLE` `SSL_CERT_FILE` `GIT_SSL_CAINFO` | `bundle.crt` — the bridge CA plus the platform roots |

Four proxy spellings because there is no agreed one: Go and most Unix
tools read the lowercase pair, Node the uppercase, libcurl either. A tool
that reads only the spelling we left out would silently bypass the proxy —
invisible, because it keeps working.

The CA names split two ways, and the difference matters. `NODE_EXTRA_CA_CERTS`
*extends* Node's trust store, so it takes `ca.crt` directly. The other four
*replace* it: whatever file they name becomes the complete set of roots, so
pointing them at `ca.crt` would leave the child trusting the bridge and nothing
else — breaking every host the bridge does not terminate. They get `bundle.crt`
instead, which Cortex writes beside `ca.crt` on startup (bridge CA + platform
roots). These are the same values `abctl configure claude-code enable` writes
into `settings.json`; `exec` reuses that derivation rather than repeating it, so
the two commands cannot disagree.

Both proxy variables get the **`http://`** URL, deliberately. The scheme
in a `*_PROXY` variable says how to reach the *proxy*, not what the
proxied request is; Cortex's forward proxy speaks plain HTTP and
CONNECT-tunnels TLS, so `https://` there would make clients attempt TLS
to the proxy itself and fail the handshake.

The values come from the **running proxy**, fetched from its stats endpoint
(`http://localhost:47602/config` by default, `--cortex-stats-url` to point
elsewhere). There is no `--config` flag: `exec` works against a proxy that has to
be up for the child to reach anything, and that process already has a config —
reading a file instead would let the two disagree, since listener addresses are
not hot-reloaded and a file says nothing about whether anything is listening. A
Cortex that is down is reported as down rather than yielding an environment that
points at nothing. The derivation from config to variables is still the one
`configure claude-code enable` uses, so the two produce identical values for the
same Cortex. Nothing is exported to your shell and no file is modified.

abctl exits with the child's status (127 if the command was not found,
128+signum if it was killed), so it is safe in a pipeline or a Makefile.
Keyboard signals (Ctrl-C) reach the child directly through the shared
process group; a signal aimed at abctl itself — `timeout 30 abctl exec
-- …`, a CI runner, systemd — is relayed to the child, so it is not left
orphaned with the injected environment.

To see the variables without running anything:

```sh
abctl exec --print                 # nine shell-quoted export lines
eval "$(abctl exec --print)"       # or apply them to the current shell
```

`--print` emits paths only — it writes nothing. `bundle.crt` and `ca.crt` are
created by Cortex itself on first start, so the exported paths keep resolving
long after abctl exits, which is what makes the `eval` form usable.

`--print` takes no command, and no `--`: it is a complete request on its own.
Both `abctl exec --print -- curl …` and a bare `abctl exec --print --` are usage
errors — the first asks for two different things at once, the second promises a
command and supplies none. The paths `--print` hands out are meant to be kept,
and are the same ones `abctl configure claude-code enable` writes into
`settings.json`; running a command is the opposite, applying them to one process
for its lifetime. Asking for both in one invocation is a contradiction about
which you want, so abctl says so rather than picking one.

`abctl configure claude-code enable` shares this requirement as of the same
change: it too refuses `tls_bridge.mode: disabled` with a `ca_dir` set, a
combination it used to
accept and write into `settings.json`, where the CA bought nothing because the
bridge terminated no TLS. `enable` also now points its four replacing variables at
`bundle.crt` rather than the bare `ca.crt`.

Requires an enabled TLS bridge — both `tls_bridge.mode: enabled` and
`tls_bridge.ca_dir`. `mode: disabled` with a `ca_dir` set is a valid config,
but the bridge then terminates nothing, so a CA would buy the child nothing
while breaking its https; `abctl exec` refuses rather than inject either half
of a setup that cannot work.

Before Cortex's first start, `ca.crt` does not exist yet. `exec` still runs the
command and says so, but leaves the four replacing variables unset — the child
keeps its own public roots and only bridged hosts fail, rather than losing all
trust to a bundle with no bridge CA in it.

## Panes

The UI has these top-level panes. `Enter` drills in; `Esc` backs out.

- **Sessions** (default): table of active sessions in the store, most
  recently updated first. Columns: ID, updated (relative), event count,
  tokens, cost, saved, active marker. The two money columns are lifetime
  totals for the session and are dropped entirely on a terminal too narrow to
  show a sub-cent charge honestly — below 72 columns — rather than rounded to
  `$0.00` or blanked.

  ```
  abctl · http://localhost:9094 · [Sessions] Pipeline
  SPEND  $30.9350 today   saved ~$0.1804   $2.9100 /1h   cache 81%   9.9M tokens

   ID                                        UPDATED    EVENTS   TOKENS      COST     SAVED  ACTIVE
   ctx-abc-1234…                             3s ago     42       48.2k    $0.1214  ~$0.0038  ●
   ctx-def-5678…                             18m ago    15       1.2k     $0.0031         —
   default                                   1h ago     8            —          —         —

  ● connected   2.1 ev/s   drops: 0
  [↑↓] nav  [↵] drill  [tab] pipeline  [u] usage  [$] spend  [/] filter  [p] pause  [?] keys  [q] quit
  ```

  The selected row is reverse-video rather than marked with a glyph, so it is
  the one thing these listings cannot show.
- **Events**: per-session event table. `c` opens a column picker — a popup with
  a checkbox and a one-line description per column, since twelve abbreviated
  headers are not self-describing.

  The picker is also where sorting lives: `s` orders the table by the column
  under the cursor, descending first, so "which calls took longest" and "which
  cost the most" are one keystroke from the column that answers them. Press it
  again for ascending, and a third time to return to arrival order. The sorted
  column is marked in its header (`DURATION▼`) and named in the footer
  (`[sort: DURATION▼]`) — the footer matters on a narrow terminal, where the
  sorted column may be one the table had to drop.

  Numeric columns sort numerically, not by the text in the cell: DURATION orders
  90ms before 1.20s, and TOKENS orders 900 before 1,048,576. Blank cells — a
  request whose response has not landed, or a figure the proxy could not model —
  sort to the bottom of a descending view rather than crowding the end you sorted
  toward. Sorting never changes the `#` exchange pairing or the per-row token and
  cost figures; it reorders the finished rows only.

  All twelve together need ~168 terminal columns, so the table drops what does
  not fit and the footer says how many (`→ N more columns`). Columns carry a
  keep rank rather than being equally expendable: DIR, DURATION, TOKENS and COST
  give way first, while `#` and HOST survive longest. That is what makes HOST
  usable at 80 columns despite being last in display order — it is the column
  most people open this pane for.

  All twelve are on by default: `#` (exchange number, shared by a request
  and its response), TIME, DIR, PHASE, ACTION, PLUGIN, METHOD, STATUS,
  DURATION, TOKENS, COST, HOST. On a narrow terminal the low-ranked ones
  are hidden rather than turned off, so widening the window brings them
  back without touching the picker.

  Live-updates while in view — if the cursor is on the last row, it
  auto-follows new events.

  All twelve columns, on a terminal wide enough for them. Each `#` appears
  twice — once for the request, once for its response — which is how a row
  with no STATUS is read as "still in flight" rather than "failed":

  ```
  abctl · ctx-abc-1234…

   #     TIME          DIR   PHASE    ACTION    PLUGIN              METHOD              STATUS   DURATION    TOKENS             COST                 HOST
   1     14:23:07.41   in    req      allow     jwt-validation                                                                                       weather-agent
   1     14:23:07.52   in    resp     —         —                                       200      118ms                                               weather-agent
   2     14:23:07.71   out   req      observe   inference-parser    claude-sonnet-5                                            681,300(−9.9k)   $0.2546(−$0.0037)   api.anthropic.com
   2     14:23:08.91   out   resp     —         —                   claude-sonnet-5     200      1.20s       412                                     api.anthropic.com
   3     14:23:09.01   out   req      modify    token-exchange      tools/call                                                                       github-tool-mcp
   3     14:23:09.10   out   resp     —         —                   tools/call          503      96ms                                                github-tool-mcp

  ● connected   2.1 ev/s   drops: 0   [sort: DURATION▼]   [filter: anthropic]
  [↑↓] nav  [b/f] page  [↵] detail  [c] columns  [u] usage  [s] hide passthru/skip  [p] pause  [/] filter  [esc] back  ·  → 4 more columns ([c] to choose)  [?] keys  [q] quit
  ```

  `—` in ACTION and PLUGIN means no plugin acted on that message; a `tunnel`
  there is an opaque CONNECT, where METHOD and STATUS are blank too because
  opaque bytes carry no request line. The TOKENS and COST figures on a request
  row carry what `tool-prune` saved in parentheses — `−` for a counted saving,
  `~` for a projected one.
- **Detail**: pretty-printed JSON of a single event. Scroll with arrow
  keys; `y` yanks to `~/.cortex/abctl-events/<timestamp>-<rand>.json` and
  shows the path in the footer until you press another key. The directory is
  private to you (0700, inside `~/.cortex`) and the files are 0600 — yanked
  events carry identity subjects, raw LLM completions and tool arguments, so
  they are deliberately not written to a shared location such as `/tmp`.
  Nothing prunes them: they accumulate until you delete them, and unlike
  `$TMPDIR` this location is never cleared by the OS. Given what they hold,
  `rm` the ones you are done with. The footer shows the path expanded rather
  than abbreviated with `~`, and gives it the whole line while the notice is up;
  on a terminal too narrow for it the path is truncated from the left, so the
  filename stays readable.
- **Pipeline**: the active plugin chain in inbound + outbound order.
  Columns: position, direction, plugin name, DEPS (✓/✗ — see "Plugin
  dependencies" below), writes, body access, event count. `e` opens
  the editor. Outside the viewer, `abctl pipeline get` prints the same
  composition — plus each plugin's description and config — and `--json` emits
  `/v1/pipeline`'s own shape for a script. It has no DEPS or event count:
  one is derived from the chain rather than reported by the proxy, the other
  counts invocations in cached session events, which a one-shot command has
  none of.
- **Plugin detail**: drill-into-row for Pipeline or Catalog. Shows
  description, position, reads/writes, body access, plugin config, and
  per-dependency satisfaction status against the active chain.
- **Usage**: time-bucketed charts of volume, errors, latency and cost,
  opened by `u` from Sessions (all sessions) or Events/Detail (the
  selected session). Sourced from `/v1/usage`, which the proxy
  aggregates server-side — so every operator watching a pod sees the
  same history, including traffic from before they attached. Refetches
  every 20s while in view.

  `m` cycles the metric. Counts (tokens/requests/errors) render as bars;
  latency renders as mean-with-whiskers (`┼` mean, `┬`/`┴` ±1σ), because
  a bar encodes magnitude from a zero baseline and mean latency has no
  meaningful zero. `b` cycles the breakdown, which stacks each bar by
  status, model, plugin or host — each series marked with a letter derived
  from its name (`s` for claude-sonnet-5) on a coloured ground, so the
  chart reads without colour too. Statuses ≥400 render red. `b` is not
  offered for latency: the aggregator holds no per-label latency, so
  there is no per-status mean to plot.

  The host breakdown answers "where is my traffic going" — one band per
  upstream, so two agents sharing one Cortex are told apart by the hosts
  they call. Ports are folded in, so a bridged `api.anthropic.com:443`
  and the request inside that tunnel are one band. A request the listener
  recorded no host for joins the `(unlabelled)` remainder. Hosts are more
  numerous than models or statuses, so expect `(other)` here sooner.

  An idle bucket shows `0` rather than an empty column, so a gap in
  traffic is distinguishable from traffic too small to plot. A bucket
  carrying traffic no label claims shows an `(unlabelled)` band, and
  series past the palette fold into `(other)` — every band drawn has a
  legend entry.

  ```
  abctl · http://localhost:9094 · usage · all

    USAGE — all sessions — 10m0s @ 1m0s — tokens — by model

     12k                            ▄▄▄▄
                              ████  ssss
    9.6k        ▃▃▃▃          ssss  ssss
                ssss    ▅▅▅▅  ssss  ssss
    7.2k  ▂▂▂▂  ssss    ssss  ssss  ssss
          ssss  ssss    hhhh  hhhh  ssss
    4.8k  ssss  ssss    hhhh  hhhh  hhhh
          hhhh  hhhh    hhhh  hhhh  hhhh
    2.4k  hhhh  hhhh    ····  ····  hhhh
          ····  ····    ····  ····  ····
       0 ┼────┴────┴────┴────┴────┴────
         14:23   :25   :27   :29   :31
        6.1k  8.8k     0  11k   9.9k  12k

    s claude-sonnet-5 (34.2k)   h claude-haiku-4-5 (9.4k)   · (unlabelled) (2.1k)

    REQUESTS 412    ERRORS 3 (0.7%)    TOKENS 48.2k    LATENCY 1.31s    COST $0.94

    updated 7s ago (every 20s)

  [m] metric  [w] window  [b] breakdown  [s] this session  [esc] back  [?] keys  [q] quit
  ```

  Under `latency` the bars give way to mean-with-whiskers and the breakdown
  hint disappears, since the aggregator holds no per-label latency:

  ```
    USAGE — all sessions — 10m0s @ 1m0s — latency — no breakdown for latency

   2.4s              ┬
                     │       ┬
   1.8s        ┬     │       │
               │     ┼       │
   1.2s  ┬     ┼     │       ┼
         ┼     │     ┴       │
   600ms ┴     ┴           ┴
       0 ┼────┴────┴────┴────┴────
         14:23   :25   :27   :29
        1.2s  1.8s     0  2.1s  1.4s

    ┼ mean   ┬ +1σ   ┴ −1σ   (0 = no measured responses)
  ```

- **Catalog**: registered-plugin browser, opened by `P` from any
  session-view pane. Lists every plugin the running binary knows how to
  construct, including ones not in the active pipeline. Useful for
  discovering what's available before adding to the pipeline. Sourced
  from `/v1/plugins`.

  REQUIRES lists a plugin's hard dependencies comma-separated; an either-or
  group appears as one entry joined by `|`. Both are checked against the
  ACTIVE pipeline in the Pipeline pane's DEPS column, not here — this pane
  lists what the binary can build, not what is wired up.

  ```
  abctl · http://localhost:9094 · catalog

   NAME                    REQUIRES                      DESCRIPTION
   jwt-validation                                        Validate inbound JWTs against JWKS
   token-exchange                                        RFC 8693 exchange for a target audience
   mcp-parser                                            Parse MCP JSON-RPC requests and results
   inference-parser                                      Parse LLM chat/completions traffic
   ibac                    a2a-parser                    Intent-based access control
   tool-prune              inference-parser              Drop unused tool definitions

  [↑↓] nav  [↵] plugin detail  [r] refresh  [esc] back  [?] keys  [q] quit
  ```

- **Kubernetes Namespaces** (optional): the way in when abctl has no endpoint
  to connect to — one row per namespace holding an AuthBridge agent, then a
  Pods pane, then an automatic `kubectl port-forward` into the session view.
  Shown when `--endpoint` was not given and either no local Cortex is answering
  or `--kubernetes` was passed; `[l]` leaves it for the Cortex on this machine.

  ```
  abctl · pick namespace

   NAMESPACE                       PODS
   team1                           2
   team2                           1
   default                         1

  [↑↓/jk] nav  [↵] open  [l] localhost:47601  [r] reload  [?] keys  [q] quit
  ```

  Without `--kubernetes`, a running local Cortex is connected to directly and
  this pane never appears. With `--endpoint` the flag is moot: an explicit
  address always wins.

Layered on top of all of them:

- **Key help**: a modal overlay listing every keybinding, opened by `?`
  from anywhere (picker included). The current pane's bindings come
  first and are highlighted; the global keys and a one-line summary of
  every other pane follow. While it's up it owns the keyboard — `?`,
  `Esc`, or `q` closes it (`q` closes the overlay rather than quitting
  abctl). This is the discoverable home for keys the single-line footer
  has no room for, `P` among them. Two exceptions: while a pipeline edit
  is in flight that overlay is already modal and owns `y`/`N`, and while
  the filter input is focused `?` is a character you're typing (session
  IDs and hosts can contain one). In both cases `?` is inert until the
  keyboard is released.

  The body scrolls, so the full reference is reachable on a short
  terminal: `↑↓`/`jk` by line, `b`/`f` or PgUp/PgDn by page, `u`/`d` by
  half page, `g`/`G` to the ends. A `[↑↓] scroll  <n>%` affordance
  appears in the overlay's footer only when the content overflows; the
  close hint stays pinned there at every scroll position. Resizing the
  terminal re-ranges the body without losing your place.

## Keybindings

| Key | Context | Action |
|---|---|---|
| `?` | any (not while filtering or mid-edit) | open the key-help overlay (`?`/`Esc`/`q` closes) |
| `↑ ↓` / `k j`, `b`/`f`, `u`/`d`, `g`/`G` | key help | scroll the overlay |
| `↑ ↓` / `k j` | picker, list | navigate rows |
| `Enter` | namespaces | open the namespace |
| `l` | namespaces | connect directly to this machine's Cortex, or to `localhost:9094` when none is installed |
| `Enter` | pods | port-forward + connect |
| `Esc` | pods | back to namespaces |
| `r` | namespaces, pods | reload agent list from cluster |
| `Enter` / `→` / `l` | sessions, events | drill into selection |
| `Esc` / `←` / `h` | detail, events | back out |
| `Esc` | sessions, pipeline | (picker mode) tear down port-forward and back to pods |
| `/` | sessions, events | filter (substring match; Enter commits and saves, Esc cancels the edit and saves nothing; clear the box and press Enter to remove a saved filter) |
| `s` | events | toggle skip-row visibility (default: hidden; the events footer shows the hidden count) |
| `c` | events | open the column picker (`↑↓`/`jk` move, `space`/`x` toggle, `s` sort, `r` reset, `Esc`/`Enter`/`c` close); the selection and sort are saved on close |
| `s` | column picker | sort by the column under the cursor: descending → ascending → chronological. Pressing it on a different column starts that column descending. `#` is not sortable — its order already *is* chronological |
| `p` | any | pause/resume stream |
| `y` | detail | yank event JSON to `~/.cortex/abctl-events` (path stays until the next keypress) |
| `g` / `G` | lists | jump to top / bottom. In the events timeline this also sets where the *next* session opens — see [Where a session opens](#where-a-session-opens) |
| `u` | sessions, events, detail | open the usage charts (sessions: all sessions; events/detail: the selected session) |
| `$` | every pane except the two pickers and usage | expand the spend strip into a per-model breakdown, in place — the table stays on screen. Needs 26 rows; refuses on the two pickers (nothing is connected yet) and on the usage pane, which is already a breakdown with its own cycles |
| `a` | while the breakdown is open | cycle the axis: model / endpoint / agent. Not `g`, which is the global "jump to top" |
| `w` | while the breakdown is open | cycle the span: 15m / 1h / 6h |
| `m` | usage | cycle metric: tokens / requests / errors / latency |
| `w` | usage | cycle window: 10m / 1h / 6h |
| `b` | usage | cycle breakdown: none / status / method / plugin (not offered for latency — there is no per-label latency) |
| `s` | usage | toggle between this session and all sessions |
| `Esc` | usage | back to the pane it was opened from |
| `P` | any session-view pane (not the picker) | open the registered-plugin catalog |
| `r` | catalog | refresh the catalog from `/v1/plugins` |
| `e` | pipeline | edit pipeline subtree in `$EDITOR` |
| `y` | edit/diff | apply the edit |
| `N` | edit/diff | abort the edit |
| `r` | edit/error | retry: re-open the editor (post-edit failure) or refetch (fetch failure) |
| `Esc` | edit/{fetching,editing,applying} | abort the edit, return to Pipeline pane |
| `Esc` | edit/{waiting,rollback} | background the watch; result lands as a footer flash |
| `q` / `Ctrl+C` | any | quit (closes the key-help overlay first, if open) |

## Settings

abctl remembers the events-table column selection, the sort order, and the active
filter in `~/.cortex/abctl-config.yaml`. Columns and the sort are saved when the
column picker closes with `Esc`/`Enter`/`c` (`q` quits without saving); the filter is
saved when you commit it with `Enter`. There is no explicit save step.

A restored filter is shown in the footer as `[filter: …]` while it is in effect but
not being edited — otherwise a shortened list would have no explanation on screen.
Pressing `/` puts the cursor in the restored value so you extend it rather than
replace it, and `Esc` abandons the edit and puts the previous filter back without
writing anything. `Enter` is the only key that saves a filter, so clearing one means
emptying the box and pressing `Enter`. In picker mode the restored filter applies to
the first session view you open and is then dropped when you go back to the pod list:
a filter surviving a pod switch reads as data loss, so the active one is cleared while
the saved one stays on disk for the next start.

`--prefs PATH` reads and writes somewhere else. This is *not* the Cortex proxy
config — that is `~/.cortex/config.yaml`, and `--config` on `abctl service` and
`abctl configure claude-code`.

```yaml
# abctl user settings. Written by abctl; safe to hand-edit or delete.
# Columns not listed under events.columns are visible — only deviations are recorded.
events:
  columns:
    - name: COST
      visible: false
    - name: TOKENS
      visible: false
  # Omit sortColumn (or name a column this build does not have) for arrival order.
  sortColumn: DURATION
  sortDesc: true
filter: github-tool
```

### Schema

| Key | Type | Default | Meaning |
|---|---|---|---|
| `events.columns[].name` | string | required | column id, from the table below |
| `events.columns[].visible` | bool | required | show that column — an entry without it reads as `false` |
| `events.sortColumn` | string | unset | sort by this column; unset means arrival order. `#` is not sortable, being arrival order already |
| `events.sortDesc` | bool | `false` | sort descending; ignored unless `sortColumn` names a sortable column |
| `filter` | string | empty | the active filter |
| `usage.metric` | string | `tokens` | usage-pane metric: `tokens`, `requests`, `errors` or `latency` |
| `usage.window` | string | `10m0s` | usage-pane window: `10m0s`, `1h0m0s` or `6h0m0s` |
| `usage.group` | string | `none` | usage-pane breakdown. `[b]` cycles `none`, `status`, `method`, `plugin`, `host`; a hand-edited file may also use `model`, `endpoint`, `session` or `agent` |

List a column only to change it — the twelve are all visible until you hide one, and
an unrecognised name or sort column is ignored. Inside an entry, always write
`visible:` explicitly: it is optional to the parser but reads as `false`, so
`- name: COST` on its own hides COST rather than showing it.

The three `usage.*` keys are the view `[m]`, `[w]` and `[b]` choose, saved as you
change them, so reopening the pane after a restart lands on the view you left. They
are stored by name rather than by position, and a name this build does not have —
a typo, or a value from a newer abctl — falls back to that field's default, so it
costs you the setting and never a broken pane. The four extra groupings above are
recognised, not defaulted: `[b]` does not cycle to them, but the pane renders them
and they survive a restart.

Column ids, in display order — the same headers the picker shows:

| Id | Shows |
|---|---|
| `#` | exchange number; a request and its response share one — quote it as `"#"` in YAML, or it reads as a comment |
| `TIME` | wall-clock time the message was recorded |
| `DIR` | `in` = toward your agent, `out` = toward an upstream |
| `PHASE` | `req`, `resp`, or `denied` |
| `ACTION` | what took effect: `deny`, `modify`, `observe`, `allow`, or `tunnel` |
| `PLUGIN` | which plugin acted; blank when none did |
| `METHOD` | protocol operation: model name, MCP or A2A method |
| `STATUS` | HTTP status of the response |
| `DURATION` | how long the exchange took |
| `TOKENS` | tokens used, and what `tool-prune` saved |
| `COST` | estimated cost, and what `tool-prune` saved |
| `HOST` | host the message was sent to |

A narrow terminal also hides columns to fit, with a `→ N more columns` note in the
footer; that is not saved, and widening the window brings them back.

Each setting comes from **this file, or the built-in default** when the file does not
set it. A missing file is normal and silent. An unreadable or malformed one is
reported on stderr and ignored in full — never partially applied, and never fatal.

Nothing else feeds a setting: there is no environment variable (no `ABCTL_*`, and
`XDG_CONFIG_HOME` is not consulted) and no flag for an individual setting. `--prefs`
chooses *which* file, never what is in it — so to try a layout without disturbing
your own, point it at a throwaway file. Otherwise change the setting in the TUI,
which saves it, or edit the YAML.

## Where a session opens

Opening a session puts the cursor on the end you last jumped to with `g` or `G`:
the newest event by default, or the oldest if `g` was the last end you asked for.
Those two keys already mean "take me to an end", so they double as the preference
and it persists with your other view settings. An arrow key that happens to reach
row 0 does not change it — scrolling up to read is navigation, not a preference.

Only the *opening* chooses an end. Every later rebuild — the two-second poll, a
filter, a column toggle — leaves the cursor where you put it, and the timeline
follows new events only while you are on the newest row. Under a column sort the
preference does not apply at all: there "an end" is the largest or smallest value
rather than the oldest or newest event, so the cursor stays pinned to the event you
were reading instead.

## How a timeline is fetched

The events table renders no message body, so it does not ask for one. Each fetch
sends `?view=summary`, which drops the conversation payloads and keeps everything
the table shows **or its filter searches** — measured at ~163x smaller, and the
difference between a session that opens in milliseconds and one that takes seconds.

The filter is the part worth spelling out, because it is easy to assume otherwise:
`/some text` still matches completion and A2A message text, and `plugin:<name>`
still works, because those fields are searched and so are kept. Dropping them would
have been ~299x instead of ~163x — both about a megabyte for a 1000-event session,
so the filter is worth far more than the difference.

The consequence is worth knowing: the first `↵` on a row fetches that one event in
full from `/v1/sessions/{id}/events/{seq}`. The detail pane renders the summary
immediately and the bodies appear when they arrive, so there is no loading screen —
but on a slow link the message text lands a moment after the rest. Re-opening the
same row is free; the bodies are kept. If the fetch fails, the pane keeps what it
has and the error goes to the footer.

`y` yanks whatever the pane is showing, so while the bodies are still in flight — or
after a failed fetch — it says so in the flash rather than handing you a body-less
event that looks complete.

Because a projected event is ~1KB rather than ~200KB, abctl asks for the server's
full 2000-event ceiling instead of the old 500. A normal session therefore arrives
whole, and the `N older ([o] to load)` note appears only for genuinely long ones.

Against a proxy that predates `?view=summary` this still works — that server
ignores the parameter and returns full events, so the timeline is correct and as
slow as it used to be. abctl can tell the difference from the response and does not
waste a per-row fetch on bytes it already holds.

## Editing the pipeline

Press `e` on the Pipeline pane to edit the runtime `pipeline:` subtree in
`$EDITOR` (or `vi` if unset). On save, abctl shows a diff and asks
`apply this change? (y/N)`. Confirming writes the change back, then polls the
framework's `/reload/status` until the reload completes (success or failure).

Where it writes depends on what you are connected to, not on how abctl was
started:

| Connected to | `e` writes | Reload confirmed via |
|---|---|---|
| a pod, via the picker | `kubectl apply --server-side` against the per-agent ConfigMap, `--field-manager=abctl --force-conflicts=true` (taking ownership of `data.config.yaml` from the operator's webhook on first edit) | the port-forward's `:9093/reload/status` |
| the Cortex on this machine | `~/.cortex/config.yaml` directly — the file that proxy was started with and already watches | that proxy's own stats address, `stats.address` in the same file |

The local path needs neither kubectl nor a ConfigMap: the proxy watches its
config file with fsnotify, so writing the file *is* the apply. abctl writes a
temporary sibling and renames it over the target, so the watcher only ever sees
a complete file — a half-written one would book a reload failure against an
edit you never made.

Local editing is offered whenever the endpoint on screen is this machine's
Cortex: a bare `abctl` that auto-connected to it, `[l]` from the picker, or an
explicit `--endpoint` aimed at its session API. Loopback spellings are
interchangeable, so `--endpoint http://localhost:47601` and
`http://127.0.0.1:47601` both match a config bound to either. A local Cortex
merely *running* is not enough — while you are looking at a pod, `e` edits the
pod.

If neither target applies, `e` says so and names the remedy instead of starting
an edit it cannot finish.

A **symlinked** `~/.cortex/config.yaml` — pointing at a dotfiles repo, say — is
written *through*, not over. `rename(2)` replaces the link itself, so without
resolving it the live config would become a regular file while the tracked copy
silently kept the old pipeline, with nothing in `git status` to show it.
Resolving also keeps the temporary file a sibling of the real one, which the
atomicity guarantee needs: a rename across filesystems fails `EXDEV`.

That relies on the proxy watching the resolved file's directory, which the
reloader does — but the reloader ships in `authbridge-proxy`, and that installs
separately from abctl. **A proxy started before that change still has the old
single watch**, so on Linux a symlinked config will not observe the write: the
poll times out and rolls the edit back. Run `abctl service restart` after
upgrading the proxy. abctl cannot detect the mismatch — nothing the proxy
exposes describes its watcher — so this is a note rather than a check.

**A concurrent write aborts the apply.** This file has other writers — `abctl
tools scan --write`, the config migration `abctl service install` runs, a second
abctl session — and `$EDITOR` can be open for minutes. The apply re-reads the file
first and refuses
if it moved, rather than renaming a whole file built from stale bytes over
somebody else's change. The refusal names the likely culprits; re-open the edit
to work from the current file. This is a compare, not a lock, so a writer
landing in the last moment before the rename still wins — it closes the
realistic window, not every window. The cluster path has no equivalent check on
purpose: it applies with `--force-conflicts=true` and takes field-manager
ownership, which is what makes editing an operator-owned ConfigMap possible.

A config with no top-level `pipeline:` key is reported as such; inventing the
block is not the editor's job.

The single edit flow covers four operations:
- **Edit a value** — change a config field of an existing plugin
- **Reorder** — move a plugin's lines up or down
- **Remove** — delete a plugin's entry from `inbound:` or `outbound:`
- **Add** — append a new plugin entry

All four work because they're all just lines you change inside the
pipeline subtree.

`e` needs a target it can write. That means either a pod chosen through the
picker, or a connection to the Cortex on this machine — including via
`--endpoint`, as described above. Pointed at anything else (a hand-run
`kubectl port-forward`, a remote session API), neither is available: there is no
pod identity to resolve a ConfigMap from and no local config file to write, so
pressing `e` flashes a hint naming the remedy instead of opening an edit it
cannot finish.

### Pre-apply validation

After save, abctl runs the same Requires/RequiresAny/After/Claims
checks the framework runs at reload-time, against the cached
`/v1/plugins` catalog. Issues land as a red banner above the diff in ~50ms
instead of at hot-reload — which means after the kubelet sync (~60s) in a
cluster, or about a second later locally:

```text
⚠ 1 validation issue — framework reload will reject:
  • [outbound] ibac pos 1: Requires "mcp-parser", but it is not in the outbound chain
```

The y/N prompt becomes "apply anyway? (y/N)" — abctl's check is
non-blocking. The framework's own validateRelationships is the
source of truth and will fire again at reload regardless.

Validation is silently skipped when the catalog isn't loaded
(operator hasn't pressed `P` yet). Visit the catalog pane once to
populate it for the rest of the session.

### Agent-name resolution

Cluster path only — a local edit has no agent and skips all of this.

The per-agent ConfigMap is named `authbridge-config-<agent>`. abctl
resolves `<agent>` from the selected pod's `app.kubernetes.io/name`
label (operator sets this). If the label is absent, abctl
falls back to stripping the last two dash-separated segments of the
pod name (the ReplicaSet hash + pod suffix).

### Auto-rollback on reload failure

If the write succeeds but the reload fails (unknown plugin name, malformed
config, validation error), the framework keeps the previous in-memory pipeline
serving requests — but the stored config now holds the bad YAML. abctl detects
this via `/reload/status` and re-applies the content captured at Fetch time,
reconciling the stored state back to what is actually running.

The error overlay names the target it reconciled:
`reload failed: <reason>; rolled back to previous ConfigMap` in the cluster, and
`…rolled back to previous config file` locally. If the rollback itself also
fails, the message says where to look — `check kubectl` for a pod, the config's
own path for a local edit.

The rollback is best-effort in both, for the same reason and by different
mechanisms. In the cluster, `--force-conflicts=true` means a third party
(controller, `kubectl edit`, kustomize) who modified the ConfigMap between Fetch
and the failed reload has their change overwritten. Locally the forward apply is
guarded by the staleness check described above, but the rollback deliberately is
not — it runs *because* the file changed, so checking would refuse every
rollback. The running pipeline is unaffected either way.

### Backgrounding the watch

Pressing `Esc` while waiting for hot-reload (or during rollback)
moves the watch to the background instead of aborting it. The
overlay closes, the footer flashes
`hot-reload watch moved to background; you'll be notified`, and you
can resume navigating the TUI. When the watch terminates, the
result lands as a one-line flash:

- `hot-reload succeeded`
- `hot-reload failed: <reason>; rolled back to previous ConfigMap` — or
  `previous config file` for a local edit
- `hot-reload failed: <reason>; rollback failed: <err>` (rare)

Flashes auto-dismiss after a few seconds; if you miss one, query
`/reload/status` directly — through the port-forward for a pod, or at the
`stats.address` in `~/.cortex/config.yaml` for a local Cortex.

### Permissions

abctl shells out to `kubectl`; kubectl uses your kubeconfig. Editing
requires `update` on `configmaps` in the agent's namespace (in
addition to `get pods` which the picker already needs). RBAC denial
surfaces verbatim in the overlay.

### Tempfile lifecycle

abctl writes the editable pipeline subtree to `$TMPDIR/abctl-pipeline-*.yaml`
on every edit. The tempfile is **left in place on every exit path**
(success, error, abort) so an interrupted edit is recoverable. On
abctl launch, files older than 24h in this glob are swept
automatically — no manual cleanup needed.

### Hot-reload window

The framework reloads via a config-file watcher, so how long the wait is depends
on how the edit reaches that file. In a cluster, kubelet syncs ConfigMap edits
into the pod's mount within ~60s and the framework then debounces and reloads —
typically under 90s wall-clock from apply. Locally there is nothing to sync: the
proxy is already watching the file abctl wrote, so a reload normally lands in
about a second. The overlay says which of the two you are waiting on. abctl shows
a spinner either way.

The poller terminates with one of:

- **Success** — `/reload/status.last_success` advances past the apply
  time.
- **Failure** — `reloads_failed` increments past its baseline; the
  framework's `last_error` is shown.
- **Unreachable** — 5 consecutive transport errors against `/reload/status`
  surface as `reload status endpoint unreachable` after a few seconds rather than
  waiting the full deadline. The endpoint is the port-forward's `:9093` for a pod
  and the local proxy's `stats.address` otherwise, so the message asks the
  question that fits: a dropped port-forward or crashed framework in the cluster,
  or simply whether the local proxy is still running.
- **Timeout** — none of the above within the target's deadline: 120s for a
  ConfigMap, sized for the kubelet sync, and 30s for a local file, which lands
  in about a second. Triggers an auto-rollback so the stored config doesn't
  drift from the running pipeline. A ceiling, not a wait — the poll returns the
  moment `last_success` moves, so this only bounds how long a *failed* reload
  takes to be declared.

## Plugin dependencies

Plugins declare dependencies in their `Capabilities()`:

- **Requires**: hard dependency. The named plugin MUST be in the same
  chain at a strictly-lower position; otherwise framework reload fails.
- **RequiresAny**: soft OR. At least one of the listed plugins must
  appear upstream; each one that IS present must be earlier.
- **After**: ordering hint. If the named plugin IS present, it must
  appear earlier; absent is OK.
- **Claims**: exclusive ownership. Within one chain, two plugins
  cannot both declare the same claim string.

abctl surfaces these in three places:

- **Pipeline pane DEPS column**: ✓ when all declared deps satisfied,
  ✗ when any fail, blank when no deps declared. The footer hint
  reports the count of plugins with unmet deps.
- **Plugin detail pane**: per-dependency rows with ✓/✗ and the
  satisfying upstream's position when applicable.
- **Pre-apply validation in the editor**: catches missing/misordered
  Requires before the write goes out (~50ms, against a framework roundtrip of
  ~60s in a cluster or ~1s locally). See the "Pre-apply validation" subsection
  above.

The framework's own validateRelationships is the source of truth and
runs at every reload. abctl's checks are the fast-feedback layer.

## Trust model

`abctl` does no authentication — same as the server. Use only against
sidecars reachable via in-cluster networking or a local port-forward.
Session events contain raw user messages, LLM completions, and tool
results; treat the output accordingly.

## Architecture

- `apiclient/` — HTTP + SSE client. Sole owner of the `:9094` wire format.
  Auto-reconnects with exponential backoff (1s → 30s, capped, indefinite).
- `tui/` — Bubble Tea model/update/view. All state mutation runs on the
  Tea event loop; the SSE goroutine produces messages the loop consumes.
- `main.go` — flag parsing, signal handling, wires `tui.Run`.

## Deferred to later PRs

- Native clipboard (currently writes a file under `~/.cortex/abctl-events`).
- More persisted settings (#954): pane sizes, theme. Each needs the setting itself
  before there is anything to persist — pane sizes are recomputed per frame, and
  there is no theme to choose. (Sort order is done: see the column picker's `s`.)
- Fuzzy search beyond substring match.
- Per-user filtering (`Identity.Subject == X`).
- Krew plugin packaging.
