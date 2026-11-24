#!/usr/bin/env python3
"""A fake Anthropic Messages API for the release smoke tests (#956).

Answers POST /v1/messages with a fixed, well-formed reply that reports token usage,
so a smoke test can send one request through Cortex and check that Cortex parsed it
and recorded the tokens, without an API key, a cost or a provider that can be down.
Any other request gets a 404.

Usage: smoke_fake_inference.py <port>
Serves on 127.0.0.1 until killed. Port 0 picks a free one; the port it serves on is
printed as the first line of stdout either way, once it is listening.
"""

import http.server
import json
import sys

INPUT_TOKENS = 12
OUTPUT_TOKENS = 3


class Handler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        try:
            request = json.loads(self.rfile.read(length) or b"{}")
        except ValueError:
            request = {}
        if self.path.split("?", 1)[0] != "/v1/messages":
            self.send_error(404)
            return
        body = json.dumps(
            {
                "id": "msg_smoke",
                "type": "message",
                "role": "assistant",
                "model": request.get("model", "claude-smoke"),
                "content": [{"type": "text", "text": "ok"}],
                "stop_reason": "end_turn",
                "stop_sequence": None,
                "usage": {"input_tokens": INPUT_TOKENS, "output_tokens": OUTPUT_TOKENS},
            }
        ).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    server = http.server.HTTPServer(("127.0.0.1", int(sys.argv[1])), Handler)
    print(server.server_address[1], flush=True)
    server.serve_forever()
