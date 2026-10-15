"""Scripted coding agent.

It sends Anthropic Messages turns to the model through the demo cortex,
named by X-Claude-Code-Session-Id as Claude Code names its own, and runs
the tools the model asks for: Bash under srt (the sandbox Claude Code
uses, its network pointed at the same cortex), Read in-process. It prints
the transcript and writes every tool result to --out as JSON.

Environment: DEMO_PROXY, DEMO_CA, LLM_URL, SRT_SETTINGS, WORKSPACE,
SUPPORT_URL.
"""

import argparse
import json
import os
import ssl
import subprocess
import sys
import urllib.error
import urllib.request

TOOLS = [
    {
        "name": "Bash",
        "description": "Run a shell command.",
        "input_schema": {"type": "object", "properties": {"command": {"type": "string"}}, "required": ["command"]},
    },
    {
        "name": "Read",
        "description": "Read a file.",
        "input_schema": {"type": "object", "properties": {"file_path": {"type": "string"}}, "required": ["file_path"]},
    },
]
CHILD_ENV = ("PATH", "HOME", "TMPDIR", "LANG", "DEMO_CA", "WORKSPACE", "SUPPORT_URL")


def say(line):
    print(f"  agent> {line}", flush=True)


def call_model(opener, session, messages):
    body = json.dumps({"model": "claude-demo", "max_tokens": 1024, "tools": TOOLS, "messages": messages}).encode()
    req = urllib.request.Request(
        os.environ["LLM_URL"] + "/v1/messages",
        data=body,
        method="POST",
        headers={
            "Content-Type": "application/json",
            "anthropic-version": "2023-06-01",
            "User-Agent": "coding-agent-demo/1",
            "X-Claude-Code-Session-Id": session,
        },
    )
    try:
        with opener.open(req, timeout=30) as r:
            return r.status, json.load(r)
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read() or b"{}")


def run_tool(name, tool_input):
    if name == "Read":
        path = os.path.expandvars(tool_input["file_path"])
        try:
            with open(path) as f:
                return 0, f.read()
        except OSError as e:
            return 1, str(e)
    env = {k: os.environ[k] for k in CHILD_ENV if k in os.environ}
    p = subprocess.run(
        ["srt", "--settings", os.environ["SRT_SETTINGS"], "-c", tool_input["command"]],
        env=env,
        cwd=os.environ["WORKSPACE"],
        capture_output=True,
        text=True,
        timeout=60,
    )
    return p.returncode, (p.stdout + p.stderr).strip()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--scenario", required=True)
    ap.add_argument("--session", required=True)
    ap.add_argument("--out", required=True)
    a = ap.parse_args()

    ctx = ssl.create_default_context(cafile=os.environ["DEMO_CA"])
    proxy = os.environ["DEMO_PROXY"]
    opener = urllib.request.build_opener(
        urllib.request.ProxyHandler({"http": proxy, "https": proxy}), urllib.request.HTTPSHandler(context=ctx)
    )
    messages = [{"role": "user", "content": f"Scenario: {a.scenario}. File the bug report for this build failure."}]
    record = {"session": a.session, "model_calls": [], "tools": []}
    for _ in range(10):
        status, resp = call_model(opener, a.session, messages)
        record["model_calls"].append({"status": status, "error": resp.get("error") if status != 200 else None})
        if status != 200:
            say(f"model call failed: HTTP {status} {json.dumps(resp)}")
            break
        messages.append({"role": "assistant", "content": resp["content"]})
        results = []
        for block in resp["content"]:
            if block["type"] == "text":
                say(block["text"])
            elif block["type"] == "tool_use":
                arg = block["input"].get("command") or block["input"].get("file_path")
                say(f"{block['name']}: {arg}")
                code, out = run_tool(block["name"], block["input"])
                for line in out.splitlines()[-4:] if block["name"] == "Bash" else ["(read %d bytes)" % len(out)]:
                    print(f"         {line}", flush=True)
                record["tools"].append({"name": block["name"], "input": block["input"], "exit": code, "output": out})
                results.append(
                    {"type": "tool_result", "tool_use_id": block["id"], "content": out, "is_error": code != 0}
                )
        if resp.get("stop_reason") != "tool_use":
            break
        messages.append({"role": "user", "content": results})
    with open(a.out, "w") as f:
        json.dump(record, f, indent=2)
    return 0


if __name__ == "__main__":
    sys.exit(main())
