#!/usr/bin/env python3
# Copyright 2026
# SPDX-License-Identifier: Apache-2.0
"""Interactive A2A client for the hr-cpex agent.

In the standalone-gateway version of this demo, chat.py *was* the agent:
it ran the LLM, called tools, and POSTed MCP directly at a CPEX gateway.
After the A2A rework the LLM and tools live in the agent container
(agent.py); this file is now a thin **A2A client** that:

  * mints a persona's user token + the hr-copilot client token (Keycloak),
  * sends the user's prompt to the agent over A2A `message/send`, carrying
    `X-User-Token` (persona) + `Authorization` (client) + `X-Session-Id`
    (per-conversation taint scope) as HTTP headers, and
  * renders the agent's reply.

The agent re-attaches those identity headers onto its outbound MCP tool
calls, where the authbridge-cpex sidecar enforces CPEX. So the same
deny / allow / redact / taint story plays out — only now the enforcement
point is the agent's egress, not a gateway in front of the tool.

    A2A client (this file)  ──message/send──►  hr-cpex-agent (Service :8082)
        │  X-User-Token: <persona JWT>             │  LLM + tools
        │  Authorization: Bearer <client JWT>      ▼
        │  X-Session-Id: <conversation id>      authbridge-cpex (outbound: cpex)
        ◄────────────── agent reply ─────────────  hr-mcp

Demo moments (drive these with the prompts below):

  * Bob (HR + view_ssn) → "compensation for EMP-001234, include the SSN"
        → SSN passes through (cpex allow + delegate on egress).
  * Eve (HR, no view_ssn) → same prompt → SSN shows as [REDACTED].
  * Alice (engineer) → compensation → denied (require(role.hr)).
  * Reading compensation taints the session; a later external email is
    refused (cross-tool data-flow control via cpex session taint).

Usage:

    pip install -r requirements-client.txt
    python chat.py --persona bob

Switch personas mid-session with `switch <name>` (starts a fresh session,
so taint never leaks across the switch). `relogin` re-mints tokens for the
current persona. `quit` to exit.
"""

import argparse
import asyncio
import os
import sys
import uuid

import httpx
from a2a.client import A2AClient
from a2a.types import (
    Message,
    MessageSendParams,
    Part,
    Role,
    SendMessageRequest,
    Task,
    TextPart,
)
from a2a.utils import get_message_text
from rich.console import Console
from rich.panel import Panel

# ---------------------------------------------------------------------------
# Defaults
# ---------------------------------------------------------------------------

# The agent's A2A endpoint. With `make port-forward` the agent Service
# (which fronts the authbridge-cpex sidecar's reverse proxy) is exposed on
# localhost:8082, so message/send traverses the sidecar (inbound passthrough)
# on its way to the agent.
DEFAULT_AGENT = "http://localhost:8082"
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


# ---------------------------------------------------------------------------
# Keycloak token minting (unchanged from the standalone version — the
# client still mints both tokens; only the transport to the agent changed)
# ---------------------------------------------------------------------------


