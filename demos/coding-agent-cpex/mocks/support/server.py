"""Mock HTTPS support service for projects A and B.

POST /projects/<id>/reports  record the report and return 201
GET  /_requests              list recorded requests
POST /_reset                 clear recorded requests
POST /_config                {"credential_required": bool}

SUPPORT_TOKEN grants access to both projects. Credential checks are disabled
by default; /_config enables them for the custody scenarios.
"""

import argparse
import base64
import json
import os
import ssl
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PROJECTS = {"A", "B"}
lock = threading.Lock()
requests = []
settings = {"credential_required": False}


class Handler(BaseHTTPRequestHandler):
    server_version = "support-mock"

    def log_message(self, fmt, *args):
        pass

    def _body(self):
        n = int(self.headers.get("Content-Length") or 0)
        return self.rfile.read(n) if n else b""

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
        self._record(b"")
        self._send(404, {"error": "not found"})

    def do_POST(self):
        body = self._body()
        if self.path == "/_reset":
            with lock:
                requests.clear()
            return self._send(200, {"ok": True})
        if self.path == "/_config":
            settings.update(json.loads(body or b"{}"))
            return self._send(200, settings)
        self._record(body)
        parts = self.path.split("/")
        if len(parts) != 4 or parts[1] != "projects" or parts[3] != "reports" or parts[2] not in PROJECTS:
            return self._send(404, {"error": "not found"})
        if settings["credential_required"]:
            if self.headers.get("Authorization") != "Bearer " + os.environ.get("SUPPORT_TOKEN", ""):
                return self._send(401, {"error": "credential required"})
        self._send(201, {"project": parts[2], "status": "received"})

    def _record(self, body):
        try:
            text = body.decode()
        except UnicodeDecodeError:
            text = "base64:" + base64.b64encode(body).decode()
        with lock:
            requests.append(
                {
                    "method": self.command,
                    "path": self.path,
                    "headers": {k.lower(): v for k, v in self.headers.items()},
                    "body": text,
                }
            )


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
