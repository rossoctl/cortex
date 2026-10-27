#!/usr/bin/env python3
"""Keeps Claude Code on moca's sandbox tools without a project .claude/settings.json.

moca (github.com/rossoctl/moca) runs an agent's tools in a remote microVM and
offers them as the `sandbox` MCP server. `mocactl sbx up` denies Claude Code's
local tools in the project's .claude/settings.json, and that deny also stops
Claude Code loading CLAUDE.md (rossoctl/moca#568). This script does the deny's
job in Cortex instead, so the settings file is not needed.

It acts only in a session whose tool list carries an mcp__sandbox__ tool:

  request   the local tools are removed from the list the model sees, and the
            result of any local call that ran anyway is replaced with an error;
  response  a local tool call the model makes anyway is renamed blocked__<name>,
            so Claude Code answers "No such tool available" and runs nothing.

Both halves are needed: Claude Code runs a tool the model names even when that
tool was not in the list.

Install it as its own slot, failing closed — see "Examples" in
docs/script-plugin.md.
"""

import json

from cortex import on, run

# The tools `mocactl sbx up` denies: CLAUDE_DENY in moca's agent-config.ts.
LOCAL = {
    "Bash",
    "Read",
    "Edit",
    "Write",
    "Glob",
    "Grep",
    "NotebookEdit",
    "WebFetch",
    "WebSearch",
    "Agent",
    "Task",
    "EnterWorktree",
    "ExitWorktree",
    "Workflow",
    "Monitor",
    "PowerShell",
    "LSP",
}
SANDBOX_PREFIX = "mcp__sandbox__"
BLOCKED_PREFIX = "blocked__"
REFUSAL = (
    "Error: %s does not run in this session. The code is in a remote sandbox at /workspace: "
    "use the " + SANDBOX_PREFIX + " tools."
)

# Sessions seen carrying the sandbox tools, so a request with no tool list is still
# covered. Kept in memory: a restarted script relearns each session from its next
# request that lists tools.
sandbox_sessions = set()


def _dicts(v):
    return [x for x in v if isinstance(x, dict)] if isinstance(v, list) else []


def _is_sandbox(flow, tools):
    if any(str(t.get("name")).startswith(SANDBOX_PREFIX) for t in _dicts(tools)):
        if flow.session:
            sandbox_sessions.add(flow.session)
        return True
    return bool(flow.session) and flow.session in sandbox_sessions


@on.request
def hide_local_tools(flow):
    body = flow.json
    if not isinstance(body, dict) or not _is_sandbox(flow, body.get("tools")):
        return
    hidden = []
    if isinstance(body.get("tools"), list):
        hidden = sorted({t.get("name") for t in _dicts(body["tools"])} & LOCAL)
        body["tools"] = [t for t in body["tools"] if not (isinstance(t, dict) and t.get("name") in LOCAL)]
    messages = _dicts(body.get("messages"))
    ran = {}
    for m in messages:
        if m.get("role") == "assistant":
            for b in _dicts(m.get("content")):
                if b.get("type") == "tool_use" and b.get("name") in LOCAL and isinstance(b.get("id"), str):
                    ran[b["id"]] = b["name"]
    refused = 0
    for m in messages:
        if m.get("role") == "user":
            for b in _dicts(m.get("content")):
                use = b.get("tool_use_id")
                if b.get("type") == "tool_result" and isinstance(use, str) and use in ran:
                    b["content"] = REFUSAL % ran[use]
                    b["is_error"] = True
                    refused += 1
    if hidden or refused:
        flow.set_body(body)
        flow.record("modify", "local_tools_hidden", hidden=",".join(hidden), refused_results=refused)


def _block(block):
    """Renames block when it is a local tool call, and returns the name it had."""
    if isinstance(block, dict) and block.get("type") == "tool_use" and block.get("name") in LOCAL:
        name = block["name"]
        block["name"] = BLOCKED_PREFIX + name
        return name
    return None


def _block_sse(text):
    """Renames local tool calls in an Anthropic event stream. Only the data line
    that starts each one changes; every other byte, line endings included, is kept."""
    blocked = []
    lines = text.split("\n")
    for i, line in enumerate(lines):
        if not line.startswith("data:"):
            continue
        try:
            data = json.loads(line[5:])  # a trailing \r is whitespace to json
        except ValueError:
            continue
        if not isinstance(data, dict) or data.get("type") != "content_block_start":
            continue
        name = _block(data.get("content_block"))
        if name:
            blocked.append(name)
            lines[i] = "data: " + json.dumps(data, separators=(",", ":")) + ("\r" if line.endswith("\r") else "")
    return "\n".join(lines), blocked


def _block_json(text):
    """Renames local tool calls in a JSON response. None when text is not JSON,
    which is how a stream is told apart: its Content-Type may be missing."""
    try:
        body = json.loads(text)
    except ValueError:
        return None
    content = body.get("content") if isinstance(body, dict) else None
    blocked = [n for n in map(_block, _dicts(content)) if n]
    if not blocked:
        return text, []
    return json.dumps(body, ensure_ascii=False, separators=(",", ":")), blocked


@on.response(rewrite=True)
def block_local_calls(flow):
    if not _is_sandbox(flow, (flow.inference or {}).get("tools")):
        return
    body = flow.response.body
    if not body:
        return
    result = _block_json(body)
    new, blocked = result if result is not None else _block_sse(body)
    if blocked:
        flow.set_body(new)
        flow.record("modify", "local_call_blocked", tools=",".join(blocked))


run(enforce=True)
