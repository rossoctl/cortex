"""Tests for the moca-sandbox catalog entry, core/plugins/script/catalog/entries/moca-sandbox/."""

import importlib.util
import json
import os
import pathlib
import re
import subprocess
import sys
import textwrap

import pytest
import yaml

REPO = pathlib.Path(__file__).resolve().parent.parent
SDK = REPO / "core" / "plugins" / "script" / "sdk" / "cortex.py"
SCRIPT = REPO / "core" / "plugins" / "script" / "catalog" / "entries" / "moca-sandbox" / "moca-sandbox.py"
GUIDE = REPO / "docs" / "script-plugin.md"

SANDBOX_TOOL = {"name": "mcp__sandbox__bash", "input_schema": {"type": "object"}}
LOCAL_TOOLS = [{"name": n, "input_schema": {"type": "object"}} for n in ("Bash", "Read", "Skill")]


@pytest.fixture
def sdk(monkeypatch):
    """A fresh SDK with the example's hooks registered on it. run() is stubbed, so
    loading the script does not serve stdin."""
    spec = importlib.util.spec_from_file_location("cortex", SDK)
    cortex = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(cortex)
    cortex.run = lambda enforce=False: None
    monkeypatch.setitem(sys.modules, "cortex", cortex)
    spec = importlib.util.spec_from_file_location("moca_sandbox", SCRIPT)
    spec.loader.exec_module(importlib.util.module_from_spec(spec))
    return cortex


def request(tools, messages=None, session="s1"):
    body = {"model": "claude-opus-5-5", "messages": messages or [{"role": "user", "content": "hi"}]}
    if tools is not None:
        body["tools"] = tools
    return {"hook": "request", "session": {"id": session} if session else {}, "request": {"body": json.dumps(body)}}


def sent(reply):
    return json.loads(reply["request"]["body"])


def test_local_tools_are_hidden_in_a_sandbox_session(sdk):
    reply, _ = sdk.handle(request([SANDBOX_TOOL] + LOCAL_TOOLS))
    assert [t["name"] for t in sent(reply)["tools"]] == ["mcp__sandbox__bash", "Skill"]
    assert reply["invocation"]["details"]["hidden"] == "Bash,Read"


def test_a_session_without_the_sandbox_tools_is_untouched(sdk):
    reply, _ = sdk.handle(request(LOCAL_TOOLS))
    assert reply == {}


def test_a_request_without_a_tool_list_is_covered_once_the_session_is_known(sdk):
    history = [
        {"role": "assistant", "content": [{"type": "tool_use", "id": "t1", "name": "Read", "input": {}}]},
        {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "t1", "content": "file text"}]},
    ]
    assert sdk.handle(request(None, history, session="s2"))[0] == {}  # session not known yet
    sdk.handle(request([SANDBOX_TOOL], session="s2"))
    reply, _ = sdk.handle(request(None, history, session="s2"))
    result = sent(reply)["messages"][1]["content"][0]
    assert result["is_error"] is True and "file text" not in result["content"]


def test_a_local_result_is_replaced_and_a_sandbox_result_kept(sdk):
    history = [
        {
            "role": "assistant",
            "content": [
                {"type": "tool_use", "id": "t1", "name": "Bash", "input": {}},
                {"type": "tool_use", "id": "t2", "name": "mcp__sandbox__bash", "input": {}},
            ],
        },
        {
            "role": "user",
            "content": [
                {"type": "tool_result", "tool_use_id": "t1", "content": "local secret"},
                {"type": "tool_result", "tool_use_id": "t2", "content": "sandbox output"},
            ],
        },
    ]
    reply, _ = sdk.handle(request([SANDBOX_TOOL], history))
    local, remote = sent(reply)["messages"][1]["content"]
    assert local["is_error"] is True
    assert "local secret" not in local["content"] and "mcp__sandbox__" in local["content"]
    assert remote == {"type": "tool_result", "tool_use_id": "t2", "content": "sandbox output"}
    assert reply["invocation"]["details"]["refused_results"] == "1"


# Claude Code sends a tool_result's content as blocks too, and a request may carry
# no session id; neither may let a local result through.
def test_a_block_list_result_without_a_session_is_replaced(sdk):
    blocks = [{"type": "text", "text": "file text"}]
    history = [
        {"role": "assistant", "content": [{"type": "tool_use", "id": "t1", "name": "Read", "input": {}}]},
        {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "t1", "content": blocks}]},
    ]
    reply, _ = sdk.handle(request([SANDBOX_TOOL], history, session=None))
    result = sent(reply)["messages"][1]["content"][0]
    assert result["is_error"] is True and "file text" not in json.dumps(result)
    assert sdk.handle(request(None, history, session=None))[0] == {}


def response(body, ctype, tools=(SANDBOX_TOOL,), session="r1"):
    return {
        "hook": "response",
        "session": {"id": session},
        "request": {"inference": {"tools": [{"name": t["name"]} for t in tools]}},
        "response": {"status": 200, "headers": {"content-type": ctype}, "body": body},
    }


def sse(*events):
    return "".join("event: %s\ndata: %s\n\n" % (e["type"], json.dumps(e)) for e in events)


