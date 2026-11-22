# Running Cortex: start, stop, and remove it

New to Cortex? Start with [Getting started](getting-started.md). For problems, see [Troubleshooting](troubleshooting.md).

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
runs setup's checks, and its own of the CA and of the programs Cortex is not reading,
without changing anything — the binaries, PATH, the config, the service, Claude Code's
routing — and ends each problem with the command that fixes it, usually `agentop setup`.
`agentop uninstall` removes it, and `~/.cortex` too with `--purge`, as
[Remove it](#remove-it) describes.

**Never use `kill` or `pkill` to stop it.** The proxy is supervised, so killing it gets
it restarted within a couple of seconds, which looks like a process refusing to die.
`agentop service stop` is the stop that works.

## Is it working?

```sh
agentop service status
```

`installed:` names the unit file, `Cortex is healthy according to` names the endpoint
that answered. If it says `Cortex is NOT answering`, the proxy is loaded but not
serving — check `~/.cortex/proxy.log`.

On Linux, when nothing answers, status asks systemd why, and says which of these it is:

| Status says | What happened | Exit |
|---|---|---|
| `Cortex is stopped, and stays stopped` | Stopped and disabled, as `agentop service stop` leaves it. It stays stopped across logins and reboots until `agentop service start` | 0 |
| `Cortex is stopped, but still enabled` | Stopped some other way, such as `systemctl --user stop`; it starts again at the next login or boot | 1 |
| `it exited … and systemd is restarting it` | It crashed, and systemd brings it back within 10 seconds | 1 |
| `systemd stopped restarting it` | It crashed five times in five minutes, so systemd gave up. Fix the cause in the log, then `agentop service restart` | 1 |

On macOS, status asks launchd instead:

| Status says | What happened | Exit |
|---|---|---|
| `Cortex is stopped, and stays stopped` | Stopped and disabled, as `agentop service stop` leaves it. It stays stopped across logins and reboots until `agentop service start` | 0 |
| `Cortex is stopped, but not disabled` | Taken out of launchd some other way, such as `launchctl bootout`; it starts again at the next login | 1 |
| `launchd has its job loaded but not running` | launchd is holding its start back, as it does for a while after every login, or waiting out its 10 seconds between restarts. `agentop service restart` starts it now | 1 |
| `you are not logged in to this Mac's desktop` | Cortex runs only in your desktop session, and you have none: nobody is at the screen, or someone else is. It starts at your next login there, or at boot with automatic login, unless `agentop service stop` stopped it, which status cannot check without the session. An SSH login does not start it | 1 |

A stopped Cortex exits 0, like a healthy one, because a stop is not a failure. So in a
script, the exit code alone does not say Cortex is serving: check for
`Cortex is healthy according to` in the output as well. Before, a stopped Cortex
exited 1.

On macOS, `service stop` and `service uninstall` return only once launchd has taken
Cortex down, so nothing still holds its ports when they say it is stopped. (`agentop
uninstall` already waited.) If launchd takes longer than 30 seconds, they say it is still
shutting down, and exit 1.

`service start`, `service restart` and `service install` clear systemd's record of the
crashes first, so they can start a unit systemd had given up on without waiting out
its five-minute window. `service stop` clears it after, so stopping a unit systemd had
given up on reads as stopped, not as a crash.

If it says `config: … will not load`, an edit to `~/.cortex/config.yaml` broke it
(`config: no config at …` means the file is gone). A proxy that was already running
rejected that edit and keeps serving the config it last loaded. Status looks for one on
the built-in health address, `127.0.0.1:47604`, since the file that names the real one
cannot be read; if your config had moved `health_addr`, silence there does not mean
Cortex is stopped. Nothing can start from the file until it loads again, so
`service start` and `service restart` refuse until then, as `service install` and
`agentop setup` do. Status exits 1 while the file is broken, even if a proxy is serving.

To see traffic rather than status, run `agentop observe`.

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
(`HTTPS_PROXY` and the CA variables
`NODE_EXTRA_CA_CERTS` / `SSL_CERT_FILE` / `GIT_SSL_CAINFO` / `REQUESTS_CA_BUNDLE` /
`CURL_CA_BUNDLE`) and leaves anything else in that file alone. Claude Code goes
straight to the API again. Restart `claude` to pick it up.

[The Claude Code guide](agents/claude-code.md) tells which keys `disable` leaves, and why.

Cortex keeps running; nothing sends traffic to it. `agentop configure claude-code
enable` puts it back.

[Running one command through Cortex](agents/any-agent.md#running-one-command-through-cortex-agentop-exec) tells why `NODE_EXTRA_CA_CERTS` gets `ca.crt` and the other CA variables get `bundle.crt`.

The third way is to remove Cortex, as [Remove it](#remove-it) says.

## Remove it

```sh
agentop uninstall --purge
```

`agentop uninstall` lists what it will remove and asks once; `--yes` skips the question.
It unroutes Claude Code, OpenCode, Codex, IBM Bob, the `bob` and `pi` shell functions and Pi's
session extension where Cortex routed them, restarting OpenCode's background service and
Codex's app-server when it finds them running. It
stops and removes the service, or the background proxy. It removes the PATH lines setup
added, unless other tools in `~/.local/bin` still need them, and removes `agentop`,
`cortex`, `cortex-session-dump` and `cortex-gateway` from `~/.local/bin`. A step that fails is reported
with its fix, the rest still run, and the end lists what was left behind.

It does not unroute Codex. Run `agentop configure codex disable` before you uninstall.

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

### Check nothing is left

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

**On macOS, if you followed [Go tools on macOS](troubleshooting.md#go-tools-on-macos-are-passed-through-unread)** and ran
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

### If you reinstall afterwards, restart your agents

Deleting `~/.cortex` deletes the CA, so a later install mints a **new** one. Every
agent that is already running still trusts the old CA — a client reads its CA file
once, at startup — and will refuse the new certificates.

That failure is quiet. Cortex falls back to tunnelling rather than breaking the
connection, so the traffic keeps flowing and nothing on the agent's side complains;
it simply stops being parsed, which shows up as `tunnel` rows in `agentop observe`.
Restart those agents and they are visible again.

An ordinary upgrade is unaffected — it keeps the existing CA. This only applies when
`~/.cortex` has been deleted, or on a first install with agents already running.

### If `agentop` is already gone

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
yourself. Delete these keys:

```
HTTPS_PROXY
NODE_EXTRA_CA_CERTS
SSL_CERT_FILE
GIT_SSL_CAINFO
REQUESTS_CA_BUNDLE
CURL_CA_BUNDLE
```

An older Cortex also set `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`, which stops
Claude Code's auto-updates. Delete it too, unless you set it yourself.

**Remove all of these keys, and do it before `rm -rf ~/.cortex`.** The middle four point at
`~/.cortex/ca/bundle.crt`, and unlike `NODE_EXTRA_CA_CERTS` each of them *replaces*
its tool's trust store rather than adding to it. Leave them behind with the file
deleted and git, curl and Python fail **every** TLS call — including calls that have
nothing to do with Cortex — with `error setting certificate verify locations`, on a
machine you believe you have just cleaned. `agentop uninstall` removes these keys first,
as `agentop configure claude-code disable` does, and `--purge` deletes `~/.cortex` last,
and not at all if a removal before it failed; this list is only for when that binary is
already gone.

Both of them skip a key whose value you have **changed since enable set it** — that
edit is yours to keep or drop — and say which under *Left behind*. `--purge` then keeps
`~/.cortex` rather than walking into the hazard above: if any key is still going to name
a file inside the directory once it has finished, the whole directory stays, and the
`purged` row says which key. That covers a key it left behind *and* one it put back,
because a value of your own under `~/.cortex` — a CA file you keep there yourself — is
one enable adopts as Cortex's and uninstall hands back to you. Delete the directory
yourself once nothing points into it. Doing the removal by hand, as this section does,
there is nothing to do that for you — check the keys you changed before you `rm -rf`.

If you configured OpenCode, its background service's environment holds the same kind of
variables. Remove the ones that point at Cortex with the `opencode` CLI, as
[Undoing it by hand](agents/opencode.md#undoing-it-by-hand) shows, also before
`rm -rf ~/.cortex`.

Last, the files setup installed: `rm -f ~/.local/bin/cortex ~/.local/bin/cortex-session-dump ~/.local/bin/cortex-gateway`,
and the two lines it marked in your shell profile, the first starting `# added by Cortex`.

## Re-running the installer is safe

`agentop update` is how you upgrade. When the newest release is already installed it
prints `Already at vX.` and downloads nothing. For a release tag (the newest release, or
`--ref=vX`) it does not run `install.sh`. The installed agentop downloads `checksums.txt`
and the tarballs from `https://github.com/rossoctl/cortex/releases/download/<tag>/`,
checks the SHA-256 of each tarball, checks the build-provenance attestation of each
tarball, extracts them into a temporary stage, and runs
`<stage>/agentop setup --from <stage>`. The attestation must come from
`https://github.com/rossoctl/cortex/.github/workflows/release-binaries.yaml@refs/tags/<tag>`
with issuer `https://token.actions.githubusercontent.com`, and must have a
transparency-log entry that is checked against the Sigstore public-good trusted root. A
missing, invalid or unreachable attestation stops the update, and the update changes
nothing. You can check a tarball yourself with
`gh attestation verify <tarball> --repo rossoctl/cortex`. For `--ref=main` (any ref that
does not start with `v`) `agentop update` runs the one-liner with the flags that keep the
install as it is (`agentop update --help` lists them). The one-liner and a first install
use `install.sh`, with no provenance check. The one-liner is
meant to be run repeatedly too. When nothing has changed it changes nothing. It still
downloads the release, but `agentop setup` finds the staged binaries identical to the
installed ones, asks nothing, and ends on one line,
`cortex v0.9.0 is installed and healthy.`, leaving the running proxy alone rather than
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
proxy that replaces it binds its ports before it opens anything in `~/.cortex`, so a client
that connects before it serves is answered once it does rather than refused. When a restart
genuinely is needed, `agentop service install` says how many connections it is about to cut.
Setup does not pass that line on.

To restart deliberately: `agentop service restart`.

## Automatic updates

A service install (launchd or systemd) updates itself:

- The proxy starts the updater unit at most once every 3 hours (`update.check_every`).
  The first start is 10 minutes after the proxy starts. The unit is
  `io.rossoctl.cortex.update` on macOS and `cortex-update.service` on Linux.
- The updater runs `agentop update --yes --auto`.
- It installs the release that GitHub marks Latest, but only when that release was
  published at least 6 hours ago (`update.wait`). The publication time is the upload
  time of the release's `checksums.txt`.
- Then it restarts the proxy. Attached sessions lose the request in flight and retry it.

A release-candidate install (`vX.Y.Z-rc.N`) moves only forward. It installs Latest
when Latest is a later version, for example `v0.10.0` over `v0.10.0-rc.1`, after the
same wait. It never moves back to an older Latest. `agentop update` by hand follows
the same rule: to move back, give the release, for example `--ref=v0.9.5`. A plain
release follows Latest in both directions, so the maintainers can withdraw a bad release.

It does not change a `dev` or `main-<sha>` install. That build has no version order
against Latest, and is often ahead of it. `agentop doctor` says so, and the title bar
shows no update notice. `agentop update` by hand installs the newest release, and
automatic updates start again from there.

When Latest is newer than the installed version, the `agentop` title bar shows
`vX.Y.Z available` after the next check, before the wait ends. A `--no-service`
install has no updater, so it gets no notice. `agentop update` by hand still works there.

To turn it off, set this in `~/.cortex/config.yaml`:

```yaml
update:
  auto: false
```

The next run reads it. You do not need a restart.

To change the timing, set one or both of these. The values here are the defaults:

```yaml
update:
  wait: 6h           # how long after its publication a release installs; "0" installs at once
  check_every: 3h    # how often the proxy starts the updater; from 1h to 24h
```

| Field | Default | Range | Who reads it | When a change applies |
|---|---|---|---|---|
| `wait` | `6h` | `0` or more | `agentop update --auto` | at the next check |
| `check_every` | `3h` | `1h` to `24h` | the proxy, at boot | after `agentop service restart`. A hot reload refuses the change |

The values are Go durations, for example `90m` or `12h`. With the defaults, a laptop
that is on installs a new release from 6 to about 9 hours after it is published. The
proxy refuses to start when a value is not valid, and `agentop doctor` names it on the
`updates` row.

Where to look:

- `~/.cortex/update.json` holds the result of the last run.
- `~/.cortex/update.log` holds the output of the updater.
- `agentop doctor` has an `updates` row.

`agentop update` by hand still installs at once, with no wait.

## Linux: staying up after you log out

On Linux, Cortex is a systemd **user** unit, which runs under your own systemd
manager. That manager normally starts at login and stops when your last session ends,
taking Cortex with it. **Lingering** keeps it running with nobody logged in, and
starts it at boot. Setup turns it on for you, which on a headless or SSH-only box is
what keeps Cortex up between logins.

Turning lingering on for yourself needs **polkit**. logind asks polkit whether you
may, and systemd's own policy says yes, with no password. But that policy is applied
by `polkitd`, a separate package, and a minimal install can lack it — Debian with
only the SSH server, for one. Without it logind has no one to ask and refuses, and
setup says so:

```text
loaded, but lingering could not be turned on: Cortex will stop when you log out.
  loginctl enable-linger: Could not enable linger: Access denied
  polkit is not installed, and logind needs it to let a user turn lingering on.
```

Cortex is still installed and running; it just stops at your next logout. Either fix
works, and neither needs Cortex reinstalled:

```sh
# Install polkit, then turn lingering on for yourself
sudo apt install polkitd      # Debian and Ubuntu; on Fedora, Arch and others the package is polkit
loginctl enable-linger

# Or turn it on directly, as root
sudo loginctl enable-linger "$(id -un)"
```

Re-running `agentop setup` after installing polkit is not enough: setup finds Cortex
installed and healthy, changes nothing, and does not retry lingering.

When polkit *is* installed and still refuses, the system's policy has decided users may
not keep services running, and setup says that instead; only the root fix applies. Any
other failure, after polkit has said yes, is shown in loginctl's own words, again with
only the root fix. Check where you stand with
`loginctl show-user "$(id -un)" --property=Linger`.

Setup records that it turned lingering on, and `agentop uninstall` turns it back off
only then. Lingering you turned on yourself, by either fix above, is left on when you
uninstall: turn it off with `loginctl disable-linger` if you no longer want it.

## Where your data lives

These sections tell which data Cortex keeps on your disk.

### Cost history is written to `~/.cortex/cost`

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
It also refuses, with a 403, every request whose `Host` is not `localhost`, `127.0.0.1` or
`[::1]`, and every request that carries an `Origin` header. That stops a web page from reading
your sessions through DNS rebinding. The stats server (`:47602`, which serves `/config`) refuses
them the same way.

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

### Session history is kept in `~/.cortex/sessions`

**A local install keeps every session on disk, on by default.** Each event the session store
records is also written to `~/.cortex/sessions`, so a session outlives a restart and the store's
own eviction (`session.max_sessions`, a `session.ttl`). `agentop` lists every session the archive
holds beside the ones in memory, and Enter opens one like any other. Each session keeps its
figures across a restart too — events, tokens, cost, title: the proxy adds what the archive holds
from before the restart to what it has seen since. LAST 1H and the usage pane's hour windows
survive a restart as well: at startup the proxy replays the archive's last six hours into them,
while it is already serving. For the few seconds that takes they count only traffic since the
start, then fill in. If the replay would take longer than a minute it gives up, and they count
only traffic since the start, as they used to.

**Unlike the cost ledger, this is the content.** Prompts, completions, tool arguments and
tool results — everything the session API serves — sit in
`~/.cortex/sessions/data/<one directory per session>/`, readable by anyone who can read your
home directory. The directories are created `0700` and the files `0600`. The proxy says what
the archive holds in its startup log line, so `grep 'session archive' ~/.cortex/proxy.log`
shows which state you are in.

**It also keeps each inference request exactly as it was sent.** For a request that
`inference-parser` reads as inference, and for each Responses WebSocket turn, the archive keeps
the body the agent sent, the body Cortex forwarded when a plugin rewrote it, the request headers
and the query. Other requests, such as an OAuth token refresh, keep their event only. Cortex
removes a header or a query parameter when its name, split at `-` and `_`, has one of the
words `key`, `apikey`, `token`, `auth`, `authorization`, `cookie`, `secret`, `signature`,
`password`, `credential` or `credentials`, with case ignored. So `x-api-key`,
`Idempotency-Key` and `Sec-WebSocket-Key` are removed, and a camelCase name such as
`accessToken` is kept. Cortex does not redact the body. Images,
files the agent read and thinking blocks are kept too. Each part of a body that repeats from
one request to the next is stored once per segment, so this costs about 8–20 KB per request
in the measured sessions. The response is kept as the proxy read it: its status, its headers
without credential headers such as `set-cookie`, and up to 10 MiB of its body. The body is as it came off the wire,
still compressed when the origin compressed it, unless the proxy decompressed it to read it
for a plugin; the capture is then marked `decoded`. A longer body is marked truncated, and a stream that broke off is marked
incomplete. A WebSocket turn's response is the origin's messages for the turn, one per line.

**It runs on a local install, or where `session.archive.dir` is set.** A local install is a
config inside `~/.cortex`, which is what `agentop service install` and `--local` produce. A
config outside `~/.cortex` runs the archive only when it names a `dir`. Without one the archive
is off, and `enabled: true` is refused with a WARN naming the reason. So a cluster cannot start
it by mounting a volume over `~/.cortex`.

Where it can run, it is on by default only while `listener.bind_loopback_only` is true, as the
generated config sets it. That is the one setting under which [clearing it](#clearing-it) works.
With the binds widened, it stays off unless `enabled: true` opts in, and the startup line
then says a clear will be refused. A cluster cannot bind loopback only, so it needs both `dir`
and `enabled: true`.

**A second Cortex keeps its own history.** To run a Cortex beside your everyday one, for
development or a demo, copy `~/.cortex/config.yaml` outside `~/.cortex`, change its ports, and
give it its own directories:

```yaml
cost_ledger:
  dir: /absolute/path/to/demo/cost
session:
  archive:
    dir: /absolute/path/to/demo/sessions
```

The copy keeps `tls_bridge.ca_dir` pointing at `~/.cortex/ca`, so agents trust the same CA.
Cortex owns everything under the archive `dir`: a clear deletes its `data/` directory whole,
and keeps `forgotten.jsonl` beside it.
Give it an empty directory that nothing else uses. Do not give two Cortex processes the same
archive `dir`: a clear in one deletes the files the other is writing.

**How big it gets.** Each session's events are written as zstd-compressed segments that store
a repeated message once rather than once per turn — the same saving the in-memory store makes,
since every LLM request re-sends the conversation so far. The benchmark fixture comes to about
1.6 KB per event. Two bounds apply, checked hourly and on startup, and the oldest segments go
first by the time they were last written:

| Setting | Default | Notes |
|---|---|---|
| `session.archive.enabled` | on when bound to loopback only | `false` always wins; `true` opts in without the loopback bind |
| `session.archive.retention_days` | 30 | how long a segment is kept after its last write; at most 3650 |
| `session.archive.max_bytes` | 2 GiB | the archive's total size; past it, the oldest segments go first |
| `session.archive.dir` | `~/.cortex/sessions` | must be an absolute path; a config outside `~/.cortex` needs it to run the archive |

A full disk does not stop the proxy. The archive stops writing until its next hourly pass,
and requests are served as before; only the history has a gap.

**A removed session leaves a forget record.** When the archive removes a session — `x`, a
clear `X`, the day limit, the size limit, or an import that replaces it — it adds one line to
`~/.cortex/sessions/forgotten.jsonl` (in the archive `dir`, next to `data/`). The line holds
the session id and the time span of its turns, `{"id":"...","from":<unix ms>,"to":<unix ms>}`,
and no content. It writes a line only for a session with tokens or cost, and only when the cost
ledger is on. A clear keeps the file. A line is kept until its span is older than
`cost_ledger.retention_days` (default 31) plus 1 day. `agentop import` uses it: see below.

**What it does not promise.** Writing happens off the request path — and the cost of that is
that a burst the writer cannot keep up with is dropped *from the archive*, never from memory. An
unclean kill loses at most the last second.
`GET /v1/sessions?archived=true` reports both the archive's size and what it lost, under
`archive` (`bytes`, `maxBytes`, `retentionDays`, and `droppedEvents`, `writeErrors`,
`droppedRenames`, `droppedCaptures`, `paused` when any is nonzero); nonzero means what you are
reading has gaps. `droppedCaptures` counts requests kept without their exact bytes, because
more than 256 MB of them waited for the writer.

#### Importing history from before Cortex

`agentop import` adds the sessions that Claude Code keeps in
`~/.claude/projects` (or under `CLAUDE_CONFIG_DIR`), the tasks that Bob
keeps in `~/.bob/db/bob.db`, the sessions that Codex keeps in
`~/.codex/sessions` (or under `CODEX_HOME`), the sessions that OpenCode
keeps in `~/.local/share/opencode/opencode.db` (or under `XDG_DATA_HOME`), and
the sessions that Pi keeps in `~/.pi/agent/sessions` (or under
`PI_CODING_AGENT_DIR`), to this history and to the cost ledger. Bob and
OpenCode need the `sqlite3` program on `PATH`. Use it for the sessions that ran
before you installed Cortex.
`agentop import --since 30d --dry-run` counts first and writes nothing. The
import skips every session that Cortex captured live, and never changes one.
Each imported session is marked `(*)` in agentop. A Claude Code turn, a Codex
turn, an OpenCode turn and a Pi turn are priced at today's rates. A Bob turn
keeps Bob's own cost. LAST 1H shows imported turns
only after the next restart. The import stops at 90% of `max_bytes`. A session
that you erased, or that a limit removed, comes back in full on the next import.
Its forget record tells the import which turns the cost ledger already holds.
The turns inside that span are not counted again, and the turns after it are. A
turn before the span is counted only when the cost ledger has no row of the same
project near it that the proxy wrote live or that an earlier import wrote. Flags
and output:
[`agentop import`](../cmd/agentop/README.md#importing-agent-history-agentop-import).
Rules: [`POST /v1/import`](session-api.md#post-v1import--transcript-import-laptop-only).

#### Clearing it

A restart used to be a reset; with history on disk it is not, so clearing is one action of its
own. **`X` on agentop's sessions pane** asks first — *Erase all N sessions from this Cortex, in
memory and on disk (S)?* — and on `y` erases every session, from memory and from disk, at once.
**The cost ledger is kept**: it holds spend totals and no content, so `today`, `7d` and `month`
read the same afterwards. From a script:

```sh
curl -X DELETE http://localhost:47601/v1/sessions
# {"sessions":12,"archivedSessions":340,"bytes":81234567}
```

**One session, or one agent's.** `x` on the sessions pane erases the highlighted session, and
`x` on the agents pane every session of the highlighted agent — on *All agents*, everything, as
`X` does. Each asks first and keeps the cost ledger. An agent still using a session brings it
back with its next request, and the conversation with it, since every request re-sends it.
From a script:

```sh
curl -X DELETE http://localhost:47601/v1/sessions/<id>
# {"sessions":1,"archivedSessions":1,"bytes":18234567}, or a 404 when this Cortex holds no such session
```

Neither erases the savings accountant's own per-session state: ids, content hashes and the
billing mode, no content, in memory and in `~/.cortex/accountant-state.json`, plus the
keep-alive keeper's entries, which hold the caller's credential and the thread's last request
body (the whole conversation), both masked, context-guru's incoming-model credential holds, and
its forwarded-body stash. A keeper entry goes within 12 minutes with no particular wait, 16
waiting on a person, or 36 waiting on its own tool call (the engine's wait plan raises the ping
count for the wait; its background-wait count never applies here, since Cortex gives it no
open-task data), or within 118 minutes (never more than 236) on a thread billed at the 1-hour
tier. The incoming-model holds (one per distinct host+credential, shared across every session
on it) are masked too, and go within 2h10m of their last use, on a timer, whether or not a later
request arrives. The forwarded-body stash DOES hold content — the last body sent upstream, for up
to 16 sessions and 96 MiB total — evicted only when a newer session's body needs the room, not on
a timer. The rest stays until newer sessions push it out (at most 10,000 threads are
kept), and a state file older than 1 hour is not read at startup (see
[session-api.md](session-api.md#delete-v1sessions)).

Either is refused — a 403 saying why — unless the request names a loopback host
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

### Plugin state is kept in `~/.cortex/plugin-state.json`

Plugins that need state to outlive a restart keep it here. Today that is the inference router:
which server each session is on, as its session id, its agent's name and the server's name — plus,
for each session moved to another server, which server wrote each reasoning block in its history,
by a fingerprint of the block — never the reasoning itself. It holds no prompts and no keys. A session's entry expires 29 to 30 days after its last request; `X`
does not clear it. The proxy saves changes within two seconds and on stop, and sets aside a file it
cannot read as `plugin-state.json.corrupt` and starts empty.

## Configuration

These sections tell how to set parts of `~/.cortex/config.yaml`.

### The `memory:` configuration

The Go runtime sizes its heap to roughly twice the live heap by default, and
reading sessions leaves the memory the encoder expanded from the store's
sharing as idle RSS — together those carried a live heap of ~1.4GB as 2.9GB of
resident memory. `memory.limit_mb` applies the Go runtime's soft memory limit
at startup; a local install defaults it to **2048**.

| Field | Default (local) | Meaning |
|---|---|---|
| `limit_mb` | 2048 | Soft limit in MB. Zero disables it. |

The limit is **soft**: near it the garbage collector runs more often (a little
CPU for the memory), and if the sessions genuinely need more than the limit the
process keeps allocating and growing exactly as it would without one — nothing
fails, nothing crashes. Setting it too low costs speed, not availability. Set
it explicitly to change the default; `agentop service restart` applies it.

### The `accounting:` configuration

The accountant prices what the context operators (`context-guru`, `tool-prune`) saved, and
what the keep-alive pings cost. Its figures land in the cost records' `avoided[]` and `spends[]`
(see [pricing.md](pricing.md#cost-records)). The block is the same on a laptop and in a cluster.

```yaml
accounting:
  enabled: auto          # auto | true | false
  subscription_weights:  # optional; the defaults are shown
    input: 1
    cache_write_5m: 1.25
    cache_write_1h: 2
    cache_read: 0.1
    output: 5
```

| Field | Default | Meaning |
|---|---|---|
| `enabled` | `auto` | `auto` runs the accountant when the outbound pipeline has a context operator. `true` runs it without one, for the cache figures alone. `false` turns it off. |
| `subscription_weights` | the API rate ratios | How much of a subscription's usage limit one token of each tier uses. A saving on a subscription login is valued in these units (`units`, `penaltyUnits`), never in dollars. A key you leave out keeps its default; an explicit `0` stays `0`; a negative weight is refused. |

On a local install the accountant keeps its state in `~/.cortex/accountant-state.json`, so
a restart does not forget which prefix the provider has cached. The file holds hashes and sizes,
never prompt text. If the file would pass 20 MiB, Cortex leaves the threads seen longest ago out
of it, so it stays under 20 MiB, and logs one WARN (`over its size limit`). Those threads stay
in memory; only a restart forgets them. If Cortex does not know the file's format (a newer
accountant), it removes the file instead and a restart starts fresh. Both fields are read once at
startup. A reload that changes a weight is refused, and so is a reload that turns accounting
on (as resolved) when the proxy started with it off; `agentop service restart` applies either.
A reload that turns it off is accepted: the accountant stays idle until a later reload turns
it back on.

### The `metrics_push:` configuration

Cortex can push the accountant's savings counters to a central OpenTelemetry collector. **It
sends nothing until you set a URL.** `agentop setup` does not set one.

```yaml
metrics_push:
  enabled: true     # default true, but with no URL nothing is sent
  url: ""           # the collector's base URL; Cortex posts to <url>/v1/metrics
  token: ""         # sent as Authorization: Bearer <token>
  interval: 30s
```

| Field | Environment override | Default | Meaning |
|---|---|---|---|
| `enabled` | `METRICS_PUSH` | `true` | A value that is not a boolean turns the push off. |
| `url` | `METRICS_PUSH_URL` | *(none)* | Must be `https`. `http` is allowed only to a loopback address. A URL with user info (`https://user:pass@host`) or a query string is refused, because neither is sent: put the credential in `token`. In the file that fails startup; in `METRICS_PUSH_URL` it logs a WARN and turns the push off. |
| `token` | `METRICS_PUSH_TOKEN` | *(none)* | Never logged. `/config` serves it redacted. |
| `interval` | `METRICS_PUSH_INTERVAL` | `30s` | How often the counters are pushed. |

What is sent: the `llm.savings.*` counters (tokens saved and added by tier and component, their
dollar value, cache effects, request and check counts), `llm.proxy.export_failures`, and
`llm.proxy.info`: a gauge that is always 1, with the build's `version` as its label. It is sent
on every push, also before the first request, so the central dashboards list an idle machine and
the `MachineSilent` alert sees when a machine stops pushing. The
resource carries `service.name=cortex`, `service.version` and `service.instance.id`: a random
id kept in `~/.cortex/machine-id` (mode 0600), so the collector sees one machine across
restarts. In a cluster the id is random per process. No prompt, path, host, session or key is
sent. Dollar counters move only for requests billed per token (an API key); a subscription
login moves the token counters alone.

The counters are cumulative. A push that fails is counted in `llm.proxy.export_failures`,
logged as a WARN at most once a minute, and retried by the next push. It never delays or fails
a request. With no URL the proxy logs one line at startup, `metrics push off`, and opens no
connection.

A bad block in the config file fails the config load, like any other block: Cortex does not
start, or a reload is refused. The `METRICS_PUSH*` variables are read at startup; a bad
`METRICS_PUSH_URL` logs a WARN and turns the push off, and Cortex runs. No error or log line
quotes the URL's user info or query string, so a credential put there does not leak into the
log.

The push starts with the accountant, so it needs `accounting` to be on. The block is read
once at startup: a reload that changes it is refused, and `agentop service restart` applies
it.

### The `tls_bridge:` configuration

The TLS bridge terminates agent egress so the outbound plugin pipeline sees
decrypted HTTPS. It is configured under the top-level `tls_bridge:` block in
`~/.cortex/config.yaml` (`authbridge-runtime-config` in a cluster):

| Field | Default | Meaning |
|---|---|---|
| `mode` | *(off)* | `disabled` or `enabled`. Empty or an absent block means off. |
| `ca_dir` | — | Directory of the signing CA: the bridge reads `tls.crt` + `tls.key` to sign leaves; `ca.crt` is the trust cert handed to the agent (cert-manager Secret key conventions, so only the directory is configured). On a laptop this is `~/.cortex/ca`. |
| `generate_ca` | `false` | Mint and persist a self-signed CA into `ca_dir` when those files are absent, instead of failing at startup. For standalone/demo use only — in-cluster the CA is a mounted Secret and this stays false so a missing Secret fails loudly. `ca_dir` should persist across restarts: on ephemeral storage a fresh CA is minted each boot, and clients must re-trust the new `ca.crt`. |
| `upstream_ca_bundle` | *(system roots)* | Extra-roots PEM file for re-origination, for private-CA origins the agent trusts. Empty means system roots only. |
| `upstream_insecure` | `false` | Disable origin cert verification on re-origination, for internal/self-signed upstreams whose CA is unavailable. Prefer `upstream_ca_bundle` when possible. |
| `passthrough_hosts` | *(a default list)* | Hosts to tunnel, never intercept. Distinct from `listener.skip_hosts`, which bypasses the whole pipeline: these still run the egress gate, they just aren't TLS-terminated. See [Developer tooling is not intercepted at all](troubleshooting.md#developer-tooling-is-not-intercepted-at-all). |
| `fail_closed` | `false` | Bridge or refuse every connection the bridge intercepts, never tunnel it. Needs `mode: enabled`. See [Failing closed](troubleshooting.md#failing-closed). |
| `ports` | `{443, 8443}` | TCP ports to intercept as TLS. Only HTTP(S)-bearing ports belong here — the bridge serves the decrypted stream as HTTP/1.1 or h2, so terminating a non-HTTP TLS protocol (LDAPS, SMTPS, DB-over-TLS, …) would break it. |

The bridge is built once at startup, so changing any `tls_bridge:` field takes
effect only after `agentop service restart`. Note the reload does not *refuse*
the edit — `/reload/status` reports a successful reload and `/config` serves
the new values while the running bridge keeps its startup settings. Restart to
apply.

With `mode: enabled`, the session API's bind is pinned to loopback whatever
`listener.session_api_addr` says (an explicit `0.0.0.0:9094` becomes
`127.0.0.1:9094`): under the bridge it serves **decrypted** request and
response bodies, and it is unauthenticated. `kubectl port-forward` and
agentop still reach it through the pod's loopback.

### Praxis mode

To turn praxis mode on, run the installer with `--praxis`. It installs
`cortex-gateway` and adds `praxis: {enabled: true}` to the config. It changes nothing
else in the file:

```sh
curl -fsSL https://www.rossoctl.dev/cortex/install.sh | sh -s -- --praxis
```

`--praxis` needs a release that carries `cortex-gateway`. If the installer stops with
`release … has no cortex-gateway build for <os>/<arch>`, add `--ref=main` to install
the channel build, which has the gateway, or build it from source as below.

To put a policy in the gateway, read
[policy in praxis mode](praxis-policy.md).

When you change `praxis.enabled` by hand in either direction, run
`agentop service install --restart`. To turn praxis mode off, set
`praxis.enabled: false` in `~/.cortex/config.yaml` first. `restart` alone does not
write the unit again, and on Linux the unit has `--supervise` only in praxis mode.
`install` without `--restart` can report "Already current" and not restart.

From a source tree, build the gateway and give it to setup:

```sh
cargo build --release -p cortex-gateway --manifest-path praxis/Cargo.toml
cp praxis/target/release/cortex-gateway bin/
bin/agentop setup --from bin --yes --no-modify-path --restart --praxis
```

## How it works

These sections tell how Cortex works inside. You do not need them for daily use.

### The supervisor

The proxy is supervised: a killed proxy is restarted within seconds. It is worth seeing once:

```sh
kill -9 $(pgrep -f 'cortex --config')             # comes back within ~2s
agentop service status                            # healthy again
```

On macOS you will see **two** `cortex` processes: a supervisor (the one
launchd starts, holding no ports) and the proxy itself. launchd does not restart user
agents added mid-session — verified across `KeepAlive`, `StartInterval` and
`RunAtLoad` — so the supervisor is what makes crash recovery work. On Linux there is
one process; systemd handles it.

In praxis mode (`praxis.enabled: true`), the supervisor has a second child,
`cortex-gateway`, so you see three processes. At each start the supervisor writes
`~/.cortex/gateway.token` and `~/.cortex/gateway.yaml`. A restart of the proxy does
not restart the gateway. When the supervisor exits, the gateway exits too. The
supervisor reads `praxis.*` only when it starts, so after you change `praxis.*`, run
`agentop service restart`. Until then, the next restart of the proxy fails at each
try.

On Linux, the systemd unit runs `cortex --supervise` in praxis mode, so you also see
the supervisor, the proxy and the gateway. In direct mode the unit does not use
`--supervise`, and systemd restarts the proxy itself. `agentop service install` writes
the unit again when it does not agree with `praxis.enabled` in the config file.

### How traffic is grouped into sessions

Each Claude Code session gets its own bucket, named with that session's id — the same
UUID Claude Code uses for its own transcript, so `agentop` and
`~/.claude/projects/<project>/<id>.jsonl` agree on what a session is. Two windows open in
different repos are two buckets, and resuming a session (`claude --resume`) files back
into the original one rather than starting a new one.

This works because Claude Code puts `X-Claude-Code-Session-Id` on every inference
request. IBM Bob (the editor and the `bob` shell command) is grouped the same
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
default does not read it, because it is a generic name as well, which Pi (through the
extension `agentop configure pi` installs) and similar frameworks send too. OpenCode also sends `X-Opencode-Session-Id`, the
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
  no agent (your own terminal's `curl`, an IDE's update check) lands in `default`, even
  while no agent is talking. Before, it went to the most recently active session once no
  agent had named a session for five minutes. So an idle laptop filed every background
  poll under the last session to speak. Which process named which session survives a
  restart through the session archive. So a process that keeps running, such as Bob's
  plugin host, keeps its session before it sends its session header again. An
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
  `session.max_sessions` of them ([Session history](#session-history-is-kept-in-cortexsessions)). On a laptop that is a non-issue: you own
  every session. It matters where telemetry attribution is a trust boundary rather than a
  convenience, and `session.id_headers: []` is the way to opt out there.

### The cpex build

The laptop cpex build is removed, and `install.sh --cpex` exits with an error. That error says to run the
installer again without `--cpex`, which installs the default `cortex`. It also
names `--praxis` and `docs/praxis-policy.md`, which is the way to run laptop
policy now. `make
cortex-cpex` is gone too. An install that runs a cpex build moves to the default
`cortex` on `agentop update`, which prints one line that says so.

The default `cortex` has no cpex plugin, and it does not start on a config that names
`cpex`. So when `~/.cortex/config.yaml` has a `cpex` entry under `pipeline`,
`agentop update`, the automatic update and the one-liner stop before they download.
Remove the entry first, as [Move from the CPEX plugin](praxis-policy.md#move-from-the-cpex-plugin)
says.

An `agentop` from before the removal asks the release for the cpex tarball. So each
release still publishes `cortex-cpex_<ver>_<os>_<arch>.tar.gz`, as a copy of the
default `cortex` tarball, and that older update moves the install too. It does not
check the config first: when the config names `cpex`, setup rolls back and the
update fails. Remove the entry, then update again.

Kubernetes use does not change: the `authbridge-cpex` image, the `cpex` and `fullcpex`
build profiles, `core/plugins/cpex/` and the `go-ci-cpex` CI job stay. See
[cpex-plugin.md](cpex-plugin.md).

Every release tarball carries build provenance as well as its `checksums.txt` entry:
`gh attestation verify <tarball> --repo rossoctl/cortex`.

## Why traffic disappears from `agentop`

Moved to [Troubleshooting](troubleshooting.md#why-traffic-disappears-from-agentop).

## Install and TLS bridge problems

These sections moved to [Troubleshooting](troubleshooting.md):

- [`agentop: command not found`](troubleshooting.md#agentop-command-not-found)
- [Restricted environments (sandboxes, no launchd session)](troubleshooting.md#restricted-environments-sandboxes-no-launchd-session)
- [`502 Cortex proxy is not serving`](troubleshooting.md#502-cortex-proxy-is-not-serving)
- [Everything shows as `tunnel` and no plugin ever runs](troubleshooting.md#everything-shows-as-tunnel-and-no-plugin-ever-runs)
- [A request gets `421 Misdirected Request`](troubleshooting.md#a-request-gets-421-misdirected-request)
- [Developer tooling is not intercepted at all](troubleshooting.md#developer-tooling-is-not-intercepted-at-all)
- [Failing closed](troubleshooting.md#failing-closed)
- [Go tools on macOS are passed through unread](troubleshooting.md#go-tools-on-macos-are-passed-through-unread)
