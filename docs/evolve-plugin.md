# evolve (Self-Improvement Loop) Plugin

The `evolve` plugin connects Cortex to [altk-evolve](https://github.com/AgentToolkit/altk-evolve),
a service that learns *guidelines* — short instructions that would have steered an agent
better — from agent trajectories. It closes the loop [rossoctl#2653](https://github.com/rossoctl/rossoctl/issues/2653) describes:

1. **Capture** — the plugin records each completed main-agent turn as OpenAI chat messages.
2. **Learn** — when a session goes quiet, it sends the new turns to evolve, which distils
   guidelines from them into a **candidates** namespace.
3. **Inject** — on each main-agent request, it appends the guidelines relevant to the
   session's task, from a **reviewed** namespace (separate by default), to the system
   instructions.

Evolve is Python, so it runs beside Cortex and the plugin is a client of its stock MCP server
(`evolve-mcp --transport sse`). The tools it calls:

| Purpose | Tool |
|---|---|
| Fetch the task-relevant guidelines, with their text and provenance | `get_guidelines_with_attribution`, with `include_details` |
| Read their text and provenance, on an evolve without `include_details` | `list_entities` |
| Create the plugin's processing profile, once | `ensure_processing_profile` |
| Learn from a batch of turns | `process_trajectory` |

`include_details` ([altk-evolve #485](https://github.com/AgentToolkit/altk-evolve/pull/485)) returns each selected guideline's content and sources with
its id, in one call. An evolve that predates it rejects the argument; the plugin then asks
again without it and reads the guidelines with `list_entities`, which reads at most 200 from
the served namespace. It asks with `include_details` again after reconnecting, in case evolve
was upgraded.

## By default, nothing learned is injected until a person accepts it

Session content includes tool output: fetched web pages, file contents, command output. A
guideline distilled from it can carry text an attacker wrote, and injecting it makes that text
persistent across every later session. So learned guidelines land in `learn_namespace`
(default `cortex-candidates`), which the plugin never injects from, and are served only once
someone copies them into `serve_namespace` (default `cortex`):

```bash
evolve entities list cortex-candidates --type guideline     # review what was learned
evolve entities show cortex-candidates <id>                 # content and provenance
evolve entities add cortex --no-conflict-resolution \
  -c "<content>" -m '{"source_task_id": "...", "task_description": "..."}'   # accept
evolve entities delete cortex-candidates <id>               # clear the candidate
evolve entities delete cortex <id>                          # withdraw a served guideline
```

Keeping the sets in separate namespaces also keeps untrusted input away from what is served:
evolve's conflict resolution reconciles new guidelines only against the namespace they are
written to, so a trajectory can never rewrite or delete a reviewed guideline.

A withdrawn guideline stops being injected once each session's cache expires
(`cache_ttl_seconds`, default 30 minutes) and its guidelines are fetched again. If evolve
can't be reached then, the session keeps the guidelines it had, withdrawn ones included, and
its event says `refresh_failed`, until a refresh succeeds. To stop every guideline at once,
remove the `evolve` entry from the pipeline.

### Serving what is learned, without review

Setting `learn_namespace` to the same namespace as `serve_namespace` turns the review off:
whatever evolve learns from a session is served to the next one. Use it only where every
session's input is trusted, such as a local demo. In that setup:

- An attacker's text in a tool result can become a guideline that later sessions are given.
- A guideline can reinforce itself: sessions given it tend to follow it, evolve can learn it
  again from that, and conflict resolution merges the copies, raising its support. At enough
  support it joins evolve's always-on core and is injected whatever the task.
- Withdrawing a guideline means deleting it from that namespace.

## What a request looks like

By default the block is appended after the existing system instructions — a trailing `system`
text block on Anthropic Messages, the leading system message on Chat Completions,
`instructions` on the Responses API. With `placement: user` it instead leads the session's
first user message, wrapped in `<system-reminder>`, where Claude Code carries its own
harness context. Models attend to that position more than to the end of a long system
prompt, and some gateways drop trailing system blocks. The first user message is the same
on every turn, so either placement keeps the prompt cache hitting after the first request:

```text
<evolve-guidelines source="altk-evolve" ids="3,2">
Guidelines learned from earlier sessions on this machine. Apply them when they are relevant to the task; ignore them otherwise.
1. Verify the outputs of commands executed and cross-check them with the task requirements.
2. …
</evolve-guidelines>
```

Guidelines are fetched **once per session** (from the session's first user message, with the
agent's harness context removed) and the rendered block is cached, so every turn appends
identical bytes and the provider's prompt cache keeps hitting. Requests the plugin leaves
alone: subagents, tool-less side calls (titles, summaries), sessions without an id, agents
outside `agents`, and its own calls (`X-Evolve-Cortex`).

## What is learned from

Only the main conversation of sessions the plugin saw: each successful (2xx) main-agent
exchange contributes the new user messages, the assistant's reply and tool calls, and the tool
results the next request carried (truncated to 4 KB each, errors marked `[error]`).
Harness context and `<evolve-guidelines>` blocks are stripped, so evolve does not learn from
the agent's own context or from its injected guidelines. Harness context is Claude Code's
`<system-reminder>` blocks, and the blocks Codex sends as user text ahead of the prompt: the
repo's AGENTS.md instructions, `<environment_context>` (cwd, shell, date) and the like. A session is sent once it has been
idle for `idle_after_seconds` with at least `min_turns` new turns; a resumed session sends only
its new turns, with the original task as context.

Each send is an identified batch (`source: cortex`, the session id, a per-process sequence
number): a send that is retried after a timeout is skipped by evolve if it was already
processed. Unsent turns live in memory, so a Cortex restart drops them.

### Which prompt evolve learns with

The plugin's processing profile names evolve's `environment-grounded` guideline prompt
(altk-evolve [#479](https://github.com/AgentToolkit/altk-evolve/pull/479)). Coding agents work
where the project states its own rules: a pre-commit hook, a CI check or an error message says
what to do. That prompt turns each such requirement into a step to take first ("before
committing, add an entry under `## Unreleased` in `CHANGES.md`"), where evolve's general
prompt tends to learn it as advice about recovering from the error. Set `guidelines_prompt` to
name another prompt, or to `""` to use whatever the evolve server is configured with
(`EVOLVE_GUIDELINES_PROMPT`).

An evolve that can't take the prompt still learns. If it predates [altk-evolve #479](https://github.com/AgentToolkit/altk-evolve/pull/479) it rejects the key, and
if it doesn't have the prompt it rejects the name. In both cases the plugin logs
`evolve can't take the configured guideline prompt` once, and creates the profile with the
server's default prompt. It asks with the prompt again after reconnecting.

## Seeing what it did

Every handled request records an invocation (`modify` when the block was added, `skip` with a
reason otherwise) and publishes `plugins.evolve` on its session event:

```json
{"injected": true, "ids": ["3", "2"], "revisions": [null, null],
 "sources": ["cortex:<session>:<batch>", ""], "cached": true, "fetchedAt": "…"}
```

`ids` name exactly which guidelines shaped the request; `sources` is the session batch each
was learned from (comma-separated for a guideline consolidated from several). agentop's detail pane shows the event; the plugin pane shows counters
(`requests injected`, `fetch errors`, `sessions sent to learn`, …).

## Failure policy: open

Evolve unreachable, slow (`timeout_ms`, default 3 s, paid once per session), or returning an
error means the request is forwarded **unchanged**, a fetch error is counted, and the plugin
tries again after `retry_after_seconds`. A session that already has guidelines keeps them
through a failed refresh. A failed learning send is sent again as it was, under its own batch
id, after a wait that doubles from 30 seconds; new turns wait for it and go in a later batch.
After 6 failed sends the batch is dropped, with a warning and a `batches dropped` count.

## Configuration

```yaml
pipeline:
  outbound:
    plugins:
      - name: inference-parser
      - name: mcp-parser
      - name: a2a-parser
      - name: tool-prune
      - name: evolve
        config:
          endpoint: http://127.0.0.1:8201/sse
          agents: [claude-code]
```

| Field | Default | Meaning |
|---|---|---|
| `endpoint` | required | SSE URL of `evolve-mcp --transport sse`. |
| `serve_namespace` | `cortex` | Guidelines injected from: the reviewed set, unless it is also `learn_namespace`. |
| `learn_namespace` | `cortex-candidates` | Learned guidelines, for review. Set it to `serve_namespace` to serve them [without review](#serving-what-is-learned-without-review). |
| `processing_profile` | `cortex` | Evolve profile created and used for learning. |
| `guidelines_prompt` | `environment-grounded` | Evolve guideline prompt the profile learns with. `""`: the evolve server's own default. See [Which prompt evolve learns with](#which-prompt-evolve-learns-with). |
| `inject` | `true` | Append guidelines to main-agent requests. |
| `placement` | `system` | `system`: after the system instructions. `user`: a `<system-reminder>` leading the first user message. |
| `learn` | `true` | Send idle sessions' new turns to evolve. |
| `agents` | all | Client names to act on (`claude-code`, `codex`, `bob`, `opencode`). |
| `top_k` | `8` | Most guidelines injected per session. |
| `timeout_ms` | `3000` | Timeout for fetching a session's guidelines. |
| `learn_timeout_seconds` | `300` | Timeout for one learning call (evolve runs an LLM). |
| `cache_ttl_seconds` | `1800` | How long a session's guidelines are reused. |
| `retry_after_seconds` | `60` | Back-off after a failed fetch. |
| `idle_after_seconds` | `900` | Quiet time before a session's new turns are sent. |
| `min_turns` | `2` | Fewest new turns worth learning from. |
| `max_sessions` | `1024` | Sessions tracked for caching and learning. |

`on_error: observe` computes the block and publishes the event without changing any bytes.

## Running evolve beside Cortex

For a step-by-step walkthrough that runs the whole loop against a real Claude Code session,
without touching your everyday Cortex, see [evolve-plugin-e2e.md](evolve-plugin-e2e.md).

```bash
env -u HTTPS_PROXY \
  EVOLVE_INJECTION_MODE=retrieval \
  EVOLVE_BACKEND=filesystem EVOLVE_DATA_DIR=~/.evolve/data \
  EVOLVE_MODEL_NAME=<litellm model for guideline generation> \
  evolve-mcp --transport sse --port 8201
```

- `EVOLVE_INJECTION_MODE=retrieval` makes `get_guidelines_with_attribution` rank guidelines by
  task similarity; the default mode matches the task text literally and on the filesystem
  backend almost never returns anything.
- Unset `HTTPS_PROXY` so evolve's own LLM calls do not route through Cortex and get recorded
  as agent traffic.
- **The MCP server has no authentication**: any local process can read and change the
  guidelines it serves. Bind it to loopback (the default) and run it only on a single-user
  machine. A shared or remote Cortex needs an authenticated evolve before it can use this.

## Turning it off

Remove the `evolve` entry (or set `on_error: off`); the config hot-reloads. Learned guidelines
stay in evolve.

## Not yet

- **Shadow evaluation.** Adoption is a manual copy today. Injecting a candidate on a fraction
  of sessions and promoting it only when cost and an outcome signal strictly beat the baseline
  ([rossoctl#2653](https://github.com/rossoctl/rossoctl/issues/2653), SkillOpt's gate) is the next step.
- Redaction of session content before it reaches evolve's LLM (evolve's hooks can do this but
  are opt-in), and subagent trajectories.
- Ordering with `context-guru`, which may compact the block away; place `evolve` after it.
