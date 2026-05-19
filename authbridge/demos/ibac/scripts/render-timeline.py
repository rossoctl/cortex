#!/usr/bin/env python3
"""Render the IBAC pipeline timeline for a given session.

Reads the session events JSON from stdin (output of
`GET /v1/sessions/<id>` from the authbridge session API at :9094)
and emits a human-readable forensic to stdout. Exits 0 always —
errors are surfaced as inline text rather than stack traces, so the
parent shell script can keep running and reach the verdict block.
"""

import json
import sys


def main() -> int:
    raw = sys.stdin.read()
    if not raw.strip():
        print("  (no session events to render — session API returned an empty body)")
        return 0

    try:
        doc = json.loads(raw)
    except json.JSONDecodeError as e:
        print(f"  (couldn't parse session JSON: {e})")
        print(f"  raw response (first 200 chars): {raw[:200]!r}")
        return 0

    events = doc.get("events", []) or []

    # 1. The user's intent. Preferred source: the first inbound A2A
    #    event with a text part. Fallback: pull intent_preview off the
    #    first IBAC invocation's Details — IBAC copies the captured
    #    intent there, so it's a load-bearing record even when the
    #    inbound A2A event itself landed in a different session
    #    bucket (kagenti's chat propagates a contextId on inbound but
    #    not on the agent's tool-call outbound, so a single user
    #    request can split across two sessions in the API).
    intent = None
    intent_source = ""
    for ev in events:
        a2a = ev.get("a2a")
        if ev.get("direction") == "inbound" and a2a:
            for part in a2a.get("parts", []) or []:
                if part.get("kind") == "text" and part.get("text"):
                    intent = part["text"]
                    intent_source = "inbound A2A"
                    break
        if intent:
            break

    if not intent:
        for ev in events:
            inv = ev.get("invocations") or {}
            for direction in ("inbound", "outbound"):
                for r in inv.get(direction) or []:
                    if r.get("plugin") != "ibac":
                        continue
                    preview = (r.get("details") or {}).get("intent_preview")
                    if preview:
                        intent = preview
                        intent_source = "IBAC details.intent_preview (inbound A2A bucketed elsewhere)"
                        break
                if intent:
                    break
            if intent:
                break

    print("User intent" + (f" ({intent_source})" if intent_source else "") + ":")
    print(f'  "{intent or "(not recorded — was a2a-parser in the inbound chain?)"}"')
    print()

    # 2. Every IBAC invocation, in order. Track the deny event so we
    #    can render its full details after the timeline.
    print("IBAC verdicts on outbound traffic:")
    ibac_seen = False
    deny_event = None
    for ev in events:
        inv = ev.get("invocations") or {}
        for direction in ("inbound", "outbound"):
            for r in inv.get(direction) or []:
                if r.get("plugin") != "ibac":
                    continue
                ibac_seen = True
                action = r.get("action", "?")
                reason = r.get("reason", "?")
                host = ev.get("host", "?")
                print(f"  [{direction}] {action}/{reason}  →  {host}")
                if action == "deny" and reason == "blocked":
                    deny_event = (ev, r)
    if not ibac_seen:
        print("  (no IBAC invocations — was IBAC enabled in the pipeline?)")
    print()

    # 3. The blocked outbound — full details for the operator.
    if deny_event:
        _, r = deny_event
        details = r.get("details") or {}
        print("IBAC's block — full details:")
        print(f'  intent: {details.get("intent_preview", "")}')
        action = details.get("action", "")
        first = action.splitlines()[0] if action else ""
        print(f'  action: {first}')
        if "\n" in action:
            print("          ...")
        print(f'  reason: {details.get("llm_reason", "")}')
        print()

    return 0


if __name__ == "__main__":
    sys.exit(main())
