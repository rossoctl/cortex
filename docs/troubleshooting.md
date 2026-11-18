# Troubleshooting

This page lists problems that a laptop install can have, and the fix for each.

## Run `agentop doctor` first

`agentop doctor` runs the checks that setup runs, and checks of its own.
It changes nothing.
It prints the fix for each problem that it finds.
Exit status 0 means that there is nothing to fix.
Exit status 1 means that there is something to fix.

## Install

### `agentop: command not found`

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

### Restricted environments (sandboxes, no launchd session)

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

## My agent does not show in agentop

### Why traffic disappears from `agentop`

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
local install so do the sessions themselves, in the session archive. See [Cost history](laptop-service.md#cost-history-is-written-to-cortexcost) and [Session history](laptop-service.md#session-history-is-kept-in-cortexsessions).

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

The proxy log names the offender, the cutoff, and which CA is actually in force. When
Cortex knows which program opened the connection, the line also has `program=`, its
executable. When that program runs under an agent, `agent=` names the agent's executable:

```sh
grep 'client-rejected-ca' ~/.cortex/proxy.log
# ... client=127.0.0.1:58041 program=/usr/bin/python3 agent=/Users/you/.local/bin/claude
#     ca_not_before=2026-09-09T17:11:39-04:00
#     ca_fingerprint=CD:19:CD:2C:... ca_file=/Users/you/.cortex/ca/ca.crt ...
```

Restart that program if it started before `ca_not_before`. When the line has no
`program=`, nothing can name the client after the fact: the connection is gone by the
time you read the log. Restart each agent that started before `ca_not_before`.

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
| `skip-cached` | An earlier handshake for this host failed, so it is not intercepted for a short window. This applies only to a connection whose program Cortex cannot name — a failed process lookup, or `session.process_attribution: off`; when it can name the program, that program's own record decides instead (`program-refused`). Any failed handshake seeds this, not only a CA rejection — the seeding failure logged its own reason. The window starts at 30s and lengthens only if **rejections** keep coming — a hang-up or a cipher mismatch seeds it but never escalates it; the first client that *does* trust the CA clears it immediately. | find the earlier failure in `proxy.log` and fix that client |
| `program-refused` | An earlier handshake by the program that opened this connection failed, so Cortex did not try to read this one. Only that program is affected: another program talking to the same host is still read. Usually the earlier failure was a CA rejection, but any failed handshake seeds this — the `proxy.log` line for that failure names the program (`program=`, and `agent=` when it runs under one) and logged its own reason. Cortex tries again after a window that starts at 30s and lengthens only if **rejections** keep coming — a hang-up or a cipher mismatch never adds to the count or lengthens the window, and nor does a rejection arriving while the window is still open. After three rejections in a row, each after a window, Cortex stops trying for that program **until Cortex restarts**. A hang-up or a cipher mismatch that is the program's first failure does start the count at one, so a hang-up followed by two such rejections stops it too. A process that started before Cortex's CA (`ca_not_before`) is remembered on its own instead, not as its program: it cannot have loaded that CA, so restarting that process is enough, and a newly started copy of the program is read meanwhile. | find the earlier failure in `proxy.log`; if it was `client-rejected-ca`, restart the client if it started before `ca_not_before`, otherwise point it at the CA, then restart Cortex |
| `os-trust-only` | A Go program on macOS, while macOS does not trust Cortex's CA. Such a program reads no CA file and trusts only the keychain, so showing it Cortex's certificate could only fail; Cortex passes it through from its first connection instead. See [Go tools on macOS](#go-tools-on-macos-are-passed-through-unread). | only if you want it read: trust the CA in your keychain |
| `bridge-disabled` | No TLS bridge is configured. | only if you wanted one |
| `client-hung-up` | The client vanished mid-handshake. Often a cancelled request; not evidence about trust, which is why it carries no advice. OpenCode hangs up rather than rejecting the CA; see [its page](agents/opencode.md#ca-trust). | usually no |
| `handshake-failed` | Some other handshake failure — a version, cipher or ALPN mismatch, or Cortex failing to mint a certificate. | check `error=` in the log |
| `origin-unverified` | **Cortex** could not verify the destination's certificate, so it declined to vouch for it. Bridging would have meant terminating TLS for a server we could not authenticate. Under `fail_closed` the CONNECT is answered `502` `upstream_unverified` instead, and nothing is tunnelled. The first failure for each host logs a WARN (`reason=upstream-verify`). Later failures for that host log at INFO. | investigate the destination |
| `sni-mismatch` | Under `fail_closed`, the client's TLS SNI named a host other than the CONNECT target, so the handshake was refused. The close row carries `421` and `error.kind: misdirected_request`. | check what the client is connecting to |
| `refused-non-tls` | Under `fail_closed`, a connection to a host the bridge intercepts sent bytes that are not a TLS handshake, so it was closed. The close row carries `error.kind: not_tls`. | add the host to `passthrough_hosts` if it should be tunnelled |
| `dial-failed` | Cortex could not reach the destination at all — a DNS failure, a refused connection or a timeout. Without the TLS bridge the CONNECT is answered `502` and no tunnel opened. With the bridge on (this install's default) the CONNECT is answered `200` first, because a redirect can still send the request to a live server; when nothing rescued it, the close row carries that `200` with the dial error in `error`. The TCP dial is bounded at 10s, so a destination that silently drops the SYN (off the VPN that reaches it, a dead NAT) fails within that rather than hanging the agent for longer. | check the destination and the network path to it |

Only a CA rejection asks you to restart anything: `client-rejected-ca` itself, and
`program-refused` when the earlier failure it remembers was one. The others are either
working as intended or point somewhere other than your agents — which is why the reason
is worth reading before acting on it.

`program-refused` and `os-trust-only` are about a program rather than a host, so Cortex
also keeps them as a list, which `agentop doctor` prints and the stats listener serves:

```sh
curl -s http://127.0.0.1:47602/tls-bridge/unread
```

The stats port also serves Go's pprof profiles under `/debug/pprof/` — this answers
"what is the proxy holding in memory", which `ps` RSS cannot (freed pages the kernel
has not reclaimed still count as resident on macOS):

```sh
curl -s --noproxy '*' http://127.0.0.1:47602/debug/pprof/heap -o /tmp/heap.pb.gz
go tool pprof -top -inuse_space /tmp/heap.pb.gz
```

`go tool pprof` cannot read the profile from a pipe, so the two-step form it is. The
`--noproxy '*'` matters on this machine: the shell's proxy env points at cortex itself.
The profiles expose stack traces and argv rather than object contents, but are served
only when `listener.bind_loopback_only` is true (the local install's default), and the
endpoints refuse a non-loopback Host or any browser Origin.

`programs` has one entry per program Cortex is not reading: `program` and `agent` (the
executables), `reason`, `connections` (how many of the program's connections Cortex has
passed through unread since Cortex started), `failures` and `stopped` (on `program-refused`),
and `lastHost`. `pid` appears on an entry kept for one process that started before Cortex's
CA. `hosts` is the host memory behind `skip-cached`: the hosts passed through for clients
whose program cannot be named. On macOS, `osTrustsCA` is whether macOS trusted Cortex's CA
when it last checked.

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
endpoint there: it would silently stop every plugin on that traffic, with no error anywhere to
notice it by.

### Go tools on macOS are passed through unread

`SSL_CERT_FILE` — the Go one, covering `go`, `gh`, `helm` and `agentop` itself — **does
nothing on macOS**. Go's `crypto/x509` honours it only in `root_unix.go`, which is built
for `linux || freebsd || …` and excludes darwin; on darwin verification is handed to
Security.framework, which consults the keychain alone. No environment variable can change
that.

So on a Mac, Cortex shows a Go program its certificate only when macOS trusts Cortex's
CA. It recognises a Go program from the executable's build information, asks macOS —
reading, never writing — whether it trusts Cortex's CA, and when it does not, passes the
program's HTTPS through unread. The row says `os-trust-only`. The program works exactly
as it does without Cortex, from its first connection; Cortex records the host and the
bytes, as for any tunnel, but not the content.

To have Cortex read those programs too, trust the CA in your login keychain:

```sh
security add-trusted-cert -k ~/Library/Keychains/login.keychain-db \
  -p ssl ~/.cortex/ca/ca.crt
```

Cortex notices within a minute, without a restart. Undo with:

```sh
security delete-certificate -c authbridge-tls-bridge-ca \
  ~/Library/Keychains/login.keychain-db
```

Trusting the CA does not have every Go program read. One that relays another system's
traffic — `gvproxy`, which carries the podman VM's, whose clients trust their own CAs — or
that trusts a CA list of its own without Cortex's CA in it (`helm --ca-file` naming another
CA, say) is shown Cortex's certificate once macOS trusts it, and refuses it. That
connection fails, and Cortex passes the program through as `program-refused` for a window.

`git`, `curl` and Python are **not** affected on macOS — they read their bundles
through OpenSSL/LibreSSL, which honours the variables on every platform. On Linux
`SSL_CERT_FILE` works normally, so a Go program there is read like any other.
`agentop configure claude-code enable` prints a short form of this note on macOS
when it changes your settings (not on a re-run that finds them already enabled).
Setup does not print it. `agentop doctor` lists every program Cortex is passing through
unread, and why, from the proxy's `/tls-bridge/unread` report; on macOS it adds the
command above when Go programs are among them.

## Errors

### `502 Cortex proxy is not serving`

An agent routed through Cortex shows this when the proxy is installed and routed but
not currently serving — a config it cannot load, a crash it is restarting from, or one
of its other ports held by another process (a local install pins them: 47601 for the
session API, 47602 for stats, 47604 for health). The supervisor answers
on the proxy's address while no proxy is running, so that you get this message instead
of a bare connection error that names nothing:

```
API Error: 502 Cortex is routed in this environment (HTTPS_PROXY) but is not serving — run: agentop service start
```

(One line; wrapped here only if your terminal is narrow.)

**The one case this cannot cover is a conflict on the forward address itself** — 47600,
or whatever `listener.forward_proxy_addr` names. The supervisor holds that address only
while it is free, so if another process has it, neither the proxy nor this message can
get there and you are back to a bare connection error. `~/.cortex/proxy.log` is where to
look: the proxy says `cannot start: another process already holds this address` on every
start attempt, and names the holder's pid and executable where the kernel will say. Where
it could not, the same line hands you the `lsof` to find it yourself:

```sh
lsof -nP -iTCP:47600 -sTCP:LISTEN
```

The fix is the one in the message. If `agentop service start` does not take, run
`agentop doctor`, which names the reason, and read `~/.cortex/proxy.log` for what the
proxy said as it failed.

No single response field reaches every client, so the response sets two: the reason
phrase (`Cortex proxy is not serving`) and the body above. What you actually see
depends on which client you are reading this from:

| Client | What it shows |
|---|---|
| `curl` 8.7.1 | `curl: (56) CONNECT tunnel failed, response 502` — neither the reason phrase nor the body |
| Go `net/http` | the reason phrase, as the error text |
| Claude Code | the body, inside its own `API Error: 502 ...` framing |

Two things to expect, neither of them fast. Requests **fail** rather than going out
unobserved, which is deliberate: Cortex does not let traffic past unseen just because
it is down — failing closed buys legibility, not speed. And the message is not
prompt: against Claude Code it took three retries and over two minutes before this
surfaced, so allow a couple of minutes before concluding the fix did not work.

This does **not** appear after `agentop service stop`, which stops the supervisor
too — nothing is left to answer on the address, so a stopped Cortex still looks like
a bare connection error, and `stop` already tells you how many connections it cut.

The supervisor reads the address once, at its own start. If you edit
`listener.forward_proxy_addr`, restart the service rather than only the proxy — the same
rule every other `listener.*` field follows.

**macOS only, in practice.** The supervisor exists only where `agentop` installs it
with `--supervise`, which is the launchd path. The systemd unit deliberately does not
pass `--supervise` — it relies on `Restart=on-failure` instead — so on Linux there is
no supervisor to hold the address, and a Linux local install still gets a bare
connection-refused in this situation.

### A request gets `421 Misdirected Request`

Inside a bridged tunnel, every decrypted request must name the tunnel's own destination
in its `Host` header — same host, compared without case or a trailing dot, and the same
port, `443` when none is given. One that names another host or port is refused with a
421 before any plugin runs and is never sent anywhere, because the proxy would dial the
CONNECT target while every host-based policy judged the `Host` header. The timeline shows
it as a 421 with `error.kind: "misdirected_request"` under the CONNECT target's host. A
plaintext proxied request has no such gap: its destination is the request line's, and
the `Host` header is ignored.

### Failing closed

By default the bridge falls open: a connection it cannot read is tunnelled, so the
agent's call still works. `tls_bridge.fail_closed: true` instead bridges or refuses
every connection the bridge intercepts, so policy judged on a host covers every byte
sent to it:

- An origin that fails verification is refused on the CONNECT: `502` with
  `X-Moca-Egress-Error: upstream_unverified`. The check is a `HEAD` bounded at 10s,
  so a timeout or reset is refused the same way; `error.message` says which. An
  origin that could not be dialled still gets a `200`, and each request is verified
  when sent.
- An SNI that names another host than the CONNECT target is refused (`sni-mismatch`,
  `421`). The comparison is textual, so CONNECT by name, not IP.
- A client that rejects Cortex's CA is refused every time, and nothing is
  remembered. Go tools on macOS need the CA in the keychain.
- Bytes that are not a TLS handshake are refused (`refused-non-tls`).

The default `passthrough_hosts` list is dropped; an explicit list is still
tunnelled. `listener.skip_hosts` destinations are verified too, so give a private-CA
origin `upstream_ca_bundle`. `fail_closed` cannot be combined with
`upstream_insecure`.

On the transparent listener, `passthrough_hosts` is judged by the dialled address,
so only IP entries apply. The sniffed SNI is the host, so `sni-mismatch` fires only
when the SNI could not be sniffed, such as a ClientHello over 8 KiB. Refusals there
are close rows, since there is no CONNECT to answer.

## Problems with one agent

- [Claude Code](agents/claude-code.md#known-issues)
- [Codex](agents/codex.md#known-issues)
- [OpenCode](agents/opencode.md#known-issues)
- [Pi](agents/pi.md#known-issues)
- [IBM Bob](agents/bob.md#what-it-knows-and-what-it-does-not)
- [Any other agent](agents/any-agent.md)
