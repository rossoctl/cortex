# Running Cortex: start, stop, and remove it

Cortex runs as a service, so there is nothing to launch by hand and nothing to keep
in a terminal. Everything below is `abctl service`; run it with no action to see the
list.

```sh
abctl service status      # is it running, and is it answering?
abctl service start       # start it
abctl service stop        # stop it, and keep it stopped
abctl service restart     # stop and start
abctl service install     # set it up in the first place (the installer does this)
abctl service uninstall   # stop it and remove the service
```

**Never use `kill` or `pkill` to stop it.** The proxy is supervised, so killing it gets
it restarted within a couple of seconds, which looks like a process refusing to die.
`abctl service stop` is the stop that works.

That restart is the point, though, and it is worth seeing once:

```sh
kill -9 $(pgrep -f 'authbridge-proxy --config')   # comes back within ~2s
abctl service status                              # healthy again
```

On macOS you will see **two** `authbridge-proxy` processes: a supervisor (the one
launchd starts, holding no ports) and the proxy itself. launchd does not restart user
agents added mid-session — verified across `KeepAlive`, `StartInterval` and
`RunAtLoad` — so the supervisor is what makes crash recovery work. On Linux there is
one process; systemd handles it.

## Re-running the installer is safe

The one-liner is how you upgrade, so it is meant to be run repeatedly. When nothing has
changed it changes nothing: it does not re-download binaries already at that version, and
`service install` reports `Already current` and leaves the running proxy alone rather
than restarting it.

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
restarting. When a restart genuinely is needed, install says how many connections it is
about to cut.

To restart deliberately: `abctl service restart`.

## How traffic is grouped into sessions

Each Claude Code session gets its own bucket, named with that session's id — the same
UUID Claude Code uses for its own transcript, so `abctl` and
`~/.claude/projects/<project>/<id>.jsonl` agree on what a session is. Two windows open in
different repos are two buckets, and resuming a session (`claude --resume`) files back
into the original one rather than starting a new one.

This works because Claude Code puts `X-Claude-Code-Session-Id` on every inference
request. Bob, our internal coding agent (not the `bob` demo user), is grouped the same
way, by the `X-Task-Id` it sets, but its buckets are named with a task id rather than a
session uuid, so one bucket covers however long Bob reuses that task. Note that
`X-Task-Id` is a generic name: traffic from anything else that sends it will be grouped
under its value too. Traffic that carries no such header falls back to the previous
behavior — the most recently active session, or the `default` bucket. In practice
`default` collects Claude Code's own connectivity probe (`HEAD /api/hello`) and anything
else that egresses through the proxy without announcing a session.

Some limitations worth knowing:

- **Tool calls are attributed by timing, not identity.** MCP requests carry no session
  header, so they are filed under whichever session was most recently active. That is
  right when sessions take turns and can misattribute when two are genuinely
  interleaved. Inference traffic — where the tokens and the cost are — is always exact.
- **Agents other than Claude Code and Bob need to be named.** Set `session.id_headers`
  to a list of headers to consult in precedence order if you run a client with its own
  session header; naming any replaces the built-in list rather than adding to it. An
  explicit empty list (`session.id_headers: []`) turns grouping off and puts everything
  back in one bucket.
- **Bucket names are taken on trust.** The id comes from the client's own header and is
  not authenticated, so a client can name any bucket — including another session's, or a
  stream of ids nobody owns, which evicts real buckets once there are more than
  `session.max_sessions` of them (see below). On a laptop that is a non-issue: you own
  every session. It matters where telemetry attribution is a trust boundary rather than a
  convenience, and `session.id_headers: []` is the way to opt out there.

## Why traffic disappears from `abctl`

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
- `abctl` fetches the **most recent** 500 events of a session, not all of them, and says
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
  vanished from `abctl` entirely rather than just losing its oldest rows, this is why.
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
need a restart to change — they are not hot-reloaded. Cost totals are the one thing that
does survive a restart; see below.

## Cost history is written to `~/.cortex/cost`

**A local install keeps a cost ledger on disk, on by default.** Sessions themselves —
prompts, completions, tool arguments — stay in memory and die with the process. Per-minute
cost totals do not: they are appended to `~/.cortex/cost/YYYY-MM-DD.jsonl`, one file per
local day, **kept for 30 days**.

**Sizing, because "roughly 10 MB" was a laptop figure and is not general.** A row is about 418
bytes, and there is one per minute *per distinct (endpoint, model, agent, provenance)*. A laptop
writes rows only for minutes with traffic, which is where 10 MB comes from. Continuous traffic
populates all 1,440 minutes of a day:

