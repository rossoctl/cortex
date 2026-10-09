# OpenCode

[OpenCode](https://opencode.ai/) is a supported agent. Cortex recognises its User-Agent,
groups its traffic by OpenCode's session id and parses its inference.
`agentop configure opencode` routes it through Cortex and takes that out again.

One fact shapes all of it: OpenCode's traffic does not leave from the `opencode` you run.
The first client starts one background service per user, `opencode serve --service`,
and that service sends all of OpenCode's traffic, for every session. Later clients reuse
it, and it outlives them. It keeps the environment it started with. So proxy variables
in your shell, or `agentop exec -- opencode`, reach it only when that command is the one
that starts it.

## Enable and revert

```sh
agentop configure opencode enable    # route OpenCode's service through Cortex
agentop configure opencode status    # what is set, and what the running service uses
agentop configure opencode disable   # take it out again
```

**What `enable` writes.** The service keeps an environment of its own, the `env` block
of OpenCode's `service.json`. That file is `$XDG_CONFIG_HOME/opencode/service.json`, or
`~/.config/opencode/service.json` when `XDG_CONFIG_HOME` is unset. When the service
starts, those values take precedence over the environment it inherited, so the routing
goes there and not into a shell profile. `enable` sets nine variables in that block,
with one `opencode service set env NAME VALUE` call per variable that differs, the CA
variables before the proxy. agentop never edits the file itself. These are the nine
variables `agentop exec` sets, chosen for the reasons
[its section](../../cmd/agentop/README.md#running-one-command-through-cortex-agentop-exec)
gives. The addresses come from `~/.cortex/config.yaml`:

| Variable | Value |
|---|---|
| `HTTPS_PROXY` `HTTP_PROXY` `https_proxy` `http_proxy` | Cortex's forward proxy URL |
| `NODE_EXTRA_CA_CERTS` | `ca.crt`, the bridge CA |
| `SSL_CERT_FILE` `GIT_SSL_CAINFO` `REQUESTS_CA_BUNDLE` `CURL_CA_BUNDLE` | `bundle.crt`, the bridge CA plus the platform roots |

`enable` refuses to run when the TLS bridge is disabled or has no `ca_dir`, because
OpenCode would then have no CA to trust. It also refuses to overwrite a value someone
else set, such as a corporate proxy, and prints the `opencode service unset env` command
that removes it. The only values it replaces are Cortex's own, such as a proxy at an
older Cortex address. On macOS it also prints that Go tools (`go`, `gh`) use the
keychain, not `SSL_CERT_FILE`; see
[Go tools on macOS](../laptop-service.md#go-tools-on-macos-need-the-keychain-not-a-variable).

**The record.** The first `enable` records what the nine variables held in
`~/.cortex/opencode-state.json`. Any value `enable` replaced was Cortex's, so the record
stores it as absent: putting an old Cortex address back would leave OpenCode on Cortex
after `disable`. As a result, the record normally holds nothing to restore.

**What `disable` does.** It changes only Cortex's values, unsetting each one or
restoring a value found in the record. A value that is not Cortex's, even one set after
`enable`, stays in place, and `disable` names it (`<KEY> left as "<v>": it is not a
value Cortex set.`) along with the command that removes it. `disable` does not need
Cortex's config. Without it, `disable` recognises Cortex's values by their shape: a
proxy on `localhost:476…` or `127.0.0.1:476…`, or a CA path under `.cortex/`. Run it
before you [remove Cortex](../laptop-service.md#remove-it). A service started with these
variables sends its requests to Cortex's address whether or not anything is listening
there.

**What `status` says.** It prints the nine variables, then `enabled` if every one holds
exactly the value `enable` writes, or `not fully enabled (N of 9 set)` if not. A proxy
written another way, such as `localhost` instead of `127.0.0.1`, counts as not set here.
Then comes one line about the running service. It answers two questions: is the proxy
the service uses now Cortex's, and would the proxy its service environment gives it on a
restart be Cortex's? The line is one of:

- it is using Cortex;
- it is running with its old environment, and `opencode service restart` would put it
  on Cortex;
- it is using Cortex, but its service environment does not route it there, so its next
  start will not use Cortex. A service started under `agentop exec` looks like this, and
  `enable` keeps it on Cortex, restarting it to do so;
- it is not using Cortex;
- it is not running;
- it could not check, because the service's process or environment cannot be read or
  `opencode service status` failed.

If the Cortex config cannot be read, `status` prints the variables and the service's pid
and proxy, with no verdict. `status` changes nothing.

**Changing the environment restarts the service.** OpenCode's CLI stops a running
service on every change to its environment. This was seen on OpenCode 2.0.21, for both
`set env` and `unset env`; the CLI stops only the service its own config started, which
is yours. An open OpenCode window can start the service again within a second, before
the change lands, and then it comes back with the environment it had. That is why
`enable` and `disable` restart it themselves:

- When there is something to change, they say first that the service is running and
  that the change restarts it, with its pid when it can be found, and then ask. When
  `opencode service status` fails, they say only that the change stops the service if it
  is running.
- If the service was running, they restart it once, after the last change, with
  `opencode service restart`, which starts it with its service environment. The restart
  interrupts every OpenCode session using the service, and an open OpenCode reconnects
  to it. Then they say whether the restarted service is using Cortex (after `enable`) or
  no longer uses it (after `disable`).
- If the restart fails, they say so: the service may be stopped, or running with its old
  environment, and `opencode service restart` applies the change. The change stands,
  and they exit 0.
- If a change fails part way, they do not restart the service. They say it may be
  stopped, and running the command again finishes the change.
- A service that was not running is left alone, and it starts with the new environment
  the next time you run OpenCode. When `opencode service status` fails, they cannot tell
  whether it was running, so they do not restart it; if it still cannot be checked
  afterwards, they say to restart it to be sure.

`status` changes nothing, so it never stops or restarts the service.

`enable` and `disable` both show what they will change and ask before doing it. `--yes`
skips the question, but not the line about the service. If you decline, or there is no
terminal to ask on, they change nothing and exit 3. `--config PATH` reads a different
Cortex config. `--opencode BIN` names the CLI when it is neither on `PATH` nor in
`~/.opencode/bin`.

**`agentop exec -- opencode` instead.** This routes OpenCode only when it is the command
that starts the service. The service then keeps `exec`'s environment, so later clients
that reuse it go through Cortex too, until the service restarts. When the service is
stopped, a proxy already set in `service.json` beats `exec`'s environment, because the
service it starts takes those values over the ones it inherits. A service that is already
running without Cortex is not changed, and `exec` warns that it is not using Cortex. The
warning names one of two fixes:

- When the service environment already holds every value `exec` sets, only the running
  service predates it, and restarting it is enough: `opencode service restart`.
- Otherwise, including when that environment cannot be read, it names `enable`, which
  restarts the service.

Either restart interrupts every OpenCode session using the service. When the service is
running but its process or its environment cannot be read, `exec` prints a note that it
could not check instead. The command runs in every case. Three kinds of command get no
check: `opencode service …` commands, `opencode serve`, which is a foreground server that
runs under `exec`'s own environment, and `opencode` run through a wrapper such as
`env opencode`.

### Undoing it by hand

Without agentop: list what the service environment holds, then unset only those of the
nine that point at Cortex: a proxy on Cortex's address, or one of Cortex's CA files
(under `~/.cortex/ca` unless you moved its `ca_dir`). Leave any that point elsewhere:
they are not Cortex's. Then delete the record. The first `unset env` stops a running
service, and an open OpenCode may start it again with the old environment, so restart it
once you are done; that interrupts every OpenCode session using it.

```sh
opencode service get env
# Only for those of the nine that point at Cortex:
opencode service unset env HTTPS_PROXY
opencode service unset env HTTP_PROXY
opencode service unset env https_proxy
opencode service unset env http_proxy
opencode service unset env NODE_EXTRA_CA_CERTS
opencode service unset env SSL_CERT_FILE
opencode service unset env GIT_SSL_CAINFO
opencode service unset env REQUESTS_CA_BUNDLE
opencode service unset env CURL_CA_BUNDLE
opencode service restart   # if the service was running
rm -f ~/.cortex/opencode-state.json
```

## CA trust

Cortex's TLS bridge decrypts OpenCode's HTTPS using certificates it forges from its own
CA, so the service has to trust that CA. This was tested on OpenCode 2.0.21 on macOS,
with each variable set only in the service environment. `NODE_EXTRA_CA_CERTS` alone was
enough, and so was `SSL_CERT_FILE` alone: with either one, OpenCode Zen's inference was
decrypted and recorded. `enable` sets both. `NODE_EXTRA_CA_CERTS` adds `ca.crt` to the
runtime's own roots. The bundle variables replace a tool's roots instead of adding to
them, so they get `bundle.crt` (the CA plus the platform roots) rather than `ca.crt`
alone. `GIT_SSL_CAINFO`, `REQUESTS_CA_BUNDLE` and `CURL_CA_BUNDLE` are for programs the
service runs, such as `git`, Python and `curl`. Whether every OpenCode tool passes the
service's environment to the commands it runs was not checked.

**Missing trust does not look like an error.** OpenCode refuses the forged certificate,
Cortex passes that host through, and OpenCode's retry succeeds as an opaque tunnel.
OpenCode keeps working normally. The gap is in Cortex: the inference appears only as
`CONNECT` rows marked `tunnel`, with no parsed inference, no token counts or cost, and no
`ses_…` session, because the session header travels inside the encrypted request.
Cortex's log names the host:

```sh
grep client-hung-up ~/.cortex/proxy.log
```

For OpenCode Zen this finds a `WARN tls-bridge passthrough host=opencode.ai
reason=client-hung-up` line; another provider shows its own host. The reason is
`client-hung-up`, not the `client-rejected-ca` that
[Everything shows as `tunnel`](../laptop-service.md#everything-shows-as-tunnel-and-no-plugin-ever-runs)
is written around. `client-hung-up` records a client that closed the connection
mid-handshake without sending a TLS alert, which is what OpenCode does here. That page
lists `client-hung-up` as usually needing no action. When it repeats for OpenCode's
inference host, missing trust is the likely cause. The fix is to put the CA variables in
the environment the service starts with: run `enable`. If the service is running,
`enable` restarts it once the variables are set, so it comes back trusting the CA.

## What Cortex shows for it

**Its own agent.** OpenCode's TUI, its service and its inference client all send the
same User-Agent, `opencode/<channel>/<version>/<client>` (for example
`opencode/latest/2.0.21/cli`). Cortex recognises it as `opencode` and takes the version
from the field after the channel. OpenCode therefore gets its own row in agentop's
agents pane instead of sharing `Other`.

**Its own sessions.** On its inference requests the service sends a session id
(`ses_…`, the form that appears in the TUI's URLs) in `X-Session-Id`, and on a laptop
install Cortex groups by it. OpenCode sends its session's affinity id there: the parent
session's id for a subagent, the source session's for a fork, and otherwise the
session's own. So a
subagent or a fork is filed under the session it belongs to, with one row and one cost,
as Claude Code's subagents are. OpenCode also sends `X-Opencode-Session-Id`, the
session's own id, and Cortex does not read it. On a laptop install `X-Session-Id` is
read by default: it is in the list the laptop installer writes, and in the built-in
`session.id_headers` wherever every listener binds loopback (`listener.bind_loopback_only`).
A cluster deployment's default does not read it, because the name is generic: the IBAC
demo agent sends it with an id it minted. If you set that list yourself, include
`X-Session-Id`, because naming any header replaces the built-in list.

**Typed inference.** OpenCode Zen serves each model on the endpoint of the SDK it speaks,
and Cortex parses two of them, under both the `/zen` prefix and the `/zen/go` prefix of
OpenCode's Go plan:

- the OpenAI dialect (model, messages, tools and the response) on
  `/zen/v1/chat/completions`;
- the Anthropic dialect on `/zen/v1/messages`, where Zen serves its Claude models and
  most of its Qwen ones.

Cortex picks the dialect from how the path ends, not from the provider: a path ending in
`/completions` is read as OpenAI and one ending in `/v1/messages` as Anthropic, under any
prefix. A provider OpenCode is pointed at that speaks either one is parsed the same way:
Anthropic's own API, LiteLLM, OpenRouter, Groq. The body must also carry a
`messages` array, or a `prompt` for legacy completions. Zen's `/zen/v1/responses` (its GPT
and Grok models) and `/zen/v1/models/<id>` (its Gemini models) are other dialects, and are
recorded with their method and path but not parsed.

**Tokens and cost.** Token counts come from the usage block in the response. A streamed
OpenAI-dialect response includes one only when the request asked for it
(`stream_options.include_usage`). Whether OpenCode asks every provider for it was not
checked. Cost needs either a reported figure or a rate. A LiteLLM gateway's
`X-Litellm-Response-Cost` header is used as the figure. Otherwise the tokens are priced
at a rate:

- **Zen's free models** — those whose id ends in `-free`, and `big-pickle` — are priced
  at zero, a rate Cortex ships for `opencode.ai` only. A call to one counts as priced,
  but its cost cells in the events and sessions panes are blank: there is no amount to
  show.
- **Claude models**, Zen's included, are priced at the bundled Anthropic list rates.
  For the model checked, Claude Sonnet 4.6 at $3 in and $15 out per million tokens, that
  is what Zen charges.
- **Any other model** is reported unpriced until you add a `pricing:` entry for it.
  [Finding traffic that is not priced](../pricing.md#finding-traffic-that-is-not-priced)
  shows how to find the endpoint and model to add. Name the model rather than writing
  `"*"` for `opencode.ai`: a configured entry outranks a shipped one, so a catch-all would
  also replace the free models' zero.

**Tool pruning is unsupported.** The `tool-prune` plugin acts on any request whose path
ends in `/v1/chat/completions` or `/v1/messages`, Zen's included, and removes the tools
named in its `remove` list. That list comes from `agentop tools scan`, which reads only
Claude Code's transcripts and proposes only Claude Code's built-in tool names, so nothing
builds a list for OpenCode. A list written by hand would be applied to OpenCode's
requests; that was not tested.

**Header-less traffic.** On a laptop, `session.process_attribution` is on by default,
and a request with no session header is filed by the process that sent it. The
service's own header-less requests and tunnels, and those of the commands it runs, are
filed under its sessions; [Known issues](#known-issues) says which one. When
OpenCode's TUI talks to the service through Cortex, those requests are forwarded
without being recorded once the service has named a session. Before that, they are
recorded. See
[How traffic is grouped into sessions](../laptop-service.md#how-traffic-is-grouped-into-sessions).

**Switching inference servers.** `agentop server use <name> --agent opencode`
sends OpenCode's new sessions to that server, whatever provider is picked in
OpenCode: its requests to OpenCode Zen (`/zen/v1/chat/completions`, `/messages`
and `/responses`) or to any other provider are captured and sent to the server, so
nothing in `opencode.json` needs to change. OpenCode's own model choice goes first;
a name the server refuses — every one of Zen's own, such as
`nemotron-3.5-lightning-free` — is sent again, before OpenCode sees the refusal,
with the server's `main` model for a request with tools and its `helper` model for
one without, such as a title. OpenCode keeps showing the name it picked; agentop's
detail pane shows the model that answered. On a routed request the router puts the
server's key in the `X-Api-Key` or `Authorization` header OpenCode sent, and adds
`Authorization: Bearer <key>` when it sent neither. Zen's paid models are only
offered by OpenCode once it has a Zen key: any value in `OPENCODE_API_KEY` will
do, since the router replaces it. A session already running on Zen when OpenCode
is routed stays on Zen.

## Verified depth

Tested live with OpenCode 2.0.21 on macOS 26.6 (arm64) and Cortex built from the change
that added this page, using OpenCode Zen's models, which ran without a key. This was
exercised:

- `enable` refusing a value someone else set, setting the nine variables, and printing
  the macOS note;
- `status` saying the service `is using Cortex.` and `is not using Cortex.`;
- `disable` emptying the service environment;
- an `opencode run` with no proxy variables of its own, recorded under the `opencode`
  agent and one `ses_…` session, with no `default` row;
- the `exec` warning, with the command still running, in the single form it had before
  the restart-is-enough form was added;
- no warning for `agentop exec -- opencode service status`;
- the CA experiment in [CA trust](#ca-trust);
- `set env` and `unset env` stopping the service;
- an open TUI starting the service again without the new environment, which is why
  agentop restarts it;
- the earlier `enable --yes` and `disable --yes`, which let the CLI stop the service and
  printed that it starts with the new environment the next time OpenCode runs; a plain
  client then started it on Cortex.

Then, on the final change, with an OpenCode TUI open:

- `enable --yes` warned with the running service's pid, restarted it once, and reported
  it `is using Cortex.`; the restarted service carried the proxy and CA variables, and the
  open TUI kept its connection;
- `disable --yes` restarted it once more and reported it `no longer uses Cortex.`; the
  restarted service had no proxy variable, although the shell agentop ran in exported one;
- an `opencode run` was grouped by `X-Session-Id` into one `ses_…` session under the
  `opencode` agent, with no `default` row.

Not tested live:

- **A subagent or fork joining its parent's session.** For a session with no parent and
  no fork source, `X-Session-Id` carries the session's own id, which is the case that was
  run.
- **Linux.** agentop's code paths and the process lookup's are unit-tested on Linux in a
  container, but OpenCode itself was not run there.
- **Other OpenCode versions.** The User-Agent shape and the session header were captured
  from 2.0.21.
- **Providers other than OpenCode Zen**, LiteLLM included.
  [#941](https://github.com/rossoctl/cortex/issues/941) has one user's report of a
  LiteLLM run, started with `agentop exec` under its earlier name, in which token counts
  showed.

Routing through `inference-router` was tested live on 2026-10-09 with OpenCode 2.0.26
(`opencode run --standalone`), through a scratch Cortex in front of a GLM LiteLLM
server: Zen's `nemotron-3.5-lightning-free` was captured from `opencode.ai`, refused by
the server, and sent again with the server's main model for the tool-carrying turns and
its helper for the title, before OpenCode saw a refusal; a file was written and read
back.

## Known issues

- **One service holds several sessions.** The service's header-less traffic goes under
  the session its own requests named last. Two sessions whose tool calls overlap can
  take each other's rows. A new session's header-less traffic goes under the previous
  session until the new session's first request names it.
- **Before the service names its first session**, its requests go to a pending bucket,
  `pending:opencode@<pid>.<start>`, once one of them has identified the process as
  OpenCode by its User-Agent. The bucket is named for the topmost OpenCode process in
  the service's chain of parents, usually the service itself. The first session the
  service names adopts the bucket, unless that session already holds events. In that
  case the bucket stays a row of its own.
- **Changing the service environment restarts the service.** When `enable` or `disable`
  changes it and the service is running, they restart it once, which interrupts every
  OpenCode session using it; an open OpenCode reconnects to it. Both say so first, and
  ask unless given `--yes`. `agentop exec -- opencode` reaches the service only when
  that command starts it, because the service keeps the environment it started with
  ([Enable and revert](#enable-and-revert)).
- **Probes of local model servers that are not running.** The service probes local model
  servers on every cycle; on 2.0.21 these included `:1234` (LM Studio's port) and
  `:8000`. With nothing listening there, each probe is recorded in the service's session
  as a `502` response row with `error.kind: upstream_refused`, every cycle — the proxy
  synthesizes that 502 when the call to the upstream fails
  ([#1223](https://github.com/rossoctl/cortex/pull/1223)) — and these count in
  `/v1/usage`'s errors.
- **`X-Session-Id` is a generic name.** Where it is read, by default on a laptop install,
  traffic from any client that sends it is grouped under its value, Pi (Inflection AI)
  and similar frameworks among them.
- **Rows recorded before this release** keep the raw label `opencode/latest/2.0.21/cli`
  and show under `Other` until they age out.
