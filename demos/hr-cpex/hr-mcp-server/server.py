"""HR Demo MCP Server — HTTP/JSON-RPC variant.

A small FastAPI app that speaks MCP-shaped JSON-RPC over HTTP so a
gateway can route to it.

The headline log line is intentional: every inbound request prints
the Authorization header (so audiences can see the IdP-minted token,
NOT the user's original IdP JWT) and the parsed tool arguments
(so they can see args.ssn redacted to `[REDACTED]` when policy
fires).

Run locally:
    pip install -r requirements.txt
    uvicorn server:app --host 0.0.0.0 --port 9100

Endpoints:
    POST /mcp        JSON-RPC 2.0 `tools/call` requests
    GET  /_requests  what reached this server since the last reset
    POST /_reset     drop the recorded requests

`/_requests` lets the scenarios assert what the sidecar forwarded rather
than print a claim next to a log line.

Tools:
    get_compensation, send_email, display_compensation, get_directory,
    search_repos, adjust_compensation, get_headcount
"""

import base64
import hashlib
import json
import logging
import math
import sys
import time
from collections import deque
from typing import Any

from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s %(levelname)-7s %(message)s",
    handlers=[logging.StreamHandler(sys.stderr)],
)
logger = logging.getLogger("hr-mcp-server")

app = FastAPI(title="HR Demo MCP Server")

# ---------------------------------------------------------------------------
# Request recorder: every inbound /mcp request, bounded and in memory.
# ---------------------------------------------------------------------------

_RECORDED: deque[dict[str, Any]] = deque(maxlen=50)


def _jwt_claims(token: str) -> dict[str, Any]:
    """Decode a JWT payload WITHOUT verifying it, so a scenario can tell
    which token arrived. Never use this to make a decision."""
    raw = token[7:] if token.startswith("Bearer ") else token
    parts = raw.split(".")
    if len(parts) != 3:
        return {}
    try:
        claims = json.loads(base64.urlsafe_b64decode(parts[1] + "=" * (-len(parts[1]) % 4)))
    except Exception:
        return {}
    aud = claims.get("aud")
    return {
        "aud": [aud] if isinstance(aud, str) else (aud or []),
        "azp": claims.get("azp"),
        "sub": claims.get("sub"),
        "digest": hashlib.sha256(raw.encode()).hexdigest()[:12],
    }


def _record_value(value: str) -> Any:
    """Serve a token-valued header as its claims, never its bytes."""
    body = value[7:] if value.startswith("Bearer ") else value
    if body.startswith("eyJ") and body.count(".") == 2:
        return {"jwt": _jwt_claims(value)}
    return value


def _record(request: Request, rpc: dict[str, Any]) -> None:
    # Swallows everything: a recorder bug must never fail a tool call.
    try:
        params = rpc.get("params") or {}
        _RECORDED.append(
            {
                "ts": time.time(),
                "method": rpc.get("method"),
                "tool": params.get("name"),
                "arguments": params.get("arguments", {}),
                "headers": {k.lower(): _record_value(v) for k, v in request.headers.items()},
            }
        )
    except Exception:
        logger.exception("failed to record the inbound request (ignored)")


# ---------------------------------------------------------------------------
# Mock data (copied from sibling-repos/apl-plugins/demo/hr_demo_server.py)
# ---------------------------------------------------------------------------

EMPLOYEES: dict[str, dict[str, Any]] = {
    "EMP-001234": {
        "employee_id": "EMP-001234",
        "name": "Jane Smith",
        "salary": 125000,
        "bonus": 15000,
        "ssn": "123-45-6789",
        "department": "Engineering",
        "internal_notes": "Performance review pending, do not disclose",
        "email": "jane.smith@corp.com",
        "title": "Senior Software Engineer",
    },
    "EMP-005678": {
        "employee_id": "EMP-005678",
        "name": "Bob Johnson",
        "salary": 145000,
        "bonus": 25000,
        "ssn": "234-56-7890",
        "department": "Marketing",
        "internal_notes": "Promotion candidate Q2",
        "email": "bob.johnson@corp.com",
        "title": "Marketing Manager",
    },
    "EMP-009012": {
        "employee_id": "EMP-009012",
        "name": "Alice Chen",
        "salary": 145000,
        "bonus": 20000,
        "ssn": "456-78-9012",
        "department": "Engineering",
        "internal_notes": "Team lead, retention risk",
        "email": "alice.chen@corp.com",
        "title": "Principal Engineer",
    },
}

