# Agent-neutral timeline attribution — design

Date: 2026-10-30
Issue: rossoctl/cortex#1559
Branch: `feat/agent-neutral-timeline`

## Problem

The timeline (`GET /v1/sessions/{id}/timeline`) attributes turns, tools and
agents only to Claude Code. The gate is `agentOf`
(`core/session/timeline/agents.go:361`): it names an agent only when the event
carries Claude Code's markers — `Subagent` (the `X-Claude-Code-Agent-Id`
header) or `Inference.AgentRole` (from the `cc_is_subagent` billing marker).
Every other client's inference requests are dropped in `pairCalls`, so those
sessions render host rows only.

Measured live on a Bob session: 160 host spans, 0 turns, 0 tools, 0 agents.
Bob's traffic carries everything the timeline needs — inference parses with
`messages` and `model` on `api.us-east.bob.ibm.com
/inference/v1/chat/completions`, and tool calls — but none of it is
attributed.

## Goals

- Bob sessions show turns (all model aliases) and tools on the timeline.
- The fix is agent-neutral: any known agent (Codex, Pi, OpenCode, …) gets the
  same treatment. No per-agent adapter.
- Claude Code's existing timeline is unchanged.

## Non-goals

- Subagents, returns and jobs for non-Claude-Code agents — they key on
  Claude marker strings (`Agent`/`Task` tool names, `<agent-message>`,
  `<task-notification>`) those clients don't send. Empty is correct.
- Cost per span and needs-you notification — #1560, builds on this.
- The Bob-on-Ollama gap: a live session against `localhost:11434` had no
  inference parses at all. Likely Ollama native-API parser coverage or an
  older proxy; separate issue.

## Design

### 1. Attribution by client

`agentOf` gains a fallback after the Claude cases: a request whose inference
parsed but carries no Claude markers attributes to `MainAgent` when
`e.Client` is a known agent (`pipeline.IsKnownAgent(e.Client.Name)`).
`SessionEvent.Client` is already recorded on every event from User-Agent
sniffing (`core/pipeline/client.go`).

Non-Claude-Code agents get a single `main` row. Unknown clients (curl,
scripts) stay host-only: the timeline must not claim agency where there is
none.

### 2. Side-call filter scoped to Claude Code

`isSideCall` (`agents.go:373`) treats "no tools" as a side call for *any*
inference. Under generic attribution that would eat Bob's `security` and
`background` turns, which never carry tools. Gate the whole filter to Claude
Code traffic — `Inference.AgentRole != ""` is the signal, since only Claude
Code sets it — so non-Claude side calls render as turns, labeled by model.

Amended in review: only the no-tools rule took the gate. The prompt literals
run for every client, so pre-marker Claude Code traffic (no `AgentRole`)
still drops its recaps — they are Claude's own prompt text, which no other
client sends.

### 3. `Turn.Model`

Add `Model string` (omitempty) to the `Turn` span, populated from the paired
request's `Inference.Model`. Without it Bob's `premium-ide`, `security` and
`background` turns are indistinguishable. agentop renders it as a label.

### 4. Parser: chat-completions tool results

`InferenceExtension.ToolResults` is populated only for Anthropic
(`tool_result` blocks) and the Responses API (`function_call_output`). OpenAI
chat-completions requests carry results as `role: "tool"` messages with
`tool_call_id`, which the parser currently drops — `InferenceMessage` has no
id field. Map those messages onto `ToolResults[].ToolUseID` in the
chat-completions request path so tool spans close. Live Bob traffic
confirmed the shape: 1 assistant `tool_calls`, 2 `role: "tool"` results.

Tool span pairing (`ToolCalls[].ID` → first request carrying the matching
`ToolResults[].ToolUseID`) then works unchanged.

### 5. Docs

`docs/session-api.md:135` — "Agents, turns, tools and returns are Claude Code
only" — rewritten: attribution is by known client; subagent tree, returns and
jobs remain Claude Code only.

## Testing

- Red-first unit test: a non-Claude known-agent event with an inference parse
  produces a turn (today: none).
- Bob-shaped fixture in `core/session/timeline`: chat-completions events with
  `premium-ide`/`security`/`background` turns (all three render) and one tool
  call closed by a `role: "tool"` result.
- Parser test: `role: "tool"` + `tool_call_id` lands in `ToolResults`.
- Existing Claude Code timeline tests must pass unchanged.
- Live verification against the archived Bob sessions on the laptop proxy
  after a scratch deploy: host-rows-only → turns + tools.

## Size

One PR, ~350–450 lines including tests.
