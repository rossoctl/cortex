"""Header-echo sink: answers every GET with the trace headers it received."""

import json
from http.server import BaseHTTPRequestHandler, HTTPServer


class Echo(BaseHTTPRequestHandler):
    def do_GET(self):
        hdr = {k.lower(): v for k, v in self.headers.items()}
        body = json.dumps({"traceparent": hdr.get("traceparent")}).encode()
        self.send_response(200)
        self.send_header("content-type", "application/json")
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *_):
        pass


HTTPServer(("0.0.0.0", 8000), Echo).serve_forever()
