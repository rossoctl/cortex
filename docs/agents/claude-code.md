# Claude Code

Claude Code is a supported agent, and the one Cortex is built and tested against first.
Cortex recognises its User-Agent, groups its traffic by Claude Code's own session id,
parses its inference, and can prune the tools it never calls.
`agentop configure claude-code` routes it through Cortex and takes that out again.

## Enable and revert

```sh
agentop configure claude-code enable    # route every Claude Code session through Cortex
agentop configure claude-code status    # what is set
agentop configure claude-code disable   # take it out again
```

After `enable`, run `claude` the way you always do, with no flags and no environment
variables. Every session goes through Cortex until you `disable` it. The installer's
`--claude-code` flag runs the same `enable` as one of setup's steps.

**What `enable` writes.** Seven keys in the `env` block of `~/.claude/settings.json`.
Claude Code applies that block to every session on the machine, including background
agents, so nothing needs exporting in a shell. The addresses come from
`~/.cortex/config.yaml`, so they match the running proxy:

| Variable | Value |
|---|---|
| `HTTPS_PROXY` | `http://127.0.0.1:47600`, the forward proxy |
| `NODE_EXTRA_CA_CERTS` | `~/.cortex/ca/ca.crt`, the bridge CA |
| `SSL_CERT_FILE` | `~/.cortex/ca/bundle.crt`, the CA plus platform roots |
| `GIT_SSL_CAINFO` | `~/.cortex/ca/bundle.crt` |
| `REQUESTS_CA_BUNDLE` | `~/.cortex/ca/bundle.crt` |
| `CURL_CA_BUNDLE` | `~/.cortex/ca/bundle.crt` |
| `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC` | `1`, see [Known issues](#known-issues) |

Every other setting is left as it was, including any other `env` entry, such as
`ANTHROPIC_BASE_URL` or an auth token. `enable` refuses to overwrite a value someone
else set, such as a corporate proxy; the only values it replaces are Cortex's own, such
as a proxy at an older Cortex port. The first run copies the original file to
`settings.json.bak` and never overwrites that copy. `enable` and `disable` ask before
writing; `--yes` skips the question. `--settings` and `--config` point them at other
files.

**When it takes effect.** Claude Code applies its `env` block when it starts, so restart
any `claude` that is already running; `claude --resume` picks the conversation back up.
The same goes after `disable`. While enabled, Claude Code needs Cortex running,
because its requests go to the proxy: `disable` is the off switch, and `agentop
service status` says whether the proxy is up.

**The record.** The first `enable` records what each of the seven keys held before, in
`~/.cortex/claude-code-state.json`. `disable` removes only the keys `enable` added, and
puts back any value it recorded. `agentop uninstall` runs the same `disable`.

### Undoing it by hand

If `agentop` is already gone, delete the seven keys from the `env` block of
`~/.claude/settings.json` yourself, **before** deleting `~/.cortex`. The four that point
at `bundle.crt` replace their tools' trust stores, so with the file gone git, curl and
Python fail every TLS call, including ones that never touch Cortex.
[If `agentop` is already gone](../laptop-service.md#if-agentop-is-already-gone) has the
full procedure.

## CA trust

Claude Code reads `NODE_EXTRA_CA_CERTS`, which adds the bridge CA to its built-in roots,
so Claude Code itself needs nothing more.

The other four variables are for the tools Claude Code runs. They inherit
`HTTPS_PROXY`, so they must be able to verify the bridge too. Each of those variables
*replaces* its tool's trust store rather than adding to it, which is why they get
`bundle.crt` and not `ca.crt`: `ca.crt` alone would leave git, curl or Python trusting
one private CA and nothing else. On macOS, Go tools (`go`, `gh`) ignore `SSL_CERT_FILE`
and use the keychain; `enable` prints the `security add-trusted-cert` command for that,
and [Go tools on macOS](../laptop-service.md#go-tools-on-macos-need-the-keychain-not-a-variable)
explains it.

**The symptom when trust fails.** `agentop observe` shows Claude Code's requests as
`tunnel` rows with `client-rejected-ca`, and no plugin runs on them. Traffic still
flows; it just is not decrypted. The usual cause is a `claude` that started before the
CA existed: before the first install, or before `~/.cortex` was deleted and recreated.
Restart it.
[Everything shows as `tunnel`](../laptop-service.md#everything-shows-as-tunnel-and-no-plugin-ever-runs)
covers the other cause.

## What Cortex shows for it

**Its own agent.** Claude Code sends a User-Agent of the form
`claude-cli/<version> (external, cli)`. Cortex recognises it as `claude-code`, so Claude
Code gets its own row in agentop's agents pane.

**Its own sessions.** Every inference request carries `X-Claude-Code-Session-Id`, which
is stable for the life of a session and reused by `claude --resume`. It is the id Claude
Code names its transcript after (`~/.claude/projects/<dir>/<id>.jsonl`), so a session in
agentop is the session you see in Claude Code. Cortex reads this header in every
deployment by default.

**Titles.** `agentop observe` reads Claude Code's transcripts in the background and shows
each session's title and directory. `--skip-claude-metadata` turns that off; see
[Naming sessions from Claude Code](../../cmd/agentop/README.md#naming-sessions-from-claude-code---skip-claude-metadata).

**Subagents.** Claude Code sends a subagent's requests under the session id of the
conversation that started it, so they are filed in that session and counted in its cost.
Cortex tags each one as a subagent, from the marker Claude Code writes on the first line
of a subagent's system prompt, and agentop's context gauge uses the tag to follow the
main conversation rather than a subagent's. Listing subagents separately in agentop is
[#1047](https://github.com/rossoctl/cortex/issues/1047).

**Typed inference.** Requests to a path ending in `/v1/messages` are parsed as the
Anthropic Messages API, whoever serves it: Anthropic's own API or a gateway such as
LiteLLM. Cortex records the model, messages, tools, tool calls and finish reason, and
from the response's usage block the input, cache-read, cache-write, output and reasoning
tokens. Streamed responses are parsed the same way. Claude Code configured for Amazon
Bedrock or Google Vertex AI sends other paths, which are listed but not parsed.

**Cost.** A gateway's own reported cost is used when it sends one (LiteLLM's
`X-Litellm-Response-Cost`). Otherwise the tokens are priced at the bundled Anthropic
list rates, tier by tier. If your gateway charges less than list, pin its rate or the
figures are overstated; [Pricing](../pricing.md) shows how.

**Tool pruning is supported.** `agentop tools scan` reads Claude Code's transcripts,
proposes the tools you have not called, and writes them to the `tool-prune` plugin's
list. Claude Code is the only agent it can build a list for. See
[Cut token cost](../laptop-token-savings.md).

## Switching inference servers

With more than one LiteLLM server, `agentop server` — or `S` on agentop's agents
pane — chooses which one Claude Code's **new** sessions use, with one Claude Code
configuration and without restarting anything. A running conversation stays on
the server it started on, with the exceptions below.
See [Choosing an inference server](../../cmd/agentop/README.md#choosing-an-inference-server-agentop-server).

Claude Code needs no setting of its own for this beyond going through Cortex.
Its inference is captured wherever `ANTHROPIC_BASE_URL` points — Anthropic
itself, or a gateway — and sent to its server. An `https` base URL is captured
only when the TLS bridge decrypts it, which needs its port in `tls_bridge.ports`,
443 and 8443 unless set: on another port, such as `:4000`, the request stays an
opaque `CONNECT` tunnel and is not routed. An `http` base URL reaches the router
only if Claude Code sends it through the proxy, and `agentop configure claude-code
enable` sets `HTTPS_PROXY` only. The host Claude Code names must be up even for
sessions routed elsewhere: the proxy dials the `CONNECT` target before it sees the
request inside.

Claude Code's own model names go first, the conversation's, auto mode's
classifier's and the background calls' each as it chose them, so `/model` and
Claude Code's model settings keep working wherever the server serves those names.
A name the server refuses is sent again, before Claude Code sees the refusal, with
the server's own model: its `main` for a request with tools, the conversation, and
its `helper` for one without, such as the title. A GLM server behind LiteLLM, say,
is added with `agentop server add glm <url> --main glm --helper nemotron`.

The key is the server's: on a routed request the router puts the configured
server's key in the header Claude Code sent its own in, `X-Api-Key` for
`ANTHROPIC_API_KEY` and `Authorization` for `ANTHROPIC_AUTH_TOKEN`, so either can
stay as it is.

Claude Code keeps naming the model it asked for, so agentop is where the server
shows: the sessions table's `SERVER` column names each session's server, the
events table shows where each request went and, under METHOD, the model it was
sent for, and the detail pane's `redirected:` and `model:` lines name the host
and the model Claude Code asked for when they were others.

A session is pinned on its conversation's opening turn, so switch **before**
`/clear` or a new `claude`, not after. Claude Code's request says which turn it
is: an opening turn carries no assistant reply yet. A conversation Cortex did not
see begin — one already running when you first route Claude Code — is never moved:
if the session's history shows which server its conversation's last request
reached, Cortex keeps it there with that server's key, and otherwise it goes where
Claude Code sends it, unrouted. Claude Code's one-shot requests (the title, auto
mode's classifier) and its subagents' requests decide nothing; they follow the
session's pin once there is one. Pins are kept in `~/.cortex/plugin-state.json`,
so a proxy restart moves no conversation.

A conversation carried to a new session id is one Cortex did not see begin, so it
goes where Claude Code sends it, and one routed to another server moves back; see
[Known issues](#known-issues). That happens in three ways: Claude Code's
continued-in hand-off copies a live conversation to a new id; `claude daemon`
starts background jobs with `--session-id <new> --fork-session --resume <parent>`;
and `/clear` with background subagents running moves their traffic to the new id,
where it goes where Claude Code sends it until the new conversation's opening turn
pins the session.

Nothing fails over. When a session's server is down its requests fail, with a 502
from the proxy when the server cannot be reached; route the agent to another
server, and start a new session there.

**`/model` may list a server's own names.** If Claude Code asks the gateway for its
model list, that request is routed like the rest, so the picker can show, say,
`glm-5.3`. A name chosen there goes to the server as it is, like any other.

## Verified depth

Tested live on 2026-10-05 with Claude Code 2.1.286 on macOS 26.6.2 (arm64), Cortex built
from `main` at `1fd69b75`, through a LiteLLM gateway serving Claude Opus 5.5, Claude
Sonnet 5 and Claude Haiku 4.5. This was exercised:

- `enable --yes`, `status` and `disable --yes` against a scratch `HOME`: `enable` wrote
  the seven keys, `status` listed them, and `disable` left the file as it started.
  Deleting `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC` by hand in between made `status`
  report 6 of 7, and `disable` still cleaned up;
- a working session through Cortex, recorded under the `claude-code` agent and Claude
  Code's own session id, with its transcript title;
- all 125 successful responses in that session parsed, each with its model, its input,
  cache-read, cache-write, output and reasoning token counts, and a cost: 21 from the
  gateway's reported figure, 104 at the bundled rates;
- 29 subagent requests tagged as subagents and filed in their parent's session;
- `tool-prune` rewriting the requests, and the session reporting the cost avoided.

Not exercised: Linux; `install.sh --claude-code`; Claude Code talking to
`api.anthropic.com` directly rather than through a gateway; Amazon Bedrock and Google
Vertex AI.

Routing through `inference-router` was tested live on 2026-10-09 with Claude Code
2.1.295 (`claude --bare -p`, an API key and no `ANTHROPIC_BASE_URL`), through a scratch
Cortex: its requests to `api.anthropic.com` were captured and sent to the server. On a
GLM server `claude-opus-5-5` was refused once and sent again with the server's main
model, then sent with it from the start; on a server that serves Claude's names it went
as Claude Code asked. A file was written and read back on each.

## Known issues

- **Nonessential traffic is switched off.** `enable` sets
  `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`, which Claude Code documents as turning
  off its auto-updater, the `/bug` command, error reporting and telemetry. To keep them,
  delete that key from the `env` block after `enable`. `status` then reports 6 of 7 keys
  set, `disable` still works, and the next `enable` adds the key back
  ([#1205](https://github.com/rossoctl/cortex/issues/1205)).
- **`disable` discards hand-edits.** If you change one of the seven keys by hand after
  `enable`, `disable` still puts back the value from before `enable`, without a warning
  ([#1289](https://github.com/rossoctl/cortex/issues/1289)).
- **A conversation moved to a new session id leaves its server.** Cortex keeps a
  conversation on its server by its session id's pin, and Claude Code's
  continued-in hand-off and `claude daemon`'s forked background jobs carry a
  conversation to a new id, which no pin follows. Cortex then sees a conversation
  it did not see begin and sends it where Claude Code sends it, so one routed to
  another server moves back to the server `ANTHROPIC_BASE_URL` names,
  mid-conversation.
- **A continued conversation can show twice.** Claude Code sometimes moves a live
  conversation to a new session id. agentop then shows two rows with the same title, and
  the older row keeps the copied title. Cortex does not yet read Claude Code's record of
  the move.