def keycloak_token(persona: str, keycloak_host: str) -> str:
    """Mint a user JWT via Keycloak password grant. Persona name is both
    the username and password in the demo realm."""
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
    `Authorization` header on every agent call)."""
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
# A2A session
# ---------------------------------------------------------------------------


def new_session_id(persona: str) -> str:
    """Fresh per-conversation session id, threaded to the agent as the A2A
    contextId AND the X-Session-Id header. cpex (on the agent's egress)
    binds it to the resolved subject to scope session state — most visibly
    the taint labels: a session that has read compensation carries the
    `secret` label and is then refused external email, while a brand-new
    session starts clean. The persona prefix is for human-readable logs;
    the uuid suffix guarantees uniqueness."""
    return f"chat-{persona}-{uuid.uuid4().hex[:8]}"


async def send_turn(
    agent_url: str,
    user_text: str,
    *,
    user_token: str,
    client_token: str,
    session_id: str,
) -> str:
    """Send one message/send to the agent and return the reply text.

    The identity headers ride on the HTTP request via `http_kwargs`; the
    agent reads them off the inbound request and threads them onto its
    cpex-governed tool calls. contextId carries the same session id inside
    the A2A envelope as a belt-and-suspenders for taint scoping."""
    async with httpx.AsyncClient(timeout=60) as httpx_client:
        client = A2AClient(httpx_client=httpx_client, url=agent_url)
        request = SendMessageRequest(
            id=uuid.uuid4().hex,
            params=MessageSendParams(
                message=Message(
                    role=Role.user,
                    parts=[Part(root=TextPart(text=user_text))],
                    message_id=uuid.uuid4().hex,
                    context_id=session_id,
                )
            ),
        )
        response = await client.send_message(
            request,
            http_kwargs={
                "headers": {
                    "X-User-Token": user_token,
                    "Authorization": f"Bearer {client_token}",
                    "X-Session-Id": session_id,
                }
            },
        )
    return _extract_reply(response)


def _extract_reply(response) -> str:
    """Pull the agent's text out of a SendMessageResponse (Message or Task)."""
    root = getattr(response, "root", response)
    error = getattr(root, "error", None)
    if error is not None:
        return f"(agent error {error.code}: {error.message})"
    result = getattr(root, "result", None)
    if result is None:
        return "(no result)"
    if isinstance(result, Task):
        # Prefer the status message; fall back to the first artifact.
        status_msg = getattr(result.status, "message", None)
        if status_msg is not None:
            text = get_message_text(status_msg)
            if text:
                return text
        for artifact in result.artifacts or []:
            text = "".join(p.root.text for p in artifact.parts if isinstance(p.root, TextPart))
            if text:
                return text
        return "(task completed, no text)"
    # Message result
    return get_message_text(result) or "(empty reply)"


# ---------------------------------------------------------------------------
# Chat loop
# ---------------------------------------------------------------------------


def run_chat(persona: str, agent_url: str, keycloak_host: str) -> None:
    console = Console()
    info = PERSONAS[persona]

    try:
        user_tok = keycloak_token(persona, keycloak_host)
        client_tok = keycloak_client_token(keycloak_host)
    except httpx.HTTPError as e:
        console.print(f"[red]Failed to mint tokens from {keycloak_host}: {e}[/red]")
        console.print("[dim]Is Keycloak port-forwarded? `make port-forward` exposes it on :8081.[/dim]")
        return

    session_id = new_session_id(persona)

    console.print()
    console.print(
        Panel(
            f"[bold]{info['name']}[/bold] — {info['title']}\n"
            f"[dim]{info['description']}[/dim]\n\n"
            f"[dim]Agent:    {agent_url}[/dim]\n"
            f"[dim]Keycloak: {keycloak_host}[/dim]\n"
            f"[dim]Session:  {session_id}[/dim]",
            title="[bold]CPEX HR Demo — A2A client[/bold]",
            border_style=info["color"],
        )
    )
    console.print(
        "[dim]commands: `quit` to exit; "
        "`switch <alice|bob|charlie|eve>` to swap personas; "
        "`relogin` to mint fresh tokens for the current persona[/dim]\n"
    )

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
            # Re-mint both tokens for the current persona. The client token
            # is otherwise minted once at startup; after accessTokenLifespan
            # it expires and the agent's cpex calls fail with
            # auth.token_expired. Demo-day escape hatch for long pauses.
            try:
                client_tok = keycloak_client_token(keycloak_host)
                user_tok = keycloak_token(persona, keycloak_host)
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
                client_tok = keycloak_client_token(keycloak_host)
                user_tok = keycloak_token(new, keycloak_host)
            except httpx.HTTPError as e:
                console.print(f"[red]failed to mint token for {new}: {e}[/red]")
                continue
            persona = new
            info = PERSONAS[persona]
            # Fresh session for the new persona: session-scoped state (taint
            # labels, conversation history) from the previous persona never
            # leaks across the switch.
            session_id = new_session_id(persona)
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

        try:
            reply = asyncio.run(
                send_turn(
                    agent_url,
                    user_input,
                    user_token=user_tok,
                    client_token=client_tok,
                    session_id=session_id,
                )
            )
        except httpx.HTTPError as e:
            console.print(f"[red]agent call failed: {e}[/red]")
            continue
        console.print(f"[bold]assistant:[/bold] {reply}\n")


# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------


def main() -> int:
    p = argparse.ArgumentParser(description="A2A client for the hr-cpex agent")
    p.add_argument(
        "--persona",
        default="alice",
        choices=list(PERSONAS),
        help="Starting persona (switch in-session with `switch <name>`)",
    )
    p.add_argument(
        "--agent",
        default=os.environ.get("AGENT_URL", DEFAULT_AGENT),
        help=f"Agent A2A endpoint (default: {DEFAULT_AGENT})",
    )
    p.add_argument(
        "--keycloak",
        default=os.environ.get("KEYCLOAK_HOST", DEFAULT_KEYCLOAK),
        help=f"Keycloak host (default: {DEFAULT_KEYCLOAK})",
    )
    args = p.parse_args()
    run_chat(args.persona, args.agent, args.keycloak)
    return 0


if __name__ == "__main__":
    sys.exit(main())
