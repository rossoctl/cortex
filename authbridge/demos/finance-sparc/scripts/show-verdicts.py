#!/usr/bin/env python3
"""Print the SPARC plugin verdicts from a session snapshot (read on stdin)."""
import json
import sys

try:
    data = json.load(sys.stdin)
except Exception:
    print("  (no session events yet)")
    sys.exit(0)

seen = False
for event in data.get("events", []):
    for inv in (event.get("invocations") or {}).get("outbound", []):
        if inv.get("plugin") == "sparc" and inv.get("action") in ("allow", "modify", "deny", "observe"):
            det = inv.get("details", {})
            seen = True
            score = det.get("score")
            score_s = f"  score={score}" if score not in (None, "") else ""
            print(
                "  SPARC {}/{}  tool={}{}".format(
                    inv.get("action"), inv.get("reason"), det.get("tool"), score_s
                )
            )
if not seen:
    print("  (no sparc verdicts found in session; check `make logs-sparc`)")
