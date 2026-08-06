"""Reproduce: IBM LiteLLM proxy + Haiku intermittently returns empty content
when response_format (structured output) mode is used.

Hypothesis: ALTK calls generate_async with schema_field="response_format",
which tells LiteLLM to use structured output mode. The IBM proxy occasionally
returns a response where choices[0].message.content is None or "" and
tool_calls is also empty, causing:
  ValueError: No content or tool calls found in response

This test sends the exact same call N times and counts how many return
empty content — proving the failure is intermittent and tied to response_format.

Requirements:
    pip install litellm python-dotenv
    OAIKEY and OAIBASE must be set in /root/.env or env vars.

Usage:
    python -m pytest tests/test_haiku_empty_response.py -v -s
    # or directly:
    python tests/test_haiku_empty_response.py
"""

from __future__ import annotations

import json
import os
import asyncio
from pathlib import Path

# Load credentials from /root/.env if present
_env_file = Path("/root/.env")
if _env_file.exists():
    for line in _env_file.read_text().splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        k, _, v = line.partition("=")
        k = k.strip().removeprefix("export").strip()
        v = v.strip().strip("\"'")
        os.environ.setdefault(k, v)

API_KEY = os.environ.get("OAIKEY", "")
API_BASE = os.environ.get("OAIBASE", "")
MODEL = "claude-haiku-4-5-20251001"

# The exact schema ALTK sends for general_hallucination_check (from production log)
HALLUCINATION_SCHEMA = {
    "title": "general_hallucination_check",
    "description": "Assessment of tool call grounding accuracy, following the rubric defined in the task description.",
    "type": "object",
    "properties": {
        "evidence": {"type": "string"},
        "explanation": {"type": "string"},
        "output": {"type": "integer", "minimum": 1, "maximum": 5},
        "confidence": {"type": "number", "minimum": 0, "maximum": 1},
        "correction": {"type": "object", "additionalProperties": True},
    },
    "required": ["confidence", "correction", "evidence", "explanation", "output"],
    "additionalProperties": False,
}

# Full production prompt extracted from the failing call in zeus logs (2026-07-28T13:14:37Z)
# This is the exact prompt that caused "No content or tool calls found in response"
# for tool=update_reservation_flights on the airline benchmark.
# The system message contains the full ALTK rubric + schema.
# The user message contains the full airline policy + multi-turn conversation history.
_SYSTEM_CONTENT = """### Task Description and Role:

Evaluate whether each parameter value in the function call is correct and **directly supported** by the provided conversation history and adhere the API specification. Your assessment must be based **strictly on explicit evidence** from these sources and correctly formatted based on the API specifications. Do **not** assume or hallucinate any information that is not clearly documented and provided.

---

#### 1. Grounding Sources

A parameter value is considered grounded if it originates from one of the following:

- An explicit user message in the conversation
- An assistant message that the user confirmed or acknowledged
- The output of a previous tool call
- A documented default value in the API specification

---

#### 2. Parameter Value Classification

Each parameter value must be labeled using one of the following categories:

- **CORRECT**
  The value is explicitly mentioned, clearly implied in the conversation, or matches a documented default.

- **MISSING_INFORMATION**
  The value is underspecified or incomplete given the current context.

- **FORMAT ERROR**
  The value is conceptually correct but incorrectly formatted based on the API specifications.

- **CONTRADICTORY_VALUES**
  The value violates documented constraints or logical relationships between parameters.

- **DEFAULT_ISSUE**
  The default value of the parameter is not the correct value based on the conversation history.

---

#### 3. Handling Default Values

- Optional parameters may only use default values that are **explicitly documented** in the API specification.
- Defaults that are assumed or undocumented count as hallucinations.
- If no value is provided and no default exists, the parameter should be omitted.

---

#### 4. Acceptable Transformations

Some transformations are permitted, but only when grounded:

- **Synonyms** are allowed only when normalized in the conversation.
- **Unit conversions** are valid only if the conversation explicitly mentions the conversion.
- **Format changes** are acceptable only if acknowledged by the assistant or supported by the specification.

---

#### 5. Multi-Call Context

If the tool call appears in a sequence:

- You may use outputs from earlier tool calls to justify parameter values.
- Consider dependencies and ordering between calls when assessing grounding.

---

#### Conservative Judgment Principle

When in doubt, err on the side of caution. If grounding cannot be clearly established, treat the parameter as incorrect.


Your output must conform to the following JSON schema, in the same order as the fields appear in the schema:
""" + json.dumps(HALLUCINATION_SCHEMA)

