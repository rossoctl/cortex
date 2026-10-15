"""Mock Anthropic Messages API that plays a scripted agent turn by turn.

The first user message names the scenario ("Scenario: <name>"); each
assistant turn already in the conversation advances one step through that
scenario's script in scripts.json. A step is a tool call (Bash or Read) or a
closing text. A request with "stream": true gets the same message as an
Anthropic SSE stream, the tool input split across input_json_delta events.
Every request is recorded:

GET  /_requests   requests received
POST /_reset      forget them
"""

import argparse
import json
import os
import ssl
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HERE = os.path.dirname(os.path.abspath(__file__))
with open(os.path.join(HERE, "scripts.json")) as f:
    SCRIPTS = json.load(f)

lock = threading.Lock()
requests = []


def text_of(content):
    if isinstance(content, str):
        return content
    return " ".join(b.get("text", "") for b in content if b.get("type") == "text")


def reply(req):
    messages = req.get("messages", [])
    first = text_of(messages[0]["content"]) if messages else ""
    name = first.split("Scenario:", 1)[1].split()[0].rstrip(".") if "Scenario:" in first else ""
    script = SCRIPTS.get(name, [{"text": "unknown scenario"}])
    step_no = sum(1 for m in messages if m.get("role") == "assistant")
    step = script[min(step_no, len(script) - 1)]
    model = req.get("model", "claude-demo")
    base = {
        "id": f"msg_{name}_{step_no}",
        "type": "message",
        "role": "assistant",
        "model": model,
        "usage": {"input_tokens": 100, "output_tokens": 20},
    }
    if "tool" in step:
        tool_input = {"command": step["command"]} if step["tool"] == "Bash" else {"file_path": step["path"]}
        content = []
        if step.get("say"):
            content.append({"type": "text", "text": step["say"]})
        content.append({"type": "tool_use", "id": f"toolu_{step_no}", "name": step["tool"], "input": tool_input})
        return {**base, "content": content, "stop_reason": "tool_use"}
    return {**base, "content": [{"type": "text", "text": step["text"]}], "stop_reason": "end_turn"}


def sse_events(msg):
    """The Anthropic stream events that assemble msg."""
    head = {k: v for k, v in msg.items() if k not in ("content", "stop_reason")}
    yield "message_start", {"type": "message_start", "message": {**head, "content": []}}
    for i, block in enumerate(msg["content"]):
        if block["type"] == "text":
            start = {"type": "text", "text": ""}
            yield "content_block_start", {"type": "content_block_start", "index": i, "content_block": start}
            delta = {"type": "text_delta", "text": block["text"]}
            yield "content_block_delta", {"type": "content_block_delta", "index": i, "delta": delta}
        else:
            start = {**block, "input": {}}
            yield "content_block_start", {"type": "content_block_start", "index": i, "content_block": start}
            raw = json.dumps(block["input"])
            for frag in (raw[: len(raw) // 2], raw[len(raw) // 2 :]):
                delta = {"type": "input_json_delta", "partial_json": frag}
                yield "content_block_delta", {"type": "content_block_delta", "index": i, "delta": delta}
        yield "content_block_stop", {"type": "content_block_stop", "index": i}
    delta = {"type": "message_delta", "delta": {"stop_reason": msg["stop_reason"]}, "usage": {"output_tokens": 20}}
    yield "message_delta", delta
    yield "message_stop", {"type": "message_stop"}


class Handler(BaseHTTPRequestHandler):
    server_version = "llm-mock"

    def log_message(self, fmt, *args):
        pass

    def _send(self, status, obj):
        data = json.dumps(obj).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        if self.path == "/_requests":
            with lock:
                return self._send(200, {"count": len(requests), "requests": list(requests)})
        self._send(404, {"error": "not found"})

    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(n) if n else b""
        if self.path == "/_reset":
            with lock:
                requests.clear()
            return self._send(200, {"ok": True})
        with lock:
            requests.append(
                {
                    "path": self.path,
                    "headers": {k.lower(): v for k, v in self.headers.items()},
                    "body": raw.decode(errors="replace"),
                }
            )
        if self.path.split("?")[0] != "/v1/messages":
            return self._send(404, {"type": "error", "error": {"type": "not_found_error"}})
        req = json.loads(raw or b"{}")
        msg = reply(req)
        if not req.get("stream"):
            return self._send(200, msg)
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.end_headers()
        for event, data in sse_events(msg):
            self.wfile.write(f"event: {event}\ndata: {json.dumps(data)}\n\n".encode())
            self.wfile.flush()
        self.close_connection = True


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, required=True)
    ap.add_argument("--cert", required=True)
    ap.add_argument("--key", required=True)
    a = ap.parse_args()
    srv = ThreadingHTTPServer(("127.0.0.1", a.port), Handler)
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ctx.load_cert_chain(a.cert, a.key)
    srv.socket = ctx.wrap_socket(srv.socket, server_side=True)
    srv.serve_forever()


if __name__ == "__main__":
    main()
