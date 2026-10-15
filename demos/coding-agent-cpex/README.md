# Coding agent under a Cortex + CPEX reference monitor

A coding agent's sandbox already decides which commands run and which hosts
they reach. It does not decide what may leave through an allowed host. This
demo puts Cortex, built with the cpex plugin, behind the sandbox's network
proxy and shows the difference: an allowed destination is not an authorized
disclosure.

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

## Run it

Prerequisites: macOS on arm64, `python3`, `openssl`, `curl`, `jq`, `srt`
(`npm install -g @anthropic-ai/sandbox-runtime`), DNS for `localtest.me`
(it resolves to loopback), and a cpex build of Cortex (`make cortex-cpex`
from the repo root, or `install.sh --cpex`).

```sh
./run.sh up          # cortex and mocks on 4770x; generates keys under .state/ and config/identity/
./run.sh scenarios   # all scenarios, with a summary
./run.sh down
```

`up` refuses to start when a process it did not start holds a demo port. It
runs its own `cortex --config config/cortex.yaml` with its own CA and state,
and never touches `~/.cortex` or an installed service on `:47600`.

## Limits

- The scripted agent is a stand-in for Claude Code: the scenarios show policy
  decisions under `srt`, not a model's behaviour.
- Content checks cover the patterns in the policy, in the first 8 KiB of a
  body sent as is. Encoded or transformed secrets, and a single command that
  reads and sends, are out of reach; a body past the excerpt is refused.
- The secret read in 04 reaches the model and is on the timeline as part of
  the model call, as Cortex records model traffic.
- Taint lives in CPEX's in-process store and resets when Cortex restarts.
- The support credential is not yet held by Cortex; the mock accepts reports
  without one.
