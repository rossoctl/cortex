# abctl

Interactive terminal UI for inspecting AuthBridge's in-memory session store.
`abctl` connects to the session API exposed by an AuthBridge sidecar
(default `http://localhost:9094`, typically reached via `kubectl port-forward`)
and lets you browse active sessions, follow a session's event stream live,
and read individual events as pretty-printed JSON.

```
┌─ abctl · http://localhost:9094 ────────────────────────────────┐
│ ID                       UPDATED    EVENTS  ACTIVE             │
│ ► ctx-abc-1234…          3s ago     42      ●                  │
│   ctx-def-5678…          18m ago    15                         │
│   default                1h ago     8                          │
│                                                                 │
│ ● connected   2.1 ev/s   drops: 0                              │
│ [↑↓/jk] nav  [↵] drill  [/] filter  [?] keys  [q] quit         │
└─────────────────────────────────────────────────────────────────┘
```

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

Press `l` on the Namespaces pane to skip the cluster entirely and
connect straight to `http://localhost:9094` — the session API's default
port on the local host. Useful when you already have your own
`kubectl port-forward` running, when abctl runs inside the mesh, or when
your kubeconfig can't list pods but a tunnel is up.

abctl probes `/v1/sessions` before switching panes, so an endpoint with
nothing listening surfaces as a footer error and leaves you in the
picker rather than dropping you into a silently empty session view.
`Esc` from a session entered this way returns to the Namespaces pane
(there's no pod to go back to). Pipeline editing (`e`) is unavailable,
same as `--endpoint` mode: the cluster fields needed to fetch and apply
the ConfigMap aren't populated.

### Power-user / scripting bypass

Pass `--endpoint` to skip the picker entirely:

```sh
kubectl port-forward -n team1 pod/weather-agent-xxxx 9094:9094 &
./abctl observe --endpoint http://localhost:9094
```

This preserves the pre-picker behavior for scripts, CI, or remote
session APIs that aren't in your kube context.

## Running one command through Cortex (`abctl exec`)

`abctl claude-code enable` works because Claude Code has a settings file:
the variables can be written once and reach every session on the machine,
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
roots). These are the same values `abctl claude-code enable` writes into
`settings.json`; `exec` reuses that derivation rather than repeating it, so the
two commands cannot disagree.

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
`claude-code enable` uses, so the two produce identical values for the same
Cortex. Nothing is
exported to your shell and no file is modified.

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
command and supplies none. The paths `--print` hands out are
meant to be kept, and are the same ones `abctl claude-code enable` writes into
`settings.json`; running a command is the opposite, applying them to one process
for its lifetime. Asking for both in one invocation is a contradiction about
which you want, so abctl says so rather than picking one.

`abctl claude-code enable` shares this requirement as of the same change: it too
refuses `tls_bridge.mode: disabled` with a `ca_dir` set, a combination it used to
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
  active marker.
- **Events**: per-session event table. `c` opens a column picker — a popup with
  a checkbox and a one-line description per column, since twelve abbreviated
  headers are not self-describing.

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
  the editor.
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
  status, model or plugin — each series marked with a letter derived
  from its name (`s` for claude-sonnet-5) on a coloured ground, so the
  chart reads without colour too. Statuses ≥400 render red. `b` is not
  offered for latency: the aggregator holds no per-label latency, so
  there is no per-status mean to plot.

  An idle bucket shows `0` rather than an empty column, so a gap in
  traffic is distinguishable from traffic too small to plot. A bucket
  carrying traffic no label claims shows an `(unlabelled)` band, and
  series past the palette fold into `(other)` — every band drawn has a
  legend entry.

- **Catalog**: registered-plugin browser, opened by `P` from any
  session-view pane. Lists every plugin the running binary knows how to
  construct, including ones not in the active pipeline. Useful for
  discovering what's available before adding to the pipeline. Sourced
  from `/v1/plugins`.

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
| `l` | namespaces | connect directly to `localhost:9094` |
| `Enter` | pods | port-forward + connect |
| `Esc` | pods | back to namespaces |
| `r` | namespaces, pods | reload agent list from cluster |
| `Enter` / `→` / `l` | sessions, events | drill into selection |
| `Esc` / `←` / `h` | detail, events | back out |
| `Esc` | sessions, pipeline | (picker mode) tear down port-forward and back to pods |
| `/` | sessions, events | filter (substring match; Enter commits, Esc cancels) |
| `s` | events | toggle skip-row visibility (default: hidden; the events footer shows the hidden count) |
| `c` | events | open the column picker (`↑↓`/`jk` move, `space`/`x` toggle, `r` reset, `Esc`/`Enter`/`c` close); the selection is saved on close |
| `p` | any | pause/resume stream |
| `y` | detail | yank event JSON to `~/.cortex/abctl-events` (path stays until the next keypress) |
| `g` / `G` | lists | jump to top / bottom |
| `u` | sessions, events, detail | open the usage charts (sessions: all sessions; events/detail: the selected session) |
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

abctl remembers the events-table column selection and the active filter in
`~/.cortex/abctl-config.yaml`. Columns are saved when the column picker closes with
`Esc`/`Enter`/`c` (`q` quits without saving); the filter is saved when you commit it
with `Enter` or clear it with `Esc`. There is no explicit save step.

