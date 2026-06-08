#!/usr/bin/env python3
"""Interactive LLM agent in front of a CPEX-enabled gateway.

The LLM thinks it's calling `get_compensation` / `send_email` /
`display_compensation` / `get_directory` tools directly. In reality:

    user prompt
        ▼
    LLM (litellm-routed: ollama/llama3, gpt-4o-mini, claude-3-7, …)
        ▼ tool_call(...)
    THIS agent
        ▼ POST /mcp with X-User-Token + Authorization
    CPEX-enabled gateway
        ▼ identity (jwt-user + jwt-client from Keycloak JWKS)
        ▼ APL: require(role.hr), redact(args.ssn) when !perm.view_ssn
        ▼ delegate(workday-oauth) — RFC 8693 → Keycloak
        ▼ forward to upstream (workday-api token, ssn maybe redacted)
    Mock HR MCP server
        ◄ tool result
    LLM
        ◄ "Here's the data: …"

The interesting demo moments:

  * Alice (engineer) asks for compensation → gateway returns an MCP
    JSON-RPC error envelope (HTTP 200, code -32001, data.violation =
    `routes.tool:get_compensation.apl.policy[0]`). The LLM sees a
    tool error and apologizes politely without leaking the violation.
  * Bob (HR + view_ssn) asks for SSN → gateway allows + delegates
    → backend sees minted workday-api token + intact SSN
  * Eve (HR, no view_ssn) asks for SSN → gateway allows + delegates
    → backend sees minted token + ssn=`[REDACTED]` (the LLM presents
    "[REDACTED]" as if it were the value, which is exactly the
    transparent enforcement story)

Usage:

    pip install -r requirements.txt

    # No API keys required — default points at a local Ollama with
    # llama3.1. Install Ollama (https://ollama.com) and `ollama pull
    # llama3.1` first.
    python chat.py --persona bob

    # Or use any LiteLLM-supported provider via env:
    export OPENAI_API_KEY=...
    python chat.py --persona bob --model gpt-4o-mini

    export ANTHROPIC_API_KEY=...
    python chat.py --persona bob --model anthropic/claude-3-7-sonnet-20250219

    # IBM watsonx.ai with Meta's Llama (tool-use needs 70B+):
    export WATSONX_APIKEY=...
    export WATSONX_URL=https://us-south.ml.cloud.ibm.com
    export WATSONX_PROJECT_ID=...
    python chat.py --persona bob \\
        --model watsonx/meta-llama/llama-3-3-70b-instruct

Switch personas mid-session with `switch <name>` — handy for showing
deny → allow → redact in one continuous demo. Type `quit` to exit.
"""

import argparse
import json
import os
import sys
import uuid
from typing import Any

import httpx
import litellm
from rich.console import Console
from rich.panel import Panel

# ---------------------------------------------------------------------------
# Defaults
# ---------------------------------------------------------------------------

DEFAULT_MODEL = "ollama/llama3.1"  # local, no API key required
DEFAULT_GATEWAY = "http://localhost:8090/mcp"
DEFAULT_KEYCLOAK = "http://localhost:8081"
KEYCLOAK_REALM = "cpex-demo"
KEYCLOAK_CLIENT_ID = "hr-copilot"
KEYCLOAK_CLIENT_SECRET = "hr-copilot-secret"

PERSONAS: dict[str, dict[str, str]] = {
    "alice": {
        "name": "Alice Chen",
        "title": "Software Engineer",
        "color": "cyan",
        "description": "Engineer — no role.hr → policy denies HR tools.",
        "password": "alice",
    },
    "bob": {
        "name": "Bob Martinez",
        "title": "HR Manager",
        "color": "green",
        "description": "HR + view_ssn → policy allows + SSN passes through.",
        "password": "bob",
    },
    "charlie": {
        "name": "Charlie Wu",
        "title": "Auditor",
        "color": "yellow",
        "description": "Auditor (no role.hr) — same as Alice for HR tools.",
        "password": "charlie",
    },
    "eve": {
        "name": "Eve Patel",
        "title": "HR Coordinator",
        "color": "magenta",
        "description": "HR but NO view_ssn → policy allows; SSN gets redacted.",
        "password": "eve",
    },
}