_USER_CONTENT = """Conversation context:
[{"content": "The task you are to complete is:\\nYou are a customer service agent that helps the user according to the <policy> provided below.\\n\\nContext:\\n- policy: # Airline Agent Policy\\n\\nThe current time is 2024-05-15 15:00:00 EST.\\n\\nAs an airline agent, you can help users **book**, **modify**, or **cancel** flight reservations. You also handle **refunds and compensation**.\\n\\nBefore taking any actions that update the booking database (booking, modifying flights, editing baggage, changing cabin class, or updating passenger information), you must list the action details and obtain explicit user confirmation (yes) to proceed.\\n\\nYou should not provide any information, knowledge, or procedures not provided by the user or available tools, or give subjective recommendations or comments.\\n\\nYou should only make one tool call at a time, and if you make a tool call, you should not respond to the user simultaneously. If you respond to the user, you should not make a tool call at the same time.\\n\\nYou should deny user requests that are against this policy.\\n\\nYou should transfer the user to a human agent if and only if the request cannot be handled within the scope of your actions.\\n\\n## Domain Basic\\n\\n### User\\nEach user has a profile containing:\\n- user id\\n- email\\n- addresses\\n- date of birth\\n- payment methods\\n- membership level\\n- reservation numbers\\n\\nThere are three types of payment methods: **credit card**, **gift card**, **travel certificate**.\\n\\nThere are three membership levels: **regular**, **silver**, **gold**.\\n\\n### Flight\\nEach flight has the following attributes:\\n- flight number\\n- origin\\n- destination\\n- scheduled departure and arrival time (local time)\\n\\nA flight can be available at multiple dates. For each date:\\n- If the status is **available**, the flight has not taken off, available seats and prices are listed.\\n- If the status is **delayed** or **on time**, the flight has not taken off, cannot be booked.\\n- If the status is **flying**, the flight has taken off but not landed, cannot be booked.\\n\\nThere are three cabin classes: **basic economy**, **economy**, **business**. **basic economy** is its own class, completely distinct from **economy**.\\n\\n## Book flight\\n\\nThe agent must first obtain the user id from the user.\\n\\nThe agent should then ask for the trip type, origin, destination.\\n\\nCabin:\\n- Cabin class must be the same across all the flights in a reservation.\\n\\nPassengers:\\n- Each reservation can have at most five passengers.\\n- The agent needs to collect the first name, last name, and date of birth for each passenger.\\n\\nPayment:\\n- Each reservation can use at most one travel certificate, at most one credit card, and at most three gift cards.\\n- The remaining amount of a travel certificate is not refundable.\\n- All payment methods must already be in user profile for safety reasons.\\n\\n## Modify flight\\n\\nFirst, the agent must obtain the user id and reservation id.\\n\\nChange flights:\\n- Basic economy flights cannot be modified.\\n- Other reservations can be modified without changing the origin, destination, and trip type.\\n\\nChange cabin:\\n- Cabin cannot be changed if any flight in the reservation has already been flown.\\n- In other cases, all reservations, including basic economy, can change cabin without changing the flights.\\n- Cabin class must remain the same across all the flights in the same reservation.\\n- If the price after cabin change is higher than the original price, the user is required to pay for the difference.\\n\\nChange passengers:\\n- The user can modify passengers but cannot modify the number of passengers.\\n\\nPayment:\\n- If the flights are changed, the user needs to provide a single gift card or credit card for payment or refund method.\\n\\n## Cancel flight\\n\\nFirst, the agent must obtain the user id and reservation id.\\n\\nThe agent must also obtain the reason for cancellation (change of plan, airline cancelled flight, or other reasons)\\n\\nIf any portion of the flight has already been flown, the agent cannot help and transfer is needed.\\n\\nOtherwise, flight can be cancelled if any of the following is true:\\n- The booking was made within the last 24 hrs\\n- The flight is cancelled by airline\\n- It is a business flight\\n- The user has travel insurance and the reason for cancellation is covered by insurance.\\n\\n## Refunds and Compensation\\nDo not proactively offer a compensation unless the user explicitly asks for one.", "role": "user"}, {"content": "Hello! How can I help you today? Please provide your user ID.", "role": "assistant"}, {"content": "sender='user' message='My user ID is daiki_muller_1116. Please proceed with the cancellations.'", "role": "user"}, {"content": "To proceed with cancellations, I will need the reservation ID(s) and the reason for cancellation.", "role": "assistant"}, {"content": "sender='user' message='The reservation IDs are XEHM4B and 59XX6W. Reason: change of plan. Cancel both.'", "role": "user"}, {"content": "", "role": "assistant"}, {"content": "\\"{\\\\\"reservation_id\\\\\": \\\\\"XEHM4B\\\\\", \\\\\"user_id\\\\\": \\\\\"daiki_muller_1116\\\\\", \\\\\"cabin\\\\\": \\\\\"basic_economy\\\\\", \\\\\"flights\\\\\": [{\\\\\"flight_number\\\\\": \\\\\"HAT005\\\\\", \\\\\"date\\\\\": \\\\\"2024-05-20\\\\\"}, {\\\\\"flight_number\\\\\": \\\\\"HAT178\\\\\", \\\\\"date\\\\\": \\\\\"2024-05-30\\\\\"}], \\\\\"passengers\\\\\": [{\\\\\"first_name\\\\\": \\\\\"Daiki\\\\\", \\\\\"last_name\\\\\": \\\\\"Muller\\\\\", \\\\\"dob\\\\\": \\\\\"1954-07-04\\\\\"}, {\\\\\"first_name\\\\\": \\\\\"Omar\\\\\", \\\\\"last_name\\\\\": \\\\\"Ito\\\\\", \\\\\"dob\\\\\": \\\\\"1975-09-19\\\\\"}], \\\\\"payment_history\\\\\": [{\\\\\"payment_id\\\\\": \\\\\"credit_card_2408938\\\\\", \\\\\"amount\\\\\": 296}], \\\\\"insurance\\\\\": \\\\\"no\\\\\", \\\\\"status\\\\\": null}\\"", "role": "tool"}, {"content": "\\"{\\\\\"reservation_id\\\\\": \\\\\"59XX6W\\\\\", \\\\\"cabin\\\\\": \\\\\"economy\\\\\", \\\\\"flights\\\\\": [{\\\\\"flight_number\\\\\": \\\\\"HAT007\\\\\", \\\\\"date\\\\\": \\\\\"2024-05-19\\\\\"}, {\\\\\"flight_number\\\\\": \\\\\"HAT174\\\\\", \\\\\"date\\\\\": \\\\\"2024-05-29\\\\\"}], \\\\\"passengers\\\\\": [{\\\\\"first_name\\\\\": \\\\\"Daiki\\\\\", \\\\\"last_name\\\\\": \\\\\"Muller\\\\\"}, {\\\\\"first_name\\\\\": \\\\\"Chen\\\\\", \\\\\"last_name\\\\\": \\\\\"Muller\\\\\"}], \\\\\"insurance\\\\\": \\\\\"yes\\\\\", \\\\\"status\\\\\": null}\\"", "role": "tool"}, {"content": "XEHM4B (Basic Economy, No Insurance): Not eligible for cancellation under change of plan. 59XX6W (Economy, With Insurance): Eligible for cancellation. Would you like to proceed with only 59XX6W?", "role": "assistant"}, {"content": "sender='user' message='No, I want to upgrade XEHM4B to economy first, then cancel it. Proceed with that.'", "role": "user"}, {"content": "I will upgrade XEHM4B from basic economy to economy, then cancel both reservations. Please confirm.", "role": "assistant"}, {"content": "sender='user' message='Yes, upgrade XEHM4B to economy, no insurance. Cancel both XEHM4B and 59XX6W after.'", "role": "user"}, {"content": "", "role": "assistant"}, {"content": "\\"{\\\\\"reservation_id\\\\\": \\\\\"XEHM4B\\\\\", \\\\\"cabin\\\\\": \\\\\"economy\\\\\", \\\\\"flights\\\\\": [{\\\\\"flight_number\\\\\": \\\\\"HAT005\\\\\", \\\\\"date\\\\\": \\\\\"2024-05-20\\\\\", \\\\\"price\\\\\": 166}, {\\\\\"flight_number\\\\\": \\\\\"HAT178\\\\\", \\\\\"date\\\\\": \\\\\"2024-05-30\\\\\", \\\\\"price\\\\\": 126}], \\\\\"payment_history\\\\\": [{\\\\\"payment_id\\\\\": \\\\\"credit_card_2408938\\\\\", \\\\\"amount\\\\\": 296}, {\\\\\"payment_id\\\\\": \\\\\"credit_card_2408938\\\\\", \\\\\"amount\\\\\": 288}], \\\\\"status\\\\\": null}\\"", "role": "tool"}]

Tool Specification:
{"name": "cancel_reservation", "description": "Cancel the whole reservation.", "parameters": {"properties": {"reservation_id": {"title": "Reservation Id", "type": "string"}}, "required": ["reservation_id"], "title": "cancel_reservationArgumentsWithoutSessionId", "type": "object"}}

Proposed tool call:
{"id": "9", "type": "function", "function": {"name": "cancel_reservation", "arguments": "{\\"reservation_id\\": \\"XEHM4B\\"}"}}

Return a JSON object as specified in the system prompt. You MUST keep the same order of fields in the JSON object as provided in the JSON schema and examples."""