START_BASH = {
    "type": "content_block_start",
    "index": 1,
    "content_block": {"type": "tool_use", "id": "t9", "name": "Bash", "input": {}},
}
STREAM = [
    {"type": "message_start", "message": {"id": "m", "content": []}},
    {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}},
    {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": "running Bash"}},
    {"type": "content_block_stop", "index": 0},
    START_BASH,
    {"type": "content_block_delta", "index": 1, "delta": {"type": "input_json_delta", "partial_json": "{}"}},
    {"type": "content_block_stop", "index": 1},
    {"type": "message_stop"},
]


def test_a_streamed_local_call_is_renamed_and_nothing_else_changes(sdk):
    body = sse(*STREAM)
    reply, _ = sdk.handle(response(body, "text/event-stream"))
    out = reply["response"]["body"]
    renamed = "data: " + json.dumps(
        dict(START_BASH, content_block=dict(START_BASH["content_block"], name="blocked__Bash")),
        separators=(",", ":"),
    )
    assert out == body.replace("data: " + json.dumps(START_BASH), renamed)
    assert reply["invocation"]["details"]["tools"] == "Bash"


# The stream is told apart by not being JSON, not by its Content-Type, which a
# gateway may drop: without it the local call would run.
def test_a_stream_without_a_content_type_is_still_renamed(sdk):
    reply, _ = sdk.handle(response(sse(*STREAM), ""))
    assert '"name":"blocked__Bash"' in reply["response"]["body"]


def test_crlf_line_endings_are_kept(sdk):
    body = sse(*STREAM).replace("\n", "\r\n")
    reply, _ = sdk.handle(response(body, "text/event-stream"))
    out = reply["response"]["body"]
    assert '"name":"blocked__Bash"' in out
    assert out.count("\r\n") == body.count("\r\n") and "\n" not in out.replace("\r\n", "")


def test_a_json_local_call_is_renamed(sdk):
    body = {
        "content": [
            {"type": "text", "text": "ok"},
            {"type": "tool_use", "id": "t1", "name": "Read", "input": {}},
            {"type": "tool_use", "id": "t2", "name": "mcp__sandbox__read", "input": {}},
        ]
    }
    reply, _ = sdk.handle(response(json.dumps(body), "application/json"))
    names = [b.get("name") for b in json.loads(reply["response"]["body"])["content"]]
    assert names == [None, "blocked__Read", "mcp__sandbox__read"]


def test_sandbox_calls_and_other_sessions_are_untouched(sdk):
    sandbox_only = dict(START_BASH, content_block=dict(START_BASH["content_block"], name="mcp__sandbox__bash"))
    assert sdk.handle(response(sse(sandbox_only), "text/event-stream"))[0] == {}
    # Its own session: r1 is now known to be a sandbox session.
    assert sdk.handle(response(sse(START_BASH), "text/event-stream", tools=LOCAL_TOOLS, session="r2"))[0] == {}


# A request with no tool list (a count_tokens call, a compaction) still belongs to a
# sandbox session once one of its requests listed the sandbox tools.
def test_a_response_in_a_known_session_without_a_tool_list_is_renamed(sdk):
    sdk.handle(request([SANDBOX_TOOL], session="r3"))
    reply, _ = sdk.handle(response(sse(START_BASH), "text/event-stream", tools=(), session="r3"))
    assert '"name":"blocked__Bash"' in reply["response"]["body"]


def test_a_body_that_is_not_json_is_left_alone(sdk):
    assert sdk.handle(response("upstream error", "text/plain"))[0] == {}
    assert sdk.handle(request([SANDBOX_TOOL]) | {"request": {"body": "not json"}})[0] == {}


# cp keeps the mode the checkout gave the file. Without the exec bit the slot sees
# no script at all: it is empty, so even fail_closed refuses nothing.
def test_the_script_is_executable():
    assert os.access(SCRIPT, os.X_OK)


def test_handshake_enforces_and_rewrites_responses():
    env = dict(os.environ, PYTHONPATH=str(SDK.parent))
    proc = subprocess.run(
        [sys.executable, str(SCRIPT)],
        input="",
        capture_output=True,
        text=True,
        env=env,
        timeout=30,
    )
    hs = json.loads(proc.stdout.splitlines()[0])
    assert hs["enforce"] is True and hs["rewrite_response"] is True
    assert hs["hooks"] == ["request", "response"]


# The guide's moca-sandbox section must match the entry: the slot it shows is
# the one install writes, and the MCP server it registers is the one the script
# keys on.
def test_the_guides_install_steps_match_the_script():
    section = GUIDE.read_text().split("### moca-sandbox:", 1)[1].split("\n### ", 1)[0]
    blocks = re.findall(r"```(\w+)\n(.*?)```", section, re.S)
    sh = [textwrap.dedent(b) for lang, b in blocks if lang == "sh"]
    (slot,) = yaml.safe_load(textwrap.dedent(next(b for lang, b in blocks if lang == "yaml")))
    entry = yaml.safe_load((SCRIPT.parent / "entry.yaml").read_text())
    assert slot == {"name": "script", "config": {"dir": SCRIPT.parent.name, "fail_closed": entry["fail_closed"]}}
    assert sh[0].strip() == "agentop script install " + SCRIPT.parent.name
    server = re.search(r"claude mcp add -s local (\S+) --", sh[1]).group(1)
    assert "mcp__%s__" % server in SCRIPT.read_text()
