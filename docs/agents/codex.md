# Codex

[Codex](https://learn.chatgpt.com/docs/codex/cli) is a supported agent. Cortex recognises
its User-Agent and parses its inference. `agentop configure codex` routes it through Cortex
and takes that out again.

One fact shapes it: Codex loads a dotenv file — `$CODEX_HOME/.env` when `CODEX_HOME` is set,
otherwise `~/.codex/.env` — itself at process start,
confirmed against the real Codex CLI (writing a dead proxy address there and running bare
`codex exec`, with no `agentop` wrapper at all, broke both its websocket and its HTTPS
fallback transport). There is no settings JSON, so the routing goes into that file
directly. **Every Codex process reads it once, when it starts** — and that includes
Codex's own background server, which outlives the windows you type into; see
[Codex's background app-server](#codexs-background-app-server).

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
| `SSL_CERT_FILE` | `bundle.crt`, the bridge CA plus the platform roots |

**The CA goes in `SSL_CERT_FILE`, not Codex's own `CODEX_CA_CERTIFICATE`** — which its HTTP
client would otherwise prefer, and which an earlier version of this page recommended. Codex's
dotenv loader discards every key whose name starts with `CODEX_`, on purpose, so that a `.env`
cannot reconfigure Codex itself. A CA written under that name is one Codex never reads.
`SSL_CERT_FILE` survives the filter and is the next name Codex checks. If some other tool
already has its own `SSL_CERT_FILE` in this file, `enable` refuses rather than overwrite it.

That mistake is worth knowing about because **its symptom is success.** With no CA it can
read, Codex rejects the bridge's forged certificate, Cortex falls back to tunnelling the
bytes opaquely, and the request still completes — Codex answers normally and nothing
reports an error. What is lost is the observation: Cortex sees an opaque tunnel instead of
the request. So "did Codex reply?" cannot tell a working CA from a broken one. The check
that can is whether the bridge **terminated**: `~/.cortex/proxy.log` carries
`tls-bridge passthrough … reason=client-hung-up` lines for every connection it could not
terminate, and none when the CA is working.

`enable` scans the **whole file** for an existing definition of each of the five keys, not
only a block it wrote itself. Codex parses this file with
[dotenvy](https://github.com/allan2/dotenvy), so `KEY=VALUE`, `export KEY=VALUE`, an
indented line and spaces around the `=` all count as definitions and all are seen; a
commented `#KEY=VALUE` does not. A key defined on more than one line is refused outright
rather than guessed at — dotenvy resolves a duplicate **last-wins**, so appending would
quietly beat an earlier definition, including a corporate proxy someone set deliberately.
For the same reason `enable` refuses to overwrite any value that is not Cortex's own,
naming it and asking you to remove it by hand first. The
only values it replaces are Cortex's own, such as a proxy at an older Cortex address. It
refuses outright when the TLS bridge is disabled or has no `ca_dir`, because Codex would
then have no CA to trust — there is no partial "proxy only" state. The first run copies the
original file to `.env.bak` and never overwrites that copy; everything else in the file,
including blank lines and comments, is left exactly as it was. A short comment marks the
newly-added lines the first time any are appended.

**The record, and why it is always empty today.** The first `enable` records what the five
variables held in `~/.cortex/codex-state.json`. In practice that record contains nothing to
restore, and the reason is worth stating plainly rather than leaving as a surprise:
`enable` **refuses** whenever one of the five holds a value that is not Cortex's own, and
a value that *is* Cortex's own is recorded as absent (so `disable` removes it rather than
putting back a stale Cortex address). Those two rules are exact opposites, so every value
that reaches the record is stored as "was not set". The file, and `disable`'s restore
branch, exist to keep Codex's state shape identical to claude-code's and OpenCode's —
[#1289](https://github.com/rossoctl/cortex/issues/1289) is one pass over all three, and a
Codex-only design would have to be unpicked to get there.

**What `disable` does.** It removes the five variables, removes the
`# Added by agentop configure codex enable` marker if it is still exactly as written, and
deletes the record — so an `enable` / `disable` cycle is a round trip that leaves the file
as it found it. It would restore a recorded prior value if one existed, which per the
paragraph above does not happen today; if you hand-edit one of the five variables after
`enable`, `disable` simply removes it. The stricter "did this change since enable"
comparison is tracked by #1289.

**What `status` says.** It prints the five variables as the file currently holds them, a
warning if any is defined more than once, then `enabled` if every one matches what `enable`
would write right now, or `not fully enabled (N of 5 set)` otherwise. Last, whether Codex's
running background app-server uses Cortex — see below. It changes nothing.

`enable` and `disable` both show what they will change and ask before doing it. `--yes`
skips the question. If you decline, or there is no terminal to ask on, they change nothing
and exit 3. `--env PATH` points at a different dotenv file; `--config PATH` reads a
different Cortex config.

### Codex's background app-server

**The file alone does not reach a Codex that is already running.** Codex runs a
long-lived background server, its app-server daemon (`codex app-server --managed-daemon`),
and the `codex` you type into is only a screen that hands it each prompt: the daemon makes
the model calls. It reads the dotenv file when it starts and never again, and it is shared
by every Codex window, so it is often days old. A daemon started before `enable` keeps
calling OpenAI directly — the window's own startup checks and telemetry reach Cortex, its
prompts do not, and agentop shows Codex only as a small `pending:codex@…` bucket under
**codex**, with no tokens. `disable` has the mirror problem: the daemon keeps routing
through Cortex, and Codex stops working if Cortex is then stopped.

**So `enable` and `disable` restart a running daemon**, once, after writing the file, with
Codex's own `codex app-server daemon restart`. They say so before asking, naming its pid and
when it started, and `--yes` skips the question but not that line. Then they check the new
daemon's environment and say whether it uses Cortex — or, if its proxy is still not what the
file says, where else it could be coming from. With no daemon running there is nothing to
restart: the next `codex` starts one that reads the new file. An `enable` that finds nothing
to change does not restart; it says whether the running daemon uses Cortex, and how to
restart it if not.

**The auto-updater is restarted too.** An auto-updatable install runs a second background
process, `codex app-server daemon pid-update-loop`, which also reads the dotenv file only when
it starts. After installing an update it starts a new daemon, and that daemon inherits the
updater's environment, but `codex app-server daemon restart` leaves a running updater alone.
So `enable` and `disable` stop the updater first, and the restart starts a fresh one that has
read the new file. Comparing `ps` before and after, both processes change. Without this, a
`disable` came undone at the next Codex update, with the daemon back on Cortex's proxy. If the
updater will not stop, they name it and the command that replaces it.

**What a restart does to Codex sessions**, from Codex's source (0.163.0 and 0.164.0), not
measured here: the old daemon stops taking new turns, gives running ones its shutdown grace — 60
seconds by default, configurable up to 5 minutes — to finish, and saves its threads. The new daemon restores them and
continues a local turn the grace cut off, saying "Resuming interrupted work". An open
`codex` window shows that it is reconnecting and rejoins its thread on its own. So a restart
interrupts Codex rather than ending it, and `enable` or `disable` can take as long as that grace
when a turn is running.

If the restart fails, the file is still changed and the command still exits 0; it prints the
error and the command to run yourself:

```sh
codex app-server daemon restart
```

To check the daemon by hand, its connections should be to `127.0.0.1:47600` rather than to
OpenAI:

```sh
lsof -nP -a -p "$(pgrep -f 'codex app-server --listen')" -iTCP -sTCP:ESTABLISHED
```

### Undoing it by hand

Without `agentop`: open `$CODEX_HOME/.env` (or `~/.codex/.env`) and remove the lines for the five variables above,
if they point at Cortex (a proxy on `localhost:476…`/`127.0.0.1:476…`, or a CA path under
`~/.cortex/ca` unless you moved its `ca_dir`). Leave any that point elsewhere — they are not
Cortex's. Then delete the record, and restart Codex's background server so it stops using
the proxy.

```sh
# Remove only lines matching these five keys, and only if they point at Cortex:
#   HTTP_PROXY  HTTPS_PROXY  http_proxy  https_proxy  SSL_CERT_FILE
$EDITOR ~/.codex/.env
rm -f ~/.cortex/codex-state.json
codex app-server daemon restart
```

## CA trust

Cortex's TLS bridge decrypts Codex's HTTPS using certificates it forges from its own CA, so
Codex has to trust that CA. Codex's HTTP client (`codex_http_client::custom_ca`) checks
`CODEX_CA_CERTIFICATE` first, then `SSL_CERT_FILE`, then falls back to the platform's own
root store with neither set — but it only ever sees the **process environment**, and
Codex's dotenv loader strips every `CODEX_`-prefixed key before that. Hence
`SSL_CERT_FILE`; see [Enable and revert](#enable-and-revert) above. Unlike the bundle
variables claude-code's and OpenCode's integrations set, Codex's custom CA is **additive** —
it is loaded on top of the platform roots rather than replacing them. `bundle.crt` (the
bridge CA plus the platform roots) is still the right value, it just is not load-bearing
for reaching hosts the bridge does not terminate.

**Verified live** with the inherited CA variables stripped from the environment
(`env -u SSL_CERT_FILE -u CURL_CA_BUNDLE -u REQUESTS_CA_BUNDLE -u GIT_SSL_CAINFO`), so that
the `.env` file was the only possible source of trust, and judged on whether the bridge
**terminated** rather than on whether Codex replied:

| `.env` holds | Bridge outcome |
|---|---|
| `SSL_CERT_FILE` | **terminates** — no `tls-bridge passthrough` lines |
| `CODEX_CA_CERTIFICATE` | passthrough; Codex rejected the forged cert |
| no CA variable at all | passthrough — *identical to the row above* |

That last pair is the whole reason this page labours the point: the `CODEX_`-prefixed name
is indistinguishable from writing nothing, and both still answer the prompt.

⚠️ **If you have already trusted the bridge CA in your OS keychain, or exported
`SSL_CERT_FILE` in your shell, Codex will work regardless of what this file says** — and a
test of this feature done in that environment proves nothing about the file. That is exactly
how the `CODEX_CA_CERTIFICATE` mistake survived its first review.

## What Cortex shows for it

**Its own agent.** Codex sends four product tokens, all folded to the canonical agent name
`codex`, so its traffic gets its own row in agentop's agents pane instead of sharing
`Other`:

| User-Agent | Sent on |
|---|---|
| `codex_exec/<version> (<os>; <arch>) <term>` — sometimes with a trailing `(codex_exec; <version>)` comment | its inference (`/backend-api/codex/responses`) and most other backend calls |
| `codex-mcp-client/<version>` | its MCP client (`/backend-api/ps/mcp`) |
| `codex-tui/<version> (<os>; <arch>) <term>` — sometimes with a trailing `(codex-tui; <version>)` comment | the interactive TUI — i.e. plain `codex` — including its inference |
| `codex_cli_rs/<version> (<os>; <arch>) <term>` | anything sent before its app-server finishes initializing |

`<term>` is the `TERM` environment variable — the terminal type. It reads `unknown` under
`codex exec`, which runs without a TTY, and the terminal's own name under the interactive
TUI (`xterm-256color` on the captures these rows come from). Nothing reads this field:
Cortex matches on the product token before the first slash, which is why a varying `TERM`
does not split one agent into several rows.

The version is the field after the first slash in all four. The MCP client folds to the same
name deliberately: it is one agent's component, not a second agent. **Codex's telemetry
exporter is not claimed** — it sends `OTel-OTLP-Exporter-Rust/<version>` to
`ab.chatgpt.com`, which names a generic Rust crate rather than Codex, so it stays
unrecognised and shows under its raw label. Captured from Codex 0.160.1.

**It groups into named sessions.** Codex sends its session id in a header named
`Session-Id` — the bare name, not the `X-Session-Id` that OpenCode uses — and a laptop
install reads it. Captured on `codex_exec/0.160.1`: a UUID, constant across all nine
inference requests of one multi-turn session and different in the next.

**Which requests carry it differs between the two front-ends**, and it is worth knowing
because it decides how many rows a run leaves:

| | Carries `Session-Id` | Rows per run |
|---|---|---|
| `codex exec` | its **inference** requests; the plugin-list and MCP chatter does not | **one** in agentop — the conversation under its real id. The chatter that arrived before anything named the session (15 events against 113, measured) is recorded in a `pending:codex@<pid>.<start>` bucket, which is linked to the session and shown inside it |
| the TUI (plain `codex`) | **also its chatter** — a TUI session was named with *zero* inference requests in it | **one** — everything lands in the named session, no pending bucket |

Either way the tokens and cost are in the named session. Note that once a session is
named, `client_affinity` files header-less requests from the same agent into it too, so a
row's presence there is not by itself proof that *that* request carried the header. `Session-Id` is read **last** in the
header precedence order, after the three dedicated names, because it is the least
namespaced of the four — and for the same reason it is read **only on a loopback-only
install**, exactly as `X-Session-Id` is. In a cluster an agent may send a bare
`Session-Id` for its own purposes, and filing its calls by it would take them away from
the session holding its user's intent.

⚠️ **An existing `~/.cortex/config.yaml` may need one line added.** The laptop config
writes `session.id_headers` out explicitly, and an explicit list is used exactly as
written rather than merged with the built-in default — so a Cortex installed before this
shipped keeps the old three names and will not read Codex's. Check with
`grep -A5 id_headers ~/.cortex/config.yaml`: if the list is there and has no
`"Session-Id"`, add it as the last entry, or delete the whole `id_headers:` block to fall
back to the current default. A config that never had the block is already fine.

Before this, Codex's traffic landed in a per-process bucket (`pending:codex@<pid>.<start>`)
and stayed there — one bucket per process rather than per conversation, so two
conversations in one long-lived process merged and one spanning a restart split in two.

**Typed inference.** Codex speaks OpenAI's Responses API, which `inference-parser` reads
(model, messages, tools, completion, token counts) on any path ending in `/responses` —
`/backend-api/codex/responses` for the ChatGPT backend, `/v1/responses` for the public API.
Two things about that traffic are unlike the other dialects, and both are handled: the
request body arrives `Content-Encoding: zstd`, and the response is a real SSE stream
mislabelled `Content-Type: application/json`. Codex's tool calls arrive as
`custom_tool_call` items whose arguments are **freeform text, not JSON** — its `exec` tool
is one — so a consumer that parses `ToolCalls[].Arguments` as JSON must tolerate failure
for this dialect.

**HTTPS, not WebSocket.** Codex prefers a WebSocket to the same `/responses` path, and
Cortex cannot carry one: `Upgrade` is a hop-by-hop header a proxy must not forward, and
the parser reads HTTP requests, not frames. So the forward proxy answers that handshake
itself with **426 Upgrade Required**, which Codex reads as "this endpoint wants HTTP" and
switches to HTTPS straight away, for the rest of its session. That covers both of
Codex's endpoints — `chatgpt.com` on a ChatGPT login and `api.openai.com` on an API key
— and no other host, since what a 426 means is only known for Codex against OpenAI. It
is not counted as an error in `/v1/usage`. The attempt shows on the
timeline as a `GET` on the Responses path answered 426 with `error.kind:
"upgrade_declined"`, and Codex prints one line for it:

```
ERROR codex_api::endpoint::responses_websocket: failed to connect to websocket:
      HTTP error: 426 Upgrade Required, url: wss://chatgpt.com/backend-api/codex/responses
```

That line is expected and harmless; the turn continues over HTTPS. Before the 426, the
proxy forwarded the stripped `GET` and chatgpt.com answered **405**, which Codex treats
as a transient failure: it retried five times on a doubling backoff, printing
`Reconnecting… n/5`, before falling back. Measured on Codex 0.163.0, a one-word
`codex exec` turn took a median **11.0s** that way and **3.0s** with the 426. There is no
supported Codex setting to skip the WebSocket attempt: the built-in `openai` provider
hardcodes `supports_websockets = true` and Codex refuses a config that overrides a
built-in provider.

The 426 is Cortex's setting, not Codex's, and it is on by default, so there is nothing to
configure. To turn it off — which brings back the 405 and its retries — add the key to the
`listener:` block that is already in `~/.cortex/config.yaml`:

```yaml
listener:
  # ...the lines already here...
  decline_responses_websockets: false
```

and restart the proxy with `agentop service restart`. Unlike most of that file, a
`listener:` edit is not picked up live, and the restart briefly disconnects every agent
routed through Cortex.

**Tokens always; cost on a laptop once the price list downloads, or wherever you
configure a rate.** Token counts come from the Responses
API's usage block and aggregate alongside other agents (verified: a one-turn `codex exec`
reported 13,260 tokens under the `codex` series of `GET /v1/usage?group=agent`).

**Cost on a laptop install: usually yes, and from a download rather than from the
binary.** The bundled table carries first-party OpenAI list rates, so a GPT model prices
the same way a Claude one does — but the *shipped* table is pinned to a LiteLLM commit,
and a model added upstream after that commit is not in it. `gpt-6-luna` is one such, so
the compiled-in table alone does not price it. What closes the gap is the price-list
download: a local install fetches LiteLLM `main` at startup and hourly through the same
filter that generated the table, keeps it in `~/.cortex/price-list.json`, and today's
upstream prices `gpt-6-luna` at $0.10 in / $0.50 out per million. So cost appears without
configuring anything, once that first download lands.

**On a sidecar in a cluster, no.** Sidecars make no such call and keep the compiled-in
table, so a model newer than the pin reports *priceable but unpriced* there — named,
never under-priced. See
[Finding traffic that is not priced](../pricing.md#finding-traffic-that-is-not-priced).

Pin the rate yourself when you want it independent of a download — in a sidecar, with
`bundled: false`, or to assert a figure rather than inherit one. These are the first-party
OpenAI list prices for the model Codex uses, and a configured entry outranks both the
shipped table and the download:

```yaml
# ~/.cortex/config.yaml
pricing:
  endpoints:
    - hosts: ["chatgpt.com"]
      models:
        "gpt-6-luna":
          input_cost_per_million:       0.10
          cache_write_cost_per_million: 0.125
          cache_read_cost_per_million:  0.01
          output_cost_per_million:      0.50
```

⚠️ **Read the resulting number as an estimate, not a bill.** Those are **per-token API
list prices**, while Codex on a ChatGPT subscription bills a flat monthly fee — so the
figure is "what this would have cost via the API", which is useful for comparison and
wrong if read as spend. The same caveat applies to Claude Code on a Pro/Max subscription,
which Cortex prices today without saying so; this is a subscription-vs-API distinction
Cortex does not currently model for any agent, not something peculiar to Codex. Check the
model name against `agentop pricing` or `unpricedBy` before copying the block — Codex
selects its own model and it changes between releases (`gpt-6.1-sol` has also been
observed).

**Tool pruning is unsupported**, for three independent reasons, any one of which is
sufficient:

1. `agentop tools scan` builds the `remove` list from Claude Code's transcripts
   (`~/.claude/projects/*.jsonl`) and proposes only Claude Code's hardcoded built-in tool
   names, so nothing builds a Codex list. `--dir PATH` repoints the scan, but the parser
   still expects Claude Code's `tool_use` JSONL schema and the built-in filter is
   unconditional.
2. `tool-prune`'s default paths are `/v1/chat/completions`, `/v1/completions` and
   `/v1/messages`, matched by suffix. Codex's `/responses` is not among them, so the plugin
   records `skip` / `path_not_inference` and never runs on its inference.
3. Even with `paths` overridden to include `/responses`, `tool-prune` reads the manifest
   from a **top-level** `tools` array, while Codex nests its manifest three levels down
   under `input[].tools[].tools[]`. The plugin would skip with `no_tool_manifest`.

Worth knowing for anyone changing this: `tool-prune` and `inference-parser` keep
**separate** suffix lists, and only the parser's recognises `/responses`. The drift is
silent — the parser reads a Responses request that the pruner skips as "not inference".

## Verified depth

Tested live with Codex 0.160.1 on macOS 26.3 (arm64), against the real ChatGPT backend,
with Cortex built from the change that added this page. This was exercised:

- `enable` on a file with unrelated content, preserving a comment, an API key line and
  another tool's variable; `status` reporting `enabled`; `disable` restoring the file and
  removing the record;
- a bare `codex exec` — no `agentop exec` wrapper, no shell export — routing through
  Cortex and returning a normal response, confirming both the proxy routing and CA trust
  come from the dotenv file alone — re-run with the ambient CA variables stripped, and
  judged on the bridge terminating rather than on Codex replying (see CA trust);
- **each of the four proxy-variable spellings alone**, with the other three absent: all
  four independently routed Codex's traffic, which is why `enable` writes all four;
- agent recognition for every User-Agent above, including the TUI's — captured from a
  real interactive session driven through the proxy over a pty, not inferred from
  Codex's source — and including the telemetry exporter staying unrecognised;
- the `codex` series in `GET /v1/usage?group=agent`, and the model showing as priceable
  but unpriced;
- **on Codex 0.163.0**, the WebSocket decline: eight `codex exec` turns each made one
  handshake attempt, answered 426, then completed over HTTPS with the model, tokens and a
  cost from the downloaded price list — against seven 405'd attempts and a fallback
  warning per turn without it. A TUI turn, driven over a pty, did the same: one 426, then
  its inference parsed over HTTPS under `codex-tui`.

Not tested:

- **Linux.** Everything here was exercised on macOS only.
- **Codex versions other than 0.160.1 and 0.162.1.** Both appeared in one capture (an
  auto-update landed mid-session) and both parse identically, which is some evidence the
  User-Agent shape is stable across releases rather than pinned to one build — but the
  `.env` loading behaviour and the `CODEX_` filter were only read from 0.160.1's source.
- **The public `/v1/responses` API** (an OpenAI API key rather than a ChatGPT
  subscription). The parser recognises the path, and the non-streaming shape of that
  dialect is modelled from the published schema rather than from captured traffic.
- **A `function_call` tool call.** Codex's own `exec` tool is a `custom_tool_call`; the
  `function_call` shape is modelled from the documented schema.
- **Tool pruning**, which is unsupported above rather than merely untested.

## Known issues

- **Changing the routing restarts Codex's background server.** `enable` and `disable`
  change a file Codex's app-server daemon and auto-updater read only when they start, so
  they restart both when they are running, which interrupts running Codex turns for up to
  the daemon's shutdown grace (a minute by default). Changing
  the file any other way — by hand, or with an agentop older than this — does not reach a
  running daemon until `codex app-server daemon restart`. See
  [Codex's background app-server](#codexs-background-app-server).
- **Cost is blank where no list price reaches the model.** Two cases, and neither is
  Codex-specific: a **sidecar** keeps the compiled-in table and makes no download, so a
  model newer than the pinned commit has no rate there; and `bundled: false` turns the
  shipped table *and* the download off together. On a laptop with defaults the rate arrives
  with the first hourly fetch. See
  [What Cortex shows for it](#what-cortex-shows-for-it).
- **Chatty non-inference traffic.** A single one-turn `codex exec` produced ~100 recorded
  events, most of them not inference: repeated polling of
  `/backend-api/ps/plugins/list` and friends, plus an MCP handshake. The inference is a
  small fraction of a Codex session's rows.
- **Tool calls carry non-JSON arguments.** `custom_tool_call` arguments are freeform text,
  and the two plugins that forward them differ in what that costs. `sparc` sends the value
  on to its reflection service, whose own parsing falls back to the raw string on a
  `JSONDecodeError`, so it is tolerated. `opa` puts the value into
  `input.inference.tool_calls[_].arguments` as a plain string, which is fine until a
  policy calls `json.unmarshal` on it — that is the author's to handle, and nothing in
  Cortex handles it for them.
- **`CODEX_HOME` has to be set where `agentop` runs, not only where Codex runs.** agentop
  reads it from its own process environment, the same way Codex does, so the two agree
  whenever both are launched from the same shell — the normal case. They come apart if you
  set it only for Codex, say in a wrapper script: agentop then writes `~/.codex/.env`,
  Codex reads `$CODEX_HOME/.env`, and `status` reports `enabled` because it is reading the
  same wrong file `enable` wrote. Nothing can detect that from outside — there is no signal
  distinguishing "`CODEX_HOME` is not in use" from "it is in use somewhere agentop cannot
  see". Pass `--env $CODEX_HOME/.env` explicitly if your setup is shaped that way.
- **Rows recorded before this release** keep the raw User-Agent as their label and show
  under `Other` until they age out.