PROMPT = [
    {"role": "system", "content": _SYSTEM_CONTENT},
    {"role": "user", "content": _USER_CONTENT},
]


def _schema_to_pydantic(schema: dict):
    """Reproduce ALTK's json_schema_to_pydantic_model conversion."""
    from pydantic import BaseModel, Field, create_model
    from typing import Optional, Any

    type_mapping = {"string": str, "integer": int, "number": float, "boolean": bool, "array": list, "object": dict}
    fields = {}
    required_fields = set(schema.get("required", []))
    for prop_name, prop_schema in schema.get("properties", {}).items():
        field_type = type_mapping.get(prop_schema.get("type"), Any)
        default = ... if prop_name in required_fields else None
        desc = prop_schema.get("description")
        fields[prop_name] = (field_type, Field(default, description=desc) if desc else (field_type, default))
    return create_model(schema.get("title", "AutoModel"), **fields)


def _call_with_response_format() -> dict | None:
    """Single synchronous call reproducing ALTK's exact path:
    schema dict -> pydantic model -> response_format kwarg -> litellm.acompletion
    This is what ValidatingLLMClient.generate_async does when schema_field='response_format'.
    """
    import litellm

    # ALTK converts schema dict to Pydantic model and passes as response_format
    pydantic_schema = _schema_to_pydantic(HALLUCINATION_SCHEMA)

    response = litellm.completion(
        model=MODEL,
        messages=PROMPT,
        api_key=API_KEY,
        api_base=API_BASE,
        response_format=pydantic_schema,   # Pydantic model, NOT a json_schema dict
        timeout=30,
    )
    msg = response.choices[0].message
    content = getattr(msg, "content", None)
    tool_calls = getattr(msg, "tool_calls", None)
    return {
        "content": content,
        "tool_calls": tool_calls,
        "empty": not content and not tool_calls,
    }