SYSTEM_PROMPT = (
    "You are an HR assistant for an HR copilot app. Help the user look up "
    "employee compensation, view directories, send emails, and similar "
    "tasks. Use the provided tools when needed. "
    "\n\n"
    "How to interpret tool results: "
    "\n"
    "  * If the tool returns a normal result, present the data to the "
    "user. If any field's value is the literal string `[REDACTED]`, "
    "show it as-is in your answer — that is the gateway's transparent "
    "enforcement marker that the field exists but is hidden for this "
    "caller. Do NOT apologize or refuse; just include the field with "
    "the value `[REDACTED]`. "
    "\n"
    "  * If the tool returns an `error` envelope (a JSON-RPC error "
    "with a `code` and `message`), the gateway denied the call. "
    "Acknowledge politely without revealing the internal violation "
    "code — the user may not have permission for that operation. "
    "\n"
    "  * If the tool returns an `auth_error`, the request failed at "
    "the transport layer. Ask the user to re-authenticate."
)

TOOLS = [
    {
        "type": "function",
        "function": {
            "name": "get_compensation",
            "description": (
                "Get compensation data for an employee. Returns salary, "
                "bonus, department, and optionally SSN."
            ),
            "parameters": {
                "type": "object",
                "properties": {
                    "employee_id": {
                        "type": "string",
                        "description": "Employee identifier (e.g., EMP-001234)",
                    },
                    "include_ssn": {
                        "type": "boolean",
                        "description": "Whether to include SSN in the response",
                        "default": False,
                    },
                    "ssn": {
                        "type": "string",
                        "description": (
                            "An echo-back of the employee's SSN if the caller "
                            "claims to already know it — this is exactly the "
                            "kind of field the gateway redacts when the "
                            "caller lacks the necessary permission."
                        ),
                    },
                },
                "required": ["employee_id"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "display_compensation",
            "description": (
                "Display a compensation summary for the employee (band only, "
                "no salary)."
            ),
            "parameters": {
                "type": "object",
                "properties": {
                    "employee_id": {"type": "string"},
                },
                "required": ["employee_id"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "get_directory",
            "description": "Get the employee directory listing.",
            "parameters": {
                "type": "object",
                "properties": {
                    "department": {
                        "type": "string",
                        "description": "Optional department filter",
                        "default": "",
                    },
                },
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "send_email",
            "description": "Send an email (simulated).",
            "parameters": {
                "type": "object",
                "properties": {
                    "to": {"type": "string"},
                    "subject": {"type": "string"},
                    "body": {"type": "string"},
                },
                "required": ["to", "subject", "body"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "search_repos",
            "description": (
                "Search the internal GitHub Enterprise for repositories. "
                "Filter by name substring and/or visibility. Visibility is "
                "one of `internal`, `public`, `external`."
            ),
            "parameters": {
                "type": "object",
                "properties": {
                    "repo_name": {
                        "type": "string",
                        "description": "Substring to filter repo names (e.g. 'web-app').",
                        "default": "",
                    },
                    "visibility": {
                        "type": "string",
                        "description": (
                            "Repo visibility — `internal` (default), `public`, or `external`. "
                            "External repos are typically off-limits for engineering."
                        ),
                        "enum": ["internal", "public", "external"],
                    },
                },
                "required": ["visibility"],
            },
        },
    },
]

# ---------------------------------------------------------------------------
# Keycloak token minting
# ---------------------------------------------------------------------------


def keycloak_token(persona: str, keycloak_host: str) -> str:
    """Mint a user JWT via Keycloak password grant. Persona name is
    both the username and password in the demo realm."""
    info = PERSONAS[persona]
    token_endpoint = f"{keycloak_host}/realms/{KEYCLOAK_REALM}/protocol/openid-connect/token"
    resp = httpx.post(
        token_endpoint,
        data={
            "grant_type": "password",
            "client_id": KEYCLOAK_CLIENT_ID,
            "client_secret": KEYCLOAK_CLIENT_SECRET,
            "username": persona,
            "password": info["password"],
            "scope": "openid",
        },
        timeout=10,
    )
    resp.raise_for_status()
    return _extract_access_token(resp)


def keycloak_client_token(keycloak_host: str) -> str:
    """Mint the hr-copilot client's own service-account token (the
    `Authorization` header on every gateway call)."""
    token_endpoint = f"{keycloak_host}/realms/{KEYCLOAK_REALM}/protocol/openid-connect/token"
    resp = httpx.post(
        token_endpoint,
        data={
            "grant_type": "client_credentials",
            "client_id": KEYCLOAK_CLIENT_ID,
            "client_secret": KEYCLOAK_CLIENT_SECRET,
            "scope": "openid",
        },
        timeout=10,
    )
    resp.raise_for_status()
    return _extract_access_token(resp)


def _extract_access_token(resp: httpx.Response) -> str:
    """Pull access_token out of a Keycloak token response.

    Keycloak can return HTTP 200 with an error body (no access_token),
    which would make resp.json()["access_token"] raise a bare KeyError
    that hides the real cause. Read the body, use .get(), and raise an
    httpx.HTTPStatusError carrying the response so the outer handlers
    surface a useful message."""
    body = resp.json()
    token = body.get("access_token")
    if not token:
        raise httpx.HTTPStatusError(
            f"token response missing access_token: {body!r}",
            request=resp.request,
            response=resp,
        )
    return token


# ---------------------------------------------------------------------------
# Gateway client
# ---------------------------------------------------------------------------


def new_session_id(persona: str) -> str:
    """Generate a fresh CPEX session id for the chat.

    Threaded to the gateway as `X-Session-Id` (CPEX reads it off
    Agent.SessionID, tier-0 of its session resolver). A fresh id per chat
    launch / persona switch gives each conversation its own session-scoped
    CPEX state — most visibly the taint labels: a session that has read
    compensation carries the `secret` label and is then refused external
    email, while a brand-new session starts clean. The persona prefix is
    purely for human-readable logs; the uuid suffix is what guarantees
    uniqueness."""
    return f"chat-{persona}-{uuid.uuid4().hex[:8]}"


class GatewayClient:
    """Calls tools through the CPEX gateway. The agent sends
    the client token in `Authorization` (which our jwt-client
    resolver reads), the user token in `X-User-Token` (which the
    jwt-user resolver reads), and a per-conversation `X-Session-Id`
    (which CPEX uses to scope session state such as taint labels)."""

    def __init__(self, gateway_url: str, client_token: str, user_token: str, session_id: str):
        self.gateway_url = gateway_url
        self.client_token = client_token
        self.user_token = user_token
        self.session_id = session_id
        self._request_id = 0

    def set_user_token(self, token: str) -> None:
        self.user_token = token

    def set_client_token(self, token: str) -> None:
        self.client_token = token

    def set_session_id(self, session_id: str) -> None:
        self.session_id = session_id

    def call_tool(self, tool_name: str, arguments: dict[str, Any]) -> tuple[int, dict[str, Any]]:
        self._request_id += 1
        payload = {
            "jsonrpc": "2.0",
            "method": "tools/call",
            "params": {"name": tool_name, "arguments": arguments},
            "id": self._request_id,
        }
        headers = {
            "Authorization": f"Bearer {self.client_token}",
            "Content-Type": "application/json",
            "Accept": "application/json",
            "X-User-Token": self.user_token,
            "X-Session-Id": self.session_id,
        }
        resp = httpx.post(self.gateway_url, json=payload, headers=headers, timeout=30)
        # Distinguish gateway-policy denies (4xx with body text) from
        # downstream tool errors (200 with JSON-RPC error). Only a
        # JSON-decode miss falls back to raw text; network/timeout/type
        # errors propagate so they aren't masked as a {"text": ...} body.
        try:
            data = resp.json()
        except (json.JSONDecodeError, ValueError):
            data = {"text": resp.text}
        return resp.status_code, data


# ---------------------------------------------------------------------------
# Chat loop
# ---------------------------------------------------------------------------


def format_tool_response(status: int, data: dict[str, Any]) -> str:
    """Convert the gateway's response into something compact the LLM
    can read. Pull text content out of MCP `result.content[].text`.

    Three shapes the gateway can return (per MCP spec):

      * HTTP 200 + `{"result": ...}`  — happy path
      * HTTP 200 + `{"error": {"code": -32001, "message": ..., "data": {...}}}`
                                      — application-level deny (policy, PDP,
                                        PII, delegation). The LLM should treat
                                        this as a tool-refusal.
      * HTTP 401 + plain-text body    — transport-level auth failure (JWT
                                        missing / invalid / wrong audience).
                                        Includes `WWW-Authenticate: Bearer`.
    """
    if status == 401:
        # Auth-level failure — transport problem, not a policy decision.
        # Surface enough for the LLM to back off without retrying.
        body = data.get("text") if isinstance(data, dict) else str(data)
        return json.dumps({"gateway_status": 401, "auth_error": body})
    if status >= 400:
        # Other HTTP errors (e.g. 502 from a Pingora upstream failure).
        # CPEX puts the violation code in X-Cpex-Violation but
        # we don't surface headers up here. Fall back to body.
        return json.dumps({"gateway_status": status, "error": data})
    if "error" in data:
        # MCP JSON-RPC error envelope — gateway-side deny (policy, PDP, PII,
        # delegation). Pass the message and any violation hint through to the
        # LLM so it can give the user a sensible refusal.
        err = data["error"]
        return json.dumps({
            "error": err.get("message", "tool error"),
            "violation": (err.get("data") or {}).get("violation"),
        })
    result = data.get("result", {})
    content = result.get("content", [])
    text_parts = [b.get("text", "") for b in content if isinstance(b, dict) and b.get("type") == "text"]
    combined = "".join(text_parts)
    return combined or json.dumps(result)


def run_chat(
    persona: str,
    model: str,
    gateway_url: str,
    keycloak_host: str,
) -> None:
    console = Console()
    info = PERSONAS[persona]

    try:
        user_tok = keycloak_token(persona, keycloak_host)
        client_tok = keycloak_client_token(keycloak_host)
    except httpx.HTTPError as e:
        console.print(f"[red]Failed to mint tokens from {keycloak_host}: {e}[/red]")
        console.print(
            "[dim]Is Keycloak running? `docker compose up -d` from the demo "
            "directory should have brought it up on :8081.[/dim]"
        )
        return

    session_id = new_session_id(persona)
    gateway = GatewayClient(gateway_url, client_tok, user_tok, session_id)

    console.print()
    console.print(
        Panel(
            f"[bold]{info['name']}[/bold] — {info['title']}\n"
            f"[dim]{info['description']}[/dim]\n\n"
            f"[dim]Model:    {model}[/dim]\n"
            f"[dim]Gateway:  {gateway_url}[/dim]\n"
            f"[dim]Keycloak: {keycloak_host}[/dim]\n"
            f"[dim]Session:  {session_id}[/dim]",
            title="[bold]CPEX HR Demo[/bold]",
            border_style=info["color"],
        )
    )
    console.print(
        "[dim]commands: `quit` to exit; "
        "`switch <alice|bob|charlie|eve>` to swap personas; "
        "`relogin` to mint fresh tokens for the current persona[/dim]\n"
    )

    messages: list[dict[str, Any]] = [{"role": "system", "content": SYSTEM_PROMPT}]

    while True:
        try:
            user_input = console.input(f"[bold {info['color']}]{info['name']}:[/] ").strip()
        except (EOFError, KeyboardInterrupt):
            console.print("\n[dim]bye[/dim]")
            return

        if not user_input:
            continue
        if user_input.lower() == "quit":
            console.print("[dim]bye[/dim]")
            return
        if user_input.lower() in ("relogin", "reauth"):
            # Re-mint both tokens for the current persona. The client
            # token (Authorization header) is otherwise minted once at
            # startup; after accessTokenLifespan it expires and every
            # request fails with auth.token_expired. This is the
            # demo-day escape hatch when a pause runs long.
            try:
                gateway.set_client_token(keycloak_client_token(keycloak_host))
                gateway.set_user_token(keycloak_token(persona, keycloak_host))
            except httpx.HTTPError as e:
                console.print(f"[red]re-auth failed: {e}[/red]")
                continue
            console.print()
            console.print(
                Panel(
                    f"Fresh tokens for [bold]{info['name']}[/bold] + the hr-copilot client.",
                    title="[bold]re-authenticated[/bold]",
                    border_style="green",
                )
            )
            continue

        if user_input.lower().startswith("switch "):
            new = user_input.split(" ", 1)[1].strip().lower()
            if new not in PERSONAS:
                console.print(f"[red]unknown persona '{new}'. valid: {', '.join(PERSONAS)}[/red]")
                continue
            try:
                gateway.set_client_token(keycloak_client_token(keycloak_host))
                gateway.set_user_token(keycloak_token(new, keycloak_host))
            except httpx.HTTPError as e:
                console.print(f"[red]failed to mint token for {new}: {e}[/red]")
                continue
            persona = new
            info = PERSONAS[persona]
            # Fresh CPEX session for the new persona: switching users starts
            # a clean session, so session-scoped state (taint labels, etc.)
            # from the previous persona never leaks across the switch.
            session_id = new_session_id(persona)
            gateway.set_session_id(session_id)
            messages = [{"role": "system", "content": SYSTEM_PROMPT}]
            console.print()
            console.print(
                Panel(
                    f"[bold]{info['name']}[/bold] — {info['title']}\n"
                    f"[dim]{info['description']}[/dim]\n\n"
                    f"[dim]Session:  {session_id}[/dim]",
                    title="[bold]switched[/bold]",
                    border_style=info["color"],
                )
            )
            continue

        messages.append({"role": "user", "content": user_input})

        try:
            response = litellm.completion(model=model, messages=messages, tools=TOOLS, tool_choice="auto")
        except Exception as e:
            console.print(f"[red]LLM error: {e}[/red]")
            messages.pop()
            continue

        assistant = response.choices[0].message
        if not assistant.tool_calls:
            text = assistant.content or "(no response)"
            console.print(f"[bold]assistant:[/bold] {text}\n")
            messages.append({"role": "assistant", "content": text})
            continue

        # Tool-call path. Replay through the gateway, hand the
        # results back to the LLM for a final summarization.
        messages.append(assistant.model_dump())
        for tc in assistant.tool_calls:
            fn = tc.function
            try:
                args = json.loads(fn.arguments) if isinstance(fn.arguments, str) else fn.arguments
            except json.JSONDecodeError:
                args = {}
            console.print(
                f"  [dim]→ {fn.name}({json.dumps(args, separators=(',', ':'))})[/dim]"
            )
            status, data = gateway.call_tool(fn.name, args)
            tool_text = format_tool_response(status, data)
            if status >= 400:
                console.print(f"  [dim]← [red]{status}[/red]: {tool_text}[/dim]")
            else:
                # Show the full tool result. Earlier versions truncated
                # at 200 chars to keep the terminal scannable, but the
                # demo punchline is fields like `ssn=[REDACTED]` — we
                # need them visible on the wire so the audience can see
                # the gateway enforcement, not just trust the LLM saw it.
                console.print(f"  [dim]← {tool_text}[/dim]")
            messages.append({"role": "tool", "tool_call_id": tc.id, "content": tool_text})

        try:
            final = litellm.completion(model=model, messages=messages)
            text = final.choices[0].message.content or ""
        except Exception as e:
            text = f"(LLM error summarizing tool results: {e})"
        messages.append({"role": "assistant", "content": text})
        console.print(f"[bold]assistant:[/bold] {text}\n")


# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------


def main() -> int:
    p = argparse.ArgumentParser(description="LLM agent in front of CPEX")
    p.add_argument(
        "--persona",
        default="alice",
        choices=list(PERSONAS),
        help="Starting persona (switch in-session with `switch <name>`)",
    )
    p.add_argument(
        "--model",
        default=os.environ.get("DEMO_MODEL", DEFAULT_MODEL),
        help=f"litellm-routed model (default: {DEFAULT_MODEL})",
    )
    p.add_argument(
        "--gateway",
        default=os.environ.get("GATEWAY_URL", DEFAULT_GATEWAY),
        help=f"CPEX endpoint (default: {DEFAULT_GATEWAY})",
    )
    p.add_argument(
        "--keycloak",
        default=os.environ.get("KEYCLOAK_HOST", DEFAULT_KEYCLOAK),
        help=f"Keycloak host (default: {DEFAULT_KEYCLOAK})",
    )
    args = p.parse_args()
    run_chat(args.persona, args.model, args.gateway, args.keycloak)
    return 0


if __name__ == "__main__":
    sys.exit(main())
