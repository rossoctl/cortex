#!/usr/bin/env python3
"""Print the SPARC plugin verdicts recorded by an agent's authbridge session API.

Scans every session (an agent's outbound calls can land in the conversation
session or the `default` bucket depending on timing), de-duplicates, and prints
each SPARC verdict. Pass the session-API base URL as the first argument
(default http://localhost:19094).
"""

import json
import sys
import urllib.request

BASE = sys.argv[1] if len(sys.argv) > 1 else "http://localhost:19094"


def get(path):
    with urllib.request.urlopen(BASE + path, timeout=10) as resp:
        return json.load(resp)


try:
    sessions = get("/v1/sessions").get("sessions", [])
except Exception:
    print("  (could not reach the session API)")
    sys.exit(0)

seen = set()
rows = []
for s in sessions:
    try:
        snapshot = get("/v1/sessions/" + s["id"])
    except Exception:
        continue
    for event in snapshot.get("events", []):
        for inv in (event.get("invocations") or {}).get("outbound", []):
            if inv.get("plugin") != "sparc":
                continue
            if inv.get("action") not in ("allow", "modify", "deny", "observe"):
                continue
            det = inv.get("details", {})
            score = det.get("score")
            key = (inv.get("action"), inv.get("reason"), det.get("tool"), score)
            if key in seen:
                continue
            seen.add(key)
            score_s = f"  score={score}" if score not in (None, "") else ""
            rows.append(
                "  SPARC {}/{}  tool={}{}".format(inv.get("action"), inv.get("reason"), det.get("tool"), score_s)
            )

print("\n".join(rows) if rows else "  (no sparc verdicts found in any session; check `make logs-sparc`)")
