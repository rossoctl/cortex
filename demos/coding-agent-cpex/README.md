# Coding agent behind Cortex and CPEX

A coding agent's sandbox already decides which commands run and which hosts
they reach. It does not decide what may leave through an allowed host. This
demo puts Cortex, built with the cpex plugin, behind the sandbox's network
proxy, where a policy decides on what each request carries.

## What a hardened setup already does

| Control | Provides | What remains | This demo adds |
|---|---|---|---|
| Permission rules | which commands and tools run | an allowed `curl` sends whatever it is given | a decision on the request itself |
| Sandbox with a domain allowlist | an OS boundary; egress by hostname | an allowed host receives any content, for any resource | the same boundary, routed to a policy on body, path and session history |
| Masked credentials (`mask`, `injectHosts`) | commands never hold the secret | the real value is injected on every request to those hosts, whatever it carries | credential custody gated on the decision: the credential is fetched only for a request the policy allowed (`--custody`) |
| Secret scanners | secrets found in files and commits | nothing on the egress path; no notion of what the session has read | a content check at egress, and a session tainted once it reads a secret |
| Hooks | a look at each tool call before it runs | they see the command string, not the bytes sent | decisions on the wire, and on the model's proposed tool calls |

Scenario 01, blocking an unapproved host, is what a sandbox allowlist does
already; it is there for comparison.

## What goes through cortex

| Traffic | Route to cortex | Status |
|---|---|---|
| sandboxed commands and their children | the sandbox's proxy port | enforced: there is no other route |
| the agent's own model calls (and WebFetch, for Claude Code) | `HTTPS_PROXY` | monitored: the process honours it |
| file tools, local MCP servers, hooks | none | outside both |

