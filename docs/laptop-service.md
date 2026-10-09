# Running Cortex: start, stop, and remove it

Cortex runs as a service, so there is nothing to launch by hand and nothing to keep
in a terminal. Everything below is `agentop service`; run it with no action to see the
list.

```sh
agentop service status      # is it running, and is it answering?
agentop service start       # start it
agentop service stop        # stop it, and keep it stopped
agentop service restart     # stop and start
agentop service install     # set it up in the first place (agentop setup does this)
agentop service uninstall   # stop it and remove the service
```

Two commands cover the whole install rather than just the service. `agentop doctor`
runs setup's checks, and its own of the CA, without changing anything — the binaries,
PATH, the config, the service, Claude Code's routing — and ends each problem with the
command that fixes it, usually `agentop setup`. `agentop uninstall` removes it, and
`~/.cortex` too with `--purge`, as [Remove it](#remove-it) describes.

**Never use `kill` or `pkill` to stop it.** The proxy is supervised, so killing it gets
it restarted within a couple of seconds, which looks like a process refusing to die.
`agentop service stop` is the stop that works.

That restart is the point, though, and it is worth seeing once:

```sh
kill -9 $(pgrep -f 'cortex --config')             # comes back within ~2s
agentop service status                            # healthy again
```

On macOS you will see **two** `cortex` processes: a supervisor (the one
launchd starts, holding no ports) and the proxy itself. launchd does not restart user
agents added mid-session — verified across `KeepAlive`, `StartInterval` and
`RunAtLoad` — so the supervisor is what makes crash recovery work. On Linux there is
one process; systemd handles it.

## Re-running the installer is safe

The one-liner is how you upgrade, so it is meant to be run repeatedly. When nothing has
changed it changes nothing. It still downloads the release, but `agentop setup` finds
the staged binaries identical to the installed ones, asks nothing, and ends on one line,
`cortex v0.9.3 is installed and healthy.`, leaving the running proxy alone rather than
restarting it. When something has changed it lists what, and asks first. An upgrade keeps
the previous binaries until the new Cortex answers its health check, and puts them back
if it never does.

That last part matters, though less than it used to read here. A restart cuts every
connection attached to the proxy, and `HTTPS_PROXY` is fixed in each client's environment
at startup so it cannot fall back to a direct connection — but it does reconnect through
the proxy on its next request. Measured across three restarts, time from bind to first
request served: **0.92s, 0.81s, 0.59s**, with the attached Claude Code sessions carrying
on through all three.

These three numbers are the only copy: `cmd_service.go` and its tests point here rather
than repeating them, so a re-measurement changes one place and not four.

So what a restart costs is the requests in flight at that moment, not the sessions. A
session that reports an error has lost one request and will recover; it does not need
restarting.

A restart does not wait for those requests either. On a stop the proxy closes every port at
once and exits, typically in well under a second, and its supervisor kills one that has not
gone within 3s — inside launchd's 5s, so no proxy is ever left behind holding the ports. The
proxy that replaces it binds its ports before it opens anything in `~/.cortex`, then replays
the session archive (up to 5s, see below) before it serves; a client that connects during
the replay is answered when it ends rather than refused. When a restart genuinely is needed, `agentop service install` says how many
connections it is about to cut. Setup does not pass that line on.

To restart deliberately: `agentop service restart`.

## How traffic is grouped into sessions

Each Claude Code session gets its own bucket, named with that session's id — the same
UUID Claude Code uses for its own transcript, so `agentop` and
`~/.claude/projects/<project>/<id>.jsonl` agree on what a session is. Two windows open in
different repos are two buckets, and resuming a session (`claude --resume`) files back
into the original one rather than starting a new one.

This works because Claude Code puts `X-Claude-Code-Session-Id` on every inference
request. Bob, our internal coding agent (not the `bob` demo user), is grouped the same
way, by the `X-Task-Id` it sets, but its buckets are named with a task id rather than a
session uuid, so one bucket covers however long Bob reuses that task. Note that
`X-Task-Id` is a generic name: traffic from anything else that sends it will be grouped
under its value too. [OpenCode](agents/opencode.md) is grouped by the `X-Session-Id` its
background service sets, and its buckets are named with OpenCode's `ses_…` session ids.
OpenCode puts its session's affinity id there: the parent session's id for a subagent
and the source session's for a fork, so a subagent or fork is filed under the session it
belongs to, as Claude Code's subagents are. Cortex reads `X-Session-Id` on a laptop
install: it is in the list the installer writes, and in the built-in default wherever
every listener binds loopback (`listener.bind_loopback_only`). A cluster deployment's
default does not read it, because it is a generic name as well, which Pi (Inflection AI)
and similar frameworks send too. OpenCode also sends `X-Opencode-Session-Id`, the
session's own id, which is not read. Traffic that carries no such header is filed by the
process that sent it (see below), and where that has no answer falls back to the previous
behavior — the most recently active session, or the `default` bucket. In practice
`default` collects Claude Code's own connectivity probe (`HEAD /api/hello`) and anything
else that egresses through the proxy without announcing a session.

Some limitations worth knowing:

- **Where the process lookup has no answer, header-less requests go by agent.** MCP tool calls,
  Claude Code's WebFetch, and Bob's startup probes and task-classifier completions carry
  no session header. Filed by timing alone — under whichever session was most recently
  active — they are right when sessions take turns and wrong when two agents run side by
  side: Bob's classifier completions — real inference, with tokens and cost — land in
  Claude Code's session, and Claude Code's WebFetch in Bob's. Inference is exact only
  where it carries the header.

  Client affinity (`session.client_affinity`) files a header-less request under the newest
  session of the same agent (by `User-Agent`). An agent's calls before its first header wait in a
  `pending:<agent>` bucket that its first session then absorbs, and a request from no
  known agent goes to `default` while two agents have both sent traffic in the last five
  minutes (a bridged tunnel's own row goes wherever its first decrypted request goes, directly before
  it; an opaque tunnel's row keeps today's attribution). Two sessions of the SAME agent
  still share by timing. It is on by default, including for a `~/.cortex/config.yaml`
  written before the setting existed; `session.client_affinity: false` turns it off and
  puts every header-less request back on timing. It is not hot-reloadable — restart the
  proxy after changing it.
- **On a laptop, header-less requests are attributed by process.** Cortex asks the
  kernel which process opened each connection, and files a request with no session
  header under the session of that process or of its nearest ancestor that named one —
  so `gh`, `git` and `curl` run by an agent's shell, its `WebFetch` and its MCP calls land
  in the agent's session, including opaque tunnels that cannot be decrypted. A process of
  no agent (your own terminal's `curl`) lands in `default` while any agent has named a
  session in the last five minutes, and otherwise in the most recently active session. An
  agent talking to its own service on this machine — OpenCode's TUI and its background
  service — is forwarded without being recorded when the service runs the agent's own
  executable and is either the agent's direct child or has named a session. OpenCode
  starts its service as the TUI's child, so its polling stays out of the timeline from
  the first request, including after a proxy restart. The first time it is skipped
  Cortex logs which program and service. It is `session.process_attribution`: `auto` (the default) means on for this
  loopback-only install and off in a cluster; `on` and `off` force it. Where the lookup is
  unavailable it logs one warning at startup and client affinity applies. Not
  hot-reloadable.
- **Agents other than Claude Code, Bob and OpenCode need to be named.** Set
  `session.id_headers` to a list of headers to consult in precedence order if you run a
  client with its own session header; naming any replaces the built-in list rather than
  adding to it. An explicit empty list (`session.id_headers: []`) turns grouping off and
  puts everything back in one bucket.
- **Bucket names are taken on trust.** The id comes from the client's own header and is
  not authenticated, so a client can name any bucket — including another session's, or a
  stream of ids nobody owns, which evicts real buckets once there are more than
  `session.max_sessions` of them (see below). On a laptop that is a non-issue: you own
  every session. It matters where telemetry attribution is a trust boundary rather than a
  convenience, and `session.id_headers: []` is the way to opt out there.

## Why traffic disappears from `agentop`

One limit by default, and it is not a clock:

| Limit | Default | Effect |
|---|---|---|
| `session.max_events` | **unset — unlimited** | when set: oldest events drop, the session stays |
| `session.max_sessions` | 100 | whole sessions evicted, least-recently-used first |

- `max_events` no longer defaults to a cap at all, so a session does not lose its oldest
  rows while it is alive. It used to trim to 500 per bucket, which made the store lossy on
  exactly the sessions worth reading — a long run dropped the beginning of its own story,
  and the trim point was invisible from the timeline. Set it, and it is **per bucket**: each
  session gets its own allowance rather than sharing one ring with every other session on
  the machine.
- With `max_events` unset, `max_sessions` bounds how many sessions are kept and nothing
  about how large one gets. A single session that never ends grows until the process does;
  that is the case to set `max_events` (or a `session.ttl`) for. Repeated message text is
  stored once per session rather than once per turn, which cut heap by 10.4x on a
  300-turn measurement — so "5000 events" costs far less than multiplying by a request
  size suggests, but it is not free.
- `agentop` fetches the **most recent** 500 events of a session, not all of them, and says
  so in the events footer (`· N older not fetched`). A session's whole history can be a
  gigabyte of JSON; asking for it took 17s and timed out at 10, which showed up as an
  events pane holding only what arrived after you opened it.
- Opening a session no longer costs the proxy memory. The snapshot response is written one
  event at a time, so serving it takes heap proportional to a single event; it used to be
  encoded whole before the first byte went out, which meant a couple hundred megabytes of
  resident memory per session you opened, kept for the life of the process. If you are
  looking at an older build and wondering why RSS climbs in steps as you browse rather
  than as traffic arrives, that is why — one step per <kbd>Enter</kbd>.
- `max_sessions` is **reachable in normal use**, which it effectively was not before.
  Every `claude` invocation mints a new bucket, so the 101st session on a busy machine
  evicts the least-recently-updated one — whole session and all. If an older session has
  vanished from `agentop` entirely rather than just losing its oldest rows, this is why.
  Raise `session.max_sessions` if you work across many sessions and want them to stay.

  This cap is also the only thing bounding the churn described above: because bucket names
  are unauthenticated, anything sending unfamiliar ids consumes the same 100 slots and
  evicts real sessions. Raising the cap trades eviction for memory; it does not remove the
  effect. On a laptop the only thing minting ids is your own tooling, so in practice this
  reads as a capacity setting — it stops reading that way on a proxy several clients share.

Sessions do **not** expire on time. They used to, after 30 minutes idle, which read as
data loss — traffic vanished because you stepped away, not because anything overflowed.
Set `session.ttl` (e.g. `30m`) if you would rather raw prompts not sit in memory
indefinitely.

The store is in memory only, so a restart clears it regardless. Both `session.*` limits
need a restart to change — they are not hot-reloaded. Cost totals survive a restart, and on a
local install so do the sessions themselves, in the session archive; see below for both.

## Cost history is written to `~/.cortex/cost`

**A local install keeps a cost ledger on disk, on by default.** Sessions themselves —
prompts, completions, tool arguments and results — are kept by the [session
archive](#session-history-is-kept-in-cortexsessions), a separate store with its own retention
and its own way to clear it. Per-minute
cost totals do not: they are appended to `~/.cortex/cost/YYYY-MM-DD.jsonl`, one file per
local day, **kept for 31 days** — the length of the longest month, so `window=month` can be
answered in full on the 31st.

**Sizing, because "roughly 10 MB" was a laptop figure and is not general.** A row is about 418
bytes, and there is one per minute *per distinct (endpoint, model, agent, provenance)*. A laptop
writes rows only for minutes with traffic, which is where 10 MB comes from. Continuous traffic
populates all 1,440 minutes of a day:

| Distinct combinations per minute | Per day | Per 31 days (the default retention) |
|---|---|---|
| 2 | 1.2 MB | 37 MB |
| 8 | 4.8 MB | 149 MB |
| 64 (the per-minute cap) | 38.5 MB | 1.19 GB |

Size a mounted volume from that table, not from the laptop number, and lower
`retention_days` if the top row is closer to your traffic.

**Where that default comes from.** It is **on wherever the ledger can survive a restart** —
which means either an explicit `cost_ledger.dir` (in Kubernetes, a path on a mounted volume) or
being started from a config inside `~/.cortex`, which is what a local install is and what both
`agentop service install` and `--local` do. A resolvable `$HOME` is deliberately *not* enough:
`HOME=/root` resolves in almost every container, and neither is a leftover `~/.cortex` directory,
which is not a decision anybody made. A container with neither signal can only write to a layer
that is discarded on restart, so there it stays off and says why in a startup log line.

That rule replaced an earlier one keyed on `--local`, which was the cause of a real bug: the
installed service runs `cortex --config ~/.cortex/config.yaml` and never `--local`, so
"on by default" was false for every install. The generated config also writes
`cost_ledger: {enabled: true}` explicitly, which is now belt-and-braces rather than the
mechanism — a config generated before that was added still gets the ledger, because the default
no longer depends on the file. `grep -A1 cost_ledger ~/.cortex/config.yaml` shows which you
have.

It exists because the in-memory counters are a 6-hour ring, and the proxy restarts several
times a day. Without the ledger, `today`, `7d` and `month` are all still answered — from the
ring's maximum window, with the response's own `window` field naming the span that was actually
covered rather than the one you asked for. So the figures stay honest and get much smaller: six
hours of a day, six hours of a week, and six hours of a month, which is the one that reads most
wrongly if you take the label at face value. agentop draws such a span as `—` rather than as a
number for that reason.

**What is in the files.** One JSON line per minute per (endpoint, model, agent,
provenance): the host, the model name, the calling agent's User-Agent, token counts,
dollar figures and a timestamp. **No prompt content, no completions, no tool arguments** —
that is a promise a test asserts against the serialized bytes, not a convention.

**Who can read them.** Anything that can read your home directory, and anything that can
reach the session API, which is **unauthenticated** (the proxy logs `UNAUTHENTICATED; contains
raw user content; never expose via ingress` when it starts). On a laptop it binds to localhost.

`agentop` is one of those readers now, and the distinction is worth being exact about:
`GET /v1/usage` reaches these files only for the symbolic windows `today`, `7d` and `month`,
which a duration cannot express — so the spend band asks for them by name through
`apiclient.GetUsageWindow`, and three of its four cells are ledger-backed. Only `LAST 1H` comes
from the 6-hour ring, and it is the one cell that survives a deployment with no ledger at all.
Nothing about the ledger makes that worse, but "my spend is on disk and readable" is worth
knowing rather than discovering.

**To turn it off**, in `~/.cortex/config.yaml`:

```yaml
cost_ledger:
  enabled: false
```

then `agentop service restart`.

**The setting takes effect on restart, not on reload.** The running proxy watches that file
and hot-reloads most of it, but the ledger is opened once at startup, so a `cost_ledger`
edit is *refused* rather than quietly accepted: the reload fails, `LastError` names
`cost_ledger`, and nothing in the block changes until the proxy restarts. That refusal
applies to the whole save — anything else edited in the same pass is refused with it —
which is deliberate. It used to be accepted, meaning the reload reported success and
`/config` served `enabled: false` while the startup writer kept appending under the old
retention. A restart costs the requests in flight and not the sessions themselves (see
*Re-running the installer is safe* for the measurements), so the edit is cheap — but it is
still worth batching with any other change that needs one.

Turning it off stops new files being written; it does not delete the ones already there.
`rm -rf ~/.cortex/cost` does that.

**Expired files are renamed before they are deleted**, so you may see
`2026-08-01.jsonl.expired` in that directory. Retention is counted back from a clock the proxy
cannot verify, and a host clock that steps forward — then records a day file at the wrong date
— makes a wrong cutoff indistinguishable from time having genuinely passed. A condemned file is
therefore invisible to every query but kept until a *second* prune agrees, and **restored if
the cutoff moves back**, so a clock fault costs a delay in retention rather than your cost
history. The cost is disk: between the two passes the directory can hold up to two retention
windows — double whichever row of the sizing table above matches your traffic — and
`rm ~/.cortex/cost/*.expired` is always safe.

Two other knobs, same restart rule:

| Setting | Default | Notes |
|---|---|---|
| `cost_ledger.dir` | `~/.cortex/cost` | Must be an absolute path. A relative one is refused, because it would resolve against whatever directory the proxy started from |
| `cost_ledger.retention_days` | 31 | The longest month, so `window=month` is answerable in full on the 31st; a shorter value makes that total a partial one and it is marked as such. Minimum **9** when set. `window=7d` is a rolling 7×24h, not seven calendar days, so it can open **nine** local day files: one extra because a rolling span starts part-way through a date, and one more because a spring-forward week is 167 hours, so the span reaches an hour further back. A retention shorter than the window is disclosed rather than silent — `DaysOutsideRetention` counts the days asked for beyond the setting, the band marks the total as a floor and `agentop cost` prints a coverage line. The floor of 9 is exactly `window=7d`'s worst case, so a legal setting can never answer *that* window short; a `month` asked of a 9-day ledger can |

## Session history is kept in `~/.cortex/sessions`

**A local install keeps every session on disk, on by default.** Each event the session store
records is also written to `~/.cortex/sessions`, so a session outlives a restart and the store's
own eviction (`session.max_sessions`, a `session.ttl`). `agentop` lists every session the archive
holds beside the ones in memory, and Enter opens one like any other. Each session keeps its
figures across a restart too — events, tokens, cost, title: the proxy adds what the archive holds
from before the restart to what it has seen since. LAST 1H and the usage pane's hour windows
survive a restart as well: at startup, before it serves anything, the proxy replays the archive's
last six hours into them. If that would take longer than 5 seconds it gives up, and they start
empty as they used to.

**Unlike the cost ledger, this is the content.** Prompts, completions, tool arguments and
tool results — everything the session API serves — sit in
`~/.cortex/sessions/data/<one directory per session>/`, readable by anyone who can read your
home directory. The directories are created `0700` and the files `0600`. The proxy says what
the archive holds in its startup log line, so `grep 'session archive' ~/.cortex/proxy.log`
shows which state you are in.

**It runs only on a local install** — a config inside `~/.cortex`, which is what `agentop
service install` and `--local` produce. Anywhere else it is off, and `enabled: true` is refused
with a WARN naming the reason. On a local install it is on by default only while
`listener.bind_loopback_only` is true, as the generated config sets it, because that is the one
setting under which [clearing it](#clearing-it) works; with the binds widened, it stays off
unless `enabled: true` opts in, and the startup line then says a clear will be refused. There
is deliberately no `dir` setting: raw prompts on a cluster's volume are a decision of their
own, and a cluster should not reach it by mounting a path.

**How big it gets.** Each session's events are written as zstd-compressed segments that store
a repeated message once rather than once per turn — the same saving the in-memory store makes,
since every LLM request re-sends the conversation so far. The benchmark fixture comes to about
1.6 KB per event. Two bounds apply, checked hourly and on startup, and the oldest segments go
first by the time they were last written:

| Setting | Default | Notes |
|---|---|---|
| `session.archive.enabled` | on for a local install bound to loopback only | `false` always wins; `true` opts in without the loopback bind |
| `session.archive.retention_days` | 30 | how long a segment is kept after its last write; at most 3650 |
| `session.archive.max_bytes` | 2 GiB | the archive's total size; past it, the oldest segments go first |

A full disk does not stop the proxy. The archive stops writing until its next hourly pass,
and requests are served as before; only the history has a gap.

**What it does not promise.** Writing happens off the request path, so a request never waits
on the disk — and the cost of that is that a burst the writer cannot keep up with is dropped
*from the archive*, never from memory. An unclean kill loses at most the last second.
`GET /v1/sessions?archived=true` reports both the archive's size and what it lost, under
`archive` (`bytes`, `maxBytes`, `retentionDays`, and `droppedEvents`, `writeErrors`,
`droppedRenames`, `paused` when any is nonzero); nonzero means what you are reading has gaps.

### Clearing it

A restart used to be a reset; with history on disk it is not, so clearing is one action of its
own. **`X` on agentop's sessions pane** asks first — *Erase all N sessions from this Cortex, in
memory and on disk (S)?* — and on `y` erases every session, from memory and from disk, at once.
**The cost ledger is kept**: it holds spend totals and no content, so `today`, `7d` and `month`
read the same afterwards. From a script:

```sh
curl -X DELETE http://localhost:47601/v1/sessions
# {"sessions":12,"archivedSessions":340,"bytes":81234567}
```

The clear is refused — a 403 saying why — unless the request names a loopback host
(`localhost`, `127.0.0.1` or `[::1]`), carries no `Origin` header, and reaches a proxy bound to
loopback only, as a local install is. Those are the checks that keep a web page you visit from
erasing your history through the unauthenticated API. If the disk half fails, memory is still
cleared and the answer is a 500 with `archiveError` saying what happened on disk — in the usual
case, that the history could not be moved aside and is all still there.

**To stop it**, set this in `~/.cortex/config.yaml` and restart (`agentop service restart`) —
the archive is opened once at startup, and a live edit of anything under `session` is refused
rather than half-applied:

```yaml
session:
  archive:
    enabled: false
```

That stops new writes and leaves what is on disk; clear first with `X` if you want it gone, or
`rm -rf ~/.cortex/sessions` with the proxy stopped.

## `agentop: command not found`

Setup puts both binaries in `~/.local/bin`. If that is not on your PATH, adding it to
your shell profile is one of the changes its consent screen lists: `~/.zshrc` for zsh,
and for bash the first of `~/.bash_profile`, `~/.bashrc` and `~/.profile` that exists
(a new `~/.bash_profile` if none does). New terminals pick it up, and for the one you
are in:

```sh
export PATH="$HOME/.local/bin:$PATH"
```

With `--no-modify-path`, or a shell it has no profile for (fish, sh), setup edits
nothing and prints an `export PATH=` line for you to add yourself.

To undo, delete the two lines setup marked in your profile (the first starts
`# added by Cortex`). `agentop uninstall` removes them too, unless other tools in
`~/.local/bin` still need them.

## Restricted environments (sandboxes, no launchd session)

Some environments cannot manage services at all — a sandboxed shell, a session without
a usable launchd domain, CI. Setup asks the supervisor before it changes anything, and
where it cannot be used, runs the proxy in the background instead, as
`cortex --local --supervise`. Its consent screen says so:
`unsupervised · won't restart after a reboot — launchd can't be used here`.
`--no-service` makes the same choice on purpose. The `--supervise` parent restarts the
proxy after a crash, but nothing brings it back at login. Setup's last lines name the
stop command, `kill $(cat ~/.cortex/proxy.pid)`, and `agentop uninstall` stops it too.

`agentop service install`, run there by hand, detects this before writing anything and
tells you so, rather than failing at `launchctl bootstrap` with `Input/output error`.

You can also run Cortex in a terminal of its own, unsupervised:

```sh
cortex --local               # in its own terminal, or backgrounded
agentop                      # the viewer, as usual
```

What you give up that way: no restart after a crash, and nothing brings it back at
login. Stop it with `kill $(pgrep -x cortex)` — there is no service to stop.

Two assumptions that do not hold in such environments, and what happens:

| Assumption | If it does not hold |
|---|---|
| `launchctl` can manage the user domain | Setup runs the proxy in the background, as above. `service install` stops early and prints the command above |
| `$HOME` is your login home | launchd never scans `$HOME/Library/LaunchAgents`, so the service cannot start at login. `service install`, and setup through it, warn and continue; crash recovery still works while you are logged in |

## Is it working?

```sh
agentop service status
```

`installed:` names the unit file, `Cortex is healthy according to` names the endpoint
that answered. If it says `Cortex is NOT answering`, the proxy is loaded but not
serving — check `~/.cortex/proxy.log`.

If it says `config: … will not load`, an edit to `~/.cortex/config.yaml` broke it
(`config: no config at …` means the file is gone). A proxy that was already running
rejected that edit and keeps serving the config it last loaded. Status looks for one on
the built-in health address, `127.0.0.1:47604`, since the file that names the real one
cannot be read; if your config had moved `health_addr`, silence there does not mean
Cortex is stopped. Nothing can start from the file until it loads again, so
`service start` and `service restart` refuse until then, as `service install` and
`agentop setup` do. Status exits 1 while the file is broken, even if a proxy is serving.

To see traffic rather than status, run `agentop` with no arguments.

## Start and stop

```sh
agentop service start
agentop service stop
```

A stop persists: Cortex stays down across logouts and reboots until you start it
again. That is deliberate — a stop that quietly undoes itself at your next login is
worse than none.

`stop` also reports how many connections it cut, because until Cortex is back those
clients have nowhere to go: `HTTPS_PROXY` is fixed in each one's environment when it
starts, so none of them can fall back to a direct connection. Run `agentop service start`
and they reconnect on their next request — the sessions themselves do not need
restarting.

## Three ways to turn it off

They are different, so pick deliberately:

### Pause it

```sh
agentop service stop
```

Claude Code fails while Cortex is stopped, because its settings still point at the
proxy. So does OpenCode once `agentop configure opencode enable` has configured it,
because its background service's environment points there too. Either start Cortex
again or unwire them: Claude Code as below, OpenCode with
`agentop configure opencode disable`.

**A running session cannot route around a stopped Cortex.** `HTTPS_PROXY` is fixed in
its environment when it starts, so it has no way to fall back to a direct connection,
and `agentop configure claude-code disable` cannot reach it — that only affects
sessions started afterwards. What it needs is Cortex back: `agentop service start`,
after which it reconnects on its next request without being restarted. `service
stop` tells you how many connections it cut, for exactly this reason.

Use `agentop service stop`, not `kill` or `pkill` — the supervisor restarts the
process within seconds, which looks like it refusing to die.

### Unwire Claude Code

```sh
agentop configure claude-code disable
```

This removes only the keys Cortex added to `~/.claude/settings.json`
(`HTTPS_PROXY`, `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`, and the CA variables
`NODE_EXTRA_CA_CERTS` / `SSL_CERT_FILE` / `GIT_SSL_CAINFO` / `REQUESTS_CA_BUNDLE` /
`CURL_CA_BUNDLE`) and leaves anything else in that file alone. Claude Code goes
straight to the API again. Restart `claude` to pick it up.

It also leaves alone any one of those keys whose value you have **changed since
enable set it**, and says which: that edit is yours, and restoring what preceded
Cortex over it would throw it away silently. Those keys are the one case where
disable does not leave Claude Code fully unwired — a `HTTPS_PROXY` you pointed
somewhere else still points there. Edit `~/.claude/settings.json` by hand if you
want them gone.

There are several CA variables because anything Claude Code spawns inherits
`HTTPS_PROXY` and so must also be able to verify the bridge. They do not all get
the same file: `NODE_EXTRA_CA_CERTS` **extends** Node's trust store, so it gets
`ca.crt`, while the rest **replace** the trust store and get `bundle.crt` — the CA
followed by this machine's public roots. Pointing a replacing variable at `ca.crt`
would leave that tool trusting one private CA and nothing else, which breaks every
direct TLS call it makes.

### Everything shows as `tunnel` and no plugin ever runs

`agentop observe` shows rows like this, with `tunnel` in ACTION and no method:

```
18:00:02  out  req  tunnel  client-rejected-ca   ete-litellm.example.com
```

The reason is in the PLUGIN column. `client-rejected-ca` means that client refused
the bridge certificate, so nothing downstream can read the traffic. There are two
causes, and they need different fixes.

**The usual one: the client started before the CA existed.** CA files are read once
at startup, so an agent already running when Cortex was first installed — or when
`~/.cortex` was deleted and recreated — is holding a different CA, or none.

The proxy log names the offender, the cutoff, and which CA is actually in force:

```sh
grep 'client-rejected-ca' ~/.cortex/proxy.log
# ... client=127.0.0.1:58041 ca_not_before=2026-09-09T17:11:39-04:00
#     ca_fingerprint=CD:19:CD:2C:... ca_file=/Users/you/.cortex/ca/ca.crt ...
```

Map that client port to a process, then restart it:

```sh
lsof -nP -iTCP:58041          # -> COMMAND / PID
ps -o lstart= -p <pid>        # started before ca_not_before? restart it
```

The port has to come from the log rather than a later `lsof` sweep: the connection
is gone by the time you look, so nothing after the fact can attribute it.

**The other one: the client trusts a different CA of the same name.** If the process
started *after* `ca_not_before` and still gets rejected, compare fingerprints —
this is what `ca_fingerprint` is for:

```sh
openssl x509 -in "$NODE_EXTRA_CA_CERTS" -noout -fingerprint -sha256
```

A mismatch against the `ca_fingerprint` in the log means the client is pointed at a
*different* CA file, not a stale one, and restarting it will not help. Every
generated CA is named `CN=authbridge-tls-bridge-ca` and lives at `~/.cortex/ca`, so
nothing but the fingerprint distinguishes them.

This happens whenever `$HOME` differs between the proxy and the client, because
`--local` derives the CA directory from `$HOME`: sandboxes, per-project homes, and
wrappers that set `HOME=$PWD` each get their own CA. Point every environment at one
CA instead:

```sh
cortex --local --ca-dir /Users/you/.cortex/ca
```

`--ca-dir` moves only the CA; the config stays at its usual path. On startup the
proxy also warns on its own when a client's recorded CA file is a bridge CA whose
fingerprint differs from the one in force.

**A third cause, once a year: the CA was renewed under you.** The generated CA is
valid for 365 days, and the proxy replaces it about a month before it expires. That
is a new CA, so — exactly as on a first install — every client has to restart to
pick it up. It is the one case nobody expects, because nothing else changed on that
boot. The startup log says so plainly (`tls-bridge: generated self-signed CA`, with
`restart_clients`), and the rejection that follows carries a `ca_not_before` of
minutes ago rather than a year back, so the restart advice is the right advice.

The other reasons you may see, and what each one asks of you:

| Reason | Meaning | Act? |
| --- | --- | --- |
| `passthrough-host` | A host Cortex deliberately does not intercept (GitHub, module proxies, package registries). | no |
| `passthrough-port` | Not a port the bridge watches. | no |
| `passthrough-nontls` | The bytes were not a TLS handshake, so there was nothing to terminate. | no |
| `skip-cached` | An earlier handshake for this host failed, so it is not intercepted for **anyone** for a short window. Any failed handshake seeds this, not only a CA rejection — the seeding failure logged its own reason. The window starts at 30s and lengthens only if **rejections** keep coming — a hang-up or a cipher mismatch seeds it but never escalates it; the first client that *does* trust the CA clears it immediately. | find the earlier failure in `proxy.log` and fix that client |
| `bridge-disabled` | No TLS bridge is configured. | only if you wanted one |
| `client-hung-up` | The client vanished mid-handshake. Often a cancelled request; not evidence about trust, which is why it carries no advice. OpenCode hangs up rather than rejecting the CA; see [its page](agents/opencode.md#ca-trust). | usually no |
| `handshake-failed` | Some other handshake failure — a version, cipher or ALPN mismatch, or Cortex failing to mint a certificate. | check `error=` in the log |
| `origin-unverified` | **Cortex** could not verify the destination's certificate, so it declined to vouch for it. Bridging would have meant terminating TLS for a server we could not authenticate. | investigate the destination |
| `dial-failed` | Cortex could not reach the destination at all — a DNS failure, a refused connection or a timeout — so no tunnel opened. Its response row is a `502` whose `error` carries the dial error. | check the destination and the network path to it |

Only `client-rejected-ca` asks you to restart anything. The others are either working as
intended or point somewhere other than your agents — which is why the reason is worth
reading before acting on it.

### Developer tooling is not intercepted at all

`gh`, `go`, `pip` and `npm` work out of the box, without trusting anything. The
bridge ships a default `passthrough_hosts` list — GitHub, the Go module proxy and
checksum DB, the package registries — and tunnels them rather than forging a leaf.

That costs nothing. `inference-parser`, the MCP/A2A parsers and `tool-prune` all act
on agent↔LLM and agent↔tool messages; none of them has anything to say about a
module download. Intercepting those hosts produced no observability and broke every
Go tool, which is the worst of both.

To see the list, or to override it, set `tls_bridge.passthrough_hosts` in
`~/.cortex/config.yaml`. An explicit list **replaces** the default rather than adding
to it, and `passthrough_hosts: []` intercepts everything. Never list an inference
endpoint there: it would silently remove the parsing and the token savings, with no
error anywhere to notice it by.

### Go tools on macOS need the keychain, not a variable

`SSL_CERT_FILE` — the Go one, covering `go`, `gh` and `agentop` itself — **does
nothing on macOS**. Go's `crypto/x509` honours it only in `root_unix.go`, which is
built for `linux || freebsd || …` and excludes darwin; darwin's `loadSystemRoots`
returns a sentinel that reads no files, and verification is then handed to
Security.framework, which consults the keychain alone. No environment variable can
change that.

So on a Mac, when the bridge decrypts a host a Go tool is talking to, that tool
fails with a bare `x509: certificate signed by unknown authority`. To cover them,
trust the CA in your login keychain:

```sh
security add-trusted-cert -k ~/Library/Keychains/login.keychain-db \
  -p ssl ~/.cortex/ca/ca.crt
```

Undo with:

```sh
security delete-certificate -c authbridge-tls-bridge-ca \
  ~/Library/Keychains/login.keychain-db
```

`git`, `curl` and Python are **not** affected on macOS — they read their bundles
through OpenSSL/LibreSSL, which honours the variables on every platform. And on
Linux `SSL_CERT_FILE` works normally, so nothing extra is needed there.
`agentop configure claude-code enable` prints a short form of this note on macOS
when it changes your settings (not on a re-run that finds them already enabled).
Setup does not print it. `agentop doctor` gives the same advice, with the command above,
as a `! Go tools` line, when Claude Code is routed and the CA is not in your login
keychain.

Cortex keeps running; nothing sends traffic to it. `agentop configure claude-code
enable` puts it back.

### Remove it

```sh
agentop uninstall --purge
```

`agentop uninstall` lists what it will remove and asks once; `--yes` skips the question.
It unroutes Claude Code, OpenCode, IBM Bob and the bob shell function where Cortex
routed them, restarting OpenCode's background service when it finds it running. It
stops and removes the service, or the background proxy. It removes the PATH lines setup
added, unless other tools in `~/.local/bin` still need them, and removes `agentop`,
`cortex` and `cortex-session-dump` from `~/.local/bin`. A step that fails is reported
with its fix, the rest still run, and the end lists what was left behind.

`--purge` also deletes `~/.cortex`: config, CA, logs, cost history, session history,
agentop's UI settings. It goes last, and only when every removal before it worked: after a failed
one `~/.cortex` stays, and the end lists it, with the `rm -rf` to run once the fixes
above are done. Without `--purge` `~/.cortex` stays too, so a later install picks up
where you left off, and a run where every removal worked ends by saying how to delete
it yourself.

Restart any `claude` that was already running: it read its settings when it started, so
it still points at Cortex. `claude --resume` picks the conversation back up. IBM Bob
needs a restart too; OpenCode's service is restarted for you. A later install routes no
agent by itself, so run each agent's `agentop configure <agent> enable` again, or pass
`--claude-code` to the installer for Claude Code.

#### Check nothing is left

```sh
opencode service get env             # if you use OpenCode: none of it should point at Cortex
grep '\.cortex/' ~/.claude/settings.json # should print nothing: Cortex's CA variables are gone
ls ~/.local/bin/agentop ~/.local/bin/cortex 2>/dev/null # should print nothing
pgrep -lx cortex                     # should print nothing
ls ~/.cortex 2>/dev/null             # should print nothing, after --purge
```

The CA that `--purge` removes was only ever trusted through the CA variables in
`~/.claude/settings.json` — *Cortex* never adds it to the system or login keychain.
`bundle.crt` lives in the same directory and is derived from `ca.crt` plus a copy of
the public roots, so removing `~/.cortex` takes it with them; it holds no private
key and grants nothing on its own.

**On macOS, if you followed the Go-tools step above** and ran
`security add-trusted-cert` yourself, that trust setting is the one thing outside
`~/.cortex` and outside `~/.claude/settings.json`, so it outlives both. Deleting the
CA file does not withdraw it — the keychain holds its own copy. Remove it too:

```sh
security delete-certificate -c authbridge-tls-bridge-ca \
  ~/Library/Keychains/login.keychain-db
```

Check whether it is there at all with:

```sh
security find-certificate -c authbridge-tls-bridge-ca ~/Library/Keychains/login.keychain-db
```

Leaving it behind means a CA whose private key you have deleted stays trusted for
TLS — harmless in itself, since nothing can sign with it any more, but it is trust
you did not intend to keep.

#### If you reinstall afterwards, restart your agents

Deleting `~/.cortex` deletes the CA, so a later install mints a **new** one. Every
agent that is already running still trusts the old CA — a client reads its CA file
once, at startup — and will refuse the new certificates.

That failure is quiet. Cortex falls back to tunnelling rather than breaking the
connection, so the traffic keeps flowing and nothing on the agent's side complains;
it simply stops being parsed, which shows up as `tunnel` rows in `agentop observe`.
Restart those agents and they are visible again.

An ordinary upgrade is unaffected — it keeps the existing CA. This only applies when
`~/.cortex` has been deleted, or on a first install with agents already running.

#### If `agentop` is already gone

The service can be removed by hand:

```sh
# macOS
launchctl bootout "gui/$(id -u)/io.rossoctl.cortex"
rm -f ~/Library/LaunchAgents/io.rossoctl.cortex.plist

# Linux
systemctl --user disable --now cortex.service
rm -f ~/.config/systemd/user/cortex.service
```

Then delete the Cortex keys from the `env` block of `~/.claude/settings.json`
yourself. There are **seven**:

```
HTTPS_PROXY
NODE_EXTRA_CA_CERTS
SSL_CERT_FILE
GIT_SSL_CAINFO
REQUESTS_CA_BUNDLE
CURL_CA_BUNDLE
CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC
```

**Remove all seven, and do it before `rm -rf ~/.cortex`.** The middle four point at
`~/.cortex/ca/bundle.crt`, and unlike `NODE_EXTRA_CA_CERTS` each of them *replaces*
its tool's trust store rather than adding to it. Leave them behind with the file
deleted and git, curl and Python fail **every** TLS call — including calls that have
nothing to do with Cortex — with `error setting certificate verify locations`, on a
machine you believe you have just cleaned. `agentop uninstall` removes all seven first,
as `agentop configure claude-code disable` does, and `--purge` deletes `~/.cortex` last,
and not at all if a removal before it failed; this list is only for when that binary is
already gone.

Both of them skip a key whose value you have **changed since enable set it** — that
edit is yours to keep or drop — and say which under *Left behind*. `--purge` then keeps
`~/.cortex` rather than walking into the hazard above: a key it left behind whose value
still names a file inside the directory holds the whole directory back, and the `purged`
row says which key. Delete it yourself once that key is gone. Doing the removal by hand,
as this section does, there is nothing to do that for you — check the keys you changed
before you `rm -rf`.

If you configured OpenCode, its background service's environment holds the same kind of
variables. Remove the ones that point at Cortex with the `opencode` CLI, as
[Undoing it by hand](agents/opencode.md#undoing-it-by-hand) shows, also before
`rm -rf ~/.cortex`.

Last, the files setup installed: `rm -f ~/.local/bin/cortex ~/.local/bin/cortex-session-dump`,
and the two lines it marked in your shell profile, the first starting `# added by Cortex`.
