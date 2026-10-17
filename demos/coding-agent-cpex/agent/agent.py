"""Scripted coding agent.

Sends Anthropic Messages requests through the demo Cortex, using
X-Claude-Code-Session-Id to identify the session. Runs Bash tools under srt
and Read tools in the agent process. Supports JSON and SSE model responses.
Prints the transcript and writes tool results to --out as JSON.

Environment: DEMO_PROXY, DEMO_CA, LLM_URL, SRT_SETTINGS, WORKSPACE,
SUPPORT_URL. AGENT_TURN_DELAY adds a delay in seconds before model calls
after the first (default 0). AGENT_OPEN_DELAY can extend the first delay
to let the recording open the session.

PRESENTER=1 prints tool summaries and outcomes. WALKTHROUGH_HOOK receives
`verdict allow|deny|blocked <code> <label>` after a tool result or blocked
model response.
"""

import argparse
import json
import os
import re
import ssl
import subprocess
import sys
import time
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


PRESENTER = os.environ.get("PRESENTER") == "1"
BOLD, DIM, RED, GREEN, CYAN, RESET = "\033[1m", "\033[2m", "\033[1;31m", "\033[1;32m", "\033[1;36m", "\033[0m"


def say(line):
    if PRESENTER:
        print(f"  {CYAN}agent{RESET}  {line}", flush=True)
    else:
        print(f"  agent> {line}", flush=True)


def label(name, tool_input):
    """Summarize a tool call's command, destination, and data."""
    if name == "Read":
        return f"read  {os.path.basename(tool_input.get('file_path', ''))}"
    cmd = tool_input.get("command", "")
    if "curl" not in cmd:
        words = cmd.split()
        return " ".join([words[0]] + [os.path.basename(w) for w in words[1:]]) if words else cmd
    m = re.search(r'"?\$SUPPORT_URL(/[^"\s]*)"?', cmd)
    if m:
        dest = "support" + m.group(1)
    else:
        m = re.search(r"https?://([^\s\"']+)", cmd)
        dest = m.group(1) if m else "?"
    dest = re.sub(r"/reports$", "", dest)
    if "report-with-key" in cmd:
        what = "report with AWS key"
    elif "report-clean" in cmd:
        what = "clean report"
    elif re.search(r"AKIA[0-9A-Z]{16}", cmd):
        what = "the AWS key"
    else:
        what = "data"
    if "head -c" in cmd:
        what = "padded " + what
    if "gzip" in cmd:
        what = "gzipped " + what
    if "X-Session-Id" in cmd:
        what += ", under another session id"
    host = re.search(r"-H '[Hh]ost: ([^']+)'", cmd)
    if host:
        what += f", Host: {host.group(1)}"
    via = "curl, around the proxy" if "--noproxy" in cmd else "curl"
    return f"{via} → {dest}  ({what})"


def outcome(name, code, out):
    """(verdict, code, line) for a tool result: allow/deny/blocked."""
    if name == "Read":
        return None
    m = re.search(r"^HTTP (\d+)$", out, re.M)
    status = m.group(1) if m else ""
    err = re.search(r'"error"\s*:\s*"([^"]+)"', out)
    if status.startswith("2"):
        return "allow", status, f"{GREEN}✓{RESET} {status} · delivered"
    if status == "403" and err:
        return "deny", err.group(1), f"{RED}✗{RESET} 403 · {err.group(1).removeprefix('cpex.')}"
    if status == "421":
        return "deny", "misdirected_request", f"{RED}✗{RESET} 421 · Host is not the tunnel's"
    if "CONNECT tunnel failed, response 403" in out:
        return "deny", "connect_refused", f"{RED}✗{RESET} connection refused by Cortex"
    if code != 0:
        last = out.splitlines()[-1].rsplit(": ", 1)[-1] if out else f"exit {code}"
        return "blocked", "sandbox", f"{RED}✗{RESET} blocked by the sandbox: {last[:60]}"
    return None


def hook(*args):
    h = os.environ.get("WALKTHROUGH_HOOK")
    if not h:
        return
    try:
        subprocess.run([h, *args], stdin=subprocess.DEVNULL, capture_output=True, timeout=10)
    except (OSError, subprocess.SubprocessError):
        pass