`--prefs PATH` reads and writes somewhere else. This is *not* the Cortex proxy
config — that is `~/.cortex/config.yaml`, and `--config` on `abctl service` and
`abctl claude-code`.

```yaml
# abctl user settings. Written by abctl; safe to hand-edit or delete.
# Columns not listed under events.columns are visible — only deviations are recorded.
events:
  columns:
    - name: COST
      visible: false
    - name: TOKENS
      visible: false
filter: github-tool
```

**Columns not listed are visible.** The file records only what you changed, so a
column added in a later abctl shows up rather than staying hidden because your file
predates it. A column id this build does not recognise is ignored.

Settings resolve **flags > this file > built-in defaults**. (No environment variable
feeds any of these today; if one is ever added it sits between the two.) A missing
file is normal and silent. An unreadable or malformed one is reported on stderr and
ignored in full — never partially applied, and never fatal.

## Editing the pipeline

Press `e` on the Pipeline pane to edit the agent's runtime `pipeline:`
subtree in `$EDITOR` (or `vi` if unset). On save, abctl shows a diff
and asks `apply this change? (y/N)`. Confirming runs
`kubectl apply --server-side` against the per-agent ConfigMap with
`--field-manager=abctl --force-conflicts=true` (taking ownership of
`data.config.yaml` from the operator's webhook on first
edit), then polls the framework's `/reload/status` until the reload
completes (success or failure).

The single edit flow covers four operations:
- **Edit a value** — change a config field of an existing plugin
- **Reorder** — move a plugin's lines up or down
- **Remove** — delete a plugin's entry from `inbound:` or `outbound:`
- **Add** — append a new plugin entry

All four work because they're all just lines you change inside the
pipeline subtree.

`e` is only available in picker mode. With `--endpoint`, the cluster
fields needed to fetch and apply aren't populated; pressing `e`
flashes a hint instead of opening a broken edit.

### Pre-apply validation

After save, abctl runs the same Requires/RequiresAny/After/Claims
checks the framework runs at reload-time, against the cached
`/v1/plugins` catalog. Issues land as a red banner above the diff
in ~50ms instead of waiting through the kubelet sync (~60s) to
discover them at hot-reload:

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

The per-agent ConfigMap is named `authbridge-config-<agent>`. abctl
resolves `<agent>` from the selected pod's `app.kubernetes.io/name`
label (operator sets this). If the label is absent, abctl
falls back to stripping the last two dash-separated segments of the
pod name (the ReplicaSet hash + pod suffix).

### Auto-rollback on reload failure

If `kubectl apply` succeeds but the in-pod reload fails (unknown
plugin name, malformed config, validation error), the framework
keeps the previous in-memory pipeline serving requests. The on-disk
ConfigMap, however, now holds the bad YAML. abctl detects this via
`/reload/status` and re-applies the original ConfigMap content
captured at Fetch time, reconciling the on-disk state back to what's
actually running. The error overlay then reports
`reload failed: <reason>; rolled back to previous ConfigMap`.

The rollback is best-effort — with `--force-conflicts=true`, if a
third party (controller, kubectl edit, kustomize) modified the
ConfigMap between Fetch and the failed reload, the rollback
overwrites their change. The running pipeline is unaffected.

### Backgrounding the watch

Pressing `Esc` while waiting for hot-reload (or during rollback)
moves the watch to the background instead of aborting it. The
overlay closes, the footer flashes
`hot-reload watch moved to background; you'll be notified`, and you
can resume navigating the TUI. When the watch terminates, the
result lands as a one-line flash:

- `hot-reload succeeded`
- `hot-reload failed: <reason>; rolled back to previous ConfigMap`
- `hot-reload failed: <reason>; rollback failed: <err>` (rare)

Flashes auto-dismiss after a few seconds; if you miss one, query
`/reload/status` directly via the port-forward.

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

The framework reloads via a config-file watcher; kubelet syncs
ConfigMap edits into the pod's mount within ~60s, then the framework
debounces and reloads. Total wall-clock from `apply` to reload is
typically under 90s. abctl shows a spinner during the wait.

The poller terminates with one of:

- **Success** — `/reload/status.last_success` advances past the apply
  time.
- **Failure** — `reloads_failed` increments past its baseline; the
  framework's `last_error` is shown.
- **Unreachable** — 5 consecutive transport errors against
  `:9093/reload/status` (port-forward dropped, framework crashed,
  etc.) surface as `reload status endpoint unreachable` after a few
  seconds rather than waiting the full deadline.
- **Timeout** — none of the above within 120s. Triggers an
  auto-rollback so the on-disk ConfigMap doesn't drift from the
  running pipeline.

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
  Requires before kubectl apply (~50ms vs ~60s framework roundtrip).
  See the "Pre-apply validation" subsection above.

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
- More persisted settings (#954): sort order, pane sizes, theme. Each needs the
  setting itself before there is anything to persist — the events table has no sort
  state, pane sizes are recomputed per frame, and there is no theme to choose.
- Fuzzy search beyond substring match.
- Per-user filtering (`Identity.Subject == X`).
- Krew plugin packaging.
