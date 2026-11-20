"""Tests for scripts/dev/cortex-session-dump.py, run as a user runs it: a subprocess against a
fake session API."""

import json
import subprocess
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlparse

import pytest

SCRIPT = Path(__file__).resolve().parent.parent / "scripts" / "dev" / "cortex-session-dump.py"

# "live" is in memory; "old" is held only by the session archive, so the proxy lists it only
# under ?archived=true, as GET /v1/sessions does.
EVENTS = {
    "live": [{"seq": 1, "at": "2026-10-04T10:00:00Z"}],
    "old": [{"seq": 1, "at": "2026-10-01T09:00:00Z"}, {"seq": 2, "at": "2026-10-01T09:00:01Z"}],
}


class FakeSessionAPI(BaseHTTPRequestHandler):
    def do_GET(self):
        url = urlparse(self.path)
        query = parse_qs(url.query)
        if url.path == "/v1/sessions":
            rows = [{"id": "live", "eventCount": 1}]
            if query.get("archived") == ["true"]:
                rows.append({"id": "old", "eventCount": 2, "resident": False})
            return self.reply(200, {"sessions": rows})
        if url.path.startswith("/v1/sessions/"):
            sid = url.path.removeprefix("/v1/sessions/")
            if sid not in EVENTS:
                return self.reply(404, {"error": "not found"})
            return self.reply(200, {"id": sid, "events": EVENTS[sid]})
        return self.reply(404, {"error": "not found"})

    def reply(self, code, body):
        data = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *args):
        pass


@pytest.fixture
def api():
    server = ThreadingHTTPServer(("127.0.0.1", 0), FakeSessionAPI)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    yield f"http://127.0.0.1:{server.server_address[1]}"
    server.shutdown()
    server.server_close()


def run_dump(api, out, *args):
    return subprocess.run(
        [sys.executable, str(SCRIPT), "--api", api, "--out", str(out), *args],
        capture_output=True,
        text=True,
    )


def dumped_ids(out):
    return sorted(p.stem for p in (out / "sessions").glob("*.jsonl"))


def test_session_flag_dumps_a_session_only_the_archive_holds(api, tmp_path):
    result = run_dump(api, tmp_path, "--session", "old")
    assert result.returncode == 0, result.stderr
    lines = (tmp_path / "sessions" / "old.jsonl").read_text().splitlines()
    assert [json.loads(line)["seq"] for line in lines] == [1, 2]


def test_session_flag_still_dumps_a_resident_session(api, tmp_path):
    result = run_dump(api, tmp_path, "--session", "live")
    assert result.returncode == 0, result.stderr
    assert dumped_ids(tmp_path) == ["live"]


def test_session_flag_still_fails_an_id_held_nowhere(api, tmp_path):
    result = run_dump(api, tmp_path, "--session", "nope")
    assert result.returncode == 1
    assert "nope: NOT IN INDEX" in result.stderr


def test_a_whole_dump_covers_the_sessions_in_memory(api, tmp_path):
    result = run_dump(api, tmp_path)
    assert result.returncode == 0, result.stderr
    assert dumped_ids(tmp_path) == ["live"]