SENT_EMAILS: list[dict[str, Any]] = []

# Fake repo fixtures — names + visibility match what Cedar policy
# expects in `args.repo_name` / `args.visibility`.
REPOS: list[dict[str, Any]] = [
    {"name": "internal/web-app", "visibility": "internal", "stars": 24, "language": "TypeScript"},
    {"name": "internal/api-gateway", "visibility": "internal", "stars": 18, "language": "Rust"},
    {"name": "internal/data-pipeline", "visibility": "internal", "stars": 11, "language": "Python"},
    {"name": "public/showcase-site", "visibility": "public", "stars": 2840, "language": "Astro"},
    {"name": "external/partner-sdk", "visibility": "external", "stars": 47, "language": "Go"},
]

# ---------------------------------------------------------------------------
# Tool implementations (logic copied from the stdio server)
# ---------------------------------------------------------------------------


def tool_get_compensation(args: dict[str, Any]) -> dict[str, Any]:
    employee_id = args.get("employee_id", "")
    include_ssn = args.get("include_ssn", False)
    employee = EMPLOYEES.get(employee_id)
    if not employee:
        return {"error": f"Employee {employee_id} not found"}
    result = {
        "employee_id": employee["employee_id"],
        "name": employee["name"],
        "salary": employee["salary"],
        "bonus": employee["bonus"],
        "department": employee["department"],
        "title": employee["title"],
        "internal_notes": employee["internal_notes"],
    }
    if include_ssn:
        # NB: this is the field gateway redaction should have stripped
        # before we ever see it. If the upstream actually sees a real
        # SSN here, the redact policy didn't fire.
        result["ssn"] = employee["ssn"]
    return result


def tool_send_email(args: dict[str, Any]) -> dict[str, Any]:
    email = {"to": args.get("to", ""), "subject": args.get("subject", ""), "body": args.get("body", "")}
    SENT_EMAILS.append(email)
    return {
        "status": "sent",
        "message_id": f"msg-{len(SENT_EMAILS):04d}",
        "to": email["to"],
        "subject": email["subject"],
    }


def tool_display_compensation(args: dict[str, Any]) -> dict[str, Any]:
    employee_id = args.get("employee_id", "")
    employee = EMPLOYEES.get(employee_id)
    if not employee:
        return {"error": f"Employee {employee_id} not found"}
    return {
        "employee_id": employee["employee_id"],
        "name": employee["name"],
        "department": employee["department"],
        "title": employee["title"],
        "salary_band": (
            "senior" if employee["salary"] >= 120000 else "mid" if employee["salary"] >= 80000 else "junior"
        ),
        "has_bonus": employee["bonus"] > 0,
    }


def tool_get_directory(args: dict[str, Any]) -> list[dict[str, Any]]:
    department = args.get("department", "")
    entries = []
    for emp in EMPLOYEES.values():
        if department and emp["department"].lower() != department.lower():
            continue
        entries.append(
            {
                "name": emp["name"],
                "department": emp["department"],
                "title": emp["title"],
                "email": emp["email"],
            }
        )
    return entries


def tool_search_repos(args: dict[str, Any]) -> dict[str, Any]:
    # Simulated GitHub Enterprise search. The interesting demo
    # property: this returns the repo IF the gateway allowed the
    # call through. The Cedar policy + post-delegate check
    # determines whether the request reaches this code at all.
    repo_name = args.get("repo_name", "")
    visibility = args.get("visibility", "")
    matches = []
    for r in REPOS:
        if repo_name and repo_name.lower() not in r["name"].lower():
            continue
        if visibility and r["visibility"].lower() != visibility.lower():
            continue
        matches.append(r)
    return {"matches": matches, "query": {"repo_name": repo_name, "visibility": visibility}}


def tool_adjust_compensation(args: dict[str, Any]) -> dict[str, Any]:
    # A change large enough to need a manager's approval only gets here
    # once the sidecar's policy has it; the tool just applies it.
    employee_id = args.get("employee_id", "")
    amount = args.get("amount")
    # The policy accepts any JSON number. Refuse a string, so nothing the
    # threshold did not see is applied, and refuse a fraction, because
    # salaries are whole dollars.
    if isinstance(amount, bool) or not isinstance(amount, (int, float)):
        return {"error": "amount must be a number"}
    if isinstance(amount, float):
        if not amount.is_integer():
            return {"error": "amount must be a whole number of dollars"}
        amount = int(amount)
    # Past 2^53 the policy's float64 is not the integer applied here.
    if abs(amount) > MAX_EXACT_INT:
        return {"error": "amount is out of range"}
    employee = EMPLOYEES.get(employee_id)
    if not employee:
        return {"error": f"Employee {employee_id} not found"}
    employee["salary"] += amount
    return {
        "status": "applied",
        "employee_id": employee["employee_id"],
        "name": employee["name"],
        "adjustment": amount,
        "new_salary": employee["salary"],
    }