| Distinct combinations per minute | Per day | Per 30 days |
|---|---|---|
| 2 | 1.2 MB | 36 MB |
| 8 | 4.8 MB | 144 MB |
| 64 (the per-minute cap) | 38.5 MB | 1.16 GB |

Size a mounted volume from that table, not from the laptop number, and lower
`retention_days` if the top row is closer to your traffic.

**Where that default comes from.** It is **on wherever the ledger can survive a restart** —
which means either an explicit `cost_ledger.dir` (in Kubernetes, a path on a mounted volume) or
being started from a config inside `~/.cortex`, which is what a local install is and what both
`abctl service install` and `--local` do. A resolvable `$HOME` is deliberately *not* enough:
`HOME=/root` resolves in almost every container, and neither is a leftover `~/.cortex` directory,
which is not a decision anybody made. A container with neither signal can only write to a layer
that is discarded on restart, so there it stays off and says why in a startup log line.

That rule replaced an earlier one keyed on `--local`, which was the cause of a real bug: the
installed service runs `authbridge-proxy --config ~/.cortex/config.yaml` and never `--local`, so
"on by default" was false for every install. The generated config also writes
`cost_ledger: {enabled: true}` explicitly, which is now belt-and-braces rather than the
mechanism — a config generated before that was added still gets the ledger, because the default
no longer depends on the file. `grep -A1 cost_ledger ~/.cortex/config.yaml` shows which you
have.

It exists because the in-memory counters are a 6-hour ring, and the proxy restarts several
times a day. Without the ledger, both `today` and `7d` are still answered — from the ring's
maximum window, with the response's own `window` field naming the span that was actually
covered rather than the one you asked for. So the figures stay honest and get much smaller:
six hours of a day, and six hours of a week.

**What is in the files.** One JSON line per minute per (endpoint, model, agent,
provenance): the host, the model name, the calling agent's User-Agent, token counts,
dollar figures and a timestamp. **No prompt content, no completions, no tool arguments** —
that is a promise a test asserts against the serialized bytes, not a convention.

**Who can read them.** Anything that can read your home directory, and anything that can
reach the session API, which is **unauthenticated** (the proxy logs `UNAUTHENTICATED; contains
raw user content; never expose via ingress` when it starts). On a laptop it binds to localhost.

`abctl` is not yet one of those readers, and the distinction is worth being exact about:
`GET /v1/usage` reaches these files only for the symbolic windows `today` and `7d`, and
`apiclient.GetUsage` takes a duration, so every view abctl draws today is served from the
6-hour ring instead. Reading the ledger from abctl is the next step, not this one.
Nothing about the ledger makes that worse, but "my spend is on disk and readable" is worth
knowing rather than discovering.

**To turn it off**, in `~/.cortex/config.yaml`:

```yaml
cost_ledger:
  enabled: false
```

then `abctl service restart`.

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
| `cost_ledger.retention_days` | 30 | Minimum **9** when set. `window=7d` is a rolling 7×24h, not seven calendar days, so it can open **nine** local day files: one extra because a rolling span starts part-way through a date, and one more because a spring-forward week is 167 hours, so the span reaches an hour further back. A shorter retention answers `window:"7d"` over a partial week with nothing saying so |

## `abctl: command not found`

The installer puts both binaries in `~/.local/bin`. If that is not on your PATH it
offers to add it to your shell profile; new terminals pick it up, and for the one you
are in:

```sh
export PATH="$HOME/.local/bin:$PATH"
```

To undo, delete the two lines the installer marked in your profile.

## Restricted environments (sandboxes, no launchd session)

Some environments cannot manage services at all — a sandboxed shell, a session without
a usable launchd domain, CI. `abctl service install` detects this before writing
anything and tells you so, rather than failing at `launchctl bootstrap` with
`Input/output error`.

Cortex still runs there; it just is not supervised:

```sh
authbridge-proxy --local     # in its own terminal, or backgrounded
abctl                        # the viewer, as usual
```

What you give up: no restart after a crash, and nothing brings it back at login. Stop
it with `kill $(pgrep -f authbridge-proxy)` — there is no service to stop.

Two assumptions that do not hold in such environments, and what happens:

| Assumption | If it does not hold |
|---|---|
| `launchctl` can manage the user domain | `service install` stops early and prints the command above |
| `$HOME` is your login home | launchd never scans `$HOME/Library/LaunchAgents`, so the service cannot start at login. `service install` warns and continues; crash recovery still works while you are logged in |

## Is it working?

```sh
abctl service status
```

