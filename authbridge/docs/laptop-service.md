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

That last part matters — a restart cuts every attached Claude Code session, because
`HTTPS_PROXY` is fixed in each session's environment at startup and cannot fall back to a
direct connection. When a restart genuinely is needed, install says how many connections
it is about to cut.

To restart deliberately: `abctl service restart`.

## Why traffic disappears from `abctl`

Two limits, and neither is a clock:

| Limit | Default | Effect |
|---|---|---|
| `session.max_events` | 500 per session | oldest events drop, the session stays |
| `session.max_sessions` | 100 | whole sessions evicted, least-recently-used first |

Sessions do **not** expire on time. They used to, after 30 minutes idle, which read as
data loss — traffic vanished because you stepped away, not because anything overflowed.
Set `session.ttl` (e.g. `30m`) if you would rather raw prompts not sit in memory
indefinitely.

The store is in memory only, so a restart clears it regardless. Both `session.*` limits
need a restart to change — they are not hot-reloaded.

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

`stop` also reports how many connections it cut, because a Claude Code session that is
already running cannot recover on its own: `HTTPS_PROXY` is fixed in its environment
when it starts, so it has no way to fall back to a direct connection. Restart any
session that begins failing to connect.

## Three ways to turn it off

They are different, so pick deliberately:

### Pause it

```sh
abctl service stop
```

Claude Code fails while Cortex is stopped, because its settings still point at the
proxy. Either start Cortex again or unwire Claude Code (below).

**A Claude Code session that is already running cannot recover on its own.**
`HTTPS_PROXY` is fixed in its environment when it starts, so it has no way to fall
back to a direct connection, and `claude-code disable` cannot reach it. Restart any
session that starts failing to connect. `service stop` tells you how many
connections it cut, for exactly this reason.

Use `abctl service stop`, not `kill` or `pkill` — the supervisor restarts the
process within seconds, which looks like it refusing to die.

### Unwire Claude Code

```sh
abctl claude-code disable
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
the bridge certificate, so nothing downstream can read the traffic. The usual cause
is a process that **started before the CA existed**: CA files are read once at
startup, so an agent already running when Cortex was first installed — or when
`~/.cortex` was deleted and recreated — is holding a different CA, or none.

The proxy log names the offender and the cutoff:

```sh
grep 'client-rejected-ca' ~/.cortex/proxy.log
# ... client=127.0.0.1:58041 ca_not_before=2026-09-09T17:11:39-04:00
#     fix=restart clients that started before ca_not_before ...
```

Map that client port to a process, then restart it:

```sh
lsof -nP -iTCP:58041          # -> COMMAND / PID
ps -o lstart= -p <pid>        # started before ca_not_before? restart it
```

The port has to come from the log rather than a later `lsof` sweep: the connection
is gone by the time you look, so nothing after the fact can attribute it.

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
`abctl claude-code enable` prints this note when it runs on macOS.

Cortex keeps running; nothing sends traffic to it. `abctl claude-code enable` puts it
back.

### Remove it

```sh
abctl claude-code disable     # 1. unwire Claude Code
abctl service uninstall       # 2. stop it and remove the service
rm -rf ~/.cortex              # 3. config, CA, logs, abctl's UI settings
rm -f ~/.local/bin/abctl ~/.local/bin/authbridge-proxy
```

Order matters for the first two: `claude-code disable` needs to read the config that
step 3 deletes.

#### Check nothing is left

```sh
abctl claude-code status                    # should say "not enabled"
pgrep -fl authbridge-prox                   # should print nothing
ls ~/.cortex 2>/dev/null                    # should print nothing
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
machine you believe you have just cleaned. `abctl claude-code disable` removes all
seven in the right order, which is why it is step 1 above; this list is only for when
that binary is already gone.