def tool_get_headcount(args: dict[str, Any]) -> dict[str, Any]:
    # Aggregate only, so an agent acting as itself (no person behind
    # the call) can be allowed it.
    by_department: dict[str, int] = {}
    for emp in EMPLOYEES.values():
        by_department[emp["department"]] = by_department.get(emp["department"], 0) + 1
    return {"total": len(EMPLOYEES), "by_department": by_department}


TOOLS = {
    "get_compensation": tool_get_compensation,
    "send_email": tool_send_email,
    "display_compensation": tool_display_compensation,
    "get_directory": tool_get_directory,
    "search_repos": tool_search_repos,
    "adjust_compensation": tool_adjust_compensation,
    "get_headcount": tool_get_headcount,
}

# ---------------------------------------------------------------------------
# JSON-RPC endpoint
# ---------------------------------------------------------------------------


def _redact_token(value: str) -> str:
    """Trim long bearer tokens in logs so audiences can see the prefix
    without 800 chars of base64 noise."""
    if value.startswith("Bearer ") and len(value) > 50:
        return f"{value[:40]}…[{len(value) - 40} chars elided]"
    return value


def _log_safe(value: Any) -> str:
    """Neutralize a user-controlled value for single-line logging: coerce to
    str and replace CR/LF and other control characters with spaces so a
    crafted request field can't forge or split log entries (CWE-117)."""
    s = value if isinstance(value, str) else str(value)
    return "".join(ch if ch.isprintable() else " " for ch in s)


# ---------------------------------------------------------------------------
# Strict request parsing. The sidecar's policy judged the request as Go's
# encoding/json read it; Python's json reads some bodies differently, and
# a body read two ways is a call the policy did not see. Each difference
# is refused here, so hr-mcp runs only what the sidecar could have read.
# ---------------------------------------------------------------------------

# Past 2^53 a float64, which is how the sidecar holds a JSON number, is no
# longer the integer Python reads.
MAX_EXACT_INT = 2**53


class _Pairs(list):
    """An object's members, kept in order until the walk checks them."""


def _refuse_constant(name: str) -> Any:
    # Python accepts NaN, Infinity and -Infinity; JSON does not.
    raise ValueError(f"{name} is not JSON")


def _finite_float(text: str) -> float:
    # Python reads 1e400 as infinity; Go refuses it as out of range.
    value = float(text)
    if not math.isfinite(value):
        raise ValueError(f"{text} is out of range")
    return value


def _has_lone_surrogate(s: str) -> bool:
    # Python keeps an unpaired \ud800 escape; Go replaces it with U+FFFD.
    return any(0xD800 <= ord(ch) <= 0xDFFF for ch in s)


def _check(value: Any, path: tuple[str, ...] = ()) -> Any:
    """Build the parsed value from _Pairs, refusing repeated keys (Python
    keeps the last, as Go does, but a server might not), keys equal under
    case folding where the sidecar's parsers match case-insensitively,
    and lone surrogates. path is the lowercased keys down from the body,
    up to params.arguments, below which everything folds."""
    if isinstance(value, str):
        if _has_lone_surrogate(value):
            raise ValueError("lone surrogate in a string")
        return value
    if isinstance(value, _Pairs):
        # The envelope, params, and everything under params.arguments are
        # what the sidecar reads case-insensitively (cpex ambiguousJSON).
        folds = path in ((), ("params",)) or path[:2] == ("params", "arguments")
        out: dict[str, Any] = {}
        seen: set[str] = set()
        for key, member in value:
            if _has_lone_surrogate(key):
                raise ValueError("lone surrogate in a key")
            k = key.casefold() if folds else key
            if k in seen:
                raise ValueError(f"repeated key {key!r}")
            seen.add(k)
            out[key] = _check(member, path + (key.lower(),))
        return out
    if isinstance(value, list):
        return [_check(v, path) for v in value]
    return value