`installed:` names the unit file, `healthy:` names the endpoint that answered. If it
says `NOT answering`, the proxy is loaded but not serving — check `~/.cortex/proxy.log`.

To see traffic rather than status, run `abctl` with no arguments.

## Start and stop

```sh
abctl service start
abctl service stop
```

A stop persists: Cortex stays down across logouts and reboots until you start it
again. That is deliberate — a stop that quietly undoes itself at your next login is
worse than none.

`stop` also reports how many connections it cut, because until Cortex is back those
clients have nowhere to go: `HTTPS_PROXY` is fixed in each one's environment when it
starts, so none of them can fall back to a direct connection. Run `abctl service start`
and they reconnect on their next request — the sessions themselves do not need
restarting.

## Three ways to turn it off

They are different, so pick deliberately:

### Pause it

```sh
abctl service stop
```

Claude Code fails while Cortex is stopped, because its settings still point at the
proxy. Either start Cortex again or unwire Claude Code (below).

**A running session cannot route around a stopped Cortex.** `HTTPS_PROXY` is fixed in
its environment when it starts, so it has no way to fall back to a direct connection,
and `abctl configure claude-code disable` cannot reach it — that only affects
sessions started afterwards. What it needs is Cortex back: `abctl service start`,
after which it reconnects on its next request without being restarted. `service
stop` tells you how many connections it cut, for exactly this reason.

Use `abctl service stop`, not `kill` or `pkill` — the supervisor restarts the
process within seconds, which looks like it refusing to die.

### Unwire Claude Code

```sh
abctl configure claude-code disable
```

This removes only the keys Cortex added to `~/.claude/settings.json`
(`HTTPS_PROXY`, `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`, and the CA variables
`NODE_EXTRA_CA_CERTS` / `SSL_CERT_FILE` / `GIT_SSL_CAINFO` / `REQUESTS_CA_BUNDLE` /
`CURL_CA_BUNDLE`) and leaves anything else in that file alone. Claude Code goes
straight to the API again. Restart `claude` to pick it up.

There are several CA variables because anything Claude Code spawns inherits
`HTTPS_PROXY` and so must also be able to verify the bridge. They do not all get
the same file: `NODE_EXTRA_CA_CERTS` **extends** Node's trust store, so it gets
`ca.crt`, while the rest **replace** the trust store and get `bundle.crt` — the CA
followed by this machine's public roots. Pointing a replacing variable at `ca.crt`
would leave that tool trusting one private CA and nothing else, which breaks every
direct TLS call it makes.

### Everything shows as `tunnel` and no plugin ever runs

`abctl observe` shows rows like this, with `tunnel` in ACTION and no method or status:

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
authbridge-proxy --local --ca-dir /Users/you/.cortex/ca
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
| `client-hung-up` | The client vanished mid-handshake. Often a cancelled request; not evidence about trust, which is why it carries no advice. | usually no |
| `handshake-failed` | Some other handshake failure — a version, cipher or ALPN mismatch, or Cortex failing to mint a certificate. | check `error=` in the log |
| `origin-unverified` | **Cortex** could not verify the destination's certificate, so it declined to vouch for it. Bridging would have meant terminating TLS for a server we could not authenticate. | investigate the destination |

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

`SSL_CERT_FILE` — the Go one, covering `go`, `gh` and `abctl` itself — **does
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
`abctl configure claude-code enable` prints this note when it runs on macOS.

Cortex keeps running; nothing sends traffic to it. `abctl configure claude-code
enable` puts it back.

### Remove it

```sh
abctl configure claude-code disable   # 1. unwire Claude Code
abctl service uninstall               # 2. stop it and remove the service
rm -rf ~/.cortex                      # 3. config, CA, logs, cost history, abctl's UI settings
rm -f ~/.local/bin/abctl ~/.local/bin/authbridge-proxy
```

Order matters for the first two: `abctl configure claude-code disable` needs to
read the config that step 3 deletes.

#### Check nothing is left

```sh
abctl configure claude-code status   # should say "not enabled"
pgrep -fl authbridge-prox            # should print nothing
ls ~/.cortex 2>/dev/null             # should print nothing
```

The CA that step 3 removes was only ever trusted through the CA variables in
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
it simply stops being parsed, which shows up as `tunnel` rows in `abctl observe`.
Restart those agents and they are visible again.

An ordinary upgrade is unaffected — it keeps the existing CA. This only applies when
`~/.cortex` has been deleted, or on a first install with agents already running.

#### If `abctl` is already gone

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
machine you believe you have just cleaned. `abctl configure claude-code disable` removes all
seven in the right order, which is why it is step 1 above; this list is only for when
that binary is already gone.
