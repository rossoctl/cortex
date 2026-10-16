# Coding agent security with Cortex and CPEX

This demo runs a scripted coding agent through Cortex with the
[cpex bridge](../../core/plugins/cpex/README.md).
The agent submits bug reports to a mock support service. Policy checks block
reports containing secrets, reports for unauthorized projects, and later
reports from sessions that have read a secret.

Cortex also checks the model's proposed tool calls for secret leaks before
returning them to the agent. The scenarios use scripted model responses, so
you can repeat each test and check its outcome.

[![asciicast](./demo/coding-agent-cpex.svg)](https://asciinema.org/a/ixFycJuuJlZqrEGR)

To use the same Cortex and policy with real Claude Code, follow the
[Claude Code guide](claude-code/README.md).

## Run the demo

You need:

- macOS on arm64.
- `python3`, `openssl`, `curl`, `jq`, `lsof`, and `gzip`.
- [`srt`](https://www.npmjs.com/package/@anthropic-ai/sandbox-runtime),
  installed with `npm install -g @anthropic-ai/sandbox-runtime`.
- DNS resolution for `localtest.me`, which resolves to loopback.
- A cpex build of Cortex: run `make cortex-cpex` from the repository root,
  or install it with `install.sh --cpex`.

From the repository root:

```sh
cd demos/coding-agent-cpex
./run.sh up                   # Start Cortex and the mock services
./run.sh scenarios            # Run all scenarios and print a summary
./run.sh walkthrough          # Step through the same scenarios with narration
./run.sh down                 # Stop the demo
```

The walkthrough pauses for Enter between scenarios. Use `--fast` to skip
pauses or `--pace N` to advance automatically every N seconds.

The demo starts a separate Cortex instance on loopback ports `4770x`, with
its own CA and state under `.state/` and keys under `config/identity/`.
It leaves `~/.cortex` and any installed Cortex service on `:47600` unchanged.
Startup fails if another process is using a demo port.

### Optional: keep the support credential in Vault

Start with `./run.sh up --custody` to have Cortex retrieve and attach the
support credential only after a report passes policy checks. The agent
never receives the credential, and the support service requires it.

This mode needs Docker and a Cortex build linked against CPEX 0.3.1 or
later, as provided by `make cortex-cpex`. Without `--custody`, scenarios 07
and 08 are skipped.

## Scenarios

| Scenario | Action | Expected result |
|---|---|---|
| 00 | Bypass the proxy or read the developer token from a sandboxed command | Both attempts blocked by the sandbox |
| 01 | Send a report to an unapproved host | Connection blocked (403) |
| 02 | Send a report without secrets to project A | Accepted (201) |
| 03 | Send a report containing an AWS key | Report blocked (403) |
| 04 | Read `.env`, then send a report without secrets | Report blocked (403) |
| 05 | Send a report to project B as a developer authorized only for A | Report blocked (403) |
| 06 | Send a report without secrets from a new session | Accepted (201) |
| 07 | Send valid reports with `--custody` enabled | Accepted (201), with the credential attached by Cortex |
| 08 | With `--custody`, send disallowed reports, then a valid report while Vault is unavailable | All blocked (403); no reports delivered |
| 09 | After reading `.env`, receive a model response proposing a command that leaks the key | Model response blocked (403); the proposed command never runs |

The scenarios also test several bypass attempts and failure cases:

- **01:** Setting `Host: support.localtest.me` does not disguise a connection
  to `paste.evil.example`: both attempts get `cpex.egress_host_not_allowed`.
  Cortex also rejects requests whose `Host` differs from the tunnel's
  destination (421).
- **03:** The key triggers `cpex.secret_in_body`. Padding it beyond the
  inspection limit or gzip-encoding the body triggers
  `cpex.egress_body_uninspectable`.
- **04:** Changing `X-Session-Id` does not bypass `cpex.session_tainted_secret`.
- **07–08:** Cortex replaces a made-up `Authorization` header with the Vault
  credential. Disallowed reports never trigger a Vault lookup. If Vault
  does not respond, an otherwise valid report gets
  `cpex.delegation_vault_timeout`.
- **09:** The model response check runs for both JSON and streamed replies
  (SSE), returning `cpex.secret_in_tool_call`. A report without secrets from
  a separate session still succeeds.

Checks inspect tool output, the Cortex session timeline, and the mock
services' request logs. They verify that blocked reports never reach the
support service, denial responses and CPEX decision records omit the test
secret, and neither mock receives the developer token. Model traffic that
contains a secret can still appear on the timeline; see [Limits](#limits).

## What goes through Cortex

| Traffic | Route to Cortex | Coverage |
|---|---|---|
| Sandboxed commands and their children | The sandbox's proxy port | Enforced: commands have no other network route |
| The agent's model calls (and WebFetch in Claude Code) | The agent's proxy configuration; `HTTPS_PROXY` for Claude Code | Routed through Cortex while the process respects that configuration |
| File tools, local MCP servers, and hooks | No direct interception | Execution is outside the sandbox and Cortex's traffic inspection |

The agent and both mock services run locally. The mock Anthropic API tells
the agent which tools to run: `Read` runs in the agent process; `Bash` runs in
[`srt`](https://www.npmjs.com/package/@anthropic-ai/sandbox-runtime), the
sandbox runtime used by Claude Code. Sandboxed commands can reach the
network only through Cortex. The mock support service accepts bug reports
for projects A and B.

```text
 agent ──(its own model calls, X-Claude-Code-Session-Id)──┐
   └─ srt sandbox: curl ──(only route out)───────────────┤
                                                          ▼
                     demo Cortex :47700  inference-parser → mcp-parser → cpex
                          │                              │
                          ▼                              ▼
          mock LLM llm.localtest.me:47710   mock support support.localtest.me:47711
```

## How it works

The policy is in [`config/cpex-policy.yaml`](config/cpex-policy.yaml), with
bridge settings in [`config/cortex.yaml`](config/cortex.yaml).

- **Identify the developer.** Cortex loads a developer JWT from
  `principal.token_file` and passes it to CPEX in memory. The token is never
  forwarded to either mock service, and the sandbox cannot read it.
- **Associate requests with a session.** The agent names its session with
  `X-Claude-Code-Session-Id`. Cortex uses process ancestry to associate
  child commands such as `curl` with that session. With
  `session_source: resolved`, a child's `X-Session-Id` header cannot change
  that association.
- **Check HTTP requests.** With `synthetic_egress`, requests not handled by
  another parser become `http_egress` tool calls. Policy checks the host,
  path, and request body. This demo inspects up to 8 KiB and blocks bodies
  that exceed that limit or use an unsupported encoding.
- **Track secret reads.** When a model request includes a tool result
  matching a secret pattern, CPEX marks the session as having read a secret
  ("tainted"). PPE 0.5.0 requires a denial to set this mark. The bridge's
  `observe_codes` setting records `secret_in_tool_result` as `observe`,
  allowing the model request to continue. Later support reports from the
  session are blocked.
- **Check proposed tool calls.** On `cmf.llm_output`, policy checks
  `custom.tool_calls`, including calls assembled from an SSE stream. In a
  tainted session, a call containing a recognized secret or matching a
  command pattern for sending a secrets file causes Cortex to block the
  model response (403). Requests from commands that do run are still
  checked by the HTTP policy.
- **Fetch credentials after approval.** With `--custody`, the final
  `http_egress` policy step calls `delegate(support-vault, ...)` to retrieve
  the support credential as the developer. Cortex attaches it through
  `delegated_credentials`, restricted to `support.localtest.me` by
  `delegated_credential_hosts`. Vault runs in dev mode on loopback, with JWT
  authentication for the developer key, the credential at
  `agents/dev-alice/support`, and its root token under `.state/`. See
  [`docker-compose.yml`](docker-compose.yml).

## Limits

- The mock model returns scripted responses. The demo tests specific policy
  rules and bypass attempts; it does not measure a live model's behavior.
- Secret detection uses the patterns in the policy. Transformed secrets
  that do not match those patterns may go undetected. HTTP request bodies
  larger than 8 KiB or using an encoding other than `identity` are blocked.
- Session tracking starts when Cortex sees a secret in a tool result sent
  to the model. A command that reads and sends a secret can run before
  Cortex observes that read. HTTP request checks still apply to its traffic.
- With cpex enabled, Cortex buffers responses, including SSE streams, up
  to the listener's 10 MB cap. The agent receives a streamed reply only
  after it is complete. Downloads exceeding the cap fail when Cortex
  decrypts and inspects the connection. Hosts configured for passthrough
  avoid this buffering and are not inspected.
- Model traffic is recorded on the session timeline. A tool result sent to
  the model can therefore expose the secret there. The developer JWT and,
  with `--custody`, the Vault support credential are kept out of that traffic.
- Session taint is stored in CPEX's process memory and resets when Cortex
  restarts or reloads its configuration.
- Without `--custody`, the mock support service accepts reports without a
  support credential.

## Recording and development

Run `./run.sh check` to verify that [`demo/steps.md`](demo/steps.md), the
walkthrough, and `scenarios/` list the same scenarios, and to run the
recording script's tests.

With the demo running, `python3 demo/record_demo.py --record` records the
walkthrough using asciinema. For a video showing the walkthrough, policy
rules, and agentop together, use `demo/record_tmux.sh`. It requires tmux,
asciinema 3, agg, ffmpeg, and an agentop build (`make agentop` from the
repository root).

The video recorder clears the demo Cortex's session list and writes `.cast`,
`.gif`, and `.mp4` files under the repository's `.sketchpad/demo-video/`.
Use `--only NN` to record one scenario. See
[`demo/record_tmux.sh`](demo/record_tmux.sh) or run it with `--help` for layout
and timing details.

For shorter terminal output, run `./walkthrough.sh --presenter` (or set
`PRESENTER=1`). It prints one line per agent action and one outcome per step
while keeping all checks enabled. Failed checks still print in full. See
`./walkthrough.sh --help` for other options.

## References

- [PPE (Praxis Policy Engine)](https://github.com/praxis-proxy/policy) provides
  the policy engine behind the demo's reference monitor, including identity
  resolution, authorization, session tracking, and credential delegation.
  In this demo, it validates the developer's identity, checks requests and
  proposed tool calls, tracks sessions that have read secrets, and optionally
  retrieves support credentials from Vault for approved requests.
- [CPEX](https://github.com/contextforge-org/cpex) wraps PPE and provides Go
  bindings through a C FFI. Cortex's cpex bridge uses those bindings to
  submit requests and model responses for evaluation and enforce the
  resulting decisions.