Everything runs on this machine. A scripted agent talks to a mock Anthropic
API and runs the tools the mock asks for: `Read` in-process, `Bash` under
[`srt`](https://www.npmjs.com/package/@anthropic-ai/sandbox-runtime), the
sandbox runtime Claude Code uses, with its network pointed at the demo
Cortex. A mock support service takes bug reports for projects A and B.

```text
 agent ──(its own model calls, X-Claude-Code-Session-Id)──┐
   └─ srt sandbox: curl ──(only route out)───────────────┤
                                                          ▼
                     demo cortex :47700  inference-parser → mcp-parser → cpex
                          │                              │
                          ▼                              ▼
          mock LLM llm.localtest.me:47710   mock support support.localtest.me:47711
```

## What it shows

| Scenario | Request | Outcome |
|---|---|---|
| 00 | sandboxed `curl --noproxy '*'` to the support service; `cat` of the developer token | both fail: the sandbox is the boundary |
| 01 | report to `paste.evil.example` | tunnel refused, `cpex.egress_host_not_allowed` (what a sandbox allowlist does too) |
| 02 | clean report, project A | 201 |
| 03 | report carrying an AWS key; the same padded past the excerpt; gzip-encoded | 403 `cpex.secret_in_body`, `cpex.egress_body_uninspectable` |
| 04 | agent reads `.env`, then sends a clean report, also with a spoofed `X-Session-Id` | the model turn goes through and taints the session; reports 403 `cpex.session_tainted_secret` |
| 05 | clean report, project B | 403 `cpex.project_not_authorized`: the developer is on A only |
| 06 | the clean report from a new session | 201: the taint is the session's, not the developer's |
| 07 | with `--custody`: a clean report to project A, then the same with a made-up `Authorization` | 201 both times; the support service received the Vault-held credential, never seen by the agent, the timeline or cortex's logs |
| 08 | with `--custody`: reports to project B and with a secret; then a clean report while Vault does not answer | 403 and Vault not consulted; then 403 `cpex.delegation_vault_timeout` |
| 09 | after reading `.env`, the model proposes a `curl` pasting the key, as JSON and as an SSE stream; an untainted session's clean report | the model turn 403 `cpex.secret_in_tool_call`, so the agent runs nothing; the clean report 201 |

Each scenario checks the agent's tool output, the Cortex session timeline
and the mock services' request logs, including that no deny body or timeline
carries the secret and that the developer token never reaches a mock.

## How it works

- **Who is asking.** Cortex holds a developer JWT (`principal.token_file`)
  and hands it to CPEX identity resolution in memory. It is never forwarded,
  and the sandbox cannot read it.
- **Which session.** `session_source: resolved` keys taint on the session the
  agent named with `X-Claude-Code-Session-Id`. The agent's `curl` is a child
  of the agent, so Cortex files it in that session by the process chain; a
  header the child sets does not move it.
- **Egress as a tool call.** With `synthetic_egress`, every request no parser
  claims is a call to the tool `http_egress`; the policy decides on its host,
  path and a bounded body excerpt ([`config/cpex-policy.yaml`](config/cpex-policy.yaml)).
- **Taint without blocking the turn.** The `llm` route's CEL step denies when
  a tool result holds a secret, and taints the session as it does. PPE 0.5.0
  has no way to taint without denying, so the plugin lists that code in
  `observe_codes`: the deny is recorded as `observe` and the turn is
  forwarded.
- **Intent before execution.** On `cmf.llm_output` the same route reads the
  tool calls the model proposes (`custom.tool_calls`, assembled from the
  stream when the reply is SSE). In a tainted session, one carrying a secret
  or sending a secrets file over the network refuses the reply: the agent
  gets a 403 instead of the tool call. Anything else goes on to the wire
  policy.

## Run it

Prerequisites: macOS on arm64, `python3`, `openssl`, `curl`, `jq`, `srt`
(`npm install -g @anthropic-ai/sandbox-runtime`), DNS for `localtest.me`
(it resolves to loopback), and a cpex build of Cortex (`make cortex-cpex`
from the repo root, or `install.sh --cpex`).

```sh
./run.sh up                   # cortex and mocks on 4770x; keys under .state/ and config/identity/
./run.sh scenarios            # all scenarios, with a summary
./run.sh walkthrough          # the same, narrated; --fast for no pauses, --pace N to advance alone
./run.sh down
```

`./run.sh up --custody` adds credential custody: Vault in dev mode
([`docker-compose.yml`](docker-compose.yml), loopback only, root token in
`.state/`), seeded with jwt auth for the developer key and the support
credential at `agents/dev-alice/support`. The support service then refuses
reports without it, and the policy's last `http_egress` step
(`delegate(support-vault, ...)`) has CPEX fetch it, as the developer, only
for a report every earlier step allowed; Cortex attaches it
(`delegated_credentials`), and only to `support.localtest.me`
(`delegated_credential_hosts`). It needs Docker and a Cortex built against CPEX 0.3.1 or later, which
`make cortex-cpex` links; without `--custody`, scenarios 07 and 08 skip.

`./run.sh check` checks that [`demo/steps.md`](demo/steps.md), the
walkthrough and `scenarios/` list the same scenarios, and runs its test.
`python3 demo/record_demo.py --record` records the walkthrough with
asciinema.

`demo/record_tmux.sh` records it as a video: a caption for each step on top
(from the last column of `demo/steps.md`), the walkthrough in presenter
mode on the left, the CPEX policy rule for the step on the right (dimmed
until the verdict, then highlighted with ALLOW or DENY; read from the
`# slide:` markers in `config/`), and agentop on the current session below
it. `--only NN` records one scenario. It needs tmux, asciinema 3, agg,
ffmpeg and `make agentop`, clears the demo cortex's session list, and writes
a `.cast`, `.gif` and `.mp4` under `.sketchpad/demo-video/`. It follows the
walkthrough through `WALKTHROUGH_HOOK`; see `walkthrough.sh --help`.

`./walkthrough.sh --presenter` (or `PRESENTER=1`) runs every check but
prints one line per agent action and one outcome per step; a failing check
still prints in full.

To run a real Claude Code session against the same cortex and policy, see
[`claude-code/README.md`](claude-code/README.md).

`up` refuses to start when a process it did not start holds a demo port. It
runs its own `cortex --config config/cortex.yaml` with its own CA and state,
and never touches `~/.cortex` or an installed service on `:47600`.

## Limits

- The scripted agent is a stand-in for Claude Code, run under `srt`: the
  scenarios show policy decisions, not a model's behaviour. For the same
  policy enforced on a real, sandboxed Claude Code, see
  [`claude-code/`](claude-code/README.md).
- Content checks cover the patterns in the policy, in the first 8 KiB of a
  body sent as is; a body past that, or encoded, is refused. Secrets
  transformed some other way, and a single command that reads and sends,
  are out of reach.
- With cpex in the chain every response is buffered, SSE included, under
  the listener's 10MB cap: a streamed reply reaches the agent only once it
  is complete, and a large download through a bridged host fails. Tunnelled
  (passthrough) hosts avoid that, and are not inspected.
- Cortex records model traffic, so a secret the agent reads is on the
  session timeline as part of the model call that carried it.
- Taint lives in CPEX's in-process store and resets when Cortex restarts or
  reloads its config.
- Without `--custody` the support service accepts reports without a
  credential.
