"""Tests for the thinking-fill catalog entry, core/plugins/script/catalog/entries/thinking-fill/."""

import importlib.util
import json
import os
import pathlib
import sys

import pytest

REPO = pathlib.Path(__file__).resolve().parent.parent
SDK = REPO / "core" / "plugins" / "script" / "sdk" / "cortex.py"
SCRIPT = REPO / "core" / "plugins" / "script" / "catalog" / "entries" / "thinking-fill" / "thinking-fill.py"


@pytest.fixture
def sdk(monkeypatch):
    """A fresh SDK with the script's hooks registered on it. run() is stubbed, so
    loading the script does not serve stdin."""
    spec = importlib.util.spec_from_file_location("cortex", SDK)
    cortex = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(cortex)
    cortex.run = lambda enforce=False: None
    monkeypatch.setitem(sys.modules, "cortex", cortex)
    spec = importlib.util.spec_from_file_location("thinking_fill", SCRIPT)
    spec.loader.exec_module(importlib.util.module_from_spec(spec))
    return cortex


def gateway_response(host, version):
    headers = {"x-litellm-version": version} if version else {}
    return {
        "hook": "response",
        "request": {"host": host, "path": "/v1/messages"},
        "response": {"status": 200, "headers": headers, "body": "{}"},
    }


def messages_request(host, path="/v1/messages"):
    """A history with one signed empty thinking block, one signed block with text,
    one empty block with no signature, and one with an empty signature."""
    body = {
        "model": "claude-opus-5-5",
        "messages": [
            {"role": "user", "content": "fix it"},
            {
                "role": "assistant",
                "content": [
                    {"type": "thinking", "thinking": "", "signature": "SIG1"},
                    {"type": "thinking", "thinking": "plan", "signature": "SIG2"},
                    {"type": "thinking", "thinking": ""},
                    {"type": "thinking", "thinking": "", "signature": ""},
                    {"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": {}},
                ],
            },
            {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_1", "content": "ok"}]},
        ],
    }
    return {"hook": "request", "request": {"host": host, "path": path, "body": json.dumps(body)}}


def thinking_blocks(reply):
    """The edited request's thinking blocks, or None when it was left unchanged."""
    if "request" not in reply:
        return None
    content = json.loads(reply["request"]["body"])["messages"][1]["content"]
    return [b for b in content if b["type"] == "thinking"]


def filled(sdk, host, version, path="/v1/messages"):
    sdk.handle(gateway_response(host, version))
    reply, _ = sdk.handle(messages_request(host, path))
    return thinking_blocks(reply)


def test_fills_signed_empty_blocks_behind_an_affected_gateway(sdk):
    sdk.handle(gateway_response("gw.example", "1.105.1"))
    reply, printed = sdk.handle(messages_request("gw.example", "/v1/messages?beta=true"))
    assert thinking_blocks(reply) == [
        {"type": "thinking", "thinking": ".", "signature": "SIG1"},
        {"type": "thinking", "thinking": "plan", "signature": "SIG2"},
        {"type": "thinking", "thinking": ""},
        {"type": "thinking", "thinking": "", "signature": ""},
    ]
    assert printed == "thinking-fill: kept 1 earlier thinking block(s)"


@pytest.mark.parametrize(
    "version,path",
    [
        ("1.105.2", "/v1/messages"),
        ("1.106.1", "/v1/messages"),
        ("1.99.4", "/v1/messages"),
        ("", "/v1/messages"),  # the host never answered with the header
        ("1.105.1", "/v1/chat/completions"),
    ],
)
def test_leaves_every_other_request_unchanged(sdk, version, path):
    assert filled(sdk, "gw.example", version, path) is None


def test_a_host_that_has_not_answered_is_not_filled(sdk):
    sdk.handle(gateway_response("other.example", "1.105.1"))
    reply, _ = sdk.handle(messages_request("gw.example"))
    assert reply == {}


def test_stops_when_the_gateway_is_upgraded(sdk):
    assert filled(sdk, "gw.example", "1.105.1") is not None
    assert filled(sdk, "gw.example", "1.108.1") is None


@pytest.mark.parametrize(
    "version,want",
    [
        ("1.99.4", False),
        ("1.100.0", True),
        ("1.100.0-rc.1", True),
        ("1.104.8", True),
        ("1.105.1", True),
        ("1.105.2", False),
        ("1.106.0", True),
        ("1.106.1", False),
        ("1.108.1", False),
        ("v1.105.1", True),
        ("dev", False),
    ],
)
def test_affected_version_range(sdk, version, want):
    assert (filled(sdk, "gw.example", version) is not None) is want


def test_the_script_is_executable():
    assert os.access(SCRIPT, os.X_OK)
