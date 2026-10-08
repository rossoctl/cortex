# Codex

[Codex](https://learn.chatgpt.com/docs/codex/cli) is a supported agent. `agentop configure
codex` routes it through Cortex and takes that out again.

One fact shapes it: Codex loads a dotenv file, `~/.codex/.env`, itself at process start —
confirmed against the real Codex CLI (writing a dead proxy address there and running bare
`codex exec`, with no `agentop` wrapper at all, broke both its websocket and its HTTPS
fallback transport). There is no settings JSON and no background service, so the routing
goes into that file directly.

This page covers enabling, reverting and CA trust only. What Cortex parses from Codex's
traffic, how far it was verified, and its known issues are not written up yet — see
[#942](https://github.com/rossoctl/cortex/issues/942).

## Enable and revert

```sh
agentop configure codex enable    # route Codex through Cortex
agentop configure codex status    # what is set
agentop configure codex disable   # take it out again
```

**What `enable` writes.** Five variables, read from `~/.cortex/config.yaml` so they always
match the running proxy:

| Variable | Value |
|---|---|
| `HTTP_PROXY` `HTTPS_PROXY` `http_proxy` `https_proxy` | Cortex's forward proxy URL |
| `CODEX_CA_CERTIFICATE` | `bundle.crt`, the bridge CA plus the platform roots |

`CODEX_CA_CERTIFICATE` is Codex's own dedicated CA override — confirmed by inspecting the
real `codex` binary, whose HTTP client checks it before falling back to the more generic
`SSL_CERT_FILE`, and before system roots when neither is set. Using the dedicated name
means `enable` never has to share a key with some other tool's unrelated `SSL_CERT_FILE`
value in the same file.

`enable` scans the **whole file** for an existing definition of each of the five keys, not
only a block it wrote itself — a plain `KEY=VALUE` line anywhere counts, a commented
`#KEY=VALUE` or an `export KEY=VALUE` line does not, matching Codex's own dotenv loading.
A key defined on more than one line is refused outright rather than guessed at: dotenv-loader
precedence for a duplicate is undocumented, and a write that is silently ignored by the
loader is worse than a refusal. `enable` also refuses to overwrite a value someone else
set, such as a corporate proxy, naming it and asking you to remove it by hand first. The
only values it replaces are Cortex's own, such as a proxy at an older Cortex address. It
refuses outright when the TLS bridge is disabled or has no `ca_dir`, because Codex would
then have no CA to trust — there is no partial "proxy only" state. The first run copies the
original file to `.env.bak` and never overwrites that copy; everything else in the file,
including blank lines and comments, is left exactly as it was. A short comment marks the
newly-added lines the first time any are appended.

**The record.** The first `enable` records what the five variables held in
`~/.cortex/codex-state.json`. A value `enable` replaced because it was already Cortex's
(shape-matched, the same heuristic claude-code's and OpenCode's integrations use) is
recorded as absent, not as something to restore — so `disable` removes it rather than
putting back a stale Cortex address.

**What `disable` does.** It restores each of the five variables to what the record says it
held before `enable` — unset if it was absent, or back to a recorded value otherwise —
then deletes the record. Unlike `status`, it does not re-check whether the live value has
changed since `enable` ran: if you hand-edited one of the five variables afterwards,
`disable` overwrites that edit on the way back to the recorded value. This is the same
simple, one-shot restore claude-code's `disable` makes today, not the stricter
"did this change since enable" comparison tracked by
[#1289](https://github.com/rossoctl/cortex/issues/1289) for a future pass across all three
integrations at once.

**What `status` says.** It prints the five variables as the file currently holds them, a
warning if any is defined more than once, then `enabled` if every one matches what `enable`
would write right now, or `not fully enabled (N of 5 set)` otherwise. It changes nothing.

`enable` and `disable` both show what they will change and ask before doing it. `--yes`
skips the question. If you decline, or there is no terminal to ask on, they change nothing
and exit 3. `--env PATH` points at a different dotenv file; `--config PATH` reads a
different Cortex config.

### Undoing it by hand

Without `agentop`: open `~/.codex/.env` and remove the lines for the five variables above,
if they point at Cortex (a proxy on `localhost:476…`/`127.0.0.1:476…`, or a CA path under
`~/.cortex/ca` unless you moved its `ca_dir`). Leave any that point elsewhere — they are not
Cortex's. Then delete the record.

```sh
# Remove only lines matching these five keys, and only if they point at Cortex:
#   HTTP_PROXY  HTTPS_PROXY  http_proxy  https_proxy  CODEX_CA_CERTIFICATE
$EDITOR ~/.codex/.env
rm -f ~/.cortex/codex-state.json
```

## CA trust

Cortex's TLS bridge decrypts Codex's HTTPS using certificates it forges from its own CA, so
Codex has to trust that CA. Confirmed by inspecting the real `codex` binary: its HTTP
client (`codex_http_client::custom_ca`) checks `CODEX_CA_CERTIFICATE` first, then
`SSL_CERT_FILE`, then falls back to the system's own root store with neither set — a
"replace the trust store" variable, the same category as the bundle variables
claude-code's and OpenCode's integrations set, not `NODE_EXTRA_CA_CERTS`'s additive one. So
it gets `bundle.crt` (the bridge CA plus the platform roots), not the bare CA alone — naming
the bare CA would leave Codex trusting Cortex and nothing else, breaking every host the
bridge does not terminate.

Verified live: with `~/.codex/.env` holding only the five variables above (no `agentop exec`
wrapper, no shell export), a bare `codex exec` reached the real backend over Cortex's proxy
and returned a normal response, with no certificate error from either Codex's websocket
attempt or its HTTPS fallback.
