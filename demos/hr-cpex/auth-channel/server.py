"""CIBA approval channel — the approver's "phone" for the HR demo.

Keycloak's built-in HTTP authentication channel POSTs every CIBA request
here: the body names the approver (`login_hint`) and what is asked
(`binding_message`), and the Authorization header carries a token that
answers it. Approving or denying posts that token back to Keycloak's
callback endpoint; the sidecar's next poll then sees the answer.

    GET  /                    the approval page (auto-refreshes)
    POST /ciba/auth           Keycloak's channel request
    GET  /requests            every request, JSON (?login_hint=, ?status=)
    POST /approve/{id}        answer SUCCEED
    POST /deny/{id}           answer UNAUTHORIZED
    GET  /healthz

DEMO ONLY: no route here authenticates its caller, so anyone who can
reach the service can approve. A real channel authenticates the approver.
"""

import html
import json
import logging
import os
import sys
import threading
import time
import urllib.error
import urllib.request
import uuid
from typing import Any

from fastapi import FastAPI, Request
from fastapi.responses import HTMLResponse, JSONResponse, RedirectResponse

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s %(levelname)-7s %(message)s",
    handlers=[logging.StreamHandler(sys.stderr)],
)
logger = logging.getLogger("auth-channel")


def _safe(value: object) -> str:
    """A value fit for one log line: newlines and carriage returns removed."""
    return str(value).replace("\r", "").replace("\n", "")


KEYCLOAK_BASE = os.environ.get("KEYCLOAK_BASE", "http://keycloak.cpex-demo:8080")
KEYCLOAK_REALM = os.environ.get("KEYCLOAK_REALM", "cpex-demo")
CALLBACK_URL = f"{KEYCLOAK_BASE}/realms/{KEYCLOAK_REALM}/protocol/openid-connect/ext/ciba/auth/callback"

app = FastAPI(title="CIBA approval channel")

_lock = threading.Lock()
_requests: dict[str, dict[str, Any]] = {}  # id -> request, insertion-ordered


def _public(rid: str, r: dict[str, Any]) -> dict[str, Any]:
    return {k: v for k, v in r.items() if k != "token"} | {"id": rid}


@app.post("/ciba/auth")
async def ciba_auth(request: Request) -> JSONResponse:
    auth = request.headers.get("authorization", "")
    if not auth.startswith("Bearer "):
        return JSONResponse({"error": "missing bearer token"}, status_code=400)
    body = await request.json()
    rid = uuid.uuid4().hex[:12]
    with _lock:
        _requests[rid] = {
            "login_hint": body.get("login_hint", ""),
            "binding_message": body.get("binding_message", ""),
            "scope": body.get("scope", ""),
            "status": "pending",
            "received": time.strftime("%Y-%m-%d %H:%M:%S"),
            "token": auth[len("Bearer ") :],
        }
    logger.info(
        "approval request %s for %s: %s", rid, _safe(body.get("login_hint")), _safe(body.get("binding_message"))
    )
    # Keycloak treats anything but 201 as a failed delivery.
    return JSONResponse({"id": rid}, status_code=201)


def _answer(rid: str, status: str) -> tuple[int, dict[str, Any]]:
    with _lock:
        r = _requests.get(rid)
        if r is None:
            return 404, {"error": "unknown request"}
        if r["status"] != "pending":
            return 409, {"error": f"already {r['status']}"}
        token = r["token"]
    req = urllib.request.Request(
        CALLBACK_URL,
        data=json.dumps({"status": status}).encode(),
        headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=5) as resp:
            code = resp.status
    except urllib.error.HTTPError as e:
        code = e.code
    except OSError as e:
        logger.warning("callback to Keycloak failed: %s", e)
        return 502, {"error": "Keycloak unreachable"}
    if code != 200:
        logger.warning("Keycloak refused the %s callback for %s: HTTP %d", _safe(status), _safe(rid), code)
        return 502, {"error": f"Keycloak answered {code}"}
    with _lock:
        r["status"] = "approved" if status == "SUCCEED" else "denied"
    logger.info("request %s %s", _safe(rid), r["status"])
    return 200, _public(rid, r)


def _reply(request: Request, code: int, body: dict[str, Any]):
    # The page's buttons post a form and want to land back on the page.
    if "text/html" in request.headers.get("accept", ""):
        return RedirectResponse("/", status_code=303)
    return JSONResponse(body, status_code=code)


@app.post("/approve/{rid}")
async def approve(rid: str, request: Request):
    return _reply(request, *_answer(rid, "SUCCEED"))


@app.post("/deny/{rid}")
async def deny(rid: str, request: Request):
    return _reply(request, *_answer(rid, "UNAUTHORIZED"))


@app.get("/requests")
async def list_requests(login_hint: str | None = None, status: str | None = None) -> list[dict[str, Any]]:
    with _lock:
        return [
            _public(rid, r)
            for rid, r in _requests.items()
            if (login_hint is None or r["login_hint"] == login_hint) and (status is None or r["status"] == status)
        ]


@app.get("/healthz")
async def healthz() -> dict[str, str]:
    return {"status": "ok"}


@app.get("/", response_class=HTMLResponse)
async def page() -> str:
    with _lock:
        items = list(_requests.items())[::-1]
    rows = []
    for rid, r in items:
        e = {k: html.escape(str(v)) for k, v in r.items() if k != "token"}
        buttons = ""
        if r["status"] == "pending":
            buttons = (
                f'<form method="post" action="/approve/{rid}"><button class="ok">Approve</button></form>'
                f'<form method="post" action="/deny/{rid}"><button class="no">Deny</button></form>'
            )
        rows.append(
            f'<div class="req {e["status"]}"><b>{e["login_hint"]}</b> <span>{e["status"]}</span>'
            f"<p>{e['binding_message']}</p><small>{e['received']} · {rid}</small>{buttons}</div>"
        )
    body = "".join(rows) or "<p>No approval requests yet.</p>"
    return f"""<!doctype html><html><head><meta charset="utf-8"><title>Approvals</title>
<meta http-equiv="refresh" content="3">
<style>
body{{font-family:system-ui,sans-serif;max-width:640px;margin:2em auto;padding:0 1em}}
.req{{border:1px solid #ccc;border-left:5px solid #f90;border-radius:6px;padding:.8em;margin:.8em 0}}
.req.approved{{border-left-color:#2a2}} .req.denied{{border-left-color:#c22}}
form{{display:inline}} button{{margin:.4em .4em 0 0;padding:.4em 1em}}
.ok{{background:#2a2;color:#fff;border:0}} .no{{background:#c22;color:#fff;border:0}}
</style></head><body><h1>Approval requests</h1>{body}</body></html>"""