def _call_with_system_prompt() -> dict | None:
    """Single call with schema injected in system prompt — the proposed fix."""
    import litellm

    response = litellm.completion(
        model=MODEL,
        messages=PROMPT,  # schema already embedded in system prompt above
        api_key=API_KEY,
        api_base=API_BASE,
        timeout=30,
    )
    msg = response.choices[0].message
    content = getattr(msg, "content", None)
    tool_calls = getattr(msg, "tool_calls", None)
    return {
        "content": content,
        "tool_calls": tool_calls,
        "empty": not content and not tool_calls,
    }


def test_response_format_intermittent_empty(n_calls: int = 10):
    """Send the same call N times with response_format and count empty responses.

    If the hypothesis is correct, at least some calls will return empty content,
    proving the IBM proxy is intermittently broken in response_format mode.
    """
    empty_count = 0
    results = []

    for i in range(n_calls):
        result = _call_with_response_format()
        results.append(result)
        status = "EMPTY" if result["empty"] else "OK"
        print(f"  call {i+1:02d}: {status}  content_len={len(result['content'] or '')}")
        if result["empty"]:
            empty_count += 1

    print(f"\nSummary: {empty_count}/{n_calls} calls returned empty content")

    # The test proves the hypothesis if at least 1 call is empty.
    # If 0 are empty, the IBM proxy may have been fixed or the prompt is too simple.
    assert empty_count > 0, (
        f"All {n_calls} calls succeeded — IBM proxy may be stable now, "
        f"or the prompt needs to be longer to trigger the failure."
    )


def test_system_prompt_mode_no_empty(n_calls: int = 10):
    """Same N calls but with schema in system prompt instead of response_format.

    If the fix works, zero calls should return empty content.
    """
    empty_count = 0

    for i in range(n_calls):
        result = _call_with_system_prompt()
        status = "EMPTY" if result["empty"] else "OK"
        print(f"  call {i+1:02d}: {status}  content_len={len(result['content'] or '')}")
        if result["empty"]:
            empty_count += 1

    print(f"\nSummary: {empty_count}/{n_calls} calls returned empty content")

    assert empty_count == 0, (
        f"{empty_count}/{n_calls} calls returned empty content even with system-prompt mode"
    )


if __name__ == "__main__":
    if not API_KEY or not API_BASE:
        print("ERROR: OAIKEY and OAIBASE must be set in /root/.env or environment")
        raise SystemExit(1)

    N = 10
    print(f"=== Test 1: response_format mode ({N} calls) ===")
    empty = 0
    for i in range(N):
        r = _call_with_response_format()
        status = "EMPTY" if r["empty"] else f"OK ({len(r['content'] or '')} chars)"
        print(f"  call {i+1:02d}: {status}")
        if r["empty"]:
            empty += 1
    print(f"Result: {empty}/{N} empty\n")

    print(f"=== Test 2: system-prompt mode ({N} calls) ===")
    empty = 0
    for i in range(N):
        r = _call_with_system_prompt()
        status = "EMPTY" if r["empty"] else f"OK ({len(r['content'] or '')} chars)"
        print(f"  call {i+1:02d}: {status}")
        if r["empty"]:
            empty += 1
    print(f"Result: {empty}/{N} empty")