def read_stream(r):
    """Assemble an Anthropic SSE stream into a message."""
    msg, blocks, partial = {}, {}, {}
    for line in r:
        line = line.decode().strip()
        if not line.startswith("data:"):
            continue
        ev = json.loads(line[5:])
        t = ev.get("type")
        if t == "message_start":
            msg = ev["message"]
        elif t == "content_block_start":
            blocks[ev["index"]] = dict(ev["content_block"])
        elif t == "content_block_delta":
            d = ev["delta"]
            if d["type"] == "text_delta":
                blocks[ev["index"]]["text"] += d["text"]
            elif d["type"] == "input_json_delta":
                partial[ev["index"]] = partial.get(ev["index"], "") + d["partial_json"]
        elif t == "content_block_stop" and ev["index"] in partial:
            blocks[ev["index"]]["input"] = json.loads(partial.pop(ev["index"]) or "{}")
        elif t == "message_delta":
            msg.update(ev.get("delta", {}))
    msg["content"] = [blocks[i] for i in sorted(blocks)]
    return msg


def call_model(opener, session, messages, stream):
    req_body = {"model": "claude-demo", "max_tokens": 1024, "tools": TOOLS, "messages": messages}
    if stream:
        req_body["stream"] = True
    body = json.dumps(req_body).encode()
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
            if r.headers.get_content_type() == "text/event-stream":
                return r.status, read_stream(r)
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
    ap.add_argument("--stream", action="store_true", help="ask the model for SSE responses")
    ap.add_argument("--title", help="name the session, as Claude Code's /rename does")
    a = ap.parse_args()

    ctx = ssl.create_default_context(cafile=os.environ["DEMO_CA"])
    proxy = os.environ["DEMO_PROXY"]
    opener = urllib.request.build_opener(
        urllib.request.ProxyHandler({"http": proxy, "https": proxy}), urllib.request.HTTPSHandler(context=ctx)
    )
    ask = f"Scenario: {a.scenario}. File the bug report for this build failure."
    content = ask
    if a.title:
        rename = f"<command-name>/rename</command-name><command-args>{a.title}</command-args>"
        content = [{"type": "text", "text": rename}, {"type": "text", "text": ask}]
    messages = [{"role": "user", "content": content}]
    record = {"session": a.session, "stream": a.stream, "model_calls": [], "tools": []}
    delay = float(os.environ.get("AGENT_TURN_DELAY") or 0)
    open_delay = max(delay, float(os.environ.get("AGENT_OPEN_DELAY") or 0))
    for turn in range(10):
        if turn and delay:
            time.sleep(open_delay if turn == 1 else delay)
        status, resp = call_model(opener, a.session, messages, a.stream)
        record["model_calls"].append({"status": status, "error": resp.get("error") if status != 200 else None})
        if status != 200:
            err = resp.get("error")
            if PRESENTER:
                code = str(err.get("type", err) if isinstance(err, dict) else err).removeprefix("cpex.")
                print(f"  {CYAN}model{RESET}  {RED}✗{RESET} reply refused before the agent saw it · {code}", flush=True)
            else:
                say(f"model call failed: HTTP {status} {json.dumps(resp)}")
            if isinstance(err, str):
                hook("verdict", "deny", err, "the model's reply")
            if delay:
                time.sleep(delay)  # so a recording can show the refusal
            break
        messages.append({"role": "assistant", "content": resp["content"]})
        results = []
        for block in resp["content"]:
            if block["type"] == "text":
                say(block["text"])
            elif block["type"] == "tool_use":
                arg = block["input"].get("command") or block["input"].get("file_path")
                short = label(block["name"], block["input"])
                say(short if PRESENTER else f"{block['name']}: {arg}")
                code, out = run_tool(block["name"], block["input"])
                result = outcome(block["name"], code, out)
                if PRESENTER:
                    line = result[2] if result else f"{DIM}(read {len(out)} bytes){RESET}"
                    print(f"         {line}", flush=True)
                else:
                    for line in out.splitlines()[-4:] if block["name"] == "Bash" else ["(read %d bytes)" % len(out)]:
                        print(f"         {line}", flush=True)
                if result:
                    hook("verdict", result[0], result[1], short)
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