def parse_body(body: bytes) -> Any:
    """Parse a request body as strictly as the sidecar read it: UTF-8 only
    (json.loads(bytes) also detects UTF-16 and UTF-32, and accepts a byte
    order mark the sidecar refuses), no NaN or Infinity, no float out of
    float64 range, no repeated or case-colliding keys, no lone surrogates,
    and nothing after the value. Integers are read at any size, as Python
    reads them; the amount check bounds the one the demo applies."""
    text = body.decode("utf-8")  # raises UnicodeDecodeError, a ValueError
    if text.startswith("\ufeff"):
        raise ValueError("byte order mark")
    raw = json.loads(
        text,
        parse_constant=_refuse_constant,
        parse_float=_finite_float,
        object_pairs_hook=_Pairs,
    )
    return _check(raw)


def parse_rpc(body: bytes) -> dict[str, Any]:
    """Parse a request body as a JSON-RPC 2.0 object, or raise ValueError.
    hr-mcp speaks JSON-RPC only, so a body that is not one is never a tool
    call here, however the sidecar read it."""
    rpc = parse_body(body)
    if not isinstance(rpc, dict) or rpc.get("jsonrpc") != "2.0":
        raise ValueError("not a JSON-RPC 2.0 request")
    return rpc


@app.post("/mcp")
async def mcp_endpoint(request: Request) -> JSONResponse:
    body_bytes = await request.body()

    # Demo headline: print what reached us. This is what the audience
    # watches to see the gateway's effects.
    logger.info("=" * 64)
    logger.info("INBOUND REQUEST  (this is what reached the MCP server)")
    interesting_headers = [
        "authorization",
        "x-user-token",
        "x-auth-user-id",
        "x-auth-username",
        "x-auth-roles",
    ]
    for name in interesting_headers:
        v = request.headers.get(name)
        if v is not None:
            logger.info("  %-25s = %s", name, _redact_token(v))
    try:
        rpc = parse_rpc(body_bytes)
        logger.info("  body.method               = %s", _log_safe(rpc.get("method")))
        params = rpc.get("params", {})
        logger.info("  body.params.name          = %s", _log_safe(params.get("name")))
        logger.info(
            "  body.params.arguments     = %s",
            _log_safe(json.dumps(params.get("arguments", {}))),
        )
    except Exception as e:
        logger.warning("body is not JSON-RPC: %s", e)
        return JSONResponse(
            {"jsonrpc": "2.0", "error": {"code": -32700, "message": "Parse error"}, "id": None},
            status_code=400,
        )

    _record(request, rpc)
    method = rpc.get("method", "")
    rpc_id = rpc.get("id")
    if method != "tools/call":
        # tools/list and others — minimal stub so MCP clients don't
        # error during discovery, but the demo never exercises these.
        return JSONResponse(
            {"jsonrpc": "2.0", "result": {"tools": list(TOOLS.keys())}, "id": rpc_id},
        )

    tool_name = params.get("name", "")
    args = params.get("arguments", {}) or {}
    impl = TOOLS.get(tool_name)
    if impl is None:
        return JSONResponse(
            {
                "jsonrpc": "2.0",
                "error": {"code": -32601, "message": f"Unknown tool: {tool_name}"},
                "id": rpc_id,
            },
            status_code=404,
        )

    try:
        out = impl(args)
    except Exception:
        # Full detail (including the stack trace) goes to the server log;
        # the client gets a generic message so internal state isn't exposed.
        logger.exception("tool '%s' failed", _log_safe(tool_name))
        return JSONResponse(
            {
                "jsonrpc": "2.0",
                "error": {"code": -32000, "message": "internal tool error"},
                "id": rpc_id,
            },
            status_code=500,
        )

    logger.info("OUTBOUND RESPONSE")
    logger.info("  tool                      = %s", _log_safe(tool_name))
    logger.info("  payload                   = %s", json.dumps(out)[:200])
    return JSONResponse(
        {
            "jsonrpc": "2.0",
            "result": {"content": [{"type": "text", "text": json.dumps(out, indent=2)}]},
            "id": rpc_id,
        },
    )


@app.get("/_requests")
async def recorded_requests() -> dict[str, Any]:
    return {"count": len(_RECORDED), "requests": list(_RECORDED)}


@app.post("/_reset")
async def reset_recorded() -> dict[str, Any]:
    _RECORDED.clear()
    return {"count": 0}


@app.get("/healthz")
async def healthz() -> dict[str, str]:
    return {"status": "ok"}
